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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	karpenter "github.com/bootc-dev/bootc-operator/internal/karpenter"
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
//
// karpenter.NodeClaim embeds unstructured.Unstructured, which the fake client
// handles without a registered GVK — objects are stored and retrieved by the
// GVK embedded in the object itself. If karpenter.NodeClaim is ever changed to
// a typed (non-unstructured) struct it would need to be added to the scheme.
func newFakeReconciler(objs ...client.Object) (*BootcNodePoolReconciler, *patchCountingClient) {
	sc := runtime.NewScheme()
	_ = scheme.AddToScheme(sc)
	_ = bootcv1alpha1.AddToScheme(sc)

	base := fake.NewClientBuilder().
		WithScheme(sc).
		WithObjects(objs...).
		Build()

	counting := &patchCountingClient{Client: base}
	r := &BootcNodePoolReconciler{Client: counting, Scheme: sc}
	return r, counting
}

func TestKarpenterRebootActive(t *testing.T) {
	tests := []struct {
		name       string
		taints     []corev1.Taint
		conditions []testutil.KarpenterNodeClaimCondition
	}{
		{
			name: "NodeClaim rebooting condition",
			conditions: []testutil.KarpenterNodeClaimCondition{
				{Type: "Rebooting", Status: "True"},
			},
		},
		{
			name: "Node rebooting taint",
			taints: []corev1.Taint{{
				Key:    karpenter.RebootingTaintKey,
				Effect: karpenter.RebootingTaintEffect,
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			claim := testutil.NewKarpenterNodeClaim("claim",
				testutil.WithKarpenterNodeClaimConditions(tt.conditions...))
			node := testutil.NewK8sNode("node", nil,
				testutil.WithOwnerReferences([]metav1.OwnerReference{{
					APIVersion: karpenter.NodeClaimAPIVersion,
					Kind:       karpenter.NodeClaimKind,
					Name:       "claim",
					UID:        types.UID("claim-uid"),
				}}),
				testutil.WithTaints(tt.taints))

			r, _ := newFakeReconciler(node, claim)
			kds, err := r.isKarpenterDisruptionActive(context.Background(), node)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(kds.active).To(BeTrue())
		})
	}
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
				karpenter.DoNotRepairAnnotationKey:  "true",
				karpenter.DoNotDisruptAnnotationKey: "true",
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
				karpenter.DoNotRepairAnnotationKey:  "true",
				karpenter.DoNotDisruptAnnotationKey: "true",
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
				karpenter.DoNotRepairAnnotationKey:  "true",
				karpenter.DoNotDisruptAnnotationKey: "true",
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
				karpenter.DoNotRepairAnnotationKey:  "true",
				karpenter.DoNotDisruptAnnotationKey: "true",
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
			bn := testutil.NewBootcNode("test-node", testImageDigestRefA)
			bn.Annotations = bnAnnotations

			k8sNode := testutil.NewK8sNode("test-node", nil,
				testutil.WithK8sNodeAnnotations(tc.nodeAnnotations),
				testutil.WithUnschedulable(tc.nodeCordoned))

			r, counting := newFakeReconciler(bn, k8sNode)

			err := r.restoreNodeState(ctx, bn, k8sNode, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(counting.nodePatches).To(Equal(tc.wantNodePatches), "node patch count")

			var finalNode corev1.Node
			g.Expect(counting.Client.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).
				To(Succeed())
			g.Expect(finalNode.Spec.Unschedulable).To(Equal(tc.wantCordoned), "cordon state")
			if tc.wantRepairPresent {
				g.Expect(finalNode.Annotations).To(HaveKey(karpenter.DoNotRepairAnnotationKey))
			} else {
				g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenter.DoNotRepairAnnotationKey))
			}
			if tc.wantDisruptPresent {
				g.Expect(finalNode.Annotations).To(HaveKey(karpenter.DoNotDisruptAnnotationKey))
			} else {
				g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenter.DoNotDisruptAnnotationKey))
			}
		})
	}
}

