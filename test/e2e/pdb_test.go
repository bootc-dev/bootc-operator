// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/test/e2e/e2eutil"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

const (
	// defaultWorkloadImage is a tiny image that every kubelet already has
	// as its pod sandbox image. Override with E2E_WORKLOAD_IMAGE when the
	// nodes cannot pull it.
	defaultWorkloadImage = "registry.k8s.io/pause:3.10"

	// drainStallWait is measured from patching the pool. It covers staging
	// the image, taking a reboot slot, the controller's five minute drain
	// stall threshold, and reconciling to report the blocking PDB. It is
	// deliberately generous to absorb slow image pulls.
	drainStallWait = 8 * time.Minute
)

// TestRolloutBlockedByPDB provisions a single node running an application
// whose PodDisruptionBudget allows no disruptions, so the node cannot be
// drained. It verifies that the controller reports the blocking budget through
// a DrainBlockedByPDB event and the UpToDate=False/PDBBlocked pool condition
// without rebooting the node, then relaxes the budget and verifies that the
// drain and the rollout finish.
func TestRolloutBlockedByPDB(t *testing.T) {
	e2eutil.Providers(t, "bink", "eks")
	t.Parallel()
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeName := env.AddNode(t)

	ctx := context.Background()

	pool := env.NewPool("pdb", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute)
	t.Logf("Node %q is Idle with original image", nodeName)

	var bootcNode bootcv1alpha1.BootcNode
	g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bootcNode)).To(Succeed())

	// Phase 1: Run a single-replica application on the node and protect it
	// with a PDB that requires that replica to stay available.
	workloadImage := defaultWorkloadImage
	if img := os.Getenv("E2E_WORKLOAD_IMAGE"); img != "" {
		workloadImage = img
	}
	appName := "pdb-" + env.TestID()
	appLabels := map[string]string{"app": appName}

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName,
			Namespace: metav1.NamespaceDefault,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: appLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: appLabels},
				Spec: corev1.PodSpec{
					NodeSelector: env.TestLabels(),
					Containers: []corev1.Container{{
						Name:            "app",
						Image:           workloadImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
					}},
				},
			},
		},
	}
	g.Expect(env.Client.Create(ctx, deploy)).To(Succeed())
	t.Cleanup(func() {
		_ = env.Client.Delete(ctx, deploy)
	})

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName,
			Namespace: metav1.NamespaceDefault,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: ptr.To(intstr.FromInt32(1)),
			Selector:     &metav1.LabelSelector{MatchLabels: appLabels},
		},
	}
	g.Expect(env.Client.Create(ctx, pdb)).To(Succeed())
	t.Cleanup(func() {
		_ = env.Client.Delete(ctx, pdb)
	})

	pdbKey := client.ObjectKeyFromObject(pdb)

	g.Eventually(func() (policyv1.PodDisruptionBudgetStatus, error) {
		var current policyv1.PodDisruptionBudget
		err := env.Client.Get(ctx, pdbKey, &current)
		return current.Status, err
	}).WithTimeout(3*time.Minute).Should(And(
		HaveField("CurrentHealthy", BeEquivalentTo(1)),
		HaveField("ExpectedPods", BeEquivalentTo(1)),
		HaveField("DisruptionsAllowed", BeEquivalentTo(0)),
	), "expected the PDB to allow no disruptions")

	t.Logf("Application %q is running with a zero disruption budget", appName)

	// Phase 2: Roll out a new image. The node stages it, takes a reboot
	// slot, and then gets stuck draining because the PDB refuses eviction.
	updateRef := env.NodeImageUpdateDigestedPullSpec()

	modified := pool.DeepCopy()
	modified.Spec.Image.Ref = updateRef
	g.Expect(env.Client.Patch(ctx, modified, client.MergeFrom(pool))).To(Succeed())
	*pool = *modified

	t.Logf("Patched pool to update image %s", updateRef)

	// Phase 3: The stall is reported on the node and on the pool.
	g.Eventually(fetchEvents(ctx, env.Client, "BootcNode", nodeName, bootcNode.UID)).
		WithTimeout(drainStallWait).
		Should(ContainElement(And(
			HaveField("Type", corev1.EventTypeWarning),
			HaveField("Reason", "DrainBlockedByPDB"),
			HaveField("Action", "Drain"),
			HaveField("Note", ContainSubstring(metav1.NamespaceDefault+"/"+appName)),
		)), "expected a DrainBlockedByPDB event naming the PDB")

	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).Should(HaveField(
		"Conditions", ContainElement(And(
			HaveField("Type", bootcv1alpha1.PoolUpToDate),
			HaveField("Status", metav1.ConditionFalse),
			HaveField("Reason", bootcv1alpha1.PoolPDBBlocked),
			HaveField("Message", ContainSubstring(nodeName+" blocked by PDB")),
			HaveField("Message", ContainSubstring(metav1.NamespaceDefault+"/"+appName)),
		)),
	), "expected the pool to report PDBBlocked")

	// The node must stay cordoned, holding its slot, and must not have been
	// told to reboot while the drain is blocked.
	var blocked bootcv1alpha1.BootcNode
	g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &blocked)).To(Succeed())
	g.Expect(blocked.Spec.DesiredImageState).To(Equal(bootcv1alpha1.DesiredImageStateStaged))
	g.Expect(blocked.Annotations).To(HaveKey(bootcv1alpha1.AnnotationInRebootSlot))

	var node corev1.Node
	g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &node)).To(Succeed())
	g.Expect(node.Spec.Unschedulable).To(BeTrue(), "expected the blocked node to stay cordoned")

	t.Logf("Rollout is reported as blocked by PDB %s/%s", metav1.NamespaceDefault, appName)

	// Phase 4: Allow disruptions and verify the drain and rollout resume.
	g.Expect(env.Client.Get(ctx, pdbKey, pdb)).To(Succeed())
	relaxed := pdb.DeepCopy()
	relaxed.Spec.MinAvailable = ptr.To(intstr.FromInt32(0))
	g.Expect(env.Client.Patch(ctx, relaxed, client.MergeFrom(pdb))).To(Succeed())

	t.Logf("Relaxed PDB to minAvailable=0")

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 8*time.Minute,
		HaveField("Booted", imageMatchesDigest(env.NodeImageUpdateDigest())),
	)

	t.Logf("Node %q is Idle with update image", nodeName)

	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageUpdateDigest()))
	g.Eventually(fetchEvents(ctx, env.Client, "BootcNodePool", pool.Name, pool.UID)).
		Should(ContainElement(And(
			HaveField("Type", corev1.EventTypeNormal),
			HaveField("Reason", "RolloutCompleted"),
		)))

	g.Eventually(func() (bool, error) {
		var n corev1.Node
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &n)
		return n.Spec.Unschedulable, err
	}).WithTimeout(3*time.Minute).Should(BeFalse(),
		"expected node to be schedulable after update")
}
