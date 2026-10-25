# CI and releases

[CI](../.github/workflows/ci.yml) runs on pull requests and pushes to `main`.
[Release](../.github/workflows/release.yml) runs on `v*` tag pushes.
Both orchestrate reusable workflows for distinct responsibilities.

## Workflows

| Workflow | Responsibility | Manual run |
| --- | --- | --- |
| [checks.yml](../.github/workflows/checks.yml) | Actions syntax, Go lint, source/generated-file checks, unit tests, example validation, vet, binary build, Helm checks and API integration | Yes |
| [security.yml](../.github/workflows/security.yml) | Gitleaks source/history scanning and reachable Go vulnerability scanning | Yes |
| [packages.yml](../.github/workflows/packages.yml) | Validate package identity, build the image and package the Chart as Actions artifacts | Yes |
| [e2e.yml](../.github/workflows/e2e.yml) | Real Core, Extensions and AgentRally lifecycle in disposable kind | Yes |
| [publish.yml](../.github/workflows/publish.yml) | Upload built image and Chart artifacts to GHCR as soon as packaging succeeds | Called by Release |

API integration uses isolated Kubernetes 1.34.1 API servers and checksum-pinned binaries.
It checks Helm CRD lifecycle, example admission and Connector election/readiness.
The E2E workflow is named **AgentRally Operator E2E**.
Its default profile uses kindnet and local-path storage.
See [local E2E profiles](development.md#real-lifecycle-release-gate) for optional Cilium,
snapshot, OpenClaw and Kata checks.

Dependabot configuration is removed.
Security scanning remains part of CI and the release workflow.

## Versions and destinations

| Trigger | Package version | Image tag | GHCR publication |
| --- | --- | --- | --- |
| PR, including forks | `0.0.0-pr.<number>.g<12-character checked SHA>` | `v` + package version | No |
| Branch push or manual branch packaging | `0.0.0-dev.g<12-character checked SHA>` | `v` + package version | No |
| Version-tag push | Git tag without its leading `v` | Exact Git tag | Image and Chart as soon as packaging succeeds |

Use **`vMAJOR.MINOR.PATCH`** for releases, for example `v0.1.0` or `v1.2.3`.
Prereleases such as `v0.1.0-rc.1` are supported.
Versions cannot contain leading-zero numeric components, `+metadata`, or exceed 63
characters after removing `v`.
Invalid tags fail package identity validation.
A branch named `v1.2.3` remains a development build.

The repository name is normalized to lowercase for the image address.
The Chart is published beneath the owner's `helm` namespace.
For this repository, tag `v0.1.0` produces:

- Image: `ghcr.io/chizhang21/agentrally-operator:v0.1.0`
- Chart: `oci://ghcr.io/chizhang21/helm/arally-operator`, version `0.1.0`
- Chart `appVersion`: `0.1.0`

The image contains one `/operator` binary, including the optional Connector.
It targets `linux/amd64`.
No `latest` tag is published.

Source Chart versions stay as placeholders.
Packaging stamps a temporary copy with the tag version and matching image reference.
No source version bump is required.

## Release sequence

Checks, security scanning and package builds run independently.
Image and Chart builds have no registry credentials or package-write permission.

Publication starts as soon as packaging succeeds.
It downloads the built artifacts, loads and pushes the image, then uploads the existing
Chart archive with `helm push`.
It does not rebuild either artifact.

Checks and security scanning run independently of publication.
E2E waits for checks, security scanning and packaging, then tests the same image and
Chart archives in a temporary kind cluster.
The cluster is deleted afterward.

A validation failure still fails the release workflow.
Packages already uploaded to GHCR remain available even if validation later fails.
Publication does not wait for validation or remove those packages automatically.

Only the publication job requests `packages: write` and logs in to GHCR.
It uses `GITHUB_TOKEN`; no registry password Secret is needed.
For an existing GHCR package, the repository must have Actions write access to it.
See [GHCR authentication and package permissions](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry#authenticating-in-a-github-actions-workflow).

Image archives are retained as Actions artifacts for seven days; Charts for 30 days.
PR and branch packages reference images that have not been published to GHCR.
Download/load their image artifact or supply an accessible image override before
installing those Charts.

Failures retain E2E diagnostics for seven days.
Release does not deploy to existing clusters or create GitHub Releases.

## Trigger a release

Commit and push the intended changes before creating a tag.
The tag must point at a commit containing these workflows.
For example:

```sh
git tag -a v0.1.0 -m "Release v0.1.0"
git push origin v0.1.0
```

For a release candidate, use a new tag such as `v0.1.0-rc.1`.
Keep published version tags attached to their original commits.

After first publication, set both GHCR packages to **Public** for anonymous downloads.
A public repository does not make its packages public automatically.
See [package visibility](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility).
Install a published Chart using the [installation guide](installation.md#install-agentrally).

For local builds and manual uploads, see [Development](development.md).

## Optional agent image

The separate [Build Codex Mobile image](../.github/workflows/codex-mobile.yml) workflow
runs only on manual request.
It builds an optional agent image and publishes to the user's selected registry.
See [inputs, credentials and fork setup](../examples/codex-mobile/README.md).