// TestAssignRebootSlotNodePatchCount verifies that assignRebootSlot issues at
// most one patch to the corev1.Node regardless of Karpenter ownership and
// prior cordon state.
func TestAssignRebootSlotNodePatchCount(t *testing.T) {
	karpenterOwnerRef := metav1.OwnerReference{
		APIVersion: karpenter.NodeClaimAPIVersion,
		Kind:       karpenter.NodeClaimKind,
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
				karpenter.DoNotRepairAnnotationKey:  "true",
				karpenter.DoNotDisruptAnnotationKey: "true",
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

			bn := testutil.NewBootcNode("test-node", testImageDigestRefA)

			k8sNode := testutil.NewK8sNode("test-node", nil,
				testutil.WithK8sNodeAnnotations(tc.preExistAnnotations),
				testutil.WithUnschedulable(tc.preCordoned))

			objects := []client.Object{bn, k8sNode}

			if tc.karpenterOwned {
				k8sNode.OwnerReferences = []metav1.OwnerReference{karpenterOwnerRef}

				claim := testutil.NewKarpenterNodeClaim(karpenterOwnerRef.Name)
				objects = append(objects, claim)
			}

			r, counting := newFakeReconciler(objects...)

			err := r.assignRebootSlot(ctx, bn, k8sNode, nil)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(counting.nodePatches).To(Equal(tc.wantNodePatches), "node patch count")

			var finalNode corev1.Node
			g.Expect(counting.Client.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).
				To(Succeed())
			g.Expect(finalNode.Spec.Unschedulable).To(Equal(tc.wantCordoned), "cordon state")
			if tc.wantRepairPresent {
				g.Expect(finalNode.Annotations).To(HaveKey(karpenter.DoNotRepairAnnotationKey))
			} else {
				g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenter.DoNotRepairAnnotationKey))
			}
			if tc.wantDisruptPresent {
				g.Expect(finalNode.Annotations).To(HaveKey(karpenter.DoNotDisruptAnnotationKey))
			} else {
				g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenter.DoNotDisruptAnnotationKey))
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
			karpenter.DoNotDisruptAnnotationKey,
			bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt,
		},
		{
			karpenter.DoNotRepairAnnotationKey,
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

				var claim *karpenter.NodeClaim
				if tc.preExistingAnnotation {
					claim = testutil.NewKarpenterNodeClaim("test-claim",
						testutil.WithKarpenterNodeClaimAnnotation(ann.claimKey, "true"))
				} else {
					claim = testutil.NewKarpenterNodeClaim("test-claim")
				}

				node := testutil.NewK8sNode("test-node", nil,
					testutil.WithOwnerReferences([]metav1.OwnerReference{{
						APIVersion: karpenter.NodeClaimAPIVersion,
						Kind:       karpenter.NodeClaimKind,
						Name:       "test-claim",
						UID:        types.UID("test-claim-uid"),
					}}))
				bn := testutil.NewBootcNode("test-node", testImageDigestRefA)

				r, _ := newFakeReconciler(bn, node, claim)
				g.Expect(r.assignRebootSlot(ctx, bn, node, nil)).To(Succeed())

				duringClaim := testutil.NewKarpenterNodeClaim("test-claim")
				g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-claim"}, duringClaim)).
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

				g.Expect(r.freeRebootSlot(ctx, &duringBN, node, nil)).To(Succeed())

				afterClaim := testutil.NewKarpenterNodeClaim("test-claim")
				g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-claim"}, afterClaim)).
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
//     calls freeRebootSlot → restoreNodeState, which uses the NodeClaim lookup
//     to remove the annotations it added.
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
	bn := testutil.NewBootcNode("test-node", testImageDigestRefA)
	bn.Annotations = map[string]string{
		bootcv1alpha1.AnnotationInRebootSlot:                       "",
		bootcv1alpha1.AnnotationWasCordoned:                        "false",
		bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotRepair:  "true",
		bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt: "true",
	}

	// Node has an owner reference pointing to a NodeClaim that no longer exists.
	k8sNode := testutil.NewK8sNode("test-node", nil,
		testutil.WithOwnerReferences([]metav1.OwnerReference{{
			APIVersion: karpenter.NodeClaimAPIVersion,
			Kind:       karpenter.NodeClaimKind,
			Name:       "gone-claim",
			UID:        types.UID("gone-uid"),
		}}),
		testutil.WithUnschedulable(true))

	// The NodeClaim is intentionally absent from the fake client.
	r, _ := newFakeReconciler(bn, k8sNode)
	g.Expect(r.freeRebootSlot(ctx, bn, k8sNode, nil)).To(Succeed())

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
	bn := testutil.NewBootcNode("staged-node", testImageDigestRefA,
		testutil.WithBootcNodeBootedDigest(testDigestB),
		testutil.WithBootcNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionFalse,
			bootcv1alpha1.NodeReasonStaged,
		),
	)

	// The corresponding K8s Node is owned by a NodeClaim that is currently rebooting.
	k8sNode := testutil.NewK8sNode("staged-node", nil,
		testutil.WithOwnerReferences([]metav1.OwnerReference{{
			APIVersion: karpenter.NodeClaimAPIVersion,
			Kind:       karpenter.NodeClaimKind,
			Name:       "active-claim",
			UID:        types.UID("active-claim-uid"),
		}}))

	// NodeClaim with Rebooting=True.
	claim := testutil.NewKarpenterNodeClaim("active-claim",
		testutil.WithKarpenterNodeClaimNodeName("staged-node"),
		testutil.WithKarpenterNodeClaimConditions(testutil.KarpenterNodeClaimCondition{
			Type: "Rebooting", Status: "True",
		}))

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
				karpenter.DoNotRepairAnnotationKey:  "true",
				karpenter.DoNotDisruptAnnotationKey: "true",
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
				karpenter.DoNotRepairAnnotationKey:  "true",
				karpenter.DoNotDisruptAnnotationKey: "true",
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
			bn := testutil.NewBootcNode("test-node", testImageDigestRefA)
			bn.Annotations = bnAnnotations

			k8sNode := testutil.NewK8sNode("test-node", nil,
				testutil.WithK8sNodeAnnotations(tc.nodeAnnotations),
				testutil.WithUnschedulable(tc.nodeCordoned))

			r, counting := newFakeReconciler(bn, k8sNode)

			err := r.freeRebootSlot(ctx, bn, k8sNode, nil)
			g.Expect(err).NotTo(HaveOccurred())

			g.Expect(counting.nodePatches).To(Equal(tc.wantNodePatches),
				"unexpected number of Node patches")

			// Verify the final node state from the fake store.
			var finalNode corev1.Node
			g.Expect(counting.Client.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).
				To(Succeed())
			g.Expect(finalNode.Spec.Unschedulable).To(Equal(tc.wantCordoned), "cordon state")
			if tc.ownsRepair {
				g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenter.DoNotRepairAnnotationKey))
			}
			if tc.ownsDisrupt {
				g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenter.DoNotDisruptAnnotationKey))
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
				Key:    karpenter.RebootingTaintKey,
				Effect: karpenter.RebootingTaintEffect,
			}},
			wantActive: true,
		},
		{
			name:               "disrupted taint: active",
			nodeKarpenterOwned: true,
			nodeTaints: []corev1.Taint{{
				Key:    karpenter.DisruptedTaintKey,
				Effect: karpenter.RebootingTaintEffect,
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

			node := testutil.NewK8sNode("test-node", nil,
				testutil.WithTaints(tc.nodeTaints))
			// Kubernetes requires a finalizer for DeletionTimestamp to be set.
			if tc.nodeDeletionTS != nil {
				node.Finalizers = []string{"test/finalizer"}
			}
			node.DeletionTimestamp = tc.nodeDeletionTS

			var objs []client.Object
			objs = append(objs, node)

			if tc.nodeKarpenterOwned && !tc.claimAbsent {
				claim := testutil.NewKarpenterNodeClaim("test-claim")
				if len(tc.claimConditions) > 0 {
					claim.Object["status"] = map[string]interface{}{
						"conditions": tc.claimConditions,
					}
				}
				if tc.claimDeletionTS != nil {
					claim.SetDeletionTimestamp(tc.claimDeletionTS)
					claim.SetFinalizers([]string{"test/finalizer"})
				}
				objs = append(objs, claim)
			}

			if tc.nodeKarpenterOwned {
				node.OwnerReferences = []metav1.OwnerReference{{
					APIVersion: karpenter.NodeClaimAPIVersion,
					Kind:       karpenter.NodeClaimKind,
					Name:       "test-claim",
					UID:        types.UID("test-claim-uid"),
				}}
			}

			r, _ := newFakeReconciler(objs...)
			kds, err := r.isKarpenterDisruptionActive(ctx, node)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(kds.active).To(Equal(tc.wantActive), "active")
			g.Expect(kds.claimMissing).To(Equal(tc.wantClaimMissing), "claimMissing")
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
				nodeAnnotations[karpenter.DoNotDisruptAnnotationKey] = tc.existingValue
				nodeAnnotations[karpenter.DoNotRepairAnnotationKey] = tc.existingValue
			}

			k8sNode := testutil.NewK8sNode("test-node", nil,
				testutil.WithOwnerReferences([]metav1.OwnerReference{{
					APIVersion: karpenter.NodeClaimAPIVersion,
					Kind:       karpenter.NodeClaimKind,
					Name:       "test-claim",
					UID:        types.UID("test-claim-uid"),
				}}),
				testutil.WithK8sNodeAnnotations(nodeAnnotations))
			claim := testutil.NewKarpenterNodeClaim("test-claim")
			bn := testutil.NewBootcNode("test-node", testImageDigestRefA)

			r, _ := newFakeReconciler(bn, k8sNode, claim)
			g.Expect(r.assignRebootSlot(ctx, bn, k8sNode, nil)).To(Succeed())

			// The Node annotation must always be "true" after assignment.
			var finalNode corev1.Node
			g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalNode)).To(Succeed())
			g.Expect(finalNode.Annotations[karpenter.DoNotDisruptAnnotationKey]).To(Equal("true"))
			g.Expect(finalNode.Annotations[karpenter.DoNotRepairAnnotationKey]).To(Equal("true"))

			// Ownership annotation on BootcNode.
			var finalBN bootcv1alpha1.BootcNode
			g.Expect(r.Get(ctx, client.ObjectKey{Name: "test-node"}, &finalBN)).To(Succeed())
			if tc.wantOwned {
				g.Expect(finalBN.Annotations).
					To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt))
				g.Expect(finalBN.Annotations).
					To(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair))
			} else {
				g.Expect(finalBN.Annotations).
					NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt))
				g.Expect(finalBN.Annotations).
					NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair))
			}
		})
	}
}

