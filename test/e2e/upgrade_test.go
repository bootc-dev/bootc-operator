// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/test/e2e/e2eutil"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

const helmReleaseName = "bootc-operator"

var operatorCRDNames = []string{
	"bootcnodepools.node.bootc.dev",
	"bootcnodes.node.bootc.dev",
}

// TestOperatorUpgrade installs the released version of the operator
// via Helm, verifies basic functionality, then upgrades by running
// helm upgrade with the current image and verifies the operator still
// works.
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

	repoRoot := findRepoRoot(t)
	chartPath := filepath.Join(repoRoot, "chart", "bootc-operator")

	// Capture the current operator image and extra args before we
	// replace it.
	var origDeploy appsv1.Deployment
	g.Expect(env.Client.Get(ctx, operatorDeployKey(), &origDeploy)).To(Succeed())
	currentImg := containerImage(t, origDeploy.Spec.Template.Spec.Containers, "manager")
	extraArgs := controllerExtraArgs(t, origDeploy.Spec.Template.Spec.Containers)
	t.Logf("Current operator image: %s", currentImg)
	t.Logf("Released operator tag: %s, image: %s", releaseTag, releasedImg)

	// Phase 1: Uninstall the current operator and re-install with
	// the released image via Helm.
	uninstallOperator(t, g, ctx, env)
	helmInstallOperator(t, chartPath, releasedImg, extraArgs)
	waitForOperatorReady(t, g, ctx, env.Client)

	t.Logf("Released operator %s installed via Helm", releaseTag)

	t.Cleanup(func() {
		t.Logf("Restoring operator to current version...")
		helmUpgradeOperator(t, chartPath, currentImg, extraArgs)
		waitForOperatorReady(t, g, ctx, env.Client)
		t.Logf("Operator restored")
	})

	// Phase 2: Verify the released version works.
	nodeName := env.AddNode(t)

	pool := env.NewPool("upgrade", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute)

	t.Logf("Node %q is Idle with released operator", nodeName)

	// Phase 3: Upgrade via helm upgrade to the current version.
	helmUpgradeOperator(t, chartPath, currentImg, extraArgs)
	waitForOperatorReady(t, g, ctx, env.Client)

	t.Logf("Upgraded operator to current version via Helm")

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

// uninstallOperator removes the operator via Helm uninstall, then
// deletes CRDs (which Helm does not manage on uninstall).  Custom
// resources are deleted first while the operator is still running so
// it can handle finalizer removal.
func uninstallOperator(
	t *testing.T,
	g Gomega,
	ctx context.Context,
	env *e2eutil.Env,
) {
	t.Helper()

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

	t.Logf("Uninstalling Helm release...")
	helmRun(t, "uninstall", helmReleaseName,
		"--namespace", testutil.OperatorNamespaceName)

	g.Eventually(func(g Gomega) {
		var d appsv1.Deployment
		err := env.Client.Get(ctx, operatorDeployKey(), &d)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		var ds appsv1.DaemonSet
		err = env.Client.Get(ctx, operatorDaemonSetKey(), &ds)
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		for _, crdName := range operatorCRDNames {
			var crd apiextensionsv1.CustomResourceDefinition
			err = env.Client.Get(ctx, client.ObjectKey{Name: crdName}, &crd)
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	}).WithTimeout(2*time.Minute).Should(Succeed(),
		"expected operator resources to be deleted")

	t.Logf("Operator uninstalled")
}

// helmInstallOperator installs the operator via helm install.
func helmInstallOperator(t *testing.T, chartPath, img string, extraArgs []string) {
	t.Helper()
	args := []string{
		"install", helmReleaseName, chartPath,
		"--namespace", testutil.OperatorNamespaceName,
		"--create-namespace",
	}
	args = append(args, helmImageArgs(img, extraArgs)...)
	helmRun(t, args...)
}

// helmUpgradeOperator upgrades the operator via helm upgrade.
func helmUpgradeOperator(t *testing.T, chartPath, img string, extraArgs []string) {
	t.Helper()
	args := []string{
		"upgrade", helmReleaseName, chartPath,
		"--namespace", testutil.OperatorNamespaceName,
	}
	args = append(args, helmImageArgs(img, extraArgs)...)
	helmRun(t, args...)
}

// helmImageArgs returns the --set flags for image and controller args.
func helmImageArgs(img string, extraArgs []string) []string {
	repo, tag := splitImage(img)
	args := []string{
		"--set", "image.repository=" + repo,
		"--set", "image.tag=" + tag,
		"--set", "image.pullPolicy=Always",
	}
	if len(extraArgs) > 0 {
		j, err := json.Marshal(extraArgs)
		if err != nil {
			panic("marshaling extraArgs: " + err.Error())
		}
		args = append(args, "--set-json", "controller.extraArgs="+string(j))
	}
	return args
}

// helmRun executes a helm command, adding --kubeconfig if set.
func helmRun(t *testing.T, args ...string) {
	t.Helper()

	subcmd := args[0]
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		args = append([]string{"--kubeconfig", kc}, args...)
	}

	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm %s failed: %v\n%s", subcmd, err, out)
	}
	t.Logf("helm %s:\n%s", subcmd, out)
}

// splitImage splits a container image reference into repository and tag.
func splitImage(ref string) (repo, tag string) {
	lastSlash := strings.LastIndex(ref, "/")
	lastColon := strings.LastIndex(ref, ":")
	if lastColon > lastSlash {
		return ref[:lastColon], ref[lastColon+1:]
	}
	return ref, "latest"
}

// controllerExtraArgs returns the controller's extra arguments,
// filtering out the flags that the Helm chart always includes.
func controllerExtraArgs(t *testing.T, containers []corev1.Container) []string {
	t.Helper()
	for _, c := range containers {
		if c.Name != "manager" {
			continue
		}
		var extra []string
		for _, arg := range c.Args {
			switch {
			case arg == "--leader-elect",
				strings.HasPrefix(arg, "--health-probe-bind-address="):
			default:
				extra = append(extra, arg)
			}
		}
		return extra
	}
	t.Fatalf("container %q not found in pod spec", "manager")
	return nil
}

// waitForOperatorReady polls until the operator deployment has one available and up-to-date replica.
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

// operatorDeployKey returns the namespaced name of the operator Deployment.
func operatorDeployKey() client.ObjectKey {
	return client.ObjectKey{
		Namespace: testutil.OperatorNamespaceName,
		Name:      "bootc-operator-controller",
	}
}

// operatorDaemonSetKey returns the namespaced name of the operator DaemonSet.
func operatorDaemonSetKey() client.ObjectKey {
	return client.ObjectKey{
		Namespace: testutil.OperatorNamespaceName,
		Name:      "bootc-operator-daemon",
	}
}

// containerImage returns the image of the named container, or fails the test if not found.
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
