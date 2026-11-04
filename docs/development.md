# Development

Use `AgentRally` in prose and `agentrally` in code, package and resource names.
The Kubernetes API group remains `arally.io`.

Run commands from the repository root.

Installation instructions are in [Installation](installation.md).
Build and publication jobs are defined in [GitHub Workflows](../.github/workflows).

## Local development

Use the Go version in [go.mod](../go.mod), Helm 3.17 or later and Python 3.
Lint also requires ShellCheck on your `PATH`.

| Command | Result |
| --- | --- |
| `make test` | Run local Go/Python tests, including API schemas and Chart rendering/packaging |
| `make lint` | Check Go, shell scripts, workflows, generated files and Helm chart |
| `make build` (or `make`) | Compile `dist/operator` and `dist/connector` locally without Docker |
| `make image-build` | Build both images from the `operator` and `connector` Containerfile targets |
| `make image-push` | Upload both built images |
| `make chart-package` | Create a local Chart `.tgz` archive |
| `make chart-push` | Package and upload the Chart to an OCI registry |
| `make e2e` | Test API contracts, Connector and workload lifecycle in a disposable Kind cluster |
| `make generate` | Update API DeepCopy code, `chart/templates/crds/` and `chart/templates/roles.yaml` after changing types or markers |
| `make help` | Available build targets |
| `make clean` | Remove build outputs, temporary staging directories and Python bytecode |

Build tools have overridable versions in [mk/tools.mk](../mk/tools.mk).
Go dependencies are recorded in `go.mod` and `go.sum`.

`make lint` includes `go vet`, golangci-lint, ShellCheck and actionlint.
Go analysis includes the E2E build tag without running its tests.
Actionlint is downloaded at the version pinned in `mk/tools.mk`.
Use `make fmt` to format Go code.

`build`, `test` and `run` do not regenerate or format files.
Use `make fmt` to format code, and `make generate` after changing API types or RBAC
markers.

Packaging uses the committed generated files; `make lint` checks for drift by
generating in an isolated directory without overwriting local source files.
These generated sources stay in Git and are needed regardless of the installation
method.
Building an image or packaging a chart does not regenerate them.

## Package versions

The source chart uses `0.0.0` placeholders.
Packaging stamps temporary copies, leaving source files unchanged.
Dependency and API versions are independent.

Make uses these defaults:

| Variable | Default |
| --- | --- |
| `VERSION` | `0.0.0-dev` |
| `IMAGE_TAG_BASE` | `agentrally-operator` |
| `IMG` | `$(IMAGE_TAG_BASE):v$(VERSION)` |
| `CONNECTOR_IMG` | `agentrally-connector:v$(VERSION)` |
| `CHART_OCI_REPO` | `oci://ghcr.io/chizhang21/helm` |

Pass overrides to each command, export them in your shell, or run related targets
in one Make invocation.
Build settings are not saved between commands.
Credentials stay in Docker and Helm login stores.

Use `VERSION=x.y.z` without `v` for releases.
Prereleases such as `x.y.z-rc.1` are supported.
Versions are limited to 63 characters and cannot contain `+metadata` so Kubernetes
version labels remain valid.
Image tags add `v`; Chart versions do not.
`IMG` and `CONNECTOR_IMG` override each complete image reference, including a digest.

Build and package for Linux amd64, matching the Chart's default node selector:

```sh
export DOCKER_DEFAULT_PLATFORM=linux/amd64
make image-build image-push chart-package \
  VERSION=0.0.0-dev IMAGE_TAG_BASE=YOUR_REGISTRY/agentrally-operator \
  CONNECTOR_IMG=YOUR_REGISTRY/agentrally-connector:v0.0.0-dev
```

Log in to your registry before pushing.
Docker retains the image locally.
The Chart archive is `dist/agentrally-operator-<VERSION>.tgz` and includes the selected
image defaults.

`make clean` removes build outputs, temporary Chart/generation staging and Python bytecode.
Downloaded tools and local test logs are retained.

