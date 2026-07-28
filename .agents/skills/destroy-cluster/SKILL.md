---
name: destroy-cluster
description: Tear down a local bink cluster and clean up all associated resources
---

## What I do

I safely tear down a local bink cluster used for bootc-operator development and testing. I verify the cluster exists, remove it, clean up the kubeconfig, and confirm everything is gone.

## When to use me

- Done with development/testing and want to free resources
- Cluster is in a bad state and you want a clean start (destroy then redeploy)
- Cleaning up after e2e test runs

---

## Instructions for AI Agent

All commands run from the project root: `/home/ptalgulk/VSCode/bootc/bootc-operator`

### Step 1: Check prerequisites

```bash
echo -n "Make: "; make --version 2>/dev/null | head -1 || echo "MISSING"
echo -n "bink: "; bink version 2>/dev/null || echo "MISSING"
echo -n "Podman: "; podman --version 2>/dev/null || echo "MISSING"
```

### Step 2: Check if cluster exists

```bash
bink cluster list 2>/dev/null
```

If the default `e2e` cluster is not listed, tell the user there is nothing to tear down.

For a named cluster:

```bash
bink cluster list 2>/dev/null | grep <cluster-name>
```

### Step 3: Tear down the cluster

Default cluster (`e2e`):

```bash
make teardown-bink
```

Named cluster:

```bash
make teardown-bink BINK_CLUSTER_NAME=<name>
```

**What this does:**
1. Removes all bink worker nodes
2. Stops and removes the control-plane container
3. Stops the local container registry

This typically takes 10-30 seconds.

### Step 4: Clean up kubeconfig

```bash
rm -f kubeconfig-e2e
```

For a named cluster:

```bash
rm -f kubeconfig-<name>
```

Tell the user to unset KUBECONFIG in their terminal if it was pointing at the deleted file:

```bash
unset KUBECONFIG
```

### Step 5: Verify teardown

```bash
echo "=== Teardown Verification ==="

echo "--- Remaining bink clusters ---"
bink cluster list 2>/dev/null || echo "No clusters found"

echo "--- Remaining containers (bootc/bink related) ---"
podman ps -a --filter "name=e2e" --format "{{.Names}}" 2>/dev/null || echo "None"

echo "--- Registry on port 5000 ---"
ss -tlnp | grep :5000 || echo "Port 5000 is free"

echo "--- Kubeconfig file ---"
ls kubeconfig-e2e 2>/dev/null || echo "Kubeconfig removed"
```

**Clean state checklist:**
- [ ] No bink clusters listed (or the target cluster is gone)
- [ ] No containers with the cluster name running
- [ ] Port 5000 is free (registry stopped)
- [ ] Kubeconfig file removed

### Troubleshooting

| Symptom | Fix |
|---------|-----|
| `make teardown-bink` fails | Try `bink cluster delete e2e --force` directly |
| Containers still running | `podman rm -f <container-name>` |
| Port 5000 still in use | `podman stop <registry-container>` then `podman rm <registry-container>` |
| Volumes left behind | `podman volume prune` (ask user first — affects all unused volumes) |
| Teardown hangs | Ctrl+C, then manually remove with `bink cluster delete e2e --force` |
