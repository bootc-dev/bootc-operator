// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

func newDockerConfigSecret(name, namespace string, dockerCfg []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: dockerCfg,
		},
	}
}

func TestPullSecretRefPropagation(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	secretData := []byte(`{"auths":{"registry.example.com":{}}}`)

	secret := newDockerConfigSecret("ps-prop-secret", "default", secretData)
	g.Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })

	node := testutil.NewK8sNode("ps-prop-node", testutil.WorkerLabels())
	g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, node) })

	pool := testutil.NewPool("ps-prop-pool", testImageDigestRefA,
		testutil.WithWorkerSelector(),
		testutil.WithPullSecret("ps-prop-secret", "default"),
	)
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

	g.Eventually(func() (*bootcv1alpha1.PullSecretRef, error) {
		var bn bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKey{Name: "ps-prop-node"}, &bn)
		return bn.Spec.PullSecretRef, err
	}).Should(Equal(&bootcv1alpha1.PullSecretRef{
		Name: "ps-prop-secret", Namespace: "default",
	}))
}

func TestPullSecretMissingDegradedAndRecovery(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	node := testutil.NewK8sNode("ps-missing-node", testutil.WorkerLabels())
	g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, node) })

	pool := testutil.NewPool("ps-missing-pool", testImageDigestRefA,
		testutil.WithWorkerSelector(),
		testutil.WithPullSecret("ps-missing-secret", "default"),
	)
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

	// Pool should be Degraded/SecretError.
	g.Eventually(func() ([]metav1.Condition, error) {
		var p bootcv1alpha1.BootcNodePool
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &p)
		return p.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolDegraded),
		HaveField("Status", metav1.ConditionTrue),
		HaveField("Reason", bootcv1alpha1.PoolSecretError),
	)))

	// BootcNode should still be created (degrade but continue rollout).
	g.Eventually(func() error {
		return k8sClient.Get(
			ctx,
			client.ObjectKey{Name: "ps-missing-node"},
			&bootcv1alpha1.BootcNode{},
		)
	}).Should(Succeed())

	// Create the missing secret and verify recovery.
	secretData := []byte(`{"auths":{"recovered":{}}}`)
	secret := newDockerConfigSecret("ps-missing-secret", "default", secretData)
	g.Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })

	g.Eventually(func() ([]metav1.Condition, error) {
		var p bootcv1alpha1.BootcNodePool
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &p)
		return p.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolDegraded),
		HaveField("Status", metav1.ConditionFalse),
		HaveField("Reason", bootcv1alpha1.PoolHealthy),
	)))
}

func TestPullSecretRemovalClearsRef(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	secretData := []byte(`{"auths":{"remove":{}}}`)
	secret := newDockerConfigSecret("ps-remove-secret", "default", secretData)
	g.Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })

	node := testutil.NewK8sNode("ps-remove-node", testutil.WorkerLabels())
	g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, node) })

	pool := testutil.NewPool("ps-remove-pool", testImageDigestRefA,
		testutil.WithWorkerSelector(),
		testutil.WithPullSecret("ps-remove-secret", "default"),
	)
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

	// Wait for pullSecretRef to be set on BootcNode.
	g.Eventually(func() (*bootcv1alpha1.PullSecretRef, error) {
		var bn bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKey{Name: "ps-remove-node"}, &bn)
		return bn.Spec.PullSecretRef, err
	}).ShouldNot(BeNil())

	// Remove pullSecretRef from pool.
	var freshPool bootcv1alpha1.BootcNodePool
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &freshPool)).To(Succeed())
	freshPool.Spec.PullSecretRef = nil
	g.Expect(k8sClient.Update(ctx, &freshPool)).To(Succeed())

	// Verify pullSecretRef is cleared on BootcNode.
	g.Eventually(func() (*bootcv1alpha1.PullSecretRef, error) {
		var bn bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKey{Name: "ps-remove-node"}, &bn)
		return bn.Spec.PullSecretRef, err
	}).Should(BeNil())
}
