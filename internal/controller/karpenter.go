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
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

// Karpenter integration constants. The controller checks for a NodeClaim owner
// reference and manages karpenter.sh/do-not-repair and
// karpenter.sh/do-not-disrupt to prevent Karpenter from replacing or
// voluntarily disrupting a node while it is being updated.
// See docs/src/operations/karpenter.md.
const (
	karpenterDoNotRepairAnnotationKey  = "karpenter.sh/do-not-repair"
	karpenterDoNotDisruptAnnotationKey = "karpenter.sh/do-not-disrupt"
	karpenterRebootingTaintKey         = "karpenter.sh/rebooting"
	karpenterDisruptedTaintKey         = "karpenter.sh/disrupted"
	karpenterRebootingTaintEffect      = corev1.TaintEffectNoSchedule
	karpenterNodeClaimAPIVersion       = "karpenter.sh/v1"
	karpenterNodeClaimKind             = "NodeClaim"

	// karpenterMissingClaimRequeue is how long driveRollout waits before
	// retrying when a Karpenter-owned node's NodeClaim is unexpectedly
	// absent. The NodeClaim may be mid-deletion; waiting avoids treating a
	// node under Karpenter termination as safe to update.
	karpenterMissingClaimRequeue = 30 * time.Second
)

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
		if ref.APIVersion == karpenterNodeClaimAPIVersion && ref.Kind == karpenterNodeClaimKind {
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
) (*unstructured.Unstructured, karpenterNodeClaimResult, error) {
	for _, ref := range node.OwnerReferences {
		if ref.APIVersion != karpenterNodeClaimAPIVersion || ref.Kind != karpenterNodeClaimKind {
			continue
		}
		claim := &unstructured.Unstructured{}
		claim.SetAPIVersion(karpenterNodeClaimAPIVersion)
		claim.SetKind(karpenterNodeClaimKind)
		if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, claim); err != nil {
			if apimeta.IsNoMatchError(err) {
				// CRD not installed — Karpenter is not present on this cluster.
				return nil, karpenterClaimCRDMissing, nil
			}
			if apierrors.IsNotFound(err) {
				// Owner ref points to a NodeClaim that no longer exists.
				// This is unsafe: Karpenter may be replacing or terminating
				// the node right now.
				return nil, karpenterClaimAbsent, nil
			}
			return nil, karpenterClaimError, err
		}
		return claim, karpenterClaimFound, nil
	}
	return nil, karpenterClaimNoRef, nil
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
// It also handles the case where the NodeClaim owner reference is present but
// the object is absent (mid-deletion); in that case it returns
// (true, true, nil, nil) to signal "defer without error". The second return
// value is true when the NodeClaim is missing and the caller should arrange a
// requeue rather than continuing immediately.
//
// When active is false and a NodeClaim was successfully fetched, claim is
// non-nil. Callers that need the NodeClaim (e.g. assignRebootSlot) can reuse
// it to avoid a second API round-trip.
func (r *BootcNodePoolReconciler) isKarpenterDisruptionActive(
	ctx context.Context,
	node *corev1.Node,
) (active bool, claimMissing bool, claim *unstructured.Unstructured, err error) {
	// A deleting Node is already being terminated — always defer.
	if node.DeletionTimestamp != nil {
		return true, false, nil, nil
	}

	// Fast path: check Node taints before making any API calls.
	for _, taint := range node.Spec.Taints {
		if taint.Effect != karpenterRebootingTaintEffect {
			continue
		}
		if taint.Key == karpenterRebootingTaintKey || taint.Key == karpenterDisruptedTaintKey {
			return true, false, nil, nil
		}
	}

	claim, result, err := r.getKarpenterNodeClaim(ctx, node)
	if err != nil {
		return false, false, nil, err
	}
	switch result {
	case karpenterClaimNoRef, karpenterClaimCRDMissing:
		// Not a Karpenter node, or Karpenter not installed.
		return false, false, nil, nil
	case karpenterClaimAbsent:
		// NodeClaim owner ref exists but object is missing. This is unsafe:
		// Karpenter may be mid-termination. Signal the caller to requeue.
		return true, true, nil, nil
	}

	// The NodeClaim exists. Check for a pending deletion first.
	if claim.GetDeletionTimestamp() != nil {
		return true, false, nil, nil
	}

	// Check NodeClaim status conditions.
	conditions, found, err := unstructured.NestedSlice(claim.Object, "status", "conditions")
	if err != nil || !found {
		return false, false, nil, err
	}
	for _, item := range conditions {
		condition, ok := item.(map[string]interface{})
		if !ok || condition["status"] != "True" {
			continue
		}
		switch condition["type"] {
		case "Rebooting", "DisruptionReason", "InstanceTerminating":
			return true, false, nil, nil
		}
	}
	return false, false, claim, nil
}

