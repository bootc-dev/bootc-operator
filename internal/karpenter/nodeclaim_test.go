// SPDX-License-Identifier: Apache-2.0

package karpenter_test

import (
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	karpenter "github.com/bootc-dev/bootc-operator/internal/karpenter"
)

func TestNodeClaim(t *testing.T) {
	g := NewWithT(t)
	claim := karpenter.NewNodeClaim()
	claim.Object["metadata"] = map[string]interface{}{"name": "claim"}
	claim.Object["status"] = map[string]interface{}{
		"nodeName": "node",
		"conditions": []interface{}{
			map[string]interface{}{"type": karpenter.ConditionDisruptionReason, "status": "False"},
			map[string]interface{}{"type": karpenter.ConditionRebooting, "status": "True"},
		},
	}

	g.Expect(claim.GetAPIVersion()).To(Equal(karpenter.NodeClaimAPIVersion))
	g.Expect(claim.GetKind()).To(Equal(karpenter.NodeClaimKind))
	nodeName, err := claim.NodeName()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(nodeName).To(Equal("node"))

	active, err := claim.ConditionIsTrue(karpenter.ConditionRebooting)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(active).To(BeTrue())
	active, err = claim.ConditionIsTrue(karpenter.ConditionDisruptionReason)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(active).To(BeFalse())
}

func TestNodeClaimUnstructuredConversion(t *testing.T) {
	g := NewWithT(t)
	claim, err := karpenter.NewNodeClaimFromUnstructured(
		karpenter.NewNodeClaim().UnstructuredObject(),
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(claim).NotTo(BeNil())

	badVersion := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "example.com/v1",
		"kind":       karpenter.NodeClaimKind,
	}}
	_, err = karpenter.NewNodeClaimFromUnstructured(badVersion)
	g.Expect(err).To(MatchError(ContainSubstring("apiVersion")))

	badKind := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": karpenter.NodeClaimAPIVersion,
		"kind":       "Other",
	}}
	_, err = karpenter.NewNodeClaimFromUnstructured(badKind)
	g.Expect(err).To(MatchError(ContainSubstring("kind")))
}

func TestNodeNameFromObject(t *testing.T) {
	g := NewWithT(t)
	claim := karpenter.NewNodeClaim()
	claim.SetName("claim")
	claim.Object["status"] = map[string]interface{}{"nodeName": "node"}
	nodeName, claimName, err := karpenter.NodeNameFromObject(claim)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(nodeName).To(Equal("node"))
	g.Expect(claimName).To(Equal("claim"))
}

func TestNodeClaimDeepCopy(t *testing.T) {
	g := NewWithT(t)
	claim := karpenter.NewNodeClaim()
	claim.SetName("claim")
	claim.SetAnnotations(map[string]string{"example.com/annotation": "original"})

	copied := claim.DeepCopy()
	copied.SetAnnotations(map[string]string{"example.com/annotation": "copy"})

	g.Expect(copied).NotTo(BeIdenticalTo(claim))
	g.Expect(copied.GetName()).To(Equal(claim.GetName()))
	g.Expect(claim.GetAnnotations()).To(HaveKeyWithValue("example.com/annotation", "original"))
	g.Expect(copied.GetAnnotations()).To(HaveKeyWithValue("example.com/annotation", "copy"))
	g.Expect(copied.DeepCopyObject()).To(BeAssignableToTypeOf(&karpenter.NodeClaim{}))
}

func TestNodeClaimStatusErrors(t *testing.T) {
	tests := []struct {
		name             string
		status           map[string]interface{}
		wantNodeName     string
		wantNodeNameErr  bool
		wantConditionErr bool
	}{
		{name: "missing node name", status: map[string]interface{}{}, wantNodeNameErr: true},
		{name: "empty node name", status: map[string]interface{}{"nodeName": ""}},
		{
			name:            "invalid node name",
			status:          map[string]interface{}{"nodeName": 42},
			wantNodeNameErr: true,
		},
		{
			name:             "invalid conditions",
			status:           map[string]interface{}{"nodeName": "node", "conditions": "invalid"},
			wantNodeName:     "node",
			wantConditionErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			claim := karpenter.NewNodeClaim()
			claim.Object["status"] = tt.status

			nodeName, err := claim.NodeName()
			g.Expect(nodeName).To(Equal(tt.wantNodeName))
			if tt.wantNodeNameErr {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}

			_, err = claim.ConditionIsTrue(karpenter.ConditionRebooting)
			if tt.wantConditionErr {
				g.Expect(err).To(HaveOccurred())
			} else {
				g.Expect(err).NotTo(HaveOccurred())
			}
		})
	}
}
