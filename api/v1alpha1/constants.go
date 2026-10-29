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

	// AnnotationOwnsKarpenterDoNotRepair is set on a BootcNode to record that
	// the controller added the karpenter.sh/do-not-repair annotation to the K8s
	// Node during slot assignment. When present, the controller removes the
	// karpenter.sh/do-not-repair annotation from the Node when the slot is freed.
	// It is not set when the annotation was already present before slot assignment
	// (externally set), so the controller never removes an annotation it did not add.
	AnnotationOwnsKarpenterDoNotRepair = "bootc.dev/owns-karpenter-do-not-repair"

	// AnnotationOwnsKarpenterDoNotDisrupt is set on a BootcNode to record that
	// the controller added karpenter.sh/do-not-disrupt to the K8s Node during
	// slot assignment. It has the same ownership and cleanup semantics as
	// AnnotationOwnsKarpenterDoNotRepair.
	AnnotationOwnsKarpenterDoNotDisrupt = "bootc.dev/owns-karpenter-do-not-disrupt"

	// AnnotationOwnsKarpenterNodeClaimDoNotRepair records that the controller
	// added do-not-repair to the associated Karpenter NodeClaim. It follows the
	// same ownership and cleanup semantics as AnnotationOwnsKarpenterDoNotRepair.
	AnnotationOwnsKarpenterNodeClaimDoNotRepair = "bootc.dev/owns-karpenter-nodeclaim-do-not-repair"

	// AnnotationOwnsKarpenterNodeClaimDoNotDisrupt records that the controller
	// added do-not-disrupt to the associated Karpenter NodeClaim. It follows the
	// same ownership and cleanup semantics as AnnotationOwnsKarpenterDoNotRepair.
	AnnotationOwnsKarpenterNodeClaimDoNotDisrupt = "bootc.dev/owns-karpenter-nodeclaim-do-not-disrupt"

	// TaintKeyOsNotReady is a well-known taint key that cluster administrators
	// can apply to Karpenter NodePools as a startupTaint to gate workload
	// scheduling until bootc-operator has applied and verified the pool's target
	// OS image. The full taint is "bootc.dev/os-not-ready:NoSchedule".
	//
	// When a node joins with this taint, bootc-operator records ownership via
	// AnnotationOwnsOsNotReadyTaint and removes the taint once the node is
	// UpToDate and Ready. If the node is already running the target image when
	// it joins, the taint is removed without requiring a reboot slot.
	//
	// This taint MUST be declared as a startupTaint in the Karpenter NodePool.
	// If it is configured only as a regular taint, Karpenter will treat nodes
	// with the taint as unable to satisfy pending pods and will provision
	// additional nodes in a loop.
	TaintKeyOsNotReady = "bootc.dev/os-not-ready"

	// AnnotationOwnsOsNotReadyTaint is set on a BootcNode to record that the
	// controller found and owns the bootc.dev/os-not-ready:NoSchedule taint on
	// the K8s Node at join time. When present, the controller removes the taint
	// once the node is UpToDate and Ready. The controller never removes a taint
	// it did not record ownership of.
	AnnotationOwnsOsNotReadyTaint = "bootc.dev/owns-os-not-ready-taint"
)
