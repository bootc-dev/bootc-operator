// SPDX-License-Identifier: Apache-2.0

package controller

// karpenter_test.go contains unit tests for the Karpenter integration logic in
// karpenter.go and the Karpenter-specific paths in rollout.go and
// bootcnodepool_controller.go. Tests that require a live API server (envtest)
// live in rollout_karpenter_envtest_test.go.

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

// patchCountingClient wraps a client.Client and counts Patch calls to the
// Node object (by GVK). Used to assert that freeRebootSlot doesn't
// double-patch the Node.
type patchCountingClient struct {
	client.Client
	nodePatches int
}

func (c *patchCountingClient) Patch(
	ctx context.Context,
	obj client.Object,
	patch client.Patch,
	opts ...client.PatchOption,
) error {
	if _, ok := obj.(*corev1.Node); ok {
		c.nodePatches++
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// newFakeReconciler builds a BootcNodePoolReconciler backed by a fake
// client pre-seeded with the given objects. It also returns the counting
// wrapper so callers can assert the number of Node patches.
func newFakeReconciler(objs ...client.Object) (*BootcNodePoolReconciler, *patchCountingClient) {
	sc := runtime.NewScheme()
	_ = scheme.AddToScheme(sc)
	_ = bootcv1alpha1.AddToScheme(sc)

	base := fake.NewClientBuilder().
		WithScheme(sc).
		WithObjects(objs...).
		Build()

	counting := &patchCountingClient{Client: base}
	r := &BootcNodePoolReconciler{Client: counting}
	return r, counting
}

func TestKarpenterRebootActive(t *testing.T) {
	claim := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": karpenterNodeClaimAPIVersion,
		"kind":       karpenterNodeClaimKind,
		"metadata": map[string]interface{}{
			"name": "claim",
		},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Rebooting", "status": "True"},
			},
		},
	}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "node",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: karpenterNodeClaimAPIVersion,
			Kind:       karpenterNodeClaimKind,
			Name:       "claim",
			UID:        types.UID("claim-uid"),
		}},
	}}

	r, _ := newFakeReconciler(node, claim)
	active, _, _, err := r.isKarpenterDisruptionActive(context.Background(), node)
	NewWithT(t).Expect(err).NotTo(HaveOccurred())
	NewWithT(t).Expect(active).To(BeTrue())

	node.Spec.Taints = []corev1.Taint{{
		Key:    karpenterRebootingTaintKey,
		Effect: karpenterRebootingTaintEffect,
	}}
	claim.Object["status"] = map[string]interface{}{}
	active, _, _, err = r.isKarpenterDisruptionActive(context.Background(), node)
	NewWithT(t).Expect(err).NotTo(HaveOccurred())
	NewWithT(t).Expect(active).To(BeTrue())
}

// TestRestoreNodeState verifies that restoreNodeState removes owned Karpenter
// annotations and restores cordon state in a single patch, covering all
// combinations of ownership and cordon state.
func TestRestoreNodeState(t *testing.T) {
	tests := []struct {
		name            string
		ownsRepair      bool
		ownsDisrupt     bool
		wasCordoned     string // "true", "false", or "" (absent)
		nodeCordoned    bool
		nodeAnnotations map[string]string

		wantNodePatches    int
		wantCordoned       bool
		wantRepairPresent  bool
		wantDisruptPresent bool
	}{
		{
			name:         "owns both annotations, operator cordoned: single patch removes all",
			ownsRepair:   true,
			ownsDisrupt:  true,
			wasCordoned:  "false",
			nodeCordoned: true,
			nodeAnnotations: map[string]string{
				karpenterDoNotRepairAnnotationKey:  "true",
				karpenterDoNotDisruptAnnotationKey: "true",
			},
			wantNodePatches:    1,
			wantCordoned:       false,
			wantRepairPresent:  false,
			wantDisruptPresent: false,
		},
		{
			name:         "owns both annotations, pre-cordoned: single patch, cordon preserved",
			ownsRepair:   true,
			ownsDisrupt:  true,
			wasCordoned:  "true",
			nodeCordoned: true,
			nodeAnnotations: map[string]string{
				karpenterDoNotRepairAnnotationKey:  "true",
				karpenterDoNotDisruptAnnotationKey: "true",
			},
			wantNodePatches:    1,
			wantCordoned:       true,
			wantRepairPresent:  false,
			wantDisruptPresent: false,
		},
		{
			name:         "owns only do-not-disrupt: removes only that one",
			ownsRepair:   false,
			ownsDisrupt:  true,
			wasCordoned:  "true",
			nodeCordoned: true,
			nodeAnnotations: map[string]string{
				karpenterDoNotRepairAnnotationKey:  "true",
				karpenterDoNotDisruptAnnotationKey: "true",
			},
			wantNodePatches:    1,
			wantCordoned:       true,
			wantRepairPresent:  true,
			wantDisruptPresent: false,
		},
		{
			name:            "no karpenter ownership, operator cordoned: single uncordon patch",
			ownsRepair:      false,
			ownsDisrupt:     false,
			wasCordoned:     "false",
			nodeCordoned:    true,
			wantNodePatches: 1,
			wantCordoned:    false,
		},
		{
			name:            "no karpenter ownership, pre-cordoned: no patch needed",
			ownsRepair:      false,
			ownsDisrupt:     false,
			wasCordoned:     "true",
			nodeCordoned:    true,
			wantNodePatches: 0,
			wantCordoned:    true,
		},
		{
			name:            "nothing to do: zero patches",
			ownsRepair:      false,
			ownsDisrupt:     false,
			wasCordoned:     "",
			nodeCordoned:    false,
			wantNodePatches: 0,
			wantCordoned:    false,
		},
		{
			// Karpenter annotations present on the node but set by an external
			// actor, not the operator. The operator must not touch them.
			name:         "annotations present but not owned: zero patches, annotations preserved",
			ownsRepair:   false,
			ownsDisrupt:  false,
			wasCordoned:  "false",
			nodeCordoned: true,
			nodeAnnotations: map[string]string{
				karpenterDoNotRepairAnnotationKey:  "true",
				karpenterDoNotDisruptAnnotationKey: "true",
			},
			wantNodePatches:    1, // uncordon only
			wantCordoned:       false,
			wantRepairPresent:  true,
			wantDisruptPresent: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			bnAnnotations := map[string]string{}
			if tc.wasCordoned != "" {
				bnAnnotations[bootcv1alpha1.AnnotationWasCordoned] = tc.wasCordoned
			}
			if tc.ownsRepair {
				bnAnnotations[bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair] = "true"
			}
			if tc.ownsDisrupt {
				bnAnnotations[bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt] = "true"
			}
			bn := testutil.NewNode("test-node", testImageDigestRefA)
			bn.Annotations = bnAnnotations

			k8sNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node",
					Annotations: tc.nodeAnnotations,
				},
				Spec: corev1.NodeSpec{Unschedulable: tc.nodeCordoned},
			}

			r, counting := newFakeReconciler(bn, k8sNode)

			err := r.restoreNodeState(ctx, bn, k8sNode)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(counting.nodePatches).To(Equal(tc.wantNodePatches), "node patch count")

			var finalNode corev1.Node
			g.Expect(counting.Client.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).
				To(Succeed())
			g.Expect(finalNode.Spec.Unschedulable).To(Equal(tc.wantCordoned), "cordon state")
			if tc.wantRepairPresent {
				g.Expect(finalNode.Annotations).To(HaveKey(karpenterDoNotRepairAnnotationKey))
			} else {
				g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenterDoNotRepairAnnotationKey))
			}
			if tc.wantDisruptPresent {
				g.Expect(finalNode.Annotations).To(HaveKey(karpenterDoNotDisruptAnnotationKey))
			} else {
				g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenterDoNotDisruptAnnotationKey))
			}
		})
	}
}

