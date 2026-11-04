VERSION ?= 0.0.0-dev
IMAGE_TAG_BASE ?= agentrally-operator
IMG ?= $(IMAGE_TAG_BASE):v$(VERSION)
CONNECTOR_IMG ?= agentrally-connector:v$(VERSION)
CHART_OCI_REPO ?= oci://ghcr.io/chizhang21/helm
CONTAINER_TOOL ?= docker

SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec
.DEFAULT_GOAL := build

##@ Development

.PHONY: help generate fmt test e2e run lint clean
help: ## List commands.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  %-20s %s\n", $$1, $$2 } /^##@/ { printf "\n%s\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

generate: controller-gen ## Generate DeepCopy code, chart CRDs and RBAC.
	GOWORK=off $(CONTROLLER_GEN) object paths="./api/..."
	CONTROLLER_GEN="$(CONTROLLER_GEN)" bash hack/generate-manifests.sh

# Optional local helpers stay callable without expanding the public command list.
fmt:
	go fmt ./...

test: ## Run local Go/Python tests, including API and packaging regressions.
	python3 -m unittest discover -s tests/packaging -p '*_test.py'
	GOWORK=off go test -count=1 -timeout 120s ./...

e2e: kind ## Test API contracts and workload lifecycle in a disposable Kind cluster.
	KIND="$(KIND)" bash tests/e2e/run.sh

run:
	go run ./cmd/main.go

lint: golangci-lint actionlint ## Check Go, shell, workflows, generated files and Helm chart.
	GOWORK=off $(GOLANGCI_LINT) run --build-tags=e2e
	GOWORK=off go vet -tags=e2e ./...
	python3 hack/verify-generated.py
	shellcheck --severity=warning hack/*.sh tests/e2e/*.sh
	$(ACTIONLINT)
	helm lint chart
	helm lint chart -f examples/values/orchestrator.yaml

clean: ## Remove build outputs, staging directories and Python bytecode.
	rm -rf dist .cache/chart.* .cache/generated.*
	find hack tests -type d -name __pycache__ -prune -exec rm -rf {} +

##@ Build and packaging

.PHONY: build image-build image-push chart-package chart-push
build: ## Compile the Operator and Connector binaries.
	mkdir -p dist
	GOWORK=off go build -trimpath -o dist/operator ./cmd
	GOWORK=off go build -trimpath -o dist/connector ./cmd/connector

image-build: ## Build the Operator and Connector images.
	$(CONTAINER_TOOL) build -f Containerfile --target operator -t "$(IMG)" .
	$(CONTAINER_TOOL) build -f Containerfile --target connector -t "$(CONNECTOR_IMG)" .

image-push: ## Push the Operator and Connector images.
	$(CONTAINER_TOOL) push "$(IMG)"
	$(CONTAINER_TOOL) push "$(CONNECTOR_IMG)"

chart-package: ## Package a versioned chart with the selected image defaults.
	python3 hack/package_chart.py --version "$(VERSION)" --image "$(IMG)" --connector-image "$(CONNECTOR_IMG)" --output "$(CURDIR)/dist"

chart-push: chart-package ## Package and push the chart to CHART_OCI_REPO.
	helm push "$(CURDIR)/dist/agentrally-operator-$(VERSION).tgz" "$(CHART_OCI_REPO)"

# Internal tools

include mk/tools.mk
