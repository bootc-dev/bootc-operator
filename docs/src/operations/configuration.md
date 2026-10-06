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

Installation does not create or overwrite your configuration. Without one,
the operator uses its defaults and command-line arguments. `cluster` is an
example name; the resource has no namespace.

## Supported settings

| Field under spec | Component | Built-in default |
| --- | --- | --- |
| `controller.allowInsecureRegistry` | Controller | `false` |
| `controller.pullSecretNamespaces` | Controller | `[]` (operator namespace only) |
| `controller.tagResolutionPeriodSeconds` | Controller | `300` (5 minutes) |
| `daemon.statusPollPeriodSeconds` | Daemon | `300` (5 minutes) |

Period values are positive whole seconds. Invalid types and zero or negative
periods are rejected by the API server. Omitted fields, including an empty
`spec: {}`, use CLI values or built-in defaults. Either section may be omitted.
Use YAML booleans and integers directly, without string quotes.

`allowInsecureRegistry` lets the controller's tag resolver fall back to HTTP
for registries without TLS. The bink development workflow enables it for its
local registry. Registry credentials continue to use each pool's
`pullSecretRef`.

The daemon normally observes bootc status through filesystem notifications.
`statusPollPeriodSeconds` controls fallback polling when those notifications
are unavailable.

## Pull secret namespaces

Allow pools to use pull secrets from additional namespaces with
`controller.pullSecretNamespaces`. The operator's own namespace is always
allowed. Omit the field to use CLI settings, or set `[]` for no extras even
when CLI flags list namespaces. Use unique, valid namespace names. Add this
setting to your existing configuration:

```yaml
spec:
  controller:
    pullSecretNamespaces:
      - team-a
```

This setting does not grant permissions. Create the namespace and grant the
controller access to its Secrets before adding it. For a Helm release named
`bootc-operator` in namespace `bootc-operator`, apply this example in `team-a`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: bootc-operator-pull-secrets
  namespace: team-a
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: bootc-operator-pull-secrets
  namespace: team-a
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: bootc-operator-pull-secrets
subjects:
  - kind: ServiceAccount
    name: bootc-operator-controller
    namespace: bootc-operator
```

Adjust the ServiceAccount name and namespace for your installation;
`install.yaml` uses `bootc-operator-controller-manager` instead.
For Helm-managed permissions, see
[Helm configuration](../helm.md#pool-pull-secret-namespaces).

After granting permissions and applying the configuration, [restart the
controller](#applying-configuration). To remove access, remove the namespace
from the CR list (or Helm/CLI setting when the CR field is omitted), then
restart before deleting its Role and RoleBinding.

## Applying configuration

To use custom settings on the first startup with Helm, follow
[Configure before first startup](../helm.md#configure-before-first-startup).

After installing or upgrading the operator, check for an existing configuration:

```shell
kubectl get bootcoperatorconfigs
```

If none exists, edit and apply the sample:

```shell
kubectl apply -f config/samples/bootc_v1alpha1_bootcoperatorconfig.yaml
kubectl get bootcoperatorconfig cluster -o yaml
```

The sample explicitly sets default values, which override matching CLI flags.
Edit it or remove fields you want to leave to CLI settings before applying it.
If a configuration already exists, edit that resource instead of creating another.

Releases also provide a separate, optional `operator-config.yaml` asset with
the same default values. On a released installation, replace `vX.Y.Z` with
the installed release tag, then download and edit the file before applying it:

```shell
curl -fL -o operator-config.yaml \
  https://github.com/bootc-dev/bootc-operator/releases/download/vX.Y.Z/operator-config.yaml
# Edit operator-config.yaml, then:
kubectl apply -f operator-config.yaml
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

For Helm installations, use `deployment/bootc-operator-controller` instead of
`deployment/bootc-operator-controller-manager` in the restart and status commands.
Use the workload names and namespace from your installation if customized.
Daemon pods run only on nodes selected by a pool. New pods read the current
configuration automatically. Restarting the controller alone does not
reconfigure existing daemon pods.

## Precedence and startup failures

For each supported setting, the value is selected in this order:

1. A field explicitly set in `BootcOperatorConfig`.
2. An explicitly supplied command-line flag when the CR field is omitted.
3. The built-in default.

Explicit `false` and `[]` in the CR override CLI values. For example,
`pullSecretNamespaces: []` permits only the operator's namespace, even if
Helm or CLI flags list extra namespaces. Omitted duration fields retain CLI
duration syntax and precision, including fractional seconds.

More than one configuration, a missing CRD, denied API access, or a connection
failure stops startup with an error. Remove extra configurations, install the
matching CRD and permissions, or restore API connectivity before restarting.
A running process keeps its loaded settings during a later API outage.

## Reverting and upgrading

Restore a field's previous value, or remove it to use the CLI value or built-in
default, then restart the affected workload. To remove all overrides from this resource:

```shell
kubectl delete bootcoperatorconfig cluster
```

Replace `cluster` with the resource's actual name if you used a different one.
Then restart the affected controller and daemon pods as above. Deletion alone
does not change running processes. Explicit command-line flags still apply.

Reapplying installation manifests during an ordinary upgrade preserves the
configuration resource. For Helm installation and removal behavior, see the
[Helm guide](../helm.md).
