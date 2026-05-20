// SPDX-License-Identifier: Apache-2.0

// Package e2eutil provides helpers for running end-to-end tests against
// a bink-managed Kubernetes cluster. The cluster and operator are
// expected to be already running (via `make deploy-bink`). Each test
// provisions its own worker nodes for isolation.
package e2eutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega" //nolint:staticcheck
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
	testutil "github.com/jlebon/bootc-operator/test/util"
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

	// clusterName is the bink cluster name.
	clusterName string

	// testID is the sanitized test name, used as the value for
	// LabelE2ETest on nodes and in pool selectors.
	testID string

	// nodes tracks node names added via AddNode for cleanup.
	nodes []string
}

// New connects to an existing bink cluster and returns an Env ready
// for testing. The cluster must be running with the operator deployed
// (via `make deploy-bink`).
func New(t *testing.T) *Env {
	t.Helper()

	kubeconfigPath := os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" {
		t.Fatal("KUBECONFIG must be set")
	}

	clusterName := os.Getenv("BINK_CLUSTER_NAME")
	if clusterName == "" {
		t.Fatal("BINK_CLUSTER_NAME must be set")
	}

	k8sClient := buildClient(t, kubeconfigPath)

	env := &Env{
		Client:      k8sClient,
		clusterName: clusterName,
		testID:      sanitizeTestName(t.Name()),
	}

	t.Cleanup(func() {
		env.cleanup(t)
	})

	return env
}

// NodeOption configures a node provisioned by AddNode.
type NodeOption func(*nodeConfig)

type nodeConfig struct {
	memory int
	labels map[string]string
}

// WithMemory sets the VM memory in MB for the node.
func WithMemory(mb int) NodeOption {
	return func(c *nodeConfig) {
		c.memory = mb
	}
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

// AddNode provisions a worker node via bink, waits for it to be Ready,
// labels it with LabelE2ETest, and registers cleanup to remove it.
// Returns the node name.
func (e *Env) AddNode(t *testing.T, opts ...NodeOption) string {
	t.Helper()

	cfg := &nodeConfig{}
	for _, o := range opts {
		o(cfg)
	}

	nodeName := e.generateNodeName(t)

	// Provision the node.
	args := []string{"node", "add", nodeName, "--cluster-name", e.clusterName, "--control-plane", "controller"}
	if cfg.memory > 0 {
		args = append(args, "--memory", fmt.Sprintf("%d", cfg.memory))
	}
	if img := os.Getenv("BINK_NODE_IMAGE"); img != "" {
		args = append(args, "--node-image", img)
	}
	t.Logf("Adding node %q...", nodeName)
	if err := runBink(t, args...); err != nil {
		t.Fatalf("adding node %q: %v", nodeName, err)
	}

	e.nodes = append(e.nodes, nodeName)

	// Wait for Ready.
	waitForNodeReady(t, e.Client, nodeName)

	// Label with test ID (replace with --label` once available:
	// https://github.com/alicefr/bink/issues/23).
	var node corev1.Node
	if err := e.Client.Get(context.Background(), client.ObjectKey{Name: nodeName}, &node); err != nil {
		t.Fatalf("getting node %q for labeling: %v", nodeName, err)
	}
	patch := client.StrategicMergeFrom(node.DeepCopy())
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	node.Labels[LabelE2ETest] = e.testID
	for k, v := range cfg.labels {
		node.Labels[k] = v
	}
	if err := e.Client.Patch(context.Background(), &node, patch); err != nil {
		t.Fatalf("labeling node %q: %v", nodeName, err)
	}

	return nodeName
}

// NewPool creates a BootcNodePool with a test-scoped name and labels.
// The pool is labeled with LabelE2ETest for cleanup. If no
// WithNodeSelector option is provided, it defaults to selecting nodes
// with LabelE2ETest (i.e. all nodes belonging to this test).
func (e *Env) NewPool(suffix, imageRef string, opts ...testutil.PoolOption) *bootcv1alpha1.BootcNodePool {
	defaults := []testutil.PoolOption{
		testutil.WithLabel(LabelE2ETest, e.testID),
		testutil.WithNodeSelector(e.TestLabels()),
	}
	allOpts := append(defaults, opts...)
	return testutil.NewPool(e.testID+"-"+suffix, imageRef, allOpts...)
}

// TestLabels returns the label map identifying resources belonging to
// this test. Use with testutil.WithNodeSelector() when overriding the
// default node selector in NewPool.
func (e *Env) TestLabels() map[string]string {
	return map[string]string{LabelE2ETest: e.testID}
}

// cleanup deletes test-scoped resources and bink nodes.
func (e *Env) cleanup(t *testing.T) {
	ctx := context.Background()
	t.Logf("Removing pools with label %s=%s...", LabelE2ETest, e.testID)
	if err := e.Client.DeleteAllOf(ctx, &bootcv1alpha1.BootcNodePool{}, client.MatchingLabels(e.TestLabels())); err != nil {
		t.Logf("WARNING: pool cleanup: %v", err)
	}
	for _, name := range e.nodes {
		t.Logf("Removing node %q...", name)
		if err := runBink(t, "node", "remove", name, "--force", "--cluster-name", e.clusterName); err != nil {
			t.Logf("WARNING: failed to remove node %q: %v", name, err)
		}
	}
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

// generateNodeName creates a unique node name derived from the test name.
func (e *Env) generateNodeName(t *testing.T) string {
	t.Helper()

	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generating random suffix: %v", err)
	}
	return e.testID + "-" + hex.EncodeToString(b)
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

// runBink executes a bink command and returns any error.
func runBink(t *testing.T, args ...string) error {
	t.Helper()

	cmd := exec.Command("bink", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
