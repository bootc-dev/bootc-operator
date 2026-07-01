# Contributing to the Bootc Operator

The Bootc Operator is a Kubernetes operator for managing
[bootc](https://github.com/bootc-dev/bootc) nodes. We welcome contributions of
all kinds: bug reports, feature requests, documentation improvements, and code
changes.

## Table of Contents

- [Prerequisites](#prerequisites)
- [Development Environment](#development-environment)
- [Building](#building)
- [Testing](#testing)
- [Investigating CI Failures](#investigating-ci-failures)
- [Code Style](#code-style)
- [Submitting Changes](#submitting-changes)
- [AI Generated Code](#ai-generated-code)
- [Community](#community)
- [License](#license)
- [Other Useful Documentation](#other-useful-documentation)

## Prerequisites

Before contributing, make sure you have the following installed:

- **Go** (see `go.mod` for the required version)
- **Make**
- **Podman**
- **kubectl**

For end-to-end testing you will also need:

- [bink](https://github.com/bootc-dev/bink) — lightweight Kubernetes clusters
  backed by bootc nodes
- [skopeo](https://github.com/containers/skopeo) — container image inspection

Tool dependencies like `controller-gen`, `kustomize`, `golangci-lint`, and
`setup-envtest` are downloaded automatically to `./bin/` by the Makefile when
needed.

## Development Environment

Most contributors work on a Linux host with Go and Podman available. Make sure
the Podman socket is running — bink and the build system communicate with
Podman through it:

```shell
systemctl --user start podman.socket
```

Before diving into the code, familiarize yourself with the project from a user
perspective by reading the [README](README.md) and the
[Architecture](docs/ARCHITECTURE.md) document. We also recommend getting
familiar with [bink](https://github.com/bootc-dev/bink), which is used to
create lightweight Kubernetes clusters backed by bootc nodes for development
and testing.

You can create a bink cluster by running:
```shell
make deploy-bink
```

To tear down the cluster when done:

```shell
make teardown-bink
```

By default, `deploy-bink` and `e2e` share the same cluster named `e2e`. To use
a separate development cluster:

```shell
make deploy-bink BINK_CLUSTER_NAME=dev
```

## Building

```shell
make build           # Build all binaries (manager + daemon) to ./bin/
make buildimg        # Build the container image (default: bootc-operator:dev)
```

The `bootc-operator` container image contains both the controller and daemon
binaries.

After modifying API types in `api/`, regenerate CRDs and code:

```shell
make manifests       # Regenerate CRD and RBAC manifests
make generate        # Regenerate DeepCopy implementations
```

## Testing

### Running Unit Tests

Unit tests live in `internal/controller/` and `internal/daemon/` alongside the
code they test. They use the controller-runtime
[envtest](https://book.kubebuilder.io/reference/envtest) framework, which
provides a local control plane (etcd + API server) without a full cluster. The
required binaries are downloaded automatically.

```shell
make unit            # Run all unit tests
make unit V=1        # Verbose output
make unit RUN=Foo    # Run tests matching "Foo"
```

### Running End-to-End Tests

E2E tests live in `test/e2e/` and run against a real
[bink](https://github.com/bootc-dev/bink) cluster. The full workflow is:

Each e2e test creates a dedicated worker node for the duration of the test and
tears it down when the test completes. The freshly provisioned node ensures
that each test starts from a clean state.

```shell
make buildimg        # Build the operator container image
make deploy-bink     # Start a bink cluster and deploy the operator
make e2e             # Run the e2e test suite
make e2e V=1         # Verbose streaming output
make e2e RUN=Foo     # Run tests matching "Foo"
```

> [!NOTE]
> The container image must be rebuilt and pushed to the bink registry after
> every code change. Run `make buildimg` followed by `make deploy-bink` to
> pick up your latest changes in the cluster.

### Internal Registry

The bink cluster includes an internal container registry. From the host, push
images to `localhost:5000`:

```shell
podman push --tls-verify=false localhost:5000/my-image:latest
```

From inside the cluster, the same image is available at
`registry.cluster.local:5000`:

```yaml
image: registry.cluster.local:5000/my-image:latest
```

### Finding Test Logs

Each e2e test automatically gathers diagnostic logs when it completes
(regardless of pass or fail). Logs are written to `_output/logs/<test-name>/`
and include:

- Operator pod logs
- Pod and deployment descriptions
- BootcNodePool and BootcNode descriptions
- Kubernetes events
- Node descriptions and journal logs for each worker node

You can also manually gather logs from a running cluster:

```shell
make gather-bink
```

This collects the same diagnostics into `_output/logs/gather-bink/`.

## Investigating CI Failures

CI runs on [GitHub Actions](https://github.com/bootc-dev/bootc-operator/actions)
and consists of three jobs: `unit`, `build-bink`, and `e2e`. When a run fails,
you can inspect it using the `gh` CLI.

View a run summary:

```shell
gh run view <run-id> --repo bootc-dev/bootc-operator
```

### Downloading E2E Logs

The e2e job uploads diagnostic logs as a GitHub Actions artifact named
`e2e-logs`. These contain the same logs collected by `make gather-bink`
(operator pod logs, node journals, event dumps, etc.). Download them with:

```shell
gh run download <run-id> --repo bootc-dev/bootc-operator
```

This creates a local `e2e-logs/` directory with per-test subdirectories
matching the structure described in [Finding Test Logs](#finding-test-logs).

## Code Style

### Go

- Run `make fmt` and `make vet` before submitting.
- Run `make lint` to check with
  [golangci-lint](https://golangci-lint.run/). Use `make lint-fix` to apply
  automatic fixes.
- All Go files must include the SPDX license header:

  ```go
  // SPDX-License-Identifier: Apache-2.0
  ```

## Submitting Changes

### Before Writing a Big Patch

If you are planning a large change, please **open an issue first** to discuss
the approach. This avoids wasted effort and helps maintainers give early
feedback.

### Developer Certificate of Origin

This project uses the [Developer Certificate of Origin](https://developercertificate.org/)
(DCO). You must sign off each commit to certify that you have the right to
submit it under the project's open source license.

Add a sign-off line to your commits:

```
Signed-off-by: Your Name <your.email@example.com>
```

Use `git commit -s` to add this automatically. Your sign-off name must match
your real name.

### Commit Style

Follow a commit style similar to the Linux kernel:

1. **Subject line**: a short contextual prefix, imperative mood, under 72
   characters, no trailing period.
2. **Body**: separated by a blank line, wrapped at 72 characters. Explain
   *what* changed and *why*.
3. Use `Closes: #<number>` or `Fixes: #<number>` to link to issues.

### Pull Request Process

1. Fork the repository and create a topic branch from `main`.
2. Make your changes in small, focused commits.
3. Ensure all checks pass locally:

   ```shell
   make fmt manifests generate
   make vet
   make lint
   make unit
   ```

4. Push your branch and open a pull request against `main`.
5. Describe the change clearly in the PR description — what it does and why.
6. Address review feedback. Maintainers may request changes before merging.

### Code Review

All submissions require review before merging. Reviewers look for:

- Correctness and test coverage
- Consistency with existing patterns
- Clear commit messages
- Generated files kept in sync

## AI generated code

For AI generated code, please refer the
[AGENTS.md](https://github.com/bootc-dev/infra/blob/main/common/AGENTS.md)
document.

## Community

- **Issues**: Use [GitHub Issues](https://github.com/bootc-dev/bootc-operator/issues)
  to report bugs or request features.

## License

By contributing, you agree that your contributions will be licensed under the
[Apache License 2.0](LICENSE).

## Other useful documentation

Please, refer to these guides for further help:
* [General bootc review guide](https://github.com/bootc-dev/infra/blob/main/common/REVIEW.md)
* [Kubernetes contributing guide](https://github.com/kubernetes/community/tree/main/contributors/guide)
* [Golang best-practise](https://go.dev/doc/effective_go)
* [Kubernetes API conventions](https://github.com/kubernetes/community/blob/main/contributors/devel/sig-architecture/api-conventions.md)
