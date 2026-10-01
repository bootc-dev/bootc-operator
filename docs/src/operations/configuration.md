# Configuring the operator

Configure the controller and daemon with a cluster-scoped
`BootcOperatorConfig` named `cluster`. Its typed `node.bootc.dev/v1alpha1`
API keeps administrator settings separate from installation manifests.

For example:

```yaml
apiVersion: node.bootc.dev/v1alpha1
kind: BootcOperatorConfig
metadata:
  name: cluster
spec:
  controller:
    tagResolutionPeriodSeconds: 60
  daemon:
    statusPollPeriodSeconds: 60
```

The operator installation includes the CRD and read permissions. The
configuration instance is optional, administrator-owned, and is not created or
overwritten by the default installation. With no instance, the controller and
daemon use their existing defaults and explicit command-line arguments. The
only supported instance name is `cluster`; it has no namespace.

## Supported settings

| Field under spec | Component | Default |
| --- | --- | --- |
| `controller.allowInsecureRegistry` | Controller | `false` |
| `controller.tagResolutionPeriodSeconds` | Controller | `300` (5 minutes) |
| `daemon.statusPollPeriodSeconds` | Daemon | `300` (5 minutes) |

Period values are positive whole seconds. Invalid types, zero or negative
periods, and other instance names are rejected by the API server. An empty
`spec: {}` uses all defaults; either component section may be omitted.
Use YAML booleans and integers directly, without string quotes.

`allowInsecureRegistry` lets the controller's tag resolver fall back to HTTP
for registries without TLS. The bink development workflow enables it for its
local registry. Registry credentials continue to use each pool's
`pullSecretRef`.

The daemon normally observes bootc status through filesystem notifications.
`statusPollPeriodSeconds` controls fallback polling when those notifications
are unavailable.

Logging, leader election, health-probe binding, kubeconfig, and node identity
use their existing command-line flags or environment variables. Run the
relevant binary with `--help` for its process options. The stock Deployment
explicitly enables leader election. Changing the health-probe port requires
matching changes to its readiness and liveness probes.

## Installing and applying configuration

After installing or upgrading the operator, wait for the CRD before applying
the example:

```shell
kubectl wait --for=condition=Established crd/bootcoperatorconfigs.node.bootc.dev --timeout=1m
kubectl apply -f config/samples/bootc_v1alpha1_bootcoperatorconfig.yaml
kubectl get bootcoperatorconfig cluster -o yaml
```

The configuration is read once when each controller or daemon process starts.
Editing the resource does not restart pods or change running processes.
Restart the workload whose settings changed. The example above changes both:

```shell
kubectl -n bootc-operator rollout restart deployment/bootc-operator-controller-manager
kubectl -n bootc-operator rollout status deployment/bootc-operator-controller-manager --timeout=3m
kubectl -n bootc-operator rollout restart daemonset/bootc-operator-daemon
kubectl -n bootc-operator rollout status daemonset/bootc-operator-daemon --timeout=3m
```

Use the workload names and namespace from your installation if customized.
Daemon pods run only on nodes selected by a pool. New pods read the current
configuration automatically. Restarting the controller alone does not
reconfigure existing daemon pods.

## Precedence and startup failures

For each supported setting, the value is selected in this order:

1. An explicitly supplied command-line flag.
2. The corresponding configuration field.
3. The existing default when configuration is absent.

An explicit `--allow-insecure-registry=false` overrides a resource that sets
`allowInsecureRegistry: true`. Existing duration flags keep their duration
syntax, including fractional seconds, independently of the whole-second API
fields.

Both binaries use their existing service account or kubeconfig to read
configuration directly from the API with a 10-second deadline for that read.
The controller and daemon service accounts need only `get` access to
`bootcoperatorconfigs/cluster`. The CRD and its read
permissions must be installed even when the configuration instance is absent.

A missing configuration instance uses defaults. A missing CRD, denied API
access, or a connection failure stops startup with a contextual error.
Install the matching CRD/RBAC or restore API connectivity before restarting.
A running process keeps its loaded settings during a later API outage.
`--help` does not require Kubernetes API access.

## Configuration flow

Each process follows this sequence independently. The daemon also requires
`NODE_NAME` before loading Kubernetes credentials. Configuration errors stop
startup even when CLI flags override every configurable setting.

```text
Parse CLI flags and defaults; record explicitly supplied flags
  |
  v
Load Kubernetes credentials (kubeconfig or service account)
  |
  v
Uncached GET BootcOperatorConfig/cluster (10-second deadline)
  |
  +-- API error (CRD/auth/network/timeout) --> log error; exit
  |
  +-- Found --> validate singleton name and positive periods
  |               |
  |               +-- Invalid --> log error; exit
  |               |
  |               +-- Valid -------------------------+
  |                                                  |
  +-- Instance absent: no CR values -----------------+
                                                     |
                                                     v
                  Resolve each field: explicit CLI > CR > default
                                                     |
                                                     v
                         Log configuration source and CLI overrides
                                                     |
                                                     v
                   Create manager; pass resolved values to component
                                                     |
                                                     v
                         Start controller or daemon with that snapshot
```

Changes take effect when the affected process next starts:

```text
Edit/delete CR --> API changes --> running processes keep their snapshot
Manual restart --> startup flow above --> new values (or defaults) apply
```

## Reverting and upgrading

Restore a field's previous value, or remove it to use its schema default,
then restart the affected workload. To remove all overrides from this resource:

```shell
kubectl delete bootcoperatorconfig cluster
```

Then restart the affected controller and daemon pods as above. Deletion alone
does not change running processes. Explicit command-line flags still apply.

Reapplying installation manifests during an ordinary upgrade preserves the
configuration resource. Uninstalling the operator's CRDs deletes their
instances too. Export the configuration before uninstalling if you intend to
restore it after reinstalling:

```shell
kubectl get bootcoperatorconfig cluster -o yaml > operator-config-backup.yaml
```

Restore its `spec` in a fresh resource, omitting server-managed metadata such
as `uid`, `resourceVersion`, and `managedFields`.

The API currently serves `v1alpha1`. Versioning defines the configuration
schema; it does not retain configuration history or automatically roll back
settings. Operator status, live configuration reload, and feature gates are
not provided by this resource.
