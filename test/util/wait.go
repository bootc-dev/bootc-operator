// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

// WaitForNodeIdle polls a BootcNode until it reaches Idle state with
// Booted not nil. Extra matchers are ANDed with the base assertions,
// allowing callers to add checks like ImageDigest or Image.
func WaitForNodeIdle(
	t *testing.T,
	g gomega.Gomega,
	ctx context.Context,
	c client.Client,
	nodeName string,
	timeout time.Duration,
	extraMatchers ...types.GomegaMatcher,
) {
	t.Helper()
	matchers := []types.GomegaMatcher{
		gomega.HaveField("Booted", gomega.Not(gomega.BeNil())),
		gomega.HaveField("Conditions", gomega.ContainElement(gomega.And(
			gomega.HaveField("Type", bootcv1alpha1.NodeIdle),
			gomega.HaveField("Status", metav1.ConditionTrue),
			gomega.HaveField("Reason", bootcv1alpha1.NodeReasonIdle),
		))),
	}
	matchers = append(matchers, extraMatchers...)
	g.Eventually(func() (bootcv1alpha1.BootcNodeStatus, error) {
		var bn bootcv1alpha1.BootcNode
		err := c.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return bn.Status, err
	}).WithTimeout(timeout).Should(gomega.And(matchers...))
}
