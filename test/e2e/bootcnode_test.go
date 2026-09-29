// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/internal/image"
	"github.com/bootc-dev/bootc-operator/test/e2e/e2eutil"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

const (
	pollTimeout  = 60 * time.Second
	pollInterval = 2 * time.Second
)

// TestControllerMembership provisions a worker node, creates a
// BootcNodePool selecting it, and verifies that a BootcNode is created
// and the node is labeled bootc.dev/managed.
func TestControllerMembership(t *testing.T) {
	e2eutil.Providers(t, "bink")
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeName := env.AddNode(t)

	ctx := context.Background()

	pool := env.NewPool("workers", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	// Wait for BootcNode to appear and verify ownerReference.
	g.Eventually(func() (*metav1.OwnerReference, error) {
		var bn bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return metav1.GetControllerOf(&bn), err
	}).Should(And(Not(BeNil()), HaveField("Name", pool.Name)))

	// Verify desiredImage.
	g.Eventually(func() (string, error) {
		var bn bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Spec.DesiredImage, err
	}).Should(Equal(env.NodeImageDigestedPullSpec()))

	// Verify the worker has the managed label.
	var node corev1.Node
	g.Eventually(func() (map[string]string, error) {
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &node)
		return node.Labels, err
	}).Should(HaveKey(bootcv1alpha1.LabelManaged))

	g.Eventually(func() ([]corev1.Pod, error) {
		var pods corev1.PodList
		err := env.Client.List(ctx, &pods,
			client.InNamespace(testutil.OperatorNamespaceName),
			client.MatchingLabels{
				"app.kubernetes.io/name":      "bootc-operator",
				"app.kubernetes.io/component": "daemon",
			},
		)
		return pods.Items, err
	}).WithTimeout(3*time.Minute).Should(ConsistOf(And(
		HaveField("Spec.NodeName", nodeName),
		HaveField("Status.Phase", corev1.PodRunning),
	)), "expected exactly one running daemon pod on %s", nodeName)

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute,
		HaveField("Booted", And(
			HaveField("Image", env.NodeImageDigestedPullSpec()),
			HaveField("ImageDigest", env.NodeImageDigest()),
		)),
	)

	// Verify pool status reflects steady state.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageDigest()))
}

// TestUpdateReboot provisions a worker node, creates a pool with the
// original image, then updates the pool to a new image and verifies the
// full update lifecycle: staging, reboot, and idle with the new image.
func TestUpdateReboot(t *testing.T) {
	e2eutil.Providers(t, "bink", "eks")
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeName := env.AddNode(t)

	ctx := context.Background()

	// Phase 1: Create pool with original image and wait for Idle.
	pool := env.NewPool("workers", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute)

	t.Logf("Node %q is Idle with original image", nodeName)

	var bootcNode bootcv1alpha1.BootcNode
	g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bootcNode)).To(Succeed())

	// Phase 2: Patch pool to update image.
	updateRef := env.NodeImageUpdateDigestedPullSpec()

	modified := pool.DeepCopy()
	modified.Spec.Image.Ref = updateRef
	g.Expect(env.Client.Patch(ctx, modified, client.MergeFrom(pool))).To(Succeed())
	*pool = *modified

	t.Logf("Patched pool to update image %s", updateRef)

	// Phase 3: Wait for Rebooting — the daemon skips reconciliation after
	// issuing a reboot, so this state is durable until the node goes down.
	g.Eventually(func() ([]metav1.Condition, error) {
		var bn bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Status.Conditions, err
	}).WithTimeout(5*time.Minute).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.NodeIdle),
		HaveField("Status", metav1.ConditionFalse),
		HaveField("Reason", bootcv1alpha1.NodeReasonRebooting),
	)), "expected node to reach Rebooting state")

	t.Logf("Node %q is Rebooting", nodeName)

	nodeEvents := []struct {
		reason string
		note   string
	}{
		{
			reason: bootcv1alpha1.NodeReasonStaging,
			note:   "Staging image " + updateRef,
		},
		{
			reason: bootcv1alpha1.NodeReasonStaged,
			note:   "Image " + updateRef + " is staged and awaiting reboot",
		},
		{
			reason: bootcv1alpha1.NodeReasonRebooting,
			note:   "Rebooting into image " + updateRef,
		},
	}
	for _, expected := range nodeEvents {
		g.Eventually(fetchEvents(ctx, env.Client, "BootcNode", nodeName, bootcNode.UID)).
			Should(ContainElement(And(
				HaveField("Type", corev1.EventTypeNormal),
				HaveField("Reason", expected.reason),
				HaveField("Action", "NodeUpdate"),
				HaveField("Note", expected.note),
				HaveField("Related", And(
					Not(BeNil()),
					HaveField("UID", pool.UID),
				)),
			)))
	}
	g.Eventually(fetchEvents(ctx, env.Client, "BootcNodePool", pool.Name, pool.UID)).
		Should(ContainElement(And(
			HaveField("Type", corev1.EventTypeNormal),
			HaveField("Reason", "RolloutStarted"),
			HaveField("Action", "Rollout"),
			HaveField(
				"Note",
				"Rollout started toward digest "+testutil.ShortDigest(env.NodeImageUpdateDigest()),
			),
		)))

	// Verify pool status during rollout.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).Should(And(
		HaveField("NodeCount", BeEquivalentTo(1)),
		HaveField("UpdatedCount", BeEquivalentTo(0)),
		HaveField("UpdatingCount", BeEquivalentTo(1)),
		HaveField("UpdateAvailable", BeTrue()),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.PoolUpToDate),
			HaveField("Status", metav1.ConditionFalse),
			HaveField("Reason", bootcv1alpha1.PoolRolloutInProgress),
		))),
	))

	// Phase 4: Wait for Idle with the update digest — proves the full
	// update lifecycle completed (staging, reboot, boot into new image).
	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 5*time.Minute,
		HaveField("Booted", imageMatchesDigest(env.NodeImageUpdateDigest())),
	)

	t.Logf("Node %q is Idle with update image", nodeName)

	// Verify pool status after rollout completes.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageUpdateDigest()))
	g.Eventually(fetchEvents(ctx, env.Client, "BootcNodePool", pool.Name, pool.UID)).
		Should(ContainElement(And(
			HaveField("Type", corev1.EventTypeNormal),
			HaveField("Reason", "RolloutCompleted"),
			HaveField("Action", "Rollout"),
			HaveField(
				"Note",
				"Rollout completed at digest "+testutil.ShortDigest(env.NodeImageUpdateDigest()),
			),
		)))

	// Phase 5: Verify node is schedulable (uncordoned after reboot).
	g.Eventually(func() (bool, error) {
		var node corev1.Node
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &node)
		return node.Spec.Unschedulable, err
	}).WithTimeout(3*time.Minute).Should(BeFalse(), "expected node to be schedulable after update")

	// Phase 6: Rollback to original image.
	originalRef := env.NodeImageDigestedPullSpec()

	modified = pool.DeepCopy()
	modified.Spec.Image.Ref = originalRef
	g.Expect(env.Client.Patch(ctx, modified, client.MergeFrom(pool))).To(Succeed())
	*pool = *modified

	t.Logf("Patched pool to rollback to original image %s", originalRef)

	// Phase 8: Wait for Idle with the original digest — proves rollback succeeded.
	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 5*time.Minute,
		HaveField("Booted", imageMatchesDigest(env.NodeImageDigest())),
	)

	t.Logf("Node %q successfully rolled back to original image", nodeName)

	// Verify pool status after rollback completes.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageDigest()))
}

