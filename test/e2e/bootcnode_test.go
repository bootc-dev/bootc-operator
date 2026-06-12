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

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
	"github.com/jlebon/bootc-operator/test/e2e/e2eutil"
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

	// Phase 3: Wait for Idle with the update digest — proves the full
	// update lifecycle completed (staging, reboot, boot into new image).
	// We don't assert on intermediate states (Staging, Rebooting) because
	// they are too transient to catch reliably with polling.
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
}
