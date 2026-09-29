// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

// StagePausedUpdate patches pool to targetRef with the rollout paused, then
// waits until nodeName has staged targetDigest while still booted on
// originalDigest. Pausing exposes zero reboot slots, so the controller still
// propagates the target and the daemon stages it, but the node parks at
// Staged and cannot reboot on its own — a deterministic pre-reboot window for
// interrupting the controller or daemon mid-rollout.
func StagePausedUpdate(
	t *testing.T,
	g gomega.Gomega,
	ctx context.Context,
	c client.Client,
	pool *bootcv1alpha1.BootcNodePool,
	nodeName, targetRef, targetDigest, originalDigest string,
) {
	t.Helper()

	patched := pool.DeepCopy()
	patched.Spec.Image.Ref = targetRef
	if patched.Spec.Rollout == nil {
		patched.Spec.Rollout = &bootcv1alpha1.RolloutSpec{}
	}
	patched.Spec.Rollout.Paused = true
	g.Expect(c.Patch(ctx, patched, client.MergeFrom(pool))).To(gomega.Succeed())
	*pool = *patched

	t.Logf("Patched pool to update image %s (paused)", targetRef)

	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		var bn bootcv1alpha1.BootcNode
		err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Status, err
	}).WithTimeout(5*time.Minute).Should(gomega.And(
		gomega.HaveField("Staged", gomega.And(
			gomega.Not(gomega.BeNil()),
			gomega.HaveField("ImageDigest", gomega.Equal(targetDigest)),
		)),
		gomega.HaveField("Booted", gomega.And(
			gomega.Not(gomega.BeNil()),
			gomega.HaveField("ImageDigest", gomega.Equal(originalDigest)),
		)),
		gomega.HaveField("Conditions", gomega.ContainElement(gomega.And(
			gomega.HaveField("Type", bootcv1alpha1.NodeIdle),
			gomega.HaveField("Status", metav1.ConditionFalse),
			gomega.HaveField("Reason", bootcv1alpha1.NodeReasonStaged),
		))),
	), "expected node to stage the update but stay pre-reboot")
}

// DaemonPodsOnNode returns a poll function listing the operator daemon pods
// scheduled on the given node.
func DaemonPodsOnNode(
	ctx context.Context,
	c client.Client,
	nodeName string,
) func() ([]corev1.Pod, error) {
	return func() ([]corev1.Pod, error) {
		var pods corev1.PodList
		if err := c.List(ctx, &pods,
			client.InNamespace(OperatorNamespaceName),
			client.MatchingLabels{
				"app.kubernetes.io/name":      "bootc-operator",
				"app.kubernetes.io/component": "daemon",
			},
		); err != nil {
			return nil, err
		}
		var onNode []corev1.Pod
		for _, p := range pods.Items {
			if p.Spec.NodeName == nodeName {
				onNode = append(onNode, p)
			}
		}
		return onNode, nil
	}
}

// RestartOperator deletes all operator pods (both the controller Deployment
// and the DaemonSet), simulating a crash or upgrade. Kubernetes recreates
// them as it would after an image bump. It waits for a fresh controller pod
// and a fresh daemon pod on the given node to be Running before returning.
func RestartOperator(
	t *testing.T,
	g gomega.Gomega,
	ctx context.Context,
	c client.Client,
	nodeName string,
) {
	t.Helper()

	g.Expect(c.DeleteAllOf(ctx, &corev1.Pod{},
		client.InNamespace(OperatorNamespaceName),
		client.MatchingLabels{"app.kubernetes.io/name": "bootc-operator"},
	)).To(gomega.Succeed())

	g.Eventually(func() ([]corev1.Pod, error) {
		var pods corev1.PodList
		err := c.List(ctx, &pods,
			client.InNamespace(OperatorNamespaceName),
			client.MatchingLabels{
				"app.kubernetes.io/name":      "bootc-operator",
				"app.kubernetes.io/component": "controller",
			},
		)
		return pods.Items, err
	}).WithTimeout(2*time.Minute).Should(gomega.ContainElement(
		gomega.HaveField("Status.Phase", corev1.PodRunning),
	), "expected a running controller pod after restart")

	g.Eventually(DaemonPodsOnNode(ctx, c, nodeName)).
		WithTimeout(2*time.Minute).Should(gomega.ConsistOf(
		gomega.HaveField("Status.Phase", corev1.PodRunning),
	), "expected a running daemon pod on %s after restart", nodeName)
}
