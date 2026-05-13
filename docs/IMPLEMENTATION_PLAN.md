# Bootc Operator -- Implementation Plan

This document breaks the implementation into milestones.
[bink](https://github.com/alicefr/bink) is used for e2e tests starting
from Milestone 2 (a tool for spinning up K8s clusters backed by bootc
VMs with a local registry).

## Milestone 1: Project Bootstrap & CRD Types

### 1a. Project scaffold ✅

Use kubebuilder to generate the initial project structure:

```bash
kubebuilder init --domain bootc.dev --repo github.com/jlebon/bootc-operator
kubebuilder create api --group bootc --version v1alpha1 --kind BootcNodePool \
  --resource --controller
kubebuilder create api --group bootc --version v1alpha1 --kind BootcNode \
  --resource --controller=false
```

BootcNodePool gets `--controller` because the Pool Reconciler lives
there. BootcNode gets `--controller=false` because the daemon is a
separate binary and the Pool Reconciler adds its own watch on
BootcNode.

### 1b. BootcNodePool types ✅

- Add `+kubebuilder:resource:scope=Cluster` marker
- Flesh out `BootcNodePoolSpec` (nodeSelector, image, rollout,
  disruption, pullSecretRef)
- Flesh out `BootcNodePoolStatus` (targetDigest, deployedDigest,
  updateAvailable, counts, conditions)
- Pool condition type/reason constants (`UpToDate`, `Degraded` with
  all their reasons)

### 1c. BootcNode types ✅

- Add `+kubebuilder:resource:scope=Cluster` marker
- Flesh out `BootcNodeSpec` (desiredImage, desiredImageState,
  pullSecretRef, pullSecretHash)
- Flesh out `BootcNodeStatus` (booted, staged, rollback using a shared
  `ImageInfo` struct, conditions)
- Node condition type/reason constants (`Idle` with its reasons)
- `DesiredImageState` enum type (`Staged`, `Booted`)

### Validation ✅

- envtest: verify CRDs load, objects can be created/retrieved with full
  spec and status, and enum validation rejects invalid values.

## Milestone 2: E2E Test Harness

Set up the bink-based e2e test infrastructure used by all subsequent milestones.
Each e2e test gets its own bink cluster for full isolation and potential for
parallelization.

### 2a. Harness infrastructure ✅

Create `test/e2e/` with a helper package that provides a single entry
point:

```go
func TestCRDSmoke(t *testing.T) {
    env := e2eutil.New(t, e2eutil.Config{}) // starts cluster, deploys CRDs, registers t.Cleanup
    // env.Client ready to use
    // cluster torn down automatically when test ends
}
```

`New(t, cfg)` handles the full lifecycle transparently. The `Config`
struct maps to bink's cluster configuration (e.g. cluster name, VM
memory) and can be extended as needed by later milestones.

- Finds `bink` binary on PATH (overridable via `BINK_PATH` env var).
  Fails the test if not found.
- Runs `bink cluster start` with a unique cluster name per test
  (e.g. `e2e-<short-hash>`, overridable via `Options`)
- Waits for node1 to be Ready
- Runs `bink api expose` to generate a kubeconfig
- Applies the kustomize manifest from `config/default/` (at M2 this
  is just CRDs; as later milestones add the controller Deployment and
  daemon DaemonSet, e2e tests automatically get the full operator)
- Builds a controller-runtime client
- Registers `t.Cleanup` → `bink cluster stop --remove-data`

### 2b. Smoke test ✅

First e2e test: create a BootcNodePool and BootcNode, verify they are
accepted and retrievable. Validates both the harness and the CRDs on
a real cluster.

### Validation ✅

- Run the smoke test, verify it passes end-to-end.
- Verify the cluster is torn down after test completes (including on
  failure).

## Milestone 3: Controller — Pool Reconciler

Build the controller first since it is the more complex piece and its
interaction with BootcNode objects can be fully tested via envtest by
simulating daemon responses (writing BootcNode status directly). This
also nails down exactly what the controller writes to BootcNode.spec
and expects from BootcNode.status, giving the daemon a clear contract
to implement.

