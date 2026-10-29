# Monitoring

The Bootc Operator exposes Prometheus metrics on a secure HTTPS
endpoint. This guide covers how to set up Prometheus and Grafana to
scrape and visualize the operator metrics.

## Exposed metrics

The operator exposes the following custom metrics:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `bootcoperator_pool_nodes_total` | Gauge | `pool` | Total number of nodes in a BootcNodePool |
| `bootcoperator_pool_nodes_updated` | Gauge | `pool` | Nodes running the target digest |
| `bootcoperator_pool_nodes_updating` | Gauge | `pool` | Nodes actively being updated |
| `bootcoperator_pool_nodes_degraded` | Gauge | `pool` | Nodes in a degraded state |
| `bootcoperator_pool_rollout_paused` | Gauge | `pool` | Whether rollout is paused (0 or 1) |
| `bootcoperator_node_booted_image_info` | Gauge | `node`, `pool`, `image`, `digest`, `version` | Info metric for the booted OS image |

In addition to these custom metrics, the operator exposes standard
controller-runtime metrics (workqueue depth, reconcile duration, etc.).

## Enabling the ServiceMonitor

The Helm chart includes a `ServiceMonitor` resource that tells
Prometheus where to scrape. Enable it during install or upgrade:

```bash
helm upgrade --install bootc-operator \
  oci://ghcr.io/bootc-dev/bootc-operator/charts/bootc-operator \
  --namespace bootc-operator \
  --set metrics.serviceMonitor.enabled=true
```

Verify the ServiceMonitor was created:

```bash
kubectl -n bootc-operator get servicemonitor
```

## Installing Prometheus and Grafana

The recommended way to deploy Prometheus and Grafana is the
[kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack)
Helm chart:

```bash
helm repo add prometheus-community \
  https://prometheus-community.github.io/helm-charts
helm repo update

helm install monitoring prometheus-community/kube-prometheus-stack \
  --namespace monitoring --create-namespace \
  --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false
```

Setting `serviceMonitorSelectorNilUsesHelmValues=false` allows
Prometheus to discover ServiceMonitors from all namespaces, including
`bootc-operator`.

## Granting Prometheus access to the metrics endpoint

The operator metrics endpoint requires authentication. The Helm chart
creates a `ClusterRole` called `<release>-metrics-reader` that grants
`GET` access to `/metrics`. Bind it to the Prometheus ServiceAccount:

```bash
kubectl create clusterrolebinding prometheus-metrics-reader \
  --clusterrole=bootc-operator-metrics-reader \
  --serviceaccount=monitoring:monitoring-kube-prometheus-prometheus
```

Adjust the ServiceAccount name if your Prometheus installation uses a
different one.

## Helm values reference

| Key | Default | Description |
|-----|---------|-------------|
| `metrics.enabled` | `true` | Deploy the metrics Service |
| `metrics.service.port` | `8443` | Service port for the metrics endpoint |
| `metrics.serviceMonitor.enabled` | `false` | Create a ServiceMonitor resource |
| `metrics.serviceMonitor.interval` | `30s` | Scrape interval |
| `metrics.serviceMonitor.namespace` | `""` | Namespace for the ServiceMonitor (defaults to release namespace) |