// TestTagResolution creates a pool with a tag-based image ref, verifies
// the controller resolves the tag to a digest, then retags the image
// and verifies re-resolution triggers a rollout.
func TestTagResolution(t *testing.T) {
	e2eutil.Providers(t, "bink")
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)

	ctx := context.Background()

	nodeName := env.AddNode(t)

	// The seed step already pushed node:latest with the original image.
	// Create a pool using the tag ref.
	pool := env.NewPool("tag", env.NodeImageTagRef())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	// Verify targetDigest is resolved to the original image digest.
	g.Eventually(func() (string, error) {
		var p bootcv1alpha1.BootcNodePool
		err := env.Client.Get(ctx, client.ObjectKeyFromObject(pool), &p)
		return p.Status.TargetDigest, err
	}).WithTimeout(1 * time.Minute).Should(Equal(env.NodeImageDigest()))

	t.Logf("Tag resolved to original digest %s", env.NodeImageDigest())

	// Wait for node to reach Idle with the original image.
	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute,
		HaveField("Booted", HaveField("ImageDigest", Equal(env.NodeImageDigest()))),
	)

	t.Logf("Node %q is Idle with original image", nodeName)

	// Retag node:latest to point at the update image.
	e2eutil.RetagImage(t,
		"localhost:5000/node@"+env.NodeImageUpdateDigest(),
		"localhost:5000/node:latest",
	)
	// Restore the original tag on cleanup so later tests are not affected.
	t.Cleanup(func() {
		e2eutil.RetagImage(t,
			"localhost:5000/node@"+env.NodeImageDigest(),
			"localhost:5000/node:latest",
		)
	})

	t.Logf("Retagged node:latest to update digest %s", env.NodeImageUpdateDigest())

	// Wait for the controller to re-resolve and pick up the new digest.
	g.Eventually(func() (string, error) {
		var p bootcv1alpha1.BootcNodePool
		err := env.Client.Get(ctx, client.ObjectKeyFromObject(pool), &p)
		return p.Status.TargetDigest, err
	}).WithTimeout(1 * time.Minute).Should(Equal(env.NodeImageUpdateDigest()))

	t.Logf("Tag re-resolved to update digest %s", env.NodeImageUpdateDigest())

	g.Eventually(fetchEvents(ctx, env.Client, "BootcNodePool", pool.Name, pool.UID)).
		Should(ContainElement(And(
			HaveField("Type", corev1.EventTypeNormal),
			HaveField("Reason", "ImageUpdateAvailable"),
			HaveField("Action", "ResolveImage"),
			HaveField(
				"Note",
				fmt.Sprintf(
					"Image tag %s resolved to new digest %s (previously %s)",
					env.NodeImageTagRef(),
					testutil.ShortDigest(env.NodeImageUpdateDigest()),
					testutil.ShortDigest(env.NodeImageDigest()),
				),
			),
		)))

	// Wait for node to reach Idle with the update image.
	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 5*time.Minute,
		HaveField("Booted", HaveField("ImageDigest", Equal(env.NodeImageUpdateDigest()))),
	)

	t.Logf("Node %q is Idle with update image", nodeName)
}

