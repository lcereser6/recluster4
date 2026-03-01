# Image URL to use all building/pushing image targets
IMG ?= recluster4:dev

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Kind Cluster Management

KIND_CLUSTER_NAME ?= recluster-dev

.PHONY: kind-create
kind-create: ## Create a Kind cluster for development (no-op if it already exists).
	@if $(KIND) get clusters 2>/dev/null | grep -q '^$(KIND_CLUSTER_NAME)$$'; then \
		echo "Kind cluster '$(KIND_CLUSTER_NAME)' already exists, skipping creation."; \
	else \
		$(KIND) create cluster --config kind-config.yaml; \
	fi

.PHONY: kind-delete
kind-delete: ## Delete the Kind development cluster.
	$(KIND) delete cluster --name $(KIND_CLUSTER_NAME)

.PHONY: kind-load
kind-load: docker-build ## Build and load the controller image into Kind.
	$(KIND) load docker-image ${IMG} --name $(KIND_CLUSTER_NAME)

.PHONY: kind-deploy
kind-deploy: manifests kustomize docker-build kind-load deploy-webhook ## Build, load, and deploy controller with webhook to Kind cluster.

.PHONY: kind-logs
kind-logs: ## Tail the controller manager logs in Kind.
	$(KUBECTL) logs -f -n recluster4-system deploy/recluster4-controller-manager -c manager

.PHONY: dev
dev: install run ## Install CRDs and run the controller locally (outside cluster).

##@ Recluster Autoscaler

# Image for the recluster cluster-autoscaler
RECLUSTER_AUTOSCALER_IMG ?= recluster/cluster-autoscaler:latest
RECLUSTER_AUTOSCALER_DIR ?= $(shell cd .. && pwd)/recluster-autoscaler/cluster-autoscaler
# Detect architecture (arm64 for M1/M2/M3 Macs, amd64 for Intel)
AUTOSCALER_ARCH ?= $(shell go env GOARCH)

.PHONY: autoscaler-build
autoscaler-build: ## Build the recluster cluster-autoscaler image.
	@echo "Building cluster-autoscaler binary for $(AUTOSCALER_ARCH)..."
	cd $(RECLUSTER_AUTOSCALER_DIR) && \
		CGO_ENABLED=0 GOOS=linux GOARCH=$(AUTOSCALER_ARCH) go build -o cluster-autoscaler-$(AUTOSCALER_ARCH) .
	@echo "Building Docker image..."
	$(CONTAINER_TOOL) build -t $(RECLUSTER_AUTOSCALER_IMG) -f $(RECLUSTER_AUTOSCALER_DIR)/Dockerfile.$(AUTOSCALER_ARCH) $(RECLUSTER_AUTOSCALER_DIR)

.PHONY: autoscaler-load
autoscaler-load: autoscaler-build ## Build and load the recluster cluster-autoscaler image into Kind.
	$(KIND) load docker-image $(RECLUSTER_AUTOSCALER_IMG) --name $(KIND_CLUSTER_NAME)

.PHONY: autoscaler-install
autoscaler-install: ## Install cluster-autoscaler with recluster provider.
	$(KUBECTL) apply -k config/autoscaler-recluster/

.PHONY: autoscaler-uninstall
autoscaler-uninstall: ## Uninstall cluster-autoscaler.
	$(KUBECTL) delete -k config/autoscaler-recluster/ --ignore-not-found

.PHONY: autoscaler-logs
autoscaler-logs: ## Tail the cluster-autoscaler logs.
	$(KUBECTL) logs -f -n kube-system deploy/cluster-autoscaler

.PHONY: autoscaler-deploy
autoscaler-deploy: autoscaler-load autoscaler-install ## Build, load, and deploy recluster cluster-autoscaler to Kind.

.PHONY: autoscaler-status
autoscaler-status: ## Show cluster autoscaler and RcNode status.
	@echo "=== Cluster Autoscaler ==="
	@$(KUBECTL) get pods -n kube-system -l app=cluster-autoscaler
	@echo "\n=== RcNodes ==="
	@$(KUBECTL) get rcnodes 2>/dev/null || echo "No RcNodes found"
	@echo "\n=== Kubernetes Nodes ==="
	@$(KUBECTL) get nodes

