// SPDX-License-Identifier: Apache-2.0

package controller

// os_not_ready_taint_test.go contains unit tests for the bootc.dev/os-not-ready
// startup taint feature: ownership recording at join time, taint removal via the
// reboot-slot path (restoreNodeState), and taint removal via the already-up-to-date
// path (removeOsNotReadyTaintAndAnnotation / freeCompletedSlots).

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

// osNotReadyTaint returns the well-known bootc.dev/os-not-ready:NoSchedule taint.
func osNotReadyTaint() corev1.Taint {
	return corev1.Taint{
		Key:    bootcv1alpha1.TaintKeyOsNotReady,
		Effect: corev1.TaintEffectNoSchedule,
	}
}

// TestNodeHasOsNotReadyTaint verifies the taint detection helper.
func TestNodeHasOsNotReadyTaint(t *testing.T) {
	tests := []struct {
		name   string
		taints []corev1.Taint
		want   bool
	}{
		{
			name:   "taint present: detected",
			taints: []corev1.Taint{osNotReadyTaint()},
			want:   true,
		},
		{
			name:   "no taints: not detected",
			taints: nil,
			want:   false,
		},
		{
			name:   "different key same effect: not detected",
			taints: []corev1.Taint{{Key: "other.taint/key", Effect: corev1.TaintEffectNoSchedule}},
			want:   false,
		},
		{
			name: "correct key wrong effect: not detected",
			taints: []corev1.Taint{
				{Key: bootcv1alpha1.TaintKeyOsNotReady, Effect: corev1.TaintEffectNoExecute},
			},
			want: false,
		},
		{
			name: "taint among others: detected",
			taints: []corev1.Taint{
				{Key: "other/taint", Effect: corev1.TaintEffectNoSchedule},
				osNotReadyTaint(),
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := testutil.NewK8sNode("test-node", nil, testutil.WithTaints(tc.taints))
			NewWithT(t).Expect(nodeHasOsNotReadyTaint(node)).To(Equal(tc.want))
		})
	}
}

func TestNodePredicatesDetectOsNotReadyTaintChanges(t *testing.T) {
	g := NewWithT(t)
	oldNode := testutil.NewK8sNode("test-node", nil)
	newNode := oldNode.DeepCopy()
	newNode.Spec.Taints = []corev1.Taint{osNotReadyTaint()}

	g.Expect(nodePredicates().Update(event.UpdateEvent{
		ObjectOld: oldNode,
		ObjectNew: newNode,
	})).To(BeTrue())
	g.Expect(nodePredicates().Update(event.UpdateEvent{
		ObjectOld: newNode,
		ObjectNew: oldNode,
	})).To(BeTrue())
}

// TestCreateBootcNodeRecordsOsNotReadyTaintOwnership verifies that createBootcNode
// sets AnnotationOwnsOsNotReadyTaint on the BootcNode when the K8s Node carries
// the startup taint, and does not set it when the taint is absent.
func TestCreateBootcNodeRecordsOsNotReadyTaintOwnership(t *testing.T) {
	tests := []struct {
		name      string
		hasTaint  bool
		wantOwned bool
	}{
		{
			name:      "node has startup taint: ownership recorded",
			hasTaint:  true,
			wantOwned: true,
		},
		{
			name:      "node has no startup taint: ownership not recorded",
			hasTaint:  false,
			wantOwned: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			pool := testutil.NewPool(
				"test-pool",
				testImageDigestRefA,
				testutil.WithWorkerSelector(),
			)
			pool.Status.TargetDigest = testDigestA

			var nodeOptions []testutil.K8sNodeOption
			if tc.hasTaint {
				nodeOptions = append(
					nodeOptions,
					testutil.WithTaints([]corev1.Taint{osNotReadyTaint()}),
				)
			}
			k8sNode := testutil.NewK8sNode("test-node", nil, nodeOptions...)

			// newFakeReconciler builds a reconciler with scheme already set up.
			r, _ := newFakeReconciler(k8sNode)

			bn, err := r.createBootcNode(ctx, pool, k8sNode)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(bn).NotTo(BeNil())

			if tc.wantOwned {
				g.Expect(bn.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsOsNotReadyTaint))
			} else {
				g.Expect(bn.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsOsNotReadyTaint))
			}
		})
	}
}

