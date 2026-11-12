// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/internal/config"
	"github.com/bootc-dev/bootc-operator/internal/controller"
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
// from its published Helm chart, verifies basic functionality, then
// upgrades by running helm upgrade with the current chart and image and
// verifies the operator still works.
func TestOperatorUpgrade(t *testing.T) {
	e2eutil.Providers(t, "bink", "eks")

	releaseTag := os.Getenv("E2E_OPERATOR_RELEASE_TAG")
	if releaseTag == "" {
		t.Skip("E2E_OPERATOR_RELEASE_TAG not set")
	}
	// Optional: overrides the image the released chart deploys, e.g.
	// with a copy in a local registry.
	releasedImg := os.Getenv("E2E_OPERATOR_RELEASED_IMG")
	releasedChart := os.Getenv("E2E_OPERATOR_RELEASED_CHART")
	if releasedChart == "" {
		t.Skip("E2E_OPERATOR_RELEASED_CHART not set")
	}
	releasedChartVersion := strings.TrimPrefix(releaseTag, "v")

	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	ctx := context.Background()
	// Registered before the Env so it runs after the Env cleanup removes
	// the test node. The restored controller may be scheduled on that
	// node; wait for its replacement to lead before parallel tests start.
	var env *e2eutil.Env
	t.Cleanup(func() {
		if env != nil {
			waitForOperatorLeader(t, g, ctx, env.Client)
		}
	})
	env = e2eutil.New(t)

	repoRoot := findRepoRoot(t)
	chartPath := filepath.Join(repoRoot, "chart", "bootc-operator")

	// Capture the current operator image and extra args before we
	// replace it.
	var origDeploy appsv1.Deployment
	g.Expect(env.Client.Get(ctx, operatorDeployKey(), &origDeploy)).To(Succeed())
	currentImg := containerImage(t, origDeploy.Spec.Template.Spec.Containers, "manager")
	extraArgs := controllerExtraArgs(t, origDeploy.Spec.Template.Spec.Containers)
	helmValues := readHelmControllerValues(t)
	pullSecretNamespaces := helmValues.PullSecretNamespaces
	// Preserve explicit namespace overrides separately from Helm-managed grants.
	_, namespaceOverrides := splitNamespaceArgs(helmValues.Controller.ExtraArgs)
	extraArgs = append(extraArgs, namespaceOverrides...)
	t.Logf("Current operator image: %s", currentImg)
	t.Logf("Released operator tag: %s, image: %s, chart: %s",
		releaseTag, releasedImg, releasedChart)

	originalConfig, err := readOperatorConfig(ctx, env.Client)
	g.Expect(err).NotTo(HaveOccurred())
	// Uninstall removes CRDs and their instances. Register recovery first.
	t.Cleanup(func() {
		t.Logf("Restoring operator to current version...")
		helmUpgradeOperator(t, chartPath, currentImg, extraArgs, pullSecretNamespaces)
		waitForOperatorConfigCRD(t, g, ctx, env.Client)
		g.Expect(restoreOperatorConfig(ctx, env.Client, originalConfig)).To(Succeed(),
			"restore administrator-owned operator configuration")
		restartOperator(t)
		waitForOperatorReady(t, g, ctx, env.Client)
		t.Logf("Operator restored")
	})

	// Phase 1: Replace the current operator with its published release chart.
	uninstallOperator(t, g, ctx, env)
	// The released image predates BootcOperatorConfig and needs bink's
	// registry settings through its command-line flags.
	releasedArgs := releasedControllerArgs(originalConfig, extraArgs)
	helmInstallOperator(t, releasedChart, releasedChartVersion,
		releasedImg, releasedArgs)
	waitForOperatorReady(t, g, ctx, env.Client)
	t.Logf("Released operator %s installed via Helm", releaseTag)

	// Phase 2: Verify the released version works.
	nodeName := env.AddNode(t)

	pool := env.NewPool("upgrade", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute)

	t.Logf("Node %q is Idle with released operator", nodeName)

	// Phase 3: Upgrade via Helm to the current version.
	helmUpgradeOperator(t, chartPath, currentImg, extraArgs, pullSecretNamespaces)
	waitForOperatorConfigCRD(t, g, ctx, env.Client)
	config, err := readOperatorConfig(ctx, env.Client)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(config).To(BeNil(), "installation must not create a configuration instance")
	g.Expect(restoreOperatorConfig(ctx, env.Client, originalConfig)).To(Succeed(),
		"restore administrator-owned operator configuration before upgrade checks")
	if originalConfig != nil {
		restartOperator(t)
	}
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
		HaveField("Booted", imageMatchesDigest(env.NodeImageUpdateDigest())),
	)

	t.Logf("Node %q completed update with upgraded operator", nodeName)

	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageUpdateDigest()))
}

