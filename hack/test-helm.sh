#!/usr/bin/env bash
# Render the Helm chart with representative values and check the output.
# Run: make helm-test

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART_DIR="$REPO_ROOT/chart/bootc-operator"
HELM="${HELM:-helm}"
YQ="${YQ:-yq}"
NS=bootc-operator
SA=test-bootc-operator-controller

fail() { echo "FAIL: $*" >&2; exit 1; }

# A go-installed helm lacks build-time version info and assumes Kubernetes
# v1.20, which the chart's kubeVersion constraint rejects.
render() {
    "$HELM" template test "$CHART_DIR" --namespace "$NS" --kube-version 1.34.0 "$@"
}

# check MANIFESTS FLAG_VALUE NAMESPACES...
#   Asserts that the controller flag is FLAG_VALUE, that each namespace has
#   exactly one secret-reading Role bound to the controller ServiceAccount,
#   and that no ClusterRole (controller or daemon) grants access to secrets.
check() {
    local manifests="$1" flag="$2"; shift 2
    local want got
    want=$(printf '%s\n' "$@" | sort)

    got=$(echo "$manifests" | "$YQ" -N 'select(.kind == "Deployment") |
        .spec.template.spec.containers[0].args[] | select(test("^--pull-secret-namespaces="))')
    [ "$got" = "--pull-secret-namespaces=$flag" ] || fail "controller flag is [$got]"

    roles=$(echo "$manifests" | "$YQ" -N 'select(.kind == "Role") |
        select(.rules | any_c(.resources | any_c(. == "secrets"))) |
        .metadata.namespace + "/" + .metadata.name' | sort)
    [ "$(echo "$roles" | cut -d/ -f1)" = "$want" ] || fail "secret Roles: [$roles]"

    got=$(echo "$manifests" | "$YQ" -N 'select(.kind == "RoleBinding") |
        select(.subjects | any_c(.name == "'"$SA"'" and .namespace == "'"$NS"'")) |
        .metadata.namespace + "/" + .roleRef.name' | sort)
    # The leader election binding has no namespace in the template.
    got=$(echo "$got" | grep -v '^/')
    [ "$got" = "$roles" ] || fail "controller RoleBindings [$got], want [$roles]"

    got=$(echo "$manifests" | "$YQ" -N 'select(.kind == "ClusterRole") |
        select(.rules | any_c(.resources | any_c(. == "secrets"))) | .metadata.name')
    [ -z "$got" ] || fail "ClusterRoles grant secrets access: [$got]"
}

"$HELM" dependency build "$CHART_DIR" >/dev/null

check "$(render)" "$NS" "$NS"
# Duplicates and the release namespace itself must not produce extra RBAC.
check "$(render --set "pullSecretNamespaces={team-a,$NS,team-b,team-a}")" \
    "$NS,team-a,team-b" "$NS" team-a team-b

for bad in "pullSecretNamespaces=team-a" "pullSecretNamespaces={team-a,}" \
    "pullSecretNamespaces={Team-A}"; do
    if render --set "$bad" >/dev/null 2>&1; then fail "accepted --set $bad"; fi
done
echo "ok"
