#!/usr/bin/env bash
# Configure and deploy the operator after make deploy-bink prepares images and CRDs.
set -euo pipefail

: "${KUBECONFIG:?must be set}" "${IMG:?must be set}" "${YQ:?must be set}"
: "${KUBECTL:=kubectl}" "${MAKE:=make}"
cd "$(dirname "${BASH_SOURCE[0]}")/.."

"$KUBECTL" --kubeconfig "$KUBECONFIG" wait --for=condition=Established \
    crd/bootcoperatorconfigs.node.bootc.dev --timeout=1m
if ! configs_json=$("$KUBECTL" --kubeconfig "$KUBECONFIG" get bootcoperatorconfigs -o json); then
    printf 'Cannot list BootcOperatorConfig resources in the bink cluster.\n' >&2
    exit 1
fi
if ! config_count=$(jq -r '.items | length' <<< "$configs_json") || \
    ! configs=$(jq -r '[.items[].metadata.name] | join(", ")' <<< "$configs_json"); then
    printf 'Cannot read BootcOperatorConfig names from the bink cluster response.\n' >&2
    exit 1
fi
if (( config_count > 1 )); then
    printf 'Bink requires at most one BootcOperatorConfig; found: %s\n' "$configs" >&2
    exit 1
fi
existing_deployment=$("$KUBECTL" --kubeconfig "$KUBECONFIG" -n bootc-operator \
    get deployment bootc-operator-controller-manager --ignore-not-found -o name)
if (( config_count == 0 )); then
    "$KUBECTL" --kubeconfig "$KUBECONFIG" create -f config/bink/operator-config.yaml
else
    patch=$("$YQ" -o=json '{"spec": .spec}' config/bink/operator-config.yaml)
    "$KUBECTL" --kubeconfig "$KUBECONFIG" patch bootcoperatorconfig "$configs" \
        --type=merge --patch "$patch"
fi
"$MAKE" deploy KUBECONFIG="$KUBECONFIG" IMG="$IMG"
if [[ -n "$existing_deployment" ]]; then
    "$KUBECTL" --kubeconfig "$KUBECONFIG" -n bootc-operator rollout restart \
        deployment/bootc-operator-controller-manager
fi
"$KUBECTL" --kubeconfig "$KUBECONFIG" -n bootc-operator rollout status \
    deployment/bootc-operator-controller-manager --timeout=3m
