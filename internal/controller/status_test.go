// SPDX-License-Identifier: Apache-2.0

package controller

import (
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
			testutil.NewNode("n1", testImageDigestRefA, testutil.WithBootedDigest(testDigestA)),
			testutil.NewNode("n2", testImageDigestRefA, testutil.WithBootedDigest(testDigestA)),
			testutil.NewNode("n3", testImageDigestRefA, testutil.WithBootedDigest(testDigestA)),
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
			testutil.NewNode("n1", testImageDigestRefA, testutil.WithBootedDigest(testDigestA)),
		},
		pending: []*bootcv1alpha1.BootcNode{
			testutil.NewNode("n2", testImageDigestRefA, testutil.WithBootedDigest(testDigestB)),
		},
		staging: []*bootcv1alpha1.BootcNode{
			testutil.NewNode("n3", testImageDigestRefA, testutil.WithBootedDigest(testDigestB)),
		},
		staged: []*bootcv1alpha1.BootcNode{
			testutil.NewNode("n4", testImageDigestRefA, testutil.WithBootedDigest(testDigestB)),
		},
		rebooting: []*bootcv1alpha1.BootcNode{
			testutil.NewNode("n5", testImageDigestRefA, testutil.WithBootedDigest(testDigestB)),
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
			testutil.NewNode("n1", testImageDigestRefA, testutil.WithBootedDigest(testDigestA)),
		},
		pending: []*bootcv1alpha1.BootcNode{
			testutil.NewNode("n2", testImageDigestRefA, testutil.WithBootedDigest(testDigestB)),
			testutil.NewNode("n3", testImageDigestRefA, testutil.WithBootedDigest(testDigestB)),
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

// Verifies that deployedDigest retains its previous value while a rollout
// is in progress, and updateAvailable is true.
func TestSyncPoolStatusDeployedDigestPreserved(t *testing.T) {
	g := NewWithT(t)

	pool := testutil.NewPool("test", testImageDigestRefB, testutil.WithWorkerSelector())
	pool.Status.TargetDigest = testDigestB
	pool.Status.DeployedDigest = testDigestA

	rs := &rolloutState{
		pending: []*bootcv1alpha1.BootcNode{
			testutil.NewNode("n1", testImageDigestRefB, testutil.WithBootedDigest(testDigestA)),
		},
	}

	syncPoolStatus(pool, rs)

	g.Expect(pool.Status.DeployedDigest).To(Equal(testDigestA))
	g.Expect(pool.Status.UpdateAvailable).To(BeTrue())
}
