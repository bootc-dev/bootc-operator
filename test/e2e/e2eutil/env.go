// SPDX-License-Identifier: Apache-2.0

// Package e2eutil provides helpers for running end-to-end tests against
// a Kubernetes cluster. The cluster and operator are expected to be
// already running. Each test provisions its own worker nodes for
// isolation.
package e2eutil

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	. "github.com/onsi/gomega" //nolint:staticcheck
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

const (
	// LabelE2ETest is applied to test-scoped resources (nodes, pools).
	// Its value is the sanitized test name, used for both node
	// selection and label-based cleanup.
	LabelE2ETest = "bootc.dev/e2e-test"
)

// Env holds a cluster context for a single e2e test. Each test creates
// its own Env via New(t), so test-scoped state (like the test ID used
// for node labeling and pool selectors) lives here.
type Env struct {
	// Client is a controller-runtime client with the bootc CRD scheme
	// registered.
	Client client.Client

	// testID is the sanitized test name, used as the value for
	// LabelE2ETest on nodes and in pool selectors.
	testID string

	// providerName identifies the active provider ("bink" or "eks").
	providerName string

	// provider handles node provisioning and removal.
	provider NodeProvider

	// nodes tracks node names added via AddNode for cleanup.
	nodes []string

	// nodeImageRef is the full digest-qualified reference for the base
	// node image (e.g. "registry.example.com/node@sha256:abc123").
	nodeImageRef string

	// nodeImageUpdateRef is the full digest-qualified reference for the
	// update image. May point to a different registry/image than the base.
	nodeImageUpdateRef string

	// nodeImageUpdate2Ref is the full digest-qualified reference for the
	// second update image. Empty when not needed.
	nodeImageUpdate2Ref string

	// registryHost is the hostname (with optional port) of the
	// authenticated registry used by TestPullSecretAuth.
	registryHost string

	// registryUser is the username for the authenticated e2e registry.
	// Empty when not configured.
	registryUser string

	// registryPassword is the password for the authenticated e2e
	// registry. Empty when not configured.
	registryPassword string

	// nodePullSecretName is the name of the auto-created pull secret
	// for node images. Empty when no credentials are configured or
	// the provider does not need one.
	nodePullSecretName string
}

// New connects to an existing cluster and returns an Env ready for
// testing. The cluster must be running with the operator deployed.
func New(t *testing.T) *Env {
	t.Helper()

	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" {
		t.Fatal("KUBECONFIG must be set")
	}

	k8sClient := buildClient(t, kubeconfigPath)

	providerName := os.Getenv("E2E_PROVIDER")
	if providerName == "" {
		providerName = "bink"
	}

	var (
		provider     NodeProvider
		nodeImageRef string
		updateRef    string
		update2Ref   string
		registryHost string
	)

	switch providerName {
	case "bink":
		nodeImageRegistry := requireEnv(t, "E2E_NODE_IMAGE_REGISTRY")
		nodeImageDigest := requireEnv(t, "E2E_NODE_IMAGE_DIGEST")
		updateDigest := requireEnv(t, "E2E_NODE_IMAGE_UPDATE_DIGEST")
		update2Digest := requireEnv(t, "E2E_NODE_IMAGE_UPDATE2_DIGEST")

		nodeImageRef = nodeImageRegistry + "@" + nodeImageDigest
		updateRef = nodeImageRegistry + "@" + updateDigest
		update2Ref = nodeImageRegistry + "@" + update2Digest

		registryHost = "auth-registry.cluster.local:5001"

		clusterName := requireEnv(t, "BINK_CLUSTER_NAME")
		diskImage := os.Getenv("BINK_NODE_DISK_IMAGE")
		provider = newBinkProvider(clusterName, nodeImageRef, diskImage)
	case "eks":
		nodeImageRef = requireEnv(t, "E2E_NODE_IMAGE_REF")
		updateRef = requireEnv(t, "E2E_NODE_IMAGE_UPDATE_REF")
		update2Ref = os.Getenv("E2E_NODE_IMAGE_UPDATE2_REF")
		registryHost = os.Getenv("E2E_REGISTRY_HOST")
		if registryHost == "" {
			registryHost = extractRegistryHost(updateRef)
		}

		eksClusterName := requireEnv(t, "EKS_CLUSTER_NAME")
		nodeGroup := requireEnv(t, "EKS_NODE_GROUP")
		region := requireEnv(t, "AWS_REGION")

		var err error
		provider, err = newEKSProvider(
			eksClusterName, nodeGroup, region, k8sClient,
		)
		if err != nil {
			t.Fatalf("creating EKS provider: %v", err)
		}
	default:
		t.Fatalf("unknown E2E_PROVIDER %q (supported: bink, eks)", providerName)
	}

	env := &Env{
		Client:              k8sClient,
		testID:              sanitizeTestName(t.Name()),
		providerName:        providerName,
		provider:            provider,
		nodeImageRef:        nodeImageRef,
		nodeImageUpdateRef:  updateRef,
		nodeImageUpdate2Ref: update2Ref,
		registryHost:        registryHost,
		registryUser:        os.Getenv("E2E_REGISTRY_USER"),
		registryPassword:    os.Getenv("E2E_REGISTRY_PASSWORD"),
	}

	if os.Getenv("E2E_REGISTRY_AUTH") == "true" {
		if env.registryUser == "" || env.registryPassword == "" {
			t.Fatal("E2E_REGISTRY_AUTH=true requires E2E_REGISTRY_USER and E2E_REGISTRY_PASSWORD")
		}
		env.createNodePullSecret(t)
	}

	t.Cleanup(func() {
		env.cleanup(t)
	})

	return env
}

