// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
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

func TestBuildRolloutState(t *testing.T) {
	const (
		desiredImage = testImageDigestRefA
		otherDigest  = testDigestB
	)

	g := NewWithT(t)

	// Classification of each state is tested in TestClassifyNode. This
	// test focuses more on aggregation: bucketing, slot counting, and
	// nodeCount.
	nodes := map[string]*bootcv1alpha1.BootcNode{
		"uptodate": testutil.NewNode(
			"uptodate",
			desiredImage,
			testutil.WithBootedDigest(testDigestA),
			testutil.WithNodeCondition(
				bootcv1alpha1.NodeIdle,
				metav1.ConditionTrue,
				bootcv1alpha1.NodeReasonIdle,
			),
		),
		"pending": testutil.NewNode(
			"pending",
			desiredImage,
			testutil.WithBootedDigest(otherDigest),
			testutil.WithNodeCondition(
				bootcv1alpha1.NodeIdle,
				metav1.ConditionTrue,
				bootcv1alpha1.NodeReasonIdle,
			),
		),
		"staged": testutil.NewNode(
			"staged",
			desiredImage,
			testutil.WithBootedDigest(otherDigest),
			testutil.WithNodeCondition(
				bootcv1alpha1.NodeIdle,
				metav1.ConditionFalse,
				bootcv1alpha1.NodeReasonStaged,
			),
		),
		"rebooting-1": testutil.NewNode(
			"rebooting-1",
			desiredImage,
			testutil.WithBootedDigest(otherDigest),
			testutil.WithNodeCondition(
				bootcv1alpha1.NodeIdle,
				metav1.ConditionFalse,
				bootcv1alpha1.NodeReasonRebooting,
			),
			testutil.WithNodeAnnotation(bootcv1alpha1.AnnotationInRebootSlot, ""),
		),
		"rebooting-2": testutil.NewNode(
			"rebooting-2",
			desiredImage,
			testutil.WithBootedDigest(otherDigest),
			testutil.WithNodeCondition(
				bootcv1alpha1.NodeIdle,
				metav1.ConditionFalse,
				bootcv1alpha1.NodeReasonRebooting,
			),
			testutil.WithNodeAnnotation(bootcv1alpha1.AnnotationInRebootSlot, ""),
		),
	}

	rs := buildRolloutState(logr.Discard(), nodes)

	g.Expect(rs.upToDate).To(HaveLen(1))
	g.Expect(rs.pending).To(HaveLen(1))
	g.Expect(rs.staged).To(HaveLen(1))
	g.Expect(rs.rebooting).To(HaveLen(2))
	g.Expect(rs.occupiedSlots).To(Equal(2))
	g.Expect(rs.unclassified).To(BeEmpty())
	g.Expect(rs.nodeCount()).To(Equal(5))
}

func TestResolveMaxUnavailable(t *testing.T) {
	intstrPtr := func(v intstr.IntOrString) *intstr.IntOrString { return &v }

	tests := []struct {
		name           string
		maxUnavailable *intstr.IntOrString
		paused         bool
		nodeCount      int
		want           int
		wantErr        bool
	}{
		{
			name:           "nil maxUnavailable defaults to 1",
			maxUnavailable: nil,
			nodeCount:      10,
			want:           1,
		},
		{
			name:           "int value",
			maxUnavailable: intstrPtr(intstr.FromInt32(3)),
			nodeCount:      10,
			want:           3,
		},
		{
			name:           "percentage rounds up",
			maxUnavailable: intstrPtr(intstr.FromString("25%")),
			nodeCount:      10,
			want:           3,
		},
		{
			name:           "paused returns 0 regardless of maxUnavailable",
			maxUnavailable: intstrPtr(intstr.FromInt32(3)),
			paused:         true,
			nodeCount:      10,
			want:           0,
		},
		{
			name:           "invalid string returns error",
			maxUnavailable: intstrPtr(intstr.FromString("banana")),
			nodeCount:      10,
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			var opts []testutil.PoolOption
			if tt.maxUnavailable != nil {
				opts = append(opts, testutil.WithMaxUnavailable(*tt.maxUnavailable))
			}
			if tt.paused {
				opts = append(opts, testutil.WithPaused(true))
			}
			pool := testutil.NewPool("p", testImageDigestRefA, opts...)
			got, err := resolveMaxUnavailable(pool, tt.nodeCount)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				g.Expect(isInvalidSpecError(err)).To(BeTrue())
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tt.want))
		})
	}
}