// TestMidRolloutImageChange provisions two worker nodes, starts a rollout
// to one update image, then switches the target to a different update image
// while one node is rebooting. It verifies both nodes converge to the final
// image and that the non-rebooting node does not wastefully reboot into the
// first update image.
func TestMidRolloutImageChange(t *testing.T) {
	e2eutil.Providers(t, "bink")
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeA := env.AddNode(t)
	nodeB := env.AddNode(t)

	ctx := context.Background()

	// Phase 1: Create pool with original image and wait for both nodes Idle.
	pool := env.NewPool("mid-rollout", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	for _, nodeName := range []string{nodeA, nodeB} {
		testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute)
	}

	t.Logf("Both nodes are Idle with original image")

	// Phase 2: Record boot count for both nodes before the rollout.
	bootCountBefore := make(map[string]string)
	for _, nodeName := range []string{nodeA, nodeB} {
		bootCountBefore[nodeName] = getBootCount(t, env, ctx, nodeName)
		t.Logf("Node %q boot count before: %s", nodeName, bootCountBefore[nodeName])
	}

	// Phase 3: Patch pool to first update image.
	updateRef1 := env.NodeImageUpdateDigestedPullSpec()

	modified := pool.DeepCopy()
	modified.Spec.Image.Ref = updateRef1
	g.Expect(env.Client.Patch(ctx, modified, client.MergeFrom(pool))).To(Succeed())
	*pool = *modified

	t.Logf("Patched pool to first update image %s", updateRef1)

	// Phase 4: Wait for any node to reach Rebooting.
	var rebootingNode string
	g.Eventually(func(g Gomega) string {
		for _, name := range []string{nodeA, nodeB} {
			var bn bootcv1alpha1.BootcNode
			g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: name}, &bn)).To(Succeed())

			cond := meta.FindStatusCondition(bn.Status.Conditions, bootcv1alpha1.NodeIdle)
			if cond != nil &&
				cond.Status == metav1.ConditionFalse &&
				cond.Reason == bootcv1alpha1.NodeReasonRebooting {
				rebootingNode = name
				return name
			}
		}
		return ""
	}).WithTimeout(5 * time.Minute).ShouldNot(BeEmpty())

	var otherNode string
	if rebootingNode == nodeA {
		otherNode = nodeB
	} else {
		otherNode = nodeA
	}

	t.Logf("Node %q is Rebooting, node %q is the other node", rebootingNode, otherNode)

	// Phase 5: Immediately switch target to second update image.
	updateRef2 := env.NodeImageUpdate2DigestedPullSpec()

	modified = pool.DeepCopy()
	modified.Spec.Image.Ref = updateRef2
	g.Expect(env.Client.Patch(ctx, modified, client.MergeFrom(pool))).To(Succeed())
	*pool = *modified

	t.Logf("Switched pool to second update image %s", updateRef2)

	// Phase 6: Wait for both nodes to be Idle with the second update image.
	for _, nodeName := range []string{nodeA, nodeB} {
		testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 8*time.Minute,
			HaveField("Booted", HaveField("ImageDigest", Equal(env.NodeImageUpdate2Digest()))),
		)
	}

	t.Logf("Both nodes are Idle with second update image")

	// Phase 7: Verify the other node (the one that was NOT rebooting when
	// we switched images) did not wastefully reboot into the first update
	// image. It should have rebooted exactly once (into the second image).
	bootCountAfter := getBootCount(t, env, ctx, otherNode)
	t.Logf(
		"Node %q boot count after: %s (before: %s)",
		otherNode,
		bootCountAfter,
		bootCountBefore[otherNode],
	)

	beforeCount, err := strconv.Atoi(bootCountBefore[otherNode])
	g.Expect(err).NotTo(HaveOccurred(), "parsing before boot count")
	afterCount, err := strconv.Atoi(bootCountAfter)
	g.Expect(err).NotTo(HaveOccurred(), "parsing after boot count")

	g.Expect(afterCount-beforeCount).To(Equal(1),
		"expected other node %s to reboot exactly once (from %d to %d), "+
			"an extra reboot means it wastefully booted into the first update image",
		otherNode, beforeCount, afterCount)

	t.Logf(
		"Verified node %q rebooted exactly once (no wasteful reboot into first image)",
		otherNode,
	)
}

