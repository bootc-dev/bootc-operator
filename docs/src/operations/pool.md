# Managing a pool

## Creating a pool

A `BootcNodePool` tells the operator which nodes to manage and what OS image
they should be running. Create one by applying a manifest to your cluster:

```yaml
apiVersion: node.bootc.dev/v1alpha1
kind: BootcNodePool
metadata:
  name: workers
spec:
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/worker: ""
  image:
    ref: ghcr.io/bootc-dev/bink/node:latest
```

The `nodeSelector` field follows the standard Kubernetes label selector format.
Each node can belong to at most one pool, if a node matches multiple selectors,
the affected pools are marked `Degraded` with reason `NodeConflict`.

The image in ref can be specified by digest or by tag. If it is specified by tag
the bootc-operator controller will periodically checks for update. If a new image
version under that tag is detected, then a rollout is started by patching the new
digest in the bootc node spec.

Once the pool exists, the operator creates a `BootcNode` for each matching node
and begins staging the image.

## Updating the OS image

To roll out a new OS image across the pool, update `spec.image.ref` to a new
digest:

```shell
kubectl patch bootcnodepool workers --type merge -p \
  '{"spec":{"image":{"ref":"ghcr.io/bootc-dev/bink/node@sha256:newdigest..."}}}'
```

The operator stages the new image on each node, drains workloads, and reboots
nodes according to the rollout settings. Nodes that are already running the
target digest are left untouched.

## Monitoring a rollout

The `BootcNodePool` status shows the overall rollout progress:

```shell
kubectl get bootcnodepool workers
```

The columns `Nodes`, `Updated`, `Updating`, and `Degraded` give a quick
summary. For more detail:

```shell
kubectl get bootcnodepool workers -o yaml
```

```yaml
status:
  targetDigest: sha256:9ce7d6d15b8558c226b4c41f3b27bf1722897b0c8a66c7a84a2877bca8d049f7
  nodeCount: 10
  updatedCount: 7
  updatingCount: 2
  degradedCount: 1
  conditions:
  - type: UpToDate
    status: "False"
    reason: RolloutInProgress
    message: "7/10 updated; 2 staging, 1 rebooting"
```

The `UpToDate` condition is `True` when all nodes in the pool are running
the target digest.

Individual node progress is available through `BootcNode` resources:

```shell
kubectl get bootcnodes
```

Each `BootcNode` status reflects the output of `bootc status` on that node:
the booted image, any staged image, and the rollback entry.

## Pausing and resuming

To pause a rollout (nodes already staging will complete, but no new reboots
start):

```shell
kubectl patch bootcnodepool workers --type merge -p '{"spec":{"rollout":{"paused":true}}}'
```

To resume:

```shell
kubectl patch bootcnodepool workers --type merge -p '{"spec":{"rollout":{"paused":false}}}'
```

## Using a pull secret

If the OS image is hosted in a private registry that requires authentication,
create a Kubernetes Secret of type `kubernetes.io/dockerconfigjson` and
reference it from the pool:

```shell
kubectl create secret docker-registry my-pull-secret \
  --namespace=bootc-operator \
  --docker-server=registry.example.com \
  --docker-username=user \
  --docker-password=pass
```

```yaml
apiVersion: node.bootc.dev/v1alpha1
kind: BootcNodePool
metadata:
  name: workers
spec:
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/worker: ""
  image:
    ref: registry.example.com/my-org/node:latest
  pullSecretRef:
    name: my-pull-secret
    namespace: bootc-operator
```

Both `name` and `namespace` are required in `pullSecretRef`.

The operator uses the secret in two places:

- **Tag resolution**: the controller authenticates against the registry when
  resolving a tag to a digest.
- **Node pull**: the daemon writes the credentials to `/run/ostree/auth.json`
  on each managed node, which is the highest-priority path in bootc's auth
  search order. Because `/run` is a tmpfs, the credentials do not persist
  across reboots; the daemon re-writes them on every reconcile.

If the referenced secret is missing or does not contain the
`.dockerconfigjson` key, the pool is marked `Degraded` with reason
`SecretError` and no nodes are created or updated until the secret is
available.

The operator watches the secret for changes. When the secret content is
updated (for example, after a credential rotation), the new credentials are
automatically propagated to all managed nodes without requiring any change
to the pool.

## Rolling back

To roll back, change `spec.image.ref` to the previous digest. Nodes already
running that image are left alone. Nodes that were updated go through the
normal staging and reboot cycle to return to the previous image.