// NodeOption configures a node provisioned by AddNode.
type NodeOption func(*nodeConfig)

type nodeConfig struct {
	labels map[string]string
}

// WithLabel adds a label to the provisioned node. This is in addition
// to the LabelE2ETest label which is always applied.
func WithLabel(key, value string) NodeOption {
	return func(c *nodeConfig) {
		if c.labels == nil {
			c.labels = make(map[string]string)
		}
		c.labels[key] = value
	}
}

// AddNode provisions a worker node via the configured provider, waits
// for it to be Ready, and returns the node name. The node is labeled
// with LabelE2ETest (and any extra labels from WithLabel).
func (e *Env) AddNode(t *testing.T, opts ...NodeOption) string {
	t.Helper()

	cfg := &nodeConfig{}
	for _, o := range opts {
		o(cfg)
	}

	labels := map[string]string{LabelE2ETest: e.testID}
	for k, v := range cfg.labels {
		labels[k] = v
	}

	ctx := context.Background()
	t.Logf("Adding node...")
	nodeName, err := e.provider.AddNode(ctx, labels)
	if err != nil {
		t.Fatalf("adding node: %v", err)
	}
	t.Logf("Added node %q", nodeName)

	e.nodes = append(e.nodes, nodeName)
	waitForNodeReady(t, e.Client, nodeName)

	return nodeName
}

// NewPool creates a BootcNodePool with a test-scoped name and labels.
// The pool is labeled with LabelE2ETest for cleanup. If no
// WithNodeSelector option is provided, it defaults to selecting nodes
// with LabelE2ETest (i.e. all nodes belonging to this test).
func (e *Env) NewPool(
	suffix, imageRef string,
	opts ...testutil.PoolOption,
) *bootcv1alpha1.BootcNodePool {
	defaults := []testutil.PoolOption{
		testutil.WithLabel(LabelE2ETest, e.testID),
		testutil.WithNodeSelector(e.TestLabels()),
	}
	if e.nodePullSecretName != "" {
		defaults = append(
			defaults,
			testutil.WithPullSecret(
				e.nodePullSecretName,
				testutil.OperatorNamespaceName,
			),
		)
	}
	allOpts := append(defaults, opts...)
	return testutil.NewPool(e.testID+"-"+suffix, imageRef, allOpts...)
}

// TestID returns the sanitized test name used for naming resources.
func (e *Env) TestID() string {
	return e.testID
}

// TestLabels returns the label map identifying resources belonging to
// this test. Use with testutil.WithNodeSelector() when overriding the
// default node selector in NewPool.
func (e *Env) TestLabels() map[string]string {
	return map[string]string{LabelE2ETest: e.testID}
}

