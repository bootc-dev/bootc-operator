// SPDX-License-Identifier: Apache-2.0

// Package testutil provides shared test helpers for building bootc CRD
// objects with functional options. It's used by both envtests and e2e tests.
package testutil

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	karpenter "github.com/bootc-dev/bootc-operator/internal/karpenter"
)

// Test fixture values. These are not helpers per se that are any use for e2e
// tests, but it's a convenient global spot for now. Should probably be moved
// to a separate envtest-specific package.
const (
	ImageRepo = "quay.io/example/myos"

	ImageTaggedRef = ImageRepo + ":latest"

	DigestA = "sha256:06f961b802bc46ee168555f066d28f4f0e9afdf3f88174c1ee6f9de004fc30a0"
	DigestB = "sha256:c0cde77fa8fef97d476c10aad3d2d54fcc2f336140d073651c2dcccf1e379fd6"
	DigestC = "sha256:12f37a8a84034d3e623d726fe10e5031f4df997ac13f4d5571b5a90c41fb84fe"

	ImageDigestRefA = ImageRepo + "@" + DigestA
	ImageDigestRefB = ImageRepo + "@" + DigestB
	ImageDigestRefC = ImageRepo + "@" + DigestC

	OperatorNamespaceName = "bootc-operator"
)

// PoolOption configures a BootcNodePool.
type PoolOption func(*bootcv1alpha1.BootcNodePool)

// WorkerLabels returns the conventional worker node label map.
func WorkerLabels() map[string]string {
	return map[string]string{"node-role.kubernetes.io/worker": ""}
}

// ControlPlaneLabels returns the conventional control-plane node label map.
func ControlPlaneLabels() map[string]string {
	return map[string]string{"node-role.kubernetes.io/control-plane": ""}
}

// NewPool creates a BootcNodePool with the given name and image ref.
// A nodeSelector must be provided via WithNodeSelector or
// WithWorkerSelector. Override fields via functional options.
func NewPool(name, imageRef string, opts ...PoolOption) *bootcv1alpha1.BootcNodePool {
	pool := &bootcv1alpha1.BootcNodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: bootcv1alpha1.BootcNodePoolSpec{
			Image: bootcv1alpha1.ImageSpec{
				Ref: imageRef,
			},
		},
	}
	for _, o := range opts {
		o(pool)
	}
	return pool
}

// WithRebootPolicy sets the disruption reboot policy.
func WithRebootPolicy(p bootcv1alpha1.RebootPolicy) PoolOption {
	return func(pool *bootcv1alpha1.BootcNodePool) {
		if pool.Spec.Disruption == nil {
			pool.Spec.Disruption = &bootcv1alpha1.DisruptionSpec{}
		}
		pool.Spec.Disruption.RebootPolicy = p
	}
}

// WithPullSecret sets the pull secret reference on a pool.
func WithPullSecret(name, namespace string) PoolOption {
	return func(pool *bootcv1alpha1.BootcNodePool) {
		pool.Spec.PullSecretRef = &bootcv1alpha1.PullSecretRef{
			Name:      name,
			Namespace: namespace,
		}
	}
}

// WithMaxUnavailable sets the rollout max unavailable field.
func WithMaxUnavailable(v intstr.IntOrString) PoolOption {
	return func(pool *bootcv1alpha1.BootcNodePool) {
		if pool.Spec.Rollout == nil {
			pool.Spec.Rollout = &bootcv1alpha1.RolloutSpec{}
		}
		pool.Spec.Rollout.MaxUnavailable = &v
	}
}

// WithPaused sets the rollout paused field.
func WithPaused(paused bool) PoolOption {
	return func(pool *bootcv1alpha1.BootcNodePool) {
		if pool.Spec.Rollout == nil {
			pool.Spec.Rollout = &bootcv1alpha1.RolloutSpec{}
		}
		pool.Spec.Rollout.Paused = paused
	}
}

// WithDrainTimeoutSeconds sets the rollout drain timeout in seconds.
func WithDrainTimeoutSeconds(seconds int32) PoolOption {
	return func(pool *bootcv1alpha1.BootcNodePool) {
		if pool.Spec.Rollout == nil {
			pool.Spec.Rollout = &bootcv1alpha1.RolloutSpec{}
		}
		pool.Spec.Rollout.DrainTimeoutSeconds = &seconds
	}
}

// WithRebootTimeoutSeconds sets the rollout reboot timeout in seconds.
func WithRebootTimeoutSeconds(seconds int32) PoolOption {
	return func(pool *bootcv1alpha1.BootcNodePool) {
		if pool.Spec.Rollout == nil {
			pool.Spec.Rollout = &bootcv1alpha1.RolloutSpec{}
		}
		pool.Spec.Rollout.RebootTimeoutSeconds = &seconds
	}
}

