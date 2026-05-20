// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// Well-known labels and annotations applied to Nodes by the controller.
const (
	// LabelManaged is set on Nodes that are managed by a BootcNodePool.
	// Its presence triggers the DaemonSet to schedule a daemon pod on
	// the node.
	LabelManaged = "bootc.dev/managed"

	// AnnotationWasCordoned records whether a node was already cordoned
	// before the controller cordoned it for a reboot. Used to restore
	// prior cordon state after update.
	AnnotationWasCordoned = "bootc.dev/was-cordoned"
)
