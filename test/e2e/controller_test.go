// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
	"github.com/jlebon/bootc-operator/test/e2e/e2eutil"
)

const (
	pollTimeout  = 60 * time.Second
	pollInterval = 2 * time.Second
)

// TestControllerMembership provisions a worker node, creates a
// BootcNodePool selecting it, and verifies that a BootcNode is created
// and the node is labeled bootc.dev/managed.
func TestControllerMembership(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeName := env.AddNode(t)

	ctx := context.Background()

	// Create a pool selecting this test's nodes.
	imageRef := "quay.io/example/myos@sha256:06f961b802bc46ee168555f066d28f4f0e9afdf3f88174c1ee6f9de004fc30a0"
	pool := env.NewPool("workers", imageRef)
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	// Wait for BootcNode to appear for the worker.
	var bn bootcv1alpha1.BootcNode
	g.Eventually(func() error {
		return env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
	}).Should(Succeed())

	// Verify ownerReference.
	owner := metav1.GetControllerOf(&bn)
	g.Expect(owner).NotTo(BeNil())
	g.Expect(owner.Name).To(Equal(pool.Name))

	// Verify desiredImage.
	g.Expect(bn.Spec.DesiredImage).To(Equal(imageRef))

	// Verify the worker has the managed label.
	var node corev1.Node
	g.Eventually(func() (map[string]string, error) {
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &node)
		return node.Labels, err
	}).Should(HaveKey(bootcv1alpha1.LabelManaged))

	// Verify daemon pod schedules on the managed node and reaches Running.
	g.Eventually(func() ([]corev1.Pod, error) {
		var pods corev1.PodList
		err := env.Client.List(ctx, &pods,
			client.InNamespace("bootc-operator"),
			client.MatchingLabels{
				"app.kubernetes.io/name":      "bootc-operator",
				"app.kubernetes.io/component": "daemon",
			},
		)
		return pods.Items, err
	}).Should(ConsistOf(And(
		HaveField("Spec.NodeName", nodeName),
		HaveField("Status.Phase", corev1.PodRunning),
	)), "expected exactly one running daemon pod on %s", nodeName)
}
