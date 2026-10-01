# Helm Chart

## Prerequisites

- Kubernetes >= 1.34
- Helm 3

## Install

```bash
helm install bootc-operator ./chart/bootc-operator \
  --create-namespace --namespace bootc-operator
```

By default the chart uses the image
`ghcr.io/bootc-dev/bootc-operator:<appVersion>` where `appVersion` is
defined in `Chart.yaml`.

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
| `image.repository` | `ghcr.io/bootc-dev/bootc-operator` | Operator image repository |
| `image.tag` | `""` (uses `appVersion`) | Operator image tag |
| `image.pullPolicy` | not set | Kubernetes image pull policy |
| `imagePullSecrets` | `[]` | List of pull secret references |
| `controller.extraArgs` | `[]` | Extra arguments for the controller |