// execOnNode finds the running daemon pod on the given node and executes
// a command inside it via kubectl exec. It returns the command output.
func execOnNode(
	t *testing.T,
	g Gomega,
	env *e2eutil.Env,
	ctx context.Context,
	nodeName string,
	command ...string,
) string {
	t.Helper()

	var daemonPod corev1.Pod
	g.Eventually(func(g Gomega) string {
		var pods corev1.PodList
		g.Expect(env.Client.List(ctx, &pods,
			client.InNamespace(testutil.OperatorNamespaceName),
			client.MatchingLabels{
				"app.kubernetes.io/name":      "bootc-operator",
				"app.kubernetes.io/component": "daemon",
			},
		)).To(Succeed())
		for _, p := range pods.Items {
			if p.Spec.NodeName == nodeName && p.Status.Phase == corev1.PodRunning {
				daemonPod = p
				return p.Name
			}
		}
		return ""
	}).WithTimeout(1*time.Minute).ShouldNot(BeEmpty(),
		"expected running daemon pod on %s", nodeName)

	kubeconfigPath := os.Getenv("KUBECONFIG")
	args := append([]string{"--kubeconfig", kubeconfigPath,
		"-n", testutil.OperatorNamespaceName, "exec", daemonPod.Name, "--"}, command...)
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(),
		fmt.Sprintf("kubectl exec on %s failed: %s", nodeName, string(out)))

	return strings.TrimSpace(string(out))
}

// getBootCount returns the number of boots on a node by running
// journalctl --list-boots inside the daemon pod via kubectl exec.
func getBootCount(t *testing.T, env *e2eutil.Env, ctx context.Context, nodeName string) string {
	t.Helper()

	g := NewWithT(t)

	out := execOnNode(t, g, env, ctx, nodeName,
		"nsenter", "-m/proc/1/ns/mnt", "--", "journalctl", "--list-boots")

	count := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return fmt.Sprintf("%d", count)
}

// TestPauseResume provisions a worker node, starts an update with the
// pool paused, verifies the node stages but does not reboot, then resumes
// and verifies the update completes.
func TestPauseResume(t *testing.T) {
	e2eutil.Providers(t, "bink")
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeName := env.AddNode(t)

	ctx := context.Background()

	// Phase 1: Create pool with original image and wait for Idle.
	pool := env.NewPool("bnp-pause", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute)

	t.Logf("Node %q is Idle with original image", nodeName)

	var bn bootcv1alpha1.BootcNode

	// Phase 2: Patch pool to update image with paused=true.
	updateRef := env.NodeImageUpdateDigestedPullSpec()

	modified := pool.DeepCopy()
	modified.Spec.Image.Ref = updateRef
	if modified.Spec.Rollout == nil {
		modified.Spec.Rollout = &bootcv1alpha1.RolloutSpec{}
	}
	modified.Spec.Rollout.Paused = true
	g.Expect(env.Client.Patch(ctx, modified, client.MergeFrom(pool))).To(Succeed())
	*pool = *modified

	t.Logf("Patched pool to update image %s with paused=true", updateRef)

	// Phase 3: Wait for node to stage the image. The node should reach
	// Staged state but not proceed to reboot because the pool is paused.
	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Status, err
	}).WithTimeout(5 * time.Minute).Should(And(
		HaveField("Staged", And(
			Not(BeNil()),
			HaveField("ImageDigest", Equal(env.NodeImageUpdateDigest())),
		)),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionFalse),
			HaveField("Reason", bootcv1alpha1.NodeReasonStaged),
		))),
		HaveField("Booted", And(
			Not(BeNil()),
			HaveField("ImageDigest", Equal(env.NodeImageDigest())),
		)),
	))

	t.Logf("Node %q staged update but did not reboot (paused)", nodeName)

	// Verify pool status while paused.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).Should(And(
		HaveField("NodeCount", BeEquivalentTo(1)),
		HaveField("UpdatedCount", BeEquivalentTo(0)),
		HaveField("UpdateAvailable", BeTrue()),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.PoolUpToDate),
			HaveField("Status", metav1.ConditionFalse),
			HaveField("Reason", bootcv1alpha1.PoolPaused),
		))),
	))

	// Verify the node stays Staged and does not proceed to reboot.
	g.Consistently(func() ([]metav1.Condition, error) {
		var bn2 bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn2)
		return bn2.Status.Conditions, err
	}).WithTimeout(10*time.Second).WithPolling(2*time.Second).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.NodeIdle),
		HaveField("Status", metav1.ConditionFalse),
		HaveField("Reason", bootcv1alpha1.NodeReasonStaged),
	)), "node should remain Staged while paused")

	// Phase 4: Resume the rollout.
	modified = pool.DeepCopy()
	modified.Spec.Rollout.Paused = false
	g.Expect(env.Client.Patch(ctx, modified, client.MergeFrom(pool))).To(Succeed())
	*pool = *modified

	t.Logf("Resumed rollout (paused=false)")

	// Phase 5: Wait for node to complete the update — proves the full
	// update lifecycle completed after resume (reboot, boot into new image).
	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 5*time.Minute,
		HaveField("Booted", HaveField("ImageDigest", Equal(env.NodeImageUpdateDigest()))),
	)

	t.Logf("Node %q completed update after resume", nodeName)

	// Verify pool status after resume completes.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageUpdateDigest()))
}

