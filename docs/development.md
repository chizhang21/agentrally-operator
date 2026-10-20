# Development

Run commands from the repository root. Installation instructions are in
[Installation](installation.md); GitHub workflows and published artifacts are
described in [CI and publication](ci.md).

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

`build`, `test` and `run` do not regenerate or format files. Use `make fmt` to
format code, and `make generate` after changing API types or RBAC markers.
Packaging uses the committed generated files; `make check` checks for drift
by generating in an isolated directory without overwriting local source files.
These generated sources stay in Git and are needed regardless of the installation
method. Building an image or packaging a chart does not regenerate them.

## Package versions

The source chart uses `0.0.0` placeholders. Packaging stamps temporary copies,
leaving source files unchanged. Dependency and API versions are independent.

The selected version, image reference and Chart OCI destination are saved in
`.cache/build.json` and reused by later build/upload commands. Explicit `VERSION`,
`IMG`, `IMAGE_TAG_BASE` and `CHART_OCI_REPO` override the record. `make configure`
updates these settings without building or uploading; changing one repository
preserves the other. Credentials stay in Docker and Helm login stores.

The default local version is `0.0.0-dev`; `make version` selects it again while
preserving repository settings. To select a release, pass `VERSION=x.y.z` without
`v`. Prereleases such as `x.y.z-rc.1` are supported; versions are limited to 63
characters and cannot contain `+metadata`, so Kubernetes version labels are valid.
Image tags add `v`; Chart versions do not. `IMG` can supply a complete image
reference, including a digest.

Build and package locally:

```sh
make configure IMAGE_TAG_BASE=YOUR_REGISTRY/arally-operator
make docker-build
make docker-push
make chart-package
```

Docker stores the built image locally. The Chart archive is
`dist/arally-operator-<VERSION>.tgz` and includes the selected image defaults.
The saved record identifies outputs; it does not prove a build or push succeeded.
`make clean` removes the record, build outputs and downloaded tools.

Repeated dev builds reuse the image tag and overwrite the archive. Dev packages
set `image.pullPolicy: Always`; after updating an installed build,
[restart its Deployment](installation.md#updating-a-dev-build) to pull the image.
To install an archive, first [install Core and Extensions](installation.md#install-upstream-agent-sandbox),
then follow [local installation](installation.md#local-development-and-testing).

## Upload image and Chart

Image and Chart repositories are independent. Set their addresses, log in, then
build and upload:

```sh
make configure \
  IMAGE_TAG_BASE=ghcr.io/chizhang21/arally-operator \
  CHART_OCI_REPO=oci://ghcr.io/chizhang21/helm
docker login ghcr.io --username chizhang21
helm registry login ghcr.io --username chizhang21
make docker-build
make docker-push
make chart-push
```

`chart-push` packages the archive before publishing it with Helm's native OCI
format. `CHART_OCI_REPO` is the parent path, without a Chart name or tag; Helm adds
`arally-operator:<VERSION>`. The default parent is `oci://ghcr.io/chizhang21/helm`,
so a dev Chart is published as
`ghcr.io/chizhang21/helm/arally-operator:0.0.0-dev`.
Use `make configure CHART_OCI_REPO=oci://REGISTRY/NAMESPACE/helm` to change it.

## Tests

The Helm lifecycle integration test uses an isolated API server and etcd. With
[envtest binaries](https://github.com/kubernetes-sigs/controller-tools/releases)
available in `KUBEBUILDER_ASSETS`, run:

```sh
go test -tags=integration ./cmd ./tests/packaging -count=1 -timeout=180s
```

It checks first installation, schema upgrades, retained CRDs/resources on uninstall,
adoption of existing CRDs, and default report creation by the controller.
The leader-election test verifies that only the elected Connector polls, backend failure
changes leader readiness, and a standby takes over on shutdown. CI runs both;
selecting integration tests without the required binaries fails explicitly.

Controller unit tests use fake clients. Scheduling, SCC admission, garbage collection,
CSI, CNI and upstream controller behavior need cluster tests. Run
`go test -race ./internal/...` for concurrency changes. Packaging tests live in
`tests/packaging`; see [packaging sources](../chart/README.md).

### Real lifecycle release gate

Run `make e2e` with Docker 28 or later, kubectl, Helm, Go and curl available. Make
selects the pinned kind version; the driver builds the production Dockerfile, creates an
isolated Kubernetes 1.34.0 cluster, and installs agent-sandbox Core/Extensions
v1.0.5 and the local chart. To test an already built local release candidate,
set `E2E_IMAGE=repository:tag`. No image is pushed.

The test checks default zero prewarming, configuration edits without replacement,
Ready/Available separation, pause/resume, explicit restart, expiry, active deletion,
upstream garbage collection and external PVC/data retention. The driver always
uses its own kubeconfig and deletes its temporary cluster. Failures retain
logs under `.cache/e2e/`; `E2E_LOG_DIR` selects another output directory.

[Operator lifecycle E2E](../.github/workflows/e2e.yml) supports manual dispatch.
Version-tag CI must pass it before image publication; PRs compile the E2E test
and keep the existing fast checks without creating a cluster. This kind test
uses local-path storage and does not establish OpenShift SCC, arbitrary CSI,
Gateway routing or NetworkPolicy enforcement compatibility.
