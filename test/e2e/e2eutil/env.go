/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package e2eutil provides helpers for running end-to-end tests against
// bink-managed Kubernetes clusters. Each test gets its own cluster for
// full isolation.
package e2eutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
	testutil "github.com/jlebon/bootc-operator/test/util"
)

// config holds resolved settings for the test cluster.
type config struct {
	memory int
}

// Option configures how the test cluster is created.
type Option func(*config)

// binkPath is resolved once on first use via resolveBinkPath.
var (
	binkPath     string
	binkPathOnce sync.Once
)

// Env holds a live cluster context for a single e2e test.
type Env struct {
	// Client is a controller-runtime client with the bootc CRD scheme
	// registered. Ready to create/get/list CRD objects.
	Client client.Client

	// Kubeconfig is the path to the kubeconfig file for this cluster.
	Kubeconfig string

	// Cluster is the bink cluster name.
	Cluster string
}

// New creates a bink cluster, deploys the operator manifests, and
// returns an Env ready for testing. The cluster is torn down
// automatically when the test ends (including on failure).
func New(t *testing.T, opts ...Option) *Env {
	t.Helper()

	cfg := config{}
	for _, o := range opts {
		o(&cfg)
	}

	resolveBinkPath(t)
	clusterName := generateClusterName(t)

	// Find the project root (where config/default/ lives).
	projectRoot, err := findProjectRoot()
	if err != nil {
		t.Fatalf("finding project root: %v", err)
	}

	startCluster(t, clusterName, cfg.memory)

	// Register cleanup early so the cluster is torn down even if
	// subsequent steps fail.
	t.Cleanup(func() {
		stopCluster(t, clusterName)
	})

	kubeconfigPath := exposeAPI(t, clusterName)

	k8sClient := buildClient(t, kubeconfigPath)
	waitForNodeReady(t, k8sClient, "node1")

	pushControllerImage(t)
	applyManifests(t, kubeconfigPath, projectRoot)
	waitForControllerReady(t, k8sClient)

	return &Env{
		Client:     k8sClient,
		Kubeconfig: kubeconfigPath,
		Cluster:    clusterName,
	}
}

// WithMemory sets the VM memory in MB. If not set, bink's default is used.
func WithMemory(mb int) Option {
	return func(c *config) {
		c.memory = mb
	}
}

// resolveBinkPath locates the bink binary on first call (via BINK_PATH
// env var or PATH lookup) and caches the result.
func resolveBinkPath(t *testing.T) {
	t.Helper()

	var resolveErr error
	binkPathOnce.Do(func() {
		if p := os.Getenv("BINK_PATH"); p != "" {
			if _, err := os.Stat(p); err != nil {
				resolveErr = fmt.Errorf("BINK_PATH=%q not found: %w", p, err)
				return
			}
			binkPath = p
			return
		}
		if p, err := exec.LookPath("bink"); err != nil {
			resolveErr = fmt.Errorf("PATH lookup failed: %w", err)
		} else {
			binkPath = p
		}
	})

	if binkPath == "" {
		// Since this is only set when Once is triggered, we implicitly only
		// log the actual failure once here. Other calls would get the generic
		// error. I don't think that's a problem.
		if resolveErr != nil {
			t.Log(resolveErr)
		}
		t.Fatal("bink binary not found (set BINK_PATH or add to PATH)")
	}
}

// generateClusterName creates a unique cluster name for test isolation.
func generateClusterName(t *testing.T) string {
	t.Helper()

	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generating random cluster name: %v", err)
	}
	return "e2e-" + hex.EncodeToString(b)
}

// findProjectRoot walks up from the working directory looking for go.mod.
func findProjectRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find go.mod in any parent directory")
		}
		dir = parent
	}
}

// startCluster runs `bink cluster start`.
func startCluster(t *testing.T, clusterName string, memory int) {
	t.Helper()

	args := []string{"cluster", "start",
		"--cluster-name", clusterName,
		"--api-port", "0",
	}
	if memory > 0 {
		args = append(args, "--memory", fmt.Sprintf("%d", memory))
	}
	if img := os.Getenv("BINK_NODE_IMAGE"); img != "" {
		args = append(args, "--node-image", img)
	}

	t.Logf("Starting bink cluster %q...", clusterName)
	if err := runBink(t, args...); err != nil {
		t.Fatalf("starting cluster: %v", err)
	}
}

// stopCluster runs `bink cluster stop --remove-data`.
func stopCluster(t *testing.T, clusterName string) {
	t.Logf("Tearing down bink cluster %q...", clusterName)
	if err := runBink(t, "cluster", "stop", "--remove-data", "--cluster-name", clusterName); err != nil {
		// Log but don't fail — we're in cleanup.
		t.Logf("WARNING: failed to stop cluster %q: %v", clusterName, err)
	}
}

// exposeAPI runs `bink api expose` and returns the kubeconfig path.
func exposeAPI(t *testing.T, clusterName string) string {
	t.Helper()
	t.Logf("Exposing API for cluster %q...", clusterName)

	kubeconfigPath := filepath.Join(t.TempDir(), "kubeconfig")
	if err := runBink(t, "api", "expose",
		"--cluster-name", clusterName,
		"--kubeconfig", kubeconfigPath,
	); err != nil {
		t.Fatalf("exposing API: %v", err)
	}
	return kubeconfigPath
}

