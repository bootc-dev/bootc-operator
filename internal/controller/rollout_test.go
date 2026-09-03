// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

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
