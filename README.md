# Bootc Operator

A Kubernetes operator for managing [bootc] nodes.

> [!WARNING]
> This project is in early development and not yet fully functional. APIs may
> change.

## Table of Contents

- [Motivation](#motivation)
- [Highlights](#highlights)
- [Installation](#installation)
- [How It Works](#how-it-works)
- [Usage](#usage)
  - [Creating a pool](#creating-a-pool)
  - [Monitoring a rollout](#monitoring-a-rollout)
  - [Pausing and resuming](#pausing-and-resuming)
  - [Rolling back](#rolling-back)
- [Related Projects](#related-projects)

## Motivation

[bootc] lets you build and deploy image-based Linux systems using container
tools. It a strong candidate for managing Kubernetes cluster nodes. At the same
time, it also provides a common distro-agnostic API through which to manage
these nodes.

The Bootc Operator then exposes this bootc API to the Kubernetes control plane
in pure operator declarative fashion: you define the desired state your hosts
should be in, and the operator handles the reconciliation. This includes image
updates, but also future bootc enhancements like dynamic config overlays,
sysexts, etc. See the [PRD](docs/PRD.md) for more details and the
[Roadmap](ROADMAP.md) for what's ahead.

## Highlights

- **Distro-agnostic** — Works with any bootc-based OS image, not tied to any
  Kubernetes distribution.
- **Declarative** — Specify your node requirements in a declarative way through
  CRDs.
- **bootc-native** — Tight integration with current and future bootc features
  like soft-reboot, non-disruptive staging, dynamic config overlays, live
  apply, etc.

## Installation

Deploy from the kustomize config:

```shell
kubectl apply -k https://github.com/bootc-dev/bootc-operator//config/default
```

This creates the `bootc-operator` namespace and deploys the controller and
daemon. The operator does nothing until you create a BootcNodePool.

> [!NOTE]
> The operator namespace requires a [Pod Security Admission] exemption for the
> `privileged` level. The daemon DaemonSet runs privileged to execute bootc
> commands on the host.

## How It Works

The operator consists of two components:

- A **controller** (Deployment) that watches BootcNodePool resources, resolves
  image digests, manages pool membership, and drives the rollout state machine
  (cordon, drain, reboot slot management).
- A **daemon** (DaemonSet) that runs on each managed node. It watches its own
  BootcNode resource, executes bootc commands on the host, and reports status
  back.

The controller and daemon communicate exclusively through BootcNode resources:
the controller writes `spec` (what the node should do), the daemon writes
`status` (what the node is doing). There is no direct communication between
them.

```
                    BootcNodePool (user-created)
                           │
                           ▼
                  ┌─────────────────┐
                  │   Controller    │
                  │  (Deployment)   │
                  └────────┬────────┘
                           │ creates/updates
                           ▼
                  BootcNode.spec ──────► BootcNode.status
                  (desired state)        (observed state)
                           │                    ▲
                           ▼                    │
                  ┌─────────────────┐           │
                  │     Daemon      ├───────────┘
                  │  (DaemonSet)    │  reports
                  │   per node      │
                  └─────────────────┘
```

## Usage

### Creating a pool

A BootcNodePool defines a group of nodes and the OS image they should run.
Nodes are selected by label:

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
    ref: quay.io/example/myos@sha256:abc123...
```

The operator creates a BootcNode for each matching node, labels it with
`bootc.dev/managed`, and begins staging the image. To update the OS, change
`spec.image.ref` to a new digest. Tags will also be supported.

### Monitoring a rollout

Pool status shows rollout progress:

```shell
kubectl get bootcnodepool workers -o yaml
```

```yaml
status:
  targetDigest: sha256:abc123...
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

Individual node status is available via BootcNode resources:

```shell
kubectl get bootcnodes
```

### Pausing and resuming

To pause a rollout (in-progress staging completes, but no new reboots start):

```shell
kubectl patch bootcnodepool workers --type merge -p '{"spec":{"rollout":{"paused":true}}}'
```

To resume:

```shell
kubectl patch bootcnodepool workers --type merge -p '{"spec":{"rollout":{"paused":false}}}'
```

### Rolling back

To roll back, change `spec.image.ref` to the previous digest. Nodes already
running that image are left alone. Nodes that were updated go through the
normal staging and reboot cycle to return to the previous image.

## Related Projects

### Machine Config Operator (MCO)

The [MCO] manages bootc hosts (such as RHEL CoreOS) on [OpenShift]. It is
powerful but tied to OpenShift. A primary goal of the Bootc Operator is to
support being leveraged by existing distributions where it may be driven by a
higher-level operator such as the MCO.

### Flightctl

[Flightctl] is a fleet management service for edge devices, primarily targeting
bootc hosts. It shares similar functionality around rollouts but operates
outside the Kubernetes operator pattern.

### Kured

[Kured] handles reboot coordination for Kubernetes nodes and works well with
bootc. The Bootc Operator covers similar ground but tightens the scope to bootc
hosts, taking advantage of image-based features like update staging and OS
rollbacks as part of the rollout strategy, and overall exposing bootc APIs
directly.

[bootc]: https://github.com/bootc-dev/bootc
[Flightctl]: https://flightctl.io/
[Kured]: https://kured.dev/
[MCO]: https://github.com/openshift/machine-config-operator
[OpenShift]: https://www.redhat.com/en/technologies/cloud-computing/openshift
[Pod Security Admission]: https://kubernetes.io/docs/concepts/security/pod-security-admission/
