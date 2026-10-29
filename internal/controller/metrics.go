// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
)

type objectLister interface {
	List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error
}

var (
	poolNodesTotal = prometheus.NewDesc(
		"bootcoperator_pool_nodes_total",
		"Total number of nodes in a BootcNodePool.",
		[]string{"pool"}, nil,
	)
	poolNodesUpdated = prometheus.NewDesc(
		"bootcoperator_pool_nodes_updated",
		"Number of nodes running the target digest.",
		[]string{"pool"}, nil,
	)
	poolNodesUpdating = prometheus.NewDesc(
		"bootcoperator_pool_nodes_updating",
		"Number of nodes actively being updated.",
		[]string{"pool"}, nil,
	)
	poolNodesDegraded = prometheus.NewDesc(
		"bootcoperator_pool_nodes_degraded",
		"Number of nodes in a degraded state.",
		[]string{"pool"}, nil,
	)
	poolRolloutPaused = prometheus.NewDesc(
		"bootcoperator_pool_rollout_paused",
		"Whether rollout is paused for a BootcNodePool (0 or 1).",
		[]string{"pool"}, nil,
	)
	nodeBootedImageInfo = prometheus.NewDesc(
		"bootcoperator_node_booted_image_info",
		"Info metric for the currently booted OS image on a node.",
		[]string{"node", "pool", "image", "digest", "version"}, nil,
	)
)

type metricsCollector struct {
	lister objectLister
}

func newMetricsCollector(lister objectLister) *metricsCollector {
	return &metricsCollector{lister: lister}
}

func (c *metricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- poolNodesTotal
	ch <- poolNodesUpdated
	ch <- poolNodesUpdating
	ch <- poolNodesDegraded
	ch <- poolRolloutPaused
	ch <- nodeBootedImageInfo
}

func (c *metricsCollector) Collect(ch chan<- prometheus.Metric) {
	log := logf.Log.WithName("metrics")
	ctx := context.Background()

	var pools bootcv1alpha1.BootcNodePoolList
	if err := c.lister.List(ctx, &pools); err != nil {
		return
	}

	for i := range pools.Items {
		pool := &pools.Items[i]
		name := pool.Name

		emitGauge(ch, log, poolNodesTotal, float64(pool.Status.NodeCount), name)
		emitGauge(ch, log, poolNodesUpdated, float64(pool.Status.UpdatedCount), name)
		emitGauge(ch, log, poolNodesUpdating, float64(pool.Status.UpdatingCount), name)
		emitGauge(ch, log, poolNodesDegraded, float64(pool.Status.DegradedCount), name)

		var paused float64
		if pool.Spec.Rollout != nil && pool.Spec.Rollout.Paused {
			paused = 1
		}
		emitGauge(ch, log, poolRolloutPaused, paused, name)
	}

	var nodes bootcv1alpha1.BootcNodeList
	if err := c.lister.List(ctx, &nodes); err != nil {
		return
	}

	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Status.Booted == nil {
			continue
		}

		poolName := ownerPoolName(node)
		emitGauge(ch, log, nodeBootedImageInfo, 1,
			node.Name,
			poolName,
			node.Status.Booted.Image,
			node.Status.Booted.ImageDigest,
			node.Status.Booted.Version,
		)
	}
}

func emitGauge(
	ch chan<- prometheus.Metric,
	log logr.Logger,
	desc *prometheus.Desc,
	value float64,
	labels ...string,
) {
	m, err := prometheus.NewConstMetric(desc, prometheus.GaugeValue, value, labels...)
	if err != nil {
		log.Error(err, "Failed to create metric", "metric", desc.String())
		return
	}
	ch <- m
}

func RegisterMetrics(lister objectLister) error {
	return metrics.Registry.Register(newMetricsCollector(lister))
}

func ownerPoolName(node *bootcv1alpha1.BootcNode) string {
	for _, ref := range node.OwnerReferences {
		if ref.Kind == "BootcNodePool" {
			return ref.Name
		}
	}
	return ""
}