.PHONY: kind-full-setup
kind-full-setup: kind-create autoscaler-deploy install ## Create Kind cluster with recluster autoscaler and CRDs.

.PHONY: full-deploy
full-deploy: ## Full deployment from zero: cluster → build → CRDs → controller+webhook → certs → samples → monitoring.
	@echo ""
	@echo "╔══════════════════════════════════════════════════╗"
	@echo "║         recluster4 – Full Deploy Script         ║"
	@echo "╚══════════════════════════════════════════════════╝"
	@echo ""
	@# ── Step 1: Kind cluster ──────────────────────────────
	@echo "▶ [1/8] Kind cluster..."
	@if $(KIND) get clusters 2>/dev/null | grep -q '^$(KIND_CLUSTER_NAME)$$'; then \
		echo "  ✓ cluster '$(KIND_CLUSTER_NAME)' already exists"; \
	else \
		$(KIND) create cluster --config kind-config.yaml; \
	fi
	@echo ""
	@# ── Step 2: Build + load image ───────────────────────
	@echo "▶ [2/8] Building and loading controller image..."
	@$(CONTAINER_TOOL) build -t $(IMG) . -q
	@$(KIND) load docker-image $(IMG) --name $(KIND_CLUSTER_NAME)
	@echo "  ✓ $(IMG) loaded into kind"
	@echo ""
	@# ── Step 3: CRDs ─────────────────────────────────────
	@echo "▶ [3/8] Installing CRDs..."
	@$(MAKE) --no-print-directory manifests kustomize
	@$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -
	@echo "  ✓ CRDs installed"
	@echo ""
	@# ── Step 4: Monitoring (before webhook, so namespace exists) ──
	@echo "▶ [4/8] Deploying monitoring stack..."
	@$(KUBECTL) apply -k config/monitoring/
	@echo "  ✓ Prometheus + Grafana deploying"
	@echo ""
	@# ── Step 5: Webhook cert (creates namespace + secret BEFORE controller) ──
	@echo "▶ [5/8] Generating webhook TLS certificates..."
	@./hack/generate-webhook-certs.sh 2>&1 | tail -1
	@echo ""
	@# ── Step 6: Deploy controller + webhook config ───────
	@echo "▶ [6/8] Deploying controller and webhook..."
	@cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	@$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -
	@# Inject the CA bundle into the webhook (cert script may have run before the MWC existed)
	@sleep 2
	@CA_BUNDLE=$$($(KUBECTL) get secret webhook-server-cert -n recluster4-system -o jsonpath='{.data.ca\.crt}' 2>/dev/null || \
		$(KUBECTL) get secret webhook-server-cert -n recluster4-system -o jsonpath='{.data.tls\.crt}') && \
		$(KUBECTL) patch mutatingwebhookconfiguration recluster4-recluster-pod-scheduling-gate \
		--type='json' -p="[{\"op\": \"replace\", \"path\": \"/webhooks/0/clientConfig/caBundle\", \"value\": \"$${CA_BUNDLE}\"}]"
	@echo "  ✓ Controller + webhook deployed"
	@echo ""
	@# ── Step 7: Wait for controller ready ────────────────
	@echo "▶ [7/8] Waiting for controller to be ready..."
	@$(KUBECTL) rollout status deploy/recluster4-controller-manager -n recluster4-system --timeout=90s
	@echo "  ✓ Controller ready"
	@echo ""
	@# ── Step 8: Samples ──────────────────────────────────
	@echo "▶ [8/8] Installing sample RcNodes and RcPolicy..."
	@$(KUBECTL) apply -k config/samples/
	@echo "  ✓ Samples installed"
	@echo ""
	@# ── Wait for monitoring ──────────────────────────────
	@echo "⏳ Waiting for monitoring pods..."
	@$(KUBECTL) wait --for=condition=Ready --timeout=120s -n monitoring pod -l app=prometheus 2>/dev/null || true
	@$(KUBECTL) wait --for=condition=Ready --timeout=120s -n monitoring pod -l app=grafana 2>/dev/null || true
	@echo ""
	@# ── Port-forwards ────────────────────────────────────
	@pkill -f 'port-forward.*svc/prometheus' 2>/dev/null || true
	@pkill -f 'port-forward.*svc/grafana' 2>/dev/null || true
	@sleep 1
	@$(KUBECTL) port-forward svc/prometheus -n monitoring 9090:9090 >/dev/null 2>&1 &
	@$(KUBECTL) port-forward svc/grafana -n monitoring 3000:3000 >/dev/null 2>&1 &
	@sleep 2
	@echo "╔══════════════════════════════════════════════════╗"
	@echo "║              ✅ Deploy complete!                 ║"
	@echo "╠══════════════════════════════════════════════════╣"
	@echo "║  Prometheus : http://localhost:9090              ║"
	@echo "║  Grafana    : http://localhost:3000 (admin/admin)║"
	@echo "║                                                  ║"
	@echo "║  make status    – check all components           ║"
	@echo "║  make kind-logs – tail controller logs           ║"
	@echo "╚══════════════════════════════════════════════════╝"

