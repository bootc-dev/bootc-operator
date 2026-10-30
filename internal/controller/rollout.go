// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/kubectl/pkg/drain"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/internal/image"
	karpenter "github.com/bootc-dev/bootc-operator/internal/karpenter"
)

// Hardcode to 2 for now; might make this configurable later, or dynamically
// adjusted. The rationale is that 1 unhealthy node might be a one-off. But 2
// unhealthy nodes becomes a pattern that might indicate a bad image. Now, this
// _probably_ should scale with number of nodes somehow. E.g. a 500 node
// cluster could tolerate more unhealthy nodes. Just starting conservative here
// and we can adjust. See
// https://github.com/bootc-dev/bootc-operator/issues/99.
const unhealthySlotHaltThreshold = 2

// rolloutState holds the classified BootcNodes for a single reconcile
// pass.
type rolloutState struct {
	// nodes are sorted into these buckets
	upToDate     []*bootcv1alpha1.BootcNode
	pending      []*bootcv1alpha1.BootcNode
	staging      []*bootcv1alpha1.BootcNode
	staged       []*bootcv1alpha1.BootcNode
	rebooting    []*bootcv1alpha1.BootcNode
	degraded     []*bootcv1alpha1.BootcNode
	unclassified []*bootcv1alpha1.BootcNode

	// rebootTimedOutNodes are the subset of degraded nodes that got there
	// by exceeding the reboot timeout (as opposed to a daemon-reported
	// error). They booted the old digest, so findUnhealthySlots can't
	// recognize them by digest and relies on this list instead.
	rebootTimedOutNodes []*bootcv1alpha1.BootcNode

	// BootcNodes with the in-reboot-slot annotation
	occupiedSlots int

	// requeueAfter, when non-zero, asks the reconciler to requeue the
	// pool after this duration. Used for time-based transitions like the
	// reboot timeout, which fire without any incoming event.
	requeueAfter time.Duration
}

// nodeCount returns the total number of nodes in the pool, including
// unclassified ones. Used for resolving percentage-based maxUnavailable.
func (rs *rolloutState) nodeCount() int {
	return len(rs.upToDate) + len(rs.pending) + len(rs.staging) + len(rs.staged) +
		len(rs.rebooting) + len(rs.degraded) + len(rs.unclassified)
}

// setRequeueAfter schedules a requeue after d unless something sooner is
// already set.
func (rs *rolloutState) setRequeueAfter(d time.Duration) {
	rs.requeueAfter = earlierRequeue(rs.requeueAfter, d)
}

