// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

// pollInterval and pollTimeout control how long tests wait for the
// async reconciler to act.
const (
	pollInterval = 200 * time.Millisecond
	pollTimeout  = 10 * time.Second
)

// TestInvalidImageRefDegradedCondition verifies that a pool with an
// invalid image ref is marked Degraded/InvalidSpec and recovers when
// the ref is corrected.
func TestInvalidImageRefDegradedCondition(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	// Create a pool with an invalid image ref (short name, rejected by
	// parseImageRef which requires fully qualified names).
	pool := testutil.NewPool("invalid-ref-pool", "myos@sha256:bad", testutil.WithWorkerSelector())
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
	})

	// Wait for pool to be marked Degraded/InvalidSpec.
	g.Eventually(func() ([]metav1.Condition, error) {
		var p bootcv1alpha1.BootcNodePool
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &p)
		return p.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolDegraded),
		HaveField("Status", metav1.ConditionTrue),
		HaveField("Reason", bootcv1alpha1.PoolInvalidSpec),
	)))

	// Fix the image ref to a valid digest ref.
	var freshPool bootcv1alpha1.BootcNodePool
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: pool.Name}, &freshPool)).To(Succeed())
	freshPool.Spec.Image.Ref = testImageDigestRefA
	g.Expect(k8sClient.Update(ctx, &freshPool)).To(Succeed())

	// Wait for pool to recover: Degraded=False/Healthy.
	g.Eventually(func() ([]metav1.Condition, error) {
		var p bootcv1alpha1.BootcNodePool
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &p)
		return p.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolDegraded),
		HaveField("Status", metav1.ConditionFalse),
		HaveField("Reason", bootcv1alpha1.PoolHealthy),
	)))
}

// TestMembershipCreatesBootcNodes verifies that creating a pool and
// matching nodes causes BootcNodes to be created with the correct
// ownerReference, desiredImage, and that nodes are labeled
// bootc.dev/managed. It also verifies that removing a node's matching
// label causes cleanup (BootcNode deleted, managed label removed).
func TestMembershipCreatesBootcNodes(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	// Create two worker nodes.
	node1 := testutil.NewK8sNode("mem-worker-1", testutil.WorkerLabels())
	node2 := testutil.NewK8sNode("mem-worker-2", testutil.WorkerLabels())
	for _, n := range []*corev1.Node{node1, node2} {
		g.Expect(k8sClient.Create(ctx, n)).To(Succeed())
		t.Cleanup(func() {
			_ = k8sClient.Delete(ctx, n)
		})
	}

	// Create a pool selecting workers.
	pool := testutil.NewPool("mem-workers", testImageDigestRefA, testutil.WithWorkerSelector())
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
	})

	// Wait for BootcNodes to appear and verify their properties.
	for _, nodeName := range []string{"mem-worker-1", "mem-worker-2"} {
		name := nodeName

		// Check ownerReference.
		g.Eventually(func() (*metav1.OwnerReference, error) {
			var bn bootcv1alpha1.BootcNode
			err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &bn)
			return metav1.GetControllerOf(&bn), err
		}).Should(And(Not(BeNil()), HaveField("Name", pool.Name)),
			"BootcNode %s owner", name)

		// Check desiredImage and desiredImageState.
		g.Eventually(func() (bootcv1alpha1.BootcNodeSpec, error) {
			var bn bootcv1alpha1.BootcNode
			err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &bn)
			return bn.Spec, err
		}).Should(And(
			HaveField("DesiredImage", testImageDigestRefA),
			HaveField("DesiredImageState", bootcv1alpha1.DesiredImageStateStaged),
		), "BootcNode %s spec", name)
	}

	// Verify nodes are labeled bootc.dev/managed.
	for _, nodeName := range []string{"mem-worker-1", "mem-worker-2"} {
		name := nodeName
		g.Eventually(func() (map[string]string, error) {
			var n corev1.Node
			err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &n)
			return n.Labels, err
		}).Should(HaveKey(bootcv1alpha1.LabelManaged))
	}

	// Remove the worker label from mem-worker-1 and verify cleanup.
	var fresh corev1.Node
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "mem-worker-1"}, &fresh)).To(Succeed())
	patch := client.StrategicMergeFrom(fresh.DeepCopy())
	delete(fresh.Labels, "node-role.kubernetes.io/worker")
	g.Expect(k8sClient.Patch(ctx, &fresh, patch)).To(Succeed())

	// Wait for BootcNode to be deleted.
	g.Eventually(func() error {
		return k8sClient.Get(
			ctx,
			client.ObjectKey{Name: "mem-worker-1"},
			&bootcv1alpha1.BootcNode{},
		)
	}).Should(MatchError(apierrors.IsNotFound, "IsNotFound"))

	// Verify managed label is removed.
	g.Eventually(func() (map[string]string, error) {
		var n corev1.Node
		err := k8sClient.Get(ctx, client.ObjectKey{Name: "mem-worker-1"}, &n)
		return n.Labels, err
	}).ShouldNot(HaveKey(bootcv1alpha1.LabelManaged))

	// Delete mem-worker-2 and verify its BootcNode is also deleted.
	g.Expect(k8sClient.Delete(ctx, node2)).To(Succeed())

	g.Eventually(func() error {
		return k8sClient.Get(
			ctx,
			client.ObjectKey{Name: "mem-worker-2"},
			&bootcv1alpha1.BootcNode{},
		)
	}).Should(MatchError(apierrors.IsNotFound, "IsNotFound"))
}

