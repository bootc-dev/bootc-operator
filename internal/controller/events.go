// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
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

// EventNote renders the human-readable message of a Kubernetes Event. Each
// implementation owns a small set of well-defined fields and is responsible for
// producing a message that fits the events.k8s.io/v1 1 KiB note limit. Notes
// assembled entirely from bounded fields (image references, digests) are safe by
// construction; notes that embed unbounded free text (condition messages, error
// strings) cap themselves with capNote.
type EventNote interface {
	Note() string
}

// shortDigest abbreviates a "sha256:<hex>" digest to a human-friendly prefix
// ("sha256:" plus 12 hex characters), which is enough to disambiguate images in
// event messages while keeping them concise. Values that are already short are
// returned unchanged.
func shortDigest(digest string) string {
	const shortLen = len("sha256:") + 12
	if len(digest) > shortLen {
		return digest[:shortLen]
	}
	return digest
}

type poolImageUpdateNote struct {
	ImageRef       string
	NewDigest      string
	PreviousDigest string
}

func (n poolImageUpdateNote) Note() string {
	return fmt.Sprintf(
		"Image tag %s resolved to new digest %s (previously %s)",
		n.ImageRef,
		shortDigest(n.NewDigest),
		shortDigest(n.PreviousDigest),
	)
}

type poolRolloutStartedNote struct {
	TargetDigest string
}

func (n poolRolloutStartedNote) Note() string {
	return fmt.Sprintf("Rollout started toward digest %s", shortDigest(n.TargetDigest))
}

type poolRolloutCompletedNote struct {
	TargetDigest string
}

func (n poolRolloutCompletedNote) Note() string {
	return fmt.Sprintf("Rollout completed at digest %s", shortDigest(n.TargetDigest))
}

// poolDegradedNote wraps a pool's Degraded condition message, which is not
// length-bounded, so it caps itself.
type poolDegradedNote struct {
	Message string
}

func (n poolDegradedNote) Note() string {
	return capNote(n.Message)
}

type nodeStagingNote struct {
	Image string
}

func (n nodeStagingNote) Note() string {
	return fmt.Sprintf("Staging image %s", n.Image)
}

type nodeStagedNote struct {
	Image string
}

func (n nodeStagedNote) Note() string {
	return fmt.Sprintf("Image %s is staged and awaiting reboot", n.Image)
}

type nodeRebootingNote struct {
	Image string
}

func (n nodeRebootingNote) Note() string {
	return fmt.Sprintf("Rebooting into image %s", n.Image)
}

type nodeIdleNote struct {
	Image string
}

func (n nodeIdleNote) Note() string {
	return fmt.Sprintf("Node is up to date with image %s", n.Image)
}

// drainFailedNote embeds an error string, which is not length-bounded, so it
// caps itself.
type drainFailedNote struct {
	Err error
}

func (n drainFailedNote) Note() string {
	return capNote(fmt.Sprintf("Failed to drain node: %v; the drain will be retried", n.Err))
}

type drainStalledNote struct {
	Threshold time.Duration
}

func (n drainStalledNote) Note() string {
	return fmt.Sprintf(
		"Drain has been running for more than %s; it may be blocked by a PodDisruptionBudget",
		n.Threshold,
	)
}

