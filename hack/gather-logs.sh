#!/bin/bash
# Gather diagnostic logs from a cluster.
#
# Usage: hack/gather-logs.sh <output-dir> [node-names...]
#
# Expects KUBECONFIG from environment.
# Each command's output is written to a separate file in <output-dir>.
# Individual command failures are non-fatal.

set -euo pipefail

if [[ $# -lt 1 ]]; then
    echo "Usage: $0 <output-dir> [node-names...]" >&2
    exit 1
fi

: "${KUBECONFIG:?must be set}"

output_dir="$1"
shift
nodes=("$@")

mkdir -p "${output_dir}"

echo "Gathering logs to ${output_dir}..."

run() {
    local filename="$1"
    shift
    echo "  ${filename}"
    "$@" > "${output_dir}/${filename}" 2>&1 || true
}

# Cluster-wide commands
run "k-get-pods.txt"              kubectl get pods -n bootc-operator -o wide
run "k-describe-pods.txt"         kubectl describe pods -n bootc-operator
run "k-get-deployment.yaml"       kubectl get deployment -n bootc-operator -o yaml
run "k-describe-bootcnodepools.txt" kubectl describe bootcnodepools
run "k-describe-bootcnodes.txt"   kubectl describe bootcnodes
run "k-get-events.txt"            kubectl get events -n bootc-operator --sort-by=.lastTimestamp

# Pod logs
for pod in $(kubectl get pods -n bootc-operator -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    run "k-logs-${pod}.log"          kubectl logs -n bootc-operator "${pod}" --all-containers
    run "k-logs-${pod}-previous.log" kubectl logs -n bootc-operator "${pod}" --all-containers --previous
done

# Per-node commands via daemon pod exec
for node in "${nodes[@]}"; do
    run "k-describe-node-${node}.txt" kubectl describe node "${node}"

    daemon_pod=$(kubectl get pods -n bootc-operator \
        -l app.kubernetes.io/name=bootc-operator,app.kubernetes.io/component=daemon \
        --field-selector "spec.nodeName=${node}" \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)

    if [[ -n "${daemon_pod}" ]]; then
        run "journal-${node}.txt" kubectl exec -n bootc-operator "${daemon_pod}" -- \
            journalctl --no-pager
    fi
done

echo "Done."
