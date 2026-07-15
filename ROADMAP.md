# Bootc Operator Roadmap

## Vision

The Bootc Operator aims to be the standard Kubernetes operator for managing
bootc-based node OS images. It bridges bootc and the Kubernetes control plane,
enabling cluster admins to declaratively manage host OS updates, rollouts, and
surfaces future bootc capabilities across any Kubernetes distribution. It claims
to be distro-agnostic and bootc-native.

## Roadmap Phases

### Phase 1: Core Operator with minimal MVP

* Complete milestone `v0.1`
  [issues](https://github.com/bootc-dev/bootc-operator/issues?q=is%3Aissue%20state%3Aopen%20milestone%3Av0.1)
* Automation of the release creation: https://github.com/bootc-dev/bootc-operator/issues/100

### Phase 2: Improve Operator API and kubernetes integration
* Define and test the operator upgrade path: https://github.com/bootc-dev/bootc-operator/issues/14
* Support configuring rollout halt condition: https://github.com/bootc-dev/bootc-operator/issues/99
* Support a configmap-backed config dir for configuring the bootc-operator: 
  https://github.com/bootc-dev/bootc-operator/issues/92
* Definition of kubernetes events: https://github.com/bootc-dev/bootc-operator/issues/101

### Phase 3: Add new Operator functionalities
* Support enforcing signature verification: https://github.com/bootc-dev/bootc-operator/issues/64
* Support I/O scheduling limits for staging operations https://github.com/bootc-dev/bootc-operator/issues/61
* Health checks and automatic rollback: https://github.com/bootc-dev/bootc-operator/issues/105
* Maintenance windows: https://github.com/bootc-dev/bootc-operator/issues/105
* Pre-staging while paused: https://github.com/bootc-dev/bootc-operator/issues/106
* Pull-through caching: https://github.com/bootc-dev/bootc-operator/issues/107
* Stuck node detection: https://github.com/bootc-dev/bootc-operator/issues/108
* Custom drain implementation: https://github.com/bootc-dev/bootc-operator/issues/109
* Cross-pool rollout ordering: https://github.com/bootc-dev/bootc-operator/issues/110

### Phase 4: Improve the security of the operator
* Privilege separation for the daemon: https://github.com/bootc-dev/bootc-operator/issues/103
* Restrict daemon so it can only update its own BootcNode's status subresource: 
  https://github.com/bootc-dev/bootc-operator/issues/6

### Phase 5: Integrate new bootc features
* Enable transient {root,etc,var} options: https://github.com/bootc-dev/bootc-operator/issues/73

### Phase 6: Integration with other projects
* Support of trusted execution clusters: https://github.com/bootc-dev/bootc-operator/issues/17
* Definition of Prometheus metrics: https://github.com/bootc-dev/bootc-operator/issues/102