// TestRestoreNodeStateRemovesOsNotReadyTaint verifies that restoreNodeState
// removes the os-not-ready taint when the BootcNode carries the ownership
// annotation, and leaves it alone when the annotation is absent.
func TestRestoreNodeStateRemovesOsNotReadyTaint(t *testing.T) {
	tests := []struct {
		name        string
		ownsTaint   bool
		taintOnNode bool

		wantTaintPresent bool
		wantNodePatches  int
	}{
		{
			name:             "owns taint and taint is on node: taint removed in same patch as uncordon",
			ownsTaint:        true,
			taintOnNode:      true,
			wantTaintPresent: false,
			wantNodePatches:  1,
		},
		{
			name:             "owns taint but taint already gone: no node patch",
			ownsTaint:        true,
			taintOnNode:      false,
			wantTaintPresent: false,
			wantNodePatches:  0,
		},
		{
			name:             "does not own taint: taint left in place, no patch",
			ownsTaint:        false,
			taintOnNode:      true,
			wantTaintPresent: true,
			wantNodePatches:  0,
		},
		{
			name:             "neither taint nor ownership: no-op",
			ownsTaint:        false,
			taintOnNode:      false,
			wantTaintPresent: false,
			wantNodePatches:  0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			bnAnnotations := map[string]string{}
			if tc.ownsTaint {
				bnAnnotations[bootcv1alpha1.AnnotationOwnsOsNotReadyTaint] = "true"
			}
			bn := testutil.NewBootcNode("test-node", testImageDigestRefA)
			bn.Annotations = bnAnnotations

			var nodeOptions []testutil.K8sNodeOption
			if tc.taintOnNode {
				nodeOptions = append(
					nodeOptions,
					testutil.WithTaints([]corev1.Taint{osNotReadyTaint()}),
				)
			}
			k8sNode := testutil.NewK8sNode("test-node", nil, nodeOptions...)

			r, counting := newFakeReconciler(bn, k8sNode)
			g.Expect(r.restoreNodeState(ctx, bn, k8sNode, nil)).To(Succeed())

			g.Expect(counting.nodePatches).To(Equal(tc.wantNodePatches), "node patch count")

			var finalNode corev1.Node
			g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).To(Succeed())
			hasTaint := nodeHasOsNotReadyTaint(&finalNode)
			g.Expect(hasTaint).To(Equal(tc.wantTaintPresent), "taint presence")
		})
	}
}

// TestRestoreNodeStateRemovesOsNotReadyTaintWithUncordon verifies that when both
// the os-not-ready taint removal and an uncordon are needed, they are combined
// into a single Node patch.
func TestRestoreNodeStateRemovesOsNotReadyTaintWithUncordon(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	bn := testutil.NewBootcNode("test-node", testImageDigestRefA)
	bn.Annotations = map[string]string{
		bootcv1alpha1.AnnotationOwnsOsNotReadyTaint: "true",
		bootcv1alpha1.AnnotationWasCordoned:         "false",
	}

	k8sNode := testutil.NewK8sNode("test-node", nil,
		testutil.WithUnschedulable(true),
		testutil.WithTaints([]corev1.Taint{osNotReadyTaint()}),
	)

	r, counting := newFakeReconciler(bn, k8sNode)
	g.Expect(r.restoreNodeState(ctx, bn, k8sNode, nil)).To(Succeed())

	// Taint removal + uncordon must be a single patch.
	g.Expect(counting.nodePatches).To(Equal(1), "taint removal and uncordon should be one patch")

	var finalNode corev1.Node
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).To(Succeed())
	g.Expect(nodeHasOsNotReadyTaint(&finalNode)).To(BeFalse(), "taint should be removed")
	g.Expect(finalNode.Spec.Unschedulable).To(BeFalse(), "node should be uncordoned")
}