// TestNonExistingImage provisions a worker node, creates a pool with the
// original image, then updates to a non-existing image and verifies the
// node enters degraded state and the update does not proceed.
func TestNonExistingImage(t *testing.T) {
	e2eutil.Providers(t, "bink")
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeName := env.AddNode(t)

	ctx := context.Background()

	// Phase 1: Create pool with original image and wait for Idle.
	pool := env.NewPool("bnp-noimg", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute)

	t.Logf("Node %q is Idle with original image", nodeName)

	var bn bootcv1alpha1.BootcNode

	// Phase 2: Patch pool to update to a non-existing image.
	nonExistingRef := "localhost:5000/node@sha256:0000000000000000000000000000000000000000000000000000000000000000"

	modified := pool.DeepCopy()
	modified.Spec.Image.Ref = nonExistingRef
	g.Expect(env.Client.Patch(ctx, modified, client.MergeFrom(pool))).To(Succeed())
	*pool = *modified

	t.Logf("Patched pool to non-existing image %s", nonExistingRef)

	// Phase 3: Wait for node to enter degraded state.
	// The daemon should fail to pull the image and report an error.
	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Status, err
	}).WithTimeout(5*time.Minute).Should(And(
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeDegraded),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonError),
			HaveField("Message", ContainSubstring("stage failed")),
		))),
		HaveField("Booted", And(
			Not(BeNil()),
			HaveField("ImageDigest", Equal(env.NodeImageDigest())),
		)),
	), "expected node to enter degraded state when pulling non-existing image")

	t.Logf("Node %q entered degraded state as expected", nodeName)

	// Verify pool status reflects degraded node.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).Should(And(
		HaveField("NodeCount", BeEquivalentTo(1)),
		HaveField("UpdatedCount", BeEquivalentTo(0)),
		HaveField("UpdatingCount", BeEquivalentTo(0)),
		HaveField("DegradedCount", BeEquivalentTo(1)),
		HaveField("UpdateAvailable", BeTrue()),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.PoolDegraded),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.PoolNodeDegraded),
		))),
	))

	// Phase 4: Verify the node did not stage the non-existing image.
	g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)).To(Succeed())
	g.Expect(bn.Status.Staged).To(BeNil(),
		"node should not have staged the non-existing image")

	t.Logf("Verified node %q did not stage non-existing image", nodeName)
}

// TestPullSecretAuth provisions a worker node, creates a
// dockerconfigjson Secret with credentials, and verifies that the
// daemon can stage from the auth-protected registry. The auth
// registry shares storage with the unauthenticated one (port 5000),
// so the update image is already available at both endpoints.
func TestPullSecretAuth(t *testing.T) {
	e2eutil.Providers(t, "bink", "eks")
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	if env.RegistryUser() == "" || env.RegistryPassword() == "" {
		t.Skip("E2E_REGISTRY_USER / E2E_REGISTRY_PASSWORD not set")
	}

	ctx := context.Background()
	nodeName := env.AddNode(t)

	// For bink, the auth registry (port 5001) shares storage with the
	// unauthenticated registry (port 5000), so we rewrite the image ref
	// to go through the auth endpoint. For EKS, the update image already
	// requires authentication.
	authImageRef := env.AuthImageRef()
	digest := env.NodeImageUpdateDigest()
	registryHost := env.RegistryHost()

	authStr := base64.StdEncoding.EncodeToString(
		[]byte(env.RegistryUser() + ":" + env.RegistryPassword()),
	)
	dockerCfg := fmt.Sprintf(
		`{"auths":{%q:{"auth":"%s"}}}`,
		registryHost, authStr,
	)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      env.TestID() + "-pull-secret",
			Namespace: testutil.OperatorNamespaceName,
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(dockerCfg),
		},
	}
	g.Expect(env.Client.Create(ctx, secret)).To(Succeed())
	t.Cleanup(func() { _ = env.Client.Delete(ctx, secret) })

	pool := env.NewPool("pullsecret", authImageRef,
		testutil.WithPullSecret(secret.Name, secret.Namespace),
	)
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	// Verify BootcNode gets the pullSecretRef.
	g.Eventually(func() (*bootcv1alpha1.PullSecretRef, error) {
		var bn bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Spec.PullSecretRef, err
	}).Should(Equal(&bootcv1alpha1.PullSecretRef{
		Name: secret.Name, Namespace: secret.Namespace,
	}))

	t.Logf("BootcNode %q has pullSecretRef set", nodeName)

	// Wait for the node to stage and reboot into the update image.
	// Check Booted.Image (the full ref with manifest digest) rather
	// than Booted.ImageDigest (the content digest) because they can
	// differ with remote registries.
	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 5*time.Minute,
		HaveField("Booted", HaveField("Image", Equal(authImageRef))),
	)

	t.Logf("Node %q booted into auth-registry image", nodeName)

	// Verify pool status reflects steady state.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, digest))
}