// driveRollout is the main function that advances the rollout state machine.
func (r *BootcNodePoolReconciler) driveRollout(
	ctx context.Context,
	pool *bootcv1alpha1.BootcNodePool,
	ownedBootcNodes map[string]*bootcv1alpha1.BootcNode,
) (*rolloutState, error) {
	log := logf.FromContext(ctx)

	// Process drain results first. This isn't really ordering dependent,
	// but it feels natural to do this upfront before classifying.
	if err := r.collectDrainResults(ctx, pool, ownedBootcNodes); err != nil {
		return nil, fmt.Errorf("collecting drain results: %w", err)
	}

	rs := buildRolloutState(log, ownedBootcNodes)

	// Detect nodes stuck in the Rebooting state past the configured
	// timeout (e.g. they failed to boot the new image or never rejoined
	// the cluster) and reclassify them as degraded. The daemon can't
	// self-report while the node is down, so the controller surfaces this
	// at the pool level. requeueAfter drives the timeout to fire without
	// any further Node event.
	rs.requeueAfter = applyRebootTimeouts(rs, resolveRebootTimeout(pool), time.Now())

	// Flag degraded nodes at the pool level.
	if len(rs.degraded) > 0 {
		names := nodeNames(rs.degraded)
		slices.Sort(names)
		setPoolDegraded(pool, bootcv1alpha1.PoolNodeDegraded,
			fmt.Sprintf("Degraded nodes: %s", strings.Join(names, ", ")))
	}

	// Free reboot slots for nodes that have successfully rebooted into
	// the desired image. This runs before computing available slots so
	// that freed capacity is immediately usable for new candidates.
	if err := r.freeCompletedSlots(ctx, rs); err != nil {
		return nil, fmt.Errorf("freeing completed slots: %w", err)
	}

	// Check for unhealthy nodes on the target digest in reboot slots. If
	// 2+ are unhealthy, halt the rollout. Note that freeCompletedSlots()
	// already removed Ready nodes, so any upToDate node still in a slot is
	// implied to be NotReady.
	unhealthy := findUnhealthySlots(rs, pool.Status.TargetDigest)

	rolloutHalted := len(unhealthy) >= unhealthySlotHaltThreshold
	if rolloutHalted {
		details := make([]string, len(unhealthy))
		for i, u := range unhealthy {
			details[i] = u.name + ": " + u.reason
		}
		slices.Sort(details)
		setPoolDegraded(
			pool,
			bootcv1alpha1.PoolRolloutHalted,
			fmt.Sprintf(
				"Rollout halted: 2+ unhealthy nodes in reboot slots (%s)",
				strings.Join(details, ", "),
			),
		)
		log.Info("Rollout halted: 2+ unhealthy nodes in reboot slots",
			"unhealthyInSlots", len(unhealthy))

		// Return early to block new slot assignments and new/restarted
		// drains. Note that collectDrainResults() already ran above,
		// so drains that were in-flight and completed between
		// reconciles will still have their results collected and
		// desiredImageState set to Booted. Trying to "un-drain" and
		// uncordon fully drained nodes is out of scope for now.
		return rs, nil
	}

	maxUnavail, err := resolveMaxUnavailable(pool, rs.nodeCount())
	if err != nil {
		return nil, err
	}

	availableSlots := max(0, maxUnavail-rs.occupiedSlots)
	candidates := selectDrainCandidates(rs.staged, availableSlots)

	log.V(1).Info("Rollout state",
		"upToDate", len(rs.upToDate),
		"pending", len(rs.pending),
		"staging", len(rs.staging),
		"staged", len(rs.staged),
		"rebooting", len(rs.rebooting),
		"degraded", len(rs.degraded),
		"unclassified", nodeNames(rs.unclassified),
		"occupiedSlots", rs.occupiedSlots,
		"maxUnavailable", maxUnavail,
		"availableSlots", availableSlots,
		"candidates", nodeNames(candidates),
	)

	// Assign reboot slots to candidates.
	// assignRebootSlot patches the BootcNode, so the resulting object event
	// triggers the next reconciliation. No timed requeue is needed here.
	for _, bn := range candidates {
		var node corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: bn.Name}, &node); err != nil {
			return nil, fmt.Errorf("fetching node %s: %w", bn.Name, err)
		}
		// isKarpenterDisruptionActive fetches the NodeClaim as part of its
		// disruption checks. When disruption is not active, it returns the
		// claim so assignRebootSlot can reuse it without a second API call.
		kds, err := r.isKarpenterDisruptionActive(ctx, &node)
		if err != nil {
			return nil, fmt.Errorf("checking Karpenter disruption for %s: %w", bn.Name, err)
		}
		if kds.claimMissing {
			log.Info(
				"Deferring reboot slot: Karpenter NodeClaim is unexpectedly absent, will retry",
				"node",
				bn.Name,
			)
			rs.setRequeueAfter(karpenterMissingClaimRequeue)
			continue
		}
		if kds.active {
			log.Info("Deferring reboot slot: Karpenter disruption is active", "node", bn.Name)
			continue
		}
		if err := r.assignRebootSlot(ctx, bn, &node, kds.claim); err != nil {
			return nil, fmt.Errorf("assigning reboot slot to %s: %w", bn.Name, err)
		}
		// Periodic requeue fallback: now that a slot is assigned to a
		// Karpenter-owned node, ensure we poll for NodeClaim status changes
		// even when the NodeClaim watch is not registered.
		if isNodeOwnedByKarpenterNodeClaim(&node) {
			rs.setRequeueAfter(karpenterMissingClaimRequeue)
		}
	}

	// Periodic requeue fallback for already-slotted Karpenter-owned nodes:
	// the annotation written by assignRebootSlot persists across reconciles,
	// so we can detect Karpenter ownership without an extra API call.
	for _, bn := range rs.staged {
		if metav1.HasAnnotation(bn.ObjectMeta, bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair) {
			rs.setRequeueAfter(karpenterMissingClaimRequeue)
			break
		}
	}

	// Start drains for all slotted Staged nodes that haven't already been
	// approved for reboot (which implies they've already been drained).
	// ensureDrain starts an asynchronous drain; completion sends an explicit
	// event through drainCh. requeueAfter is therefore reserved for timeout
	// and polling fallbacks, rather than normal drain progress.
	for _, bn := range rs.staged {
		if !metav1.HasAnnotation(bn.ObjectMeta, bootcv1alpha1.AnnotationInRebootSlot) {
			continue
		}
		if bn.Spec.DesiredImageState == bootcv1alpha1.DesiredImageStateBooted {
			continue
		}
		var node corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: bn.Name}, &node); err != nil {
			return nil, fmt.Errorf("fetching node %s: %w", bn.Name, err)
		}
		kds, err := r.isKarpenterDisruptionActive(ctx, &node)
		if err != nil {
			return nil, fmt.Errorf("checking Karpenter disruption for %s: %w", bn.Name, err)
		}
		if kds.claimMissing {
			log.Info("Deferring drain: Karpenter NodeClaim is unexpectedly absent, will retry",
				"node", bn.Name)
			rs.setRequeueAfter(karpenterMissingClaimRequeue)
			continue
		}
		if kds.active {
			log.Info("Deferring drain: Karpenter disruption is active", "node", bn.Name)
			continue
		}
		r.ensureDrain(ctx, pool, bn)
	}

	return rs, nil
}

