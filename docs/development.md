# Development

Use `AgentRally` in prose and `agentrally` in code, package and resource names.
The Kubernetes API group remains `arally.io`.

Run commands from the repository root.

Installation instructions are in [Installation](installation.md).
Build and publication jobs are defined in [GitHub Workflows](../.github/workflows).

## Local development

Use the Go version in [go.mod](../go.mod), Helm 3.17 or later and Python 3.

| Command | Result |
| --- | --- |
| `make check lint` | Check formatting, documentation links, generated files, Go tests/vet/lint and packaging |
| `make build` (or `make`) | Compile `dist/operator`, including the optional Connector, locally without Docker |
| `make configure` | Save local image and Chart repository settings |
| `make docker-push` | Upload the built Operator image |
| `make chart-package` | Create a local Chart `.tgz` archive |
| `make chart-push` | Package and upload the Chart to an OCI registry |
| `make e2e` | Build and test the real upstream lifecycle in a disposable kind cluster |
| `make generate` | Update API DeepCopy code, `chart/templates/crds/` and `chart/templates/roles.yaml` after changing types or markers |
| `make help` | Available build targets |
| `make clean` | Remove `dist/` and `.cache/` |

Build tools have overridable versions in [mk/tools.mk](../mk/tools.mk).
Go dependencies are recorded in `go.mod` and `go.sum`.

`make check` runs `go vet`; `make lint` supplies the remaining static checks.

For workflow changes, install `actionlint` and ShellCheck on your `PATH`, then run:

```sh
shellcheck --version
actionlint
```

`actionlint` skips shell-script analysis when ShellCheck is absent.
CI requires both tools.

`build`, `test` and `run` do not regenerate or format files.
Use `make fmt` to format code, and `make generate` after changing API types or RBAC
markers.

Packaging uses the committed generated files; `make check` checks for drift by
generating in an isolated directory without overwriting local source files.
These generated sources stay in Git and are needed regardless of the installation
method.
Building an image or packaging a chart does not regenerate them.

## Package versions

The source chart uses `0.0.0` placeholders.
Packaging stamps temporary copies, leaving source files unchanged.
Dependency and API versions are independent.

The selected version, image reference and Chart OCI destination are saved in
`.cache/build.json` and reused by later build/upload commands.
Explicit `VERSION`, `IMG`, `IMAGE_TAG_BASE` and `CHART_OCI_REPO` override the record.
`make configure` updates these settings without building or uploading; changing one
repository preserves the other.

Credentials stay in Docker and Helm login stores.

The default local version is `0.0.0-dev`; `make version` selects it again while
preserving repository settings.

To select a release, pass `VERSION=x.y.z` without `v`.
Prereleases such as `x.y.z-rc.1` are supported; versions are limited to 63 characters
and cannot contain `+metadata`, so Kubernetes version labels are valid.

Image tags add `v`; Chart versions do not.
`IMG` can supply a complete image reference, including a digest.

Build and package locally:

```sh
make configure IMAGE_TAG_BASE=YOUR_REGISTRY/agentrally-operator
make docker-build
make docker-push
make chart-package
```

Docker stores the built image locally.
The Chart archive is `dist/agentrally-operator-<VERSION>.tgz` and includes the selected
image defaults.

The saved record identifies outputs; it does not prove a build or push succeeded.
`make clean` removes the record, build outputs and downloaded tools.