// TestDriveRolloutDoesNotProtectStaging verifies that driveRollout does not
// apply Karpenter protection until a reboot slot is assigned.
func TestDriveRolloutDoesNotProtectStaging(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// A staging BootcNode (image pull in progress, not yet staged).
	bn := testutil.NewBootcNode("staging-node", testImageDigestRefA,
		testutil.WithBootcNodeBootedDigest(testDigestB),
		testutil.WithBootcNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionFalse,
			bootcv1alpha1.NodeReasonStaging,
		),
	)

	k8sNode := testutil.NewK8sNode("staging-node", nil,
		testutil.WithOwnerReferences([]metav1.OwnerReference{{
			APIVersion: karpenter.NodeClaimAPIVersion,
			Kind:       karpenter.NodeClaimKind,
			Name:       "staging-claim",
			UID:        types.UID("staging-claim-uid"),
		}}))
	claim := testutil.NewKarpenterNodeClaim("staging-claim",
		testutil.WithKarpenterNodeClaimNodeName("staging-node"))

	pool := testutil.NewPool("test-pool", testImageDigestRefA, testutil.WithWorkerSelector())
	pool.Status.TargetDigest = testDigestA

	r, _ := newFakeReconciler(bn, k8sNode, claim)
	ownedNodes := map[string]*bootcv1alpha1.BootcNode{"staging-node": bn}

	_, err := r.driveRollout(ctx, pool, ownedNodes)
	g.Expect(err).NotTo(HaveOccurred())

	// A staging node must not have Karpenter protection applied.
	var finalNode corev1.Node
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "staging-node"}, &finalNode)).To(Succeed())
	g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenter.DoNotRepairAnnotationKey),
		"do-not-repair must not be set during staging")
	g.Expect(finalNode.Annotations).NotTo(HaveKey(karpenter.DoNotDisruptAnnotationKey),
		"do-not-disrupt must not be set during staging")

	finalClaim := testutil.NewKarpenterNodeClaim("staging-claim")
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "staging-claim"}, finalClaim)).To(Succeed())
	g.Expect(finalClaim.GetAnnotations()).NotTo(HaveKey(karpenter.DoNotRepairAnnotationKey),
		"NodeClaim do-not-repair must not be set during staging")
	g.Expect(finalClaim.GetAnnotations()).NotTo(HaveKey(karpenter.DoNotDisruptAnnotationKey),
		"NodeClaim do-not-disrupt must not be set during staging")

	// The node must not have been assigned a reboot slot (it's still staging).
	var finalBN bootcv1alpha1.BootcNode
	g.Expect(r.Get(ctx, client.ObjectKey{Name: "staging-node"}, &finalBN)).To(Succeed())
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationInRebootSlot),
		"slot must not be assigned during staging")
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair))
	g.Expect(finalBN.Annotations).NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt))
	g.Expect(finalBN.Annotations).
		NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotRepair))
	g.Expect(finalBN.Annotations).
		NotTo(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt))
}

