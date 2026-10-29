# Karpenter integration

bootc-operator is aware of [Karpenter](https://karpenter.sh/) and coordinates
with it during node updates to prevent unnecessary node replacement,
voluntary disruption, and concurrent reboot handling.

## Why this matters

Karpenter monitors nodes for health and may decide to replace or disrupt a
node that appears unhealthy or underutilized — for example, one that is
cordoned, being drained, or not yet Ready after a reboot. All of those
conditions arise naturally during a bootc-operator update window, so without
coordination Karpenter could race with the operator and terminate a node that
is in the middle of a legitimate OS update.

Karpenter respects two node-level annotations:

- `karpenter.sh/do-not-repair: "true"` — prevents Node Auto Repair from
  replacing a node that has unhealthy conditions.
- `karpenter.sh/do-not-disrupt: "true"` — prevents voluntary disruption
  actions (consolidation, drift) from selecting this node.

> **Note:** `karpenter.sh/do-not-repair` was introduced in Karpenter v1.15.0.
> That is the minimum required version for full bootc-operator Karpenter integration.

bootc-operator sets both annotations automatically on Karpenter-managed nodes
when assigning a reboot slot, and keeps them through node reboot and health
verification. Where the associated `NodeClaim` exists, it also sets the
annotations there. This is best-effort coordination, not a distributed lock.

## How it works

Karpenter protection is applied when a Karpenter-owned node is assigned a
reboot slot. To do this, bootc-operator sets `do-not-repair` and
`do-not-disrupt` on the Node and, where available, the associated NodeClaim.
This protection is not applied during the staging process. This protection
persists through drain, reboot, and post-boot health verification.

Before assigning a reboot slot or starting a drain, the controller checks
whether Karpenter has already committed any lifecycle operation that would
conflict with the bootc update. It checks:

- Node taints `karpenter.sh/rebooting:NoSchedule` and
  `karpenter.sh/disrupted:NoSchedule`
- Node `DeletionTimestamp` (node is being terminated)
- NodeClaim `DeletionTimestamp` (NodeClaim is being deleted)
- NodeClaim `status.conditions`: `Rebooting=True`, `DisruptionReason=True`,
  or `InstanceTerminating=True`

If any of these signals is active, bootc-operator defers and retries. It
will never remove a Karpenter taint.

If the Karpenter NodeClaim owner reference is present but the NodeClaim object
does not exist, bootc-operator treats this as unsafe (the NodeClaim may be
mid-deletion) and defers reboot-slot assignment and drain with a short requeue
rather than proceeding. No Karpenter annotations are applied until a reboot
slot is assigned.

If none of those signals is active, the controller takes the following steps:

1. For each annotation (`do-not-repair`, `do-not-disrupt`): if the annotation
   is **not** already set to `"true"`, the controller sets it (value `"true"`)
   on the Node and, where available, the NodeClaim. It records ownership
   separately on the `BootcNode` object:
   - `bootc.dev/owns-karpenter-do-not-repair: "true"`
   - `bootc.dev/owns-karpenter-do-not-disrupt: "true"`
   - `bootc.dev/owns-karpenter-nodeclaim-do-not-repair: "true"`
   - `bootc.dev/owns-karpenter-nodeclaim-do-not-disrupt: "true"`
2. The controller proceeds through the normal update flow: drain, reboot, and
   image verification.
3. When the slot is freed (node is Ready and running the new image), the
   controller removes from the Node and NodeClaim any annotation it owns that
   is still present. If an external actor already removed an annotation the
   operator added, the operator clears its own ownership record without
   attempting to remove the already-absent annotation. If an external actor
   changed the value of an annotation the operator owns, the operator still
   removes that annotation when the slot is freed.

## NodeClaim annotations are supplemental

NodeClaim annotations (`do-not-repair`, `do-not-disrupt`) are a supplemental
signal, not a distributed lock. By the time bootc-operator writes them,
Karpenter may already have selected or queued a disruption for the registered
Node. The authoritative disruption checks are the Node deletion timestamp,
Node-level taints, NodeClaim deletion timestamp, and NodeClaim status
conditions — the controller checks these before both slot assignment and
drain.

## NodeClaim watch registration

When the controller starts, it checks whether the Karpenter `NodeClaim` API is
available. If it is, the controller registers a watch for NodeClaim events and
uses changes to the associated NodeClaim to requeue its owning
`BootcNodePool`. This allows changes such as Karpenter starting or completing
a reboot to be observed promptly.

The watch is registered only during startup. If Karpenter is installed after
bootc-operator has started, the operator does not add the watch dynamically;
restart the bootc-operator controller after installing Karpenter. As a fallback,
the controller schedules a periodic requeue (every 30 seconds) while
Karpenter-owned nodes are in the staged or slot-assignment portion of the
update, so that missed NodeClaim events do not stall progress indefinitely.

## Pre-existing annotations

If a Karpenter annotation is already present on the Node with a value of
`"true"` before protection is applied — set by an external actor, another
controller, or a manual operation — bootc-operator leaves it alone:

- It does **not** set the corresponding tracking annotation on the `BootcNode`.
- It does **not** remove the annotation when the slot is freed.

If the annotation is present but set to any value other than `"true"` (for
example `"false"`), bootc-operator overwrites it with `"true"` and records
ownership. This is required because Karpenter only honors the literal value
`"true"`; any other value leaves the node unprotected.

If an annotation owned by bootc-operator is changed by another actor while the
slot is active, bootc-operator removes that annotation when the slot is freed;
the ownership marker on the `BootcNode` remains authoritative for cleanup.

The two annotations are tracked independently. If only one is already set to
`"true"`, the operator owns (and later removes) only the other.

## Temporary overprovisioning during node drains

During a bootc-operator update, the following sequence can cause temporary
overprovisioning:

1. bootc-operator evicts workloads from the node being updated.
2. The evicted workloads become pending.
3. Karpenter provisions new capacity to satisfy the pending pods.
4. The original node reboots into the new image and rejoins the cluster.
5. The newly provisioned capacity may become excess and be consolidated later.

This is expected behavior. The `do-not-disrupt` annotation suppresses
Karpenter's voluntary disruption of the node being updated, but it does not
suppress Karpenter provisioning new capacity for pending workloads. Karpenter
provisioning is driven by pending pod demand rather than disruption eligibility.

## Post-rollout consolidation and administrator policy

bootc-operator's protection is intentionally limited to the update lifecycle.
When a node is Ready and the new image has been verified, the operator frees its
reboot slot and removes only the Karpenter annotations that it added. If the
workload has moved to newly provisioned capacity and the original node is now
empty, Karpenter may then consolidate that node with an `Empty` disruption. This
is normal Karpenter behavior after the bootc update has completed; it is not an
OS update failure.

Cluster administrators who need a node to remain in service after the rollout
must configure that steady-state policy in Karpenter. bootc-operator should not
permanently disable consolidation by default, because doing so can leave unused
capacity running indefinitely.

This is an intentional separation of responsibilities. bootc-operator protects
only the Karpenter-owned nodes that are participating in its update lifecycle;
it does not apply protection to every node in the `BootcNodePool`.

| Responsibility | bootc-operator | Cluster administrator / Karpenter |
|---|---|---|
| Protect a node during slot assignment, drain, reboot, and health verification | Applies temporary node and NodeClaim annotations and checks for conflicting Karpenter operations | — |
| Provision capacity for pending workloads | — | Karpenter provisions nodes according to its NodePool and workload requirements |
| Consolidate or drift unrelated nodes | — | Karpenter manages nodes according to the configured disruption policy |
| Retain a node after the rollout becomes complete | Removes only annotations it owns; does not freeze unrelated nodes | Configure `do-not-disrupt`, disruption budgets, or `consolidateAfter` as needed |
| Protect an entire Karpenter pool during maintenance | — | Configure an explicit Karpenter-wide maintenance policy |
| Handle expiration, interruption, manual deletion, or other forceful termination | Detects some conflicts and defers its own update when possible | Configure and operate the applicable Karpenter and infrastructure policies |

If an entire pool must be held stable, configure that as an explicit Karpenter
maintenance policy rather than relying on bootc-operator to freeze unrelated
nodes.

Options include:

1. **Keep a node protected explicitly.** Set
   `karpenter.sh/do-not-disrupt: "true"` on the Node and its NodeClaim. An
   annotation that was already present before bootc-operator starts protecting
   the node is treated as externally owned and is not removed when the rollout
   completes. This blocks voluntary disruption such as consolidation and drift,
   but not expiration, interruption, manual deletion, or other forceful
   termination.
2. **Temporarily block disruption for the whole Karpenter NodePool.** During a
   rollout, an administrator can set a zero disruption budget:

   ```yaml
   spec:
     disruption:
       budgets:
         - nodes: "0"
   ```

   Restore the normal budget after the rollout. This protects all nodes in the
   pool, not just the node being updated.
3. **Delay consolidation.** Configure a suitable `consolidateAfter` interval so
   that a rebooted node has time to become schedulable and receive workloads
   before Karpenter considers it empty. This reduces the race but does not
   guarantee that the node will be retained.

All three options are administrator-owned policy. The first is appropriate for
selected nodes that must remain stable; the second is appropriate when an
entire pool must be held stable during a controlled maintenance window; and the
third is appropriate when consolidation should remain enabled but be delayed.

## OS initialization startup taint

The lifecycle-manager-neutral behavior and taint lifecycle are documented in
[OS initialization taint](os-not-ready-taint.md). With Karpenter, configure
the taint as a `startupTaint` on the `NodePool`.

Add `bootc.dev/os-not-ready` to the `startupTaints` list in the Karpenter
`NodePool`:

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  template:
    spec:
      startupTaints:
        - key: bootc.dev/os-not-ready
          effect: NoSchedule
```

No changes to the `BootcNodePool` spec are required. bootc-operator activates
the feature automatically when it detects the taint on a newly joined node.

### `startupTaints` vs `taints`

This taint **must** be declared under `startupTaints`, not `taints`, in the
Karpenter `NodePool`.

Karpenter uses `startupTaints` to mark nodes that require external
initialization before workloads are scheduled. It factors the taint into its
bin-packing decisions and does not treat nodes with the taint as unable to
satisfy pending pods.

If the taint is instead declared under `taints` (permanent taints), Karpenter
will observe that pending pods cannot be scheduled on nodes carrying the taint
and will provision additional nodes in a loop — each of which will also carry
the taint, perpetuating the loop.

## Limitations

`karpenter.sh/do-not-disrupt` only blocks voluntary disruption methods
(consolidation and drift). It does not protect a node against forceful
termination triggered by expiration (`expireAfter`), interruption (spot
instance reclaim), Node Auto Repair, or manual deletion. If any of those
events occur while a node is in a bootc-operator reboot slot, Karpenter may
still terminate it.

`karpenter.sh/do-not-repair` similarly has no effect on voluntary disruption
or expiration — it only suppresses Node Auto Repair.

If a node is terminated by expiration, interruption, or any other forceful
method while a bootc-operator update is in progress, the termination is caused
by Karpenter acting on its own policy — not by the bootc-operator update. The
update simply happened to be in flight at the time.

## Residual race and coordination guarantees

The annotations and disruption checks are not an authoritative mutex. Karpenter
does not currently acknowledge `do-not-repair`, expose a shared Lease, or
coordinate its reboot commit with bootc-operator's slot assignment. Karpenter
can therefore commit a disruption immediately before or after bootc-operator's
last check. Setting `do-not-repair` does not cancel a disruption that Karpenter
has already committed.

The integration narrows this window by:

- Checking all disruption signals (reboot taint, disrupted taint, deletion
  timestamp, and NodeClaim conditions) before both slot assignment and drain.
- Applying protection when the reboot slot is assigned, before draining.

Full serialization requires Karpenter support for a shared ownership protocol
or an admission/provider mechanism that prevents a competing operation from
being committed.

Refer to the [Karpenter disruption documentation](https://karpenter.sh/docs/concepts/disruption/)
for a full breakdown of which controls apply to which disruption methods.

## Non-Karpenter nodes

Nodes without a `NodeClaim` owner reference are unaffected. The feature
activates only when the owner reference check matches.

## Annotation reference

| Annotation | Object | Managed by | Purpose |
|---|---|---|---|
| `karpenter.sh/do-not-repair` | K8s Node | bootc-operator (or external) | Prevents Karpenter Node Auto Repair from replacing this node during an update |
| `karpenter.sh/do-not-disrupt` | K8s Node | bootc-operator (or external) | Prevents Karpenter voluntary disruption (consolidation, drift) during an update |
| `karpenter.sh/do-not-repair` | NodeClaim | bootc-operator (or external) | Best-effort suppression of Karpenter repair selection before an operator reboot |
| `karpenter.sh/do-not-disrupt` | NodeClaim | bootc-operator (or external) | Best-effort suppression of Karpenter voluntary disruption before an operator reboot |
| `bootc.dev/owns-karpenter-do-not-repair` | BootcNode | bootc-operator | Tracks that the operator added `do-not-repair`; cleared when the annotation is removed on slot free |
| `bootc.dev/owns-karpenter-do-not-disrupt` | BootcNode | bootc-operator | Tracks that the operator added `do-not-disrupt`; cleared when the annotation is removed on slot free |
| `bootc.dev/owns-karpenter-nodeclaim-do-not-repair` | BootcNode | bootc-operator | Tracks ownership of NodeClaim `do-not-repair` |
| `bootc.dev/owns-karpenter-nodeclaim-do-not-disrupt` | BootcNode | bootc-operator | Tracks ownership of NodeClaim `do-not-disrupt` |
