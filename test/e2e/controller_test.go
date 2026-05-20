// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"maps"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
	"github.com/jlebon/bootc-operator/test/e2e/e2eutil"
	testutil "github.com/jlebon/bootc-operator/test/util"
)

const (
	pollTimeout  = 60 * time.Second
	pollInterval = 2 * time.Second
)

// TestControllerMembership deploys the controller in a bink cluster,
// creates a BootcNodePool selecting the worker node, and verifies that
// a BootcNode is created and the node is labeled bootc.dev/managed.
func TestControllerMembership(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)

	ctx := context.Background()

	// The bink cluster has a node called "node1". Label it as a worker so it
	// matches our pool's default nodeSelector. XXX: lower this down to bink?
	var node corev1.Node
	g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: "node1"}, &node)).To(Succeed())
	patch := client.StrategicMergeFrom(node.DeepCopy())
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	maps.Copy(node.Labels, testutil.WorkerLabels())
	g.Expect(env.Client.Patch(ctx, &node, patch)).To(Succeed())

	// Create a pool with a digest ref.
	imageRef := "quay.io/example/myos@sha256:06f961b802bc46ee168555f066d28f4f0e9afdf3f88174c1ee6f9de004fc30a0"
	pool := testutil.NewPool("e2e-workers", imageRef, testutil.WithWorkerSelector())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	// Wait for BootcNode to appear for node1.
	var bn bootcv1alpha1.BootcNode
	g.Eventually(func() error {
		return env.Client.Get(ctx, client.ObjectKey{Name: "node1"}, &bn)
	}).Should(Succeed())

	// Verify ownerReference.
	owner := metav1.GetControllerOf(&bn)
	g.Expect(owner).NotTo(BeNil())
	g.Expect(owner.Name).To(Equal("e2e-workers"))

	// Verify desiredImage.
	g.Expect(bn.Spec.DesiredImage).To(Equal(imageRef))

	// Verify node1 has the managed label.
	g.Eventually(func() (map[string]string, error) {
		err := env.Client.Get(ctx, client.ObjectKey{Name: "node1"}, &node)
		return node.Labels, err
	}).Should(HaveKey(bootcv1alpha1.LabelManaged))
}