Validation is mostly envtest-based since the daemon doesn't exist yet
and the controller's logic (membership sync, rollout state machine,
status aggregation) can be fully exercised by simulating BootcNode
status updates. E2e tests become more valuable in Milestone 4 when
the full controller+daemon loop can be tested end-to-end.

### 3a. Pool membership sync ✅

- Watch BootcNodePool, Node (with predicates: label changes, Ready
  condition, `spec.unschedulable` only), BootcNode (via ownerReference)
- Match nodes via `nodeSelector`, create BootcNode with ownerReference
  to pool
- Label nodes `bootc.dev/managed: ""`
- Handle node leaving pool (label removed or node deleted): delete
  BootcNode, remove `bootc.dev/managed` label, restore cordon state
  via `bootc.dev/was-cordoned` annotation
- Conflict detection: node matches multiple pools →
  `Degraded/NodeConflict` on conflicting pools, skip rollout steps

**Validation:** ✅

- envtest: create Nodes and a BootcNodePool, verify BootcNodes are
  created with correct ownerReference and desiredImage. Verify nodes
  are labeled `bootc.dev/managed`. Remove a node's matching label,
  verify BootcNode is deleted and label removed. Create overlapping
  pools, verify `Degraded/NodeConflict`.
- e2e: enhance the smoke test to deploy the controller, create a
  BootcNodePool with worker nodes, and verify BootcNodes appear and
  nodes are labeled. This exercises the e2e harness for the first time
  with a running operator and validates it functions in a real cluster.

### 3b. Digest-only rollout state machine

- Set `targetDigest` directly from `spec.image.ref` (digest refs only;
  tag resolution deferred to Milestone 5)
- Sync `desiredImage` on all owned BootcNodes; reset `desiredImageState`
  to `Staged` when `desiredImage` changes
- Reboot slot accounting based on `maxUnavailable`
- Drive transitions per the state table: detect Staged → cordon (record
  prior state in `bootc.dev/was-cordoned`) → drain (using
  `k8s.io/kubectl/pkg/drain`, 90s per-attempt timeout, requeue on
  failure) → set `desiredImageState: Booted`
- Post-reboot: detect `desiredImage == booted` + node Ready → uncordon
  (respecting `was-cordoned`) → free slot
- `spec.rollout.paused`: block new reboot slot assignments; let
  in-progress staging complete
- Error handling: StagingFailed → mark pool Degraded, continue rollout
  on other nodes; post-reboot NotReady beyond timeout → halt rollout
  (no new `desiredImageState: Booted`)

**Validation (envtest):**

- Simulate a 3-node rollout with `maxUnavailable: 1` by writing
  BootcNode status transitions (Staging → Staged → Rebooting → Idle).
  Verify only one node is cordoned/drained at a time.
- Verify `desiredImageState` resets to `Staged` when `desiredImage`
  changes mid-rollout.
- Verify pause blocks new reboot slot assignments but lets staging
  complete. Resume and verify rollout continues.
- Simulate `StagingFailed` on one node, verify rollout continues on
  others. Simulate post-reboot `NotReady`, verify rollout halts.

### 3c. Pool status aggregation

- Compute `nodeCount`, `updatedCount`, `updatingCount`, `degradedCount`
- `UpToDate` condition with reasons: `AllUpdated`, `RolloutInProgress`,
  `Paused`
- `Degraded` condition with reasons: `NodeConflict`, `StagingFailed`,
  `NodeNotReady`, `DaemonStuck`, `OK`
