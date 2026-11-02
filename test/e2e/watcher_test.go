// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/test/e2e/e2eutil"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

// TestExternalBootcStatusChange verifies that an external host operation is
// reflected in BootcNode status through fsnotify, without a spec change or
// daemon restart triggering reconciliation.
func TestExternalBootcStatusChange(t *testing.T) {
	e2eutil.Providers(t, "bink")
	t.Parallel()
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	// Match the interval configured by deploy-bink and preserved by the upgrade test.
	pollIntervalArg := os.Getenv("E2E_BOOTC_POLL_INTERVAL")
	fallbackPollInterval, err := time.ParseDuration(pollIntervalArg)
	g.Expect(err).NotTo(HaveOccurred(), "E2E_BOOTC_POLL_INTERVAL must be a duration")
	g.Expect(fallbackPollInterval).To(BeNumerically(">", 0))
	env := e2eutil.New(t)
	workerCreationStart := time.Now()
	nodeName := env.AddNode(t)
	ctx, cancelTest := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancelTest()

	// Keep the desired image equal to the booted image so the daemon itself
	// has no staging work. Pausing also prevents new reboot slots.
	pool := env.NewPool(
		"external-status",
		env.NodeImageDigestedPullSpec(),
		testutil.WithPaused(true),
	)
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())
	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute,
		HaveField("Booted", imageMatchesDigest(env.NodeImageDigest())),
		HaveField("Staged", BeNil()),
	)

	var before bootcv1alpha1.BootcNode
	g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &before)).To(Succeed())
	pods, err := testutil.DaemonPodsOnNode(ctx, env.Client, nodeName)()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pods).To(HaveLen(1))
	pod := pods[0]
	g.Expect(pod.Status.ContainerStatuses).To(ConsistOf(HaveField("RestartCount", int32(0))))
	g.Expect(pod.Spec.Containers).To(HaveLen(1))
	g.Expect(append(pod.Spec.Containers[0].Command, pod.Spec.Containers[0].Args...)).
		To(ContainElement("--bootc-poll-interval="+pollIntervalArg),
			"the test relies on the configured E2E polling interval")

	const stageUnit = "bootc-e2e-external-switch.service"
	// Killing kubectl does not stop a transient service on the host. Run this
	// cleanup before the harness gathers logs and removes the worker, with a
	// fresh context so a failed or timed-out test can still stop the service.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		run := func(args ...string) ([]byte, error) {
			base := []string{
				"--kubeconfig", os.Getenv("KUBECONFIG"), "-n", pod.Namespace,
				"exec", pod.Name, "-c", "daemon", "--",
				"nsenter", "--target", "1", "--mount", "--pid", "--cgroup",
				"--setuid", "0", "--setgid", "0", "--env", "--", "systemctl",
			}
			return exec.CommandContext(cleanupCtx, "kubectl", append(base, args...)...).
				CombinedOutput()
		}
		if out, err := run("stop", stageUnit); err != nil {
			// --collect removes a completed unit; stopping it then reports an
			// error. Ignore only that case, preserving transport/stop failures.
			state, stateErr := run("show", stageUnit, "--property=LoadState", "--value")
			if stateErr != nil || strings.TrimSpace(string(state)) != "not-found" {
				t.Errorf("stopping external staging unit: %s: %v (load state: %s, %v)",
					out, err, state, stateErr)
			}
		}
	})

	// Use a separate unit from the daemon's bootc-operator-switch.service.
	// Enter the host environment as the daemon does, avoiding container env
	// and SELinux restrictions on running bootc directly inside the pod.
	stageCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	execOnNode(t, g, env, stageCtx, nodeName,
		"nsenter", "--target", "1", "--mount", "--pid", "--cgroup",
		"--setuid", "0", "--setgid", "0", "--env", "--",
		"systemd-run", "--wait", "--collect", "--unit="+stageUnit,
		"--property=RuntimeMaxSec=240s", "--property=TimeoutStopSec=15s",
		"bootc", "switch", env.NodeImageUpdateDigestedPullSpec(),
	)

	// This is well below the configured E2E fallback polling interval.
	observationCtx, cancelObservation := context.WithTimeout(ctx, 30*time.Second)
	defer cancelObservation()
	testutil.WaitForNodeIdle(t, g, observationCtx, env.Client, nodeName, 30*time.Second,
		HaveField("Booted", imageMatchesDigest(env.NodeImageDigest())),
		HaveField("Staged", imageMatchesDigest(env.NodeImageUpdateDigest())),
	)
	g.Expect(observationCtx.Err()).
		NotTo(HaveOccurred(), "status must be observed within 30 seconds")

	// Fallback polling waits for the configured interval after watcher startup.
	// This bound starts before the worker exists and uses the runner's clock,
	// so a fallback poll cannot account for the observed status update.
	g.Expect(time.Since(workerCreationStart)).To(BeNumerically("<", fallbackPollInterval),
		"status must be observed before the first possible fallback poll")

	var after bootcv1alpha1.BootcNode
	g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &after)).To(Succeed())
	g.Expect(after.UID).To(Equal(before.UID))
	g.Expect(after.Generation).
		To(Equal(before.Generation), "the BootcNode spec must remain unchanged")

	var currentPod corev1.Pod
	g.Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(&pod), &currentPod)).To(Succeed())
	g.Expect(currentPod.UID).To(Equal(pod.UID))
	g.Expect(currentPod.Status.ContainerStatuses).To(ConsistOf(HaveField("RestartCount", int32(0))),
		"a daemon restart must not supply a fresh startup status read")
}