// TestDriveRolloutDefersWhenKarpenterDisruptionActive verifies that driveRollout
// defers slot assignment when Karpenter has committed a disruption (not just a
// reboot).
func TestDriveRolloutDefersWhenKarpenterDisruptionActive(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// A staged BootcNode ready for a reboot slot.
	bn := testutil.NewBootcNode("staged-node", testImageDigestRefA,
		testutil.WithBootcNodeBootedDigest(testDigestB),
		testutil.WithBootcNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionFalse,
			bootcv1alpha1.NodeReasonStaged,
		),
	)

	// The Node has the disrupted taint (Karpenter voluntary disruption committed).
	k8sNode := testutil.NewK8sNode("staged-node", nil,
		testutil.WithOwnerReferences([]metav1.OwnerReference{{
			APIVersion: karpenter.NodeClaimAPIVersion,
			Kind:       karpenter.NodeClaimKind,
			Name:       "active-claim",
			UID:        types.UID("active-claim-uid"),
		}}),
		testutil.WithTaints([]corev1.Taint{{
			Key:    karpenter.DisruptedTaintKey,
			Effect: karpenter.RebootingTaintEffect,
		}}))
	claim := testutil.NewKarpenterNodeClaim("active-claim",
		testutil.WithKarpenterNodeClaimNodeName("staged-node"))

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

	bn := testutil.NewBootcNode("staged-node", testImageDigestRefA,
		testutil.WithBootcNodeBootedDigest(testDigestB),
		testutil.WithBootcNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionFalse,
			bootcv1alpha1.NodeReasonStaged,
		),
	)

	// The Node has a Karpenter owner reference, but the NodeClaim does not exist.
	k8sNode := testutil.NewK8sNode("staged-node", nil,
		testutil.WithOwnerReferences([]metav1.OwnerReference{{
			APIVersion: karpenter.NodeClaimAPIVersion,
			Kind:       karpenter.NodeClaimKind,
			Name:       "missing-claim",
			UID:        types.UID("missing-claim-uid"),
		}}))
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

	// A staged Karpenter-owned node. It has no protection annotations until the
	// reboot slot is assigned.
	bn := testutil.NewBootcNode("staged-node", testImageDigestRefA,
		testutil.WithBootcNodeBootedDigest(testDigestB),
		testutil.WithBootcNodeCondition(
			bootcv1alpha1.NodeIdle,
			metav1.ConditionFalse,
			bootcv1alpha1.NodeReasonStaged,
		),
	)
	k8sNode := testutil.NewK8sNode("staged-node", nil,
		testutil.WithOwnerReferences([]metav1.OwnerReference{{
			APIVersion: karpenter.NodeClaimAPIVersion,
			Kind:       karpenter.NodeClaimKind,
			Name:       "test-claim",
			UID:        types.UID("test-claim-uid"),
		}}))
	claim := testutil.NewKarpenterNodeClaim("test-claim",
		testutil.WithKarpenterNodeClaimNodeName("staged-node"))

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

