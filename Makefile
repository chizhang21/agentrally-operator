# Resolve without writing; packaging targets save the selected identity below.
# Explicit version, image and Chart destination override the local build record.
ifneq ($(filter configure version docker-build docker-push chart-package chart-push,$(MAKECMDGOALS)),)
BUILD_IDENTITY := $(shell python3 -B hack/build_state.py resolve \
  $(if $(filter version,$(MAKECMDGOALS)),--dev) \
  $(if $(filter undefined,$(origin VERSION)),,--version "$(VERSION)") \
  $(if $(filter undefined,$(origin IMG)),,--image "$(IMG)") \
  $(if $(filter undefined,$(origin IMAGE_TAG_BASE)),,--image-base "$(IMAGE_TAG_BASE)") \
  $(if $(filter undefined,$(origin CHART_OCI_REPO)),,--chart-repo "$(CHART_OCI_REPO)"))
ifeq ($(BUILD_IDENTITY),)
$(error Cannot select package identity; check the error above or run make clean to reset local state)
endif
VERSION := $(word 1,$(BUILD_IDENTITY))
IMG := $(word 2,$(BUILD_IDENTITY))
CHART_OCI_REPO := $(word 3,$(BUILD_IDENTITY))
export VERSION IMG
endif
IMAGE_TAG_BASE ?= arally-operator
CONTAINER_TOOL ?= docker

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec
.DEFAULT_GOAL := build

##@ Development

.PHONY: help configure version save-build-state generate fmt test e2e run check lint verify-generated clean
help: ## List commands.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  %-20s %s\n", $$1, $$2 } /^##@/ { printf "\n%s\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

# Version selection happens above; this internal target only saves that identity.
save-build-state:
	@python3 -B hack/build_state.py save --version "$(VERSION)" --image "$(IMG)" --chart-repo "$(CHART_OCI_REPO)"

version: save-build-state ## Save the dev version or explicit VERSION; keep repository settings.

configure: save-build-state ## Save IMAGE_TAG_BASE and CHART_OCI_REPO without building or uploading.

generate: controller-gen ## Generate DeepCopy code, chart CRDs and RBAC.
	GOWORK=off $(CONTROLLER_GEN) object paths="./api/..."
	CONTROLLER_GEN="$(CONTROLLER_GEN)" bash hack/generate-manifests.sh

fmt: ## Format Go code.
	go fmt ./...

test: ## Run Go tests without generating or formatting files.
	GOWORK=off go test -count=1 -timeout 120s ./...

e2e: kind ## Test the real upstream lifecycle in a disposable kind cluster.
	KIND="$(KIND)" bash hack/e2e.sh

run: ## Run the manager against the current kubeconfig.
	go run ./cmd/main.go

lint: golangci-lint ## Run Go lint checks.
	$(GOLANGCI_LINT) run

verify-generated: ## Check DeepCopy, CRD and RBAC generation for drift.
	python3 hack/verify-generated.py

check: verify-generated ## Check source, generated files, tests and chart.
	python3 hack/verify-source.py
	python3 -m unittest discover -s tests/packaging -p '*_test.py'
	$(MAKE) test
	GOWORK=off go test -tags=e2e ./tests/e2e -run '^$$'
	GOWORK=off go vet ./...
	helm lint chart
	helm template arally-operator chart -n arally-system >/dev/null
	helm lint chart --set connector.enabled=true --set connector.endpoint=https://backend.example --set connector.id=test-cluster --set connector.credentialsSecret=test-credentials

clean: ## Remove build outputs and downloaded tools.
	rm -rf dist .cache

##@ Build and packaging

.PHONY: build docker-build docker-push chart-package chart-push
build: ## Compile the operator binary, including the optional Connector.
	mkdir -p dist
	GOWORK=off go build -trimpath -o dist/operator ./cmd

docker-build: save-build-state ## Build the operator image.
	$(CONTAINER_TOOL) build -f Dockerfile -t "$(IMG)" .

docker-push: save-build-state ## Push the operator image.
	$(CONTAINER_TOOL) push "$(IMG)"

chart-package: save-build-state ## Package a versioned chart with the selected image defaults.
	bash hack/package-chart.sh "$(IMG)" "$(CURDIR)/dist"

chart-push: chart-package ## Package and push the chart to CHART_OCI_REPO.
	helm push "$(CURDIR)/dist/arally-operator-$(VERSION).tgz" "$(CHART_OCI_REPO)"

##@ Tools

include mk/tools.mk