// TestAssignRebootSlotNodePatchCount verifies that assignRebootSlot issues at
// most one patch to the corev1.Node regardless of Karpenter ownership and
// prior cordon state.
func TestAssignRebootSlotNodePatchCount(t *testing.T) {
	karpenterOwnerRef := metav1.OwnerReference{
		APIVersion: karpenterNodeClaimAPIVersion,
		Kind:       karpenterNodeClaimKind,
		Name:       "test-claim",
		UID:        "test-claim-uid",
	}

	tests := []struct {
		name                string
		karpenterOwned      bool
		preExistAnnotations map[string]string // annotations already on the Node
		preCordoned         bool

		wantNodePatches    int
		wantCordoned       bool
		wantRepairPresent  bool
		wantDisruptPresent bool
	}{
		{
			name:               "karpenter owned, not cordoned: one patch for cordon + both annotations",
			karpenterOwned:     true,
			wantNodePatches:    1,
			wantCordoned:       true,
			wantRepairPresent:  true,
			wantDisruptPresent: true,
		},
		{
			name:               "karpenter owned, pre-cordoned: one patch for annotations only",
			karpenterOwned:     true,
			preCordoned:        true,
			wantNodePatches:    1,
			wantCordoned:       true,
			wantRepairPresent:  true,
			wantDisruptPresent: true,
		},
		{
			name:           "karpenter owned, both annotations pre-existing: zero node patches",
			karpenterOwned: true,
			preCordoned:    true,
			preExistAnnotations: map[string]string{
				karpenterDoNotRepairAnnotationKey:  "true",
				karpenterDoNotDisruptAnnotationKey: "true",
			},
			wantNodePatches:    0,
			wantCordoned:       true,
			wantRepairPresent:  true,
			wantDisruptPresent: true,
		},
		{
			name:            "non-karpenter, not cordoned: one patch for cordon only",
			karpenterOwned:  false,
			wantNodePatches: 1,
			wantCordoned:    true,
		},
		{
			name:            "non-karpenter, pre-cordoned: zero node patches",
			karpenterOwned:  false,
			preCordoned:     true,
			wantNodePatches: 0,
			wantCordoned:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			bn := testutil.NewNode("test-node", testImageDigestRefA)

			k8sNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node",
					Annotations: tc.preExistAnnotations,
				},
				Spec: corev1.NodeSpec{Unschedulable: tc.preCordoned},
			}
			if tc.karpenterOwned {
				k8sNode.OwnerReferences = []metav1.OwnerReference{karpenterOwnerRef}
			}

			r, counting := newFakeReconciler(bn, k8sNode)

			err := r.assignRebootSlot(ctx, bn, k8sNode, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(counting.nodePatches).To(Equal(tc.wantNodePatches), "node patch count")

			var finalNode corev1.Node
			g.Expect(counting.Client.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).
				To(Succeed())
			g.Expect(finalNode.Spec.Unschedulable).To(Equal(tc.wantCordoned), "cordon state")
			if tc.wantRepairPresent {
				g.Expect(finalNode.Annotations).To(HaveKey(karpenterDoNotRepairAnnotationKey))
			} else {
				g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenterDoNotRepairAnnotationKey))
			}
			if tc.wantDisruptPresent {
				g.Expect(finalNode.Annotations).To(HaveKey(karpenterDoNotDisruptAnnotationKey))
			} else {
				g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenterDoNotDisruptAnnotationKey))
			}

			// The BootcNode must always have the slot annotation.
			var finalBN bootcv1alpha1.BootcNode
			g.Expect(counting.Client.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalBN)).
				To(Succeed())
			g.Expect(finalBN.Annotations).To(HaveKey(bootcv1alpha1.AnnotationInRebootSlot))
		})
	}
}

