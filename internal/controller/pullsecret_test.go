// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
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

	secret := newDockerConfigSecret("ps-prop-secret", testutil.OperatorNamespaceName, secretData)
	g.Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })

	node := testutil.NewK8sNode("ps-prop-node", testutil.WorkerLabels())
	g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, node) })

	pool := testutil.NewPool("ps-prop-pool", testImageDigestRefA,
		testutil.WithWorkerSelector(),
		testutil.WithPullSecret("ps-prop-secret", testutil.OperatorNamespaceName),
	)
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

	g.Eventually(func() (*bootcv1alpha1.PullSecretRef, error) {
		var bn bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKey{Name: "ps-prop-node"}, &bn)
		return bn.Spec.PullSecretRef, err
	}).Should(Equal(&bootcv1alpha1.PullSecretRef{
		Name: "ps-prop-secret", Namespace: testutil.OperatorNamespaceName,
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
		testutil.WithPullSecret("ps-missing-secret", testutil.OperatorNamespaceName),
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

	// BootcNode should NOT be created while the secret is missing.
	g.Consistently(func() error {
		return k8sClient.Get(
			ctx,
			client.ObjectKey{Name: "ps-missing-node"},
			&bootcv1alpha1.BootcNode{},
		)
	}).ShouldNot(Succeed())

	// Create the missing secret and verify recovery.
	secretData := []byte(`{"auths":{"recovered":{}}}`)
	secret := newDockerConfigSecret("ps-missing-secret", testutil.OperatorNamespaceName, secretData)
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
	secret := newDockerConfigSecret("ps-remove-secret", testutil.OperatorNamespaceName, secretData)
	g.Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })

	node := testutil.NewK8sNode("ps-remove-node", testutil.WorkerLabels())
	g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, node) })

	pool := testutil.NewPool("ps-remove-pool", testImageDigestRefA,
		testutil.WithWorkerSelector(),
		testutil.WithPullSecret("ps-remove-secret", testutil.OperatorNamespaceName),
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

func TestPullSecretNamespaceDeniedAndRecovery(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	deniedNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ps-denied-ns"}}
	g.Expect(k8sClient.Create(ctx, deniedNS)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, deniedNS) })

	secretData := []byte(`{"auths":{"denied":{}}}`)
	deniedSecret := newDockerConfigSecret("ps-denied-secret", deniedNS.Name, secretData)
	g.Expect(k8sClient.Create(ctx, deniedSecret)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, deniedSecret) })

	node := testutil.NewK8sNode("ps-denied-node", testutil.WorkerLabels())
	g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, node) })

	pool := testutil.NewPool("ps-denied-pool", testImageDigestRefA,
		testutil.WithWorkerSelector(),
		testutil.WithPullSecret(deniedSecret.Name, deniedSecret.Namespace),
	)
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

	// The secret exists, but its namespace is not in the allowlist.
	g.Eventually(func() ([]metav1.Condition, error) {
		var p bootcv1alpha1.BootcNodePool
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &p)
		return p.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolDegraded),
		HaveField("Status", metav1.ConditionTrue),
		HaveField("Reason", bootcv1alpha1.PoolSecretError),
		HaveField("Message", ContainSubstring(`"ps-denied-ns"`)),
	)))

	g.Consistently(func() error {
		return k8sClient.Get(
			ctx,
			client.ObjectKey{Name: "ps-denied-node"},
			&bootcv1alpha1.BootcNode{},
		)
	}, "2s", pollInterval).ShouldNot(Succeed())

	// Move the secret to the operator namespace and point the pool at it.
	allowedSecret := newDockerConfigSecret(
		"ps-denied-secret",
		testutil.OperatorNamespaceName,
		secretData,
	)
	g.Expect(k8sClient.Create(ctx, allowedSecret)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, allowedSecret) })

	g.Eventually(func() error {
		var p bootcv1alpha1.BootcNodePool
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &p); err != nil {
			return err
		}
		p.Spec.PullSecretRef.Namespace = testutil.OperatorNamespaceName
		return k8sClient.Update(ctx, &p)
	}).Should(Succeed())

	g.Eventually(func() ([]metav1.Condition, error) {
		var p bootcv1alpha1.BootcNodePool
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &p)
		return p.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolDegraded),
		HaveField("Status", metav1.ConditionFalse),
		HaveField("Reason", bootcv1alpha1.PoolHealthy),
	)))

	g.Eventually(func() (*bootcv1alpha1.PullSecretRef, error) {
		var bn bootcv1alpha1.BootcNode
		err := k8sClient.Get(ctx, client.ObjectKey{Name: "ps-denied-node"}, &bn)
		return bn.Spec.PullSecretRef, err
	}).Should(Equal(&bootcv1alpha1.PullSecretRef{
		Name: "ps-denied-secret", Namespace: testutil.OperatorNamespaceName,
	}))
}

func TestPullSecretNameTooLong(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	// A valid Secret name whose aggregated key "<namespace>_<name>" exceeds
	// the 253-character Secret key limit.
	secretName := strings.Repeat("a", 240)
	secret := newDockerConfigSecret(secretName, testutil.OperatorNamespaceName,
		[]byte(`{"auths":{"long":{}}}`))
	g.Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })

	pool := testutil.NewPool("ps-too-long-pool", testImageDigestRefA,
		testutil.WithWorkerSelector(),
		testutil.WithPullSecret(secretName, testutil.OperatorNamespaceName),
	)
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

	g.Eventually(func() ([]metav1.Condition, error) {
		var p bootcv1alpha1.BootcNodePool
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &p)
		return p.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolDegraded),
		HaveField("Status", metav1.ConditionTrue),
		HaveField("Reason", bootcv1alpha1.PoolSecretError),
		HaveField("Message", ContainSubstring("name too long")),
	)))
}

func TestParsePullSecretNamespaces(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		opNS    string
		want    []string
		wantErr bool
	}{
		{"empty", "", "op", []string{"op"}, false},
		{"only separators", " , ,", "op", []string{"op"}, false},
		{"unknown operator namespace", "", "", nil, false},
		{"operator namespace prepended", "team-a", "op", []string{"op", "team-a"}, false},
		{"trims and dedupes", " team-a ,,op,team-a", "op", []string{"op", "team-a"}, false},
		{"invalid name", "Team-A", "op", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			got, err := ParsePullSecretNamespaces(tt.value, tt.opNS)
			if tt.wantErr {
				g.Expect(err).To(MatchError(ContainSubstring(`invalid namespace "Team-A"`)))
				return
			}
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(got).To(Equal(tt.want))
		})
	}
}
