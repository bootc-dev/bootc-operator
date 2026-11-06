// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"math"
	"slices"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

// Verifies that when all nodes are running the target digest, the counts
// are correct, UpToDate is True/AllUpdated, and deployedDigest is set.
func TestSyncPoolStatusAllUpdated(t *testing.T) {
	g := NewWithT(t)

	pool := testutil.NewPool("test", testImageDigestRefA, testutil.WithWorkerSelector())
	pool.Status.TargetDigest = testDigestA

	rs := &rolloutState{
		upToDate: []*bootcv1alpha1.BootcNode{
			testutil.NewBootcNode(
				"n1",
				testImageDigestRefA,
				testutil.WithBootcNodeBootedDigest(testDigestA),
			),
			testutil.NewBootcNode(
				"n2",
				testImageDigestRefA,
				testutil.WithBootcNodeBootedDigest(testDigestA),
			),
			testutil.NewBootcNode(
				"n3",
				testImageDigestRefA,
				testutil.WithBootcNodeBootedDigest(testDigestA),
			),
		},
	}

	syncPoolStatus(pool, rs)

	g.Expect(pool.Status.NodeCount).To(Equal(int32(3)))
	g.Expect(pool.Status.UpdatedCount).To(Equal(int32(3)))
	g.Expect(pool.Status.UpdatingCount).To(Equal(int32(0)))
	g.Expect(pool.Status.DegradedCount).To(Equal(int32(0)))
	g.Expect(pool.Status.DeployedDigest).To(Equal(testDigestA))
	g.Expect(pool.Status.UpdateAvailable).To(BeFalse())
	g.Expect(pool.Status.Conditions).To(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolUpToDate),
		HaveField("Status", metav1.ConditionTrue),
		HaveField("Reason", bootcv1alpha1.PoolAllUpdated),
	)))
}

// Verifies that a rollout in progress with nodes in every bucket produces
// the correct counts, UpToDate False/RolloutInProgress, and a breakdown message.
func TestSyncPoolStatusRolloutInProgress(t *testing.T) {
	g := NewWithT(t)

	pool := testutil.NewPool("test", testImageDigestRefA, testutil.WithWorkerSelector())
	pool.Status.TargetDigest = testDigestA

	rs := &rolloutState{
		upToDate: []*bootcv1alpha1.BootcNode{
			testutil.NewBootcNode(
				"n1",
				testImageDigestRefA,
				testutil.WithBootcNodeBootedDigest(testDigestA),
			),
		},
		pending: []*bootcv1alpha1.BootcNode{
			testutil.NewBootcNode(
				"n2",
				testImageDigestRefA,
				testutil.WithBootcNodeBootedDigest(testDigestB),
			),
		},
		staging: []*bootcv1alpha1.BootcNode{
			testutil.NewBootcNode(
				"n3",
				testImageDigestRefA,
				testutil.WithBootcNodeBootedDigest(testDigestB),
			),
		},
		staged: []*bootcv1alpha1.BootcNode{
			testutil.NewBootcNode(
				"n4",
				testImageDigestRefA,
				testutil.WithBootcNodeBootedDigest(testDigestB),
			),
		},
		rebooting: []*bootcv1alpha1.BootcNode{
			testutil.NewBootcNode(
				"n5",
				testImageDigestRefA,
				testutil.WithBootcNodeBootedDigest(testDigestB),
			),
		},
	}

	syncPoolStatus(pool, rs)

	g.Expect(pool.Status.NodeCount).To(Equal(int32(5)))
	g.Expect(pool.Status.UpdatedCount).To(Equal(int32(1)))
	g.Expect(pool.Status.UpdatingCount).To(Equal(int32(4)))
	g.Expect(pool.Status.DegradedCount).To(Equal(int32(0)))
	g.Expect(pool.Status.Conditions).To(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolUpToDate),
		HaveField("Status", metav1.ConditionFalse),
		HaveField("Reason", bootcv1alpha1.PoolRolloutInProgress),
		HaveField(
			"Message",
			Equal("1/5 updated; 1 pending, 1 staging, 1 staged, 1 rebooting, 0 degraded"),
		),
	)))
}