// TestKarpenterNodeClaimAnnotationLifecycle verifies that assignRebootSlot
// applies do-not-repair and do-not-disrupt to the NodeClaim when absent, and
// that freeRebootSlot removes only annotations the operator owns.
func TestKarpenterNodeClaimAnnotationLifecycle(t *testing.T) {
	type annotationPair struct {
		claimKey            string
		ownershipAnnotation string
	}
	annotations := []annotationPair{
		{
			karpenterDoNotDisruptAnnotationKey,
			bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt,
		},
		{
			karpenterDoNotRepairAnnotationKey,
			bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotRepair,
		},
	}

	tests := []struct {
		name                  string
		preExistingAnnotation bool
		wantDuring            bool
		wantOwned             bool
		wantAfter             bool
	}{
		{
			name:       "operator-owned annotation is applied and removed",
			wantDuring: true,
			wantOwned:  true,
			wantAfter:  false,
		},
		{
			name:                  "pre-existing annotation is preserved",
			preExistingAnnotation: true,
			wantDuring:            true,
			wantOwned:             false,
			wantAfter:             true,
		},
	}

	for _, ann := range annotations {
		for _, tc := range tests {
			t.Run(ann.claimKey+"/"+tc.name, func(t *testing.T) {
				g := NewWithT(t)
				ctx := context.Background()

				claim := &unstructured.Unstructured{Object: map[string]interface{}{
					"apiVersion": karpenterNodeClaimAPIVersion,
					"kind":       karpenterNodeClaimKind,
					"metadata": map[string]interface{}{
						"name": "test-claim",
					},
				}}
				if tc.preExistingAnnotation {
					claim.SetAnnotations(map[string]string{ann.claimKey: "true"})
				}

				node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: karpenterNodeClaimAPIVersion,
						Kind:       karpenterNodeClaimKind,
						Name:       "test-claim",
						UID:        types.UID("test-claim-uid"),
					}},
				}}
				bn := testutil.NewNode("test-node", testImageDigestRefA)

				r, _ := newFakeReconciler(bn, node, claim)
				g.Expect(r.assignRebootSlot(ctx, bn, node, nil)).To(Succeed())

				var duringClaim unstructured.Unstructured
				duringClaim.SetAPIVersion(karpenterNodeClaimAPIVersion)
				duringClaim.SetKind(karpenterNodeClaimKind)
				g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-claim"}, &duringClaim)).
					To(Succeed())
				if tc.wantDuring {
					g.Expect(duringClaim.GetAnnotations()).
						To(HaveKeyWithValue(ann.claimKey, "true"))
				} else {
					g.Expect(duringClaim.GetAnnotations()).NotTo(HaveKey(ann.claimKey))
				}

				var duringBN bootcv1alpha1.BootcNode
				g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &duringBN)).To(Succeed())
				if tc.wantOwned {
					g.Expect(duringBN.Annotations).To(HaveKey(ann.ownershipAnnotation))
				} else {
					g.Expect(duringBN.Annotations).NotTo(HaveKey(ann.ownershipAnnotation))
				}

				g.Expect(r.freeRebootSlot(ctx, &duringBN, node)).To(Succeed())

				var afterClaim unstructured.Unstructured
				afterClaim.SetAPIVersion(karpenterNodeClaimAPIVersion)
				afterClaim.SetKind(karpenterNodeClaimKind)
				g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-claim"}, &afterClaim)).
					To(Succeed())
				if tc.wantAfter {
					g.Expect(afterClaim.GetAnnotations()).To(HaveKeyWithValue(ann.claimKey, "true"))
				} else {
					g.Expect(afterClaim.GetAnnotations()).NotTo(HaveKey(ann.claimKey))
				}
			})
		}
	}
}

// TestFreeRebootSlotNodeClaimGone verifies that freeRebootSlot succeeds cleanly
// when the NodeClaim no longer exists by the time the slot is freed.
//
// The scenario unfolds as follows:
//
//  1. The operator assigns the node a reboot slot and, because the node is
//     Karpenter-managed, sets do-not-repair / do-not-disrupt annotations on
//     both the corev1.Node and its backing NodeClaim to prevent Karpenter from
//     interfering while the OS update is in progress. It records ownership of
//     those annotations via AnnotationOwnsKarpenterNodeClaimDoNotRepair and
//     AnnotationOwnsKarpenterNodeClaimDoNotDisrupt on the BootcNode.
//
//  2. The node reboots and comes back running the new image. The reconciler
//     calls freeRebootSlot → restoreNodeState, which fetches the NodeClaim via
//     getKarpenterNodeClaim so it can remove the annotations it added.
//
//  3. Between step 1 and step 2 Karpenter forcefully replaced the node via a
//     mechanism that the annotations do not block — for example a spot-instance
//     interruption, node expiration (expireAfter), or manual NodeClaim
//     deletion. (do-not-repair and do-not-disrupt only suppress Node Auto
//     Repair and voluntary disruption respectively; they cannot prevent these
//     forceful termination paths.) Karpenter deletes the NodeClaim and
//     provisions a new one. The original NodeClaim therefore returns a 404
//     (IsNotFound) from the API server.
//
// getKarpenterNodeClaim treats a 404 (or no-match) as a nil return, so
// restoreNodeState skips the NodeClaim patch and returns nil. This test
// confirms the operator does not surface that 404 as an error, and that it
// still clears all slot/ownership annotations and uncordons the node, leaving
// state clean for the next reconcile cycle.
func TestFreeRebootSlotNodeClaimGone(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// Build a BootcNode with both NodeClaim ownership annotations set.
	bn := testutil.NewNode("test-node", testImageDigestRefA)
	bn.Annotations = map[string]string{
		bootcv1alpha1.AnnotationInRebootSlot:                       "",
		bootcv1alpha1.AnnotationWasCordoned:                        "false",
		bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotRepair:  "true",
		bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt: "true",
	}

	// Node has an owner reference pointing to a NodeClaim that no longer exists.
	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: karpenterNodeClaimAPIVersion,
				Kind:       karpenterNodeClaimKind,
				Name:       "gone-claim",
				UID:        types.UID("gone-uid"),
			}},
		},
		Spec: corev1.NodeSpec{Unschedulable: true},
	}

	// The NodeClaim is intentionally absent from the fake client.
	r, _ := newFakeReconciler(bn, k8sNode)
	g.Expect(r.freeRebootSlot(ctx, bn, k8sNode)).To(Succeed())

	// Cordon should be restored (was-cordoned=false).
	var finalNode corev1.Node
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).To(Succeed())
	g.Expect(finalNode.Spec.Unschedulable).To(BeFalse(), "node should be uncordoned")

	// All slot annotations should be cleared.
	var finalBN bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalBN)).To(Succeed())
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationInRebootSlot))
	g.Expect(finalBN.Annotations).
		NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotRepair))
	g.Expect(finalBN.Annotations).
		NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt))
}

