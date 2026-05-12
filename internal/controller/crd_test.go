/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"reflect"
	"testing"
	"time"

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
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("Failed to create BootcNodePool: %v", err)
	}
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, pool); client.IgnoreNotFound(err) != nil {
			t.Logf("cleanup: failed to delete pool: %v", err)
		}
	})

	// Retrieve and verify spec round-trips. We set all defaulted
	// fields explicitly (RebootPolicy), so the input and output specs
	// should match exactly.
	got := &bootcv1alpha1.BootcNodePool{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), got); err != nil {
		t.Fatalf("Failed to get BootcNodePool: %v", err)
	}
	if !reflect.DeepEqual(got.Spec, wantSpec) {
		t.Errorf("spec mismatch:\n  got:  %+v\n  want: %+v", got.Spec, wantSpec)
	}
}

func TestBootcNodeCRD(t *testing.T) {
	ctx := context.Background()

	node := testutil.NewNode("worker-1", testImageDigestRefA,
		testutil.WithNodePullSecret(testSecretName, testSecretNS, testSecretHash),
	)

	// Save the spec before Create, which mutates node in-place.
	wantSpec := *node.Spec.DeepCopy()

	// Create
	if err := k8sClient.Create(ctx, node); err != nil {
		t.Fatalf("Failed to create BootcNode: %v", err)
	}
	t.Cleanup(func() {
		if err := k8sClient.Delete(ctx, node); client.IgnoreNotFound(err) != nil {
			t.Logf("cleanup: failed to delete node: %v", err)
		}
	})

	// Retrieve and verify spec round-trips. BootcNodeSpec has no
	// defaulted fields, so the input and output should match exactly.
	got := &bootcv1alpha1.BootcNode{}
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(node), got); err != nil {
		t.Fatalf("Failed to get BootcNode: %v", err)
	}
	if !reflect.DeepEqual(got.Spec, wantSpec) {
		t.Errorf("spec mismatch:\n  got:  %+v\n  want: %+v", got.Spec, wantSpec)
	}

	// Update status. Use a fixed timestamp truncated to seconds to match the
	// precision the API server stores so we can just `DeepEqual` the whole thing.
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
	if err := k8sClient.Status().Update(ctx, got); err != nil {
		t.Fatalf("Failed to update BootcNode status: %v", err)
	}

	// Verify status round-trips
	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(node), got); err != nil {
		t.Fatalf("Failed to get BootcNode after status update: %v", err)
	}
	// Copy canonical timestamps from the server response so
	// DeepEqual ignores timezone/precision differences.
	for i := range wantStatus.Conditions {
		wantStatus.Conditions[i].LastTransitionTime = got.Status.Conditions[i].LastTransitionTime
	}
	if wantStatus.Booted != nil && wantStatus.Booted.Timestamp != nil {
		wantStatus.Booted.Timestamp = got.Status.Booted.Timestamp
	}
	if !reflect.DeepEqual(got.Status, wantStatus) {
		t.Errorf("status mismatch:\n  got:  %+v\n  want: %+v", got.Status, wantStatus)
	}

}

func TestBootcNodePoolEnumValidation(t *testing.T) {
	ctx := context.Background()

	pool := testutil.NewPool("invalid-reboot-policy", testImageTaggedRef,
		testutil.WithWorkerSelector(),
		testutil.WithRebootPolicy("Invalid"),
	)
	if err := k8sClient.Create(ctx, pool); err == nil {
		_ = k8sClient.Delete(ctx, pool)
		t.Fatal("Expected creation with invalid rebootPolicy to fail, but it succeeded")
	}
}

func TestBootcNodeEnumValidation(t *testing.T) {
	ctx := context.Background()

	node := testutil.NewNode("invalid-image-state", testImageDigestRefA)
	node.Spec.DesiredImageState = "Invalid"
	if err := k8sClient.Create(ctx, node); err == nil {
		_ = k8sClient.Delete(ctx, node)
		t.Fatal("Expected creation with invalid desiredImageState to fail, but it succeeded")
	}
}

func TestBootcNodePoolMinLengthValidation(t *testing.T) {
	ctx := context.Background()

	pool := testutil.NewPool("empty-image-ref", "", testutil.WithWorkerSelector())
	if err := k8sClient.Create(ctx, pool); err == nil {
		_ = k8sClient.Delete(ctx, pool)
		t.Fatal("Expected creation with empty image.ref to fail, but it succeeded")
	}
}

func TestBootcNodeMinLengthValidation(t *testing.T) {
	ctx := context.Background()

	node := testutil.NewNode("empty-desired-image", "")
	if err := k8sClient.Create(ctx, node); err == nil {
		_ = k8sClient.Delete(ctx, node)
		t.Fatal("Expected creation with empty desiredImage to fail, but it succeeded")
	}
}
