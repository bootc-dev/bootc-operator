// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/test/e2e/e2eutil"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

// TestOperatorConfig changes shared operator configuration and must remain
// serial. Run it on a dedicated test cluster, like the other lifecycle tests.
func TestOperatorConfig(t *testing.T) {
	e2eutil.Providers(t, "bink")
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(3 * time.Minute)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	env := e2eutil.New(t)
	ctx := context.Background()
	original, err := readOperatorConfig(ctx, env.Client)
	g.Expect(err).NotTo(HaveOccurred())
	config := &bootcv1alpha1.BootcOperatorConfig{
		ObjectMeta: metav1.ObjectMeta{Name: bootcv1alpha1.BootcOperatorConfigName},
	}
	var nodeName string

	waitReady := func(component, node string, previousUID types.UID) *corev1.Pod {
		var pod *corev1.Pod
		g.Eventually(func() (*corev1.Pod, error) {
			var err error
			pod, err = configTestPod(ctx, env.Client, component, node)
			return pod, err
		}).Should(And(
			Not(BeNil()),
			HaveField("UID", Not(Equal(previousUID))),
			HaveField("Status.Conditions", ContainElement(And(
				HaveField("Type", corev1.PodReady),
				HaveField("Status", corev1.ConditionTrue),
			))),
		), "waiting for a new ready %s pod on node %q", component, node)
		return pod
	}
	restart := func(component, node string) {
		pod, err := configTestPod(ctx, env.Client, component, node)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(env.Client.Delete(ctx, pod)).To(Succeed())
		waitReady(component, node, pod.UID)
	}
	// Registered after env cleanup so configuration and the controller are
	// restored before the test's pool and worker are removed, even on failure.
	t.Cleanup(func() {
		if err := restoreOperatorConfig(ctx, env.Client, original); err != nil {
			t.Errorf("restore operator configuration: %v", err)
			return
		}
		restart("controller", "")
		if nodeName != "" {
			restart("daemon", nodeName)
		}
	})

	g.Expect(client.IgnoreNotFound(env.Client.Delete(ctx, config))).To(Succeed())
	restart("controller", "")
	nodeName = env.AddNode(t)
	// A digest avoids relying on registry tag resolution while its configuration
	// is absent. The pool selects only the worker provisioned by this test.
	pool := env.NewPool("config", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())
	waitReady("daemon", nodeName, "")
	waitStartupInterval := func(want time.Duration) configDaemonState {
		var state configDaemonState
		g.Eventually(func() (time.Duration, error) {
			var err error
			state, err = readConfigDaemonState(ctx, env.Client, nodeName)
			return state.StartupPollInterval, err
		}).Should(Equal(want))
		return state
	}
	current := waitStartupInterval(5 * time.Minute)

	for _, seconds := range []int32{17, 23} {
		config.Spec.Daemon = &bootcv1alpha1.OperatorDaemonConfig{StatusPollPeriodSeconds: &seconds}
		if config.ResourceVersion == "" {
			g.Expect(env.Client.Create(ctx, config)).To(Succeed())
		} else {
			g.Expect(env.Client.Update(ctx, config)).To(Succeed())
		}
		// Observe pod/container identity alongside its logged startup value.
		// This detects automatic restarts; the log is not a live settings endpoint.
		g.Consistently(func() (configDaemonState, error) {
			return readConfigDaemonState(ctx, env.Client, nodeName)
		}).WithTimeout(20*time.Second).WithPolling(pollInterval).Should(Equal(current),
			"editing configuration must not restart the daemon")
		restart("daemon", nodeName)
		current = waitStartupInterval(time.Duration(seconds) * time.Second)
	}

	// An ordinary installation upgrade must preserve the administrator's
	// instance, not recreate it from a default manifest or reset its settings.
	saved := config.DeepCopy()
	var deployment appsv1.Deployment
	g.Expect(env.Client.Get(ctx, operatorDeployKey(), &deployment)).To(Succeed())
	applyCurrentManifests(t, containerImage(t, deployment.Spec.Template.Spec.Containers, "manager"))
	waitForOperatorReady(t, g, ctx, env.Client)
	g.Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(config), config)).To(Succeed())
	g.Expect(config.UID).To(Equal(saved.UID))
	g.Expect(config.Spec).To(Equal(saved.Spec))
	restart("daemon", nodeName)
	current = waitStartupInterval(23 * time.Second)

	g.Expect(env.Client.Delete(ctx, config)).To(Succeed())
	g.Consistently(func() (configDaemonState, error) {
		return readConfigDaemonState(ctx, env.Client, nodeName)
	}).WithTimeout(20*time.Second).WithPolling(pollInterval).Should(Equal(current),
		"deleting configuration must not restart the daemon")
	restart("daemon", nodeName)
	waitStartupInterval(5 * time.Minute)
}