// WithLabel sets a metadata label on the pool.
func WithLabel(key, value string) PoolOption {
	return func(pool *bootcv1alpha1.BootcNodePool) {
		if pool.Labels == nil {
			pool.Labels = make(map[string]string)
		}
		pool.Labels[key] = value
	}
}

// WithNodeSelector sets the nodeSelector on a pool.
func WithNodeSelector(labels map[string]string) PoolOption {
	return func(pool *bootcv1alpha1.BootcNodePool) {
		pool.Spec.NodeSelector = &metav1.LabelSelector{
			MatchLabels: labels,
		}
	}
}

// WithWorkerSelector sets the nodeSelector to the conventional worker
// node label.
func WithWorkerSelector() PoolOption {
	return WithNodeSelector(WorkerLabels())
}

// BootcNodeOption configures a BootcNode.
type BootcNodeOption func(*bootcv1alpha1.BootcNode)

// NewBootcNode creates a BootcNode with the given name and desired image.
// DesiredImageState defaults to Staged. Override fields via functional
// options.
func NewBootcNode(name, desiredImage string, opts ...BootcNodeOption) *bootcv1alpha1.BootcNode {
	node := &bootcv1alpha1.BootcNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: bootcv1alpha1.BootcNodeSpec{
			DesiredImage:      desiredImage,
			DesiredImageState: bootcv1alpha1.DesiredImageStateStaged,
		},
	}
	for _, o := range opts {
		o(node)
	}
	return node
}

// WithBootcNodeBootedDigest sets the booted image digest in the node's status.
func WithBootcNodeBootedDigest(digest string) BootcNodeOption {
	return func(node *bootcv1alpha1.BootcNode) {
		node.Status.Booted = &bootcv1alpha1.ImageInfo{
			ImageDigest: digest,
		}
	}
}

// WithBootcNodeCondition appends a condition to the node's status.
func WithBootcNodeCondition(
	condType string,
	status metav1.ConditionStatus,
	reason string,
) BootcNodeOption {
	return func(node *bootcv1alpha1.BootcNode) {
		node.Status.Conditions = append(
			node.Status.Conditions,
			metav1.Condition{Type: condType, Status: status, Reason: reason},
		)
	}
}

// WithBootcNodeConditionAt appends a condition with an explicit
// LastTransitionTime. Useful for exercising time-based logic such as
// reboot timeouts.
func WithBootcNodeConditionAt(
	condType string,
	status metav1.ConditionStatus,
	reason string,
	at time.Time,
) BootcNodeOption {
	return func(node *bootcv1alpha1.BootcNode) {
		node.Status.Conditions = append(
			node.Status.Conditions,
			metav1.Condition{
				Type:               condType,
				Status:             status,
				Reason:             reason,
				LastTransitionTime: metav1.NewTime(at),
			},
		)
	}
}

// WithBootcNodeAnnotation sets a single annotation on the node.
func WithBootcNodeAnnotation(key, value string) BootcNodeOption {
	return func(node *bootcv1alpha1.BootcNode) {
		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}
		node.Annotations[key] = value
	}
}

// WithBootcNodeDesiredImageState overrides the default DesiredImageState on a node.
func WithBootcNodeDesiredImageState(state bootcv1alpha1.DesiredImageState) BootcNodeOption {
	return func(node *bootcv1alpha1.BootcNode) {
		node.Spec.DesiredImageState = state
	}
}

// WithBootcNodePullSecret sets the pull secret reference on a node.
func WithBootcNodePullSecret(name, namespace string) BootcNodeOption {
	return func(node *bootcv1alpha1.BootcNode) {
		node.Spec.PullSecretRef = &bootcv1alpha1.PullSecretRef{
			Name:      name,
			Namespace: namespace,
		}
	}
}

// WithBootcNodeRebootPolicy sets the reboot policy on a BootcNode.
func WithBootcNodeRebootPolicy(p bootcv1alpha1.RebootPolicy) BootcNodeOption {
	return func(node *bootcv1alpha1.BootcNode) {
		node.Spec.RebootPolicy = p
	}
}

// K8sNodeOption configures a corev1.Node.
type K8sNodeOption func(*corev1.Node)

// NewK8sNode creates a corev1.Node with the given name and labels. This is
// strictly used by envtests since there are no nodes there.
func NewK8sNode(name string, labels map[string]string, opts ...K8sNodeOption) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
	for _, o := range opts {
		o(node)
	}
	return node
}

// WithK8sNodeAnnotations sets the annotations on a Kubernetes Node.
func WithK8sNodeAnnotations(annotations map[string]string) K8sNodeOption {
	return func(node *corev1.Node) {
		node.Annotations = annotations
	}
}

// WithOwnerReferences sets the owner references on a Kubernetes Node.
func WithOwnerReferences(ownerReferences []metav1.OwnerReference) K8sNodeOption {
	return func(node *corev1.Node) {
		node.OwnerReferences = ownerReferences
	}
}

