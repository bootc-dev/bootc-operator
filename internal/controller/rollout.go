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
)

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

	// BootcNodes with the in-reboot-slot annotation
	occupiedSlots int
}

// nodeCount returns the total number of nodes in the pool, including
// unclassified ones. Used for resolving percentage-based maxUnavailable.
func (rs *rolloutState) nodeCount() int {
	return len(rs.upToDate) + len(rs.pending) + len(rs.staging) + len(rs.staged) +
		len(rs.rebooting) + len(rs.degraded) + len(rs.unclassified)
}

// driveRollout is the main function that advances the rollout state machine.
func (r *BootcNodePoolReconciler) driveRollout(ctx context.Context, pool *bootcv1alpha1.BootcNodePool, ownedBootcNodes map[string]*bootcv1alpha1.BootcNode) error {
	log := logf.FromContext(ctx)

	// Process drain results first. This isn't really ordering dependent,
	// but it feels natural to do this upfront before classifying.
	if err := r.collectDrainResults(ctx, ownedBootcNodes); err != nil {
		return fmt.Errorf("collecting drain results: %w", err)
	}

	rs := buildRolloutState(log, ownedBootcNodes)

	// Flag degraded nodes at the pool level. NodeConflict (set during
	// membership sync) takes priority, so we only set NodeDegraded if
	// the pool isn't already degraded for another reason.
	syncNodeDegradedCondition(pool, rs.degraded)

	// Free reboot slots for nodes that have successfully rebooted into
	// the desired image. This runs before computing available slots so
	// that freed capacity is immediately usable for new candidates.
	if err := r.freeCompletedSlots(ctx, rs); err != nil {
		return fmt.Errorf("freeing completed slots: %w", err)
	}

	maxUnavail, err := resolveMaxUnavailable(pool, rs.nodeCount())
	if err != nil {
		return err
	}

	avail := max(0, maxUnavail-rs.occupiedSlots)
	candidates := selectDrainCandidates(rs.staged, avail)

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
		"availableSlots", avail,
		"candidates", nodeNames(candidates),
	)

	// Assign reboot slots to candidates.
	for _, bn := range candidates {
		var node corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: bn.Name}, &node); err != nil {
			return fmt.Errorf("fetching node %s: %w", bn.Name, err)
		}
		if err := r.assignRebootSlot(ctx, bn, &node); err != nil {
			return fmt.Errorf("assigning reboot slot to %s: %w", bn.Name, err)
		}
	}

	// Start drains for all slotted Staged nodes that haven't already been
	// approved for reboot (which implies they've already been drained).
	for _, bn := range rs.staged {
		if !metav1.HasAnnotation(bn.ObjectMeta, bootcv1alpha1.AnnotationInRebootSlot) {
			continue
		}
		if bn.Spec.DesiredImageState == bootcv1alpha1.DesiredImageStateBooted {
			continue
		}
		r.ensureDrain(ctx, pool, bn)
	}

	return nil
}

// assignRebootSlot marks a BootcNode as occupying a reboot slot and
// cordons the corresponding K8s Node. It sets the in-reboot-slot
// annotation on the BootcNode, records prior cordon state in the
// was-cordoned annotation, and cordons the node. All operations are
// idempotent.
func (r *BootcNodePoolReconciler) assignRebootSlot(ctx context.Context, bn *bootcv1alpha1.BootcNode, node *corev1.Node) error {
	log := logf.FromContext(ctx)

	// Set annotations on the BootcNode if not already present.
	if !metav1.HasAnnotation(bn.ObjectMeta, bootcv1alpha1.AnnotationInRebootSlot) {
		log.Info("Assigning reboot slot", "node", bn.Name)
		modifiedBN := bn.DeepCopy()
		if modifiedBN.Annotations == nil {
			modifiedBN.Annotations = map[string]string{}
		}
		modifiedBN.Annotations[bootcv1alpha1.AnnotationInRebootSlot] = ""
		// Record whether the node was already cordoned before us.
		if node.Spec.Unschedulable {
			modifiedBN.Annotations[bootcv1alpha1.AnnotationWasCordoned] = "true"
		} else {
			modifiedBN.Annotations[bootcv1alpha1.AnnotationWasCordoned] = "false"
		}
		if err := r.Patch(ctx, modifiedBN, client.MergeFrom(bn)); err != nil {
			return fmt.Errorf("annotating BootcNode: %w", err)
		}
		*bn = *modifiedBN
	}

	// Cordon the K8s Node if not already cordoned.
	if !node.Spec.Unschedulable {
		log.Info("Cordoning node", "node", node.Name)
		modifiedNode := node.DeepCopy()
		modifiedNode.Spec.Unschedulable = true
		if err := r.Patch(ctx, modifiedNode, client.StrategicMergeFrom(node)); err != nil {
			return fmt.Errorf("cordoning node: %w", err)
		}
		*node = *modifiedNode
	}

	return nil
}

