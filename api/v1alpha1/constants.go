// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// FinalizerPoolCleanup is added to every BootcNodePool so the controller
// can remove the bootc.dev/managed label from member nodes before the
// pool object is fully deleted.
const FinalizerPoolCleanup = "bootc.dev/pool-cleanup"

// Well-known labels and annotations applied to Nodes by the controller.
const (
	// LabelManaged is set on Nodes that are managed by a BootcNodePool.
	// Its presence triggers the DaemonSet to schedule a daemon pod on
	// the node.
	LabelManaged = "bootc.dev/managed"

	// AnnotationInRebootSlot is set on a BootcNode when the controller
	// assigns it a reboot slot (cordons the K8s Node and starts
	// draining). Cleared when the slot is freed (node is healthy and
	// Ready after reboot). Used for persistent slot counting across
	// controller restarts.
	AnnotationInRebootSlot = "bootc.dev/in-reboot-slot"

	// AnnotationWasCordoned is set on a BootcNode to record whether
	// the K8s Node was already cordoned before the controller cordoned
	// it for a reboot. Used to restore prior cordon state after update.
	AnnotationWasCordoned = "bootc.dev/was-cordoned"

	// AnnotationLastObservedState records the last BootcNode Idle condition
	// state for which the controller considered emitting an event. It is
	// observability bookkeeping only and is never used to drive reconciliation.
	AnnotationLastObservedState = "bootc.dev/last-observed-state"
)
