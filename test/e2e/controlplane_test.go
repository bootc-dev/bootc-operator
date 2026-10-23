// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/test/e2e/e2eutil"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

// TestControlPlaneUpgrade adds a second control-plane node, creates a pool
// selecting all control-plane nodes, and triggers an OS upgrade. Because the
// operator itself runs on a control-plane node, the rollout eventually
// upgrades the node hosting the operator, forcing it to migrate and recover.
// This validates that the controller can survive upgrading its own node.
func TestControlPlaneUpgrade(t *testing.T) {
	e2eutil.Providers(t, "bink")
	// Serial: modifies the original control-plane node and cluster
	// infrastructure; no other tests should run concurrently.
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	ctx := context.Background()

	// Phase 1: Add a second control-plane node so infrastructure pods can
	// migrate when the first one reboots.
	cpNode := env.AddNode(t, e2eutil.WithRole("control-plane"))
	t.Logf("Added control-plane node %q", cpNode)

	// Phase 2: Create a pool selecting all control-plane nodes. The
	// original "controller" node and our new node both carry the
	// node-role.kubernetes.io/control-plane label from kubeadm.
	pool := env.NewPool("cp-upgrade",
		env.NodeImageDigestedPullSpec(),
		testutil.WithNodeSelector(testutil.ControlPlaneLabels()),
	)
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())
	t.Logf("Created pool %q selecting control-plane nodes", pool.Name)

	// Wait for both control-plane nodes to reach Idle with the base image.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		WithTimeout(5 * time.Minute).
		Should(And(
			HaveField("NodeCount", BeNumerically(">=", 2)),
			HaveField("UpdatingCount", BeEquivalentTo(0)),
			HaveField("Conditions", ContainElement(And(
				HaveField("Type", bootcv1alpha1.PoolUpToDate),
				HaveField("Status", metav1.ConditionTrue),
			))),
		))
	t.Logf("Pool is up to date with base image, %d nodes managed", 2)

	// Phase 3: Trigger the upgrade by patching the pool to the update image.
	updateRef := env.NodeImageUpdateDigestedPullSpec()
	modified := pool.DeepCopy()
	modified.Spec.Image.Ref = updateRef
	g.Expect(env.Client.Patch(ctx, modified, client.MergeFrom(pool))).To(Succeed())
	*pool = *modified
	t.Logf("Patched pool to update image %s", updateRef)

	// Phase 4: Wait for the rollout to complete. This involves two reboots
	// (one per control-plane node, serialized by maxUnavailable=1). When the
	// operator's own node reboots, the operator pod migrates to the other
	// control-plane and must resume the rollout. Use a generous timeout:
	// each reboot cycle (drain + reboot + rejoin) takes several minutes, and
	// the operator needs time to recover after migration.
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		WithTimeout(15*time.Minute).WithPolling(10*time.Second).
		Should(poolAllUpdated(2, env.NodeImageUpdateDigest()),
			"expected all control-plane nodes to complete the upgrade")

	t.Logf("All control-plane nodes upgraded successfully")

	// Phase 5: Verify each managed BootcNode is Idle with the update digest.
	var poolObj bootcv1alpha1.BootcNodePool
	g.Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pool), &poolObj)).To(Succeed())

	var bnList bootcv1alpha1.BootcNodeList
	g.Expect(env.Client.List(ctx, &bnList)).To(Succeed())
	for _, bn := range bnList.Items {
		testutil.WaitForNodeIdle(t, g, ctx, env.Client, bn.Name, 3*time.Minute,
			HaveField("Booted", imageMatchesDigest(env.NodeImageUpdateDigest())),
		)
	}

	// Phase 6: Roll back to the original image to leave the cluster clean.
	originalRef := env.NodeImageDigestedPullSpec()
	rollback := pool.DeepCopy()
	rollback.Spec.Image.Ref = originalRef
	g.Expect(env.Client.Patch(ctx, rollback, client.MergeFrom(pool))).To(Succeed())
	*pool = *rollback
	t.Logf("Rolling back to original image %s", originalRef)

	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		WithTimeout(15*time.Minute).WithPolling(10*time.Second).
		Should(poolAllUpdated(2, env.NodeImageDigest()),
			"expected rollback to complete on all control-plane nodes")

	t.Logf("Rollback completed, cluster is back on the original image")
}
