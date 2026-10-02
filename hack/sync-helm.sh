#!/usr/bin/env bash
# Sync Helm chart with Kustomize manifests.
# Generates CRDs, RBAC, and workload templates from kustomize sources.
# Run: make helm

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
CHART_DIR="$REPO_ROOT/chart/bootc-operator"
CONFIG_DIR="$REPO_ROOT/config"
YQ="${YQ:-yq}"

command -v "$YQ" >/dev/null 2>&1 || { echo "error: yq required (run: make yq)" >&2; exit 1; }

# --- CRDs ---
mkdir -p "$CHART_DIR/crds"
cp "$CONFIG_DIR"/crd/bases/*.yaml "$CHART_DIR/crds/"
echo "Synced CRDs"

# --- RBAC ---
# gen_rbac OUTPUT_FILE SOURCE_FILE KIND NAME_SUFFIX COMPONENT [NAMESPACE]
gen_rbac() {
    local output="$1" source="$2" kind="$3" suffix="$4" component="$5" ns="${6:-}"
    local rules
    rules=$("$YQ" -I2 '.rules' "$source")
    {
        echo "apiVersion: rbac.authorization.k8s.io/v1"
        echo "kind: ${kind}"
        echo "metadata:"
        echo "  name: {{ include \"bootc-operator.fullname\" . }}-${suffix}"
        [ -n "$ns" ] && echo "  namespace: ${ns}"
        echo "  labels:"
        echo "    {{- include \"bootc-operator.labels\" . | nindent 4 }}"
        echo "    app.kubernetes.io/component: ${component}"
        echo "rules:"
        echo "$rules"
    } > "$output"
}

gen_rbac "$CHART_DIR/templates/controller-clusterrole.yaml" "$CONFIG_DIR/rbac/role.yaml" \
    ClusterRole controller controller
gen_rbac "$CHART_DIR/templates/daemon-clusterrole.yaml" "$CONFIG_DIR/rbac/daemon_role.yaml" \
    ClusterRole daemon daemon
gen_rbac "$CHART_DIR/templates/leader-election-role.yaml" "$CONFIG_DIR/rbac/leader_election_role.yaml" \
    Role leader-election controller "{{ .Release.Namespace }}"
echo "Synced RBAC rules"

# --- Workloads ---
# gen_workload SOURCE KIND NAME OUTPUT_FILE
#   Copies the kustomize manifest and patches metadata/image for Helm.
gen_workload() {
    local source="$1" kind="$2" name="$3" output="$4"

    "$YQ" -I2 "select(.kind == \"${kind}\") |
        .metadata.name = \"HELM_FULLNAME-${name}\" |
        .metadata.namespace = \"HELM_NAMESPACE\" |
        .spec.template.spec.serviceAccountName = \"HELM_FULLNAME-${name}\" |
        .spec.template.spec.containers[0].image = \"HELM_IMAGE\" |
        del(.metadata.labels.\"app.kubernetes.io/managed-by\") |
        del(.spec.template.spec.containers[0].volumeMounts | select(length == 0)) |
        del(.spec.template.spec.volumes | select(length == 0)) |
        ... comments=\"\"
    " "$source" | sed \
        -e 's/HELM_FULLNAME/{{ include "bootc-operator.fullname" . }}/g' \
        -e 's/HELM_NAMESPACE/{{ .Release.Namespace }}/g' \
        -e 's/HELM_IMAGE/{{ include "bootc-operator.image" . }}/g' \
    > "$output"

    sed -i '/image: {{ include "bootc-operator\.image" \. }}/a\
          {{- with .Values.image.pullPolicy }}\
          imagePullPolicy: {{ . }}\
          {{- end }}' "$output"

    sed -i '/^      containers:$/i\
      {{- with .Values.imagePullSecrets }}\
      imagePullSecrets:\
        {{- toYaml . | nindent 8 }}\
      {{- end }}' "$output"
}

gen_workload "$CONFIG_DIR/manager/manager.yaml" Deployment controller \
    "$CHART_DIR/templates/controller-deployment.yaml"

# Inject controller.extraArgs into the args list
sed -i '/^          image: {{ include "bootc-operator\.image" \. }}/i\
            {{- range .Values.controller.extraArgs }}\
            - {{ . | quote }}\
            {{- end }}' "$CHART_DIR/templates/controller-deployment.yaml"

gen_workload "$CONFIG_DIR/daemon/daemon.yaml" DaemonSet daemon \
    "$CHART_DIR/templates/daemon-daemonset.yaml"
echo "Synced workloads"

# --- Bindings & ServiceAccounts ---
# helm_sed: shared sed replacements for Helm placeholders.
helm_sed() {
    sed \
        -e 's/HELM_FULLNAME/{{ include "bootc-operator.fullname" . }}/g' \
        -e 's/HELM_NAMESPACE/{{ .Release.Namespace }}/g'
}

# gen_binding SOURCE OUTPUT BINDING_NAME ROLE_KIND ROLE_SUFFIX SA_SUFFIX
gen_binding() {
    local source="$1" output="$2" name="$3" role_kind="$4" role_suffix="$5" sa_suffix="$6"

    "$YQ" -I2 "
        .metadata.name = \"HELM_FULLNAME-${name}\" |
        del(.metadata.labels.\"app.kubernetes.io/managed-by\") |
        .roleRef.name = \"HELM_FULLNAME-${role_suffix}\" |
        .subjects[0].name = \"HELM_FULLNAME-${sa_suffix}\" |
        .subjects[0].namespace = \"HELM_NAMESPACE\"
    " "$source" | helm_sed > "$output"
}

gen_binding "$CONFIG_DIR/rbac/role_binding.yaml" \
    "$CHART_DIR/templates/controller-clusterrolebinding.yaml" \
    controller ClusterRole controller controller
gen_binding "$CONFIG_DIR/rbac/daemon_role_binding.yaml" \
    "$CHART_DIR/templates/daemon-clusterrolebinding.yaml" \
    daemon ClusterRole daemon daemon
gen_binding "$CONFIG_DIR/rbac/leader_election_role_binding.yaml" \
    "$CHART_DIR/templates/leader-election-rolebinding.yaml" \
    leader-election Role leader-election controller

# gen_sa SOURCE OUTPUT SA_NAME
gen_sa() {
    local source="$1" output="$2" name="$3"

    "$YQ" -I2 "
        .metadata.name = \"HELM_FULLNAME-${name}\" |
        .metadata.namespace = \"HELM_NAMESPACE\" |
        del(.metadata.labels.\"app.kubernetes.io/managed-by\")
    " "$source" | helm_sed > "$output"
}

gen_sa "$CONFIG_DIR/rbac/controller_service_account.yaml" \
    "$CHART_DIR/templates/controller-serviceaccount.yaml" controller
gen_sa "$CONFIG_DIR/rbac/daemon_service_account.yaml" \
    "$CHART_DIR/templates/daemon-serviceaccount.yaml" daemon
echo "Synced bindings and service accounts"
