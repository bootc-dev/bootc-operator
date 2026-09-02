// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

const (
	eventReasonImageUpdateAvailable = "ImageUpdateAvailable"
	eventReasonRolloutStarted       = "RolloutStarted"
	eventReasonRolloutCompleted     = "RolloutCompleted"
	eventReasonDrainFailed          = "DrainFailed"
	eventReasonDrainTakingTooLong   = "DrainTakingTooLong"

	eventActionResolveImage = "ResolveImage"
	eventActionRollout      = "Rollout"
	eventActionPoolDegraded = "PoolDegraded"
	eventActionNodeUpdate   = "NodeUpdate"
	eventActionDrain        = "Drain"

	drainStallThreshold = 5 * time.Minute
	eventNoteLimit      = 1024
	eventNoteSuffix     = "..."
)

// updatePoolStatus writes a changed pool status and then records events for
// meaningful transitions. Events are supplemental: a failed status write
// returns before any event is emitted.
func (r *BootcNodePoolReconciler) updatePoolStatus(
	ctx context.Context,
	pool *bootcv1alpha1.BootcNodePool,
	previous *bootcv1alpha1.BootcNodePoolStatus,
	tagTargetChanged bool,
) error {
	if reflect.DeepEqual(pool.Status, *previous) {
		return nil
	}
	if err := r.Status().Update(ctx, pool); err != nil {
		return err
	}

	r.recordPoolEvents(pool, previous, tagTargetChanged)
	return nil
}

func (r *BootcNodePoolReconciler) recordPoolEvents(
	pool *bootcv1alpha1.BootcNodePool,
	previous *bootcv1alpha1.BootcNodePoolStatus,
	tagTargetChanged bool,
) {
	if tagTargetChanged {
		r.recordEventf(
			pool,
			nil,
			corev1.EventTypeNormal,
			eventReasonImageUpdateAvailable,
			eventActionResolveImage,
			"Image tag %s resolved to new digest %s (previously %s)",
			pool.Spec.Image.Ref,
			pool.Status.TargetDigest,
			previous.TargetDigest,
		)
	}

	oldUpToDate := apimeta.FindStatusCondition(previous.Conditions, bootcv1alpha1.PoolUpToDate)
	newUpToDate := apimeta.FindStatusCondition(pool.Status.Conditions, bootcv1alpha1.PoolUpToDate)
	targetChanged := previous.TargetDigest != "" &&
		previous.TargetDigest != pool.Status.TargetDigest &&
		pool.Status.TargetDigest != ""
	if conditionEnteredReason(
		oldUpToDate,
		newUpToDate,
		metav1.ConditionFalse,
		bootcv1alpha1.PoolRolloutInProgress,
	) || (targetChanged && newUpToDate != nil &&
		newUpToDate.Status == metav1.ConditionFalse &&
		newUpToDate.Reason == bootcv1alpha1.PoolRolloutInProgress) {
		r.recordEventf(
			pool,
			nil,
			corev1.EventTypeNormal,
			eventReasonRolloutStarted,
			eventActionRollout,
			"Rollout started toward digest %s",
			pool.Status.TargetDigest,
		)
	}
	if oldUpToDate != nil &&
		oldUpToDate.Status != metav1.ConditionTrue &&
		newUpToDate != nil &&
		newUpToDate.Status == metav1.ConditionTrue {
		r.recordEventf(
			pool,
			nil,
			corev1.EventTypeNormal,
			eventReasonRolloutCompleted,
			eventActionRollout,
			"Rollout completed at digest %s",
			pool.Status.TargetDigest,
		)
	}

	oldDegraded := apimeta.FindStatusCondition(previous.Conditions, bootcv1alpha1.PoolDegraded)
	newDegraded := apimeta.FindStatusCondition(pool.Status.Conditions, bootcv1alpha1.PoolDegraded)
	if degradedConditionChanged(oldDegraded, newDegraded) {
		r.recordEventf(
			pool,
			nil,
			corev1.EventTypeWarning,
			newDegraded.Reason,
			eventActionPoolDegraded,
			"%s",
			newDegraded.Message,
		)
	}
}

func conditionEnteredReason(
	previous, current *metav1.Condition,
	status metav1.ConditionStatus,
	reason string,
) bool {
	if current == nil || current.Status != status || current.Reason != reason {
		return false
	}
	return previous == nil || previous.Status != status || previous.Reason != reason
}

func degradedConditionChanged(previous, current *metav1.Condition) bool {
	if current == nil || current.Status != metav1.ConditionTrue {
		return false
	}
	return previous == nil ||
		previous.Status != metav1.ConditionTrue ||
		previous.Reason != current.Reason ||
		previous.Message != current.Message
}

