/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Pool condition types.
const (
	// PoolUpToDate indicates whether all nodes in the pool are running
	// the target image digest.
	PoolUpToDate string = "UpToDate"

	// PoolDegraded indicates whether the pool has encountered an error
	// that may require attention.
	PoolDegraded string = "Degraded"
)

// PoolUpToDate condition reasons.
const (
	// PoolAllUpdated means all nodes are running the target digest.
	PoolAllUpdated string = "AllUpdated"

	// PoolRolloutInProgress means nodes are actively being updated.
	PoolRolloutInProgress string = "RolloutInProgress"

	// PoolPaused means updates are pending but the pool is paused.
	PoolPaused string = "Paused"
)

// PoolDegraded condition reasons.
const (
	// PoolNodeConflict means a node's labels match multiple pool selectors.
	PoolNodeConflict string = "NodeConflict"

	// PoolNodeDegraded means one or more nodes have encountered errors
	// or are not converging. The condition message identifies the
	// affected nodes and their issues.
	PoolNodeDegraded string = "NodeDegraded"

	// PoolInvalidSpec means the pool's spec contains invalid values that
	// the controller cannot process (e.g. an image ref that is not a
	// digest ref, or a malformed nodeSelector).
	PoolInvalidSpec string = "InvalidSpec"

	// PoolHealthy means no issues.
	PoolHealthy string = "Healthy"
)

// RebootPolicy defines how a node should be rebooted during updates.
// +kubebuilder:validation:Enum=RebootOnly;AllowSoftReboot
type RebootPolicy string

const (
	// RebootPolicyRebootOnly always performs a full node reboot.
	RebootPolicyRebootOnly RebootPolicy = "RebootOnly"

	// RebootPolicyAllowSoftReboot performs a userspace-only restart
	// when the update supports it, falling back to a full reboot
	// otherwise.
	RebootPolicyAllowSoftReboot RebootPolicy = "AllowSoftReboot"
)

// ImageSpec defines the desired OS image for a pool.
type ImageSpec struct {
	// ref is the image reference, either a tag (e.g. "quay.io/example/myos:latest")
	// or a digest (e.g. "quay.io/example/myos@sha256:abc123").
	// +required
	// +kubebuilder:validation:MinLength=1
	Ref string `json:"ref"`
}

// RolloutSpec controls how updates are rolled out across pool nodes.
type RolloutSpec struct {
	// maxUnavailable is the maximum number of nodes that can be unavailable
	// during a rolling update. Value can be an absolute number (e.g. 1) or
	// a percentage of total nodes in the pool (e.g. "25%"). Defaults to 1.
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`

	// paused prevents the operator from starting new rollouts. Nodes that
	// are already mid-staging will complete their staging. Tag resolution
	// continues and status.targetDigest is kept current.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// drainTimeoutSeconds is the maximum time in seconds to wait for a
	// node drain to complete before marking the node degraded and freeing
	// the reboot slot. If not set, drains block indefinitely.
	// +optional
	// +kubebuilder:validation:Minimum=0
	DrainTimeoutSeconds *int32 `json:"drainTimeoutSeconds,omitempty"`
}

// DisruptionSpec controls the disruption behavior during updates.
type DisruptionSpec struct {
	// rebootPolicy defines how nodes should be rebooted during updates.
	// "RebootOnly" always performs a full node reboot.
	// "AllowSoftReboot" performs a userspace-only restart when the
	// update supports it, falling back to a full reboot otherwise.
	// Defaults to "RebootOnly".
	// +optional
	// +kubebuilder:default=RebootOnly
	RebootPolicy RebootPolicy `json:"rebootPolicy,omitempty"`
}

// PullSecretRef references a Secret containing image pull credentials.
type PullSecretRef struct {
	// name is the name of the Secret.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// namespace is the namespace of the Secret.
	// +required
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
}

// BootcNodePoolSpec defines the desired state of a BootcNodePool.
type BootcNodePoolSpec struct {
	// nodeSelector selects which nodes belong to this pool. Each node can
	// belong to at most one pool; overlapping selectors cause the
	// conflicting pools to be marked Degraded with reason NodeConflict.
	// +required
	NodeSelector *metav1.LabelSelector `json:"nodeSelector"`

	// image defines the desired OS image for nodes in this pool.
	// +required
	Image ImageSpec `json:"image"`

	// rollout controls how updates are rolled out across pool nodes.
	// +optional
	Rollout *RolloutSpec `json:"rollout,omitempty"`

	// disruption controls the disruption behavior during updates.
	// +optional
	Disruption *DisruptionSpec `json:"disruption,omitempty"`

	// pullSecretRef references a Secret containing image pull credentials
	// for the OS image. The operator propagates this secret to managed nodes.
	// +optional
	PullSecretRef *PullSecretRef `json:"pullSecretRef,omitempty"`
}

// BootcNodePoolStatus defines the observed state of a BootcNodePool.
type BootcNodePoolStatus struct {
	// observedGeneration is the most recent generation observed by the
	// controller. It corresponds to the pool's metadata.generation which
	// is updated on spec mutations.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// targetDigest is the image digest the pool is rolling out to. For
	// digest refs, this is set directly from spec.image.ref. For tag refs,
	// this is the resolved digest from the registry.
	// +optional
	TargetDigest string `json:"targetDigest,omitempty"`

	// deployedDigest is the last digest fully rolled out to all nodes in
	// the pool.
	// +optional
	DeployedDigest string `json:"deployedDigest,omitempty"`

	// updateAvailable is true when targetDigest differs from deployedDigest.
	// +optional
	UpdateAvailable bool `json:"updateAvailable,omitempty"`

	// nodeCount is the total number of nodes in this pool.
	// +optional
	NodeCount int32 `json:"nodeCount,omitempty"`

	// updatedCount is the number of nodes running the target digest.
	// +optional
	UpdatedCount int32 `json:"updatedCount,omitempty"`

	// updatingCount is the number of nodes actively being updated
	// (staging, staged, or rebooting).
	// +optional
	UpdatingCount int32 `json:"updatingCount,omitempty"`

	// degradedCount is the number of nodes in a degraded state.
	// +optional
	DegradedCount int32 `json:"degradedCount,omitempty"`

	// conditions represent the current state of the pool.
	// Known condition types are "UpToDate" and "Degraded".
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster

// BootcNodePool defines a group of nodes and their desired OS image state.
// Users create BootcNodePool resources to register nodes with the bootc
// operator and specify what image they should be running.
type BootcNodePool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of the pool.
	// +required
	Spec BootcNodePoolSpec `json:"spec"`

	// status defines the observed state of the pool.
	// +optional
	Status BootcNodePoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// BootcNodePoolList contains a list of BootcNodePool.
type BootcNodePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BootcNodePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BootcNodePool{}, &BootcNodePoolList{})
}
