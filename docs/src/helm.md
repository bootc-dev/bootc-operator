# Helm Chart

## Prerequisites

- Kubernetes >= 1.34
- Helm 3

## Install

The operator chart includes the CRDs as a
[dependency](https://helm.sh/docs/helm/helm_dependency/). CRDs are
installed in a sub-chart (instead of the `crds/` directory) so that
`helm upgrade` can update them.

Install from the OCI registry:

```bash
helm install bootc-operator oci://ghcr.io/bootc-dev/bootc-operator/charts/bootc-operator \
  --create-namespace --namespace bootc-operator
```

To install a specific version:

```bash
helm install bootc-operator oci://ghcr.io/bootc-dev/bootc-operator/charts/bootc-operator \
  --version 0.1.0 --create-namespace --namespace bootc-operator
```

Alternatively, generate and install the chart from a local checkout:

```bash
make helm
helm dependency build ./chart/bootc-operator
helm install bootc-operator ./chart/bootc-operator \
  --create-namespace --namespace bootc-operator
```

The chart installs the configuration CRD and the operator's read permissions.
It does not create a `BootcOperatorConfig` instance. See
[Configuring the operator](operations/configuration.md) to add one.

By default the chart uses the image
`ghcr.io/bootc-dev/bootc-operator:<appVersion>` where `appVersion` is
defined in `Chart.yaml`.

### Configure before first startup

For custom settings on first startup, install the CRDs and operator as separate
Helm releases. This procedure is for a fresh installation; do not use it to
transfer CRDs from an existing combined release.

1. Install the CRDs. Replace `X.Y.Z` with a published chart version containing
   `BootcOperatorConfig`, and use that version for both charts:

   ```bash
   CHART_VERSION=X.Y.Z
   helm install bootc-operator-crds \
     oci://ghcr.io/bootc-dev/bootc-operator/charts/bootc-operator-crds \
     --version "$CHART_VERSION" --create-namespace --namespace bootc-operator
   kubectl wait --for=condition=Established \
     crd/bootcoperatorconfigs.node.bootc.dev --timeout=1m
   ```

2. Check for an existing configuration. If one exists, edit it instead of
   creating another. Otherwise, prepare an `operator-config.yaml` using the
   [configuration example](operations/configuration.md), set the desired
   values, and apply it:

   ```bash
   kubectl get bootcoperatorconfigs
   kubectl apply -f operator-config.yaml
   ```

3. Install the operator with its CRD dependency disabled, since the separate
   release already owns the CRDs:

   ```bash
   helm install bootc-operator \
     oci://ghcr.io/bootc-dev/bootc-operator/charts/bootc-operator \
     --version "$CHART_VERSION" --namespace bootc-operator --set crds.install=false
   ```

The controller and daemon read the configuration on their first startup.
Later configuration edits still require restarting the affected workloads.

## Custom image

Override the repository and tag to use a different image:

```bash
helm install bootc-operator ./chart/bootc-operator \
  --create-namespace --namespace bootc-operator \
  --set image.repository=my-registry.example.com/bootc-operator \
  --set image.tag=v0.2.0
```

## Image pull policy

The chart does not set `imagePullPolicy` by default, so Kubernetes uses
its own rules (`Always` for `:latest`, `IfNotPresent` otherwise). To
override it:

```bash
helm install bootc-operator ./chart/bootc-operator \
  --create-namespace --namespace bootc-operator \
  --set image.pullPolicy=Always
```

## Image pull secrets

If your registry requires authentication, create the secret first, then
pass it to the chart. The secret must be in the same namespace as the
release:

```bash
kubectl create namespace bootc-operator

kubectl create secret docker-registry my-pull-secret \
  --namespace bootc-operator \
  --docker-server=my-registry.example.com \
  --docker-username=user \
  --docker-password=pass

helm install bootc-operator ./chart/bootc-operator \
  --namespace bootc-operator \
  --set image.repository=my-registry.example.com/bootc-operator \
  --set image.tag=v0.2.0 \
  --set image.pullPolicy=Always \
  --set-json 'imagePullSecrets=[{"name":"my-pull-secret"}]'
```

Alternatively, create the secret from an existing pull secret file:

```bash
kubectl create secret generic my-pull-secret \
  --namespace bootc-operator \
  --from-file=.dockerconfigjson=$HOME/.docker/config.json \
  --type=kubernetes.io/dockerconfigjson
```

## Upgrade

```bash
helm upgrade bootc-operator oci://ghcr.io/bootc-dev/bootc-operator/charts/bootc-operator \
  --version <version> --namespace bootc-operator
```

Replace `<version>` with the chart version (e.g. `0.2.0`). The CRD
sub-chart is upgraded automatically.

If you installed CRDs separately, upgrade their release first, then upgrade the
operator with `--set crds.install=false`. Use the same target chart version for
both commands:

```bash
helm upgrade bootc-operator-crds oci://ghcr.io/bootc-dev/bootc-operator/charts/bootc-operator-crds \
  --version <version> --namespace bootc-operator
helm upgrade bootc-operator oci://ghcr.io/bootc-dev/bootc-operator/charts/bootc-operator \
  --version <version> --namespace bootc-operator --set crds.install=false
```

## Uninstall

```bash
helm uninstall bootc-operator --namespace bootc-operator
```

> **Warning**: uninstalling the chart also removes the CRDs and all
> their instances cluster-wide, including administrator-owned
> `BootcOperatorConfig` settings. This applies when the CRD dependency is enabled
> (the default). With separate releases, uninstalling only `bootc-operator`
> preserves the CRDs and configuration. Uninstalling `bootc-operator-crds`
> deletes the CRDs and all their instances.

## Regenerating the chart

Most Helm templates are generated from the kustomize manifests. After
changing CRDs, RBAC rules, deployments, or service accounts under
`config/`, regenerate the chart:

```bash
make helm
```

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `crds.install` | `true` | Install the CRD sub-chart |
| `image.repository` | `ghcr.io/bootc-dev/bootc-operator` | Operator image repository |
| `image.tag` | `""` (uses `appVersion`) | Operator image tag |
| `image.pullPolicy` | not set | Kubernetes image pull policy |
| `imagePullSecrets` | `[]` | List of pull secret references |
| `controller.extraArgs` | `[]` | Extra arguments for the controller |
