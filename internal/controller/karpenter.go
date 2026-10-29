// SPDX-License-Identifier: Apache-2.0

package controller

// karpenter.go contains all Karpenter-specific controller logic: constants,
// types, and methods that coordinate bootc-operator node updates with Karpenter
// lifecycle management. The integration is designed to be optional — clusters
// without the Karpenter NodeClaim CRD continue to reconcile normally.
//
// See docs/src/operations/karpenter.md for a full description of the
// integration design, annotation semantics, and known limitations.

import (
	"context"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	karpenter "github.com/bootc-dev/bootc-operator/internal/karpenter"
)

// karpenterMissingClaimRequeue is how long the controller waits before retrying when a
// Karpenter-owned node's NodeClaim is unexpectedly absent. The NodeClaim may
// be mid-deletion; waiting avoids treating a node under Karpenter termination
// as safe to update.
const karpenterMissingClaimRequeue = 30 * time.Second

// karpenterNodeClaimResult is the outcome of a NodeClaim lookup.
type karpenterNodeClaimResult int

const (
	// karpenterClaimFound means the NodeClaim exists and was returned.
	karpenterClaimFound karpenterNodeClaimResult = iota
	// karpenterClaimAbsent means the node has a NodeClaim owner reference but
	// the object returned 404. The NodeClaim may be mid-deletion; callers
	// should treat this as unsafe and defer.
	karpenterClaimAbsent
	// karpenterClaimCRDMissing means the Karpenter NodeClaim CRD is not
	// installed on this cluster. Treat the node as non-Karpenter.
	karpenterClaimCRDMissing
	// karpenterClaimNoRef means the node has no Karpenter NodeClaim owner
	// reference at all.
	karpenterClaimNoRef
	// karpenterClaimError means the API call failed for a transient or
	// unexpected reason. The caller should propagate the accompanying error.
	karpenterClaimError
)

// isNodeOwnedByKarpenterNodeClaim reports whether the K8s Node has an owner
// reference pointing to a Karpenter NodeClaim (karpenter.sh/v1).
func isNodeOwnedByKarpenterNodeClaim(node *corev1.Node) bool {
	for _, ref := range node.OwnerReferences {
		if ref.APIVersion == karpenter.NodeClaimAPIVersion && ref.Kind == karpenter.NodeClaimKind {
			return true
		}
	}
	return false
}

// getKarpenterNodeClaim returns the NodeClaim owning node, if one is available.
// The unstructured type keeps Karpenter optional; clusters without its CRD
// continue to reconcile normally.
//
// The second return value distinguishes five cases:
//   - karpenterClaimFound: claim is non-nil, everything is normal.
//   - karpenterClaimAbsent: the node has a NodeClaim owner ref but the object
//     returned 404. This is unsafe — the NodeClaim may be mid-deletion.
//   - karpenterClaimCRDMissing: the Karpenter CRD is not installed; treat the
//     node as non-Karpenter.
//   - karpenterClaimNoRef: no Karpenter owner reference on this node.
//   - karpenterClaimError: a transient or unexpected API error; the
//     accompanying error value is non-nil and should be propagated.
func (r *BootcNodePoolReconciler) getKarpenterNodeClaim(
	ctx context.Context,
	node *corev1.Node,
) (*karpenter.NodeClaim, karpenterNodeClaimResult, error) {
	for _, ref := range node.OwnerReferences {
		if ref.APIVersion != karpenter.NodeClaimAPIVersion || ref.Kind != karpenter.NodeClaimKind {
			continue
		}
		claim := karpenter.NewNodeClaim()
		err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, claim)
		switch {
		case err == nil:
			return claim, karpenterClaimFound, nil
		case apimeta.IsNoMatchError(err):
			// CRD not installed — Karpenter is not present on this cluster.
			return nil, karpenterClaimCRDMissing, nil
		case apierrors.IsNotFound(err):
			// Owner ref points to a NodeClaim that no longer exists.
			// This is unsafe: Karpenter may be replacing or terminating
			// the node right now.
			return nil, karpenterClaimAbsent, nil
		default:
			return nil, karpenterClaimError, err
		}
	}
	return nil, karpenterClaimNoRef, nil
}

// karpenterDisruptionStatus is the result of isKarpenterDisruptionActive.
type karpenterDisruptionStatus struct {
	// active is true when Karpenter owns a lifecycle operation that conflicts
	// with a bootc-operator update. claimMissing implies active.
	active bool

	// claimMissing distinguishes an unexpectedly absent NodeClaim from other
	// active disruptions. It always implies active; callers should check it
	// before active and arrange a requeue rather than proceeding immediately.
	claimMissing bool

	// claim is the fetched NodeClaim. It is only set when active is false and
	// the node is Karpenter-owned. Callers can reuse it to avoid a second API
	// round-trip (e.g. assignRebootSlot).
	claim *karpenter.NodeClaim
}

