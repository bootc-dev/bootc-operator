// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

type fakeCache struct {
	pools []bootcv1alpha1.BootcNodePool
	nodes []bootcv1alpha1.BootcNode
}

func newFakeCache(pools []bootcv1alpha1.BootcNodePool, nodes []bootcv1alpha1.BootcNode) *fakeCache {
	return &fakeCache{pools: pools, nodes: nodes}
}

func (f *fakeCache) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	switch v := list.(type) {
	case *bootcv1alpha1.BootcNodePoolList:
		v.Items = f.pools
	case *bootcv1alpha1.BootcNodeList:
		v.Items = f.nodes
	default:
		return fmt.Errorf("unexpected list type: %T", list)
	}
	return nil
}

func collectMetrics(t *testing.T, c prometheus.Collector) []*dto.MetricFamily {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("failed to register collector: %v", err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	return families
}

func findFamily(families []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, f := range families {
		if f.GetName() == name {
			return f
		}
	}
	return nil
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

func TestCollectorPoolGauges(t *testing.T) {
	g := NewWithT(t)

	const poolName = "prod"

	pools := []bootcv1alpha1.BootcNodePool{
		*testutil.NewPool(poolName, testutil.ImageDigestRefA,
			testutil.WithWorkerSelector(),
			testutil.WithPaused(true),
		),
	}
	pools[0].Status = bootcv1alpha1.BootcNodePoolStatus{
		NodeCount:     5,
		UpdatedCount:  3,
		UpdatingCount: 1,
		DegradedCount: 1,
	}

	c := newMetricsCollector(newFakeCache(pools, nil))
	families := collectMetrics(t, c)

	tests := []struct {
		name  string
		value float64
	}{
		{"bootcoperator_pool_nodes_total", 5},
		{"bootcoperator_pool_nodes_updated", 3},
		{"bootcoperator_pool_nodes_updating", 1},
		{"bootcoperator_pool_nodes_degraded", 1},
		{"bootcoperator_pool_rollout_paused", 1},
	}

	for _, tt := range tests {
		f := findFamily(families, tt.name)
		g.Expect(f).ToNot(BeNil(), "metric %s not found", tt.name)
		g.Expect(f.GetMetric()).To(HaveLen(1))
		m := f.GetMetric()[0]
		g.Expect(m.GetGauge().GetValue()).To(Equal(tt.value), "metric %s", tt.name)
		g.Expect(labelValue(m, "pool")).To(Equal(poolName))
	}
}

func TestCollectorPoolRolloutNotPaused(t *testing.T) {
	g := NewWithT(t)

	pools := []bootcv1alpha1.BootcNodePool{
		*testutil.NewPool("staging", testutil.ImageDigestRefA, testutil.WithWorkerSelector()),
	}

	c := newMetricsCollector(newFakeCache(pools, nil))
	families := collectMetrics(t, c)

	f := findFamily(families, "bootcoperator_pool_rollout_paused")
	g.Expect(f).ToNot(BeNil())
	g.Expect(f.GetMetric()[0].GetGauge().GetValue()).To(Equal(float64(0)))
}

func TestCollectorNodeInfoMetric(t *testing.T) {
	g := NewWithT(t)

	const (
		poolName = "prod"
		nodeName = "worker-1"
		version  = "1.0.0"
	)

	pools := []bootcv1alpha1.BootcNodePool{
		*testutil.NewPool(poolName, testutil.ImageDigestRefA, testutil.WithWorkerSelector()),
	}
	pools[0].UID = "pool-uid-1"

	nodes := []bootcv1alpha1.BootcNode{
		*testutil.NewNode(nodeName, testutil.ImageDigestRefA,
			testutil.WithBootedImage(testutil.ImageDigestRefA),
			testutil.WithBootedDigest(testutil.DigestA),
			testutil.WithBootedVersion(version),
		),
	}
	nodes[0].OwnerReferences = []metav1.OwnerReference{{
		APIVersion: bootcv1alpha1.GroupVersion.String(),
		Kind:       "BootcNodePool",
		Name:       poolName,
		UID:        pools[0].UID,
	}}

	c := newMetricsCollector(newFakeCache(pools, nodes))
	families := collectMetrics(t, c)

	f := findFamily(families, "bootcoperator_node_booted_image_info")
	g.Expect(f).ToNot(BeNil())
	g.Expect(f.GetMetric()).To(HaveLen(1))
	m := f.GetMetric()[0]
	g.Expect(m.GetGauge().GetValue()).To(Equal(float64(1)))
	g.Expect(labelValue(m, "node")).To(Equal(nodeName))
	g.Expect(labelValue(m, "pool")).To(Equal(poolName))
	g.Expect(labelValue(m, "image")).To(Equal(testutil.ImageDigestRefA))
	g.Expect(labelValue(m, "digest")).To(Equal(testutil.DigestA))
	g.Expect(labelValue(m, "version")).To(Equal(version))
}

// Orphaned nodes (no BootcNodePool owner) still emit a metric with an empty pool label.
func TestCollectorNodeWithoutOwnerHasEmptyPool(t *testing.T) {
	g := NewWithT(t)

	nodes := []bootcv1alpha1.BootcNode{
		*testutil.NewNode("orphan", testutil.ImageDigestRefA,
			testutil.WithBootedImage(testutil.ImageDigestRefA),
			testutil.WithBootedDigest(testutil.DigestA),
		),
	}

	c := newMetricsCollector(newFakeCache(nil, nodes))
	families := collectMetrics(t, c)

	f := findFamily(families, "bootcoperator_node_booted_image_info")
	g.Expect(f).ToNot(BeNil())
	g.Expect(f.GetMetric()).To(HaveLen(1))
	g.Expect(labelValue(f.GetMetric()[0], "pool")).To(Equal(""))
}

func TestCollectorNodeWithoutBootedStatus(t *testing.T) {
	g := NewWithT(t)

	nodes := []bootcv1alpha1.BootcNode{
		*testutil.NewNode("no-boot", testutil.ImageDigestRefA),
	}

	c := newMetricsCollector(newFakeCache(nil, nodes))
	families := collectMetrics(t, c)

	f := findFamily(families, "bootcoperator_node_booted_image_info")
	g.Expect(f).To(BeNil())
}

func TestCollectorMultiplePools(t *testing.T) {
	g := NewWithT(t)

	const (
		prodPool    = "prod"
		stagingPool = "staging"
	)

	pools := []bootcv1alpha1.BootcNodePool{
		*testutil.NewPool(prodPool, testutil.ImageDigestRefA, testutil.WithWorkerSelector()),
		*testutil.NewPool(stagingPool, testutil.ImageDigestRefB, testutil.WithNodeSelector(testutil.ControlPlaneLabels())),
	}
	pools[0].Status.NodeCount = 3
	pools[1].Status.NodeCount = 2

	c := newMetricsCollector(newFakeCache(pools, nil))
	families := collectMetrics(t, c)

	f := findFamily(families, "bootcoperator_pool_nodes_total")
	g.Expect(f).ToNot(BeNil())
	g.Expect(f.GetMetric()).To(HaveLen(2))

	for _, m := range f.GetMetric() {
		switch labelValue(m, "pool") {
		case prodPool:
			g.Expect(m.GetGauge().GetValue()).To(Equal(float64(3)))
		case stagingPool:
			g.Expect(m.GetGauge().GetValue()).To(Equal(float64(2)))
		default:
			t.Errorf("unexpected pool label: %s", labelValue(m, "pool"))
		}
	}
}

func TestCollectorEmptyCache(t *testing.T) {
	g := NewWithT(t)

	c := newMetricsCollector(newFakeCache(nil, nil))
	families := collectMetrics(t, c)

	g.Expect(findFamily(families, "bootcoperator_pool_nodes_total")).To(BeNil())
	g.Expect(findFamily(families, "bootcoperator_node_booted_image_info")).To(BeNil())
}
