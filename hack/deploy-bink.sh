#!/usr/bin/env bash
# Configure the operator after make deploy-bink installs its Helm chart.
set -euo pipefail

: "${KUBECONFIG:?must be set}"
: "${KUBECTL:=kubectl}"
cd "$(dirname "${BASH_SOURCE[0]}")/.."

"$KUBECTL" --kubeconfig "$KUBECONFIG" wait --for=condition=Established \
    crd/bootcoperatorconfigs.node.bootc.dev --timeout=1m
"$KUBECTL" --kubeconfig "$KUBECONFIG" apply -f config/bink/operator-config.yaml
"$KUBECTL" --kubeconfig "$KUBECONFIG" -n bootc-operator rollout restart \
    deployment/bootc-operator-controller
"$KUBECTL" --kubeconfig "$KUBECONFIG" -n bootc-operator rollout status \
    deployment/bootc-operator-controller --timeout=3m