// assignRebootSlot marks a BootcNode as occupying a reboot slot and cordons
// the corresponding K8s Node. It sets the in-reboot-slot annotation on the
// BootcNode and records the prior cordon state in the was-cordoned annotation.
// For nodes owned by a Karpenter NodeClaim, it also sets the
// karpenter.sh/do-not-repair and karpenter.sh/do-not-disrupt annotations on
// the K8s Node to prevent Karpenter from replacing or disrupting the node
// during the update. Ownership of those annotations is tracked on the BootcNode
// so that restoreNodeState can remove only the ones the controller added.
//
// Note: these annotations are best-effort coordination signals, not a
// distributed lock. Karpenter can commit a disruption or reboot between the
// isKarpenterDisruptionActive check and the start of the drain.
//
// All operations are idempotent.
//
// prefetchedClaim may be the NodeClaim already fetched by a preceding
// isKarpenterDisruptionActive call. When non-nil and the node is
// Karpenter-owned, it is used directly to avoid a redundant API round-trip.
func (r *BootcNodePoolReconciler) assignRebootSlot(
	ctx context.Context,
	bn *bootcv1alpha1.BootcNode,
	node *corev1.Node,
	prefetchedClaim *karpenter.NodeClaim,
) error {
	log := logf.FromContext(ctx)

	karpenterOwned := isNodeOwnedByKarpenterNodeClaim(node)

	// Resolve the NodeClaim. Use the caller-supplied prefetched claim when
	// available to avoid a second API round-trip. If no claim was pre-fetched
	// (e.g. the node is not Karpenter-owned or the call site doesn't supply
	// one), fall back to fetching it here.
	//
	// claim may be nil even when karpenterOwned is true:
	//   - The NodeClaim CRD is not installed (Karpenter is not present on this
	//     cluster, so the API server returns a no-match error).
	//   - The NodeClaim is absent (karpenterClaimAbsent) — callers must have
	//     already deferred via isKarpenterDisruptionActive before reaching here.
	// In both cases we skip NodeClaim annotation management and proceed with
	// only Node-level fencing.
	var claim *karpenter.NodeClaim
	if karpenterOwned {
		if prefetchedClaim != nil {
			claim = prefetchedClaim
		} else {
			var claimResult karpenterNodeClaimResult
			var err error
			claim, claimResult, err = r.getKarpenterNodeClaim(ctx, node)
			if err != nil {
				return fmt.Errorf("getting NodeClaim: %w", err)
			}
			if claimResult == karpenterClaimAbsent {
				return fmt.Errorf("NodeClaim for node %s disappeared unexpectedly", node.Name)
			}
		}
	}
	// Build the desired BootcNode annotation state and patch only if anything
	// changed. We apply every annotation the slot requires unconditionally and
	// let the comparison at the end decide whether an API call is needed. This
	// avoids a separate upfront guard expression that would duplicate the same
	// per-annotation conditions.
	modifiedBN := bn.DeepCopy()
	modifiedNode := node.DeepCopy()
	var modifiedClaim *karpenter.NodeClaim
	if claim != nil {
		modifiedClaim = claim.DeepCopy()
	}

	// Mark the node as occupying a reboot slot.
	metav1.SetMetaDataAnnotation(&modifiedBN.ObjectMeta, bootcv1alpha1.AnnotationInRebootSlot, "")

	// Record the prior cordon state so restoreNodeState can leave the node as
	// it was found (i.e. not uncordon a node that was already cordoned). Only
	// set this on first assignment — if the slot annotation is already present
	// we must not overwrite the original recorded value.
	if !metav1.HasAnnotation(bn.ObjectMeta, bootcv1alpha1.AnnotationInRebootSlot) {
		wasCordoned := "false"

		if node.Spec.Unschedulable {
			wasCordoned = "true"
		}

		metav1.SetMetaDataAnnotation(
			&modifiedBN.ObjectMeta,
			bootcv1alpha1.AnnotationWasCordoned,
			wasCordoned,
		)
	}

	// For Karpenter-managed nodes, record ownership of the do-not-repair and
	// do-not-disrupt annotations on the Node and NodeClaim. We take ownership
	// when the annotation is absent or not already set to "true". Karpenter
	// only honors the literal value "true"; any other value (e.g. "false") is
	// ineffective, so we overwrite it and track ownership so we clean it up.
	if karpenterOwned {
		ensureOwnedAnnotation(
			modifiedBN,
			modifiedNode,
			bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair,
			karpenter.DoNotRepairAnnotationKey,
			"true",
		)
		ensureOwnedAnnotation(
			modifiedBN,
			modifiedNode,
			bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt,
			karpenter.DoNotDisruptAnnotationKey,
			"true",
		)

		if claim != nil {
			ensureOwnedAnnotation(
				modifiedBN,
				modifiedClaim,
				bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotRepair,
				karpenter.DoNotRepairAnnotationKey,
				"true",
			)
			ensureOwnedAnnotation(
				modifiedBN,
				modifiedClaim,
				bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt,
				karpenter.DoNotDisruptAnnotationKey,
				"true",
			)
		}
	}

	if !annotationsEqual(bn.Annotations, modifiedBN.Annotations) {
		if !metav1.HasAnnotation(bn.ObjectMeta, bootcv1alpha1.AnnotationInRebootSlot) {
			log.Info("Assigning reboot slot", "node", bn.Name)
		} else {
			log.Info("Updating reboot slot annotations", "node", bn.Name)
		}
		if err := r.Patch(ctx, modifiedBN, client.MergeFrom(bn)); err != nil {
			return fmt.Errorf("annotating BootcNode: %w", err)
		}
	}

	// Apply the Karpenter fence to the NodeClaim after recording ownership on
	// the BootcNode. This ordering prevents a successful NodeClaim patch from
	// becoming orphaned if the durable BootcNode update fails.
	//
	// NodeClaim annotations are a supplemental signal, not a distributed lock.
	// Karpenter can already have selected or queued a disruption by the time
	// these annotations are written; the Node-level checks (taint, deletion
	// timestamp, disruption conditions) are the authoritative guard.
	if claim != nil {
		if !annotationsEqual(claim.GetAnnotations(), modifiedClaim.GetAnnotations()) {
			if err := r.Patch(
				ctx,
				&modifiedClaim.Unstructured,
				client.MergeFrom(&claim.Unstructured),
			); err != nil {
				return fmt.Errorf("patching Karpenter NodeClaim: %w", err)
			}
		}
	}

	// Cordon the K8s Node and set the Node-level Karpenter annotations.
	// Skip the patch entirely when nothing needs to change.
	if !node.Spec.Unschedulable {
		log.Info("Cordoning node", "node", node.Name)
		modifiedNode.Spec.Unschedulable = true
	}

	if modifiedNode.Spec.Unschedulable != node.Spec.Unschedulable ||
		!annotationsEqual(node.Annotations, modifiedNode.Annotations) {
		if err := r.Patch(ctx, modifiedNode, client.StrategicMergeFrom(node)); err != nil {
			return fmt.Errorf("patching node: %w", err)
		}
		if karpenterOwned {
			if node.Annotations[karpenter.DoNotRepairAnnotationKey] != "true" {
				log.Info("Set karpenter.sh/do-not-repair on node", "node", node.Name)
			}
			if node.Annotations[karpenter.DoNotDisruptAnnotationKey] != "true" {
				log.Info("Set karpenter.sh/do-not-disrupt on node", "node", node.Name)
			}
		}
	}

	return nil
}