// NodeImageDigestedPullSpec returns the full digest-qualified reference
// for the base node image.
func (e *Env) NodeImageDigestedPullSpec() string {
	return e.nodeImageRef
}

// NodeImageTagRef returns the tag-based reference for the base node
// image (e.g. "registry.cluster.local:5000/node:latest").
func (e *Env) NodeImageTagRef() string {
	repo, _, _ := strings.Cut(e.nodeImageRef, "@")
	return repo + ":latest"
}

// NodeImageDigest returns the manifest digest portion of the base
// node image reference.
func (e *Env) NodeImageDigest() string {
	_, digest, _ := strings.Cut(e.nodeImageRef, "@")
	return digest
}

// NodeImageUpdateDigestedPullSpec returns the full digest-qualified
// reference for the update image.
func (e *Env) NodeImageUpdateDigestedPullSpec() string {
	return e.nodeImageUpdateRef
}

// NodeImageUpdateDigest returns the manifest digest portion of the
// update image reference.
func (e *Env) NodeImageUpdateDigest() string {
	_, digest, _ := strings.Cut(e.nodeImageUpdateRef, "@")
	return digest
}

// NodeImageUpdate2DigestedPullSpec returns the full digest-qualified
// reference for the second update image.
func (e *Env) NodeImageUpdate2DigestedPullSpec() string {
	return e.nodeImageUpdate2Ref
}

// NodeImageUpdate2Digest returns the manifest digest portion of the
// second update image reference.
func (e *Env) NodeImageUpdate2Digest() string {
	_, digest, _ := strings.Cut(e.nodeImageUpdate2Ref, "@")
	return digest
}

// AuthImageRef returns the image reference to use for pull secret
// tests. For bink, this rewrites the update digest to go through the
// auth registry (port 5001). For EKS, the update image already
// requires auth, so it is returned as-is.
func (e *Env) AuthImageRef() string {
	switch e.providerName {
	case "bink":
		return e.registryHost + "/node@" + e.NodeImageUpdateDigest()
	default:
		return e.nodeImageUpdateRef
	}
}

// RegistryHost returns the hostname (with optional port) of the
// authenticated registry used for pull secret tests.
func (e *Env) RegistryHost() string {
	return e.registryHost
}

// RegistryUser returns the authenticated registry username, or empty
// if not configured.
func (e *Env) RegistryUser() string {
	return e.registryUser
}

// RegistryPassword returns the authenticated registry password, or
// empty if not configured.
func (e *Env) RegistryPassword() string {
	return e.registryPassword
}

// createNodePullSecret creates a dockerconfigjson Secret for pulling
// node images from an authenticated registry. The secret is scoped to
// the test via testID and cleaned up automatically.
func (e *Env) createNodePullSecret(t *testing.T) {
	t.Helper()

	authStr := base64.StdEncoding.EncodeToString(
		[]byte(e.registryUser + ":" + e.registryPassword),
	)
	dockerCfg := fmt.Sprintf(
		`{"auths":{%q:{"auth":"%s"}}}`,
		e.registryHost, authStr,
	)

	secretName := e.testID + "-node-pull-secret"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: testutil.OperatorNamespaceName,
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(dockerCfg),
		},
	}

	ctx := context.Background()
	if err := e.Client.Create(ctx, secret); err != nil {
		t.Fatalf("creating node pull secret: %v", err)
	}
	t.Logf("Created node pull secret %q", secretName)

	t.Cleanup(func() {
		if err := e.Client.Delete(ctx, secret); err != nil {
			t.Logf("WARNING: deleting node pull secret %q: %v", secretName, err)
		}
	})

	e.nodePullSecretName = secretName
}

