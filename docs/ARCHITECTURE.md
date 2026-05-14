# Bootc Operator -- Architecture

## Design Principles

1. No per-node mutations -- all OS changes flow through images
2. Not tied to OpenShift -- runs on vanilla K8s
3. Tight integration with bootc APIs (soft-reboot, staging, rollback)
4. Minimize API server load -- daemon watches a single per-node object
5. Clean CRD APIs that can be driven by a higher-level operator (e.g. MCO on
   OpenShift, or a multi-cluster management layer)

## Overview

Two binaries: a **controller** (Deployment) and a **daemon** (DaemonSet).

```
┌──────────────────────────────────────────────────────────┐
│                     Control Plane                        │
│                                                          │
│  ┌─────────────────────┐     ┌────────────────────────┐  │
│  │   BootcNodePool     │     │  BootcNode (per node)  │  │
│  │   (user-created)    │     │  (operator-managed)    │  │
│  │                     │     │                        │  │
│  │ spec:               │     │ spec:   ← controller   │  │
│  │   nodeSelector      │     │   desiredImage         │  │
│  │   image (tag/digest)│     │   desiredImageState    │  │
│  │   rollout config    │     │                        │  │
│  │   update policy     │     │ status: ← daemon       │  │
│  │                     │     │   booted image/digest  │  │
│  │ status:             │     │   staged image/digest  │  │
│  │   targetDigest      │     │   rollback image       │  │
│  │   node counts       │     │   conditions           │  │
│  │   conditions        │     │     (Idle)             │  │
│  └──────────┬──────────┘     └──────────┬─────────────┘  │
│             │ watches            r/w    │                │
│  ┌──────────▼───────────────────────────▼─────────────┐  │
│  │              Controller (Deployment)               │  │
│  │                                                    │  │
│  │  Pool Reconciler: resolves tags, selects nodes,    │  │
│  │    computes candidates, writes BootcNode.spec,     │  │
│  │    handles drain/cordon/uncordon,                  │  │
│  │    polls registries for tag updates                │  │
│  └────────────────────────────────────────────────────┘  │
│                                                          │
└──────────────────────────────────────────────────────────┘

┌──────────────────────────────────────────────────────────┐
│                       Each Node                          │
│                                                          │
│  ┌──────────────────────────────────────────────────┐    │
│  │           Daemon (DaemonSet pod)                 │    │
│  │                                                  │    │
│  │  Watches: its own BootcNode (single object)      │    │
│  │                                                  │    │
│  │  On spec change:                                 │    │
│  │    if desiredImage != booted → stage (locked)    │    │
│  │    if desiredImageState == Booted → reboot       │    │
│  │                                                  │    │
│  │  On bootc status change:                         │    │
│  │    (via fsnotify on /proc/1/root/ostree/bootc)   │    │
│  │    → update BootcNode.status                     │    │
│  │                                                  │    │
│  │  Runs: bootc switch, bootc upgrade, bootc        │    │
│  │    status, bootc rollback (via nsenter into      │    │
│  │    host mount namespace)                         │    │
│  └──────────────────────────────────────────────────┘    │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

## Node Interaction Model

The DaemonSet only runs on nodes managed by a pool. The Pool Reconciler
labels managed nodes with `bootc.dev/managed: ""` when creating their
BootcNode, and removes the label when the node leaves all pools. The
DaemonSet uses a `nodeSelector` for this label, so daemon pods are
automatically created and deleted by the scheduler as nodes are
registered/unregistered.

To keep API server load minimal, each daemon pod watches **exactly one
object**: its own BootcNode CRD (field-selected by node name). This is the sole
communication channel:

- **Controller → Daemon**: writes to `BootcNode.spec` (desired image,
  desired image state)
- **Daemon → Controller**: writes to `BootcNode.status` (bootc state,
  conditions)

Updates to the BootcNode should only happen only on state transitions (not
periodic heartbeats).

## CRDs

### BootcNodePool (cluster-scoped, user-created)

Defines a group of nodes and their desired OS image state.

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
    # User specifies tag, digest, or both
    ref: quay.io/example/myos:latest
  rollout:
    maxUnavailable: 1        # int or percentage string (e.g. "25%")
    paused: false            # when true, no new rollouts start
  disruption:
    rebootPolicy: AllowSoftReboot # RebootOnly (default), AllowSoftReboot
  pullSecretRef:
    name: my-pull-secret
    namespace: bootc-operator
status:
  deployedDigest: sha256:old789... # last digest fully rolled out
  targetDigest: sha256:abc123...   # what we're rolling out to
  updateAvailable: true            # targetDigest != deployedDigest
  nodeCount: 10
  updatedCount: 7
  updatingCount: 2
  degradedCount: 1
  conditions:
    - type: UpToDate
    - type: Degraded
```

