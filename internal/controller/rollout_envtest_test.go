// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

// TestSimpleRollout simulates a 3-node rollout with maxUnavailable: 1. It
// verifies that only one node is cordoned at a time, that desiredImageState is
// set to Booted after drain completes, and that after each node reboots into
// the desired image and becomes Ready, its reboot slot is freed and the next
// node gets its turn.
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

	// Verify the sequential rollout: with maxUnavailable: 1 and
	// alphabetical candidate selection, nodes are processed in
	// deterministic order w1 → w2 → w3.
	for i, name := range nodeNames {
		// Wait for this node to receive its reboot slot: cordoned,
		// annotated, desiredImageState set to Booted (drain completes
		// instantly in envtest since there are no pods).
		g.Eventually(func(g Gomega) {
			var bn bootcv1alpha1.BootcNode
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &bn)).To(Succeed())
			g.Expect(bn.Annotations).To(HaveKey(bootcv1alpha1.AnnotationInRebootSlot),
				"node %s should have in-reboot-slot annotation", name)
			g.Expect(bn.Annotations).To(HaveKey(bootcv1alpha1.AnnotationWasCordoned),
				"node %s should have was-cordoned annotation", name)
			g.Expect(bn.Spec.DesiredImageState).To(Equal(bootcv1alpha1.DesiredImageStateBooted),
				"node %s should have desiredImageState Booted", name)

			var node corev1.Node
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &node)).To(Succeed())
			g.Expect(node.Spec.Unschedulable).To(BeTrue(),
				"node %s should be cordoned", name)
		}).Should(Succeed())

		// Verify remaining nodes are not yet touched.
		for _, other := range nodeNames[i+1:] {
			var node corev1.Node
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: other}, &node)).To(Succeed())
			g.Expect(node.Spec.Unschedulable).To(BeFalse(),
				"node %s should not be cordoned", other)

			var bn bootcv1alpha1.BootcNode
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: other}, &bn)).To(Succeed())
			g.Expect(bn.Spec.DesiredImageState).To(Equal(bootcv1alpha1.DesiredImageStateStaged),
				"node %s should still have desiredImageState Staged", other)
		}

		// Simulate the daemon reporting a successful reboot and the
		// node becoming Ready.
		simulateDaemonStatus(g, ctx, name, testDigestB, bootcv1alpha1.NodeReasonIdle)
		setNodeReady(g, ctx, name)

		// Verify the reboot slot is freed: annotations removed and
		// node uncordoned.
		g.Eventually(func(g Gomega) {
			var bn bootcv1alpha1.BootcNode
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &bn)).To(Succeed())
			g.Expect(bn.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationInRebootSlot),
				"node %s should have in-reboot-slot annotation removed", name)
			g.Expect(bn.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationWasCordoned),
				"node %s should have was-cordoned annotation removed", name)

			var node corev1.Node
			g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name}, &node)).To(Succeed())
			g.Expect(node.Spec.Unschedulable).To(BeFalse(),
				"node %s should be uncordoned after reboot", name)
		}).Should(Succeed())
	}
}

// TestDegradedNodeSetsPoolCondition verifies that when a daemon reports
// Degraded=True on a BootcNode, the pool is marked Degraded/NodeDegraded,
// and the rollout continues on non-degraded nodes.
func TestDegradedNodeSetsPoolCondition(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	const poolName = "degraded-pool"

	// Create 2 worker nodes.
	nodeNames := []string{"degraded-w1", "degraded-w2"}
	for _, name := range nodeNames {
		name := name
		node := testutil.NewK8sNode(name, testutil.WorkerLabels())
		g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
		t.Cleanup(func() {
			_ = k8sClient.Delete(ctx, node)
		})
	}

	// Create pool targeting digest B with maxUnavailable: 1.
	pool := testutil.NewPool(poolName, testImageDigestRefB,
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

	// Simulate w1 as degraded (staging failed) and w2 as staged.
	simulateDaemonDegraded(g, ctx, "degraded-w1", testDigestA)
	simulateDaemonStatus(g, ctx, "degraded-w2", testDigestA, bootcv1alpha1.NodeReasonStaged)

	// Verify pool is Degraded/NodeDegraded mentioning w1.
	g.Eventually(func() ([]metav1.Condition, error) {
		var p bootcv1alpha1.BootcNodePool
		err := k8sClient.Get(ctx, client.ObjectKey{Name: poolName}, &p)
		return p.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolDegraded),
		HaveField("Status", metav1.ConditionTrue),
		HaveField("Reason", bootcv1alpha1.PoolNodeDegraded),
		HaveField("Message", ContainSubstring("degraded-w1")),
	)))

	// Verify rollout continues on the non-degraded node: w2 should get
	// a reboot slot despite w1 being degraded.
	g.Eventually(func() (map[string]string, error) {
		var bn bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKey{Name: "degraded-w2"}, &bn)
		return bn.Annotations, err
	}).Should(HaveKey(bootcv1alpha1.AnnotationInRebootSlot), "non-degraded node should get a reboot slot")
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

// simulateDaemonDegraded writes BootcNode status as if the daemon had
// reported the given booted digest with Degraded=True (e.g. staging failed).
func simulateDaemonDegraded(g Gomega, ctx context.Context, nodeName, bootedDigest string) {
	var bn bootcv1alpha1.BootcNode
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)).To(Succeed())

	bn.Status.Booted = &bootcv1alpha1.ImageInfo{
		Image:       "quay.io/example/myos@" + bootedDigest,
		ImageDigest: bootedDigest,
	}
	apimeta.SetStatusCondition(&bn.Status.Conditions, metav1.Condition{
		Type:   bootcv1alpha1.NodeIdle,
		Status: metav1.ConditionFalse,
		Reason: bootcv1alpha1.NodeReasonStaging,
	})
	apimeta.SetStatusCondition(&bn.Status.Conditions, metav1.Condition{
		Type:    bootcv1alpha1.NodeDegraded,
		Status:  metav1.ConditionTrue,
		Reason:  bootcv1alpha1.NodeReasonError,
		Message: "simulated staging failure",
	})

	g.Expect(k8sClient.Status().Update(ctx, &bn)).To(Succeed())
}

// setNodeReady sets the Ready condition on a K8s Node to True. In
// envtest there is no kubelet, so this simulates the node becoming
// healthy after a reboot.
func setNodeReady(g Gomega, ctx context.Context, nodeName string) {
	var node corev1.Node
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: nodeName}, &node)).To(Succeed())

	// Replace any existing Ready condition, preserving other conditions.
	var filtered []corev1.NodeCondition
	for _, c := range node.Status.Conditions {
		if c.Type != corev1.NodeReady {
			filtered = append(filtered, c)
		}
	}
	node.Status.Conditions = append(filtered, corev1.NodeCondition{
		Type:               corev1.NodeReady,
		Status:             corev1.ConditionTrue,
		LastHeartbeatTime:  metav1.Now(),
		LastTransitionTime: metav1.Now(),
	})

	g.Expect(k8sClient.Status().Update(ctx, &node)).To(Succeed())
}
