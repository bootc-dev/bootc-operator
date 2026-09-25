// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	sigsyaml "sigs.k8s.io/yaml"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/test/e2e/e2eutil"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

const (
	releaseManifestURL = "https://github.com/bootc-dev/bootc-operator/releases/download/%s/install.yaml"
)

var operatorCRDNames = []string{
	"bootcnodepools.node.bootc.dev",
	"bootcnodes.node.bootc.dev",
}

// TestOperatorUpgrade installs the released version of the operator
// from its published manifest, verifies basic functionality, then
// upgrades by re-applying the current manifests (including CRDs) on
// top and verifies the operator still works.
func TestOperatorUpgrade(t *testing.T) {
	e2eutil.Providers(t, "bink")

	releaseTag := os.Getenv("E2E_OPERATOR_RELEASE_TAG")
	if releaseTag == "" {
		t.Skip("E2E_OPERATOR_RELEASE_TAG not set")
	}
	releasedImg := os.Getenv("E2E_OPERATOR_RELEASED_IMG")
	if releasedImg == "" {
		t.Skip("E2E_OPERATOR_RELEASED_IMG not set")
	}

	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	ctx := context.Background()

	// Capture the current operator image before we replace it.
	var origDeploy appsv1.Deployment
	g.Expect(env.Client.Get(ctx, operatorDeployKey(), &origDeploy)).To(Succeed())
	currentImg := containerImage(t, origDeploy.Spec.Template.Spec.Containers, "manager")
	t.Logf("Current operator image: %s", currentImg)
	t.Logf("Released operator tag: %s, image: %s", releaseTag, releasedImg)

	// Phase 1: Delete the current operator and install the released
	// version from its published manifest.
	installReleasedOperator(t, g, ctx, env, releaseTag, releasedImg)

	t.Cleanup(func() {
		t.Logf("Restoring operator to current version...")
		applyCurrentManifests(t, currentImg)
		waitForOperatorReady(t, g, ctx, env.Client)
		t.Logf("Operator restored")
	})

	// Phase 2: Verify the released version works.
	nodeName := env.AddNode(t)

	pool := env.NewPool("upgrade", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute)

	t.Logf("Node %q is Idle with released operator", nodeName)

	// Phase 3: Upgrade by applying the current manifests on top.
	applyCurrentManifests(t, currentImg)
	waitForOperatorReady(t, g, ctx, env.Client)

	t.Logf("Upgraded operator to current version via manifest apply")

	// Verify the pre-existing pool and node survived the upgrade.
	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute)
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageDigest()))

	t.Logf("Pool and node survived the upgrade in expected state")

	// Phase 4: Verify the operator still works after upgrade by
	// triggering a node image update and verifying the full update
	// lifecycle completes.
	updateRef := env.NodeImageUpdateDigestedPullSpec()

	modified := pool.DeepCopy()
	modified.Spec.Image.Ref = updateRef
	g.Expect(env.Client.Patch(ctx, modified, client.MergeFrom(pool))).To(Succeed())
	*pool = *modified

	t.Logf("Patched pool to update image %s", updateRef)

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 5*time.Minute,
		HaveField("Booted", HaveField("ImageDigest", env.NodeImageUpdateDigest())),
	)

	t.Logf("Node %q completed update with upgraded operator", nodeName)

	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageUpdateDigest()))
}

// installReleasedOperator deletes the current operator, downloads the
// released install manifest, patches the image reference, and applies
// it.
func installReleasedOperator(
	t *testing.T,
	g Gomega,
	ctx context.Context,
	env *e2eutil.Env,
	releaseTag, releasedImg string,
) {
	t.Helper()

	deleteCurrentOperator(t, g, ctx, env)

	manifest := downloadReleaseManifest(t, releaseTag)
	patchedManifest := patchManifestImage(t, manifest, releasedImg)

	kubectlApply(t, patchedManifest)
	waitForOperatorReady(t, g, ctx, env.Client)

	t.Logf("Released operator %s installed from manifest", releaseTag)
}