// uninstallOperator removes the operator. Custom resources are deleted
// first while the operator is still running so it can handle finalizer
// removal. The test also removes a standalone CRD release if present.
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

	t.Logf("Uninstalling Helm releases...")
	crdsRelease := helmReleaseName + "-crds"
	hasCRDsRelease := helmReleaseExists(t, crdsRelease)
	helmRun(t, "uninstall", helmReleaseName,
		"--namespace", testutil.OperatorNamespaceName)
	if hasCRDsRelease {
		t.Logf("Uninstalling standalone CRDs release...")
		helmRun(t, "uninstall", crdsRelease,
			"--namespace", testutil.OperatorNamespaceName)
	}

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

// helmInstallOperator installs the given version of a published chart
// via helm install.
func helmInstallOperator(
	t *testing.T,
	chartRef, chartVersion, img string,
	extraArgs []string,
) {
	t.Helper()
	args := []string{
		"install", helmReleaseName, chartRef,
		"--version", chartVersion,
		"--namespace", testutil.OperatorNamespaceName,
		"--create-namespace",
	}
	args = append(args, helmImageArgs(t, img, extraArgs)...)
	helmRun(t, args...)
}

// helmUpgradeOperator upgrades the operator to the local chart via helm
// upgrade, installing it if cleanup follows a failed release installation.
// The release namespace is always allowed, so including it in
// pullSecretNamespaces is harmless.
func helmUpgradeOperator(
	t *testing.T,
	chartPath, img string,
	extraArgs, pullSecretNamespaces []string,
) {
	t.Helper()
	helmRun(t, "dependency", "build", chartPath)
	args := []string{
		"upgrade", "--install", helmReleaseName, chartPath,
		"--namespace", testutil.OperatorNamespaceName,
	}
	args = append(args, helmImageArgs(t, img, extraArgs)...)
	if len(pullSecretNamespaces) > 0 {
		j, err := json.Marshal(pullSecretNamespaces)
		if err != nil {
			t.Fatalf("marshaling pullSecretNamespaces: %v", err)
		}
		args = append(args, "--set-json", "pullSecretNamespaces="+string(j))
	}
	helmRun(t, args...)
}

