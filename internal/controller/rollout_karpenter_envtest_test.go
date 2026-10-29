// SPDX-License-Identifier: Apache-2.0

package controller

// TestKarpenterAnnotations is a table-driven integration test that covers
// Karpenter NodeClaim annotation management across all distinct scenarios. Each
// row exercises a different combination of node ownership, pre-existing
// annotations, and cordon state.

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	karpenter "github.com/bootc-dev/bootc-operator/internal/karpenter"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

func TestKarpenterAnnotations(t *testing.T) {
	type expectedAnnotations struct {
		repair  bool
		disrupt bool
	}

	type lifecycle struct {
		during expectedAnnotations
		after  expectedAnnotations
	}

	tests := []struct {
		// name is the subtest name and is used to derive unique resource names.
		name string

		// Node setup.
		hasNodeClaim           bool              // whether to attach a NodeClaim owner reference
		preExistingAnnotations map[string]string // annotations to set on the Node before the rollout
		preCordon              bool              // whether the Node starts cordoned

		bootcNode expectedAnnotations
		node      lifecycle
		nodeClaim lifecycle

		// Expected Node cordon state after the slot is freed.
		wantCordonedAfter bool
	}{
		// A plain NodeClaim-owned node with no pre-existing Karpenter state.
		// The operator adds both annotations on slot assignment, records
		// ownership, and removes them when the slot is freed.
		{
			name:         "nodeclaim owned, no pre-existing annotations",
			hasNodeClaim: true,
			bootcNode:    expectedAnnotations{repair: true, disrupt: true},
			node: lifecycle{
				during: expectedAnnotations{repair: true, disrupt: true},
			},
			nodeClaim: lifecycle{
				during: expectedAnnotations{repair: true, disrupt: true},
			},
			wantCordonedAfter: false,
		},
		// A node with both Karpenter annotations already set by an external
		// actor and no NodeClaim owner reference. The operator must not claim
		// ownership of either annotation and must leave both untouched
		// throughout, including after the slot is freed.
		{
			name:         "no nodeclaim, both annotations pre-existing",
			hasNodeClaim: false,
			preExistingAnnotations: map[string]string{
				karpenter.DoNotRepairAnnotationKey:  "true",
				karpenter.DoNotDisruptAnnotationKey: "true",
			},
			bootcNode: expectedAnnotations{},
			node: lifecycle{
				during: expectedAnnotations{repair: true, disrupt: true},
				after:  expectedAnnotations{repair: true, disrupt: true},
			},
			wantCordonedAfter: false,
		},
		// A NodeClaim-owned node that was already cordoned before the rollout.
		// The operator adds both Karpenter annotations and removes them on slot
		// free, but the pre-existing cordon must be preserved throughout.
		{
			name:         "nodeclaim owned, pre-cordoned",
			hasNodeClaim: true,
			preCordon:    true,
			bootcNode:    expectedAnnotations{repair: true, disrupt: true},
			node: lifecycle{
				during: expectedAnnotations{repair: true, disrupt: true},
			},
			nodeClaim: lifecycle{
				during: expectedAnnotations{repair: true, disrupt: true},
			},
			wantCordonedAfter: true, // pre-existing cordon preserved
		},
		// A NodeClaim-owned node where only do-not-repair is pre-existing.
		// The operator must not claim do-not-repair (already set externally)
		// but must add, own, and later remove do-not-disrupt independently.
		// This verifies that per-annotation ownership tracking is applied
		// independently rather than as an all-or-nothing decision.
		{
			name:         "nodeclaim owned, only do-not-repair pre-existing",
			hasNodeClaim: true,
			preExistingAnnotations: map[string]string{
				karpenter.DoNotRepairAnnotationKey: "true",
			},
			// The pre-existing Node annotation is not independently present on
			// the NodeClaim, so the claim annotation is operator-owned.
			bootcNode: expectedAnnotations{repair: false, disrupt: true},
			node: lifecycle{
				during: expectedAnnotations{repair: true, disrupt: true},
				after:  expectedAnnotations{repair: true},
			},
			nodeClaim: lifecycle{
				during: expectedAnnotations{repair: true, disrupt: true},
			},
			wantCordonedAfter: false,
		},
	}

	for i, tc := range tests {
		tc := tc
		// Derive unique, collision-free resource names from the subtest index.
		nodeName := fmt.Sprintf("karp-ann-%d-node", i)
		poolName := fmt.Sprintf("karp-ann-%d-pool", i)

		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			g.SetDefaultEventuallyTimeout(pollTimeout)
			g.SetDefaultEventuallyPollingInterval(pollInterval)
			ctx := context.Background()

			// Build and create the K8s Node.
			node := testutil.NewK8sNode(nodeName, testutil.WorkerLabels())
			if tc.hasNodeClaim {
				claimName := nodeName + "-claim"
				claim := testutil.NewKarpenterNodeClaim(claimName)
				g.Expect(k8sClient.Create(ctx, claim)).To(Succeed())
				t.Cleanup(func() { _ = k8sClient.Delete(ctx, claim) })
				node.OwnerReferences = []metav1.OwnerReference{{
					APIVersion: karpenter.NodeClaimAPIVersion,
					Kind:       karpenter.NodeClaimKind,
					Name:       claimName,
					UID:        types.UID(nodeName + "-claim-uid"),
				}}
			}
			if len(tc.preExistingAnnotations) > 0 {
				node.Annotations = tc.preExistingAnnotations
			}
			if tc.preCordon {
				node.Spec.Unschedulable = true
			}
			g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
			t.Cleanup(func() { _ = k8sClient.Delete(ctx, node) })

			// Create pool and wait for BootcNode.
			pool := testutil.NewPool(poolName, testImageDigestRefB,
				testutil.WithWorkerSelector(),
				testutil.WithMaxUnavailable(intstr.FromInt32(1)),
			)
			g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
			t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

			g.Eventually(func() error {
				return k8sClient.Get(
					ctx,
					client.ObjectKey{Name: nodeName},
					&bootcv1alpha1.BootcNode{},
				)
			}).Should(Succeed())

			simulateDaemonStatus(g, ctx, nodeName, testDigestA, bootcv1alpha1.NodeReasonStaged)

			// Wait for slot assignment and drain completion.
			g.Eventually(func() (*bootcv1alpha1.BootcNode, error) {
				var bn bootcv1alpha1.BootcNode
				err := k8sClient.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
				return &bn, err
			}).Should(And(
				HaveField("Annotations", HaveKey(bootcv1alpha1.AnnotationInRebootSlot)),
				HaveField("Spec.DesiredImageState", Equal(bootcv1alpha1.DesiredImageStateBooted)),
			), "BootcNode should have reboot slot and desiredImageState Booted")

			// Assert ownership annotations on BootcNode.
			g.Eventually(func() (map[string]string, error) {
				var bn bootcv1alpha1.BootcNode
				err := k8sClient.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
				return bn.Annotations, err
			}).Should(matchOwnership(tc.bootcNode.repair, tc.bootcNode.disrupt),
				"BootcNode ownership annotations during slot")

			// Assert Karpenter annotations on Node.
			g.Eventually(func() (map[string]string, error) {
				var n corev1.Node
				err := k8sClient.Get(ctx, client.ObjectKey{Name: nodeName}, &n)
				return n.Annotations, err
			}).Should(matchKarpenterAnnotations(tc.node.during.repair, tc.node.during.disrupt),
				"Node Karpenter annotations during slot")

			if tc.hasNodeClaim {
				g.Eventually(func() (map[string]string, error) {
					var claim karpenter.NodeClaim
					claim.SetGroupVersionKind(
						schema.GroupVersionKind{
							Group:   "karpenter.sh",
							Version: "v1",
							Kind:    "NodeClaim",
						},
					)
					err := k8sClient.Get(ctx, client.ObjectKey{Name: nodeName + "-claim"}, &claim)
					return claim.GetAnnotations(), err
				}).Should(matchKarpenterAnnotations(tc.nodeClaim.during.repair, tc.nodeClaim.during.disrupt),
					"NodeClaim Karpenter annotations during slot")
			}

			// Simulate successful reboot and node becoming Ready.
			simulateDaemonStatus(g, ctx, nodeName, testDigestB, bootcv1alpha1.NodeReasonIdle)
			setNodeReady(g, ctx, nodeName)

			// Wait for slot to be freed.
			g.Eventually(func() (map[string]string, error) {
				var bn bootcv1alpha1.BootcNode
				err := k8sClient.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
				return bn.Annotations, err
			}).Should(And(
				Not(HaveKey(bootcv1alpha1.AnnotationInRebootSlot)),
				Not(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair)),
				Not(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt)),
			), "BootcNode should have no slot or ownership annotations after slot free")

			// Assert final Node annotation state.
			g.Eventually(func() (map[string]string, error) {
				var n corev1.Node
				err := k8sClient.Get(ctx, client.ObjectKey{Name: nodeName}, &n)
				return n.Annotations, err
			}).Should(matchKarpenterAnnotations(tc.node.after.repair, tc.node.after.disrupt),
				"Node Karpenter annotations after slot free")

			if tc.hasNodeClaim {
				g.Eventually(func() (map[string]string, error) {
					var claim karpenter.NodeClaim
					claim.SetGroupVersionKind(
						schema.GroupVersionKind{
							Group:   "karpenter.sh",
							Version: "v1",
							Kind:    "NodeClaim",
						},
					)
					err := k8sClient.Get(ctx, client.ObjectKey{Name: nodeName + "-claim"}, &claim)
					return claim.GetAnnotations(), err
				}).Should(matchKarpenterAnnotations(tc.nodeClaim.after.repair, tc.nodeClaim.after.disrupt),
					"NodeClaim Karpenter annotations after slot free")
			}

			// Assert final cordon state.
			g.Eventually(func() (bool, error) {
				var n corev1.Node
				err := k8sClient.Get(ctx, client.ObjectKey{Name: nodeName}, &n)
				return n.Spec.Unschedulable, err
			}).Should(Equal(tc.wantCordonedAfter), "Node cordon state after slot free")
		})
	}
}

// matchOwnership returns a Gomega matcher for the BootcNode annotation map
// that asserts the presence or absence of the two Karpenter ownership
// annotations independently.
func matchOwnership(wantRepair, wantDisrupt bool) OmegaMatcher {
	repair := Not(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair))
	if wantRepair {
		repair = HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair)
	}
	disrupt := Not(HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt))
	if wantDisrupt {
		disrupt = HaveKey(bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt)
	}
	return And(repair, disrupt)
}

// matchKarpenterAnnotations returns a Gomega matcher for the Node annotation
// map that asserts the presence or absence of the two Karpenter annotations
// independently.
func matchKarpenterAnnotations(wantRepair, wantDisrupt bool) OmegaMatcher {
	repair := Not(HaveKey(karpenter.DoNotRepairAnnotationKey))
	if wantRepair {
		repair = HaveKey(karpenter.DoNotRepairAnnotationKey)
	}
	disrupt := Not(HaveKey(karpenter.DoNotDisruptAnnotationKey))
	if wantDisrupt {
		disrupt = HaveKey(karpenter.DoNotDisruptAnnotationKey)
	}
	return And(repair, disrupt)
}