func (r *BootcNodePoolReconciler) recordPoolEvents(
	pool *bootcv1alpha1.BootcNodePool,
	previous *bootcv1alpha1.BootcNodePoolStatus,
	tagTargetChanged bool,
) {
	if tagTargetChanged {
		r.recordEvent(
			pool,
			nil,
			corev1.EventTypeNormal,
			eventReasonImageUpdateAvailable,
			eventActionResolveImage,
			poolImageUpdateNote{
				ImageRef:       pool.Spec.Image.Ref,
				NewDigest:      pool.Status.TargetDigest,
				PreviousDigest: previous.TargetDigest,
			},
		)
	}

	oldUpToDate := apimeta.FindStatusCondition(previous.Conditions, bootcv1alpha1.PoolUpToDate)
	newUpToDate := apimeta.FindStatusCondition(pool.Status.Conditions, bootcv1alpha1.PoolUpToDate)
	targetChanged := previous.TargetDigest != "" &&
		previous.TargetDigest != pool.Status.TargetDigest &&
		pool.Status.TargetDigest != ""
	poolRolloutInProgress := conditionEnteredReason(
		oldUpToDate,
		newUpToDate,
		metav1.ConditionFalse,
		bootcv1alpha1.PoolRolloutInProgress,
	) || (targetChanged && newUpToDate != nil &&
		newUpToDate.Status == metav1.ConditionFalse &&
		newUpToDate.Reason == bootcv1alpha1.PoolRolloutInProgress)
	if poolRolloutInProgress {
		r.recordEvent(
			pool,
			nil,
			corev1.EventTypeNormal,
			eventReasonRolloutStarted,
			eventActionRollout,
			poolRolloutStartedNote{TargetDigest: pool.Status.TargetDigest},
		)
	}

	rolloutCompleted := oldUpToDate != nil &&
		oldUpToDate.Status != metav1.ConditionTrue &&
		newUpToDate != nil &&
		newUpToDate.Status == metav1.ConditionTrue
	if rolloutCompleted {
		r.recordEvent(
			pool,
			nil,
			corev1.EventTypeNormal,
			eventReasonRolloutCompleted,
			eventActionRollout,
			poolRolloutCompletedNote{TargetDigest: pool.Status.TargetDigest},
		)
	}

	oldDegraded := apimeta.FindStatusCondition(previous.Conditions, bootcv1alpha1.PoolDegraded)
	newDegraded := apimeta.FindStatusCondition(pool.Status.Conditions, bootcv1alpha1.PoolDegraded)
	if degradedConditionChanged(oldDegraded, newDegraded) {
		r.recordEvent(
			pool,
			nil,
			corev1.EventTypeWarning,
			newDegraded.Reason,
			eventActionPoolDegraded,
			poolDegradedNote{Message: newDegraded.Message},
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

// recordNodeEvents emits an event once for each observed Staging, Staged,
// Rebooting, or return-to-idle transition. A controller-owned annotation
// persists the last observation so unrelated reconciles and controller restarts
// do not repeat events. Annotation write failures are logged but never block a
// rollout.
func (r *BootcNodePoolReconciler) recordNodeEvents(
	ctx context.Context,
	pool *bootcv1alpha1.BootcNodePool,
	nodes map[string]*bootcv1alpha1.BootcNode,
) {
	log := logf.FromContext(ctx)
	for _, node := range nodes {
		previous := node.Annotations[bootcv1alpha1.AnnotationLastObservedState]
		observation, reason, note := nodeEvent(node, previous)
		if observation == "" || previous == observation {
			continue
		}

		if note != nil {
			r.recordEvent(
				node,
				pool,
				corev1.EventTypeNormal,
				reason,
				eventActionNodeUpdate,
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

// nodeEvent maps a BootcNode's Idle condition to the event that should be
// recorded for it. It returns the observation to persist, the event reason, and
// the note to emit. A nil note means the transition should be tracked (so it is
// not re-evaluated) but no event is emitted. previousObservation is the last
// persisted observation and is used to emit a return-to-idle event only when the
// node was previously mid-rollout.
func nodeEvent(
	node *bootcv1alpha1.BootcNode,
	previousObservation string,
) (observation, reason string, note EventNote) {
	idle := apimeta.FindStatusCondition(node.Status.Conditions, bootcv1alpha1.NodeIdle)
	if idle == nil {
		return "", "", nil
	}

	observation = fmt.Sprintf("%s:%s:%s", idle.Status, idle.Reason, node.Spec.DesiredImage)
	if idle.Status != metav1.ConditionFalse {
		// The node is idle. Only announce it when it just finished a rollout;
		// otherwise (freshly created or already idle) record the observation
		// silently so restarts do not emit a spurious event.
		if idle.Reason == bootcv1alpha1.NodeReasonIdle && isActiveObservation(previousObservation) {
			return observation,
				bootcv1alpha1.NodeReasonIdle,
				nodeIdleNote{Image: node.Spec.DesiredImage}
		}
		return observation, "", nil
	}

	switch idle.Reason {
	case bootcv1alpha1.NodeReasonStaging:
		return observation,
			bootcv1alpha1.NodeReasonStaging,
			nodeStagingNote{Image: node.Spec.DesiredImage}
	case bootcv1alpha1.NodeReasonStaged:
		return observation,
			bootcv1alpha1.NodeReasonStaged,
			nodeStagedNote{Image: node.Spec.DesiredImage}
	case bootcv1alpha1.NodeReasonRebooting:
		return observation,
			bootcv1alpha1.NodeReasonRebooting,
			nodeRebootingNote{Image: node.Spec.DesiredImage}
	default:
		return observation, "", nil
	}
}

// isActiveObservation reports whether a persisted observation represents a node
// that was mid-rollout (Idle=False). Observations are formatted as
// "<status>:<reason>:<image>".
func isActiveObservation(observation string) bool {
	return strings.HasPrefix(observation, string(metav1.ConditionFalse)+":")
}

func (r *BootcNodePoolReconciler) recordDrainFailedEvent(
	pool *bootcv1alpha1.BootcNodePool,
	node *bootcv1alpha1.BootcNode,
	err error,
) {
	r.recordEvent(
		node,
		pool,
		corev1.EventTypeWarning,
		eventReasonDrainFailed,
		eventActionDrain,
		drainFailedNote{Err: err},
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
		r.recordEvent(
			node,
			pool,
			corev1.EventTypeWarning,
			eventReasonDrainTakingTooLong,
			eventActionDrain,
			drainStalledNote{Threshold: drainStallThreshold},
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

func (r *BootcNodePoolReconciler) recordEvent(
	regarding, related runtime.Object,
	eventType, reason, action string,
	note EventNote,
) {
	r.Recorder.Eventf(regarding, related, eventType, reason, action, "%s", note.Note())
}

// capNote keeps a note within the events.k8s.io/v1 1 KiB limit and never splits
// a UTF-8 sequence. It is only needed for notes built from unbounded free text
// (condition messages, error strings); notes assembled from bounded fields fit
// by construction.
func capNote(note string) string {
	note = strings.ToValidUTF8(note, "�")
	if len(note) <= eventNoteLimit {
		return note
	}

	limit := eventNoteLimit - len(eventNoteSuffix)
	for limit > 0 && !utf8.RuneStart(note[limit]) {
		limit--
	}
	return note[:limit] + eventNoteSuffix
}