Pool conditions and their reasons:

| Condition | Status | Reason            | Meaning                                           |
|-----------|--------|-------------------|---------------------------------------------------|
| UpToDate  | True   | AllUpdated        | All nodes are running targetDigest                |
| UpToDate  | False  | RolloutInProgress | Nodes are actively being updated                  |
| UpToDate  | False  | Paused            | Updates pending but pool is paused                |
| Degraded  | True   | NodeConflict      | Node selector overlaps with another pool          |
| Degraded  | True   | StagingFailed     | One or more nodes failed to stage                 |
| Degraded  | True   | NodeNotReady      | A node did not come back after reboot             |
| Degraded  | True   | DaemonStuck       | Daemon not responding on one or more nodes        |
| Degraded  | False  | OK                | No issues                                         |

The `UpToDate` condition is determined by the controller by comparing
`spec.desiredImage` vs `status.booted.imageDigest` across all nodes in the pool.
When the condition is `False`, the `message` field includes a breakdown of node
states (e.g. "5/10 updated; 2 staging, 2 staged, 1 rebooting") so the user can
see exactly what's happening without inspecting individual BootcNodes.

The `DaemonStuck` reason is set when one or more nodes have
`desiredImage != booted` but the daemon reports `Idle=True` (the
"Pending" state in the state table) for an extended period.

Note each node can belong to at most one BootcNodePool. If a node matches
multiple pool selectors, the controller sets `Degraded` with reason
`NodeConflict` on the conflicting pools rather than silently picking one.

### BootcNode (cluster-scoped, operator-managed)

Per-node object auto-created by the controller. Named after the Node it
represents. The controller writes `spec`, the daemon writes `status`.

```yaml
apiVersion: node.bootc.dev/v1alpha1
kind: BootcNode
metadata:
  name: worker-1           # matches Node name
  ownerReferences:
    - kind: BootcNodePool  # owned by pool
spec:                       # ← written by controller
  desiredImage: quay.io/example/myos@sha256:abc123
  desiredImageState: Staged  # Staged or Booted
  pullSecretRef:
    name: my-pull-secret
    namespace: bootc-operator
  pullSecretHash: sha256:e3b0c4...
status:                     # ← written by daemon
  booted:
    image: quay.io/example/myos@sha256:old789
    imageDigest: sha256:old789
    version: "9.4"
    timestamp: "2026-03-20T12:00:00Z"
    architecture: amd64
    softRebootCapable: true
    incompatible: false     # true if node has local mutations bootc can't manage
  staged:
    image: quay.io/example/myos@sha256:abc123
    imageDigest: sha256:abc123
    softRebootCapable: true
  rollback:
    image: quay.io/example/myos@sha256:xyz000
    imageDigest: sha256:xyz000
  conditions:
    - type: Idle
      status: "False"
      reason: Staged
      message: "Image staged, awaiting desiredImageState: Booted"
```

The `Idle` condition reports whether the daemon is actively doing work.
It does **not** claim whether the node is "up to date" -- that is
determined by the controller by comparing `spec.desiredImage` against
`status.booted.imageDigest`.