.PHONY: full-reset
full-reset: ## Nuke everything and redeploy from scratch.
	@echo "🔥 Nuking everything..."
	@$(KIND) delete cluster --name $(KIND_CLUSTER_NAME) 2>/dev/null || true
	@rm -rf bin/ testbin/ 2>/dev/null || true
	@$(CONTAINER_TOOL) rmi $(IMG) 2>/dev/null || true
	@go clean -cache -testcache 2>/dev/null || true
	@pkill -f 'port-forward.*svc/prometheus' 2>/dev/null || true
	@pkill -f 'port-forward.*svc/grafana' 2>/dev/null || true
	@echo "  ✓ Clean slate"
	@echo ""
	@$(MAKE) --no-print-directory full-deploy

.PHONY: install-samples
install-samples: ## Install sample RcNodes and default RcPolicy.
	@echo "Installing sample RcNodes and RcPolicy..."
	$(KUBECTL) apply -k config/samples/
	@echo "✅ Samples installed!"

.PHONY: deploy-webhook
deploy-webhook: kustomize ## Deploy controller with webhook and configure TLS certs (standalone; prefer full-deploy).
	@cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -
	@echo "Generating webhook TLS certificates..."
	@./hack/generate-webhook-certs.sh
	@echo "Injecting CA bundle into webhook configuration..."
	@sleep 2
	@CA_BUNDLE=$$($(KUBECTL) get secret webhook-server-cert -n recluster4-system -o jsonpath='{.data.ca\.crt}' 2>/dev/null || \
		$(KUBECTL) get secret webhook-server-cert -n recluster4-system -o jsonpath='{.data.tls\.crt}') && \
		$(KUBECTL) patch mutatingwebhookconfiguration recluster4-recluster-pod-scheduling-gate \
		--type='json' -p="[{\"op\": \"replace\", \"path\": \"/webhooks/0/clientConfig/caBundle\", \"value\": \"$${CA_BUNDLE}\"}]"
	@echo "Restarting controller to pick up certs..."
	@$(KUBECTL) rollout restart deploy/recluster4-controller-manager -n recluster4-system
	@$(KUBECTL) rollout status deploy/recluster4-controller-manager -n recluster4-system

.PHONY: status
status: ## Show status of all components.
	@echo "=== Kind Cluster ==="
	@$(KIND) get clusters 2>/dev/null || echo "No clusters"
	@echo "\n=== Controller ==="
	@$(KUBECTL) get pods -n recluster4-system 2>/dev/null || echo "Not deployed"
	@echo "\n=== Webhook ==="
	@$(KUBECTL) get mutatingwebhookconfiguration 2>/dev/null | grep recluster || echo "Not configured"
	@echo "\n=== Cluster Autoscaler ==="
	@$(KUBECTL) get pods -n kube-system -l app=cluster-autoscaler 2>/dev/null || echo "Not deployed"
	@echo "\n=== RcNodes ==="
	@$(KUBECTL) get rcnodes 2>/dev/null || echo "No RcNodes"
	@echo "\n=== Kubernetes Nodes ==="
	@$(KUBECTL) get nodes 2>/dev/null || echo "No nodes"
	@echo "\n=== Monitoring ==="
	@$(KUBECTL) get pods -n monitoring 2>/dev/null || echo "Not deployed"