func TestOwnedAnnotationHelpers(t *testing.T) {
	g := NewWithT(t)
	owner := &bootcv1alpha1.BootcNode{}
	target := &corev1.Node{}
	const (
		ownerKey  = "operator.example.com/owns-annotation"
		targetKey = "example.com/annotation"
	)

	target.SetAnnotations(map[string]string{targetKey: "false"})
	g.Expect(ensureOwnedAnnotation(owner, target, ownerKey, targetKey, "true")).To(BeTrue())
	g.Expect(owner.GetAnnotations()).To(HaveKeyWithValue(ownerKey, "true"))
	g.Expect(target.GetAnnotations()).To(HaveKeyWithValue(targetKey, "true"))

	// Repeating the operation does not change either object.
	g.Expect(ensureOwnedAnnotation(owner, target, ownerKey, targetKey, "true")).To(BeFalse())

	g.Expect(removeOwnedAnnotation(owner, target, ownerKey, targetKey)).To(BeTrue())
	g.Expect(target.GetAnnotations()).NotTo(HaveKey(targetKey))
	g.Expect(removeOwnedAnnotation(owner, target, ownerKey, targetKey)).To(BeFalse())
}

func TestRemoveOwnedAnnotationRequiresOwnership(t *testing.T) {
	g := NewWithT(t)
	owner := &bootcv1alpha1.BootcNode{}
	target := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		"example.com/annotation": "true",
	}}}

	g.Expect(removeOwnedAnnotation(
		owner,
		target,
		"operator.example.com/owns-annotation",
		"example.com/annotation",
	)).To(BeFalse())
	g.Expect(target.GetAnnotations()).To(HaveKey("example.com/annotation"))
}

func TestRemoveOwnedAnnotationRemovesExternallyChangedValue(t *testing.T) {
	g := NewWithT(t)
	owner := &bootcv1alpha1.BootcNode{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		"operator.example.com/owns-annotation": "true",
	}}}
	target := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		"example.com/annotation": "external-value",
	}}}

	g.Expect(removeOwnedAnnotation(
		owner,
		target,
		"operator.example.com/owns-annotation",
		"example.com/annotation",
	)).To(BeTrue())
	g.Expect(target.GetAnnotations()).NotTo(HaveKey("example.com/annotation"))
}
