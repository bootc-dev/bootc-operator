# Karpenter integration

bootc-operator is aware of [Karpenter](https://karpenter.sh/) and coordinates
with it during node updates to prevent unnecessary node replacement or
voluntary disruption.

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

> **Note:** `karpenter.sh/do-not-repair` was introduced in
> [kubernetes-sigs/karpenter#3311](https://github.com/kubernetes-sigs/karpenter/pull/3311)
> and has not yet appeared in a Karpenter release. This doc will be updated
> with a minimum required version once one is available.

bootc-operator sets both annotations automatically on Karpenter-managed nodes
for the duration of the reboot slot.

## How it works

When the controller assigns a reboot slot to a node it checks whether the K8s
Node object has an owner reference pointing to a Karpenter `NodeClaim`
(`apiVersion: karpenter.sh/v1`, `kind: NodeClaim`). If it does, the
controller takes the following steps:

1. For each annotation (`do-not-repair`, `do-not-disrupt`): if the annotation
   is **not** already present on the Node, the controller adds it (value
   `"true"`) and records ownership by setting the corresponding tracking
   annotation on the `BootcNode` object:
   - `bootc.dev/owns-karpenter-do-not-repair: "true"`
   - `bootc.dev/owns-karpenter-do-not-disrupt: "true"`
2. The node proceeds through the normal update flow: drain, reboot, image
   verification.
3. When the slot is freed (node is Ready and running the new image), the
   controller checks the tracking annotations on the `BootcNode`. For each
   annotation it owns, it removes the corresponding annotation from the Node
   and clears the tracking annotation from the `BootcNode`. Both removals are
   issued as a single Node patch.

## Pre-existing annotations

If either Karpenter annotation is already on the Node before slot assignment —
set by an external actor, another controller, or a manual operation —
bootc-operator leaves it completely alone:

- It does **not** overwrite or change the annotation on assignment.
- It does **not** set the corresponding tracking annotation on the `BootcNode`.
- It does **not** remove the annotation when the slot is freed.

The two annotations are tracked independently. If only one is pre-existing, the
operator owns (and later removes) only the other.

This ensures bootc-operator never removes an annotation it did not add.

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
| `bootc.dev/owns-karpenter-do-not-repair` | BootcNode | bootc-operator | Tracks that the operator added `do-not-repair`; cleared when the annotation is removed on slot free |
| `bootc.dev/owns-karpenter-do-not-disrupt` | BootcNode | bootc-operator | Tracks that the operator added `do-not-disrupt`; cleared when the annotation is removed on slot free |