func TestSelectDrainCandidates(t *testing.T) {
	g := NewWithT(t)

	makeBN := func(name string, slotted bool) *bootcv1alpha1.BootcNode {
		bn := &bootcv1alpha1.BootcNode{}
		bn.Name = name
		if slotted {
			bn.Annotations = map[string]string{bootcv1alpha1.AnnotationInRebootSlot: ""}
		}
		return bn
	}

	// Core behavior: slotted nodes always included and come first
	// (alphabetical), then unslotted up to availableSlots (alphabetical).
	staged := []*bootcv1alpha1.BootcNode{
		makeBN("node-c", false),
		makeBN("node-b", true),
		makeBN("node-a", true),
		makeBN("node-d", false),
	}
	candidates := selectDrainCandidates(staged, 1)
	g.Expect(candidates).To(HaveLen(3))
	g.Expect(candidates[0].Name).To(Equal("node-a")) // slotted, alphabetical
	g.Expect(candidates[1].Name).To(Equal("node-b")) // slotted, alphabetical
	g.Expect(candidates[2].Name).To(Equal("node-c")) // first unslotted by name

	// Zero available slots with no slotted nodes returns nil.
	g.Expect(selectDrainCandidates([]*bootcv1alpha1.BootcNode{makeBN("x", false)}, 0)).To(BeNil())
}

func TestClassifyNode(t *testing.T) {
	const (
		desiredImage  = testImageDigestRefA
		desiredDigest = testDigestA
		otherDigest   = testDigestB
		nodeName      = "test-node"
	)

	idleCond := func(status metav1.ConditionStatus, reason string) metav1.Condition {
		return metav1.Condition{
			Type:   bootcv1alpha1.NodeIdle,
			Status: status,
			Reason: reason,
		}
	}

	degradedCond := func(status metav1.ConditionStatus, reason string) metav1.Condition {
		return metav1.Condition{
			Type:   bootcv1alpha1.NodeDegraded,
			Status: status,
			Reason: reason,
		}
	}

	tests := []struct {
		name         string
		bootedDigest string
		conditions   []metav1.Condition
		want         nodeState
	}{
		{
			name:         "UpToDate: image matches, Idle=True",
			bootedDigest: desiredDigest,
			conditions: []metav1.Condition{
				idleCond(metav1.ConditionTrue, bootcv1alpha1.NodeReasonIdle),
			},
			want: nodeStateUpToDate,
		},
		{
			name:         "Pending: image differs, Idle=True (daemon hasn't reacted)",
			bootedDigest: otherDigest,
			conditions: []metav1.Condition{
				idleCond(metav1.ConditionTrue, bootcv1alpha1.NodeReasonIdle),
			},
			want: nodeStatePending,
		},
		{
			name:         "Pending: no booted status yet (daemon starting)",
			bootedDigest: "",
			conditions:   nil,
			want:         nodeStatePending,
		},
		{
			name:         "Pending: image differs, no conditions",
			bootedDigest: otherDigest,
			conditions:   nil,
			want:         nodeStatePending,
		},
		{
			name:         "Staging: image differs, Idle=False reason=Staging",
			bootedDigest: otherDigest,
			conditions: []metav1.Condition{
				idleCond(metav1.ConditionFalse, bootcv1alpha1.NodeReasonStaging),
			},
			want: nodeStateStaging,
		},
		{
			name:         "Staged: image differs, Idle=False reason=Staged",
			bootedDigest: otherDigest,
			conditions: []metav1.Condition{
				idleCond(metav1.ConditionFalse, bootcv1alpha1.NodeReasonStaged),
			},
			want: nodeStateStaged,
		},
		{
			name:         "Rebooting: image differs, Idle=False reason=Rebooting",
			bootedDigest: otherDigest,
			conditions: []metav1.Condition{
				idleCond(metav1.ConditionFalse, bootcv1alpha1.NodeReasonRebooting),
			},
			want: nodeStateRebooting,
		},
		{
			name:         "Staging: Degraded=False does not affect classification",
			bootedDigest: otherDigest,
			conditions: []metav1.Condition{
				idleCond(metav1.ConditionFalse, bootcv1alpha1.NodeReasonStaging),
				degradedCond(metav1.ConditionFalse, bootcv1alpha1.NodeReasonHealthy),
			},
			want: nodeStateStaging,
		},
		{
			name:         "Degraded: Degraded=True takes priority over Idle",
			bootedDigest: desiredDigest,
			conditions: []metav1.Condition{
				idleCond(metav1.ConditionTrue, bootcv1alpha1.NodeReasonIdle),
				degradedCond(metav1.ConditionTrue, bootcv1alpha1.NodeReasonError),
			},
			want: nodeStateDegraded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			var opts []testutil.NodeOption
			if tt.bootedDigest != "" {
				opts = append(opts, testutil.WithBootedDigest(tt.bootedDigest))
			}
			for _, c := range tt.conditions {
				opts = append(opts, testutil.WithNodeCondition(c.Type, c.Status, c.Reason))
			}
			bn := testutil.NewNode(nodeName, desiredImage, opts...)
			got, err := classifyNode(bn)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tt.want))
		})
	}
}