// freeCompletedSlots releases reboot slots for upToDate nodes that are Ready.
// It decrements rs.occupiedSlots for each freed slot so the caller's available
// slot count is accurate.
func (r *BootcNodePoolReconciler) freeCompletedSlots(ctx context.Context, rs *rolloutState) error {
	log := logf.FromContext(ctx)

	for _, bn := range rs.upToDate {
		if !metav1.HasAnnotation(bn.ObjectMeta, bootcv1alpha1.AnnotationInRebootSlot) {
			continue
		}

		var node corev1.Node
		if err := r.Get(ctx, types.NamespacedName{Name: bn.Name}, &node); err != nil {
			return fmt.Errorf("fetching node %s: %w", bn.Name, err)
		}

		if nodeReadyStatus(&node) != corev1.ConditionTrue {
			log.V(1).Info("Node rebooted but not yet Ready, holding reboot slot", "node", bn.Name)
			continue
		}

		log.Info("Freeing reboot slot: node healthy and Ready", "node", bn.Name)
		if err := r.freeRebootSlot(ctx, bn, &node); err != nil {
			return fmt.Errorf("freeing reboot slot for %s: %w", bn.Name, err)
		}
		rs.occupiedSlots--
	}

	return nil
}

// freeRebootSlot releases a node's reboot slot by restoring its prior cordon
// state and removing annotations from the BootcNode.
func (r *BootcNodePoolReconciler) freeRebootSlot(ctx context.Context, bn *bootcv1alpha1.BootcNode, node *corev1.Node) error {
	if err := r.restoreCordonState(ctx, bn, node); err != nil {
		return err
	}

	// Remove both reboot slot annotations from the BootcNode.
	modified := bn.DeepCopy()
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
func (r *BootcNodePoolReconciler) ensureDrain(ctx context.Context, pool *bootcv1alpha1.BootcNodePool, bn *bootcv1alpha1.BootcNode) {
	log := logf.FromContext(ctx)

	r.drainsMu.Lock()
	defer r.drainsMu.Unlock()

	if _, exists := r.drains[bn.Name]; exists {
		// Drain already in progress (or completed and pending collection).
		return
	}

	log.Info("Starting drain", "node", bn.Name)

	drainCtx, cancel := context.WithCancel(ctx)
	status := &drainStatus{
		result:    make(chan error, 1),
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
func (r *BootcNodePoolReconciler) collectDrainResults(ctx context.Context, ownedBootcNodes map[string]*bootcv1alpha1.BootcNode) error {
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

		// Drain finished; remove from the map.
		delete(r.drains, nodeName)

		if drainErr != nil {
			// TODO: handle drain errors and cancellations.
			log.Info("Drain failed", "node", nodeName, "error", drainErr)
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
func buildRolloutState(log logr.Logger, ownedBootcNodes map[string]*bootcv1alpha1.BootcNode) *rolloutState {
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
			if cond := apimeta.FindStatusCondition(bn.Status.Conditions, bootcv1alpha1.NodeDegraded); cond != nil {
				log.Info("Node is degraded", "node", bn.Name, "message", cond.Message)
			}
			rs.degraded = append(rs.degraded, bn)
		}
	}
	return rs
}

// syncNodeDegradedCondition sets Degraded/NodeDegraded on the pool if any
// nodes are degraded. It respects priority: if the pool is already degraded
// for another reason (e.g. NodeConflict), it does not overwrite it.
// XXX(jl): Or... should this be a new condition type so it can be surfaced in
// parallel? Let's see how this approach feels and iterate.
func syncNodeDegradedCondition(pool *bootcv1alpha1.BootcNodePool, degraded []*bootcv1alpha1.BootcNode) {
	if len(degraded) == 0 {
		return
	}
	if apimeta.IsStatusConditionTrue(pool.Status.Conditions, bootcv1alpha1.PoolDegraded) {
		return
	}
	names := nodeNames(degraded)
	// Sort so the message is stable across reconciles.
	slices.Sort(names)
	apimeta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:    bootcv1alpha1.PoolDegraded,
		Status:  metav1.ConditionTrue,
		Reason:  bootcv1alpha1.PoolNodeDegraded,
		Message: fmt.Sprintf("Degraded nodes: %s", strings.Join(names, ", ")),
	})
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
	v, err := intstr.GetScaledValueFromIntOrPercent(pool.Spec.Rollout.MaxUnavailable, nodeCount, true)
	if err != nil {
		return 0, newInvalidSpecError(fmt.Sprintf("invalid maxUnavailable %q: %v", pool.Spec.Rollout.MaxUnavailable.String(), err))
	}
	return v, nil
}

// selectDrainCandidates picks Staged nodes that need the drain flow started or
// restarted. Nodes that already have the in-reboot-slot annotation are always
// included (e.g. they had a slot before a controller restart and need their
// drain restarted). These nodes are already counted in occupiedSlots so they
// don't consume availableSlots. Beyond those, up to availableSlots unslotted
// nodes are appended, sorted alphabetically.
func selectDrainCandidates(staged []*bootcv1alpha1.BootcNode, availableSlots int) []*bootcv1alpha1.BootcNode {
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

	if digested.Digest().String() == bn.Status.Booted.ImageDigest {
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
