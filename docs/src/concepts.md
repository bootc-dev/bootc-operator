# Concepts

## BootcNodePool

A `BootcNodePool` is the only resource users need to create. It defines a
group of nodes by label selector and the OS image those nodes should be running.
The operator creates one `BootcNode` per matching node and drives each toward
the desired image.

## BootcNode

A `BootcNode` represents a single managed node. It is created automatically by
the controller (one per node that matches a pool's selector) and named after
the Kubernetes `Node` it represents. Users do not create or modify `BootcNode`
objects directly.

The status of a `BootcNode` reflects the information reported by `bootc status`
on that node: the currently booted image, any staged image, and the rollback
entry if one is available.

## Image references

A pool's `spec.image.ref` can be either a digest reference
(`quay.io/example/myos@sha256:abc123`) or a tag reference
(`quay.io/example/myos:latest`). With a digest ref, the target is pinned and
immutable. With a tag ref, the controller periodically resolves the tag to a
digest and begins a new rollout whenever the digest changes.