// TestDriveRolloutDefersWhenKarpenterRebootActive verifies that driveRollout
// skips slot assignment and drain for a node whose NodeClaim has
// Rebooting=True, leaving the BootcNode without an in-reboot-slot annotation.
func TestDriveRolloutDefersWhenKarpenterRebootActive(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// A staged BootcNode ready for a reboot slot.
	bn := testutil.NewNode("staged-node", testImageDigestRefA,
		testutil.WithBootedDigest(testDigestB),
		testutil.WithNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionFalse,
			bootcv1alpha1.NodeReasonStaged,
		),
	)

	// The corresponding K8s Node is owned by a NodeClaim that is currently rebooting.
	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "staged-node",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: karpenterNodeClaimAPIVersion,
				Kind:       karpenterNodeClaimKind,
				Name:       "active-claim",
				UID:        types.UID("active-claim-uid"),
			}},
		},
	}

	// NodeClaim with Rebooting=True.
	claim := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": karpenterNodeClaimAPIVersion,
		"kind":       karpenterNodeClaimKind,
		"metadata": map[string]interface{}{
			"name": "active-claim",
		},
		"status": map[string]interface{}{
			"nodeName": "staged-node",
			"conditions": []interface{}{
				map[string]interface{}{"type": "Rebooting", "status": "True"},
			},
		},
	}}

	pool := testutil.NewPool("test-pool", testImageDigestRefA, testutil.WithWorkerSelector())
	pool.Status.TargetDigest = testDigestA

	r, _ := newFakeReconciler(bn, k8sNode, claim)
	ownedNodes := map[string]*bootcv1alpha1.BootcNode{"staged-node": bn}

	rs, err := r.driveRollout(ctx, pool, ownedNodes)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs).NotTo(BeNil())

	// The node must not have been assigned a reboot slot.
	var finalBN bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "staged-node"}, &finalBN)).To(Succeed())
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationInRebootSlot),
		"slot must not be assigned while Karpenter reboot is active")
}

// TestFreeRebootSlotNodePatchCount verifies that freeRebootSlot issues at most
// one patch to the corev1.Node regardless of which combination of Karpenter
// annotations and cordon state are in play.
func TestFreeRebootSlotNodePatchCount(t *testing.T) {
	tests := []struct {
		name string

		// BootcNode annotation state
		ownsRepair  bool
		ownsDisrupt bool
		wasCordoned string // "true", "false", or "" (absent)

		// Node state
		nodeCordoned    bool
		nodeAnnotations map[string]string

		// Expected outcomes
		wantNodePatches    int
		wantCordoned       bool
		wantRepairPresent  bool
		wantDisruptPresent bool
	}{
		{
			// Karpenter annotations owned + operator cordoned: remove both
			// annotations and uncordon in a single patch.
			name:         "karpenter owned, operator cordoned: one patch",
			ownsRepair:   true,
			ownsDisrupt:  true,
			wasCordoned:  "false",
			nodeCordoned: true,
			nodeAnnotations: map[string]string{
				karpenterDoNotRepairAnnotationKey:  "true",
				karpenterDoNotDisruptAnnotationKey: "true",
			},
			wantNodePatches:    1,
			wantCordoned:       false,
			wantRepairPresent:  false,
			wantDisruptPresent: false,
		},
		{
			// Karpenter annotations owned + node was pre-cordoned: remove
			// annotations but leave the cordon, still a single patch.
			name:         "karpenter owned, pre-cordoned: one patch, cordon preserved",
			ownsRepair:   true,
			ownsDisrupt:  true,
			wasCordoned:  "true",
			nodeCordoned: true,
			nodeAnnotations: map[string]string{
				karpenterDoNotRepairAnnotationKey:  "true",
				karpenterDoNotDisruptAnnotationKey: "true",
			},
			wantNodePatches:    1,
			wantCordoned:       true,
			wantRepairPresent:  false,
			wantDisruptPresent: false,
		},
		{
			// No Karpenter ownership + operator cordoned: single uncordon patch.
			name:            "no karpenter, operator cordoned: one patch",
			ownsRepair:      false,
			ownsDisrupt:     false,
			wasCordoned:     "false",
			nodeCordoned:    true,
			wantNodePatches: 1,
			wantCordoned:    false,
		},
		{
			// No Karpenter ownership + node was pre-cordoned: no node patch needed.
			name:            "no karpenter, pre-cordoned: zero patches",
			ownsRepair:      false,
			ownsDisrupt:     false,
			wasCordoned:     "true",
			nodeCordoned:    true,
			wantNodePatches: 0,
			wantCordoned:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			// Build the BootcNode with the appropriate slot annotations.
			bnAnnotations := map[string]string{
				bootcv1alpha1.AnnotationInRebootSlot: "",
			}
			if tc.wasCordoned != "" {
				bnAnnotations[bootcv1alpha1.AnnotationWasCordoned] = tc.wasCordoned
			}
			if tc.ownsRepair {
				bnAnnotations[bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair] = "true"
			}
			if tc.ownsDisrupt {
				bnAnnotations[bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt] = "true"
			}
			bn := testutil.NewNode("test-node", testImageDigestRefA)
			bn.Annotations = bnAnnotations

			// Build the corev1.Node.
			k8sNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node",
					Annotations: tc.nodeAnnotations,
				},
				Spec: corev1.NodeSpec{Unschedulable: tc.nodeCordoned},
			}

			r, counting := newFakeReconciler(bn, k8sNode)

			err := r.freeRebootSlot(ctx, bn, k8sNode)
			g.Expect(err).NotTo(HaveOccurred())

			g.Expect(counting.nodePatches).To(Equal(tc.wantNodePatches),
				"unexpected number of Node patches")

			// Verify the final node state from the fake store.
			var finalNode corev1.Node
			g.Expect(counting.Client.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).
				To(Succeed())
			g.Expect(finalNode.Spec.Unschedulable).To(Equal(tc.wantCordoned), "cordon state")
			if tc.ownsRepair {
				g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenterDoNotRepairAnnotationKey))
			}
			if tc.ownsDisrupt {
				g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenterDoNotDisruptAnnotationKey))
			}

			// Verify slot annotations are cleared from the BootcNode.
			var finalBN bootcv1alpha1.BootcNode
			g.Expect(counting.Client.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalBN)).
				To(Succeed())
			g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationInRebootSlot))
			g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationWasCordoned))
			g.Expect(finalBN.Annotations).
				NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair))
			g.Expect(finalBN.Annotations).
				NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt))
		})
	}
}