// TestMembershipSyncsDesiredImage verifies that changing the pool's
// image ref updates desiredImage on all owned BootcNodes and resets
// desiredImageState to Staged.
func TestMembershipSyncsDesiredImage(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	node := testutil.NewK8sNode("mem-sync-1", testutil.WorkerLabels())
	g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, node)
	})

	pool := testutil.NewPool("mem-sync-pool", testImageDigestRefA, testutil.WithWorkerSelector())
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool)
	})

	// Wait for BootcNode to be created and verify image A.
	g.Eventually(func() (string, error) {
		var bn bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(node), &bn)
		return bn.Spec.DesiredImage, err
	}).Should(Equal(testImageDigestRefA))

	// Update pool image to B.
	var freshPool bootcv1alpha1.BootcNodePool
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &freshPool)).To(Succeed())
	freshPool.Spec.Image.Ref = testImageDigestRefB
	g.Expect(k8sClient.Update(ctx, &freshPool)).To(Succeed())

	// Wait for BootcNode to be updated with image B.
	g.Eventually(func() (bootcv1alpha1.BootcNodeSpec, error) {
		var bn bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(node), &bn)
		return bn.Spec, err
	}).Should(And(
		HaveField("DesiredImage", testImageDigestRefB),
		HaveField("DesiredImageState", bootcv1alpha1.DesiredImageStateStaged),
	))
}

// TestPoolDeletionRemovesManagedLabel verifies that when a BootcNodePool is
// deleted, the controller removes the bootc.dev/managed label from all member
// nodes and deletes all owned BootcNode objects. It also verifies that
// control-plane nodes belonging to a separate pool are unaffected.
func TestPoolDeletionRemovesManagedLabel(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	workerNodes := []*corev1.Node{
		testutil.NewK8sNode("del-worker-1", testutil.WorkerLabels()),
		testutil.NewK8sNode("del-worker-2", testutil.WorkerLabels()),
	}

	controlPlaneNodes := []*corev1.Node{
		testutil.NewK8sNode("del-control-plane-1", testutil.ControlPlaneLabels()),
		testutil.NewK8sNode("del-control-plane-2", testutil.ControlPlaneLabels()),
	}

	allNodes := append(workerNodes, controlPlaneNodes...)

	// Create nodes.
	for _, node := range allNodes {
		g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
		t.Cleanup(func() {
			_ = k8sClient.Delete(ctx, node)
		})
	}

	// Create a worker pool and a separate control-plane pool.
	workerPool := testutil.NewPool(
		"del-workers",
		testImageDigestRefA,
		testutil.WithWorkerSelector(),
	)
	g.Expect(k8sClient.Create(ctx, workerPool)).To(Succeed())

	cpPool := testutil.NewPool("del-control-plane", testImageDigestRefA,
		testutil.WithNodeSelector(testutil.ControlPlaneLabels()))
	g.Expect(k8sClient.Create(ctx, cpPool)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, cpPool)
	})

	// Wait for BootcNodes to appear and managed labels to be applied on all nodes.
	for _, node := range allNodes {
		g.Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKey{Name: node.Name}, &bootcv1alpha1.BootcNode{})
		}).Should(Succeed(), "BootcNode %s should exist", node.Name)

		g.Eventually(func() (map[string]string, error) {
			var n corev1.Node
			err := k8sClient.Get(ctx, client.ObjectKey{Name: node.Name}, &n)
			return n.Labels, err
		}).Should(HaveKey(bootcv1alpha1.LabelManaged), "node %s should have managed label", node.Name)
	}

	// Delete only the worker pool.
	g.Expect(k8sClient.Delete(ctx, workerPool)).To(Succeed())

	// The worker pool's finalizer must remove the managed label from worker nodes.
	for _, node := range workerNodes {
		g.Eventually(func() (map[string]string, error) {
			var n corev1.Node
			err := k8sClient.Get(ctx, client.ObjectKey{Name: node.Name}, &n)
			return n.Labels, err
		}).ShouldNot(HaveKey(bootcv1alpha1.LabelManaged), "worker node %s should not have managed label after pool deletion", node.Name)
	}

	// Worker BootcNodes should be deleted.
	for _, node := range workerNodes {
		g.Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKey{Name: node.Name}, &bootcv1alpha1.BootcNode{})
		}).Should(MatchError(apierrors.IsNotFound, "IsNotFound"), "BootcNode %s should be deleted", node.Name)
	}

	// The worker pool itself should be fully deleted (finalizer removed).
	g.Eventually(func() error {
		return k8sClient.Get(
			ctx,
			client.ObjectKeyFromObject(workerPool),
			&bootcv1alpha1.BootcNodePool{},
		)
	}).Should(MatchError(apierrors.IsNotFound, "IsNotFound"), "worker pool should be fully deleted")

	// Control-plane nodes must still carry the managed label — their pool was not deleted.
	for _, node := range controlPlaneNodes {
		var n corev1.Node
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: node.Name}, &n)).To(Succeed())
		g.Expect(n.Labels).To(HaveKey(bootcv1alpha1.LabelManaged),
			"control-plane node %s should still have managed label", node.Name)
	}

	// Control-plane BootcNodes must still exist.
	for _, node := range controlPlaneNodes {
		g.Expect(k8sClient.Get(ctx, client.ObjectKey{Name: node.Name}, &bootcv1alpha1.BootcNode{})).
			To(Succeed(),
				"BootcNode %s should still exist", node.Name)
	}
}