func TestResolveRebootTimeout(t *testing.T) {
	g := NewWithT(t)

	secs := func(v int32) *int32 { return &v }

	// Nil rollout / nil field means the timeout is disabled.
	g.Expect(resolveRebootTimeout(&bootcv1alpha1.BootcNodePool{})).To(Equal(time.Duration(0)))

	poolNoField := &bootcv1alpha1.BootcNodePool{
		Spec: bootcv1alpha1.BootcNodePoolSpec{Rollout: &bootcv1alpha1.RolloutSpec{}},
	}
	g.Expect(resolveRebootTimeout(poolNoField)).To(Equal(time.Duration(0)))

	pool := &bootcv1alpha1.BootcNodePool{
		Spec: bootcv1alpha1.BootcNodePoolSpec{
			Rollout: &bootcv1alpha1.RolloutSpec{RebootTimeoutSeconds: secs(600)},
		},
	}
	g.Expect(resolveRebootTimeout(pool)).To(Equal(10 * time.Minute))
}

func TestRebootTimedOut(t *testing.T) {
	const desiredImage = testImageDigestRefA
	now := time.Now()

	rebootingNode := func(transitioned time.Time) *bootcv1alpha1.BootcNode {
		return testutil.NewNode(
			"n", desiredImage,
			testutil.WithBootedDigest(testDigestB),
			testutil.WithNodeConditionAt(
				bootcv1alpha1.NodeIdle,
				metav1.ConditionFalse,
				bootcv1alpha1.NodeReasonRebooting,
				transitioned,
			),
		)
	}

	tests := []struct {
		name    string
		node    *bootcv1alpha1.BootcNode
		timeout time.Duration
		want    bool
	}{
		{
			name:    "rebooting past timeout",
			node:    rebootingNode(now.Add(-11 * time.Minute)),
			timeout: 10 * time.Minute,
			want:    true,
		},
		{
			name:    "rebooting within timeout",
			node:    rebootingNode(now.Add(-5 * time.Minute)),
			timeout: 10 * time.Minute,
			want:    false,
		},
		{
			name:    "timeout disabled",
			node:    rebootingNode(now.Add(-1 * time.Hour)),
			timeout: 0,
			want:    false,
		},
		{
			name: "not rebooting",
			node: testutil.NewNode(
				"n", desiredImage,
				testutil.WithBootedDigest(testDigestB),
				testutil.WithNodeConditionAt(
					bootcv1alpha1.NodeIdle,
					metav1.ConditionFalse,
					bootcv1alpha1.NodeReasonStaged,
					now.Add(-1*time.Hour),
				),
			),
			timeout: 10 * time.Minute,
			want:    false,
		},
		{
			name:    "no idle condition",
			node:    testutil.NewNode("n", desiredImage, testutil.WithBootedDigest(testDigestB)),
			timeout: 10 * time.Minute,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(rebootTimedOut(tt.node, tt.timeout, now)).To(Equal(tt.want))
		})
	}
}