// deleteCurrentOperator deletes custom resources (while the operator
// is still running so it can handle finalizer removal), then removes
// the operator Deployment, DaemonSet, and CRDs.
func deleteCurrentOperator(
	t *testing.T,
	g Gomega,
	ctx context.Context,
	env *e2eutil.Env,
) {
	t.Helper()

	// Delete CRs first while the operator is running so it can
	// remove finalizers. If CRs with finalizers remain when the
	// controller is gone, CRD deletion hangs indefinitely.
	t.Logf("Deleting custom resources...")
	g.Expect(env.Client.DeleteAllOf(ctx, &bootcv1alpha1.BootcNodePool{})).To(Succeed())
	g.Expect(env.Client.DeleteAllOf(ctx, &bootcv1alpha1.BootcNode{})).To(Succeed())

	g.Eventually(func(g Gomega) {
		var pools bootcv1alpha1.BootcNodePoolList
		g.Expect(env.Client.List(ctx, &pools)).To(Succeed())
		g.Expect(pools.Items).To(BeEmpty())
		var nodes bootcv1alpha1.BootcNodeList
		g.Expect(env.Client.List(ctx, &nodes)).To(Succeed())
		g.Expect(nodes.Items).To(BeEmpty())
	}).WithTimeout(2*time.Minute).Should(Succeed(),
		"expected all custom resources to be deleted")

	var deploy appsv1.Deployment
	g.Expect(env.Client.Get(ctx, operatorDeployKey(), &deploy)).To(Succeed())
	var ds appsv1.DaemonSet
	g.Expect(env.Client.Get(ctx, operatorDaemonSetKey(), &ds)).To(Succeed())

	t.Logf("Deleting operator deployment, daemonset, and CRDs...")
	g.Expect(env.Client.Delete(ctx, &deploy)).To(Succeed())
	g.Expect(env.Client.Delete(ctx, &ds)).To(Succeed())

	for _, crdName := range operatorCRDNames {
		var crd apiextensionsv1.CustomResourceDefinition
		if err := env.Client.Get(ctx, client.ObjectKey{Name: crdName}, &crd); err == nil {
			g.Expect(env.Client.Delete(ctx, &crd)).To(Succeed())
		}
	}

	g.Eventually(func(g Gomega) {
		var d appsv1.Deployment
		err := env.Client.Get(ctx, operatorDeployKey(), &d)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		var dset appsv1.DaemonSet
		err = env.Client.Get(ctx, operatorDaemonSetKey(), &dset)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		for _, crdName := range operatorCRDNames {
			var crd apiextensionsv1.CustomResourceDefinition
			err = env.Client.Get(ctx, client.ObjectKey{Name: crdName}, &crd)
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	}).WithTimeout(2*time.Minute).Should(Succeed(),
		"expected operator resources to be deleted")

	t.Logf("Operator deployment, daemonset, and CRDs deleted")
}

// downloadReleaseManifest fetches install.yaml from a GitHub release.
func downloadReleaseManifest(t *testing.T, tag string) []byte {
	t.Helper()

	url := fmt.Sprintf(releaseManifestURL, tag)
	t.Logf("Downloading release manifest from %s", url)

	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		t.Fatalf("downloading release manifest: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("downloading release manifest: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading release manifest body: %v", err)
	}

	return data
}

// patchManifestImage replaces container image references in
// Deployment (manager) and DaemonSet (daemon) documents.
func patchManifestImage(t *testing.T, manifest []byte, img string) []byte {
	t.Helper()

	var out bytes.Buffer
	reader := utilyaml.NewDocumentDecoder(io.NopCloser(bytes.NewReader(manifest)))
	defer func() { _ = reader.Close() }()

	first := true
	for {
		buf := make([]byte, len(manifest)+256)
		n, err := reader.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("reading YAML document: %v", err)
		}
		doc := buf[:n]

		var obj map[string]interface{}
		if err := sigsyaml.Unmarshal(doc, &obj); err != nil {
			t.Fatalf("unmarshaling YAML document: %v", err)
		}

		kind, _ := obj["kind"].(string)
		switch kind {
		case "Deployment":
			setContainerImage(t, obj, "manager", img)
		case "DaemonSet":
			setContainerImage(t, obj, "daemon", img)
		}

		patched, err := sigsyaml.Marshal(obj)
		if err != nil {
			t.Fatalf("marshaling patched %s: %v", kind, err)
		}

		if !first {
			out.WriteString("---\n")
		}
		out.Write(patched)
		first = false
	}

	return out.Bytes()
}

