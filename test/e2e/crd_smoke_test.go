// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
	"github.com/jlebon/bootc-operator/test/e2e/e2eutil"
	testutil "github.com/jlebon/bootc-operator/test/util"
)

// This is a simple "e2e" test which just tests CRD round-trips for now. We'll
// nuke it or enhance it with more e2e-worthy flows once we have more of the
// controller and daemon implemented.
//
// Note more comprehensive CRD round-trip tests exist in the unit tests.
func TestCRDSmoke(t *testing.T) {
	env := e2eutil.New(t)

	ctx := context.Background()

	t.Run("BootcNodePool", func(t *testing.T) {
		g := NewWithT(t)

		pool := testutil.NewPool("smoke-pool", "quay.io/example/myos:latest", testutil.WithWorkerSelector())

		g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

		got := &bootcv1alpha1.BootcNodePool{}
		g.Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pool), got)).To(Succeed())
		g.Expect(got.Spec.Image.Ref).To(Equal("quay.io/example/myos:latest"))

		g.Expect(env.Client.Delete(ctx, pool)).To(Succeed())
	})

	t.Run("BootcNode", func(t *testing.T) {
		g := NewWithT(t)

		node := testutil.NewNode("smoke-node", "quay.io/example/myos@sha256:abc123")

		g.Expect(env.Client.Create(ctx, node)).To(Succeed())

		got := &bootcv1alpha1.BootcNode{}
		g.Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(node), got)).To(Succeed())
		g.Expect(got.Spec.DesiredImage).To(Equal("quay.io/example/myos@sha256:abc123"))
		g.Expect(got.Spec.DesiredImageState).To(Equal(bootcv1alpha1.DesiredImageStateStaged))

		g.Expect(env.Client.Delete(ctx, node)).To(Succeed())
	})
}