| Status | Reason        | Meaning                                             |
|--------|---------------|-----------------------------------------------------|
| True   | Idle          | Daemon has no active update cycle                   |
| False  | Staging       | Pulling/staging the image                           |
| False  | Staged        | Image staged, waiting for desiredImageState: Booted |
| False  | Rebooting     | Reboot in progress                                  |
| False  | StagingFailed | Something went wrong during staging                 |

## Daemon Logic

The daemon is intentionally simple -- driven by two inputs: the BootcNode
spec (from the controller) and local bootc status.

```
            desiredImage != booted
  Idle ────────────────────────────► Staging
  (True)                             (bootc switch --download-only)
    ▲                                    │
    │                               ok ──┴── error
    │                               │        │
    │                               ▼        ▼
    │                           Staged   StagingFailed
    │                               │
    │                   desiredImageState == Booted
    │                   && staged == desiredImage
    │                               │
    │                               ▼
    │                           Rebooting
    │                           (bootc switch --from-downloaded --apply)
    │                               │
    │                   ... node reboots ...
    │                   daemon restarts
    │                   reads bootc status
    └───────────────────────────────┘
```

The daemon sets `Idle=True` when `desiredImage == booted` (or on startup
if they match). It sets `Idle=False` with the appropriate reason when an
update cycle is in progress. It never claims whether the node is "up to
date" -- that determination is made by the controller.