// TestIsKarpenterDisruptionActive verifies that isKarpenterDisruptionActive detects
// all relevant disruption signals:
//   - Node taint karpenter.sh/rebooting:NoSchedule
//   - Node taint karpenter.sh/disrupted:NoSchedule
//   - Node DeletionTimestamp set
//   - NodeClaim DeletionTimestamp set
//   - NodeClaim condition DisruptionReason=True
//   - NodeClaim condition InstanceTerminating=True
//   - NodeClaim missing but owner ref present → claimMissing=true
func TestIsKarpenterDisruptionActive(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name string
		// node setup
		nodeTaints         []corev1.Taint
		nodeDeletionTS     *metav1.Time
		nodeKarpenterOwned bool
		// claim setup
		claimConditions []interface{}
		claimDeletionTS *metav1.Time
		claimAbsent     bool // NodeClaim owner ref present but object 404
		// expected
		wantActive       bool
		wantClaimMissing bool
	}{
		{
			name:       "idle node, no owner ref: inactive",
			wantActive: false,
		},
		{
			name:               "rebooting taint: active",
			nodeKarpenterOwned: true,
			nodeTaints: []corev1.Taint{{
				Key:    karpenterRebootingTaintKey,
				Effect: karpenterRebootingTaintEffect,
			}},
			wantActive: true,
		},
		{
			name:               "disrupted taint: active",
			nodeKarpenterOwned: true,
			nodeTaints: []corev1.Taint{{
				Key:    karpenterDisruptedTaintKey,
				Effect: karpenterRebootingTaintEffect,
			}},
			wantActive: true,
		},
		{
			name:               "node deletion timestamp: active",
			nodeKarpenterOwned: true,
			nodeDeletionTS:     &now,
			wantActive:         true,
		},
		{
			name:               "NodeClaim deletion timestamp: active",
			nodeKarpenterOwned: true,
			claimDeletionTS:    &now,
			wantActive:         true,
		},
		{
			name:               "NodeClaim Rebooting=True condition: active",
			nodeKarpenterOwned: true,
			claimConditions: []interface{}{
				map[string]interface{}{"type": "Rebooting", "status": "True"},
			},
			wantActive: true,
		},
		{
			name:               "NodeClaim DisruptionReason=True condition: active",
			nodeKarpenterOwned: true,
			claimConditions: []interface{}{
				map[string]interface{}{"type": "DisruptionReason", "status": "True"},
			},
			wantActive: true,
		},
		{
			name:               "NodeClaim InstanceTerminating=True condition: active",
			nodeKarpenterOwned: true,
			claimConditions: []interface{}{
				map[string]interface{}{"type": "InstanceTerminating", "status": "True"},
			},
			wantActive: true,
		},
		{
			name:               "NodeClaim condition status False: not active",
			nodeKarpenterOwned: true,
			claimConditions: []interface{}{
				map[string]interface{}{"type": "DisruptionReason", "status": "False"},
			},
			wantActive: false,
		},
		{
			name:               "NodeClaim absent but owner ref present: claimMissing=true",
			nodeKarpenterOwned: true,
			claimAbsent:        true,
			wantActive:         true,
			wantClaimMissing:   true,
		},
		{
			name:               "idle Karpenter node, no conditions: not active",
			nodeKarpenterOwned: true,
			wantActive:         false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-node",
					// Kubernetes requires a finalizer for DeletionTimestamp to be set.
					Finalizers: func() []string {
						if tc.nodeDeletionTS != nil {
							return []string{"test/finalizer"}
						}
						return nil
					}(),
					DeletionTimestamp: tc.nodeDeletionTS,
				},
				Spec: corev1.NodeSpec{Taints: tc.nodeTaints},
			}

			var objs []client.Object
			objs = append(objs, node)

			if tc.nodeKarpenterOwned && !tc.claimAbsent {
				claimObj := map[string]interface{}{
					"apiVersion": karpenterNodeClaimAPIVersion,
					"kind":       karpenterNodeClaimKind,
					"metadata": map[string]interface{}{
						"name": "test-claim",
					},
				}
				if len(tc.claimConditions) > 0 {
					claimObj["status"] = map[string]interface{}{
						"conditions": tc.claimConditions,
					}
				}
				claim := &unstructured.Unstructured{Object: claimObj}
				if tc.claimDeletionTS != nil {
					claim.SetDeletionTimestamp(tc.claimDeletionTS)
					claim.SetFinalizers([]string{"test/finalizer"})
				}
				objs = append(objs, claim)
			}

			if tc.nodeKarpenterOwned {
				node.OwnerReferences = []metav1.OwnerReference{{
					APIVersion: karpenterNodeClaimAPIVersion,
					Kind:       karpenterNodeClaimKind,
					Name:       "test-claim",
					UID:        types.UID("test-claim-uid"),
				}}
			}

			r, _ := newFakeReconciler(objs...)
			active, claimMissing, _, err := r.isKarpenterDisruptionActive(ctx, node)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(active).To(Equal(tc.wantActive), "active")
			g.Expect(claimMissing).To(Equal(tc.wantClaimMissing), "claimMissing")
		})
	}
}

