# OS initialization taint

`bootc.dev/os-not-ready:NoSchedule` prevents normal workloads from being
scheduled onto a newly created node until bootc-operator has verified that the
node is running the target OS image and is Ready.

Use this taint with a node lifecycle manager that can apply it before the node
accepts normal workloads. Karpenter is one example; the taint is not specific
to Karpenter.

## Why use it

A lifecycle manager may create a node from an image that predates the target
OS image configured in a `BootcNodePool`. Without the taint, workloads can be
scheduled before bootc-operator stages, reboots, and verifies the target OS.

Apply this exact taint to new nodes:

```yaml
key: bootc.dev/os-not-ready
effect: NoSchedule
```

No changes to the `BootcNodePool` spec are required.

## Lifecycle

When bootc-operator creates a `BootcNode` for a newly joined node that carries
the taint, it records responsibility for removing it with the
`bootc.dev/owns-os-not-ready-taint: "true"` annotation on the `BootcNode`.
It then follows the normal update lifecycle.

The operator removes the taint only after the node is both:

- UpToDate, meaning its booted image matches the pool target digest; and
- Ready.

If the node already runs the target image when it joins the pool, the operator
removes the taint as soon as those two conditions are confirmed. The node does
not need to occupy a reboot slot first.

The operator does not add this taint itself. It records responsibility only
when the `BootcNode` is created. Therefore, a taint added later to an existing
managed node is not removed by bootc-operator.

If a node becomes Degraded before it reaches the target image, the taint stays
in place. If a node leaves the `BootcNodePool` before verification completes,
the operator also leaves the taint in place; an administrator must decide how
to recover or retire that node.

## Lifecycle-manager requirements

The lifecycle manager must apply the taint before normal workload scheduling
can begin, and it must not remove the taint itself before bootc-operator has
verified the target OS. Ensure that any system workloads required for node
initialization tolerate the taint as appropriate.

For Karpenter-specific configuration and its `startupTaints` requirement, see
[Karpenter integration](karpenter.md#os-initialization-startup-taint).

## Annotation reference

| Annotation | Object | Managed by | Purpose |
|---|---|---|---|
| `bootc.dev/owns-os-not-ready-taint` | BootcNode | bootc-operator | Records responsibility for removing the observed `bootc.dev/os-not-ready:NoSchedule` taint; cleared when the taint is removed |