// TestControllerRecovery provisions a worker node, starts a rollout, and
// scales the controller deployment to zero while that rollout is actively in
// progress (the node is staging and cannot reboot until the controller drains
// it and flips the desired state). It then scales the controller back up and
// verifies it resumes and completes the interrupted rollout. Covers scenario 5
// of #69 (kill the controller during a roll-out).
func TestControllerRecovery(t *testing.T) {
	e2eutil.Providers(t, "bink")
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeName := env.AddNode(t)

	ctx := context.Background()

	// Phase 1: Create pool with original image and wait for Idle.
	pool := env.NewPool("bnp-recovery", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute)

	t.Logf("Node %q is Idle with original image", nodeName)

	// Phase 2-3: Patch the pool to the update image with the rollout paused
	// and wait until the node has staged the update but has not rebooted.
	// Pausing makes the pre-reboot window deterministic: the controller still
	// propagates the target to the node spec and the daemon stages it, but a
	// paused pool exposes zero reboot slots, so the node parks at Staged and
	// cannot reboot on its own. Without pausing, a fast image pull could let
	// the controller drain and reboot the node before we scale it down, racing
	// the stall assertion below.
	updateRef := env.NodeImageUpdateDigestedPullSpec()
	testutil.StagePausedUpdate(t, g, ctx, env.Client, pool, nodeName,
		updateRef, env.NodeImageUpdateDigest(), env.NodeImageDigest())

	t.Logf("Rollout staged and parked pre-reboot; interrupting the controller")

	// Phase 4: Scale the controller to zero, interrupting the rollout. Restore
	// it on cleanup in case the test fails while scaled down.
	t.Cleanup(func() { scaleController(t, env, ctx, 1) })
	scaleController(t, env, ctx, 0)
	t.Logf("Controller scaled to zero mid-rollout")

	// Phase 5: Unpause the pool while the controller is down. The rollout is
	// now free to proceed, but nothing can drive it: the node cannot reboot
	// into the update until the controller drains it and flips
	// DesiredImageState to Booted. The booted image must stay on the original
	// digest, proving the interrupted rollout cannot complete on its own and
	// that recovery is genuinely driven by the controller coming back.
	unpaused := pool.DeepCopy()
	unpaused.Spec.Rollout.Paused = false
	g.Expect(env.Client.Patch(ctx, unpaused, client.MergeFrom(pool))).To(Succeed())
	*pool = *unpaused

	g.Consistently(func() (string, error) {
		var bn bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		if bn.Status.Booted == nil {
			return "", err
		}
		return bn.Status.Booted.ImageDigest, err
	}).WithTimeout(20*time.Second).WithPolling(2*time.Second).Should(
		Equal(env.NodeImageDigest()),
		"rollout should stall on the original image while the controller is down",
	)

	// Phase 6: Restore the controller. The recovered controller must resume and
	// complete the interrupted rollout.
	scaleController(t, env, ctx, 1)
	t.Logf("Controller restored; waiting for the interrupted rollout to finish")

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 5*time.Minute,
		HaveField("Booted", HaveField("ImageDigest", Equal(env.NodeImageUpdateDigest()))),
	)

	t.Logf("Node %q completed the interrupted rollout after controller recovery", nodeName)

	// Verify pool status after recovery.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageUpdateDigest()))
}

// scaleController scales the operator's controller-manager Deployment to the
// given replica count and waits until its available replicas match. It is used
// to simulate controller outages in recovery tests.
func scaleController(t *testing.T, env *e2eutil.Env, ctx context.Context, replicas int32) {
	t.Helper()

	g := NewWithT(t)
	deployKey := client.ObjectKey{
		Namespace: testutil.OperatorNamespaceName,
		Name:      "bootc-operator-controller-manager",
	}

	var deploy appsv1.Deployment
	g.Expect(env.Client.Get(ctx, deployKey, &deploy)).To(Succeed())

	scaled := deploy.DeepCopy()
	scaled.Spec.Replicas = &replicas
	g.Expect(env.Client.Patch(ctx, scaled, client.MergeFrom(&deploy))).To(Succeed())

	g.Eventually(func() (int32, error) {
		var d appsv1.Deployment
		err := env.Client.Get(ctx, deployKey, &d)
		return d.Status.AvailableReplicas, err
	}).WithTimeout(2*time.Minute).Should(Equal(replicas),
		"expected controller to scale to %d replica(s)", replicas)
}

func fetchPoolStatus(
	ctx context.Context,
	c client.Client,
	pool *bootcv1alpha1.BootcNodePool,
) func() (bootcv1alpha1.BootcNodePoolStatus, error) {
	return func() (bootcv1alpha1.BootcNodePoolStatus, error) {
		var p bootcv1alpha1.BootcNodePool
		err := c.Get(ctx, client.ObjectKeyFromObject(pool), &p)
		return p.Status, err
	}
}