// TestAssignRebootSlotAnnotationValueCheck verifies that assignRebootSlot
// takes ownership of and overwrites a Karpenter annotation that already exists
// but has a value other than "true" (e.g. "false"). Karpenter only honors the
// literal value "true"; any other value leaves the node unprotected.
func TestAssignRebootSlotAnnotationValueCheck(t *testing.T) {
	tests := []struct {
		name          string
		existingValue string // value already set on the Node annotation
		wantOwned     bool   // does the BootcNode get an ownership annotation?
	}{
		{
			name:      "annotation absent: operator takes ownership",
			wantOwned: true,
		},
		{
			name:          "annotation already true: operator does not take ownership",
			existingValue: "true",
			wantOwned:     false,
		},
		{
			name:          "annotation is false: operator overwrites and takes ownership",
			existingValue: "false",
			wantOwned:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			ctx := context.Background()

			nodeAnnotations := map[string]string{}
			if tc.existingValue != "" {
				nodeAnnotations[karpenterDoNotDisruptAnnotationKey] = tc.existingValue
				nodeAnnotations[karpenterDoNotRepairAnnotationKey] = tc.existingValue
			}

			k8sNode := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-node",
					Annotations: nodeAnnotations,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: karpenterNodeClaimAPIVersion,
						Kind:       karpenterNodeClaimKind,
						Name:       "test-claim",
						UID:        types.UID("test-claim-uid"),
					}},
				},
			}
			claim := &unstructured.Unstructured{Object: map[string]interface{}{
				"apiVersion": karpenterNodeClaimAPIVersion,
				"kind":       karpenterNodeClaimKind,
				"metadata":   map[string]interface{}{"name": "test-claim"},
			}}
			bn := testutil.NewNode("test-node", testImageDigestRefA)

			r, _ := newFakeReconciler(bn, k8sNode, claim)
			g.Expect(r.assignRebootSlot(ctx, bn, k8sNode, nil)).To(Succeed())

			// The Node annotation must always be "true" after assignment.
			var finalNode corev1.Node
			g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).To(Succeed())
			g.Expect(finalNode.Annotations[karpenterDoNotDisruptAnnotationKey]).To(Equal("true"))
			g.Expect(finalNode.Annotations[karpenterDoNotRepairAnnotationKey]).To(Equal("true"))

			// Ownership annotation on BootcNode.
			var finalBN bootcv1alpha1.BootcNode
			g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalBN)).To(Succeed())
			if tc.wantOwned {
				g.Expect(finalBN.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt))
				g.Expect(finalBN.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair))
			} else {
				g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt))
				g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair))
			}
		})
	}
}

// TestEnsureKarpenterStagingProtection verifies that ensureKarpenterStagingProtection
// applies do-not-repair and do-not-disrupt to a Karpenter-owned node in the
// Staging state, before any reboot slot is assigned.
func TestEnsureKarpenterStagingProtection(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	bn := testutil.NewNode("staging-node", testImageDigestRefA,
		testutil.WithBootedDigest(testDigestB),
		testutil.WithNodeCondition(bootcv1alpha1.NodeIdle, metav1.ConditionFalse, bootcv1alpha1.NodeReasonStaging),
	)

	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "staging-node",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: karpenterNodeClaimAPIVersion,
				Kind:       karpenterNodeClaimKind,
				Name:       "staging-claim",
				UID:        types.UID("staging-claim-uid"),
			}},
		},
	}
	claim := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": karpenterNodeClaimAPIVersion,
		"kind":       karpenterNodeClaimKind,
		"metadata":   map[string]interface{}{"name": "staging-claim"},
	}}

	r, _ := newFakeReconciler(bn, k8sNode, claim)
	g.Expect(r.ensureKarpenterStagingProtection(ctx, bn, k8sNode)).To(Succeed())

	// Node must have both Karpenter protection annotations.
	var finalNode corev1.Node
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "staging-node"}, &finalNode)).To(Succeed())
	g.Expect(finalNode.Annotations[karpenterDoNotRepairAnnotationKey]).To(Equal("true"))
	g.Expect(finalNode.Annotations[karpenterDoNotDisruptAnnotationKey]).To(Equal("true"))

	// NodeClaim must have both annotations.
	var finalClaim unstructured.Unstructured
	finalClaim.SetAPIVersion(karpenterNodeClaimAPIVersion)
	finalClaim.SetKind(karpenterNodeClaimKind)
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "staging-claim"}, &finalClaim)).To(Succeed())
	g.Expect(finalClaim.GetAnnotations()[karpenterDoNotRepairAnnotationKey]).To(Equal("true"))
	g.Expect(finalClaim.GetAnnotations()[karpenterDoNotDisruptAnnotationKey]).To(Equal("true"))

	// BootcNode must record ownership.
	var finalBN bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "staging-node"}, &finalBN)).To(Succeed())
	g.Expect(finalBN.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair))
	g.Expect(finalBN.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt))
	g.Expect(finalBN.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotRepair))
	g.Expect(finalBN.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt))

	// No reboot slot should have been assigned.
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationInRebootSlot))
}

// TestEnsureKarpenterStagingProtectionNonKarpenter verifies that
// ensureKarpenterStagingProtection is a no-op for non-Karpenter nodes.
func TestEnsureKarpenterStagingProtectionNonKarpenter(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	bn := testutil.NewNode("staging-node", testImageDigestRefA,
		testutil.WithBootedDigest(testDigestB),
		testutil.WithNodeCondition(bootcv1alpha1.NodeIdle, metav1.ConditionFalse, bootcv1alpha1.NodeReasonStaging),
	)
	// No Karpenter owner reference.
	k8sNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "staging-node"}}

	r, counting := newFakeReconciler(bn, k8sNode)
	g.Expect(r.ensureKarpenterStagingProtection(ctx, bn, k8sNode)).To(Succeed())

	// No patches to the Node should have been issued.
	g.Expect(counting.nodePatches).To(Equal(0))

	// BootcNode must be unchanged.
	var finalBN bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "staging-node"}, &finalBN)).To(Succeed())
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair))
}

