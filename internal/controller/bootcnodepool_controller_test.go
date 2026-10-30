// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	karpenter "github.com/bootc-dev/bootc-operator/internal/karpenter"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
	"github.com/distribution/reference"
	. "github.com/onsi/gomega"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestIsKarpenterInstalled(t *testing.T) {
	// Verify that a discoverable NodeClaim enables the optional integration,
	// while an absent mapping is handled without returning an error.
	tests := []struct {
		name string
		gvk  *schema.GroupVersionKind
		want bool
	}{
		{
			name: "Resource is discoverable",
			gvk: &schema.GroupVersionKind{
				Group:   karpenter.Group,
				Version: "v1",
				Kind:    "NodeClaim",
			},
			want: true,
		},
		{
			name: "Resource is not discoverable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			var gv []schema.GroupVersion
			if tt.gvk != nil {
				gv = []schema.GroupVersion{{
					Group:   tt.gvk.Group,
					Version: tt.gvk.Version,
				}}
			}

			mapper := apimeta.NewDefaultRESTMapper(gv)
			if tt.gvk != nil {
				mapper.Add(*tt.gvk, apimeta.RESTScopeRoot)
			}

			got, err := isKarpenterInstalled(mapper)
			g.Expect(got).To(Equal(tt.want))
			g.Expect(err).NotTo(HaveOccurred())
		})
	}
}

func TestMapNodeClaimToPoolRequests(t *testing.T) {
	pool := testutil.NewPool("pool-a", testImageDigestRefA)
	pool.UID = types.UID("pool-a-uid")
	bootcNode := testutil.NewBootcNode("node-a", testImageDigestRefA,
		func(node *bootcv1alpha1.BootcNode) {
			node.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: bootcv1alpha1.GroupVersion.String(),
				Kind:       "BootcNodePool",
				Name:       pool.Name,
				UID:        pool.UID,
				Controller: ptr.To(true),
			}}
		},
	)

	tests := []struct {
		name    string
		object  client.Object
		objects []client.Object
		want    []reconcile.Request
	}{
		{
			name: "maps owned node to pool",
			object: testutil.NewKarpenterNodeClaim("claim-a",
				testutil.WithKarpenterNodeClaimNodeName("node-a")),
			objects: []client.Object{bootcNode},
			want:    []reconcile.Request{{NamespacedName: types.NamespacedName{Name: pool.Name}}},
		},
		{
			name:   "empty node name",
			object: testutil.NewKarpenterNodeClaim("claim-a"),
			want:   nil,
		},
		{
			name: "missing bootc node",
			object: testutil.NewKarpenterNodeClaim(
				"claim-a",
				testutil.WithKarpenterNodeClaimNodeName("node-a"),
			),
			want: nil,
		},
		{
			name:   "unexpected object",
			object: testutil.NewK8sNode("node-a", nil),
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			reconciler := &BootcNodePoolReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(scheme.Scheme).
					WithObjects(tt.objects...).
					Build(),
			}
			got := reconciler.mapNodeClaimToPoolRequests(context.Background(), tt.object)
			g.Expect(got).To(Equal(tt.want))
		})
	}
}

func TestParseImageRef(t *testing.T) {
	tests := []struct {
		name       string
		ref        string
		wantErr    bool
		wantDigest string
		wantTag    string
	}{
		{
			name:       "digest ref",
			ref:        testImageDigestRefA,
			wantDigest: testDigestA,
		},
		{
			name:       "digest ref with tag",
			ref:        "quay.io/example/myos:latest@" + testDigestA,
			wantDigest: testDigestA,
			wantTag:    "latest",
		},
		{
			name:    "tag ref",
			ref:     "quay.io/example/myos:latest",
			wantTag: "latest",
		},
		{
			name: "bare image",
			ref:  "quay.io/example/myos",
		},
		{
			name:    "empty string",
			ref:     "",
			wantErr: true,
		},
		{
			name:    "invalid characters",
			ref:     "INVALID:///ref",
			wantErr: true,
		},
		{
			name:    "short name",
			ref:     "myos:latest",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			ref, err := parseImageRef(tt.ref)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).NotTo(HaveOccurred())

			if tt.wantDigest != "" {
				digested, ok := ref.(reference.Digested)
				g.Expect(ok).To(BeTrue(), "expected digest in %q", tt.ref)
				g.Expect(digested.Digest().String()).To(Equal(tt.wantDigest))
			}
			if tt.wantTag != "" {
				tagged, ok := ref.(reference.Tagged)
				g.Expect(ok).To(BeTrue(), "expected tag in %q", tt.ref)
				g.Expect(tagged.Tag()).To(Equal(tt.wantTag))
			}
		})
	}
}