// freeCompletedSlots releases reboot slots for upToDate nodes that are Ready,
// and removes the os-not-ready startup taint from nodes that are UpToDate and
// Ready but never held a reboot slot (e.g. nodes whose booted image already
// matched the target when they joined the pool).
func (r *BootcNodePoolReconciler) freeCompletedSlots(ctx context.Context, rs *rolloutState) error {
	log := logf.FromContext(ctx)

	for _, bn := range rs.upToDate {
		inSlot := metav1.HasAnnotation(bn.ObjectMeta, bootcv1alpha1.AnnotationInRebootSlot)
		ownsTaint := metav1.HasAnnotation(
			bn.ObjectMeta,
			bootcv1alpha1.AnnotationOwnsOsNotReadyTaint,
		)

		if !inSlot && !ownsTaint {
			continue
		}

		var node corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: bn.Name}, &node); err != nil {
			return fmt.Errorf("fetching node %s: %w", bn.Name, err)
		}

		if nodeReadyStatus(&node) != corev1.ConditionTrue {
			if inSlot {
				log.V(1).
					Info("Node rebooted but not yet Ready, holding reboot slot", "node", bn.Name)
			}
			continue
		}

		if inSlot {
			log.Info("Freeing reboot slot: node healthy and Ready", "node", bn.Name)
			// Reuse the NodeClaim fetched by the disruption check when the
			// node is Karpenter-owned. This avoids fetching it again while
			// restoring NodeClaim annotations.
			kds, err := r.isKarpenterDisruptionActive(ctx, &node)
			if err != nil {
				return fmt.Errorf("checking Karpenter disruption for %s: %w", bn.Name, err)
			}
			if kds.claimMissing {
				log.Info(
					"Deferring slot release: Karpenter NodeClaim absent, will retry",
					"node",
					bn.Name,
				)
				rs.setRequeueAfter(karpenterMissingClaimRequeue)
				continue
			}
			if kds.active {
				log.Info("Deferring slot release: Karpenter disruption active", "node", bn.Name)
				continue
			}
			if err := r.freeRebootSlot(ctx, bn, &node, kds.claim); err != nil {
				return fmt.Errorf("freeing reboot slot for %s: %w", bn.Name, err)
			}
			rs.occupiedSlots--
			continue
		}

		if ownsTaint {
			// Node was already running the target image when it joined; it
			// never needed a reboot slot. Remove only the startup taint and
			// clear the ownership annotation.
			if err := r.removeOsNotReadyTaintAndAnnotation(ctx, bn, &node); err != nil {
				return fmt.Errorf("removing os-not-ready taint for %s: %w", bn.Name, err)
			}
		}
	}

	return nil
}