##@ Monitoring

.PHONY: monitoring-install
monitoring-install: ## Install Prometheus and Grafana monitoring stack.
	@echo "Installing monitoring stack (Prometheus + Grafana)..."
	$(KUBECTL) apply -k config/monitoring/
	@echo "Waiting for monitoring pods to be ready..."
	@$(KUBECTL) wait --for=condition=Ready --timeout=120s -n monitoring pod -l app=prometheus 2>/dev/null || true
	@$(KUBECTL) wait --for=condition=Ready --timeout=120s -n monitoring pod -l app=grafana 2>/dev/null || true
	@echo "✅ Monitoring installed!"
	@echo "  Prometheus: kubectl port-forward svc/prometheus -n monitoring 9090:9090"
	@echo "  Grafana: kubectl port-forward svc/grafana -n monitoring 3000:3000 (admin/admin)"

.PHONY: monitoring-uninstall
monitoring-uninstall: ## Uninstall the monitoring stack.
	$(KUBECTL) delete -k config/monitoring/ --ignore-not-found

.PHONY: monitoring-port-forward
monitoring-port-forward: ## Port-forward Prometheus (9090) and Grafana (3000) to localhost.
	@echo "Starting port-forwards (press Ctrl+C to stop)..."
	@echo "  Prometheus: http://localhost:9090"
	@echo "  Grafana: http://localhost:3000 (admin/admin)"
	@$(KUBECTL) port-forward svc/prometheus -n monitoring 9090:9090 & \
	$(KUBECTL) port-forward svc/grafana -n monitoring 3000:3000 & \
	wait

##@ Webhook & Cert-Manager

CERT_MANAGER_VERSION ?= v1.17.2

.PHONY: cert-manager-install
cert-manager-install: ## Install cert-manager in the cluster.
	$(KUBECTL) apply -f https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.yaml
	@echo "Waiting for cert-manager to be ready..."
	$(KUBECTL) wait --for=condition=Available --timeout=120s -n cert-manager deployment/cert-manager
	$(KUBECTL) wait --for=condition=Available --timeout=120s -n cert-manager deployment/cert-manager-webhook
	$(KUBECTL) wait --for=condition=Available --timeout=120s -n cert-manager deployment/cert-manager-cainjector

.PHONY: cert-manager-uninstall
cert-manager-uninstall: ## Uninstall cert-manager from the cluster.
	$(KUBECTL) delete -f https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.yaml --ignore-not-found

.PHONY: webhook-install
webhook-install: manifests kustomize ## Install the pod scheduling gate webhook.
	$(KUBECTL) apply -k config/webhook/
	$(KUBECTL) apply -k config/certmanager/

.PHONY: webhook-uninstall
webhook-uninstall: ## Uninstall the pod scheduling gate webhook.
	$(KUBECTL) delete -k config/webhook/ --ignore-not-found
	$(KUBECTL) delete -k config/certmanager/ --ignore-not-found

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd:allowDangerousTypes=true webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
.PHONY: test-e2e
test-e2e: manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	@command -v $(KIND) >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@$(KIND) get clusters | grep -q 'kind' || { \
		echo "No Kind cluster is running. Please start a Kind cluster before running the e2e tests."; \
		exit 1; \
	}
	go test ./test/e2e/ -v -ginkgo.v

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name recluster4-builder
	$(CONTAINER_TOOL) buildx use recluster4-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm recluster4-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
KUSTOMIZE_VERSION ?= v5.6.0
CONTROLLER_TOOLS_VERSION ?= v0.17.2
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v1.63.4

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
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
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef
