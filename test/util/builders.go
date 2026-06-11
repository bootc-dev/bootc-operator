// SPDX-License-Identifier: Apache-2.0

// Package testutil provides shared test helpers for building bootc CRD
// objects with functional options. It's used by both envtests and e2e tests.
package testutil

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
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
)

// PoolOption configures a BootcNodePool.
type PoolOption func(*bootcv1alpha1.BootcNodePool)

// WorkerLabels returns the conventional worker node label map.
func WorkerLabels() map[string]string {
	return map[string]string{"node-role.kubernetes.io/worker": ""}
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

// NodeOption configures a BootcNode.
type NodeOption func(*bootcv1alpha1.BootcNode)

// NewNode creates a BootcNode with the given name and desired image.
// DesiredImageState defaults to Staged. Override fields via functional
// options.
func NewNode(name, desiredImage string, opts ...NodeOption) *bootcv1alpha1.BootcNode {
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

// WithBootedDigest sets the booted image digest in the node's status.
func WithBootedDigest(digest string) NodeOption {
	return func(node *bootcv1alpha1.BootcNode) {
		node.Status.Booted = &bootcv1alpha1.ImageInfo{
			ImageDigest: digest,
		}
	}
}

// WithNodeCondition appends a condition to the node's status.
func WithNodeCondition(condType string, status metav1.ConditionStatus, reason string) NodeOption {
	return func(node *bootcv1alpha1.BootcNode) {
		node.Status.Conditions = append(node.Status.Conditions, metav1.Condition{
			Type:   condType,
			Status: status,
			Reason: reason,
		})
	}
}

// WithNodeAnnotation sets a single annotation on the node.
func WithNodeAnnotation(key, value string) NodeOption {
	return func(node *bootcv1alpha1.BootcNode) {
		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}
		node.Annotations[key] = value
	}
}

// WithDesiredImageState overrides the default DesiredImageState on a node.
func WithDesiredImageState(state bootcv1alpha1.DesiredImageState) NodeOption {
	return func(node *bootcv1alpha1.BootcNode) {
		node.Spec.DesiredImageState = state
	}
}

// WithNodePullSecret sets the pull secret reference and hash on a node.
func WithNodePullSecret(name, namespace, hash string) NodeOption {
	return func(node *bootcv1alpha1.BootcNode) {
		node.Spec.PullSecretRef = &bootcv1alpha1.PullSecretRef{
			Name:      name,
			Namespace: namespace,
		}
		node.Spec.PullSecretHash = hash
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
