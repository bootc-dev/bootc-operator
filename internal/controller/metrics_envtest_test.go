// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

func TestMetricsFromEnvtestCache(t *testing.T) {
	if k8sClient == nil {
		t.Skip("envtest not available")
	}
	if testCache == nil {
		t.Skip("cache not available")
	}
	g := NewWithT(t)
	ctx := context.Background()

	pool := testutil.NewPool(
		"metrics-envtest",
		testutil.ImageDigestRefA,
		testutil.WithWorkerSelector(),
	)
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

	g.Eventually(func() (int64, error) {
		var fetched bootcv1alpha1.BootcNodePool
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &fetched)
		return fetched.Status.ObservedGeneration, err
	}).WithTimeout(10 * time.Second).WithPolling(200 * time.Millisecond).Should(Equal(pool.Generation))

	reg := prometheus.NewPedanticRegistry()
	collector := newMetricsCollector(testCache)
	g.Expect(reg.Register(collector)).To(Succeed())

	families, err := reg.Gather()
	g.Expect(err).ToNot(HaveOccurred())

	f := findFamily(families, "bootcoperator_pool_nodes_total")
	g.Expect(f).ToNot(BeNil())
	g.Expect(f.GetMetric()).ToNot(BeEmpty())

	found := false
	for _, m := range f.GetMetric() {
		if labelValue(m, "pool") == pool.Name {
			found = true
			break
		}
	}
	g.Expect(found).To(BeTrue(), "expected pool %q in metrics", pool.Name)
}

func TestMetricsNodeInfoFromEnvtestCache(t *testing.T) {
	if k8sClient == nil {
		t.Skip("envtest not available")
	}
	if testCache == nil {
		t.Skip("cache not available")
	}
	g := NewWithT(t)
	ctx := context.Background()

	pool := testutil.NewPool("metrics-node-envtest", testutil.ImageDigestRefA,
		testutil.WithNodeSelector(map[string]string{"envtest-metrics": "true"}),
	)
	g.Expect(k8sClient.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, pool) })

	g.Eventually(func() (string, error) {
		var fetched bootcv1alpha1.BootcNodePool
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &fetched)
		return fetched.Name, err
	}).WithTimeout(10 * time.Second).WithPolling(200 * time.Millisecond).Should(Equal(pool.Name))

	var fetched bootcv1alpha1.BootcNodePool
	g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), &fetched)).To(Succeed())

	node := &bootcv1alpha1.BootcNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "metrics-envtest-node",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: bootcv1alpha1.GroupVersion.String(),
				Kind:       "BootcNodePool",
				Name:       fetched.Name,
				UID:        fetched.UID,
			}},
		},
		Spec: bootcv1alpha1.BootcNodeSpec{
			DesiredImage:      testutil.ImageDigestRefA,
			DesiredImageState: bootcv1alpha1.DesiredImageStateStaged,
		},
	}
	g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, node) })

	node.Status.Booted = &bootcv1alpha1.ImageInfo{
		Image:       testutil.ImageDigestRefA,
		ImageDigest: testutil.DigestA,
		Version:     "1.0.0",
	}
	g.Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

	reg := prometheus.NewPedanticRegistry()
	collector := newMetricsCollector(testCache)
	g.Expect(reg.Register(collector)).To(Succeed())

	g.Eventually(func() (string, error) {
		families, err := reg.Gather()
		if err != nil {
			return "", err
		}
		f := findFamily(families, "bootcoperator_node_booted_image_info")
		if f == nil {
			return "", nil
		}
		for _, m := range f.GetMetric() {
			if labelValue(m, "node") == node.Name {
				return labelValue(m, "pool"), nil
			}
		}
		return "", nil
	}).WithTimeout(10 * time.Second).WithPolling(500 * time.Millisecond).Should(Equal(pool.Name))
}
