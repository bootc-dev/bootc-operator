// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/test/e2e/e2eutil"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

// TestCRDSmoke verifies CRD round-trips. We'll nuke it or enhance it with more
// e2e-worthy flows once we have more of the controller and daemon implemented.
// Note more comprehensive CRD round-trip tests exist in the unit tests.
func TestCRDSmoke(t *testing.T) {
	env := e2eutil.New(t)

	ctx := context.Background()

	t.Run("BootcNodePool", func(t *testing.T) {
		g := NewWithT(t)

		pool := env.NewPool("pool", testutil.ImageTaggedRef)
		g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

		got := &bootcv1alpha1.BootcNodePool{}
		g.Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(pool), got)).To(Succeed())
		g.Expect(got.Spec.Image.Ref).To(Equal(testutil.ImageTaggedRef))
	})

	t.Run("BootcNode", func(t *testing.T) {
		g := NewWithT(t)

		node := testutil.NewNode("smoke-node", testutil.ImageDigestRefA)
		g.Expect(env.Client.Create(ctx, node)).To(Succeed())
		t.Cleanup(func() {
			_ = env.Client.Delete(ctx, node)
		})

		got := &bootcv1alpha1.BootcNode{}
		g.Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(node), got)).To(Succeed())
		g.Expect(got.Spec.DesiredImage).To(Equal(testutil.ImageDigestRefA))
		g.Expect(got.Spec.DesiredImageState).To(Equal(bootcv1alpha1.DesiredImageStateStaged))
	})
}