func TestRemoveBootcNodePreservesUnverifiedOsNotReadyTaint(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	bn := testutil.NewBootcNode("test-node", testImageDigestRefA)
	bn.Annotations = map[string]string{
		bootcv1alpha1.AnnotationOwnsOsNotReadyTaint: "true",
	}
	k8sNode := testutil.NewK8sNode("test-node", nil,
		testutil.WithTaints([]corev1.Taint{osNotReadyTaint()}),
	)

	r, _ := newFakeReconciler(bn, k8sNode)
	g.Expect(r.removeBootcNode(ctx, bn)).To(Succeed())

	var finalNode corev1.Node
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).To(Succeed())
	g.Expect(nodeHasOsNotReadyTaint(&finalNode)).To(BeTrue())
	var finalBN bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalBN)).
		To(MatchError(apierrors.IsNotFound, "IsNotFound"))
}

// TestFreeRebootSlotClearsOsNotReadyOwnershipAnnotation verifies that
// freeRebootSlot removes the AnnotationOwnsOsNotReadyTaint ownership
// annotation from the BootcNode as part of slot cleanup, in addition to
// removing the taint from the Node via restoreNodeState.
func TestFreeRebootSlotClearsOsNotReadyOwnershipAnnotation(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	bn := testutil.NewBootcNode("test-node", testImageDigestRefA)
	bn.Annotations = map[string]string{
		bootcv1alpha1.AnnotationInRebootSlot:        "",
		bootcv1alpha1.AnnotationWasCordoned:         "false",
		bootcv1alpha1.AnnotationOwnsOsNotReadyTaint: "true",
	}

	k8sNode := testutil.NewK8sNode("test-node", nil,
		testutil.WithUnschedulable(true),
		testutil.WithTaints([]corev1.Taint{osNotReadyTaint()}),
	)

	r, _ := newFakeReconciler(bn, k8sNode)
	g.Expect(r.freeRebootSlot(ctx, bn, k8sNode, nil)).To(Succeed())

	// Taint must be gone from the Node.
	var finalNode corev1.Node
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).To(Succeed())
	g.Expect(nodeHasOsNotReadyTaint(&finalNode)).To(BeFalse(), "taint should be removed")

	// All slot and ownership annotations must be gone from the BootcNode.
	var finalBN bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalBN)).To(Succeed())
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationInRebootSlot))
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationWasCordoned))
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsOsNotReadyTaint))
}

// TestFreeCompletedSlotsRemovesTaintForAlreadyUpToDateNode verifies the
// already-up-to-date path: a node that was running the target image when it
// joined (never held a reboot slot) has its startup taint removed and its
// ownership annotation cleared once it is UpToDate and Ready.
func TestFreeCompletedSlotsRemovesTaintForAlreadyUpToDateNode(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// BootcNode is UpToDate (booted digest matches desired), owns the taint,
	// but has never held a reboot slot.
	bn := testutil.NewBootcNode(
		"test-node",
		testImageDigestRefA,
		testutil.WithBootedDigest(testDigestA),
		testutil.WithNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionTrue,
			bootcv1alpha1.NodeReasonIdle,
		),
		testutil.WithNodeAnnotation(bootcv1alpha1.AnnotationOwnsOsNotReadyTaint, "true"),
	)

	k8sNode := testutil.NewK8sNode("test-node", nil,
		testutil.WithTaints([]corev1.Taint{osNotReadyTaint()}),
		testutil.WithK8sNodeConditions([]corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}),
	)

	r, _ := newFakeReconciler(bn, k8sNode)

	rs := &rolloutState{upToDate: []*bootcv1alpha1.BootcNode{bn}}
	g.Expect(r.freeCompletedSlots(ctx, rs)).To(Succeed())

	// Taint must be removed from the Node.
	var finalNode corev1.Node
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).To(Succeed())
	g.Expect(nodeHasOsNotReadyTaint(&finalNode)).To(BeFalse(), "taint should be removed")

	// Ownership annotation must be cleared from the BootcNode.
	var finalBN bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalBN)).To(Succeed())
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsOsNotReadyTaint))

	// The node was never in a reboot slot; slot count must be unchanged.
	g.Expect(rs.occupiedSlots).To(Equal(0))
}