// helmImageArgs returns the --set flags for image and E2E operator args.
// An empty img keeps the image of the chart.
func helmImageArgs(t *testing.T, img string, extraArgs []string) []string {
	t.Helper()
	args := []string{"--set-string", "image.pullPolicy=Always"}
	if img != "" {
		repo, tag := splitImage(img)
		args = append(args,
			"--set-string", "image.repository="+repo,
			"--set-string", "image.tag="+tag,
		)
	}
	if secret := os.Getenv("E2E_IMAGE_PULL_SECRET"); secret != "" {
		args = append(args, "--set", "imagePullSecrets[0].name="+secret)
	}
	if interval := os.Getenv("E2E_BOOTC_POLL_INTERVAL"); interval != "" {
		args = append(args, "--set-string", "daemon.extraArgs[0]=--bootc-poll-interval="+interval)
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

// helmReleaseExists returns true if the named Helm release exists in
// the operator namespace. Any error other than "release: not found"
// fails the test.
func helmReleaseExists(t *testing.T, name string) bool {
	t.Helper()
	args := []string{"status", name,
		"--namespace", testutil.OperatorNamespaceName}
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		args = append([]string{"--kubeconfig", kc}, args...)
	}
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true
	}
	if strings.Contains(string(out), "release: not found") {
		return false
	}
	t.Fatalf("helm status %s failed: %v\n%s", name, err, out)
	return false
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

// releasedControllerArgs carries CR settings to releases that only support CLI
// flags. Present CR fields come last and override Deployment arguments.
func releasedControllerArgs(cfg *bootcv1alpha1.BootcOperatorConfig, explicit []string) []string {
	args, _ := splitNamespaceArgs(explicit)
	if cfg != nil && cfg.Spec.Controller != nil {
		c := cfg.Spec.Controller
		if c.AllowInsecureRegistry != nil {
			args = append(
				args,
				fmt.Sprintf("--%s=%t", config.AllowInsecureRegistryFlag, *c.AllowInsecureRegistry),
			)
		}
		if c.TagResolutionPeriodSeconds != nil {
			period := time.Duration(*c.TagResolutionPeriodSeconds) * time.Second
			args = append(args, "--"+config.TagResolutionIntervalFlag+"="+period.String())
		}
	}
	return args
}

// TestReleasedControllerArgs verifies that older releases receive supported CR
// settings as flags with CR-first precedence, without unsupported namespace flags.
func TestReleasedControllerArgs(t *testing.T) {
	allowInsecure, seconds := true, int32(17)
	cfg := &bootcv1alpha1.BootcOperatorConfig{
		Spec: bootcv1alpha1.BootcOperatorConfigSpec{
			Controller: &bootcv1alpha1.OperatorControllerConfig{
				AllowInsecureRegistry: &allowInsecure, TagResolutionPeriodSeconds: &seconds,
				PullSecretNamespaces: []string{"from-cr"},
			},
		},
	}
	for _, tt := range []struct {
		name     string
		cfg      *bootcv1alpha1.BootcOperatorConfig
		explicit []string
		wantHTTP bool
		wantWait time.Duration
	}{
		{name: "absent config", wantWait: 5 * time.Minute},
		{name: "omitted fields", cfg: &bootcv1alpha1.BootcOperatorConfig{}, wantWait: 5 * time.Minute},
		{name: "saved settings", cfg: cfg, wantHTTP: true, wantWait: 17 * time.Second},
		{name: "omitted field retains CLI precision", cfg: &bootcv1alpha1.BootcOperatorConfig{}, explicit: []string{"--tag-resolution-interval=1500ms"}, wantWait: 1500 * time.Millisecond},
		{name: "omit unsupported namespace flag", cfg: cfg, explicit: []string{"--pull-secret-namespaces", "team-a"}, wantHTTP: true, wantWait: 17 * time.Second},
		{
			name: "CR fields override arguments", cfg: cfg,
			explicit: []string{"--allow-insecure-registry=false", "--tag-resolution-interval=1500ms"},
			wantHTTP: true, wantWait: 17 * time.Second,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			fs := flag.NewFlagSet(tt.name, flag.ContinueOnError)
			http := fs.Bool(config.AllowInsecureRegistryFlag, false, "")
			wait := fs.Duration(config.TagResolutionIntervalFlag, 5*time.Minute, "")
			g.Expect(fs.Parse(releasedControllerArgs(tt.cfg, tt.explicit))).To(Succeed())
			g.Expect(*http).To(Equal(tt.wantHTTP))
			g.Expect(*wait).To(Equal(tt.wantWait))
		})
	}
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
		args, _ := splitNamespaceArgs(c.Args)
		for _, arg := range args {
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

// controllerPullSecretNamespaces resolves the startup settings from CR and CLI.
// Call it only when the Deployment has restarted after configuration changes.
func controllerPullSecretNamespaces(
	t *testing.T, g Gomega, ctx context.Context, c client.Client,
) []string {
	t.Helper()
	var d appsv1.Deployment
	g.Expect(c.Get(ctx, operatorDeployKey(), &d)).To(Succeed())
	cfg, err := readOperatorConfig(ctx, c)
	g.Expect(err).NotTo(HaveOccurred())
	for _, cont := range d.Spec.Template.Spec.Containers {
		if cont.Name == "manager" {
			namespaces, err := effectivePullSecretNamespaces(cont.Args, cfg, d.Namespace)
			g.Expect(err).NotTo(HaveOccurred())
			return namespaces
		}
	}
	t.Fatal("manager container not found")
	return nil
}

func effectivePullSecretNamespaces(
	args []string, cfg *bootcv1alpha1.BootcOperatorConfig, operatorNamespace string,
) ([]string, error) {
	fs := flag.NewFlagSet("controller namespaces", flag.ContinueOnError)
	value := fs.String(config.PullSecretNamespacesFlag, "", "")
	_, namespaceArgs := splitNamespaceArgs(args)
	if err := fs.Parse(namespaceArgs); err != nil {
		return nil, err
	}
	if err := config.ApplyToFlags(fs, cfg); err != nil {
		return nil, err
	}
	return controller.ParsePullSecretNamespaces(*value, operatorNamespace)
}

// splitNamespaceArgs handles both flag forms and retains their order so the last
// explicit value wins. Older released operators must not receive this new flag.
func splitNamespaceArgs(args []string) (other, namespaces []string) {
	for i := 0; i < len(args); i++ {
		name := strings.TrimPrefix(args[i], "-")
		name = strings.TrimPrefix(name, "-")
		if name == config.PullSecretNamespacesFlag {
			namespaces = append(namespaces, args[i])
			if i+1 < len(args) {
				i++
				namespaces = append(namespaces, args[i])
			}
		} else if strings.HasPrefix(name, config.PullSecretNamespacesFlag+"=") {
			namespaces = append(namespaces, args[i])
		} else {
			other = append(other, args[i])
		}
	}
	return other, namespaces
}

type helmControllerValues struct {
	PullSecretNamespaces []string `json:"pullSecretNamespaces"`
	Controller           struct {
		ExtraArgs []string `json:"extraArgs"`
	} `json:"controller"`
}

// Read actual release values, rather than turning CR-derived namespaces into
// Helm-managed RBAC or a persistent CLI fallback during recovery.
func readHelmControllerValues(t *testing.T) helmControllerValues {
	t.Helper()
	args := []string{
		"get",
		"values",
		helmReleaseName,
		"--namespace",
		testutil.OperatorNamespaceName,
		"--output",
		"json",
	}
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		args = append([]string{"--kubeconfig", kc}, args...)
	}
	out, err := exec.Command("helm", args...).Output()
	if err != nil {
		t.Fatalf("read original Helm controller settings: %v", err)
	}
	var values helmControllerValues
	if err := json.Unmarshal(out, &values); err != nil {
		t.Fatalf("decode original Helm controller settings: %v", err)
	}
	return values
}

// TestEffectivePullSecretNamespaces checks CR-first namespace selection, CLI
// fallback for omitted fields, and the operator namespace always remaining allowed.
func TestEffectivePullSecretNamespaces(t *testing.T) {
	cfg := &bootcv1alpha1.BootcOperatorConfig{Spec: bootcv1alpha1.BootcOperatorConfigSpec{
		Controller: &bootcv1alpha1.OperatorControllerConfig{
			PullSecretNamespaces: []string{"from-cr"},
		},
	}}
	empty := cfg.DeepCopy()
	empty.Spec.Controller.PullSecretNamespaces = []string{}
	for _, tc := range []struct {
		name       string
		cfg        *bootcv1alpha1.BootcOperatorConfig
		args, want []string
	}{
		{"defaults", nil, nil, []string{"operator"}},
		{"CR only", cfg, nil, []string{"operator", "from-cr"}},
		{"empty CR overrides CLI", empty, []string{"--pull-secret-namespaces=from-cli"}, []string{"operator"}},
		{"CLI only", nil, []string{"--pull-secret-namespaces=from-cli"}, []string{"operator", "from-cli"}},
		{"CR overrides CLI", cfg, []string{"--pull-secret-namespaces", "from-cli"}, []string{"operator", "from-cr"}},
		{"CR overrides empty CLI", cfg, []string{"--pull-secret-namespaces="}, []string{"operator", "from-cr"}},
		{"last CLI flag wins without CR field", nil, []string{"--pull-secret-namespaces=first", "--pull-secret-namespaces=last"}, []string{"operator", "last"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			got, err := effectivePullSecretNamespaces(tc.args, tc.cfg, "operator")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tc.want))
		})
	}
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
		g.Expect(d.Status.ObservedGeneration).To(BeNumerically(">=", d.Generation))
		g.Expect(d.Status.Replicas).To(Equal(int32(1)))
		g.Expect(d.Status.UpdatedReplicas).To(Equal(int32(1)))
		g.Expect(d.Status.AvailableReplicas).To(Equal(int32(1)))
	}).WithTimeout(3*time.Minute).Should(Succeed(),
		"expected operator deployment to be ready")
}