Repeated dev builds reuse the image tag and overwrite the archive.
Dev packages set `image.pullPolicy: Always`; after updating an installed build,
[restart its Deployment](installation.md#updating-a-dev-build) to pull the image.

To install an archive, first
[install Core and Extensions](installation.md#install-upstream-agent-sandbox), then
follow [local installation](installation.md#local-development-and-testing).

## Upload image and Chart

Image and Chart repositories are independent.
Set their addresses, log in, then build and upload:

```sh
make configure \
  IMAGE_TAG_BASE=ghcr.io/chizhang21/agentrally-operator \
  CHART_OCI_REPO=oci://ghcr.io/chizhang21/helm
docker login ghcr.io --username chizhang21
helm registry login ghcr.io --username chizhang21
make docker-build
make docker-push
make chart-push
```

`chart-push` packages the archive before publishing it with Helm's native OCI format.
`CHART_OCI_REPO` is the parent path, without a Chart name or tag; Helm adds
`agentrally-operator:<VERSION>`.
The default parent is `oci://ghcr.io/chizhang21/helm`, so a dev Chart is published as
`ghcr.io/chizhang21/helm/agentrally-operator:0.0.0-dev`.
Use `make configure CHART_OCI_REPO=oci://REGISTRY/NAMESPACE/helm` to change it.

## Tests

Run `make check lint` before submitting changes.
Controller unit tests use fake clients; API admission, scheduling, GC and upstream
controllers need the verification layers below.
For concurrency changes, also run `go test -race ./internal/...`.

### API-server integration

Install the [envtest binaries](https://github.com/kubernetes-sigs/controller-tools/releases)
selected by [CI](../.github/workflows/checks.yml) and set `KUBEBUILDER_ASSETS`.

```sh
go test -tags=integration ./cmd ./tests/packaging -count=1 -timeout=180s
```

This tests Helm installation, CRD upgrade/adoption/uninstall retention, example admission,
Group policy convergence and Connector leader handoff/readiness.
Missing binaries fail explicitly.
These tests do not exercise a scheduler, CNI, CSI or Kubernetes garbage collector.

### Real lifecycle release gate

Run `make e2e` with Docker 28+, kubectl, Helm, Go and curl.
The [driver](../hack/e2e.sh) pins Kind, Kubernetes and upstream controller versions.
It builds the production Containerfile, creates a disposable cluster and installs
Core, Extensions and AgentRally through the normal chart.
It never uses the caller's current kubeconfig.

| Setting | Purpose |
| --- | --- |
| `E2E_TEST` | Select Go test names; default is the full suite |
| `E2E_IMAGE=repository:tag` | Use an already built local image; no publication |
| `E2E_CHART=/path/to/chart.tgz` | Install a packaged Chart |
| `E2E_CNI=cilium` | Install Cilium and execute ingress, ConnectorOnly and native egress traffic tests |
| `E2E_SNAPSHOTS=true` | Install CSI snapshot fixtures and test workspace initialization |
| `E2E_LOG_DIR` | Select the evidence output root |

```sh
make e2e
E2E_CNI=cilium make e2e
E2E_SNAPSHOTS=true make e2e
```

The suite covers publication/revision pinning, dependency rotation, Group policies,
allocation, prewarming, suspend/resume, replacement, expiry and ownership cleanup.
Generic native-template tasks verify completion policies.
Workspace checks distinguish execution-private, external and platform-persistent storage.
See [tests/e2e](../tests/e2e) for individual scenarios.
Latency samples are smoke measurements, rather than production performance guarantees.

Cilium tests use new connections with repeated allow/deny checks.
The outbound test verifies UDP/TCP DNS, namespace-and-Pod selectors, CIDR exceptions,
ports, additive policies and removal without restarting the Agent.
Denied targets have an unrestricted healthy control client.
A metadata-address fixture runs inside a temporary Docker container; its address and
Kind-only route are verified before requests.
No real cloud metadata or host routes are used.

Snapshot fixtures are pinned in [e2e-snapshots.sh](../hack/e2e-snapshots.sh).
The Operator gains no fixture privileges or host socket access.
An explicitly requested infrastructure profile must pass; missing infrastructure is a failure.

Every initialized run has a new directory under `.cache/e2e/`.
`report.json` and `summary.md` distinguish pass, failure, skip and not-run.
`environment.json` contains whitelisted component/version observations.
The report distinguishes an image built from the checkout from an externally supplied image.
The ingress and native-egress profiles have independent results.
Empty selections and incomplete selected subcases cannot pass.

Raw application output and resource dumps remain local.
CI uploads only the report, summary and safe environment record.
Go cleanup and driver EXIT handle ordinary completion and catchable interruption;
SIGKILL or runner loss can prevent cleanup.

The [E2E workflow](../.github/workflows/e2e.yml) supports manual dispatch.
Tag releases test the same image and Chart artifacts selected for publication.
Publication follows packaging without waiting for E2E.
PR/main validation compiles E2E without creating a cluster.

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

### External application fixtures

Basic/full CR references are validated by default.
To add an external examples checkout to schema/reference checks:

```sh
AGENTRALLY_EXAMPLES_DIR=/path/to/agentrally-examples/operator \
go test ./tests/packaging -run 'TestAllManifests|TestManifestReferences' -count=1 -v
```

The optional OpenClaw smoke test uses that project's actual manifests:

```sh
E2E_OPENCLAW=true \
E2E_OPENCLAW_DIR=/path/to/agentrally-examples/operator/openclaw \
E2E_TEST='^TestOpenClawLifecycle$' make e2e
```

It checks application startup and retained state with temporary credentials.
No external channel or model account is configured.
The Operator's normal build and tests require no external checkout.

### Persistent workspace foundation

```sh
E2E_TEST='^TestPersistentWorkspaceFoundation$' make e2e
```

This checks same-Instance storage reuse and retained namespace/revision/PVC identity
through foreground/background parent deletion under the actual Operator ServiceAccount.
The disposable cluster removes intentionally retained resources at the end.
The bootstrap-deletion case also checks interrupted namespace identity pinning.

Results establish the tested versions and paths only.
Kind or envtest cannot substitute for a specific Kata, production CSI or CNI acceptance run.
Passive capability discovery remains separate from execution evidence.
