// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/internal/pullsecret"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

// aggregatedData returns the data of the aggregated pull secret.
func aggregatedData(ctx context.Context) (map[string][]byte, error) {
	var secret corev1.Secret
	err := k8sClient.Get(ctx, client.ObjectKey{
		Namespace: testutil.OperatorNamespaceName,
		Name:      pullsecret.SecretName,
	}, &secret)
	return secret.Data, err
}

// newAggregatorPool returns a pool that references the given pull secret and
// selects no node, so only the aggregator acts on it.
func newAggregatorPool(name, secretName, secretNamespace string) *bootcv1alpha1.BootcNodePool {
	return testutil.NewPool(name, testImageDigestRefA,
		testutil.WithNodeSelector(map[string]string{"bootc.dev/aggregator-test": name}),
		testutil.WithPullSecret(secretName, secretNamespace),
	)
}

func TestPullSecretAggregatorAddsKey(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	data := []byte(`{"auths":{"add.example.com":{}}}`)
	secret := newDockerConfigSecret("agg-add-secret", testutil.OperatorNamespaceName, data)
	g.Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })

	pool := newAggregatorPool("agg-add-pool", secret.Name, secret.Namespace)
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

	g.Eventually(aggregatedData).WithArguments(ctx).
		Should(HaveKeyWithValue(testutil.OperatorNamespaceName+"_agg-add-secret", data))
}

func TestPullSecretAggregatorUpdatesAndRemovesKey(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()
	key := testutil.OperatorNamespaceName + "_agg-update-secret"

	secret := newDockerConfigSecret("agg-update-secret", testutil.OperatorNamespaceName,
		[]byte(`{"auths":{"old.example.com":{}}}`))
	g.Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })

	// Two pools share one source secret, and so one key.
	pool1 := newAggregatorPool("agg-update-pool1", secret.Name, secret.Namespace)
	g.Expect(k8sClient.Create(ctx, pool1)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool1) })
	pool2 := newAggregatorPool("agg-update-pool2", secret.Name, secret.Namespace)
	g.Expect(k8sClient.Create(ctx, pool2)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool2) })

	g.Eventually(aggregatedData).WithArguments(ctx).Should(HaveKey(key))

	// Rotating the source secret updates the key.
	rotated := []byte(`{"auths":{"new.example.com":{}}}`)
	secret.Data[corev1.DockerConfigJsonKey] = rotated
	g.Expect(k8sClient.Update(ctx, secret)).To(Succeed())
	g.Eventually(aggregatedData).WithArguments(ctx).Should(HaveKeyWithValue(key, rotated))

	// The key stays while any pool references the secret.
	g.Expect(k8sClient.Delete(ctx, pool1)).To(Succeed())
	g.Eventually(func() error {
		return k8sClient.Get(ctx, client.ObjectKeyFromObject(pool1), &bootcv1alpha1.BootcNodePool{})
	}).Should(Satisfy(apierrors.IsNotFound))
	g.Consistently(aggregatedData, "2s", pollInterval).WithArguments(ctx).Should(HaveKey(key))

	// Dropping the last reference removes the key; the Secret is kept.
	g.Eventually(func() error {
		var p bootcv1alpha1.BootcNodePool
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool2), &p); err != nil {
			return err
		}
		p.Spec.PullSecretRef = nil
		return k8sClient.Update(ctx, &p)
	}).Should(Succeed())
	g.Eventually(aggregatedData).WithArguments(ctx).ShouldNot(HaveKey(key))
}

func TestPullSecretAggregatorSkipsInvalidReferences(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()

	longName := strings.Repeat("a", 240)
	long := newDockerConfigSecret(longName, testutil.OperatorNamespaceName, []byte(`{}`))
	g.Expect(k8sClient.Create(ctx, long)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, long) })

	noData := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agg-nodata-secret",
			Namespace: testutil.OperatorNamespaceName,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"other": []byte("x")},
	}
	g.Expect(k8sClient.Create(ctx, noData)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, noData) })

	valid := newDockerConfigSecret("agg-valid-secret", testutil.OperatorNamespaceName, []byte(`{}`))
	g.Expect(k8sClient.Create(ctx, valid)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, valid) })

	for _, p := range []*bootcv1alpha1.BootcNodePool{
		// The namespace is not allowed, hence not cached: reading from it
		// would fail the reconcile and block the valid key below.
		newAggregatorPool("agg-skip-denied", "agg-denied-secret", "agg-denied-ns"),
		newAggregatorPool("agg-skip-long", long.Name, long.Namespace),
		newAggregatorPool("agg-skip-nodata", noData.Name, noData.Namespace),
		newAggregatorPool("agg-skip-missing", "agg-missing-secret", testutil.OperatorNamespaceName),
		newAggregatorPool("agg-skip-valid", valid.Name, valid.Namespace),
	} {
		g.Expect(k8sClient.Create(ctx, p)).To(Succeed())
		t.Cleanup(func() { _ = k8sClient.Delete(ctx, p) })
	}

	// Invalid references are skipped without blocking the valid one.
	g.Eventually(aggregatedData).WithArguments(ctx).
		Should(HaveKey(testutil.OperatorNamespaceName + "_agg-valid-secret"))
	g.Expect(aggregatedData(ctx)).To(And(
		Not(HaveKey("agg-denied-ns_agg-denied-secret")),
		Not(HaveKey(testutil.OperatorNamespaceName+"_"+longName)),
		Not(HaveKey(testutil.OperatorNamespaceName+"_agg-nodata-secret")),
		Not(HaveKey(testutil.OperatorNamespaceName+"_agg-missing-secret")),
	))
}

func TestPullSecretAggregatorRepairsSecret(t *testing.T) {
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	ctx := context.Background()
	key := testutil.OperatorNamespaceName + "_agg-repair-secret"

	data := []byte(`{"auths":{"repair.example.com":{}}}`)
	secret := newDockerConfigSecret("agg-repair-secret", testutil.OperatorNamespaceName, data)
	g.Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, secret) })

	pool := newAggregatorPool("agg-repair-pool", secret.Name, secret.Namespace)
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

	g.Eventually(aggregatedData).WithArguments(ctx).Should(HaveKeyWithValue(key, data))

	// A manual edit is reverted.
	var aggregated corev1.Secret
	g.Expect(k8sClient.Get(ctx, client.ObjectKey{
		Namespace: testutil.OperatorNamespaceName, Name: pullsecret.SecretName,
	}, &aggregated)).To(Succeed())
	aggregated.Data[key] = []byte("tampered")
	aggregated.Data["stray"] = []byte("x")
	g.Expect(k8sClient.Update(ctx, &aggregated)).To(Succeed())
	g.Eventually(aggregatedData).WithArguments(ctx).Should(And(
		HaveKeyWithValue(key, data),
		Not(HaveKey("stray")),
	))

	// A deleted Secret is recreated.
	g.Expect(k8sClient.Delete(ctx, &aggregated)).To(Succeed())
	g.Eventually(aggregatedData).WithArguments(ctx).Should(HaveKeyWithValue(key, data))
}