// freeRebootSlot releases a node's reboot slot by restoring its prior cordon
// state and removing annotations from the BootcNode.
func (r *BootcNodePoolReconciler) freeRebootSlot(
	ctx context.Context,
	bn *bootcv1alpha1.BootcNode,
	node *corev1.Node,
	prefetchedClaim *karpenter.NodeClaim,
) error {
	if err := r.restoreNodeState(ctx, bn, node, prefetchedClaim); err != nil {
		return err
	}

	// Remove all reboot slot annotations from the BootcNode, including the
	// Karpenter and os-not-ready ownership trackers if present.
	modified := bn.DeepCopy()
	delete(modified.Annotations, bootcv1alpha1.AnnotationOwnsKarpenterDoNotRepair)
	delete(modified.Annotations, bootcv1alpha1.AnnotationOwnsKarpenterDoNotDisrupt)
	delete(modified.Annotations, bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotRepair)
	delete(modified.Annotations, bootcv1alpha1.AnnotationOwnsKarpenterNodeClaimDoNotDisrupt)
	delete(modified.Annotations, bootcv1alpha1.AnnotationOwnsOsNotReadyTaint)
	delete(modified.Annotations, bootcv1alpha1.AnnotationWasCordoned)
	delete(modified.Annotations, bootcv1alpha1.AnnotationInRebootSlot)
	if err := r.Patch(ctx, modified, client.MergeFrom(bn)); err != nil {
		return fmt.Errorf("removing reboot slot annotations: %w", err)
	}
	*bn = *modified
	return nil
}

// ensureDrain checks whether a drain goroutine is already running for
// the given node and starts one if not. It is a no-op if a drain is
// already in progress.
func (r *BootcNodePoolReconciler) ensureDrain(
	ctx context.Context,
	pool *bootcv1alpha1.BootcNodePool,
	bn *bootcv1alpha1.BootcNode,
) {
	log := logf.FromContext(ctx)

	r.drainsMu.Lock()
	defer r.drainsMu.Unlock()

	if _, exists := r.drains[bn.Name]; exists {
		// Drain already in progress (or completed and pending collection).
		return
	}

	log.Info("Starting drain", "node", bn.Name)

	// cancel is stored in r.drains and called when the drain is collected.
	drainCtx, cancel := context.WithCancel(ctx) //nolint:gosec // G118
	status := &drainStatus{
		result:    make(chan error, 1),
		ctx:       drainCtx,
		cancel:    cancel,
		startTime: time.Now(),
	}
	r.drains[bn.Name] = status

	var drainTimeout time.Duration // 0 means no timeout
	if pool.Spec.Rollout != nil && pool.Spec.Rollout.DrainTimeoutSeconds != nil {
		drainTimeout = time.Duration(*pool.Spec.Rollout.DrainTimeoutSeconds) * time.Second
	}

	drainLog := log.WithValues("node", bn.Name)

	go func() {
		drainer := &drain.Helper{
			Client:               r.KubeClient,
			Ctx:                  drainCtx,
			Timeout:              drainTimeout,
			EvictErrorRetryDelay: 5 * time.Second,
			Force:                true,
			IgnoreAllDaemonSets:  true,
			DeleteEmptyDirData:   true,
			GracePeriodSeconds:   -1,
			Out:                  newDrainOutWriter(drainLog, bn.Name),
			ErrOut:               newDrainErrWriter(drainLog, bn.Name),
		}
		status.result <- drain.RunNodeDrain(drainer, bn.Name)

		// Re-enqueue the owning pool so the reconciler picks up the
		// drain result.
		r.drainCh <- event.GenericEvent{
			Object: &bootcv1alpha1.BootcNodePool{
				ObjectMeta: metav1.ObjectMeta{Name: pool.Name},
			},
		}
	}()
}