Repeated dev builds reuse the image tag and overwrite the archive.
Versions with the `-dev` prerelease set both image pull policies to `Always`.
To install or update them, follow [local installation](installation.md#local-development-and-testing).

## Upload images and Chart

The Operator, Connector and Chart use separate OCI repositories.
Log in, then build and upload with explicit settings:

```sh
docker login ghcr.io --username YOUR_GITHUB_USER
helm registry login ghcr.io --username YOUR_GITHUB_USER
make image-build image-push chart-push \
  VERSION=x.y.z \
  IMAGE_TAG_BASE=ghcr.io/YOUR_GITHUB_USER/agentrally-operator \
  CONNECTOR_IMG=ghcr.io/YOUR_GITHUB_USER/agentrally-connector:vx.y.z \
  CHART_OCI_REPO=oci://ghcr.io/YOUR_GITHUB_USER/helm
```

`chart-push` packages the archive before publishing it with Helm's native OCI format.
`CHART_OCI_REPO` is the parent path, without a Chart name or tag.
Helm adds `agentrally-operator:<VERSION>`.
For CI publication, push a tag such as `vx.y.z`.
The release workflow uses that version for both images and the Chart.
After checks, E2E and OCI publication succeed, it creates a GitHub Release with the tag as its title and no release notes.
GitHub selects the latest stable release automatically; prereleases do not replace it.

The quick start also uses `ghcr.io/chizhang21/agentrally-demo-agent:v0.0.3`.
Its source and independent image workflow live in
[agentrally-examples](https://github.com/chizhang21/agentrally-examples/tree/main/operator/http-api-agent).
Tag that repository `v0.0.3` to publish the matching image and source before releasing
the Operator `v0.0.3` tag.
Set the demo GHCR package to Public so users can pull it without registry credentials.

## Tests

Run `make lint test` before submitting changes.
`make test` runs local Go and Python tests without starting a Kubernetes API server or cluster.
It includes Chart rendering and packaging assertions, which require Helm.
Generated-file consistency belongs to `make lint`.

| Directory | Scope |
| --- | --- |
| `tests/packaging` | Package identity, image/version preservation, credential exclusion and Connector wiring |
| `tests/api` | CRD schemas, CEL rules, field preservation and example references |
| `tests/e2e` | Helm upgrades/uninstall, restricted Connector management and workload lifecycle in Kind |
| `cmd/connector/*_e2e_test.go` | Startup, Connector failure isolation and leader handoff against the same Kind API |

Controller unit tests use fake clients; API admission, scheduling, GC and upstream
controllers need the verification layers below.
For concurrency changes, also run `go test -race ./internal/...`.

### Kind E2E

Run `make e2e` with Docker 28+, kubectl, Helm, Go and curl.
The [driver](../tests/e2e/run.sh) pins Kind, Kubernetes and upstream controller versions.
It builds the production Containerfile, creates a disposable cluster and installs
Core, Extensions and AgentRally through the normal chart.
It never uses the caller's current kubeconfig.

| Setting | Purpose |
| --- | --- |
| `E2E_TEST` | Select Go test names; default runs `cmd/...` and `tests/e2e` suites |
| `E2E_IMAGE=repository:tag` | Use an already built local Operator image; no publication |
| `E2E_CONNECTOR_IMAGE=repository:tag` | Use an already built local Connector image; no publication |
| `E2E_CHART=/path/to/chart.tgz` | Install a packaged Chart |
| `E2E_CNI=cilium` | Install Cilium and execute ingress, ConnectorOnly and native egress traffic tests |
| `E2E_SNAPSHOTS=true` | Install CSI snapshot fixtures and test workspace initialization |
| `E2E_LOG_DIR` | Select the local log directory |

```sh
make e2e
E2E_CNI=cilium make e2e
E2E_SNAPSHOTS=true make e2e
```

The suite covers publication/revision pinning, dependency rotation, Group policies,
allocation, prewarming, suspend/resume, replacement, expiry and ownership cleanup.
Generic native-template tasks verify completion policies.
Workspace checks distinguish execution-private volumes from externally managed PVCs.
Connector management uses its dedicated rendered RBAC rather than administrator permissions.
It checks scoped metadata reads, write-result retries and guarded Secret/ConfigMap deletion.
Startup tests verify leader handoff against the Kind API.
Deployment tests exercise backend connectivity, outage isolation, Pod replacement and cleanup on disable.
The Helm lifecycle test restores the selected Chart and values after checking CRD retention.
Resource aggregation rules run in a pinned Prometheus engine with small attribution fixtures.
They verify shared-PVC deduplication and unknown usage, without requiring a cluster monitoring stack.
See [tests/e2e](../tests/e2e) for test cases and their supporting scripts.
Latency samples are smoke measurements, rather than production performance guarantees.

Cilium tests use new connections with repeated allow/deny checks.
The outbound test verifies UDP/TCP DNS, namespace-and-Pod selectors, CIDR exceptions,
ports, additive policies and removal without restarting the Agent.
Denied targets have an unrestricted healthy control client.
A metadata-address fixture runs inside a temporary Docker container; its address and
Kind-only route are verified before requests.
No real cloud metadata or host routes are used.

Snapshot fixtures are pinned in [snapshots.sh](../tests/e2e/snapshots.sh).
The Operator gains no fixture privileges or host socket access.
An explicitly requested infrastructure profile must pass; missing infrastructure is a failure.

Every initialized Kind run has a new directory under `.cache/e2e/`.
Go test output appears in the terminal and is saved to `lifecycle.log`.
A run must pass at least one scenario beyond the target guard.
Empty selections and skipped-only runs fail.
Test and cleanup failures return a nonzero exit code.
Failed runs also save controller logs and resource dumps locally.
CI results are available in the job logs.

Go cleanup and driver EXIT handle ordinary completion and catchable interruption;
SIGKILL or runner loss can prevent cleanup.

The [E2E workflow](../.github/workflows/e2e.yml) supports manual dispatch.
Tag releases test the same image archives and Chart artifact selected for publication.
OCI publication follows packaging without waiting for E2E.
GitHub Release publication waits for E2E and OCI publication to succeed.
PR/main, tag releases and manual E2E workflow dispatches run Kind tests.

### Optional Kata runtime profile

For a disposable Kind cluster, provide a host-compatible node configuration and runtime installer:

```sh
E2E_KIND_CONFIG=/path/to/kata-kind.yaml \
E2E_RUNTIME_MANIFEST=/path/to/kata-runtime.yaml \
E2E_RUNTIME_CLASS=kata-qemu \
E2E_TEST='^TestKataRuntimeLifecycle$' make e2e
```

The driver applies that installer only inside its new cluster.
The selected RuntimeClass must exist.
The test exercises actual execution, workspace suspension/replacement and a controlled
cleanup hold exceeding five minutes.
It removes only its own finalizer on the same Pod UID.

Testing an existing cluster requires explicit matching context and API server values:

```sh
AGENTRALLY_E2E_KUBECONFIG=/path/to/private-kubeconfig \
E2E_KATA_CONTEXT=my-kata-cluster \
E2E_KATA_API_SERVER=https://kubernetes.example.com:6443 \
E2E_RUNTIME_CLASS=kata-qemu \
E2E_KATA_STORAGE_CLASS=disposable-workspaces \
go test -tags=e2e ./tests/e2e -run '^TestKataRuntimeLifecycle$' -count=1 -v -timeout=18m
```

This creates a uniquely named temporary Group and PVCs.
The StorageClass must use Delete reclaim policy.
It neither installs runtimes nor deletes the existing cluster.
After a failure, inspect the reported Group for resources still awaiting runtime cleanup.
Other E2E tests accept only the driver's disposable Kind context.

### Workspace lifecycle checks

```sh
E2E_TEST='^(TestOperatorLifecycle|TestRetainedWorkspaceUpgrade)$' make e2e
```

These checks exercise creation, suspension/resumption and deletion with ordinary
volumeClaimTemplates and external PVCs.
They verify retained data across suspension, a fresh private PVC after Instance
recreation, and external PVC retention after Instance deletion.

Results establish the tested versions and paths only.
Kind cannot substitute for a specific Kata, production CSI or CNI acceptance run.
Passive capability discovery remains separate from execution evidence.
