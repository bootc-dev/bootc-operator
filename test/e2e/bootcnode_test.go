// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
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

	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		var bn bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Status, err
	}).WithTimeout(3 * time.Minute).Should(And(
		HaveField("Booted", And(
			Not(BeNil()),
			HaveField("Image", env.NodeImageDigestedPullSpec()),
			HaveField("ImageDigest", env.NodeImageDigest()),
		)),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
		))),
	))

	// Verify pool status reflects steady state.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageDigest()))
}

// TestUpdateReboot provisions a worker node, creates a pool with the
// original image, then updates the pool to a new image and verifies the
// full update lifecycle: staging, reboot, and idle with the new image.
func TestUpdateReboot(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeName := env.AddNode(t)

	ctx := context.Background()

	// Phase 1: Create pool with original image and wait for Idle.
	pool := env.NewPool("workers", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		var bn bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Status, err
	}).WithTimeout(3 * time.Minute).Should(And(
		HaveField("Booted", Not(BeNil())),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
		))),
	))

	t.Logf("Node %q is Idle with original image", nodeName)

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
	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		var bn bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Status, err
	}).WithTimeout(5*time.Minute).Should(And(
		HaveField("Booted", And(
			Not(BeNil()),
			HaveField("ImageDigest", env.NodeImageUpdateDigest()),
		)),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
		))),
	), "expected node to reach Idle with update image after reboot")

	t.Logf("Node %q is Idle with update image", nodeName)

	// Verify pool status after rollout completes.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageUpdateDigest()))

	// Phase 5: Verify node is schedulable (uncordoned after reboot).
	g.Eventually(func() (bool, error) {
		var node corev1.Node
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &node)
		return node.Spec.Unschedulable, err
	}).WithTimeout(3*time.Minute).Should(BeFalse(), "expected node to be schedulable after update")

	// Phase 6: Verify update marker exists on the host via daemon pod exec.
	execOnNode(t, g, env, ctx, nodeName,
		"stat", "/proc/1/root/usr/share/update-marker")

	t.Logf("Verified update-marker exists on host via daemon pod")

	// Phase 7: Rollback to original image.
	originalRef := env.NodeImageDigestedPullSpec()

	modified = pool.DeepCopy()
	modified.Spec.Image.Ref = originalRef
	g.Expect(env.Client.Patch(ctx, modified, client.MergeFrom(pool))).To(Succeed())
	*pool = *modified

	t.Logf("Patched pool to rollback to original image %s", originalRef)

	// Phase 8: Wait for Idle with the original digest — proves rollback succeeded.
	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		var bn2 bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn2)
		return bn2.Status, err
	}).WithTimeout(5*time.Minute).Should(And(
		HaveField("Booted", And(
			Not(BeNil()),
			HaveField("ImageDigest", Equal(env.NodeImageDigest())),
		)),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
		))),
	), "expected node to reach Idle with original image after rollback")

	t.Logf("Node %q successfully rolled back to original image", nodeName)

	// Verify pool status after rollback completes.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageDigest()))
}

// TestTagResolution creates a pool with a tag-based image ref, verifies
// the controller resolves the tag to a digest, then retags the image
// and verifies re-resolution triggers a rollout.
func TestTagResolution(t *testing.T) {
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
	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		var bn bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Status, err
	}).WithTimeout(3 * time.Minute).Should(And(
		HaveField("Booted", And(
			Not(BeNil()),
			HaveField("ImageDigest", Equal(env.NodeImageDigest())),
		)),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionTrue),
		))),
	))

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

	// Wait for node to reach Idle with the update image.
	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		var bn bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Status, err
	}).WithTimeout(5 * time.Minute).Should(And(
		HaveField("Booted", And(
			Not(BeNil()),
			HaveField("ImageDigest", Equal(env.NodeImageUpdateDigest())),
		)),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionTrue),
		))),
	))

	t.Logf("Node %q is Idle with update image", nodeName)
}

// TestMidRolloutImageChange provisions two worker nodes, starts a rollout
// to one update image, then switches the target to a different update image
// while one node is rebooting. It verifies both nodes converge to the final
// image and that the non-rebooting node does not wastefully reboot into the
// first update image.
func TestMidRolloutImageChange(t *testing.T) {
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
		g.Eventually(func() (bootcv1alpha1.BootcNode, error) {
			var bn bootcv1alpha1.BootcNode
			err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
			return bn, err
		}).WithTimeout(3 * time.Minute).Should(SatisfyAll(
			HaveField("Status.Booted", Not(BeNil())),
			HaveField("Status.Conditions", ContainElement(And(
				HaveField("Type", bootcv1alpha1.NodeIdle),
				HaveField("Status", metav1.ConditionTrue),
				HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
			))),
		))
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
		g.Eventually(func() (bootcv1alpha1.BootcNode, error) {
			var bn bootcv1alpha1.BootcNode
			err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
			return bn, err
		}).WithTimeout(8*time.Minute).Should(SatisfyAll(
			HaveField("Status.Booted", Not(BeNil())),
			HaveField("Status.Booted.ImageDigest", Equal(env.NodeImageUpdate2Digest())),
			HaveField("Status.Conditions", ContainElement(And(
				HaveField("Type", bootcv1alpha1.NodeIdle),
				HaveField("Status", metav1.ConditionTrue),
				HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
			))),
		), "expected node %s to reach Idle with second update image", nodeName)
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
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeName := env.AddNode(t)

	ctx := context.Background()

	// Phase 1: Create pool with original image and wait for Idle.
	pool := env.NewPool("bnp-pause", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	var bn bootcv1alpha1.BootcNode
	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Status, err
	}).WithTimeout(3 * time.Minute).Should(And(
		HaveField("Booted", Not(BeNil())),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
		))),
	))

	t.Logf("Node %q is Idle with original image", nodeName)

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
	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Status, err
	}).WithTimeout(5*time.Minute).Should(And(
		HaveField("Booted", And(
			Not(BeNil()),
			HaveField("ImageDigest", Equal(env.NodeImageUpdateDigest())),
		)),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
		))),
	), "expected node to reach Idle with update image after resume")

	t.Logf("Node %q completed update after resume", nodeName)

	// Verify pool status after resume completes.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageUpdateDigest()))
}

// TestNonExistingImage provisions a worker node, creates a pool with the
// original image, then updates to a non-existing image and verifies the
// node enters degraded state and the update does not proceed.
func TestNonExistingImage(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeName := env.AddNode(t)

	ctx := context.Background()

	// Phase 1: Create pool with original image and wait for Idle.
	pool := env.NewPool("bnp-noimg", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	var bn bootcv1alpha1.BootcNode
	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Status, err
	}).WithTimeout(3 * time.Minute).Should(And(
		HaveField("Booted", Not(BeNil())),
		HaveField("Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
		))),
	))

	t.Logf("Node %q is Idle with original image", nodeName)

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