// TestFreeCompletedSlotsHoldsTaintWhenNotReady verifies that the taint is not
// removed for an UpToDate node that is not yet Ready, even if the ownership
// annotation is present.
func TestFreeCompletedSlotsHoldsTaintWhenNotReady(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	bn := testutil.NewBootcNode(
		"test-node",
		testImageDigestRefA,
		testutil.WithBootedDigest(testDigestA),
		testutil.WithNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionTrue,
			bootcv1alpha1.NodeReasonIdle,
		),
		testutil.WithNodeAnnotation(bootcv1alpha1.AnnotationOwnsOsNotReadyTaint, "true"),
	)

	// Node is not Ready.
	k8sNode := testutil.NewK8sNode("test-node", nil,
		testutil.WithTaints([]corev1.Taint{osNotReadyTaint()}),
		testutil.WithK8sNodeConditions([]corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
		}),
	)

	r, counting := newFakeReconciler(bn, k8sNode)
	rs := &rolloutState{upToDate: []*bootcv1alpha1.BootcNode{bn}}
	g.Expect(r.freeCompletedSlots(ctx, rs)).To(Succeed())

	// No patches should have been issued.
	g.Expect(counting.nodePatches).To(Equal(0), "taint must not be removed when node is not Ready")

	// Taint must still be present.
	var finalNode corev1.Node
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).To(Succeed())
	g.Expect(nodeHasOsNotReadyTaint(&finalNode)).
		To(BeTrue(), "taint should be preserved until Ready")
}

// TestFreeCompletedSlotsNoOpWhenNoTaintOwnership verifies that freeCompletedSlots
// skips UpToDate nodes that carry neither a reboot slot nor the taint ownership
// annotation, issuing no API calls.
func TestFreeCompletedSlotsNoOpWhenNoTaintOwnership(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// UpToDate node with no slot and no taint ownership.
	bn := testutil.NewBootcNode(
		"test-node",
		testImageDigestRefA,
		testutil.WithBootedDigest(testDigestA),
		testutil.WithNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionTrue,
			bootcv1alpha1.NodeReasonIdle,
		),
	)

	k8sNode := testutil.NewK8sNode("test-node", nil,
		testutil.WithK8sNodeConditions([]corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}),
	)

	r, counting := newFakeReconciler(bn, k8sNode)
	rs := &rolloutState{upToDate: []*bootcv1alpha1.BootcNode{bn}}
	g.Expect(r.freeCompletedSlots(ctx, rs)).To(Succeed())

	g.Expect(counting.nodePatches).To(Equal(0), "no patches should be issued")
}

// TestDriveRolloutRemovesTaintForAlreadyUpToDateNode is an integration-style
// unit test verifying that driveRollout removes the startup taint for a node
// that is UpToDate and Ready on its first reconcile (never needed a reboot slot).
func TestDriveRolloutRemovesTaintForAlreadyUpToDateNode(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// BootcNode is UpToDate (booted digest matches pool target), owns the taint.
	bn := testutil.NewBootcNode(
		"up-to-date-node",
		testImageDigestRefA,
		testutil.WithBootedDigest(testDigestA),
		testutil.WithNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionTrue,
			bootcv1alpha1.NodeReasonIdle,
		),
		testutil.WithNodeAnnotation(bootcv1alpha1.AnnotationOwnsOsNotReadyTaint, "true"),
	)

	k8sNode := testutil.NewK8sNode("up-to-date-node", nil,
		testutil.WithTaints([]corev1.Taint{osNotReadyTaint()}),
		testutil.WithK8sNodeConditions([]corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		}),
	)

	pool := testutil.NewPool("test-pool", testImageDigestRefA, testutil.WithWorkerSelector())
	pool.Status.TargetDigest = testDigestA

	r, _ := newFakeReconciler(bn, k8sNode)
	ownedNodes := map[string]*bootcv1alpha1.BootcNode{"up-to-date-node": bn}

	_, err := r.driveRollout(ctx, pool, ownedNodes)
	g.Expect(err).NotTo(HaveOccurred())

	// Taint must be gone from the Node.
	var finalNode corev1.Node
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "up-to-date-node"}, &finalNode)).To(Succeed())
	g.Expect(nodeHasOsNotReadyTaint(&finalNode)).To(BeFalse(),
		"startup taint must be removed for a node that is already up-to-date")

	// Ownership annotation must be cleared from the BootcNode.
	var finalBN bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "up-to-date-node"}, &finalBN)).To(Succeed())
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsOsNotReadyTaint))
}
