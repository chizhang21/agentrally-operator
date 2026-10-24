# Development

Run commands from the repository root.

Installation instructions are in [Installation](installation.md); GitHub workflows and
published artifacts are described in [CI and publication](ci.md).

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
make configure IMAGE_TAG_BASE=YOUR_REGISTRY/arally-operator
make docker-build
make docker-push
make chart-package
```

Docker stores the built image locally.
The Chart archive is `dist/arally-operator-<VERSION>.tgz` and includes the selected
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
  IMAGE_TAG_BASE=ghcr.io/chizhang21/arally-operator \
  CHART_OCI_REPO=oci://ghcr.io/chizhang21/helm
docker login ghcr.io --username chizhang21
helm registry login ghcr.io --username chizhang21
make docker-build
make docker-push
make chart-push
```

`chart-push` packages the archive before publishing it with Helm's native OCI format.
`CHART_OCI_REPO` is the parent path, without a Chart name or tag; Helm adds
`arally-operator:<VERSION>`.
The default parent is `oci://ghcr.io/chizhang21/helm`, so a dev Chart is published as
`ghcr.io/chizhang21/helm/arally-operator:0.0.0-dev`.
Use `make configure CHART_OCI_REPO=oci://REGISTRY/NAMESPACE/helm` to change it.

## Tests