// ensureKarpenterStagingProtection applies Karpenter do-not-repair and
// do-not-disrupt annotations to a Karpenter-owned node that has entered the
// Staging state but has not yet been assigned a reboot slot. This extends the
// protection window to cover the full update lifecycle (image pull → staged →
// reboot → healthy), rather than starting only at slot assignment.
//
// It reuses the same ownership-tracking annotations as assignRebootSlot, so
// restoreNodeState/freeRebootSlot will clean them up at slot free time.
func (r *BootcNodePoolReconciler) ensureKarpenterStagingProtection(
	ctx context.Context,
	bn *bootcv1alpha1.BootcNode,
	node *corev1.Node,
) error {
	if !isNodeOwnedByKarpenterNodeClaim(node) {
		return nil
	}
	log := logf.FromContext(ctx)

	claim, claimResult, err := r.getKarpenterNodeClaim(ctx, node)
	if err != nil {
		return fmt.Errorf("getting NodeClaim for staging protection: %w", err)
	}
	claimAvailable := claimResult == karpenterClaimFound

	// Build desired BootcNode state.
	modifiedBN := bn.DeepCopy()
	if node.Annotations[karpenterDoNotRepairAnnotationKey] != "true" {
		metav1.SetMetaDataAnnotation(
			&modifiedBN.ObjectMeta,
			bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair,
			"true",
		)
	}
	if node.Annotations[karpenterDoNotDisruptAnnotationKey] != "true" {
		metav1.SetMetaDataAnnotation(
			&modifiedBN.ObjectMeta,
			bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt,
			"true",
		)
	}
	if claimAvailable {
		claimAnnotations := claim.GetAnnotations()
		if claimAnnotations[karpenterDoNotRepairAnnotationKey] != "true" {
			metav1.SetMetaDataAnnotation(
				&modifiedBN.ObjectMeta,
				bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotRepair,
				"true",
			)
		}
		if claimAnnotations[karpenterDoNotDisruptAnnotationKey] != "true" {
			metav1.SetMetaDataAnnotation(
				&modifiedBN.ObjectMeta,
				bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt,
				"true",
			)
		}
	}

	if !annotationsEqual(bn.Annotations, modifiedBN.Annotations) {
		log.Info("Applying Karpenter staging protection", "node", bn.Name)
		if err := r.Patch(ctx, modifiedBN, client.MergeFrom(bn)); err != nil {
			return fmt.Errorf("annotating BootcNode for staging protection: %w", err)
		}
		*bn = *modifiedBN
	}

	// Apply fence to NodeClaim.
	if claimAvailable {
		claimAnnotations := claim.GetAnnotations()
		modifiedClaimAnnotations := map[string]string{}
		maps.Copy(modifiedClaimAnnotations, claimAnnotations)
		if claimAnnotations[karpenterDoNotRepairAnnotationKey] != "true" {
			modifiedClaimAnnotations[karpenterDoNotRepairAnnotationKey] = "true"
		}
		if claimAnnotations[karpenterDoNotDisruptAnnotationKey] != "true" {
			modifiedClaimAnnotations[karpenterDoNotDisruptAnnotationKey] = "true"
		}
		if !annotationsEqual(claimAnnotations, modifiedClaimAnnotations) {
			modifiedClaim := claim.DeepCopy()
			modifiedClaim.SetAnnotations(modifiedClaimAnnotations)
			if err := r.Patch(ctx, modifiedClaim, client.MergeFrom(claim)); err != nil {
				return fmt.Errorf("patching NodeClaim for staging protection: %w", err)
			}
		}
	}

	// Apply fence to Node.
	modifiedNode := node.DeepCopy()
	if node.Annotations[karpenterDoNotRepairAnnotationKey] != "true" {
		log.Info("Setting karpenter.sh/do-not-repair on staging node", "node", node.Name)
		metav1.SetMetaDataAnnotation(&modifiedNode.ObjectMeta, karpenterDoNotRepairAnnotationKey, "true")
	}
	if node.Annotations[karpenterDoNotDisruptAnnotationKey] != "true" {
		log.Info("Setting karpenter.sh/do-not-disrupt on staging node", "node", node.Name)
		metav1.SetMetaDataAnnotation(&modifiedNode.ObjectMeta, karpenterDoNotDisruptAnnotationKey, "true")
	}
	if !annotationsEqual(node.Annotations, modifiedNode.Annotations) {
		if err := r.Patch(ctx, modifiedNode, client.StrategicMergeFrom(node)); err != nil {
			return fmt.Errorf("patching node for staging protection: %w", err)
		}
	}

	return nil
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
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