// RetagImage reads the image at srcRef from the localhost registry and
// tags it as dstTag.
func RetagImage(t *testing.T, srcRef, dstTag string) {
	t.Helper()

	src, err := name.ParseReference(srcRef, name.Insecure)
	if err != nil {
		t.Fatalf("parsing src ref %q: %v", srcRef, err)
	}
	desc, err := remote.Get(src)
	if err != nil {
		t.Fatalf("fetching %q: %v", srcRef, err)
	}
	img, err := desc.Image()
	if err != nil {
		t.Fatalf("getting image from descriptor: %v", err)
	}

	dst, err := name.ParseReference(dstTag, name.Insecure)
	if err != nil {
		t.Fatalf("parsing dst ref %q: %v", dstTag, err)
	}
	if err := remote.Write(dst, img); err != nil {
		t.Fatalf("writing %q: %v", dstTag, err)
	}
}

// cleanup gathers diagnostic logs, then deletes test-scoped resources
// and removes nodes via the provider.
func (e *Env) cleanup(t *testing.T) {
	e.gatherLogs(t)

	ctx := context.Background()
	t.Logf("Removing pools with label %s=%s...", LabelE2ETest, e.testID)
	if err := e.Client.DeleteAllOf(
		ctx,
		&bootcv1alpha1.BootcNodePool{},
		client.MatchingLabels(e.TestLabels()),
	); err != nil {
		t.Logf("WARNING: pool cleanup: %v", err)
	}
	for _, name := range e.nodes {
		t.Logf("Removing node %q...", name)
		if err := e.provider.RemoveNode(ctx, name); err != nil {
			t.Logf("WARNING: failed to remove node %q: %v", name, err)
		}
	}
}

// gatherLogs calls hack/gather-logs.sh to collect diagnostic logs into
// $ARTIFACTS/<testID>/. Skipped if ARTIFACTS is not set.
func (e *Env) gatherLogs(t *testing.T) {
	artifactsDir := os.Getenv("ARTIFACTS")
	if artifactsDir == "" {
		return
	}

	outputDir := filepath.Join(artifactsDir, e.testID)

	// The test runs from test/e2e/, so resolve the script relative
	// to the repo root.
	args := []string{"../../hack/gather-logs.sh", outputDir}
	args = append(args, e.nodes...)

	t.Logf("Gathering logs to %s...", outputDir)
	cmd := exec.Command("bash", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Logf("WARNING: gather-logs.sh failed: %v", err)
	}
}

func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Fatalf("%s must be set", key)
	}
	return v
}

// extractRegistryHost returns the registry hostname from a full image
// reference like "registry.example.com/path/image@sha256:...".
func extractRegistryHost(ref string) string {
	withoutDigest, _, _ := strings.Cut(ref, "@")
	host, _, _ := strings.Cut(withoutDigest, "/")
	return host
}

// sanitizeTestName lowercases a test name for use in k8s object names.
// Panics if the result exceeds 63 characters (k8s label value limit).
func sanitizeTestName(name string) string {
	name = strings.ToLower(name)
	if len(name) > 63 {
		panic(fmt.Sprintf("test name %q is %d characters (max 63)", name, len(name)))
	}
	return name
}

// buildClient creates a controller-runtime client from the kubeconfig
// with the bootc CRD scheme registered.
func buildClient(t *testing.T, kubeconfigPath string) client.Client {
	t.Helper()

	if err := bootcv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatalf("adding bootc scheme: %v", err)
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		t.Fatalf("building rest config from kubeconfig: %v", err)
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		t.Fatalf("creating controller-runtime client: %v", err)
	}

	return c
}

// waitForNodeReady polls until the named node has condition Ready=True.
func waitForNodeReady(t *testing.T, c client.Client, nodeName string) {
	t.Helper()
	t.Logf("Waiting for node %q to be Ready...", nodeName)

	g := NewWithT(t)
	ctx := context.Background()
	g.Eventually(func(g Gomega) {
		node := &corev1.Node{}
		g.Expect(c.Get(ctx, client.ObjectKey{Name: nodeName}, node)).To(Succeed())
		g.Expect(node.Status.Conditions).To(ContainElement(And(
			HaveField("Type", corev1.NodeReady),
			HaveField("Status", corev1.ConditionTrue),
		)), "node %q not Ready yet", nodeName)
		t.Logf("  node %q is Ready", nodeName)
	}).WithTimeout(5 * time.Minute).WithPolling(5 * time.Second).Should(Succeed())
}
