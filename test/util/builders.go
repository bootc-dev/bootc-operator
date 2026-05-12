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

// Package testutil provides shared test helpers for building bootc CRD
// objects with functional options. It's used by both envtests and e2e tests.
package testutil

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	bootcv1alpha1 "github.com/jlebon/bootc-operator/api/v1alpha1"
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