// Verifies that a paused pool with pending nodes reports UpToDate
// False/Paused and includes the breakdown message.
func TestSyncPoolStatusPaused(t *testing.T) {
	g := NewWithT(t)

	pool := testutil.NewPool(
		"test",
		testImageDigestRefA,
		testutil.WithWorkerSelector(),
		testutil.WithPaused(true),
	)
	pool.Status.TargetDigest = testDigestA

	rs := &rolloutState{
		upToDate: []*bootcv1alpha1.BootcNode{
			testutil.NewBootcNode(
				"n1",
				testImageDigestRefA,
				testutil.WithBootcNodeBootedDigest(testDigestA),
			),
		},
		pending: []*bootcv1alpha1.BootcNode{
			testutil.NewBootcNode(
				"n2",
				testImageDigestRefA,
				testutil.WithBootcNodeBootedDigest(testDigestB),
			),
			testutil.NewBootcNode(
				"n3",
				testImageDigestRefA,
				testutil.WithBootcNodeBootedDigest(testDigestB),
			),
		},
	}

	syncPoolStatus(pool, rs)

	g.Expect(pool.Status.NodeCount).To(Equal(int32(3)))
	g.Expect(pool.Status.UpdatedCount).To(Equal(int32(1)))
	g.Expect(pool.Status.UpdatingCount).To(Equal(int32(2)))
	g.Expect(pool.Status.Conditions).To(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolUpToDate),
		HaveField("Status", metav1.ConditionFalse),
		HaveField("Reason", bootcv1alpha1.PoolPaused),
		HaveField(
			"Message",
			Equal("1/3 updated; 2 pending, 0 staging, 0 staged, 0 rebooting, 0 degraded"),
		),
	)))
}

// Verifies that an empty pool with no nodes is vacuously considered
// up-to-date, with all counts at zero and deployedDigest set.
func TestSyncPoolStatusEmptyPool(t *testing.T) {
	g := NewWithT(t)

	pool := testutil.NewPool("test", testImageDigestRefA, testutil.WithWorkerSelector())
	pool.Status.TargetDigest = testDigestA

	rs := &rolloutState{}

	syncPoolStatus(pool, rs)

	g.Expect(pool.Status.NodeCount).To(Equal(int32(0)))
	g.Expect(pool.Status.UpdatedCount).To(Equal(int32(0)))
	g.Expect(pool.Status.UpdatingCount).To(Equal(int32(0)))
	g.Expect(pool.Status.DegradedCount).To(Equal(int32(0)))
	g.Expect(pool.Status.DeployedDigest).To(Equal(testDigestA))
	g.Expect(pool.Status.UpdateAvailable).To(BeFalse())
	g.Expect(pool.Status.Conditions).To(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolUpToDate),
		HaveField("Status", metav1.ConditionTrue),
		HaveField("Reason", bootcv1alpha1.PoolAllUpdated),
	)))
}

// pdbStagedNode builds a staged node, optionally holding a reboot slot.
func pdbStagedNode(name string, slotted bool) *bootcv1alpha1.BootcNode {
	node := testutil.NewBootcNode(name, testImageDigestRefA)
	if slotted {
		node.Annotations = map[string]string{
			bootcv1alpha1.AnnotationInRebootSlot: "",
		}
	}
	return node
}

// Verifies that a fully consumed rollout blocked by PDBs reports
// UpToDate=False/PDBBlocked, while partial blocks keep RolloutInProgress with a
// per-node hint. Paused keeps priority over PDBBlocked.
func TestSyncPoolStatusPDBBlocked(t *testing.T) {
	tests := []struct {
		name           string
		paused         bool
		occupiedSlots  int
		maxUnavailable int
		slottedNodes   []string
		blockedNodes   []string
		wantReason     string
		wantMessage    string
	}{
		{
			name:           "total block",
			occupiedSlots:  2,
			maxUnavailable: 2,
			slottedNodes:   []string{"n1", "n2"},
			blockedNodes:   []string{"n1", "n2"},
			wantReason:     bootcv1alpha1.PoolPDBBlocked,
			wantMessage: "0/2 updated; 0 pending, 0 staging, 2 staged, 0 rebooting, " +
				"0 degraded; n1 blocked by PDB default/a; " +
				"n2 blocked by PDB default/b",
		},
		{
			name:           "partial block",
			occupiedSlots:  2,
			maxUnavailable: 2,
			slottedNodes:   []string{"n1", "n2"},
			blockedNodes:   []string{"n1"},
			wantReason:     bootcv1alpha1.PoolRolloutInProgress,
			wantMessage: "0/2 updated; 0 pending, 0 staging, 2 staged, 0 rebooting, " +
				"0 degraded; n1 blocked by PDB default/a",
		},
		{
			name:           "slots not fully consumed",
			occupiedSlots:  1,
			maxUnavailable: 2,
			slottedNodes:   []string{"n1"},
			blockedNodes:   []string{"n1"},
			wantReason:     bootcv1alpha1.PoolRolloutInProgress,
			wantMessage: "0/2 updated; 0 pending, 0 staging, 2 staged, 0 rebooting, " +
				"0 degraded; n1 blocked by PDB default/a",
		},
		{
			name:           "paused has priority",
			paused:         true,
			occupiedSlots:  2,
			maxUnavailable: 2,
			slottedNodes:   []string{"n1", "n2"},
			blockedNodes:   []string{"n1", "n2"},
			wantReason:     bootcv1alpha1.PoolPaused,
			wantMessage: "0/2 updated; 0 pending, 0 staging, 2 staged, 0 rebooting, " +
				"0 degraded; n1 blocked by PDB default/a; " +
				"n2 blocked by PDB default/b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			pool := testutil.NewPool(
				"test",
				testImageDigestRefA,
				testutil.WithWorkerSelector(),
				testutil.WithPaused(tt.paused),
			)
			pool.Status.TargetDigest = testDigestA

			rs := &rolloutState{
				staged: []*bootcv1alpha1.BootcNode{
					pdbStagedNode("n1", slices.Contains(tt.slottedNodes, "n1")),
					pdbStagedNode("n2", slices.Contains(tt.slottedNodes, "n2")),
				},
				occupiedSlots:  tt.occupiedSlots,
				maxUnavailable: tt.maxUnavailable,
				pdbBlocked:     map[string][]pdbBlock{},
			}
			for _, nodeName := range tt.blockedNodes {
				pdbName := "a"
				if nodeName == "n2" {
					pdbName = "b"
				}
				rs.pdbBlocked[nodeName] = []pdbBlock{{
					PDBs: []pdbRef{{Namespace: "default", Name: pdbName}},
					Hint: pdbNeverAllowsEvictionHint,
				}}
			}

			syncPoolStatus(pool, rs)

			g.Expect(pool.Status.Conditions).To(ContainElement(And(
				HaveField("Type", bootcv1alpha1.PoolUpToDate),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", tt.wantReason),
				HaveField("Message", tt.wantMessage),
			)))
		})
	}
}