func TestApplyRebootTimeouts(t *testing.T) {
	const desiredImage = testImageDigestRefA
	now := time.Now()

	rebootingNode := func(name string, transitioned time.Time) *bootcv1alpha1.BootcNode {
		return testutil.NewNode(
			name, desiredImage,
			testutil.WithBootedDigest(testDigestB),
			testutil.WithNodeConditionAt(
				bootcv1alpha1.NodeIdle,
				metav1.ConditionFalse,
				bootcv1alpha1.NodeReasonRebooting,
				transitioned,
			),
			testutil.WithNodeAnnotation(bootcv1alpha1.AnnotationInRebootSlot, ""),
		)
	}

	t.Run("expired node moves to degraded, fresh node requeued", func(t *testing.T) {
		g := NewWithT(t)

		expired := rebootingNode("expired", now.Add(-11*time.Minute))
		fresh := rebootingNode("fresh", now.Add(-4*time.Minute))
		rs := &rolloutState{rebooting: []*bootcv1alpha1.BootcNode{expired, fresh}}

		requeue := applyRebootTimeouts(rs, 10*time.Minute, now)

		g.Expect(rs.degraded).To(HaveLen(1))
		g.Expect(rs.degraded[0].Name).To(Equal("expired"))
		g.Expect(rs.rebooting).To(HaveLen(1))
		g.Expect(rs.rebooting[0].Name).To(Equal("fresh"))
		// Timed-out nodes are also tracked so findUnhealthySlots can
		// escalate to a halt regardless of their booted digest.
		g.Expect(rs.rebootTimedOutNodes).To(HaveLen(1))
		g.Expect(rs.rebootTimedOutNodes[0].Name).To(Equal("expired"))
		// fresh transitioned 4m ago with a 10m timeout → 6m remaining.
		g.Expect(requeue).To(Equal(6 * time.Minute))
	})

	t.Run("disabled timeout is a no-op", func(t *testing.T) {
		g := NewWithT(t)

		node := rebootingNode("n", now.Add(-1*time.Hour))
		rs := &rolloutState{rebooting: []*bootcv1alpha1.BootcNode{node}}

		requeue := applyRebootTimeouts(rs, 0, now)

		g.Expect(rs.degraded).To(BeEmpty())
		g.Expect(rs.rebooting).To(HaveLen(1))
		g.Expect(requeue).To(Equal(time.Duration(0)))
	})
}

func TestFindUnhealthySlots(t *testing.T) {
	const (
		desiredImage = testImageDigestRefA
		targetDigest = testDigestA
		oldDigest    = testDigestB
	)

	slottedNode := func(name, bootedDigest string) *bootcv1alpha1.BootcNode {
		return testutil.NewNode(
			name, desiredImage,
			testutil.WithBootedDigest(bootedDigest),
			testutil.WithNodeAnnotation(bootcv1alpha1.AnnotationInRebootSlot, ""),
		)
	}

	t.Run("degraded on target digest counts, on old digest does not", func(t *testing.T) {
		g := NewWithT(t)
		onTarget := slottedNode("on-target", targetDigest)
		onOld := slottedNode("on-old", oldDigest)
		rs := &rolloutState{degraded: []*bootcv1alpha1.BootcNode{onTarget, onOld}}

		got := findUnhealthySlots(rs, targetDigest)

		g.Expect(got).To(ConsistOf(unhealthySlot{name: "on-target", reason: "Degraded"}))
	})

	t.Run("reboot-timed-out node counts regardless of digest", func(t *testing.T) {
		g := NewWithT(t)
		// A timed-out node never booted the target; it sits on the old
		// digest but must still count toward the halt threshold.
		timedOut := slottedNode("timed-out", oldDigest)
		rs := &rolloutState{
			degraded:            []*bootcv1alpha1.BootcNode{timedOut},
			rebootTimedOutNodes: []*bootcv1alpha1.BootcNode{timedOut},
		}

		got := findUnhealthySlots(rs, targetDigest)

		g.Expect(got).To(ConsistOf(unhealthySlot{name: "timed-out", reason: "RebootTimeout"}))
	})

	t.Run("upToDate node still in a slot is NotReady", func(t *testing.T) {
		g := NewWithT(t)
		notReady := slottedNode("not-ready", targetDigest)
		rs := &rolloutState{upToDate: []*bootcv1alpha1.BootcNode{notReady}}

		got := findUnhealthySlots(rs, targetDigest)

		g.Expect(got).To(ConsistOf(unhealthySlot{name: "not-ready", reason: "NotReady"}))
	})

	t.Run("unslotted timed-out node is ignored", func(t *testing.T) {
		g := NewWithT(t)
		unslotted := testutil.NewNode(
			"unslotted",
			desiredImage,
			testutil.WithBootedDigest(oldDigest),
		)
		rs := &rolloutState{
			degraded:            []*bootcv1alpha1.BootcNode{unslotted},
			rebootTimedOutNodes: []*bootcv1alpha1.BootcNode{unslotted},
		}

		g.Expect(findUnhealthySlots(rs, targetDigest)).To(BeEmpty())
	})
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

			err := r.assignRebootSlot(ctx, bn, k8sNode)
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