func fetchEvents(
	ctx context.Context,
	c client.Client,
	kind, name string,
	uid k8stypes.UID,
) func() ([]eventsv1.Event, error) {
	return func() ([]eventsv1.Event, error) {
		var eventList eventsv1.EventList
		if err := c.List(ctx, &eventList); err != nil {
			return nil, err
		}

		return testutil.FilterEventsByObject(eventList.Items, kind, name, uid), nil
	}
}

// imageMatchesDigest returns a matcher that checks whether an ImageInfo
// matches the given digest using image.InfoMatchesDigest, which accepts
// both the content digest and the manifest-list digest embedded in the
// Image pullspec.
func imageMatchesDigest(digest string) types.GomegaMatcher {
	return WithTransform(func(info *bootcv1alpha1.ImageInfo) bool {
		return image.InfoMatchesDigest(info, digest)
	}, BeTrue())
}

func poolAllUpdated(nodeCount int32, deployedDigest string) types.GomegaMatcher {
	return And(
		HaveField("NodeCount", BeEquivalentTo(nodeCount)),
		HaveField("UpdatedCount", BeEquivalentTo(nodeCount)),
		HaveField("UpdatingCount", BeEquivalentTo(0)),
		HaveField("DegradedCount", BeEquivalentTo(0)),
		HaveField("DeployedDigest", Equal(deployedDigest)),
		HaveField("UpdateAvailable", BeFalse()),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.PoolUpToDate),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.PoolAllUpdated),
		))),
	)
}

// TestDaemonRecovery provisions a worker node, starts a rollout, and deletes
// the node's daemon pod while the rollout is parked pre-reboot. The DaemonSet
// must recreate the pod and the rollout must still complete after resume.
// Covers the daemon half of scenario 5 of #69 (kill the daemon during a
// roll-out).
func TestDaemonRecovery(t *testing.T) {
	e2eutil.Providers(t, "bink")
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeName := env.AddNode(t)

	ctx := context.Background()

	// Phase 1: Create pool with original image and wait for Idle.
	pool := env.NewPool("bnp-daemonrec", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())
	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute,
		HaveField("Booted", HaveField("ImageDigest", Equal(env.NodeImageDigest()))),
	)
	t.Logf("Node %q is Idle with original image", nodeName)

	// Phase 2-3: Patch to the update image with the rollout paused and wait
	// until the node has staged the update but has not rebooted, so the node
	// deterministically parks at Staged (staging done, pre-reboot) — a stable
	// window in which to kill the daemon.
	updateRef := env.NodeImageUpdateDigestedPullSpec()
	testutil.StagePausedUpdate(t, g, ctx, env.Client, pool, nodeName,
		updateRef, env.NodeImageUpdateDigest(), env.NodeImageDigest())

	// Phase 4: Delete the node's daemon pod mid-rollout.
	g.Eventually(testutil.DaemonPodsOnNode(ctx, env.Client, nodeName)).
		WithTimeout(2*time.Minute).Should(HaveLen(1),
		"expected one daemon pod on %s", nodeName)
	pods, err := testutil.DaemonPodsOnNode(ctx, env.Client, nodeName)()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pods).To(HaveLen(1))
	oldPod := pods[0]
	oldUID := oldPod.UID
	g.Expect(env.Client.Delete(ctx, &oldPod)).To(Succeed())
	t.Logf("Deleted daemon pod %q (uid %s) mid-rollout", oldPod.Name, oldUID)

	// Phase 5: The DaemonSet must recreate the daemon pod.
	g.Eventually(testutil.DaemonPodsOnNode(ctx, env.Client, nodeName)).
		WithTimeout(2*time.Minute).Should(ConsistOf(And(
		HaveField("Status.Phase", corev1.PodRunning),
		Not(HaveField("UID", oldUID)),
	)), "expected a fresh running daemon pod on %s", nodeName)
	t.Logf("Daemon pod recreated; resuming the rollout")

	// Phase 6: Unpause and verify the rollout completes despite the daemon
	// having been restarted mid-rollout.
	unpaused := pool.DeepCopy()
	unpaused.Spec.Rollout.Paused = false
	g.Expect(env.Client.Patch(ctx, unpaused, client.MergeFrom(pool))).To(Succeed())
	*pool = *unpaused

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 5*time.Minute,
		HaveField("Booted", HaveField("ImageDigest", Equal(env.NodeImageUpdateDigest()))),
	)
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageUpdateDigest()))
	t.Logf("Node %q completed the rollout after daemon recovery", nodeName)
}