// collectDrainResults checks all in-progress drains for completed
// results. On success, it sets desiredImageState to Booted on the
// BootcNode.
func (r *BootcNodePoolReconciler) collectDrainResults(
	ctx context.Context,
	pool *bootcv1alpha1.BootcNodePool,
	ownedBootcNodes map[string]*bootcv1alpha1.BootcNode,
) error {
	log := logf.FromContext(ctx)

	r.drainsMu.Lock()
	defer r.drainsMu.Unlock()

	for nodeName, ds := range r.drains {
		// Non-blocking check for drain result.
		var drainErr error
		select {
		case drainErr = <-ds.result:
		default:
			// Drain still in progress.
			continue
		}

		// Drain finished; remove from the map and release its context,
		// which would otherwise stay registered with the parent.
		delete(r.drains, nodeName)
		cancelled := ds.ctx.Err() != nil
		ds.cancel()

		if drainErr != nil {
			if cancelled {
				// Context was cancelled (node left pool or controller
				// shutdown). The canceller is responsible for cleanup.
				log.V(1).Info("Drain cancelled", "node", nodeName)
			} else {
				// Real drain error (e.g. PDB timeout). The node keeps
				// its reboot slot and stays cordoned. It will be
				// retried naturally: the entry is deleted from the map,
				// selectDrainCandidates picks the still-slotted Staged
				// node, and ensureDrain starts a new goroutine.
				log.Info("Drain failed, will retry", "node", nodeName, "error", drainErr)
				if bn, ok := ownedBootcNodes[nodeName]; ok {
					r.recordDrainFailedEvent(pool, bn, drainErr)
				}
			}
			continue
		}

		// Drain succeeded. Set desiredImageState to Booted.
		bn, ok := ownedBootcNodes[nodeName]
		if !ok {
			// Node left the pool while draining. Once
			// removeBootcNode cancels the drain (see related TODO
			// there), this would normally be caught by the
			// drainErr cancellation check above. But if somehow we
			// raced and did actually successfully drain, just
			// ignore it; we should've already uncordoned the node.
			log.Info("Drain completed but node no longer in pool", "node", nodeName)
			continue
		}

		log.Info("Drain completed, setting desiredImageState to Booted", "node", nodeName)
		modified := bn.DeepCopy()
		modified.Spec.DesiredImageState = bootcv1alpha1.DesiredImageStateBooted
		if err := r.Patch(ctx, modified, client.MergeFrom(bn)); err != nil {
			// The drain result was already consumed, so on retry a
			// redundant drain will run (completing instantly).
			// Could optimize this but meh... not worth the
			// complexity.
			return fmt.Errorf("setting desiredImageState on %s: %w", nodeName, err)
		}
		*bn = *modified
	}

	return nil
}

// buildRolloutState classifies all owned BootcNodes and counts occupied
// reboot slots.
func buildRolloutState(
	log logr.Logger,
	ownedBootcNodes map[string]*bootcv1alpha1.BootcNode,
) *rolloutState {
	rs := &rolloutState{}
	for _, bn := range ownedBootcNodes {
		// Count occupied reboot slots from the persistent annotation.
		if metav1.HasAnnotation(bn.ObjectMeta, bootcv1alpha1.AnnotationInRebootSlot) {
			rs.occupiedSlots++
		}

		state, err := classifyNode(bn)
		if err != nil {
			// as mentioned in classifyNode(), should never happen...
			log.Info("WARNING: skipping unclassifiable node", "node", bn.Name, "error", err)
			rs.unclassified = append(rs.unclassified, bn)
			continue
		}
		log.V(1).Info("Classified node", "node", bn.Name, "state", state.String())

		switch state {
		case nodeStateUpToDate:
			rs.upToDate = append(rs.upToDate, bn)
		case nodeStatePending:
			rs.pending = append(rs.pending, bn)
		case nodeStateStaging:
			rs.staging = append(rs.staging, bn)
		case nodeStateStaged:
			rs.staged = append(rs.staged, bn)
		case nodeStateRebooting:
			rs.rebooting = append(rs.rebooting, bn)
		case nodeStateDegraded:
			if cond := apimeta.FindStatusCondition(
				bn.Status.Conditions,
				bootcv1alpha1.NodeDegraded,
			); cond != nil {
				log.Info("Node is degraded", "node", bn.Name, "message", cond.Message)
			}
			rs.degraded = append(rs.degraded, bn)
		}
	}
	return rs
}

// unhealthySlot identifies a node in a reboot slot that is unhealthy.
type unhealthySlot struct {
	name   string
	reason string // "Degraded" or "NotReady"
}

// findUnhealthySlots returns nodes occupying reboot slots that are unhealthy.
// Two categories qualify:
//   - Degraded nodes (marked by the daemon itself) that booted the target
//     digest (the image itself may be bad).
//   - UpToDate nodes still holding a slot after freeCompletedSlots (not
//     Ready). I.e. the daemon doesn't see anything wrong, but there's something
//     wrong preventing the kubelet from working correctly.
func findUnhealthySlots(rs *rolloutState, targetDigest string) []unhealthySlot {
	var result []unhealthySlot
	for _, bn := range rs.degraded {
		if !metav1.HasAnnotation(bn.ObjectMeta, bootcv1alpha1.AnnotationInRebootSlot) {
			continue
		}
		// Note we count nil Booted as unhealthy here; this really
		// shouldn't happen because clearly the daemon came up at least
		// once in this node's history to be able to get to a reboot
		// slot. And so Booted should always be set here.
		if bn.Status.Booted == nil || image.InfoMatchesDigest(bn.Status.Booted, targetDigest) {
			result = append(result, unhealthySlot{name: bn.Name, reason: "Degraded"})
		}
	}
	for _, bn := range rs.upToDate {
		if metav1.HasAnnotation(bn.ObjectMeta, bootcv1alpha1.AnnotationInRebootSlot) {
			// Still has annotation after freeCompletedSlots → not Ready.
			result = append(result, unhealthySlot{name: bn.Name, reason: "NotReady"})
		}
	}
	// Reboot-timed-out nodes booted the old digest (they never came back),
	// so the digest check above skips them. Count them here: a node that
	// failed to boot the target is strong evidence the image is bad, which
	// is exactly what the halt is meant to catch.
	for _, bn := range rs.rebootTimedOutNodes {
		if metav1.HasAnnotation(bn.ObjectMeta, bootcv1alpha1.AnnotationInRebootSlot) {
			result = append(result, unhealthySlot{name: bn.Name, reason: "RebootTimeout"})
		}
	}
	return result
}

