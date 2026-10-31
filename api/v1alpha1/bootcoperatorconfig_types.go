// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const (
	// DefaultTagResolutionPeriodSeconds is the default interval between resolving image tags.
	DefaultTagResolutionPeriodSeconds int32 = 300

	// DefaultStatusPollPeriodSeconds is the default interval between fallback bootc status polls.
	DefaultStatusPollPeriodSeconds int32 = 300
)

// OperatorControllerConfig configures the operator controller at startup.
type OperatorControllerConfig struct {
	// allowInsecureRegistry allows falling back to HTTP when accessing image
	// registries. Enable this only for trusted registries that do not support TLS.
	// +optional
	// +kubebuilder:default=false
	AllowInsecureRegistry *bool `json:"allowInsecureRegistry,omitempty"`

	// tagResolutionPeriodSeconds is the interval in seconds between resolving
	// image tags to digests. Defaults to 300 seconds.
	// +optional
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=1
	TagResolutionPeriodSeconds *int32 `json:"tagResolutionPeriodSeconds,omitempty"`
}

// OperatorDaemonConfig configures each node daemon at startup.
type OperatorDaemonConfig struct {
	// statusPollPeriodSeconds is the interval in seconds between bootc status
	// polls when filesystem notifications are unavailable. Defaults to 300 seconds.
	// +optional
	// +kubebuilder:default=300
	// +kubebuilder:validation:Minimum=1
	StatusPollPeriodSeconds *int32 `json:"statusPollPeriodSeconds,omitempty"`
}

// BootcOperatorConfigSpec defines operator settings consumed at process startup.
// Restart the affected controller or daemon pods after changing these settings.
type BootcOperatorConfigSpec struct {
	// controller configures the controller. Omitted settings use their defaults.
	// +optional
	// +kubebuilder:default={}
	Controller *OperatorControllerConfig `json:"controller,omitempty"`

	// daemon configures all node daemons. Omitted settings use their defaults.
	// +optional
	// +kubebuilder:default={}
	Daemon *OperatorDaemonConfig `json:"daemon,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster

// BootcOperatorConfig configures the controller and node daemons in a cluster.
// The optional resource is administrator-owned and may have any valid name.
// The operator rejects multiple configurations at startup. Changes take effect
// when the affected pods restart; explicit command-line flags take precedence.
type BootcOperatorConfig struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is standard object metadata.
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the configuration consumed when operator processes start.
	// +required
	Spec BootcOperatorConfigSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// BootcOperatorConfigList contains a list of BootcOperatorConfig.
type BootcOperatorConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []BootcOperatorConfig `json:"items"`
}