// TestRebootTimeoutDegraded provisions a worker node, triggers a rollout with a
// short reboot timeout, and powers the node off once it starts rebooting so it
// never returns. The controller must report the stuck node as degraded at the
// pool level once the timeout elapses. Covers scenario 4 of #69 (a faulty node
// that fails to come back after the reboot).
func TestRebootTimeoutDegraded(t *testing.T) {
	e2eutil.Providers(t, "bink")
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeName := env.AddNode(t)

	ctx := context.Background()

	// Phase 1: Pool with the original image and a short reboot timeout.
	pool := env.NewPool("bnp-reboottmo", env.NodeImageDigestedPullSpec(),
		testutil.WithRebootTimeoutSeconds(90))
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())
	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute,
		HaveField("Booted", HaveField("ImageDigest", Equal(env.NodeImageDigest()))),
	)
	t.Logf("Node %q is Idle with original image", nodeName)

	// Phase 2: Patch to the update image to trigger a reboot.
	updateRef := env.NodeImageUpdateDigestedPullSpec()

	patched := pool.DeepCopy()
	patched.Spec.Image.Ref = updateRef
	g.Expect(env.Client.Patch(ctx, patched, client.MergeFrom(pool))).To(Succeed())
	*pool = *patched

	t.Logf("Patched pool to update image %s", updateRef)

	// Phase 3: Wait for the node to enter Rebooting. The daemon skips
	// reconciliation after issuing the reboot, so this state is durable.
	g.Eventually(func() ([]metav1.Condition, error) {
		var bn bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Status.Conditions, err
	}).WithTimeout(5*time.Minute).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.NodeIdle),
		HaveField("Status", metav1.ConditionFalse),
		HaveField("Reason", bootcv1alpha1.NodeReasonRebooting),
	)), "expected node to reach Rebooting state")

	// Phase 4: Power the node off so it never returns from the reboot. This is
	// not racy against the VM actually rebooting: the daemon stops reconciling
	// once it issues the reboot (Phase 3), so the node stays in Rebooting until
	// it either comes back Booted or the reboot timeout trips. Powering it off
	// here guarantees the former never happens, so the timeout path is taken.
	env.PowerOffNode(t, nodeName)
	t.Logf("Node %q powered off while Rebooting; awaiting reboot-timeout", nodeName)

	// Phase 5: Once the reboot timeout elapses the controller must report the
	// stuck node as degraded at the pool level (the downed node cannot
	// self-report while it is off).
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		WithTimeout(4*time.Minute).Should(And(
		HaveField("DegradedCount", BeEquivalentTo(1)),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.PoolDegraded),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.PoolNodeDegraded),
		))),
	), "expected pool to report the faulty node as degraded")
	t.Logf("Pool reported the faulty node %q as degraded", nodeName)
}

// TestOperatorRestartResilience brings a node to steady state, simulates an
// unexpected operator crash/restart by cycling both operator components, and
// verifies managed nodes are not disrupted (no extra reboot, still Idle and
// up to date) and that the operator still drives a subsequent rollout to
// completion. Covers scenario 9 of #69 (restart resilience). Complements
// TestOperatorUpgrade, which covers an actual version upgrade via a
// released manifest rather than a same-version pod restart.
func TestOperatorRestartResilience(t *testing.T) {
	e2eutil.Providers(t, "bink")
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeName := env.AddNode(t)

	ctx := context.Background()

	// Phase 1: Reach steady state on the original image.
	pool := env.NewPool("bnp-restartres", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())
	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute,
		HaveField("Booted", HaveField("ImageDigest", Equal(env.NodeImageDigest()))),
	)
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageDigest()))
	t.Logf("Node %q is Idle and pool is up to date", nodeName)

	bootsBefore := getBootCount(t, env, ctx, nodeName)

	// Phase 2: Simulate an operator upgrade by cycling both components.
	testutil.RestartOperator(t, g, ctx, env.Client, nodeName)
	t.Logf("Operator components restarted")

	// Phase 3: The managed node must not be disrupted by the operator restart:
	// it must stay on its current image without rebooting.
	g.Consistently(func() (string, error) {
		var bn bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		if bn.Status.Booted == nil {
			return "", err
		}
		return bn.Status.Booted.ImageDigest, err
	}).WithTimeout(30*time.Second).WithPolling(3*time.Second).Should(
		Equal(env.NodeImageDigest()),
		"node should stay on its image across an operator restart",
	)

	bootsAfter := getBootCount(t, env, ctx, nodeName)
	g.Expect(bootsAfter).To(Equal(bootsBefore),
		"operator restart must not reboot the node")

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 2*time.Minute,
		HaveField("Booted", HaveField("ImageDigest", Equal(env.NodeImageDigest()))),
	)
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageDigest()))

	// Phase 4: The restarted operator must still function — a subsequent
	// rollout completes normally.
	updateRef := env.NodeImageUpdateDigestedPullSpec()

	patched := pool.DeepCopy()
	patched.Spec.Image.Ref = updateRef
	g.Expect(env.Client.Patch(ctx, patched, client.MergeFrom(pool))).To(Succeed())
	*pool = *patched

	t.Logf("Patched pool to update image %s after operator restart", updateRef)

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 5*time.Minute,
		HaveField("Booted", HaveField("ImageDigest", Equal(env.NodeImageUpdateDigest()))),
	)
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageUpdateDigest()))
	t.Logf("Operator functioned normally after restart: rollout completed")
}