// WithTaints sets the taints on a Kubernetes Node.
func WithTaints(taints []corev1.Taint) K8sNodeOption {
	return func(node *corev1.Node) {
		node.Spec.Taints = taints
	}
}

// WithUnschedulable sets whether a Kubernetes Node is cordoned.
func WithUnschedulable(unschedulable bool) K8sNodeOption {
	return func(node *corev1.Node) {
		node.Spec.Unschedulable = unschedulable
	}
}

// WithK8sNodeConditions sets the status conditions on a Kubernetes Node.
func WithK8sNodeConditions(conditions []corev1.NodeCondition) K8sNodeOption {
	return func(node *corev1.Node) {
		node.Status.Conditions = conditions
	}
}

// KarpenterNodeClaimOption configures an internal Karpenter NodeClaim.
type KarpenterNodeClaimOption func(*karpenter.NodeClaim)

// KarpenterNodeClaimCondition describes a NodeClaim status condition.
type KarpenterNodeClaimCondition struct {
	Type   string
	Status string
	Reason string
}

// KarpenterNodeClaimRequirement describes a NodeClaim scheduling requirement.
type KarpenterNodeClaimRequirement struct {
	Key      string
	Operator string
	Values   []string
}

// NewKarpenterNodeClaim creates a NodeClaim with the given name. Override
// metadata and status fields via functional options.
func NewKarpenterNodeClaim(name string, opts ...KarpenterNodeClaimOption) *karpenter.NodeClaim {
	claim := karpenter.NewNodeClaim()
	claim.SetName(name)
	for _, o := range opts {
		o(claim)
	}
	return claim
}

// WithKarpenterNodeClaimAnnotation sets one annotation on a NodeClaim.
func WithKarpenterNodeClaimAnnotation(key, value string) KarpenterNodeClaimOption {
	return func(claim *karpenter.NodeClaim) {
		annotations := claim.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[key] = value
		claim.SetAnnotations(annotations)
	}
}

// WithKarpenterNodeClaimNodeClassRef sets spec.nodeClassRef on a NodeClaim.
func WithKarpenterNodeClaimNodeClassRef(name, kind, group string) KarpenterNodeClaimOption {
	return func(claim *karpenter.NodeClaim) {
		spec := karpenterNodeClaimSpec(claim)
		spec["nodeClassRef"] = map[string]interface{}{
			"name":  name,
			"kind":  kind,
			"group": group,
		}
	}
}

// WithKarpenterNodeClaimRequirements sets spec.requirements on a NodeClaim.
func WithKarpenterNodeClaimRequirements(
	requirements ...KarpenterNodeClaimRequirement,
) KarpenterNodeClaimOption {
	return func(claim *karpenter.NodeClaim) {
		items := make([]interface{}, 0, len(requirements))
		for _, requirement := range requirements {
			item := map[string]interface{}{
				"key":      requirement.Key,
				"operator": requirement.Operator,
			}
			if requirement.Values != nil {
				item["values"] = requirement.Values
			}
			items = append(items, item)
		}
		karpenterNodeClaimSpec(claim)["requirements"] = items
	}
}

// WithKarpenterNodeClaimNodeName sets status.nodeName on a NodeClaim.
func WithKarpenterNodeClaimNodeName(nodeName string) KarpenterNodeClaimOption {
	return func(claim *karpenter.NodeClaim) {
		status := karpenterNodeClaimStatus(claim)
		status["nodeName"] = nodeName
	}
}

// WithKarpenterNodeClaimConditions sets status.conditions on a NodeClaim.
func WithKarpenterNodeClaimConditions(
	conditions ...KarpenterNodeClaimCondition,
) KarpenterNodeClaimOption {
	return func(claim *karpenter.NodeClaim) {
		items := make([]interface{}, 0, len(conditions))
		for _, condition := range conditions {
			item := map[string]interface{}{
				"type":   condition.Type,
				"status": condition.Status,
			}
			if condition.Reason != "" {
				item["reason"] = condition.Reason
			}
			items = append(items, item)
		}
		karpenterNodeClaimStatus(claim)["conditions"] = items
	}
}

func karpenterNodeClaimStatus(claim *karpenter.NodeClaim) map[string]interface{} {
	if claim.Object == nil {
		claim.Object = make(map[string]interface{})
	}
	status, ok := claim.Object["status"].(map[string]interface{})
	if !ok {
		status = make(map[string]interface{})
		claim.Object["status"] = status
	}
	return status
}

func karpenterNodeClaimSpec(claim *karpenter.NodeClaim) map[string]interface{} {
	if claim.Object == nil {
		claim.Object = make(map[string]interface{})
	}
	spec, ok := claim.Object["spec"].(map[string]interface{})
	if !ok {
		spec = make(map[string]interface{})
		claim.Object["spec"] = spec
	}
	return spec
}