// isKarpenterDisruptionActive reports whether Karpenter currently owns any
// lifecycle operation that would conflict with a bootc-operator update. It
// checks for:
//
//   - Node or NodeClaim deletion in progress (DeletionTimestamp set)
//   - Karpenter reboot taint on the Node
//   - Karpenter disrupted taint on the Node (voluntary disruption committed)
//   - NodeClaim status conditions: Rebooting, DisruptionReason, or
//     InstanceTerminating set to True
//
// When the NodeClaim owner reference is present but the object is absent
// (mid-deletion), Active and ClaimMissing are both true. Callers should
// arrange a requeue rather than continuing immediately.
//
// When Active is false and a NodeClaim was successfully fetched, Claim is
// non-nil. Callers that need the NodeClaim (e.g. assignRebootSlot) can reuse
// it to avoid a second API round-trip.
func (r *BootcNodePoolReconciler) isKarpenterDisruptionActive(
	ctx context.Context,
	node *corev1.Node,
) (karpenterDisruptionStatus, error) {
	// A deleting Node is already being terminated — always defer.
	if node.DeletionTimestamp != nil {
		return karpenterDisruptionStatus{active: true}, nil
	}

	// Fast path: check Node taints before making any API calls.
	for _, taint := range node.Spec.Taints {
		if taint.Key == karpenter.RebootingTaintKey &&
			taint.Effect == karpenter.RebootingTaintEffect {
			return karpenterDisruptionStatus{active: true}, nil
		}
		if taint.Key == karpenter.DisruptedTaintKey &&
			taint.Effect == karpenter.DisruptedTaintEffect {
			return karpenterDisruptionStatus{active: true}, nil
		}
	}

	claim, result, err := r.getKarpenterNodeClaim(ctx, node)
	if err != nil {
		return karpenterDisruptionStatus{}, err
	}
	switch result {
	case karpenterClaimNoRef, karpenterClaimCRDMissing:
		// Not a Karpenter node, or Karpenter not installed.
		return karpenterDisruptionStatus{}, nil
	case karpenterClaimAbsent:
		// NodeClaim owner ref exists but object is missing. This is unsafe:
		// Karpenter may be mid-termination. Signal the caller to requeue.
		return karpenterDisruptionStatus{active: true, claimMissing: true}, nil
	}

	// The NodeClaim exists. Check for a pending deletion first.
	if claim.GetDeletionTimestamp() != nil {
		return karpenterDisruptionStatus{active: true}, nil
	}

	// Check NodeClaim status conditions.
	for _, conditionType := range []string{
		karpenter.ConditionRebooting,
		karpenter.ConditionDisruptionReason,
		karpenter.ConditionInstanceTerminating,
	} {
		active, err := claim.ConditionIsTrue(conditionType)
		if err != nil {
			return karpenterDisruptionStatus{}, err
		}
		if active {
			return karpenterDisruptionStatus{active: true}, nil
		}
	}
	return karpenterDisruptionStatus{claim: claim}, nil
}

// annotationsEqual reports whether two annotation maps are identical.
// It treats a nil map and an empty map as equal, unlike maps.Equal which
// returns false when one argument is nil and the other is a non-nil empty map.
// Kubernetes can return either (a freshly created object with no annotations
// has nil; one that had all annotations deleted may have an empty map), so
// this distinction matters for correct annotation comparison.
func annotationsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	return len(a) == 0 || maps.Equal(a, b)
}

// ensureOwnedAnnotation sets targetKey on target and records ownership on
// ownerKey when target does not already have the requested value. It does not
// rewrite the owner when ownership is already recorded.
func ensureOwnedAnnotation(
	owner *bootcv1alpha1.BootcNode,
	target metav1.Object,
	ownerKey, targetKey, value string,
) bool {
	if target.GetAnnotations()[targetKey] == value {
		return false
	}

	if _, owned := owner.GetAnnotations()[ownerKey]; !owned {
		ownerAnnotations := maps.Clone(owner.GetAnnotations())
		if ownerAnnotations == nil {
			ownerAnnotations = map[string]string{}
		}
		ownerAnnotations[ownerKey] = "true"
		owner.SetAnnotations(ownerAnnotations)
	}

	targetAnnotations := maps.Clone(target.GetAnnotations())
	if targetAnnotations == nil {
		targetAnnotations = map[string]string{}
	}
	targetAnnotations[targetKey] = value
	target.SetAnnotations(targetAnnotations)
	return true
}

// removeOwnedAnnotation removes targetKey from target when owner has recorded
// that it owns the annotation (via ownerKey). It does not remove ownerKey from owner; callers are
// responsible for clearing all ownership annotations together (e.g. in
// freeRebootSlot). This intentional asymmetry with ensureOwnedAnnotation keeps
// the ownership cleanup atomic at the call site rather than scattered across
// individual annotation removals.
//
// The map index on a nil annotations map is safe in Go and returns ("", false).
func removeOwnedAnnotation(
	owner *bootcv1alpha1.BootcNode,
	target metav1.Object,
	ownerKey, targetKey string,
) bool {
	if _, ok := owner.GetAnnotations()[ownerKey]; !ok {
		return false
	}

	targetAnnotations := maps.Clone(target.GetAnnotations())
	if _, ok := targetAnnotations[targetKey]; !ok {
		return false
	}
	delete(targetAnnotations, targetKey)
	target.SetAnnotations(targetAnnotations)
	return true
}