// setContainerImage patches the image field of a named container in a
// Deployment or DaemonSet manifest map.
func setContainerImage(t *testing.T, obj map[string]interface{}, containerName, img string) {
	t.Helper()

	spec, _ := obj["spec"].(map[string]interface{})
	template, _ := spec["template"].(map[string]interface{})
	podSpec, _ := template["spec"].(map[string]interface{})
	containers, _ := podSpec["containers"].([]interface{})

	for _, c := range containers {
		container, _ := c.(map[string]interface{})
		if name, _ := container["name"].(string); name == containerName {
			container["image"] = img
			return
		}
	}
	t.Fatalf("container %q not found in %s", containerName, obj["kind"])
}

// kubectlApply applies the given manifest bytes via kubectl.
func kubectlApply(t *testing.T, manifest []byte) {
	t.Helper()

	kubeconfigPath := os.Getenv("KUBECONFIG")
	args := []string{"apply", "--server-side", "-f", "-"}
	if kubeconfigPath != "" {
		args = append([]string{"--kubeconfig", kubeconfigPath}, args...)
	}

	cmd := exec.Command("kubectl", args...)
	cmd.Stdin = bytes.NewReader(manifest)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl apply failed: %v\n%s", err, out)
	}

	t.Logf("kubectl apply:\n%s", out)
}

// applyCurrentManifests runs the equivalent of "make deploy" for the
// current version: kustomize build + image patch + kubectl apply.
func applyCurrentManifests(t *testing.T, img string) {
	t.Helper()

	repoRoot := findRepoRoot(t)

	kustomize := filepath.Join(repoRoot, "bin", "kustomize")
	if _, err := os.Stat(kustomize); err != nil {
		kustomize = "kustomize"
	}

	cmd := exec.Command(kustomize, "build", filepath.Join(repoRoot, "config", "default"))
	manifest, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kustomize build failed: %v\n%s", err, manifest)
	}

	manifest = patchManifestImage(t, manifest, img)
	kubectlApply(t, manifest)
}

// findRepoRoot walks up from the test directory to find the repository
// root (containing go.mod).
func findRepoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getting working directory: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repository root (go.mod)")
		}
		dir = parent
	}
}

func waitForOperatorReady(
	t *testing.T,
	g Gomega,
	ctx context.Context,
	c client.Client,
) {
	t.Helper()
	g.Eventually(func(g Gomega) {
		var d appsv1.Deployment
		g.Expect(c.Get(ctx, operatorDeployKey(), &d)).To(Succeed())
		g.Expect(d.Status.Replicas).To(Equal(int32(1)))
		g.Expect(d.Status.UpdatedReplicas).To(Equal(int32(1)))
		g.Expect(d.Status.AvailableReplicas).To(Equal(int32(1)))
	}).WithTimeout(3*time.Minute).Should(Succeed(),
		"expected operator deployment to be ready")
}

func operatorDeployKey() client.ObjectKey {
	return client.ObjectKey{
		Namespace: testutil.OperatorNamespaceName,
		Name:      "bootc-operator-controller-manager",
	}
}

func operatorDaemonSetKey() client.ObjectKey {
	return client.ObjectKey{
		Namespace: testutil.OperatorNamespaceName,
		Name:      "bootc-operator-daemon",
	}
}

func containerImage(t *testing.T, containers []corev1.Container, name string) string {
	t.Helper()
	for _, c := range containers {
		if c.Name == name {
			return c.Image
		}
	}
	t.Fatalf("container %q not found in pod spec", name)
	return ""
}