// resolveMaxUnavailable computes the effective maxUnavailable value from the
// pool's rollout spec. Defaults to 1 when unset. A value of 0 is allowed and
// means no reboot slots are available (effectively paused). Returns an
// invalidSpecError if the value is malformed.
func resolveMaxUnavailable(pool *bootcv1alpha1.BootcNodePool, nodeCount int) (int, error) {
	if pool.Spec.Rollout != nil && pool.Spec.Rollout.Paused {
		return 0, nil
	}
	if pool.Spec.Rollout == nil || pool.Spec.Rollout.MaxUnavailable == nil {
		return 1, nil
	}

	// We roundUp here; this matches Deployments maxUnavailable for example
	v, err := intstr.GetScaledValueFromIntOrPercent(
		pool.Spec.Rollout.MaxUnavailable,
		nodeCount,
		true,
	)
	if err != nil {
		return 0, newInvalidSpecError(
			fmt.Sprintf(
				"invalid maxUnavailable %q: %v",
				pool.Spec.Rollout.MaxUnavailable.String(),
				err,
			),
		)
	}
	return v, nil
}

// selectDrainCandidates picks Staged nodes that need the drain flow started or
// restarted. Nodes that already have the in-reboot-slot annotation are always
// included (e.g. they had a slot before a controller restart and need their
// drain restarted). These nodes are already counted in occupiedSlots so they
// don't consume availableSlots. Beyond those, up to availableSlots unslotted
// nodes are appended, sorted alphabetically.
func selectDrainCandidates(
	staged []*bootcv1alpha1.BootcNode,
	availableSlots int,
) []*bootcv1alpha1.BootcNode {
	if len(staged) == 0 {
		return nil
	}

	// Partition into already-slotted vs new candidates.
	var slotted, unslotted []*bootcv1alpha1.BootcNode
	for _, bn := range staged {
		if metav1.HasAnnotation(bn.ObjectMeta, bootcv1alpha1.AnnotationInRebootSlot) {
			slotted = append(slotted, bn)
		} else {
			unslotted = append(unslotted, bn)
		}
	}
	slices.SortFunc(slotted, func(a, b *bootcv1alpha1.BootcNode) int {
		return strings.Compare(a.Name, b.Name)
	})
	slices.SortFunc(unslotted, func(a, b *bootcv1alpha1.BootcNode) int {
		return strings.Compare(a.Name, b.Name)
	})

	// Always re-select slotted nodes. Fill remaining capacity with
	// unslotted nodes.
	result := slotted
	if availableSlots > 0 && len(unslotted) > 0 {
		n := min(availableSlots, len(unslotted))
		result = append(result, unslotted[:n]...)
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

// nodeNames returns the names of the given BootcNodes for logging.
func nodeNames(nodes []*bootcv1alpha1.BootcNode) []string {
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names
}

// nodeState represents the effective state of a BootcNode as seen by
// the controller.
type nodeState int

const (
	// nodeStateUpToDate means the node is running the desired image.
	nodeStateUpToDate nodeState = iota

	// nodeStatePending means the node's state is indeterminate: the
	// daemon hasn't reported yet (no booted status), or hasn't reacted
	// to a desiredImage change.
	nodeStatePending

	// nodeStateStaging means the daemon is pulling/staging the image.
	nodeStateStaging

	// nodeStateStaged means the image is staged and locked, awaiting a
	// reboot slot.
	nodeStateStaged

	// nodeStateRebooting means a reboot is in progress.
	nodeStateRebooting

	// nodeStateDegraded means the daemon has reported an error via the
	// BootcNode Degraded condition.
	nodeStateDegraded
)

func (s nodeState) String() string {
	switch s {
	case nodeStateUpToDate:
		return "UpToDate"
	case nodeStatePending:
		return "Pending"
	case nodeStateStaging:
		return "Staging"
	case nodeStateStaged:
		return "Staged"
	case nodeStateRebooting:
		return "Rebooting"
	case nodeStateDegraded:
		return "Degraded"
	default:
		return fmt.Sprintf("Unknown(%d)", int(s))
	}
}

// resolveRebootTimeout returns the configured reboot timeout, or 0 if the
// timeout is disabled (unset).
func resolveRebootTimeout(pool *bootcv1alpha1.BootcNodePool) time.Duration {
	if pool.Spec.Rollout == nil || pool.Spec.Rollout.RebootTimeoutSeconds == nil {
		return 0
	}
	return time.Duration(*pool.Spec.Rollout.RebootTimeoutSeconds) * time.Second
}

// rebootTimedOut reports whether a BootcNode has been in the Rebooting
// state longer than timeout. A non-positive timeout disables the check.
func rebootTimedOut(bn *bootcv1alpha1.BootcNode, timeout time.Duration, now time.Time) bool {
	if timeout <= 0 {
		return false
	}
	idle := apimeta.FindStatusCondition(bn.Status.Conditions, bootcv1alpha1.NodeIdle)
	if idle == nil || idle.Reason != bootcv1alpha1.NodeReasonRebooting {
		return false
	}
	return now.Sub(idle.LastTransitionTime.Time) > timeout
}

// applyRebootTimeouts moves rebooting nodes that have exceeded the reboot
// timeout out of rs.rebooting and into rs.degraded, so the pool reports
// them as degraded (the daemon can't self-report while the node is down).
// It returns the duration after which the pool should be requeued to
// re-evaluate the soonest not-yet-expired rebooting node, or 0 if the
// timeout is disabled or no rebooting nodes remain.
func applyRebootTimeouts(rs *rolloutState, timeout time.Duration, now time.Time) time.Duration {
	if timeout <= 0 {
		return 0
	}

	remaining := rs.rebooting[:0]
	var requeue time.Duration
	for _, bn := range rs.rebooting {
		if rebootTimedOut(bn, timeout, now) {
			rs.degraded = append(rs.degraded, bn)
			rs.rebootTimedOutNodes = append(rs.rebootTimedOutNodes, bn)
			continue
		}
		remaining = append(remaining, bn)

		// Schedule a requeue for when this node would time out, so the
		// timeout fires even without a further Node event.
		idle := apimeta.FindStatusCondition(bn.Status.Conditions, bootcv1alpha1.NodeIdle)
		if idle == nil {
			continue
		}
		if left := timeout - now.Sub(idle.LastTransitionTime.Time); left > 0 &&
			(requeue == 0 || left < requeue) {
			requeue = left
		}
	}
	rs.rebooting = remaining
	return requeue
}

// classifyNode determines the effective state of a BootcNode.
func classifyNode(bn *bootcv1alpha1.BootcNode) (nodeState, error) {
	// Check Degraded first — takes priority over activity state.
	if apimeta.IsStatusConditionTrue(bn.Status.Conditions, bootcv1alpha1.NodeDegraded) {
		return nodeStateDegraded, nil
	}

	if bn.Status.Booted == nil {
		// The only way this can happen really is on a brand new
		// BootcNode and the daemon is still being provisioned. It
		// should resolve in a future reconciliation (though ideally
		// eventually we can handle 'stuck' states like this... see
		// related comment below).
		return nodeStatePending, nil
	}

	// We could pass in the pool here and use targetDigest instead to avoid
	// parsing. Though I do also like how this function takes just a BootcNode.
	// The errors here should never happen since we literally just synced those
	// specs and we didn't e.g. re-Get() them either.
	ref, err := parseImageRef(bn.Spec.DesiredImage)
	if err != nil {
		return 0, fmt.Errorf("unparseable desiredImage %q: %w", bn.Spec.DesiredImage, err)
	}
	digested, ok := ref.(reference.Digested)
	if !ok {
		return 0, fmt.Errorf("non-digested desiredImage %q", bn.Spec.DesiredImage)
	}

	if image.InfoMatchesDigest(bn.Status.Booted, digested.Digest().String()) {
		// Image matches; nothing for the controller to act on
		// regardless of whether the daemon has settled yet.
		return nodeStateUpToDate, nil
	}

	// OK, the node isn't yet booting the desired digest. Let's dig into where
	// it is in the update cycle.

	if apimeta.IsStatusConditionFalse(bn.Status.Conditions, bootcv1alpha1.NodeIdle) {
		idleCond := apimeta.FindStatusCondition(bn.Status.Conditions, bootcv1alpha1.NodeIdle)
		switch idleCond.Reason {
		case bootcv1alpha1.NodeReasonStaging:
			return nodeStateStaging, nil
		case bootcv1alpha1.NodeReasonStaged:
			return nodeStateStaged, nil
		case bootcv1alpha1.NodeReasonRebooting:
			return nodeStateRebooting, nil
		}
	}

	// Image doesn't match and daemon is either Idle, has no conditions, or
	// has an unrecognized Idle reason. This likely means we're racing with
	// the daemon reconciliation (it hasn't reacted to the spec change
	// yet), or something more broken is happening (e.g. daemon not running
	// at all). For now we don't try to detect 'stuck' nodes, but may in
	// the future. It'll still show up as holding up the pool's
	// `deployedDigest` field and the updatingCount stat.
	return nodeStatePending, nil
}
