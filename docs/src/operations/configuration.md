# Configuring the operator

Configure the controller and daemon with a cluster-scoped
`BootcOperatorConfig`. Its typed `node.bootc.dev/v1alpha1` API keeps
administrator settings separate from installation manifests. Choose any valid
Kubernetes resource name; use at most one configuration in the cluster.

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
daemon use their existing defaults and explicit command-line arguments.
`cluster` is only the example name; the resource has no namespace. If more
than one instance exists, both processes stop at startup until an
administrator removes the extras.

## Supported settings

| Field under spec | Component | Default |
| --- | --- | --- |
| `controller.allowInsecureRegistry` | Controller | `false` |
| `controller.tagResolutionPeriodSeconds` | Controller | `300` (5 minutes) |
| `daemon.statusPollPeriodSeconds` | Daemon | `300` (5 minutes) |

Period values are positive whole seconds. Invalid types and zero or negative
periods are rejected by the API server. An empty `spec: {}` uses all defaults;
either component section may be omitted.
Use YAML booleans and integers directly, without string quotes.

`allowInsecureRegistry` lets the controller's tag resolver fall back to HTTP
for registries without TLS. The bink development workflow enables it for its
local registry. Registry credentials continue to use each pool's
`pullSecretRef`.

The daemon normally observes bootc status through filesystem notifications.
`statusPollPeriodSeconds` controls fallback polling when those notifications
are unavailable.

## Applying configuration

After installing or upgrading the operator, check for an existing configuration:

```shell
kubectl get bootcoperatorconfigs
```

If none exists, edit and apply the sample:

```shell
kubectl apply -f config/samples/bootc_v1alpha1_bootcoperatorconfig.yaml
kubectl get bootcoperatorconfig cluster -o yaml
```

The sample contains the existing defaults (`false`, `300`, and `300`); edit it
to set the values you need before applying it. If a configuration already
exists, edit that resource instead of creating another. The sample name
`cluster` can be changed.
Reapplying `install.yaml` will not overwrite the configuration.

Releases also provide a separate, optional `operator-config.yaml` asset with
the same default values. On a released installation, replace `vX.Y.Z` with
the installed release tag, then download and edit the file before applying it:

```shell
curl -fL -o operator-config.yaml \
  https://github.com/bootc-dev/bootc-operator/releases/download/vX.Y.Z/operator-config.yaml
# Edit operator-config.yaml, then:
kubectl apply -f operator-config.yaml
```

Check for an existing instance first as shown above. The optional asset is
not part of `install.yaml`, so routine operator upgrades leave administrator
settings alone.

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

More than one configuration, a missing CRD, denied API access, or a connection
failure stops startup with an error. Remove extra configurations, install the
matching CRD and permissions, or restore API connectivity before restarting.
A running process keeps its loaded settings during a later API outage.

## Reverting and upgrading

Restore a field's previous value, or remove it to use its schema default,
then restart the affected workload. To remove all overrides from this resource:

```shell
kubectl delete bootcoperatorconfig cluster
```

Replace `cluster` with the resource's actual name if you used a different one.
Then restart the affected controller and daemon pods as above. Deletion alone
does not change running processes. Explicit command-line flags still apply.

Reapplying installation manifests during an ordinary upgrade preserves the
configuration resource. For Helm installation and removal behavior, see the
[Helm guide](../helm.md).