// recordNodeEvents emits an event once for each observed Staging, Staged, or
// Rebooting condition. A controller-owned annotation persists the last
// observation so unrelated reconciles and controller restarts do not repeat
// events. Annotation write failures are logged but never block a rollout.
func (r *BootcNodePoolReconciler) recordNodeEvents(
	ctx context.Context,
	pool *bootcv1alpha1.BootcNodePool,
	nodes map[string]*bootcv1alpha1.BootcNode,
) {
	log := logf.FromContext(ctx)
	for _, node := range nodes {
		observation, reason, note := nodeEvent(node)
		if observation == "" ||
			node.Annotations[bootcv1alpha1.AnnotationLastObservedState] == observation {
			continue
		}

		if reason != "" {
			r.recordEventf(
				node,
				pool,
				corev1.EventTypeNormal,
				reason,
				eventActionNodeUpdate,
				"%s",
				note,
			)
		}

		modified := node.DeepCopy()
		if modified.Annotations == nil {
			modified.Annotations = map[string]string{}
		}
		modified.Annotations[bootcv1alpha1.AnnotationLastObservedState] = observation
		if err := r.Patch(ctx, modified, client.MergeFrom(node)); err != nil {
			// Emit first so a transient marker write failure cannot permanently
			// hide the transition. A retry may aggregate the same event into an
			// EventSeries, which is preferable to losing it.
			log.Error(err, "Failed to persist last observed node state", "node", node.Name)
			continue
		}
		*node = *modified
	}
}

func nodeEvent(node *bootcv1alpha1.BootcNode) (observation, reason, note string) {
	idle := apimeta.FindStatusCondition(node.Status.Conditions, bootcv1alpha1.NodeIdle)
	if idle == nil {
		return "", "", ""
	}

	observation = fmt.Sprintf("%s:%s", idle.Status, idle.Reason)
	if idle.Status != metav1.ConditionFalse {
		return observation, "", ""
	}

	observationWithImage := observation + ":" + node.Spec.DesiredImage
	switch idle.Reason {
	case bootcv1alpha1.NodeReasonStaging:
		return observationWithImage,
			bootcv1alpha1.NodeReasonStaging,
			fmt.Sprintf("Staging image %s", node.Spec.DesiredImage)
	case bootcv1alpha1.NodeReasonStaged:
		return observationWithImage,
			bootcv1alpha1.NodeReasonStaged,
			fmt.Sprintf("Image %s is staged and awaiting reboot", node.Spec.DesiredImage)
	case bootcv1alpha1.NodeReasonRebooting:
		return observationWithImage,
			bootcv1alpha1.NodeReasonRebooting,
			fmt.Sprintf("Rebooting into image %s", node.Spec.DesiredImage)
	default:
		return observation, "", ""
	}
}

func (r *BootcNodePoolReconciler) recordDrainFailedEvent(
	pool *bootcv1alpha1.BootcNodePool,
	node *bootcv1alpha1.BootcNode,
	err error,
) {
	r.recordEventf(
		node,
		pool,
		corev1.EventTypeWarning,
		eventReasonDrainFailed,
		eventActionDrain,
		"Failed to drain node: %v; the drain will be retried",
		err,
	)
}

// recordDrainStalls emits one warning per drain that crosses the stall
// threshold and returns when the next active drain should be checked. The
// existing in-memory drain state is sufficient because drains are restarted
// after a controller restart.
func (r *BootcNodePoolReconciler) recordDrainStalls(
	pool *bootcv1alpha1.BootcNodePool,
	nodes map[string]*bootcv1alpha1.BootcNode,
) time.Duration {
	now := time.Now()
	var stalledNodes []*bootcv1alpha1.BootcNode
	var nextCheck time.Duration

	r.drainsMu.Lock()
	for nodeName, status := range r.drains {
		if status.isStalled {
			continue
		}

		remaining := drainStallThreshold - now.Sub(status.startTime)
		if remaining > 0 {
			nextCheck = earlierRequeue(nextCheck, remaining)
			continue
		}

		status.isStalled = true
		if node, ok := nodes[nodeName]; ok {
			stalledNodes = append(stalledNodes, node)
		}
	}
	r.drainsMu.Unlock()

	for _, node := range stalledNodes {
		r.recordEventf(
			node,
			pool,
			corev1.EventTypeWarning,
			eventReasonDrainTakingTooLong,
			eventActionDrain,
			"Drain has been running for more than %s; it may be blocked by a PodDisruptionBudget",
			drainStallThreshold,
		)
	}

	return nextCheck
}

func earlierRequeue(current, candidate time.Duration) time.Duration {
	if candidate <= 0 {
		return current
	}
	if current <= 0 || candidate < current {
		return candidate
	}
	return current
}

func (r *BootcNodePoolReconciler) recordEventf(
	regarding, related runtime.Object,
	eventType, reason, action, note string,
	args ...any,
) {
	note = truncateEventNote(fmt.Sprintf(note, args...))
	r.Recorder.Eventf(regarding, related, eventType, reason, action, "%s", note)
}

// truncateEventNote keeps notes within the events.k8s.io/v1 1 KiB limit and
// never splits a UTF-8 sequence.
func truncateEventNote(note string) string {
	note = strings.ToValidUTF8(note, "\uFFFD")
	if len(note) <= eventNoteLimit {
		return note
	}

	limit := eventNoteLimit - len(eventNoteSuffix)
	for limit > 0 && !utf8.RuneStart(note[limit]) {
		limit--
	}
	return note[:limit] + eventNoteSuffix
}