- Message on `UpToDate=False` includes breakdown (e.g. "5/10 updated;
  2 staging, 2 staged, 1 rebooting")
- Set `deployedDigest = targetDigest` when all nodes match

**Validation (envtest):**

- Verify `nodeCount`, `updatedCount`, `updatingCount`, `degradedCount`
  reflect BootcNode states accurately.
- Verify `UpToDate` condition transitions: `RolloutInProgress` during
  update, `Paused` when paused, `AllUpdated` when complete.
- Verify `Degraded` condition reasons: `StagingFailed`, `NodeNotReady`,
  `DaemonStuck`, `NodeConflict`, and `OK` when clear.
- Verify `UpToDate=False` message includes state breakdown.
- Verify `deployedDigest = targetDigest` when all nodes match.

## Milestone 4: Daemon

### 4a. DaemonSet manifest + skeleton binary

- DaemonSet manifest: `privileged: true`, `hostPID: true`,
  `nodeSelector: bootc.dev/managed: ""`, ServiceAccount + RBAC for
  BootcNode read/write
- Minimal daemon binary that starts and exits cleanly (no-op)

**Validation (e2e):** enhance the existing e2e test to include the
DaemonSet, label a node with `bootc.dev/managed`, and verify the daemon
pod starts on that node.

### 4b. Core loop

- Binary identifies its node name (downward API env var)
- Watches its single BootcNode via field selector on `metadata.name`
- Parses `bootc status --json --format-version=1` via
  `nsenter -m/proc/1/ns/mnt` (filtering `container` env var)
- Writes `BootcNode.status` (booted/staged/rollback fields + `Idle`
  condition)
- On startup: read bootc status, populate status, set `Idle=True` if
  `desiredImage == booted`

**Validation (e2e):** enhance the existing e2e test to verify the
daemon populates BootcNode status from `bootc status`.

### 4c. State machine

- Detect `spec.desiredImage != booted` → set `Idle=False reason=Staging`,
  run `bootc switch <desiredImage>` (no `--download-only` for now;
  [pending upstream](https://github.com/bootc-dev/bootc/issues/2137))
- On success → set `Idle=False reason=Staged`
- On error → set `Idle=False reason=StagingFailed`
- Detect `spec.desiredImageState == Booted` + staged matches desired →
  set `Idle=False reason=Rebooting`, run `bootc switch --apply` and
  reboot
- Handle re-stage: if `staged.imageDigest != desiredImage` in Staged
  state, go back to Staging

**Validation (e2e):** enhance the existing e2e test to push an updated
bootc image to the local registry, set `desiredImage` to the new image,
and verify the daemon stages it and reports `Idle=False reason=Staged`.
Then set `desiredImageState: Booted`, verify the node reboots into the
new image and the daemon reports `Idle=True`.

### 4d. fsnotify + polling

- fsnotify watch on `/proc/1/root/ostree/bootc` for CHMOD events
- Fallback: also try `/proc/1/root/sysroot/state/deploy` (for composefs
  where `/ostree/bootc` doesn't exist)
- Polling fallback every ~5 minutes
- On event: re-read bootc status, update BootcNode.status if changed

**Validation (e2e):** add a test that triggers an external bootc status
change on the host (e.g. via SSH) and verifies the daemon detects it
and updates BootcNode.status within a bounded time window (shorter than
the polling interval, proving fsnotify is working).

## Milestone 5: Tag Resolution & Pull Secrets

### 5a. Tag resolution

- Registry client (e.g. `go-containerregistry`) to resolve tags to
  digests
- On pool reconcile: if `image.ref` is a tag and enough time has elapsed
  since last resolution, query registry
- Store resolved digest in `status.targetDigest`, track resolution time
- Set `updateAvailable = (targetDigest != deployedDigest)`
- Schedule next resolution via `RequeueAfter`

**Validation (e2e):** add a test that pushes two image versions to the
local registry under a tag, creates a pool referencing the tag, and
verifies the tag resolves and the initial rollout completes. Then push
a new version to the same tag, verify `updateAvailable` surfaces, and
the updated image rolls out to the node.

### 5b. Pull secret propagation

- Watch Secrets referenced by pools (`EnqueueRequestsFromMapFunc`)
- Controller copies `pullSecretRef` + content hash to `BootcNode.spec`
- Daemon: on `pullSecretHash` change, GET the Secret, write
  `.dockerconfigjson` to `/run/ostree/auth.json` on the host via nsenter
- Daemon ServiceAccount gets `get` on Secrets in operator namespace

**Validation (e2e):** add a test that creates a pull-secret-protected
image in the registry, verifies the operator propagates the secret, and
the daemon uses it for staging.

## Milestone 6: Packaging & CI

A lot of this can and should be done in parallel with the earlier milestones.
Once we have a test harness and a basic E2E test, we should be unblocked on
hooking up CI.

- Render a static manifest from `config/default/` kustomize for
  single-command install (`kubectl create -f https://...`) without
  needing the repo cloned
- CI pipeline running bink-based e2e tests
- README with quickstart