// Verifies that the PDBBlocked reason clears when the blocking PDBs no longer
// block the drains.
func TestSyncPoolStatusPDBBlockedClears(t *testing.T) {
	g := NewWithT(t)

	pool := testutil.NewPool("test", testImageDigestRefA, testutil.WithWorkerSelector())
	pool.Status.TargetDigest = testDigestA

	rs := &rolloutState{
		staged:         []*bootcv1alpha1.BootcNode{pdbStagedNode("n1", true)},
		occupiedSlots:  1,
		maxUnavailable: 1,
		pdbBlocked: map[string][]pdbBlock{
			"n1": {{
				PDBs: []pdbRef{{Namespace: "default", Name: "a"}},
				Hint: pdbNeverAllowsEvictionHint,
			}},
		},
	}
	syncPoolStatus(pool, rs)
	g.Expect(pool.Status.Conditions).To(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolUpToDate),
		HaveField("Reason", bootcv1alpha1.PoolPDBBlocked),
	)))

	rs.pdbBlocked = nil
	syncPoolStatus(pool, rs)
	g.Expect(pool.Status.Conditions).To(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolUpToDate),
		HaveField("Reason", bootcv1alpha1.PoolRolloutInProgress),
		HaveField("Message", Not(ContainSubstring("blocked by PDB"))),
	)))
}

// Verifies that deployedDigest retains its previous value while a rollout
// is in progress, and updateAvailable is true.
func TestSyncPoolStatusDeployedDigestPreserved(t *testing.T) {
	g := NewWithT(t)

	pool := testutil.NewPool("test", testImageDigestRefB, testutil.WithWorkerSelector())
	pool.Status.TargetDigest = testDigestB
	pool.Status.DeployedDigest = testDigestA

	rs := &rolloutState{
		pending: []*bootcv1alpha1.BootcNode{
			testutil.NewBootcNode(
				"n1",
				testImageDigestRefB,
				testutil.WithBootcNodeBootedDigest(testDigestA),
			),
		},
	}

	syncPoolStatus(pool, rs)

	g.Expect(pool.Status.DeployedDigest).To(Equal(testDigestA))
	g.Expect(pool.Status.UpdateAvailable).To(BeTrue())
}

// Verifies that clampCount passes in-range counts through and saturates
// instead of wrapping when a count does not fit in int32.
func TestClampCount(t *testing.T) {
	g := NewWithT(t)

	g.Expect(clampCount(0)).To(Equal(int32(0)))
	g.Expect(clampCount(42)).To(Equal(int32(42)))
	g.Expect(clampCount(math.MaxInt32)).To(Equal(int32(math.MaxInt32)))
	g.Expect(clampCount(math.MaxInt32 + 1)).To(Equal(int32(math.MaxInt32)))
	g.Expect(clampCount(-1)).To(Equal(int32(0)))
}
