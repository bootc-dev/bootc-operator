---
name: deploy-cluster
description: Deploy the bootc-operator to a local bink cluster for development and testing
---

## What I do

I guide you through building and deploying the bootc-operator to a local bink cluster from scratch. I check all prerequisites, build the operator image, create the cluster, deploy the operator, and verify everything is healthy.

## When to use me

- Setting up a local development cluster for the first time
- Redeploying after code changes
- Troubleshooting a broken cluster by tearing down and redeploying
- Preparing to run e2e tests

---

## Instructions for AI Agent

All commands run from the project root: `/home/ptalgulk/VSCode/bootc/bootc-operator`

### Step 1: Check prerequisites

Run each check. If any tool is missing, tell the user how to install it before continuing.

```bash
echo "=== Checking prerequisites ==="

echo -n "Go: "; go version 2>/dev/null || echo "MISSING - install from https://go.dev/dl/ (need >= 1.26)"
echo -n "Make: "; make --version 2>/dev/null | head -1 || echo "MISSING - dnf install make"
echo -n "Podman: "; podman --version 2>/dev/null || echo "MISSING - dnf install podman"
echo -n "kubectl: "; kubectl version --client 2>/dev/null | head -1 || echo "MISSING - https://kubernetes.io/docs/tasks/tools/"
echo -n "bink: "; bink version 2>/dev/null || echo "MISSING - go install github.com/bootc-dev/bink@latest"
echo -n "skopeo: "; skopeo --version 2>/dev/null || echo "MISSING - dnf install skopeo"
```

Check the Go version meets the `go.mod` requirement (>= 1.26):

```bash
go_ver=$(go version | grep -oP 'go\K[0-9]+\.[0-9]+')
echo "Go version: $go_ver (need >= 1.26)"
```

Check Podman socket is running:

```bash
systemctl --user is-active podman.socket
```

If inactive, start it:

```bash
systemctl --user start podman.socket
```

Check port 5000 is available (used by the local registry):

```bash
ss -tlnp | grep :5000 || echo "Port 5000 is available"
```

If port 5000 is already in use by something other than a bink registry, tell the user to free it.

**Stop here if any prerequisite is missing. Do not proceed until all tools are installed.**

### Step 2: Build the operator image

```bash
make buildimg
```

**What this does:** Builds the `bootc-operator:dev` container image using Podman. The image contains both the controller and daemon binaries.

**Expected output:** The last line should say `Successfully tagged localhost/bootc-operator:dev`.

If the build fails, check:
- Is `podman.socket` running? (`systemctl --user start podman.socket`)
- Are there Go compilation errors? (fix the code first)

### Step 3: Deploy to bink cluster

```bash
make deploy-bink
```

**What this does (in order):**
1. Starts a local container registry at `localhost:5000`
2. Pulls the bootc node disk image and pushes it to the registry
3. Creates a bink cluster named `e2e` with a control-plane node (skips if already running)
4. Builds a derived node image for update testing
5. Pushes the operator image to the in-cluster registry
6. Deploys the operator via kustomize with `--allow-insecure-registry` and short tag resolution
7. Waits for the controller deployment to roll out

**Expected output:** The last line should say `deployment "bootc-operator-controller-manager" successfully rolled out`.

This step takes 2-5 minutes on first run (pulling images). Subsequent runs are faster.

To use a separate dev cluster (so e2e tests don't interfere):

```bash
make deploy-bink BINK_CLUSTER_NAME=dev
```

### Step 4: Set up kubeconfig

Tell the user to run this in their terminal:

```bash
export KUBECONFIG=$(pwd)/kubeconfig-e2e
```

For a named cluster (e.g. `dev`):

```bash
export KUBECONFIG=$(pwd)/kubeconfig-dev
```

### Step 5: Verify deployment health

Run these checks and report results to the user:

```bash
echo "=== Cluster Health Check ==="

echo "--- Nodes ---"
kubectl get nodes

echo "--- CRDs ---"
kubectl get crd | grep bootc

echo "--- Operator Pods ---"
kubectl -n bootc-operator get pods

echo "--- Controller Deployment ---"
kubectl -n bootc-operator get deployment bootc-operator-controller-manager

echo "--- Daemon DaemonSet ---"
kubectl -n bootc-operator get daemonset bootc-operator-daemon
```

**Healthy state checklist:**
- [ ] Controller node is `Ready`
- [ ] CRDs `bootcnodes.node.bootc.dev` and `bootcnodepools.node.bootc.dev` exist
- [ ] Controller manager pod is `Running` (1/1)
- [ ] Daemon DaemonSet has desired count matching available nodes

If any check fails:

**Controller pod not running:**
```bash
kubectl -n bootc-operator describe deployment bootc-operator-controller-manager
kubectl -n bootc-operator logs deployment/bootc-operator-controller-manager
```

**Daemon pod not running:**
```bash
kubectl -n bootc-operator describe daemonset bootc-operator-daemon
kubectl -n bootc-operator get events -n bootc-operator --sort-by='.lastTimestamp' | tail -20
```

**CRDs missing:**
```bash
make install KUBECONFIG=./kubeconfig-e2e
```

**Node not Ready:**
```bash
kubectl describe node controller
```

### Step 6 (optional): Smoke test

If the user wants to verify end-to-end functionality:

```bash
make e2e V=1 RUN=TestControllerMembership
```

This runs a single fast test that provisions a worker node, creates a pool, and verifies a BootcNode is created. Takes about 40 seconds.

To run the full e2e suite:

```bash
make e2e V=1
```

To run a specific test:

```bash
make e2e V=1 RUN=TestUpdateReboot
```

### Redeploying after code changes

```bash
make buildimg
make deploy-bink
```

If API types in `api/` were changed, regenerate manifests first:

```bash
make manifests generate
make buildimg
make deploy-bink
```

### Gathering diagnostic logs

```bash
make gather-bink
```

Logs are written to `_output/logs/gather-bink/`.

### Troubleshooting

| Symptom | Fix |
|---------|-----|
| `podman.socket` not running | `systemctl --user start podman.socket` |
| Port 5000 in use | Stop the process using it, or tear down and recreate |
| Cluster unhealthy | `make teardown-bink && make buildimg && make deploy-bink` |
| Image not updating | Verify `make buildimg` succeeded, check `skopeo inspect --tls-verify=false docker://localhost:5000/bootc-operator-e2e:latest` |
| CRDs not installed | `make install KUBECONFIG=./kubeconfig-e2e` |
| `bink` not found | `go install github.com/bootc-dev/bink@latest` |
| `skopeo` not found | `dnf install skopeo` |
| Go version too old | Install Go >= 1.26 from https://go.dev/dl/ |

### Key variables

| Variable | Default | Description |
|----------|---------|-------------|
| `IMG` | `bootc-operator:dev` | Operator container image name |
| `CONTAINER_TOOL` | `podman` | Container runtime |
| `BINK_CLUSTER_NAME` | `e2e` | Cluster name (change to avoid conflicts) |
| `KUBECONFIG_BINK` | `./kubeconfig-e2e` | Kubeconfig path (derived from cluster name) |
