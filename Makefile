# Image URL to use all building/pushing image targets
IMG ?= bootc-operator:dev
CONTAINER_TOOL ?= podman
# Bink cluster settings. deploy-bink and e2e share the same cluster by default.
# To use a separate dev cluster: make deploy-bink BINK_CLUSTER_NAME=dev
BINK_CLUSTER_NAME ?= e2e
KUBECONFIG_BINK ?= ./kubeconfig-$(BINK_CLUSTER_NAME)
ARTIFACTS ?= $(abspath _output/logs)
BINK_NODE_DISK_IMAGE ?= ghcr.io/bootc-dev/bink/node:v1.35-fedora-44-disk
BINK_NODE_DISK_IMAGE_COMPOSEFS ?=
BINK_LOCAL_REGISTRY_NODE_IMAGE ?= registry.cluster.local:5000/node
# YEAR defines the year value used for substituting the YEAR placeholder in the boilerplate header.
YEAR ?= $(shell date +%Y)

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate CRD and RBAC manifests.
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate DeepCopy method implementations.
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: unit
unit: manifests generate setup-envtest ## Run unit tests (envtest). V=1 for verbose. RUN=<regex> to filter.
	KUBEBUILDER_ASSETS="$(shell "$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path)" go test $(if $(V),-v) $(if $(RUN),-run $(RUN)) $$(go list ./... | grep -v /test/e2e)

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter.
	"$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes.
	"$(GOLANGCI_LINT)" run --fix