// TestKarpenterStagingProtectionToSlotFreeLifecycle verifies the full
// staging-protection → slot-assignment → slot-free lifecycle for a
// Karpenter-owned node. Specifically it checks that:
//
//  1. Ownership annotations written by ensureKarpenterStagingProtection are not
//     lost when assignRebootSlot subsequently runs (idempotency).
//  2. freeRebootSlot removes the Karpenter annotations from both the Node and
//     the NodeClaim, and clears all ownership annotations from the BootcNode.
func TestKarpenterStagingProtectionToSlotFreeLifecycle(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	bn := testutil.NewNode("karp-node", testImageDigestRefA,
		testutil.WithBootedDigest(testDigestB),
		testutil.WithNodeCondition(bootcv1alpha1.NodeIdle, metav1.ConditionFalse, bootcv1alpha1.NodeReasonStaging),
	)
	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "karp-node",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: karpenterNodeClaimAPIVersion,
				Kind:       karpenterNodeClaimKind,
				Name:       "karp-claim",
				UID:        types.UID("karp-claim-uid"),
			}},
		},
	}
	claim := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": karpenterNodeClaimAPIVersion,
		"kind":       karpenterNodeClaimKind,
		"metadata":   map[string]interface{}{"name": "karp-claim"},
	}}

	r, _ := newFakeReconciler(bn, k8sNode, claim)

	// Step 1: staging protection is applied (simulates image pull in progress).
	g.Expect(r.ensureKarpenterStagingProtection(ctx, bn, k8sNode)).To(Succeed())

	// The BootcNode should carry all four ownership annotations.
	var afterStaging bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "karp-node"}, &afterStaging)).To(Succeed())
	g.Expect(afterStaging.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair))
	g.Expect(afterStaging.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt))
	g.Expect(afterStaging.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotRepair))
	g.Expect(afterStaging.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt))

	// Step 2: slot assignment runs (simulates image staged and slot available).
	// Re-fetch the node to get the current annotation state from the fake store.
	var currentNode corev1.Node
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "karp-node"}, &currentNode)).To(Succeed())
	g.Expect(r.assignRebootSlot(ctx, &afterStaging, &currentNode, nil)).To(Succeed())

	var afterSlot bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "karp-node"}, &afterSlot)).To(Succeed())
	// All four ownership annotations must still be present — assignRebootSlot
	// must not clear or duplicate them.
	g.Expect(afterSlot.Annotations).To(HaveKey(bootcv1alpha1.AnnotationInRebootSlot))
	g.Expect(afterSlot.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair))
	g.Expect(afterSlot.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt))
	g.Expect(afterSlot.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotRepair))
	g.Expect(afterSlot.Annotations).To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt))

	// Step 3: slot is freed (simulates successful reboot and Ready node).
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "karp-node"}, &currentNode)).To(Succeed())
	g.Expect(r.freeRebootSlot(ctx, &afterSlot, &currentNode)).To(Succeed())

	// All slot and ownership annotations must be gone from the BootcNode.
	var afterFree bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "karp-node"}, &afterFree)).To(Succeed())
	g.Expect(afterFree.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationInRebootSlot))
	g.Expect(afterFree.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair))
	g.Expect(afterFree.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt))
	g.Expect(afterFree.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotRepair))
	g.Expect(afterFree.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt))

	// Karpenter annotations must be removed from the Node.
	var finalNode corev1.Node
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "karp-node"}, &finalNode)).To(Succeed())
	g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenterDoNotRepairAnnotationKey))
	g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenterDoNotDisruptAnnotationKey))

	// Karpenter annotations must be removed from the NodeClaim.
	var finalClaim unstructured.Unstructured
	finalClaim.SetAPIVersion(karpenterNodeClaimAPIVersion)
	finalClaim.SetKind(karpenterNodeClaimKind)
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "karp-claim"}, &finalClaim)).To(Succeed())
	g.Expect(finalClaim.GetAnnotations()).NotTo(HaveKey(karpenterDoNotRepairAnnotationKey))
	g.Expect(finalClaim.GetAnnotations()).NotTo(HaveKey(karpenterDoNotDisruptAnnotationKey))
}

// TestDriveRolloutStagingProtection verifies that driveRollout applies Karpenter
// protection to staging nodes before they receive a reboot slot.
func TestDriveRolloutStagingProtection(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// A staging BootcNode (image pull in progress, not yet staged).
	bn := testutil.NewNode("staging-node", testImageDigestRefA,
		testutil.WithBootedDigest(testDigestB),
		testutil.WithNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionFalse,
			bootcv1alpha1.NodeReasonStaging,
		),
	)

	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "staging-node",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: karpenterNodeClaimAPIVersion,
				Kind:       karpenterNodeClaimKind,
				Name:       "staging-claim",
				UID:        types.UID("staging-claim-uid"),
			}},
		},
	}
	claim := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": karpenterNodeClaimAPIVersion,
		"kind":       karpenterNodeClaimKind,
		"metadata":   map[string]interface{}{"name": "staging-claim"},
		"status":     map[string]interface{}{"nodeName": "staging-node"},
	}}

	pool := testutil.NewPool("test-pool", testImageDigestRefA, testutil.WithWorkerSelector())
	pool.Status.TargetDigest = testDigestA

	r, _ := newFakeReconciler(bn, k8sNode, claim)
	ownedNodes := map[string]*bootcv1alpha1.BootcNode{"staging-node": bn}

	_, err := r.driveRollout(ctx, pool, ownedNodes)
	g.Expect(err).NotTo(HaveOccurred())

	// Node must have Karpenter protection applied.
	var finalNode corev1.Node
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "staging-node"}, &finalNode)).To(Succeed())
	g.Expect(finalNode.Annotations[karpenterDoNotRepairAnnotationKey]).To(Equal("true"),
		"do-not-repair must be set during staging")
	g.Expect(finalNode.Annotations[karpenterDoNotDisruptAnnotationKey]).To(Equal("true"),
		"do-not-disrupt must be set during staging")

	// The node must not have been assigned a reboot slot (it's still staging).
	var finalBN bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "staging-node"}, &finalBN)).To(Succeed())
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationInRebootSlot),
		"slot must not be assigned during staging")
}

