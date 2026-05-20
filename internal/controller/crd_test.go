// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
	testutil "github.com/jlebon/bootc-operator/test/util"
)

// Test constants for image refs and digests.
const (
	testImageTaggedRef = "quay.io/example/myos:latest"

	testDigestA         = "sha256:06f961b802bc46ee168555f066d28f4f0e9afdf3f88174c1ee6f9de004fc30a0" // "A"
	testDigestB         = "sha256:c0cde77fa8fef97d476c10aad3d2d54fcc2f336140d073651c2dcccf1e379fd6" // "B"
	testDigestC         = "sha256:12f37a8a84034d3e623d726fe10e5031f4df997ac13f4d5571b5a90c41fb84fe" // "C"
	testImageDigestRefA = "quay.io/example/myos@" + testDigestA
	testImageDigestRefB = "quay.io/example/myos@" + testDigestB
	testImageDigestRefC = "quay.io/example/myos@" + testDigestC

	testSecretName = "my-pull-secret"
	testSecretNS   = "bootc-operator"
	testSecretHash = "sha256:b37e50cedcd3e3f1ff64f4afc0422084ae694253cf399326868e07a35f4a45fb" // "secret"
)

func TestBootcNodePoolCRD(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	pool := testutil.NewPool("workers", testImageTaggedRef,
		testutil.WithWorkerSelector(),
		testutil.WithRebootPolicy(bootcv1alpha1.RebootPolicyAllowSoftReboot),
		testutil.WithPullSecret(testSecretName, testSecretNS),
	)

	// Save the spec before Create, which mutates pool in-place with
	// the server response (including defaults).
	wantSpec := *pool.Spec.DeepCopy()

	// Create
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, pool))
	})

	// Retrieve and verify spec round-trips. We set all defaulted
	// fields explicitly (RebootPolicy), so the input and output specs
	// should match exactly.
	got := &bootcv1alpha1.BootcNodePool{}
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), got)).To(Succeed())
	g.Expect(got.Spec).To(Equal(wantSpec))
}

func TestBootcNodeCRD(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := testutil.NewNode("worker-1", testImageDigestRefA,
		testutil.WithNodePullSecret(testSecretName, testSecretNS, testSecretHash),
	)

	// Save the spec before Create, which mutates node in-place.
	wantSpec := *node.Spec.DeepCopy()

	// Create
	g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
	t.Cleanup(func() {
		_ = client.IgnoreNotFound(k8sClient.Delete(ctx, node))
	})

	// Retrieve and verify spec round-trips. BootcNodeSpec has no
	// defaulted fields, so the input and output should match exactly.
	got := &bootcv1alpha1.BootcNode{}
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), got)).To(Succeed())
	g.Expect(got.Spec).To(Equal(wantSpec))

	// Update status. Use a fixed timestamp truncated to seconds to match the
	// precision the API server stores so we can just `Equal` the whole thing.
	now := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	ts := metav1.NewTime(time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC))
	got.Status = bootcv1alpha1.BootcNodeStatus{
		ObservedGeneration: got.Generation,
		Booted: &bootcv1alpha1.ImageInfo{
			Image:             testImageDigestRefB,
			ImageDigest:       testDigestB,
			Version:           "9.4",
			Timestamp:         &ts,
			Architecture:      "amd64",
			SoftRebootCapable: true,
			Incompatible:      false,
		},
		Staged: &bootcv1alpha1.ImageInfo{
			Image:             testImageDigestRefA,
			ImageDigest:       testDigestA,
			SoftRebootCapable: true,
		},
		Rollback: &bootcv1alpha1.ImageInfo{
			Image:       testImageDigestRefC,
			ImageDigest: testDigestC,
		},
		Conditions: []metav1.Condition{
			{
				Type:               bootcv1alpha1.NodeIdle,
				Status:             metav1.ConditionFalse,
				Reason:             bootcv1alpha1.NodeReasonStaged,
				Message:            "Image staged, awaiting desiredImageState: Booted",
				LastTransitionTime: now,
			},
		},
	}
	wantStatus := *got.Status.DeepCopy() // snapshot before Update
	g.Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

	// Verify status round-trips
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(node), got)).To(Succeed())
	// Copy canonical timestamps from the server response so
	// Equal ignores timezone/precision differences.
	for i := range wantStatus.Conditions {
		wantStatus.Conditions[i].LastTransitionTime = got.Status.Conditions[i].LastTransitionTime
	}
	if wantStatus.Booted != nil && wantStatus.Booted.Timestamp != nil {
		wantStatus.Booted.Timestamp = got.Status.Booted.Timestamp
	}
	g.Expect(got.Status).To(Equal(wantStatus))
}

func TestBootcNodePoolEnumValidation(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	pool := testutil.NewPool("invalid-reboot-policy", testImageTaggedRef,
		testutil.WithWorkerSelector(),
		testutil.WithRebootPolicy("Invalid"),
	)
	err := k8sClient.Create(ctx, pool)
	if err == nil {
		_ = k8sClient.Delete(ctx, pool)
	}
	g.Expect(err).To(MatchError(apierrors.IsInvalid, "IsInvalid"))
}

func TestBootcNodeEnumValidation(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := testutil.NewNode("invalid-image-state", testImageDigestRefA)
	node.Spec.DesiredImageState = "Invalid"
	err := k8sClient.Create(ctx, node)
	if err == nil {
		_ = k8sClient.Delete(ctx, node)
	}
	g.Expect(err).To(MatchError(apierrors.IsInvalid, "IsInvalid"))
}

func TestBootcNodePoolMinLengthValidation(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	pool := testutil.NewPool("empty-image-ref", "", testutil.WithWorkerSelector())
	err := k8sClient.Create(ctx, pool)
	if err == nil {
		_ = k8sClient.Delete(ctx, pool)
	}
	g.Expect(err).To(MatchError(apierrors.IsInvalid, "IsInvalid"))
}

func TestBootcNodeMinLengthValidation(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	node := testutil.NewNode("empty-desired-image", "")
	err := k8sClient.Create(ctx, node)
	if err == nil {
		_ = k8sClient.Delete(ctx, node)
	}
	g.Expect(err).To(MatchError(apierrors.IsInvalid, "IsInvalid"))
}
