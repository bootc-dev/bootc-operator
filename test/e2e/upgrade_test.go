// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/test/e2e/e2eutil"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

const helmReleaseName = "bootc-operator"

var operatorCRDNames = []string{
	"bootcnodepools.node.bootc.dev",
	"bootcnodes.node.bootc.dev",
	"bootcoperatorconfigs.node.bootc.dev",
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

	originalConfig, err := readOperatorConfig(ctx, env.Client)
	g.Expect(err).NotTo(HaveOccurred())
	// Uninstall removes CRDs and their instances. Register recovery first.
	t.Cleanup(func() {
		t.Logf("Restoring operator to current version...")
		helmUpgradeOperator(t, chartPath, currentImg, extraArgs)
		waitForOperatorConfigCRD(t, g, ctx, env.Client)
		g.Expect(restoreOperatorConfig(ctx, env.Client, originalConfig)).To(Succeed(),
			"restore administrator-owned operator configuration")
		restartOperator(t)
		waitForOperatorReady(t, g, ctx, env.Client)
		t.Logf("Operator restored")
	})

	// Phase 1: Replace the current Helm release with the released image.
	uninstallOperator(t, g, ctx, env)
	// The released image predates BootcOperatorConfig and needs bink's
	// registry settings through its command-line flags.
	releasedArgs := append([]string{
		"--allow-insecure-registry", "--tag-resolution-interval=10s",
	}, extraArgs...)
	helmInstallOperator(t, chartPath, releasedImg, releasedArgs)
	waitForOperatorReady(t, g, ctx, env.Client)
	t.Logf("Released operator %s installed via Helm", releaseTag)

	// Phase 2: Verify the released version works.
	nodeName := env.AddNode(t)

	pool := env.NewPool("upgrade", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute)

	t.Logf("Node %q is Idle with released operator", nodeName)

	// Phase 3: Upgrade via Helm to the current version.
	helmUpgradeOperator(t, chartPath, currentImg, extraArgs)
	waitForOperatorConfigCRD(t, g, ctx, env.Client)
	waitForOperatorReady(t, g, ctx, env.Client)
	config, err := readOperatorConfig(ctx, env.Client)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(config).To(BeNil(), "installation must not create a configuration instance")

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

// uninstallOperator removes the Helm release and its CRDs. Custom
// resources are deleted first so the operator can handle finalizers.
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
	args = append(args, helmImageArgs(t, img, extraArgs)...)
	helmRun(t, args...)
}

// helmUpgradeOperator upgrades the operator, installing it if cleanup follows
// a failed release installation.
func helmUpgradeOperator(t *testing.T, chartPath, img string, extraArgs []string) {
	t.Helper()
	args := []string{
		"upgrade", "--install", helmReleaseName, chartPath,
		"--namespace", testutil.OperatorNamespaceName,
	}
	args = append(args, helmImageArgs(t, img, extraArgs)...)
	helmRun(t, args...)
}

// helmImageArgs returns the --set flags for image and controller args.
func helmImageArgs(t *testing.T, img string, extraArgs []string) []string {
	t.Helper()
	repo, tag := splitImage(img)
	args := []string{
		"--set-string", "image.repository=" + repo,
		"--set-string", "image.tag=" + tag,
		"--set-string", "image.pullPolicy=Always",
	}
	if len(extraArgs) > 0 {
		j, err := json.Marshal(extraArgs)
		if err != nil {
			t.Fatalf("marshaling extraArgs: %v", err)
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

// readOperatorConfig returns the optional administrator-owned instance. An
// ambiguous configuration should fail the upgrade instead of being discarded.
func readOperatorConfig(
	ctx context.Context,
	c client.Client,
) (*bootcv1alpha1.BootcOperatorConfig, error) {
	var configs bootcv1alpha1.BootcOperatorConfigList
	if err := c.List(ctx, &configs); err != nil {
		return nil, fmt.Errorf("list operator configurations: %w", err)
	}
	switch len(configs.Items) {
	case 0:
		return nil, nil
	case 1:
		return configs.Items[0].DeepCopy(), nil
	default:
		return nil, fmt.Errorf(
			"expected at most one operator configuration, found %d",
			len(configs.Items),
		)
	}
}

// restoreOperatorConfig restores only administrator-owned fields after CRD
// deletion. A different instance name is an error rather than an implicit rename.
func restoreOperatorConfig(
	ctx context.Context,
	c client.Client,
	original *bootcv1alpha1.BootcOperatorConfig,
) error {
	current, err := readOperatorConfig(ctx, c)
	if err != nil {
		return err
	}
	if original == nil {
		if current == nil {
			return nil
		}
		return c.Delete(ctx, current)
	}
	if current != nil && current.Name != original.Name {
		return fmt.Errorf(
			"restore operator configuration %q: unexpected current instance %q",
			original.Name,
			current.Name,
		)
	}
	if current == nil {
		restored := original.DeepCopy()
		restored.ObjectMeta = metav1.ObjectMeta{
			Name:        original.Name,
			Labels:      original.Labels,
			Annotations: original.Annotations,
		}
		return c.Create(ctx, restored)
	}
	current.Spec = original.Spec
	current.Labels = original.Labels
	current.Annotations = original.Annotations
	return c.Update(ctx, current)
}

func waitForOperatorConfigCRD(t *testing.T, g Gomega, ctx context.Context, c client.Client) {
	t.Helper()
	g.Eventually(func() ([]apiextensionsv1.CustomResourceDefinitionCondition, error) {
		var crd apiextensionsv1.CustomResourceDefinition
		err := c.Get(ctx, client.ObjectKey{Name: "bootcoperatorconfigs.node.bootc.dev"}, &crd)
		return crd.Status.Conditions, err
	}).WithTimeout(time.Minute).Should(ContainElement(And(
		HaveField("Type", apiextensionsv1.Established),
		HaveField("Status", apiextensionsv1.ConditionTrue),
	)), "waiting for the operator configuration API to be established")
}

// Both components may have started with defaults before configuration was
// restored. Restart and await each workload so later tests see the saved values.
func restartOperator(t *testing.T) {
	t.Helper()
	workloads := []string{
		"deployment/" + operatorDeployKey().Name,
		"daemonset/" + operatorDaemonSetKey().Name,
	}
	for _, workload := range workloads {
		for _, command := range [][]string{
			{"rollout", "restart", workload},
			{"rollout", "status", workload, "--timeout=3m"},
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			args := append([]string{
				"--kubeconfig", os.Getenv("KUBECONFIG"), "-n", testutil.OperatorNamespaceName,
			}, command...)
			out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
			cancel()
			if err != nil {
				t.Fatalf("restart operator workload %s: %v\n%s", workload, err, out)
			}
		}
	}
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