const (
	// registryImage is the pullspec used to push to the bink registry
	// from the host.
	registryImage = "localhost:5000/bootc-operator:e2e"
	// inClusterControllerRepo is the in-cluster registry repo for the
	// controller image (without tag).
	inClusterControllerRepo = "registry.cluster.local:5000/bootc-operator"
	// inClusterControllerTag is the tag used for the controller image.
	inClusterControllerTag = "e2e"
)

// controllerImagePushed tracks whether the image has already been pushed
// for this test run. Multiple tests share the same image.
var (
	controllerImagePushed     bool
	controllerImagePushedOnce sync.Once
)

// pushControllerImage pushes the pre-built controller image (from IMG
// env var, set by the Makefile) to the bink registry. This is done once
// per test run.
func pushControllerImage(t *testing.T) {
	t.Helper()

	controllerImagePushedOnce.Do(func() {
		srcImage := os.Getenv("IMG")
		if srcImage == "" {
			t.Fatal("IMG env var not set (run via 'make e2e')")
		}

		t.Logf("Pushing %s to %s...", srcImage, registryImage)
		cmd := exec.Command("podman", "push", "--tls-verify=false", srcImage, registryImage)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("pushing controller image: %v", err)
		}

		controllerImagePushed = true
	})

	if !controllerImagePushed {
		t.Fatal("controller image push failed in another test")
	}
}

// applyManifests applies the operator manifests with the controller
// image set to the in-cluster registry pullspec. It creates a temporary
// kustomize overlay that references the project's config/default via a
// symlink and adds an image override.
func applyManifests(t *testing.T, kubeconfigPath, projectRoot string) {
	t.Helper()
	t.Logf("Applying operator manifests with controller image %s...", inClusterControllerRepo+":"+inClusterControllerTag)

	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Fatal("kubectl not found on PATH")
	}

	// Build a temp dir with an overlay kustomization that references
	// the project's config/default via a relative path. The layout is:
	//   <tmpdir>/config -> <projectRoot>/config  (symlink)
	//   <tmpdir>/overlay/kustomization.yaml      (overlay)
	// So the overlay can use "../config/default" as a resource (because
	// kustomize doesn't allow absolute paths for resources).
	tmpdir := t.TempDir()

	absConfig, _ := filepath.Abs(filepath.Join(projectRoot, "config"))
	if err := os.Symlink(absConfig, filepath.Join(tmpdir, "config")); err != nil {
		t.Fatalf("creating config symlink: %v", err)
	}

	overlay := filepath.Join(tmpdir, "overlay")
	if err := os.Mkdir(overlay, 0755); err != nil {
		t.Fatalf("creating overlay dir: %v", err)
	}

	kustomization := fmt.Sprintf(`
resources:
- ../config/default
images:
- name: controller
  newName: %s
  newTag: %s
patches:
- patch: |
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: bootc-operator-controller-manager
      namespace: bootc-operator
    spec:
      template:
        spec:
          tolerations:
          - key: node-role.kubernetes.io/control-plane
            operator: Exists
            effect: NoSchedule
`, inClusterControllerRepo, inClusterControllerTag)
	if err := os.WriteFile(filepath.Join(overlay, "kustomization.yaml"), []byte(kustomization), 0644); err != nil {
		t.Fatalf("writing overlay kustomization: %v", err)
	}

	cmd := exec.Command(kubectl, "apply",
		"--kubeconfig", kubeconfigPath,
		"-k", overlay,
		"--server-side",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("kubectl apply failed: %v", err)
	}
}

// waitForControllerReady waits for the controller manager Deployment to
// have at least one ready replica.
func waitForControllerReady(t *testing.T, c client.Client) {
	t.Helper()
	t.Log("Waiting for controller to be ready...")

	ctx := context.Background()
	testutil.WaitFor(t, 3*time.Minute, 5*time.Second, "controller to be ready", func() (bool, error) {
		var dep appsv1.Deployment
		key := client.ObjectKey{
			Namespace: "bootc-operator",
			Name:      "bootc-operator-controller-manager",
		}
		if err := c.Get(ctx, key, &dep); apierrors.IsNotFound(err) {
			t.Logf("  controller deployment not found yet")
			return false, nil
		} else if err != nil {
			return false, err
		}
		if dep.Status.ReadyReplicas > 0 {
			t.Log("  controller is ready")
			return true, nil
		}
		t.Logf("  controller not ready yet (ready=%d)", dep.Status.ReadyReplicas)
		return false, nil
	})
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

	ctx := context.Background()
	testutil.WaitFor(t, 5*time.Minute, 5*time.Second, "node "+nodeName+" to be Ready", func() (bool, error) {
		node := &corev1.Node{}
		if err := c.Get(ctx, client.ObjectKey{Name: nodeName}, node); apierrors.IsNotFound(err) {
			t.Logf("  node %q not found yet", nodeName)
			return false, nil
		} else if err != nil {
			return false, err
		}
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				t.Logf("  node %q is Ready", nodeName)
				return true, nil
			}
		}
		return false, nil
	})
}

// runBink executes a bink command and returns any error.
func runBink(t *testing.T, args ...string) error {
	t.Helper()

	if binkPath == "" {
		panic("runBink called before resolveBinkPath")
	}
	cmd := exec.Command(binkPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