The Helm lifecycle integration test uses an isolated API server and etcd.
With [envtest binaries](https://github.com/kubernetes-sigs/controller-tools/releases)
available in `KUBEBUILDER_ASSETS`, run:

```sh
go test -tags=integration ./cmd ./tests/packaging -count=1 -timeout=180s
```

It checks first installation and upgrades of all four Arally CRDs, retained
CRDs/resources on uninstall, adoption of existing CRDs, default report creation, and
Group LimitRange convergence after native API defaulting and policy edits.

The leader-election test verifies that only the elected Connector polls, backend failure
changes leader readiness, and a standby takes over on shutdown.

CI runs both; selecting integration tests without the required binaries fails
explicitly.

Controller unit tests use fake clients.
Scheduling, admission, garbage collection, CSI, CNI and upstream controller behavior
need cluster tests.

Run `go test -race ./internal/...` for concurrency changes.
Packaging tests live in `tests/packaging`; see [packaging sources](../chart/README.md).

### Real lifecycle release gate

Run `make e2e` with Docker 28 or later, kubectl, Helm, Go and curl available.
Make selects the pinned kind version; the driver builds the production Dockerfile,
creates an isolated Kubernetes 1.34.0 cluster, and installs agent-sandbox
Core/Extensions v1.0.5 and the local chart.

To test an already built local release candidate, set `E2E_IMAGE=repository:tag`.
No image is pushed.

The test checks the canonical capability report, native RuntimeClass scheduling and
overhead, administrator mappings, a real runc execution and direct audit renewal after
manager replacement.
An absent snapshot class API must not prevent startup.

It also checks admission-injected LimitRange defaults, default zero spares, Group
summaries and metrics, and manager replacement while upstream allocation is stopped.

It covers Group allocation pause/unpause, configuration edits without replacement,
Ready/Available separation, suspend/resume, explicit restart, expiry, active deletion,
upstream garbage collection and workspace retention across suspension.

Declarative prewarming, private PVC isolation, verified external pool allocation and
Group deletion bypassing idle retention use real Core and Extensions controllers.

Revision tests cover new-instance selection, explicit historical revisions,
upgrade/rollback, and definition deletion protection.

Dependency tests cover recovery from missing Secrets/ConfigMaps and credential rotation
without implicit restart.

Native task tests exercise the custom template example, successful completion,
failed-task retention and TTL deletion policies.

Execution replacement and cleanup reclaim Sandbox-owned volumes.

The driver always uses its own kubeconfig and deletes its temporary cluster.
Failures retain logs under `.cache/e2e/`; `E2E_LOG_DIR` selects another output
directory.

[Operator lifecycle E2E](../.github/workflows/e2e.yml) supports manual dispatch.
Version-tag CI must pass it before image publication; PRs compile the E2E test and keep
the existing fast checks without creating a cluster.

The default profile uses kindnet and local-path storage.
The snapshot profile preloads its fixture images before starting CSI Pods.
Select optional infrastructure profiles explicitly:

```sh
E2E_CNI=cilium make e2e
E2E_SNAPSHOTS=true make e2e
E2E_CNI=cilium E2E_SNAPSHOTS=true make e2e
```

To test the real OpenClaw example in isolation:

```sh
E2E_OPENCLAW=true E2E_TEST='^TestOpenClawLifecycle$' make e2e
```

This opt-in profile pulls the example's fixed OpenClaw image, uses a temporary Gateway
token, and checks probes/RPC, persisted scheduler state and workspace data through
suspend/resume and explicit configuration replacement.
External channels and model providers are not configured.

`E2E_TEST` selects Go tests; without it the usual lifecycle suite runs.
See the [OpenClaw example](../examples/openclaw/README.md).

Runtime policy tests reject missing RuntimeClasses before allocation, accept a digest-pinned image, and exercise activity-driven suspension and wake-up.
They check retained files, version binding, explicit suspension and maximum lifetime cleanup.
Group capacity tests verify a total managed-spare budget across multiple configurations.
The retained-workspace test also rejects a competing Instance.

Lifecycle tests log nearest-rank P50/P95 for observed cold/warm readiness and resume latency.
These small samples are smoke measurements, not production performance guarantees.
Control-plane observations and application transaction latency remain separate measurements.

### Optional Kata runtime profile

The default RuntimeClass test uses runc.
For Kata, supply a local kind configuration and runtime installation manifest suitable for the host:

```sh
E2E_CNI=kindnet \
E2E_KIND_CONFIG=/path/to/kata-kind.yaml \
E2E_RUNTIME_MANIFEST=/path/to/kata-runtime.yaml \
E2E_RUNTIME_CLASS=kata-qemu \
E2E_TEST='^TestKataRuntimeLifecycle$' make e2e
```

The configuration must expose the host devices and mounts required by the chosen runtime.
The manifest must install the actual handler and its RuntimeClass inside the disposable cluster.
It is never applied to the caller's current kubeconfig.
An absent named RuntimeClass fails the profile.
Without E2E_RUNTIME_CLASS, only the Kata-specific test is skipped.

This profile runs a real workload through the selected Kata handler.
It verifies workspace retention across suspension, explicit replacement, and stalled cleanup with a controlled test finalizer.
It removes only its own test finalizer before checking normal reclamation.
Successful execution validates these lifecycle paths; it is not a general security attestation.

For an existing Kata-enabled cluster, run only the Kata test with an explicitly confirmed context and API server:

```sh
ARALLY_E2E_KUBECONFIG=/path/to/private-kubeconfig \
E2E_KATA_CONTEXT=my-kata-cluster \
E2E_KATA_API_SERVER=https://kubernetes.example.com:6443 \
E2E_RUNTIME_CLASS=kata-qemu \
E2E_KATA_STORAGE_CLASS=disposable-workspaces \
go test -tags=e2e ./tests/e2e -run '^TestKataRuntimeLifecycle$' -count=1 -v -timeout=18m
```

Both target values must exactly match the kubeconfig.
The selected StorageClass must use `reclaimPolicy: Delete` for the temporary workspace.
The test creates a uniquely named `kata-e2e-` Group and removes its resources on success.
It never creates or deletes the existing cluster or installs runtimes.
Other lifecycle tests still require the driver's disposable kind context.

The controlled cleanup hold deliberately takes more than five minutes.
Cleanup removes only the test's finalizer from the same Pod UID, including on failure.
After a failed live run, inspect the reported temporary Group before removing any remaining resources.

The Cilium profile replaces kindnet using the official
[kind installation](https://docs.cilium.io/en/stable/installation/kind/) with Cilium
1.20.3.

Its traffic test starts two Agents, checks application connectivity, applies a Group
ingress-deny policy, then permits the same path.
It requires three consecutive denied and allowed probes and treats test-client exec
failures as infrastructure errors.

The snapshot profile installs checksum-pinned upstream
[external-snapshotter v8.2.0](https://github.com/kubernetes-csi/external-snapshotter/tree/v8.2.0/deploy/kubernetes)
and
[CSI hostpath v1.16.1 fixtures](https://github.com/kubernetes-csi/csi-driver-host-path/tree/v1.16.1/deploy/kubernetes-1.30).

It writes and quiesces an Agent workspace, takes a ready snapshot, restores a second
Agent to an independent volume and reads the original data.

The test driver installs privileged CSI fixtures only inside its disposable cluster; the
Operator gains no Pod creation, exec or CSI socket access.
An explicitly selected profile must pass; missing infrastructure is not silently
skipped.

Results apply to these versions, handlers, volumes and traffic paths.
The default, Cilium and snapshot profiles do not establish Kata teardown.
Runtime-specific lifecycle evidence requires the optional runtime profile.
No profile establishes arbitrary CSI, Gateway routing or cluster-wide NetworkPolicy compatibility.
Passive discovery remains configuration evidence and keeps `enforcementVerified=false`
after these tests.
