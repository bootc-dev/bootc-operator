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
for the duration of the full update lifecycle — from image staging through
node reboot and health verification. Where the associated `NodeClaim` exists,
it also sets the annotations there. This is best-effort coordination, not a
distributed lock.

## How it works

When a Karpenter-owned node enters the `Staging` state (the daemon is pulling
the new image), bootc-operator immediately sets `do-not-repair` and
`do-not-disrupt` on the Node and, where available, the associated NodeClaim.
This protection persists through slot assignment, drain, reboot, and post-boot
health verification.

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
never removes a Karpenter taint.

If the Karpenter NodeClaim owner reference is present but the NodeClaim object
returns a 404, bootc-operator treats this as unsafe (the NodeClaim may be
mid-deletion) and defers with a short requeue rather than proceeding.

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
   controller removes only annotations that it still owns and whose value is
   still `"true"`. It never removes annotations it did not add.

## NodeClaim annotations are supplemental

NodeClaim annotations (`do-not-repair`, `do-not-disrupt`) are a supplemental
signal, not a distributed lock. By the time bootc-operator writes them,
Karpenter may already have selected or queued a disruption for the registered
Node. The authoritative disruption checks are the Node-level taint, the
NodeClaim deletion timestamp, and the NodeClaim status conditions — the
controller checks these before both slot assignment and drain.

## NodeClaim watch registration

When the controller starts, it checks whether the Karpenter `NodeClaim` API is
available. If it is, the controller registers a watch for NodeClaim events and
uses changes to the associated NodeClaim to requeue its owning
`BootcNodePool`. This allows changes such as Karpenter starting or completing
a reboot to be observed promptly.

The watch is registered only during startup. If Karpenter is installed after
bootc-operator has started, the operator does not add the watch dynamically;
restart the bootc-operator controller after installing Karpenter. As a fallback,
the controller schedules a periodic requeue (every 30 seconds) whenever
Karpenter-owned nodes are active, so that missed NodeClaim events do not
stall progress indefinitely.

The controller represents NodeClaims with Kubernetes `Unstructured` objects
instead of importing Karpenter's Go API types. This keeps Karpenter an optional
integration and avoids adding a Karpenter API dependency or scheme registration
to bootc-operator. It also lets clusters without the NodeClaim CRD continue to
run normally. The tradeoff is that the small set of fields used here, such as
`status.nodeName` and `status.conditions`, is checked at runtime rather than
being enforced by Go types.

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

The integration narrows this window substantially by:

- Applying protection from the start of image staging rather than only at
  slot assignment.
- Checking all disruption signals (reboot taint, disrupted taint, deletion
  timestamp, and NodeClaim conditions) before both slot assignment and drain.

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