// waitForOperatorLeader waits until the only running controller pod holds
// the leader election lease. A ready pod does not lead until the lease of
// a terminated predecessor expires.
func waitForOperatorLeader(
	t *testing.T,
	g Gomega,
	ctx context.Context,
	c client.Client,
) {
	t.Helper()
	g.Eventually(func(g Gomega) {
		var pods corev1.PodList
		g.Expect(c.List(ctx, &pods,
			client.InNamespace(testutil.OperatorNamespaceName),
			client.MatchingLabels{
				"app.kubernetes.io/name":      "bootc-operator",
				"app.kubernetes.io/component": "controller",
			},
		)).To(Succeed())
		var running []string
		for _, p := range pods.Items {
			if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning {
				running = append(running, p.Name)
			}
		}
		g.Expect(running).To(HaveLen(1))

		var lease coordinationv1.Lease
		g.Expect(c.Get(ctx, client.ObjectKey{
			Namespace: testutil.OperatorNamespaceName,
			Name:      "bootc-operator.bootc.dev",
		}, &lease)).To(Succeed())
		g.Expect(lease.Spec.HolderIdentity).NotTo(BeNil())
		g.Expect(*lease.Spec.HolderIdentity).To(HavePrefix(running[0] + "_"))
	}).WithTimeout(3*time.Minute).Should(Succeed(),
		"expected the running controller to hold the leader election lease")
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
