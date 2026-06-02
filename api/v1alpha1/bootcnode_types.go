// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Node condition types.
const (
	// NodeIdle reports whether the daemon has an active update cycle.
	// It does not claim whether the node is "up to date" — that is
	// determined by the controller by comparing spec.desiredImage
	// against status.booted.imageDigest.
	NodeIdle string = "Idle"

	// NodeDegraded indicates that the daemon has encountered an error.
	// The reason and message provide details about the specific error.
	NodeDegraded string = "Degraded"
)

// NodeIdle condition reasons.
const (
	// NodeReasonIdle means the daemon has no active update cycle.
	NodeReasonIdle string = "Idle"

	// NodeReasonStaging means the daemon is pulling/staging an image.
	NodeReasonStaging string = "Staging"

	// NodeReasonStaged means the image is staged and locked, awaiting
	// desiredImageState: Booted to proceed with reboot.
	NodeReasonStaged string = "Staged"

	// NodeReasonRebooting means a reboot is in progress.
	NodeReasonRebooting string = "Rebooting"
)

// NodeDegraded condition reasons.
const (
	// NodeReasonError is a catch-all reason for daemon errors. The
	// condition message provides details about the specific error.
	NodeReasonError string = "Error"

	// NodeReasonHealthy means no errors.
	NodeReasonHealthy string = "Healthy"
)

// DesiredImageState describes the target state the daemon should drive
// the node toward for the desired image.
// +kubebuilder:validation:Enum=Staged;Booted
type DesiredImageState string

const (
	// DesiredImageStateStaged means the daemon should stage the image
	// but not reboot into it.
	DesiredImageStateStaged DesiredImageState = "Staged"

	// DesiredImageStateBooted means the daemon should apply the staged
	// image and reboot into it.
	DesiredImageStateBooted DesiredImageState = "Booted"
)

// ImageInfo describes an OS image in a boot slot. Used for the booted,
// staged, and rollback entries. Fields are populated from bootc status;
// not all fields are present for every slot.
type ImageInfo struct {
	// image is the full image pullspec (e.g.
	// "quay.io/example/myos@sha256:abc123").
	// +required
	Image string `json:"image"`

	// imageDigest is the image digest (e.g. "sha256:abc123").
	// +required
	ImageDigest string `json:"imageDigest"`

	// version is the image version string, if any.
	// +optional
	Version string `json:"version,omitempty"`

	// timestamp is the image build timestamp, if any.
	// +optional
	Timestamp *metav1.Time `json:"timestamp,omitempty"`

	// architecture is the hardware architecture of the image
	// (e.g. "amd64").
	// +optional
	Architecture string `json:"architecture,omitempty"`

	// softRebootCapable is true if this boot entry supports a
	// userspace-only restart relative to the currently booted system.
	// +optional
	SoftRebootCapable bool `json:"softRebootCapable,omitempty"`

	// incompatible is true if the boot entry has local mutations that
	// bootc cannot manage (e.g. package layering via rpm-ostree).
	// +optional
	Incompatible bool `json:"incompatible,omitempty"`
}

// BootcNodeSpec defines the desired state of a BootcNode.
// Written by the controller; the daemon treats it as read-only.
type BootcNodeSpec struct {
	// desiredImage is the image the node should be running, specified
	// as a pullspec with digest (e.g.
	// "quay.io/example/myos@sha256:abc123"). Set by the controller
	// from the owning pool's targetDigest.
	// +required
	// +kubebuilder:validation:MinLength=1
	DesiredImage string `json:"desiredImage"`

	// desiredImageState is the target state the daemon should drive
	// the node toward. "Staged" means stage the image but do not
	// reboot. "Booted" means apply the staged image and reboot.
	// +required
	DesiredImageState DesiredImageState `json:"desiredImageState"`

	// pullSecretRef references a Secret containing image pull
	// credentials. Copied from the owning pool's spec.
	// +optional
	PullSecretRef *PullSecretRef `json:"pullSecretRef,omitempty"`

	// pullSecretHash is a hash of the pull secret's content, used to
	// detect changes. When this value changes, the daemon re-fetches
	// the secret and updates the host filesystem.
	// +optional
	PullSecretHash string `json:"pullSecretHash,omitempty"`
}

// BootcNodeStatus defines the observed state of a BootcNode.
// Written by the daemon; the controller treats it as read-only.
type BootcNodeStatus struct {
	// observedGeneration is the most recent generation observed by the
	// daemon. It corresponds to the BootcNode's metadata.generation
	// which is updated on spec mutations.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// booted describes the currently booted OS image.
	// +optional
	Booted *ImageInfo `json:"booted,omitempty"`

	// staged describes the OS image staged for the next boot, if any.
	// +optional
	Staged *ImageInfo `json:"staged,omitempty"`

	// rollback describes the previous OS image available for rollback,
	// if any.
	// +optional
	Rollback *ImageInfo `json:"rollback,omitempty"`

	// conditions represent the daemon's current state.
	// Known condition types are "Idle" and "Degraded".
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// BootcNode represents a single node managed by the bootc operator.
// BootcNode objects are auto-created by the controller (one per managed
// node) and named after the Node they represent. The controller writes
// spec, the daemon writes status.
type BootcNode struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state, written by the controller.
	// +required
	Spec BootcNodeSpec `json:"spec"`

	// status defines the observed state, written by the daemon.
	// +optional
	Status BootcNodeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BootcNodeList contains a list of BootcNode.
type BootcNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BootcNode `json:"items"`
}
