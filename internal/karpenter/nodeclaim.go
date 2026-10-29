// SPDX-License-Identifier: Apache-2.0

// Package karpenter provides types and constants for working with Karpenter
// NodeClaim objects without importing the Karpenter API module. Using
// unstructured keeps Karpenter optional, avoids adding a Karpenter API
// dependency or scheme registration to bootc-operator, and lets clusters
// without the NodeClaim CRD continue to work normally. The tradeoff is that
// the small set of NodeClaim fields used by the controller, such as
// status.nodeName and status.conditions, is checked at runtime rather than
// being enforced by Go types.
//
// See docs/src/operations/karpenter.md for the integration design.
package karpenter

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// Karpenter API identity constants.
const (
	Group               = "karpenter.sh"
	NodeClaimAPIVersion = Group + "/v1"
	NodeClaimKind       = "NodeClaim"
)

// Annotation keys managed by bootc-operator on Nodes and NodeClaims to
// coordinate with Karpenter lifecycle operations.
const (
	DoNotRepairAnnotationKey  = "karpenter.sh/do-not-repair"
	DoNotDisruptAnnotationKey = "karpenter.sh/do-not-disrupt"
)

// Taint keys and effects used by Karpenter during node lifecycle operations.
const (
	RebootingTaintKey    = "karpenter.sh/rebooting"
	RebootingTaintEffect = corev1.TaintEffectNoSchedule
	DisruptedTaintKey    = "karpenter.sh/disrupted"
	DisruptedTaintEffect = corev1.TaintEffectNoSchedule
)

// NodeClaim status condition types that indicate an active lifecycle operation
// incompatible with a bootc-operator update.
const (
	ConditionRebooting           = "Rebooting"
	ConditionDisruptionReason    = "DisruptionReason"
	ConditionInstanceTerminating = "InstanceTerminating"
)

// NodeClaim is the controller's representation of a Karpenter NodeClaim.
// Embedding Unstructured keeps Karpenter optional while keeping all
// Karpenter-specific field access behind this type. If Karpenter API types are
// imported in the future, this is the only type callers need to change.
type NodeClaim struct {
	unstructured.Unstructured
}

var (
	_ metav1.Object  = (*NodeClaim)(nil)
	_ runtime.Object = (*NodeClaim)(nil)
)

// NewNodeClaim returns a NodeClaim with APIVersion and Kind pre-set.
func NewNodeClaim() *NodeClaim {
	c := &NodeClaim{}
	c.SetAPIVersion(NodeClaimAPIVersion)
	c.SetKind(NodeClaimKind)
	return c
}

// NewNodeClaimFromUnstructured validates and wraps an unstructured NodeClaim.
func NewNodeClaimFromUnstructured(obj *unstructured.Unstructured) (*NodeClaim, error) {
	if obj == nil {
		return nil, fmt.Errorf("NodeClaim object is nil")
	}
	if obj.GetAPIVersion() != NodeClaimAPIVersion {
		return nil, fmt.Errorf(
			"unexpected NodeClaim apiVersion %q, want %q",
			obj.GetAPIVersion(),
			NodeClaimAPIVersion,
		)
	}
	if obj.GetKind() != NodeClaimKind {
		return nil, fmt.Errorf(
			"unexpected NodeClaim kind %q, want %q",
			obj.GetKind(),
			NodeClaimKind,
		)
	}
	return &NodeClaim{Unstructured: *obj}, nil
}

// NodeNameFromObject validates a typed or unstructured NodeClaim and returns
// its Kubernetes Node name and claim name.
func NodeNameFromObject(obj interface{}) (string, string, error) {
	claim, ok := obj.(*NodeClaim)
	if !ok {
		unstructuredClaim, isUnstructured := obj.(*unstructured.Unstructured)
		if !isUnstructured {
			return "", "", fmt.Errorf("unexpected NodeClaim object type %T", obj)
		}
		var err error
		claim, err = NewNodeClaimFromUnstructured(unstructuredClaim)
		if err != nil {
			return "", "", err
		}
	}
	nodeName, err := claim.NodeName()
	if err != nil {
		return "", claim.GetName(), err
	}
	return nodeName, claim.GetName(), nil
}

// UnstructuredObject returns the underlying object for dynamic clients and watches.
func (c *NodeClaim) UnstructuredObject() *unstructured.Unstructured {
	if c == nil {
		return nil
	}
	return &c.Unstructured
}

// DeepCopy preserves the wrapper type when a NodeClaim is copied for a patch.
func (c *NodeClaim) DeepCopy() *NodeClaim {
	if c == nil {
		return nil
	}
	return &NodeClaim{Unstructured: *c.Unstructured.DeepCopy()}
}

// DeepCopyObject preserves the wrapper type for runtime.Object callers.
func (c *NodeClaim) DeepCopyObject() runtime.Object {
	return c.DeepCopy()
}

// NodeName returns the name of the Kubernetes Node represented by this claim.
// An empty node name is valid when the field is present. It returns an error
// when status.nodeName is missing or has the wrong type.
func (c *NodeClaim) NodeName() (string, error) {
	nodeName, found, err := unstructured.NestedString(c.Object, "status", "nodeName")
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("status.nodeName not found")
	}
	return nodeName, nil
}

// ConditionIsTrue reports whether the specified NodeClaim status condition is
// currently True.
func (c *NodeClaim) ConditionIsTrue(conditionType string) (bool, error) {
	conditions, _, err := unstructured.NestedSlice(c.Object, "status", "conditions")
	if err != nil {
		return false, err
	}

	for _, item := range conditions {
		condition, ok := item.(map[string]interface{})
		if !ok || condition["status"] != "True" {
			continue
		}
		if condition["type"] == conditionType {
			return true, nil
		}
	}
	return false, nil
}