// TestDriveRolloutDefersWhenKarpenterDisruptionActive verifies that driveRollout
// defers slot assignment when Karpenter has committed a disruption (not just a
// reboot).
func TestDriveRolloutDefersWhenKarpenterDisruptionActive(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// A staged BootcNode ready for a reboot slot.
	bn := testutil.NewNode("staged-node", testImageDigestRefA,
		testutil.WithBootedDigest(testDigestB),
		testutil.WithNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionFalse,
			bootcv1alpha1.NodeReasonStaged,
		),
	)

	// The Node has the disrupted taint (Karpenter voluntary disruption committed).
	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "staged-node",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: karpenterNodeClaimAPIVersion,
				Kind:       karpenterNodeClaimKind,
				Name:       "active-claim",
				UID:        types.UID("active-claim-uid"),
			}},
		},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{{
				Key:    karpenterDisruptedTaintKey,
				Effect: karpenterRebootingTaintEffect,
			}},
		},
	}
	claim := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": karpenterNodeClaimAPIVersion,
		"kind":       karpenterNodeClaimKind,
		"metadata":   map[string]interface{}{"name": "active-claim"},
		"status":     map[string]interface{}{"nodeName": "staged-node"},
	}}

	pool := testutil.NewPool("test-pool", testImageDigestRefA, testutil.WithWorkerSelector())
	pool.Status.TargetDigest = testDigestA

	r, _ := newFakeReconciler(bn, k8sNode, claim)
	ownedNodes := map[string]*bootcv1alpha1.BootcNode{"staged-node": bn}

	rs, err := r.driveRollout(ctx, pool, ownedNodes)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs).NotTo(BeNil())

	// The node must not have been assigned a reboot slot.
	var finalBN bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "staged-node"}, &finalBN)).To(Succeed())
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationInRebootSlot),
		"slot must not be assigned while Karpenter disruption is active")
}

// TestDriveRolloutDefersWhenNodeClaimAbsent verifies that driveRollout defers
// slot assignment when the Karpenter NodeClaim owner reference is present but
// the NodeClaim object is missing (404).
func TestDriveRolloutDefersWhenNodeClaimAbsent(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	bn := testutil.NewNode("staged-node", testImageDigestRefA,
		testutil.WithBootedDigest(testDigestB),
		testutil.WithNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionFalse,
			bootcv1alpha1.NodeReasonStaged,
		),
	)

	// The Node has a Karpenter owner reference, but the NodeClaim does not exist.
	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "staged-node",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: karpenterNodeClaimAPIVersion,
				Kind:       karpenterNodeClaimKind,
				Name:       "missing-claim",
				UID:        types.UID("missing-claim-uid"),
			}},
		},
	}
	// The NodeClaim is intentionally absent from the fake client.

	pool := testutil.NewPool("test-pool", testImageDigestRefA, testutil.WithWorkerSelector())
	pool.Status.TargetDigest = testDigestA

	r, _ := newFakeReconciler(bn, k8sNode)
	ownedNodes := map[string]*bootcv1alpha1.BootcNode{"staged-node": bn}

	rs, err := r.driveRollout(ctx, pool, ownedNodes)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs).NotTo(BeNil())

	// Slot must not have been assigned — the absent NodeClaim is unsafe.
	var finalBN bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "staged-node"}, &finalBN)).To(Succeed())
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationInRebootSlot),
		"slot must not be assigned when NodeClaim owner is missing")

	// A requeue should be scheduled to retry.
	g.Expect(rs.requeueAfter).To(BeNumerically(">", 0),
		"requeueAfter must be set to retry after missing NodeClaim")
}

// TestDriveRolloutKarpenterRequeueFallback verifies that driveRollout sets a
// periodic requeue when Karpenter-owned nodes are active (staging or staged),
// to ensure NodeClaim status changes are eventually observed even if the
// NodeClaim watch is not registered.
func TestDriveRolloutKarpenterRequeueFallback(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// A staged Karpenter-owned node. The BootcNode carries the ownership
	// annotation that staging protection would have set in a prior reconcile —
	// driveRollout uses these annotations to detect Karpenter-owned staged nodes
	// without an extra API call per node.
	bn := testutil.NewNode("staged-node", testImageDigestRefA,
		testutil.WithBootedDigest(testDigestB),
		testutil.WithNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionFalse,
			bootcv1alpha1.NodeReasonStaged,
		),
		testutil.WithNodeAnnotation(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair, "true"),
	)
	k8sNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "staged-node",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: karpenterNodeClaimAPIVersion,
				Kind:       karpenterNodeClaimKind,
				Name:       "test-claim",
				UID:        types.UID("test-claim-uid"),
			}},
		},
	}
	claim := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": karpenterNodeClaimAPIVersion,
		"kind":       karpenterNodeClaimKind,
		"metadata":   map[string]interface{}{"name": "test-claim"},
		"status":     map[string]interface{}{"nodeName": "staged-node"},
	}}

	pool := testutil.NewPool("test-pool", testImageDigestRefA, testutil.WithWorkerSelector())
	pool.Status.TargetDigest = testDigestA

	r, _ := newFakeReconciler(bn, k8sNode, claim)
	ownedNodes := map[string]*bootcv1alpha1.BootcNode{"staged-node": bn}

	rs, err := r.driveRollout(ctx, pool, ownedNodes)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(rs).NotTo(BeNil())

	// requeueAfter must be set to the fallback interval.
	g.Expect(rs.requeueAfter).To(Equal(karpenterMissingClaimRequeue),
		"requeueAfter must be set to the Karpenter fallback requeue interval")
}