.PHONY: e2e
e2e: ## Run e2e tests (requires: make deploy-bink). V=1 for verbose. RUN=<regex> to filter.
# NB: we `cd` here instead of passing a package path to `go test` so that `-v`
# actually gives us streaming output (otherwise, it spawns a subprocess for
# each package, even though we just have one here--but I really like streaming
# output...).
	rm -rf $(ARTIFACTS)
	cd test/e2e && KUBECONFIG=$(abspath $(KUBECONFIG_BINK)) BINK_CLUSTER_NAME=$(BINK_CLUSTER_NAME) \
		$(if $(BINK_NODE_IMAGE),BINK_NODE_IMAGE=$(BINK_NODE_IMAGE)) \
		BINK_NODE_DISK_IMAGE=$(BINK_NODE_DISK_IMAGE) \
		$(if $(BINK_NODE_DISK_IMAGE_COMPOSEFS),BINK_NODE_DISK_IMAGE_COMPOSEFS=$(BINK_NODE_DISK_IMAGE_COMPOSEFS)) \
		BINK_LOCAL_REGISTRY_NODE_IMAGE=$(BINK_LOCAL_REGISTRY_NODE_IMAGE) \
		ARTIFACTS=$(ARTIFACTS) \
		BINK_NODE_IMAGE_DIGEST=$$(skopeo inspect --tls-verify=false --format '{{.Digest}}' docker://localhost:5000/node:latest) \
		BINK_NODE_IMAGE_UPDATE_DIGEST=$$(skopeo inspect --tls-verify=false docker://localhost:5000/node:update | jq -r '.Digest') \
		go test -timeout 10m -count=1 $(if $(V),-v) $(if $(RUN),-run $(RUN)) .

##@ Build

.PHONY: build
build: build-manager build-daemon ## Build all binaries.

.PHONY: build-manager
build-manager: ## Build manager binary.
	go build -o bin/manager ./cmd/controller/

.PHONY: build-daemon
build-daemon: ## Build daemon binary.
	go build -o bin/daemon ./cmd/daemon/

.PHONY: buildimg
buildimg: ## Build container image.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: build-update-image
build-update-image: ## Build a derived node image for update testing and push to bink registry.
	@printf 'FROM localhost:5000/node:latest\nRUN touch /usr/share/update-marker\n' | \
		podman build -t localhost:5000/node:update -f - .
	podman push --tls-verify=false localhost:5000/node:update

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	"$(KUSTOMIZE)" build config/crd | "$(KUBECTL)" apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config.
	"$(KUSTOMIZE)" build config/crd | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize yq ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	"$(KUSTOMIZE)" build config/default | \
		"$(YQ)" '(select(.kind == "Deployment") | .spec.template.spec.containers[] | select(.name == "manager")).image = "$(IMG)"' | \
		"$(YQ)" '(select(.kind == "DaemonSet") | .spec.template.spec.containers[] | select(.name == "daemon")).image = "$(IMG)"' | \
		"$(KUBECTL)" apply --server-side -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config.
	"$(KUSTOMIZE)" build config/default | "$(KUBECTL)" delete --ignore-not-found=$(ignore-not-found) -f -

# In-cluster image used by deploy-bink after pushing to the bink registry.
# Note the :latest tag here: this makes the pull policy be Always.
IMG_BINK ?= registry.cluster.local:5000/bootc-operator-e2e:latest

.PHONY: seed-node-image
seed-node-image: ## Pull the bootc node image by digest and push to the bink registry.
	bink registry start
	podman pull $(BINK_NODE_DISK_IMAGE)
	bootc_img=$$(podman inspect --format '{{index .Config.Labels "bink.bootc-image"}}' $(BINK_NODE_DISK_IMAGE)) && \
		bootc_digest=$$(podman inspect --format '{{index .Config.Labels "bink.bootc-image-digest"}}' $(BINK_NODE_DISK_IMAGE)) && \
		podman pull "$$bootc_img@$$bootc_digest" && \
		podman tag "$$bootc_img@$$bootc_digest" localhost:5000/node:latest
	podman push --tls-verify=false localhost:5000/node:latest

.PHONY: start-bink
start-bink: seed-node-image ## Start a bink cluster (idempotent).
	bink cluster list 2>&1 | grep -qw $(BINK_CLUSTER_NAME) || { \
		node_digest=$$(skopeo inspect --tls-verify=false --format '{{.Digest}}' docker://localhost:5000/node:latest) && \
		bink cluster start --cluster-name $(BINK_CLUSTER_NAME) --node-name controller --api-port 0 --expose $(KUBECONFIG_BINK) \
		--node-image $(BINK_NODE_DISK_IMAGE) --target-imgref $(BINK_LOCAL_REGISTRY_NODE_IMAGE)@$$node_digest; }
	kubectl --kubeconfig $(KUBECONFIG_BINK) wait --for=condition=Ready node/controller --timeout=5m

.PHONY: deploy-bink
deploy-bink: start-bink build-update-image kustomize ## Deploy to a bink cluster (requires: buildimg).
	podman push --tls-verify=false $(IMG) localhost:5000/bootc-operator-e2e:latest
	# On re-deploy, restart the rollout to force a re-pull of the :latest tag.
	# On fresh deploy, skip the restart -- the pod is already pulling the correct image.
	@existed=$$(kubectl --kubeconfig $(KUBECONFIG_BINK) -n bootc-operator get deploy bootc-operator-controller-manager -o name 2>/dev/null || true) && \
	$(MAKE) deploy KUBECONFIG=$(abspath $(KUBECONFIG_BINK)) IMG=$(IMG_BINK) && \
	if [ -n "$$existed" ]; then \
		kubectl --kubeconfig $(KUBECONFIG_BINK) -n bootc-operator rollout restart deployment/bootc-operator-controller-manager; \
	fi
	kubectl --kubeconfig $(KUBECONFIG_BINK) -n bootc-operator rollout status deployment/bootc-operator-controller-manager --timeout=3m

.PHONY: gather-bink
gather-bink: ## Gather diagnostic logs from the bink cluster.
	KUBECONFIG=$(abspath $(KUBECONFIG_BINK)) BINK_CLUSTER_NAME=$(BINK_CLUSTER_NAME) \
		hack/gather-logs.sh $(ARTIFACTS)/gather-bink controller

.PHONY: teardown-bink
teardown-bink: ## Tear down the bink cluster.
	bink cluster stop --remove-data --cluster-name $(BINK_CLUSTER_NAME)
	rm -f $(KUBECONFIG_BINK)

##@ Dependencies

LOCALBIN ?= $(shell pwd -P)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint
YQ ?= $(LOCALBIN)/yq

KUSTOMIZE_VERSION ?= v5.8.1
CONTROLLER_TOOLS_VERSION ?= v0.20.1
GOLANGCI_LINT_VERSION ?= v2.11.4
YQ_VERSION ?= v4.53.2

# ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell v='$(call gomodver,sigs.k8s.io/controller-runtime)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_VERSION manually (controller-runtime replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?([0-9]+)\.([0-9]+).*/release-\1.\2/')
# ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell v='$(call gomodver,k8s.io/api)'; \
  [ -n "$$v" ] || { echo "Set ENVTEST_K8S_VERSION manually (k8s.io/api replace has no tag)" >&2; exit 1; }; \
  printf '%s\n' "$$v" | sed -E 's/^v?[0-9]+\.([0-9]+).*/1.\1/')

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@"$(ENVTEST)" use $(ENVTEST_K8S_VERSION) --bin-dir "$(LOCALBIN)" -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: yq
yq: $(YQ) ## Download yq locally if necessary.
$(YQ): $(LOCALBIN)
	$(call go-install-tool,$(YQ),github.com/mikefarah/yq/v4,$(YQ_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef

define gomodver
$(shell go list -m -f '{{if .Replace}}{{.Replace.Version}}{{else}}{{.Version}}{{end}}' $(1) 2>/dev/null)
endef