// TestMembershipConflictDetection verifies that when a node matches two
// pools, the conflicting pool is marked Degraded with reason
// NodeConflict for the contested node, but non-contested nodes in
// that pool are still handled.
func TestMembershipConflictDetection(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	// node1: pool1 only, node2: pool2 only, node3: both (contested).
	node1 := testutil.NewK8sNode("mem-conflict-1", map[string]string{"pool1": "true"})
	node2 := testutil.NewK8sNode("mem-conflict-2", map[string]string{"pool2": "true"})
	node3 := testutil.NewK8sNode(
		"mem-conflict-3",
		map[string]string{"pool1": "true", "pool2": "true"},
	)
	for _, n := range []*corev1.Node{node1, node2, node3} {
		g.Expect(k8sClient.Create(ctx, n)).To(Succeed())
		t.Cleanup(func() {
			_ = k8sClient.Delete(ctx, n)
		})
	}

	// Create first pool selecting pool1=true — matches node1 and node3.
	pool1 := testutil.NewPool("mem-conflict-pool1", testImageDigestRefA,
		testutil.WithNodeSelector(map[string]string{"pool1": "true"}))
	g.Expect(k8sClient.Create(ctx, pool1)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool1)
	})

	// Wait for pool1 to claim node1 and node3.
	for _, name := range []string{node1.Name, node3.Name} {
		name := name
		g.Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKey{Name: name}, &bootcv1alpha1.BootcNode{})
		}).Should(Succeed())
	}

	// Create second pool selecting pool2=true — matches node2 and node3,
	// but node3 is already owned by pool1 (conflict).
	pool2 := testutil.NewPool("mem-conflict-pool2", testImageDigestRefB,
		testutil.WithNodeSelector(map[string]string{"pool2": "true"}))
	g.Expect(k8sClient.Create(ctx, pool2)).To(Succeed())
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, pool2)
	})

	// Wait for pool2 to be marked Degraded/NodeConflict.
	g.Eventually(func() ([]metav1.Condition, error) {
		var p bootcv1alpha1.BootcNodePool
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool2), &p)
		return p.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolDegraded),
		HaveField("Status", metav1.ConditionTrue),
		HaveField("Reason", bootcv1alpha1.PoolNodeConflict),
		HaveField("Message", ContainSubstring(pool1.Name)),
	)))

	// Verify pool1 is not degraded.
	var p1 bootcv1alpha1.BootcNodePool
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pool1), &p1)).To(Succeed())
	g.Expect(p1.Status.Conditions).NotTo(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolDegraded),
		HaveField("Status", metav1.ConditionTrue),
	)), "pool1 should not be degraded")

	// Verify non-contested nodes are still handled: node1 by pool1,
	// node2 by pool2.
	for _, tc := range []struct {
		nodeName string
		poolName string
	}{
		{node1.Name, pool1.Name},
		{node2.Name, pool2.Name},
	} {
		tc := tc
		g.Eventually(func() (*metav1.OwnerReference, error) {
			var bn bootcv1alpha1.BootcNode
			err := k8sClient.Get(ctx, client.ObjectKey{Name: tc.nodeName}, &bn)
			return metav1.GetControllerOf(&bn), err
		}).Should(And(Not(BeNil()), HaveField("Name", tc.poolName)))
	}

	// Now resolve the conflict: remove pool1=true from node3 so pool1
	// releases it, then pool2 can claim it.
	var freshNode3 corev1.Node
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node3), &freshNode3)).To(Succeed())
	patch := client.StrategicMergeFrom(freshNode3.DeepCopy())
	delete(freshNode3.Labels, "pool1")
	g.Expect(k8sClient.Patch(ctx, &freshNode3, patch)).To(Succeed())

	// Verify pool2 recovers: Degraded condition should clear.
	g.Eventually(func() ([]metav1.Condition, error) {
		var p bootcv1alpha1.BootcNodePool
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool2), &p)
		return p.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolDegraded),
		HaveField("Status", metav1.ConditionFalse),
		HaveField("Reason", bootcv1alpha1.PoolHealthy),
	)))

	// Verify node3 is now owned by pool2.
	g.Eventually(func() (*metav1.OwnerReference, error) {
		var bn bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(node3), &bn)
		return metav1.GetControllerOf(&bn), err
	}).Should(And(
		Not(BeNil()),
		HaveField("Name", pool2.Name),
	))
}
