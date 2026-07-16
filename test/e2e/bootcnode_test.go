// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/test/e2e/e2eutil"
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

	// Wait for BootcNode to appear for the worker.
	var bn bootcv1alpha1.BootcNode
	g.Eventually(func() error {
		return env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
	}).Should(Succeed())

	// Verify ownerReference.
	owner := metav1.GetControllerOf(&bn)
	g.Expect(owner).NotTo(BeNil())
	g.Expect(owner.Name).To(Equal(pool.Name))

	// Verify desiredImage.
	g.Expect(bn.Spec.DesiredImage).To(Equal(env.NodeImageDigestedPullSpec()))

	// Verify the worker has the managed label.
	var node corev1.Node
	g.Eventually(func() (map[string]string, error) {
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &node)
		return node.Labels, err
	}).Should(HaveKey(bootcv1alpha1.LabelManaged))

	g.Eventually(func() ([]corev1.Pod, error) {
		var pods corev1.PodList
		err := env.Client.List(ctx, &pods,
			client.InNamespace("bootc-operator"),
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

	g.Eventually(func(g Gomega) {
		g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)).To(Succeed())
		g.Expect(bn.Status.Booted).NotTo(BeNil(), "expected booted status to be populated")
		g.Expect(bn.Status.Booted.Image).To(Equal(env.NodeImageDigestedPullSpec()),
			"booted image should match seeded registry image")
		g.Expect(bn.Status.Booted.ImageDigest).To(Equal(env.NodeImageDigest()),
			"booted image digest should match seeded registry image")
		g.Expect(bn.Status.Conditions).To(ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
		)))
	}).WithTimeout(3 * time.Minute).Should(Succeed())
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

	var bn bootcv1alpha1.BootcNode
	g.Eventually(func(g Gomega) {
		g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)).To(Succeed())
		g.Expect(bn.Status.Booted).NotTo(BeNil())
		g.Expect(bn.Status.Conditions).To(ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
		)))
	}).WithTimeout(3 * time.Minute).Should(Succeed())

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
	g.Eventually(func(g Gomega) {
		g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)).To(Succeed())
		g.Expect(bn.Status.Conditions).To(ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionFalse),
			HaveField("Reason", bootcv1alpha1.NodeReasonRebooting),
		)))
	}).WithTimeout(5*time.Minute).Should(Succeed(), "expected node to reach Rebooting state")

	t.Logf("Node %q is Rebooting", nodeName)

	// Phase 4: Wait for Idle with the update digest — proves the full
	// update lifecycle completed (staging, reboot, boot into new image).
	g.Eventually(func(g Gomega) {
		g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)).To(Succeed())
		g.Expect(bn.Status.Booted).NotTo(BeNil())
		g.Expect(bn.Status.Booted.ImageDigest).To(Equal(env.NodeImageUpdateDigest()),
			"expected booted digest to match update image")
		g.Expect(bn.Status.Conditions).To(ContainElement(And(
			HaveField("Type", bootcv1alpha1.NodeIdle),
			HaveField("Status", metav1.ConditionTrue),
			HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
		)))
	}).WithTimeout(5*time.Minute).Should(Succeed(), "expected node to reach Idle with update image after reboot")

	t.Logf("Node %q is Idle with update image", nodeName)

	// Phase 5: Verify node is schedulable (uncordoned after reboot).
	var node corev1.Node
	g.Eventually(func(g Gomega) bool {
		g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &node)).To(Succeed())
		return node.Spec.Unschedulable
	}).WithTimeout(3*time.Minute).Should(BeFalse(), "expected node to be schedulable after update")

	// Phase 6: Verify update marker exists on the host via daemon pod exec.
	var daemonPod corev1.Pod
	g.Eventually(func(g Gomega) {
		var pods corev1.PodList
		g.Expect(env.Client.List(ctx, &pods,
			client.InNamespace("bootc-operator"),
			client.MatchingLabels{
				"app.kubernetes.io/name":      "bootc-operator",
				"app.kubernetes.io/component": "daemon",
			},
		)).To(Succeed())
		var matched []corev1.Pod
		for _, p := range pods.Items {
			if p.Spec.NodeName == nodeName {
				matched = append(matched, p)
			}
		}
		g.Expect(matched).To(HaveLen(1), "expected exactly one daemon pod on %s", nodeName)
		g.Expect(matched[0].Status.Phase).To(Equal(corev1.PodRunning))
		daemonPod = matched[0]
	}).WithTimeout(1*time.Minute).Should(Succeed(), "expected running daemon pod on %s", nodeName)

	kubeconfigPath := os.Getenv("KUBECONFIG")
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"-n", "bootc-operator", "exec", daemonPod.Name, "--",
		"stat", "/proc/1/root/usr/share/update-marker")
	out, err := cmd.CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(),
		fmt.Sprintf("expected update-marker to exist on host, kubectl exec output: %s", string(out)))

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

	// Phase 4: Verify the node did not stage the non-existing image.
	g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)).To(Succeed())
	g.Expect(bn.Status.Staged).To(BeNil(),
		"node should not have staged the non-existing image")

	t.Logf("Verified node %q did not stage non-existing image", nodeName)
}
