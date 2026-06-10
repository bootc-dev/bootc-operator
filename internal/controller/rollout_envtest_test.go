// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
	testutil "github.com/jlebon/bootc-operator/test/util"
)

// TestSimpleRollout simulates a 3-node rollout with maxUnavailable: 1. It
// verifies that only one node is cordoned at a time and that desiredImageState
// is set to Booted after drain completes.
func TestSimpleRollout(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	const (
		poolName = "rollout-3node"
		// All nodes are booted on digest A; pool targets digest B.
		oldImage    = testImageDigestRefA
		newImage    = testImageDigestRefB
		newImageRef = testImageDigestRefB
	)

	// Create 3 worker nodes.
	nodeNames := []string{"rollout-w1", "rollout-w2", "rollout-w3"}
	for _, name := range nodeNames {
		name := name
		node := testutil.NewK8sNode(name, testutil.WorkerLabels())
		g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
		t.Cleanup(func() {
			_ = k8sClient.Delete(ctx, node)
		})
	}

	// Create pool targeting digest A with maxUnavailable: 1.
	pool := testutil.NewPool(poolName, newImageRef,
		testutil.WithWorkerSelector(),
		testutil.WithMaxUnavailable(intstr.FromInt32(1)),
	)
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
	})

	// Wait for BootcNodes to be created.
	for _, name := range nodeNames {
		name := name
		g.Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKey{Name: name}, &bootcv1alpha1.BootcNode{})
		}).Should(Succeed())
	}

	// Simulate daemon: set all nodes as booting the old image and Staged
	// for the new one. This is the state where nodes have staged the
	// target image and are waiting for a reboot slot.
	for _, name := range nodeNames {
		simulateDaemonStatus(g, ctx, name, testDigestA, bootcv1alpha1.NodeReasonStaged)
	}

	// Wait for exactly one node to be cordoned (reboot slot assigned).
	var cordonedNode string
	g.Eventually(func() string {
		cordonedNode = ""
		for _, name := range nodeNames {
			var node corev1.Node
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &node)).To(Succeed())
			if node.Spec.Unschedulable {
				if cordonedNode != "" {
					// More than one node cordoned — fail.
					return "MULTIPLE"
				}
				cordonedNode = name
			}
		}
		return cordonedNode
	}).ShouldNot(BeEmpty())
	g.Expect(cordonedNode).NotTo(Equal("MULTIPLE"), "only one node should be cordoned with maxUnavailable: 1")

	// Verify the cordoned node has the in-reboot-slot annotation.
	var bn bootcv1alpha1.BootcNode
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: cordonedNode}, &bn)).To(Succeed())
	g.Expect(bn.Annotations).To(HaveKey(bootcv1alpha1.AnnotationInRebootSlot))
	g.Expect(bn.Annotations).To(HaveKey(bootcv1alpha1.AnnotationWasCordoned))

	// In envtest, drain completes instantly (no pods). Verify that
	// desiredImageState is set to Booted on the cordoned node.
	g.Eventually(func(g Gomega) {
		var bn bootcv1alpha1.BootcNode
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: cordonedNode}, &bn)).To(Succeed())
		g.Expect(bn.Spec.DesiredImageState).To(Equal(bootcv1alpha1.DesiredImageStateBooted))
	}).Should(Succeed())

	// Verify the other nodes are NOT cordoned and still have
	// desiredImageState: Staged.
	for _, name := range nodeNames {
		if name == cordonedNode {
			continue
		}
		var node corev1.Node
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &node)).To(Succeed())
		g.Expect(node.Spec.Unschedulable).To(BeFalse(), "node %s should not be cordoned", name)

		var bn bootcv1alpha1.BootcNode
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &bn)).To(Succeed())
		g.Expect(bn.Spec.DesiredImageState).To(Equal(bootcv1alpha1.DesiredImageStateStaged),
			"node %s should still have desiredImageState Staged", name)
	}
}

// simulateDaemonStatus writes BootcNode status as if the daemon had
// reported the given booted digest and Idle condition reason.
func simulateDaemonStatus(g Gomega, ctx context.Context, nodeName, bootedDigest, idleReason string) {
	var bn bootcv1alpha1.BootcNode
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)).To(Succeed())

	bn.Status.Booted = &bootcv1alpha1.ImageInfo{
		Image:       "quay.io/example/myos@" + bootedDigest,
		ImageDigest: bootedDigest,
	}

	idleStatus := metav1.ConditionFalse
	if idleReason == bootcv1alpha1.NodeReasonIdle {
		idleStatus = metav1.ConditionTrue
	}
	bn.Status.Conditions = []metav1.Condition{
		{
			Type:               bootcv1alpha1.NodeIdle,
			Status:             idleStatus,
			Reason:             idleReason,
			LastTransitionTime: metav1.Now(),
		},
	}

	g.Expect(k8sClient.Status().Update(ctx, &bn)).To(Succeed())
}