On startup and on fsnotify event (see [Detecting bootc status
changes](#detecting-bootc-status-changes)), the daemon reads `bootc
status --json` and writes the result to `BootcNode.status`. This is
event-driven from bootc itself rather than polling.

## Daemon Permissions

The DaemonSet runs at the Privileged Pod Security level. The operator
namespace needs a PSA exemption for this. The pod runs with:

- `privileged: true` -- grants all Linux capabilities
- `hostPID: true` -- shares the host PID namespace

No host filesystem mount is needed. The daemon accesses the host
through two mechanisms, both available via `hostPID`:

- `nsenter -m/proc/1/ns/mnt` -- enters PID 1's mount namespace for
  executing bootc commands and triggering reboots. The `container`
  environment variable is filtered out so bootc sees the real host
  state rather than detecting a container context.
- `/proc/1/root/` -- resolves to PID 1's root filesystem for fsnotify
  watching and writing pull secrets to the host.

See the related [Future Enhancements](#future-enhancements) entry to improve
this.

## Pool Reconciler

The Pool Reconciler is the sole controller in the operator. It translates
the user's high-level intent (a pool of nodes running a specific image)
into concrete per-node actions. It owns the entire update lifecycle.

Watches: BootcNodePool, Node, BootcNode, Secret (for pull secrets).

All watches map events to the owning BootcNodePool key, so there is a
single `Reconcile()` function. Each invocation runs the full loop below.
Every step is idempotent -- the reconciler is safe to re-run at any point
regardless of what triggered it.

Watch-to-pool mapping:

- **BootcNodePool**: Direct -- the event *is* the pool.
- **BootcNode**: Follow `ownerReference` to the pool.
- **Node**: `EnqueueRequestsFromMapFunc` that enqueues two sets of pools:
  (1) pools whose `nodeSelector` matches the node's current labels, and
  (2) if a BootcNode exists for this node, the pool that owns it (from
  `ownerReference`). The second set is needed to handle label removal:
  when a label changes such that a node no longer matches its current
  pool, only the owning pool can clean up the BootcNode and remove the
  `bootc.dev/managed` label. All lookups are from cache (no API calls).
- **Secret**: `EnqueueRequestsFromMapFunc` -- list pools whose
  `pullSecretRef` references the changed Secret.

The Node watch uses predicates to filter out high-frequency noise:
only label changes, Ready condition changes, and `spec.unschedulable`
changes trigger reconciliation. Kubelet heartbeats, resource capacity
updates, and other frequent Node updates are thus ignored.

### Reconciliation loop

**1. Resolve target digest**

Determine the digest the pool should be running:

- **Digest ref** (e.g. `myos@sha256:abc`): Set `status.targetDigest`
  directly from the spec. No registry query needed.
- **Tag ref** (e.g. `myos:latest`): If enough time has passed since the
  last resolution (tracked in pool status), query the registry to
  resolve the tag. If the resolved digest differs from
  `status.targetDigest`, update it. Schedule the next resolution via
  `RequeueAfter`.

In both cases, set `status.updateAvailable = (targetDigest != deployedDigest)`.

**2. Sync pool membership**

Compute two sets:
- **matching nodes**: nodes whose labels match the pool's `nodeSelector`.
- **owned BootcNodes**: BootcNodes with an `ownerReference` to this pool.

For each matching node, look up the BootcNode with the same name from
cache. Then reconcile:

- **New match** (matching node, no BootcNode exists): Create a BootcNode
  (owned by this pool) with `spec.desiredImage` set to the pool's
  `targetDigest`. Label the node with `bootc.dev/managed` (which
  triggers DaemonSet pod scheduling). If the node is already running
  the target image, the daemon will report `Idle=True`. If not,
  it will begin staging immediately -- staging is non-disruptive (image
  pull only), and the disruptive reboot step is still gated by
  `maxUnavailable`.
- **Conflict** (matching node, BootcNode exists but owned by a different
  pool): Do not create a BootcNode for the contested node. Continue
  reconciling uncontested nodes normally through all steps. After
  membership sync completes, set the pool's `Degraded` condition with
  reason `NodeConflict` and a message identifying the conflicting
  pool(s).
- **No longer matching** (owned BootcNode whose node doesn't match, or
  whose node was deleted): Delete the BootcNode. Remove the
  `bootc.dev/managed` label if the node still exists (which triggers
  DaemonSet pod removal). If the node has the `bootc.dev/was-cordoned`
  annotation, restore the prior cordon state and remove the annotation.

Sync each BootcNode's spec fields from the pool: set `desiredImage` to
`targetDigest`, and copy `pullSecretRef` and `pullSecretHash` (if they
differ). When `desiredImage` changes, also reset `desiredImageState` to
`Staged` -- this revokes any pending reboot approval for the previous
image. When `targetDigest` changes, this causes all daemons to begin
staging in parallel. This is intentional -- staging is non-disruptive
(image pull only), and pre-staging everywhere means nodes are ready to
reboot as soon as `maxUnavailable` capacity allows.

**3. Drive per-node rollout state machine**

For each BootcNode owned by this pool, read its `status.conditions` and
act based on the current state. If `spec.rollout.paused` is true, do not
set `desiredImageState: Booted` on any new nodes (but let in-progress
staging complete).

The effective state of a BootcNode is determined by three fields:

- `spec.desiredImage` -- always set. Reflects the image this node should
  be running. Kept in sync with the pool's `targetDigest` (updated on
  all BootcNodes when `targetDigest` changes). After a successful
  update, it already matches the booted image -- no clearing needed.
- `spec.desiredImageState` -- set by the controller. `Staged` means the
  daemon should stage the image but not reboot. `Booted` means the daemon
  should apply the staged image and reboot. Set to `Booted` after drain
  completes.
- `status.conditions[Idle]` -- set by the daemon to report whether it
  is actively working. The daemon sets `Idle=True` when it has no
  active update cycle, and `Idle=False` with a reason when it does.

The controller determines whether a node is up to date by comparing
`spec.desiredImage` against `status.booted.imageDigest`. It does not
rely on the daemon's `Idle` condition for this -- a misbehaving daemon
that claims `Idle=True` while the images don't match will be detected
by the controller and surfaced at the pool level.

State determination:

| `desiredImage` vs booted | `Idle` condition     | Effective state | Reconciler action                                                 |
|--------------------------|----------------------|-----------------|-------------------------------------------------------------------|
| == booted                | True                 | Idle            | If in reboot slot: free slot only once node is Ready              |
| == booted                | False                | Settling        | Wait for daemon to set Idle=True; if persists, flag at pool level |
| != booted                | True                 | Pending         | Daemon should be working; if persists, flag at pool level         |
| != booted                | False, Staging       | Staging         | Wait for daemon (non-disruptive)                                  |
| != booted                | False, StagingFailed | StagingFailed   | Mark degraded                                                     |
| != booted                | False, Staged        | Staged          | If reboot slot available: assign slot; else wait                  |
| != booted                | False, Rebooting     | Rebooting       | Wait for node to come back                                        |

The pool has `maxUnavailable` **reboot slots**. A node enters a reboot
slot when the controller cordons it, drains it, and sets
`desiredImageState: Booted`. The node holds its slot until it reboots
into the desired image and the controller uncordons it, freeing the
slot for the next candidate.

Staging is non-disruptive (image pull only) and does not occupy a
reboot slot. Neither do Pending and Staged nodes -- they are still
serving workloads normally.

Error handling depends on the failure type:
- **Staging errors** (StagingFailed): likely node-specific (disk,
  network). The pool is marked `Degraded` but the rollout continues
  on other nodes.
- **Post-reboot errors** (node not Ready beyond timeout): likely
  image-specific. The rollout is halted -- no further nodes are set
  to `desiredImageState: Booted` until the error is resolved.
  Already-rebooting nodes finish, but no new nodes are cordoned.

The controller and daemon each own specific transitions:

```
  ┌────────┐ controller updates  ┌───────────┐ daemon stages ┌──────────┐
  │  Idle  ├────────────────────►│  Staging  ├──────────────►│  Staged  │
  │        │ desiredImage        │           │ successfully  │          │
  │        │                     │ (daemon   │◄──────────────┤ (waiting │
  │        │                     │  pulling) │ staged !=     │ for slot)│
  │        │                     │           │ desiredImage  │          │
  └────▲───┘                     └─────┬─────┘               └────┬─────┘
       │                               │                          │
       │                               │ error              slot  │
       │                               ▼                 assigned │
       │                      ┌────────────────┐                  │
       │                      │ StagingFailed  │                  │
       │                      └────────────────┘                  │
       │                                                          │
       │                                       ┌───────────┐      │
       │               node reboots,           │ Rebooting │      │
       └───────────────daemon restarts─────────┤           │◄─────┘
                                               │ (daemon   │
                                               │  reboots) │
                                               └───────────┘
```

Transition details:

- **Idle → Staging**: The daemon detects that `spec.desiredImage` no longer
  matches the booted image. It then sets `Idle=False reason=Staging`, and runs
  `bootc switch --download-only <desiredImage>` to stage the image in locked
  mode (the staged image will not be applied on an unexpected reboot).

- **Staging → Staged**: The daemon finishes `bootc switch --download-only`
  successfully and sets `Idle=False reason=Staged`. The staged image is locked
  and safe from unexpected reboots. If `desiredImage` changed during staging,
  the mismatch is caught in the Staged state (see Staged → Staging below).

- **Staging → StagingFailed**: The daemon's `bootc switch --download-only` failed. Sets
  `Idle=False reason=StagingFailed`. The node is marked degraded. Staging errors
  are likely node-specific (disk, network), so the rollout continues on other
  nodes.

- **Staged → Staging** (re-stage): If `staged.imageDigest != desiredImage`
  (because `desiredImage` changed while staging or while waiting for a
  reboot slot), the daemon goes back to Staging. It sets `Idle=False
  reason=Staging` and re-runs `bootc switch --download-only` with the new
  `desiredImage`.

- **Staged → Rebooting**: The controller assigns the node a reboot
  slot if one is available. It cordons the node and records prior
  cordon state in the `bootc.dev/was-cordoned` annotation. It starts
  an async drain goroutine using `k8s.io/kubectl/pkg/drain`. The
  goroutine blocks until drain completes (or is cancelled), then
  signals completion via a channel that re-enqueues the pool. On
  successful drain, the controller sets
  `BootcNode.spec.desiredImageState = Booted`. The daemon detects this
  and verifies `staged.imageDigest == desiredImage` before proceeding.
  If they match, it sets `Idle=False reason=Rebooting` and runs
  `bootc switch --from-downloaded --apply` to unlock the staged image
  and reboot into it. If they don't match (race with a `desiredImage`
  update), the daemon goes back to Staging instead.

- **Rebooting → Idle**: The node reboots into the new image. The daemon
  pod restarts, reads `bootc status --json`, and sets `Idle=True`. The
  controller detects that `desiredImage == booted` but keeps the reboot
  slot occupied until the node is Ready. Once Ready, it restores prior
  cordon state (uncordons only if the node was not already
  unschedulable before) and removes the annotation. This frees the
  reboot slot for the next candidate.

- **Node not Ready beyond timeout**: The node holds its reboot slot,
  naturally blocking further reboots. If the node does not become Ready
  within a timeout, it is marked degraded. Post-reboot failures are
  likely image-specific, so the rollout is halted -- no further nodes
  are set to `desiredImageState: Booted` until the error is resolved.

**4. Aggregate pool status**

Compute pool-level fields from the BootcNode statuses:
`nodeCount`, `updatedCount`, `updatingCount`, `degradedCount`.
Set pool conditions: `UpToDate`, `Degraded`.
If all nodes are up to date, set `deployedDigest = targetDigest` and
clear `updateAvailable`.

## Update Flow

```
User sets BootcNodePool.spec.image.ref = quay.io/example/myos:v2
    │
    ▼
Pool Reconciler: resolves :v2 → sha256:abc123
Pool Reconciler: stores in pool status.targetDigest
Pool Reconciler: updates desiredImage on ALL BootcNodes to sha256:abc123
    │
    ▼
All daemons: detect spec change, begin staging in parallel
All daemons: set Idle=False Reason=Staged (as each finishes)
    │
    ▼
Pool Reconciler: assigns reboot slot to a Staged node
  (cordons, drains, sets desiredImageState: Booted)
    │
    ▼
Daemon: detects desiredImageState: Booted, reboots
    │
    ▼
Node reboots into new image
Daemon: restarts, reads bootc status, sets Idle=True
    │
    ▼
Pool Reconciler: detects desiredImage == booted
Pool Reconciler: waits for node Ready, then frees reboot slot
  (uncordons)
Pool Reconciler: assigns freed slot to next Staged node
```

## Rollback, Pause, Cancel

- **Pause**: User sets `pool.spec.rollout.paused = true`. The Pool
  Reconciler stops picking new candidates and does not set
  `desiredImageState: Booted` on any new nodes.
  Nodes already mid-staging complete their staging. Tag resolution
  continues and `status.targetDigest` is kept current, so the user can
  see what's pending.

- **Resume**: User sets `pool.spec.rollout.paused = false`. The
  reconciler picks up where it left off: selects candidates, sets
  `desiredImageState: Booted` for already-staged nodes.

- **Cancel + rollback**: User changes `pool.spec.image` back to the
  previous digest. The reconciler updates `targetDigest` and sets
  `desiredImage` on all BootcNodes as usual. Nodes already running
  that image are Idle. Nodes that were updated to the new image go
  through the normal staging/reboot cycle.

## bootc Integration Points

All commands are run via `nsenter -m/proc/1/ns/mnt` to enter the host
mount namespace.

| Operator action | bootc invocation | Notes |
|----------------|-----------------|-------|
| Read status | `bootc status --json --format-version=1` | Parse Host struct |
| Stage update | `bootc switch --download-only <image>` | Stages but locks -- won't apply on unexpected reboot ([pending upstream](https://github.com/bootc-dev/bootc/issues/2137)) |
| Apply + reboot | `bootc switch --from-downloaded --apply` | Unlocks staged update and reboots |
| Apply + soft reboot | `bootc switch --from-downloaded --apply --soft-reboot=auto` | Unlocks and does userspace-only restart when possible |
| React to changes | fsnotify on `/proc/1/root/ostree/bootc` | See below |

### Detecting bootc status changes

On the host, the `bootc-status-updated.path` systemd path unit watches
`/ostree/bootc` (physically `/sysroot/ostree/bootc`) for mtime changes.
Whenever bootc performs an operation that changes status (switch, upgrade,
rollback, edit), it calls `update_mtime()` to touch this directory's
mtime, which triggers the path unit.

The daemon detects these changes using Go's fsnotify on
`/proc/1/root/ostree/bootc`. Since the DaemonSet has `hostPID: true`,
`/proc/1/root/` resolves to PID 1's root filesystem, giving full visibility into
the host mount tree. The mtime touch appears as a `CHMOD` event.

On receiving an event, the daemon re-reads `bootc status --json` via
nsenter and updates `BootcNode.status` if the state has changed.

The daemon also polls `bootc status --json` on a long interval (e.g. 5 minutes)
as a fallback in case an fsnotify event is missed.

Note: bootc's path unit mechanism may not work with composefs (the
`/ostree/bootc` directory does not exist). This is a known upstream issue. The
polling fallback covers this case.

## Pull Secret Propagation

BootcNodePool references a pull secret (`spec.pullSecretRef`). The
controller copies this reference into `BootcNode.spec.pullSecretRef` along
with a hash of the Secret's content (`spec.pullSecretHash`).

The daemon reads the Secret via the K8s API (one-shot GET, not a watch)
and writes the `.dockerconfigjson` key to `/run/ostree/auth.json` on the
host filesystem via nsenter. This is the highest-priority path in bootc's
auth file search order (`/run/ostree/` > `/etc/ostree/` >
`/usr/lib/ostree/`), so it cleanly overrides any persistent or vendor
auth config without mutating `/etc/`.

Using a `/run/` path means the file does not survive reboots, but this is
desirable: the daemon re-writes it on every startup and on every
BootcNode spec change, so it is always present before any `bootc upgrade`
runs. If the DaemonSet is removed, the credentials disappear on the next
reboot rather than lingering on disk.

When the Secret's content changes, the controller detects the change and
bumps the hash in BootcNode.spec. This triggers the daemon's existing
BootcNode watch, causing it to re-fetch the Secret and update the host
file.

This requires the daemon ServiceAccount to have `get` permission on Secrets in
the operator namespace.

## Future Enhancements

1. **Privilege separation**: The daemon could fork a privileged helper early on
   and then drop privileges. The unprivileged main process (API server watch,
   state machine) would communicate with the helper via a Unix socket. Only
   the helper would execute nsenter operations and only knows how to execute
   specific commands.

2. **Health checks and automatic rollback**: When enabled, monitors node
   health and automatically roll back if unhealthy. Simplest is NotReady,
   but could integrate with systemd's Automatic Boot Assessment (i.e.
   `boot-complete.target`) for more customization.

3. **Maintenance windows**: Allow pools to specify time windows during which
   reboots are permitted (e.g. weekends, off-peak hours). Staging would
   still happen immediately, but the reconciler would only set
   `desiredImageState: Booted` when the current time falls within the
   window. Similar to kured/Zincati.

4. **Pre-staging while paused**: A mode where pausing blocks reboots but
   allows staging to proceed on all target nodes. This way, when the user
   unpauses, nodes are already staged and can drain and reboot immediately
   without waiting for image pulls.

5. **Signature policy enforcement**: Allow users to require signature
   verification of OS update payloads.

6. **Pull-through caching**: When enabled, bootc is actually pointed at a
   pullspec we own, and we cache the layers ourselves. Need to make sure it
   doesn't conflict with signature policy enforcement feature.

7. **Cross-pool rollout ordering**: Allow a pool to declare a dependency
   on another pool (e.g. `dependsOn: workers`). The reconciler would
   gate rollout on the dependency pool reaching `UpToDate=True` first.
   The primary use case is updating worker nodes before control plane
   nodes. Without this, users must manually sequence pool updates or
   rely on a higher-level operator to coordinate. This is intentionally
   deferred -- two independent pools cover the common case, and
   cross-pool ordering adds coordination complexity (handling degraded
   dependencies, cycles, multi-phase chains).
