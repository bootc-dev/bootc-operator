# Bootc Operator

A Kubernetes operator for managing [bootc] nodes.

[bootc] lets you define a complete Linux operating system as an OCI/Docker
container image and deploy it transactionally on physical or virtual machines.
It is a natural fit for managing Kubernetes cluster nodes: a node OS becomes
a container image, updated and rolled back with the same registry tooling
already in use for workloads.

The Bootc Operator exposes the bootc API to the Kubernetes control plane in
declarative fashion. You declare the desired OS image for a group of nodes in a
`BootcNodePool` resource; the operator resolves image tags, coordinates staged
rollouts, drains nodes, and orchestrates reboots, all without any per-node
configuration. Node OS upgrades become a standard Kubernetes operation, managed
with the same `kubectl` workflows used for any other resource and requiring no
additional tooling.

[bootc]: https://github.com/bootc-dev/bootc
