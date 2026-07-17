# Bootc Operator Roadmap

## Vision

The Bootc Operator aims to be the standard Kubernetes operator for managing
bootc-based node OS images. It acts as a bridge between bootc and Kubernetes,
surfacing all relevant host features to the control plane in a distro-agnostic
way.

## Roadmap

The immediate goal is to ship [v0.1](https://github.com/bootc-dev/bootc-operator/issues?q=is%3Aissue%20state%3Aopen%20milestone%3Av0.1), which we consider the MVP state.

Afterwards, here are various categories in which we'd like to make progress:
* Usability gaps: [expose Prometheus metrics](https://github.com/bootc-dev/bootc-operator/issues/102), [emit K8s Events](https://github.com/bootc-dev/bootc-operator/issues/101), [support phased staging](https://github.com/bootc-dev/bootc-operator/issues/114), [manage operator manifests](https://github.com/bootc-dev/bootc-operator/issues/14), [publish AI skills](https://github.com/bootc-dev/bootc-operator/issues/27)
* Quality of life improvements: [integrate soft-reboot](https://github.com/bootc-dev/bootc-operator/issues/117), [support maintenance windows](https://github.com/bootc-dev/bootc-operator/issues/105), [limit staging I/O](https://github.com/bootc-dev/bootc-operator/issues/61), [surface download progress](https://github.com/bootc-dev/bootc-operator/issues/116), [simplify operator configuration](https://github.com/bootc-dev/bootc-operator/issues/92), [support cross-pool rollout ordering](https://github.com/bootc-dev/bootc-operator/issues/110)
* Major features: [add automatic rollback](https://github.com/bootc-dev/bootc-operator/issues/104), [enforce signature verification](https://github.com/bootc-dev/bootc-operator/issues/64), [enable transient `/etc` and/or `/var`](https://github.com/bootc-dev/bootc-operator/issues/73), support [config overlays](https://github.com/bootc-dev/bootc/issues/22), integrate [sysexts](https://github.com/bootc-dev/bootc/issues/7), [implement pull-through caching](https://github.com/bootc-dev/bootc-operator/issues/107), [integrate with trusted-cluster-operator](https://github.com/bootc-dev/bootc-operator/issues/17)