// A nil snapshot represents an absent instance. Callers require the CRD to be
// installed before reading or restoring configuration.
func readOperatorConfig(
	ctx context.Context,
	c client.Client,
) (*bootcv1alpha1.BootcOperatorConfig, error) {
	config := &bootcv1alpha1.BootcOperatorConfig{}
	err := c.Get(ctx, client.ObjectKey{Name: bootcv1alpha1.BootcOperatorConfigName}, config)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return config, err
}

// restoreOperatorConfig preserves existing server metadata when possible and
// restores only administrator-owned data after CRD deletion/recreation.
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
	restored := original.DeepCopy()
	restored.ObjectMeta = metav1.ObjectMeta{
		Name:        original.Name,
		Labels:      restored.Labels,
		Annotations: restored.Annotations,
	}
	if current == nil {
		return c.Create(ctx, restored)
	}
	current.Spec = restored.Spec
	current.Labels = restored.Labels
	current.Annotations = restored.Annotations
	return c.Update(ctx, current)
}

func configTestPod(
	ctx context.Context,
	c client.Client,
	component, node string,
) (*corev1.Pod, error) {
	options := []client.ListOption{
		client.InNamespace(testutil.OperatorNamespaceName),
		client.MatchingLabels{
			"app.kubernetes.io/name":      "bootc-operator",
			"app.kubernetes.io/component": component,
		},
	}
	if node != "" {
		options = append(options, client.MatchingFields{"spec.nodeName": node})
	}
	var pods corev1.PodList
	if err := c.List(ctx, &pods, options...); err != nil {
		return nil, err
	}
	var active []*corev1.Pod
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp.IsZero() {
			active = append(active, &pods.Items[i])
		}
	}
	if len(active) != 1 {
		return nil, fmt.Errorf(
			"expected one %s pod on node %q, found %d",
			component,
			node,
			len(active),
		)
	}
	return active[0], nil
}

type configDaemonState struct {
	UID                 types.UID
	ContainerID         string
	RestartCount        int32
	StartupPollInterval time.Duration
}

func readConfigDaemonState(
	ctx context.Context,
	c client.Client,
	node string,
) (configDaemonState, error) {
	var state configDaemonState
	pod, err := configTestPod(ctx, c, "daemon", node)
	if err != nil {
		return state, err
	}
	state.UID = pod.UID
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name == "daemon" && container.State.Running != nil {
			state.ContainerID = container.ContainerID
			state.RestartCount = container.RestartCount
		}
	}
	if state.ContainerID == "" {
		return state, fmt.Errorf("daemon container in pod %s is not running", pod.Name)
	}
	logCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(logCtx, "kubectl", "--kubeconfig", os.Getenv("KUBECONFIG"),
		"-n", pod.Namespace, "logs", pod.Name, "-c", "daemon").CombinedOutput()
	if err != nil {
		return state, fmt.Errorf("read daemon logs from %s: %w: %s", pod.Name, err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.Contains(line, "Starting daemon") {
			continue
		}
		// The default console encoder appends a JSON object with the fields;
		// the JSON encoder emits the entire record as an object.
		start := strings.IndexByte(line, '{')
		if start < 0 {
			return state, fmt.Errorf("daemon startup log has no fields: %s", line)
		}
		var fields struct {
			PollInterval string `json:"pollInterval"`
		}
		if err := json.Unmarshal([]byte(line[start:]), &fields); err != nil {
			return state, fmt.Errorf("parse daemon startup log: %w", err)
		}
		state.StartupPollInterval, err = time.ParseDuration(fields.PollInterval)
		return state, err
	}
	return state, fmt.Errorf("pod %s has no daemon startup log", pod.Name)
}
