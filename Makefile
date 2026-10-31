VERSION ?= 0.0.0-dev
IMAGE_TAG_BASE ?= agentrally-operator
IMG ?= $(IMAGE_TAG_BASE):v$(VERSION)
CHART_OCI_REPO ?= oci://ghcr.io/chizhang21/helm
CONTAINER_TOOL ?= docker
export VERSION

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec
.DEFAULT_GOAL := build

##@ Development

.PHONY: help generate fmt test e2e run check lint verify-generated clean
help: ## List commands.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  %-20s %s\n", $$1, $$2 } /^##@/ { printf "\n%s\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

generate: controller-gen ## Generate DeepCopy code, chart CRDs and RBAC.
	GOWORK=off $(CONTROLLER_GEN) object paths="./api/..."
	CONTROLLER_GEN="$(CONTROLLER_GEN)" bash hack/generate-manifests.sh

# Optional local helpers stay callable without expanding the public command list.
fmt:
	go fmt ./...

test: ## Run Go tests without generating or formatting files.
	GOWORK=off go test -count=1 -timeout 120s ./...

e2e: kind ## Test the real upstream lifecycle in a disposable kind cluster.
	KIND="$(KIND)" bash hack/e2e.sh

run:
	go run ./cmd/main.go

lint: golangci-lint
	$(GOLANGCI_LINT) run

verify-generated:
	python3 hack/verify-generated.py

check: lint verify-generated ## Lint and check source, generated files, tests and chart.
	python3 hack/verify-source.py
	python3 -m unittest discover -s tests/packaging -p '*_test.py'
	$(MAKE) test
	GOWORK=off go test -tags=e2e ./tests/e2e -run '^$$'
	GOWORK=off go vet ./...
	helm lint chart
	helm template agentrally-operator chart -n agentrally-system >/dev/null
	helm lint chart --set connector.enabled=true --set connector.endpoint=https://backend.example --set connector.id=test-cluster --set connector.credentialsSecret=test-credentials

clean: ## Remove build outputs, staging directories and Python bytecode.
	rm -rf dist .cache/chart.* .cache/generated.*
	find hack tests -type d -name __pycache__ -prune -exec rm -rf {} +

##@ Build and packaging

.PHONY: build image-build image-push chart-package chart-push
build: ## Compile the operator binary, including the optional Connector.
	mkdir -p dist
	GOWORK=off go build -trimpath -o dist/operator ./cmd

image-build: ## Build the operator image.
	$(CONTAINER_TOOL) build -f Containerfile -t "$(IMG)" .

image-push: ## Push the operator image.
	$(CONTAINER_TOOL) push "$(IMG)"

chart-package: ## Package a versioned chart with the selected image defaults.
	bash hack/package-chart.sh "$(IMG)" "$(CURDIR)/dist"

chart-push: chart-package ## Package and push the chart to CHART_OCI_REPO.
	helm push "$(CURDIR)/dist/agentrally-operator-$(VERSION).tgz" "$(CHART_OCI_REPO)"

# Internal tools

include mk/tools.mk
