# CI and image publication

[The workflow](workflows/ci.yml) runs on PR creation, updates and reopening,
and on `v*` tag pushes. It runs lint, source/generated-file checks, unit tests,
Helm chart checks, credential scanning and `govulncheck`, then builds the image.
It also runs CRD lifecycle and Connector election/readiness integration tests
against isolated Kubernetes 1.34.1 API servers using checksum-pinned test binaries.

Images use `ghcr.io/<owner>/<repository>` in lowercase.

| Trigger | Package version | Image tag | Publication |
| --- | --- | --- | --- |
| Same-repository PR | `0.0.0-pr.<number>.g<12-character checked SHA>` | `v` + package version | Push to GHCR |
| Fork PR | Same PR version format | `v` + package version | Build only; no registry login |
| Version tag | Git tag with leading `v` removed | Exact Git tag | Push to GHCR |

A tag such as `v1.2.3` builds version `1.2.3`; no source version update is needed.
CI injects this version and the corresponding image reference into the chart.
It publishes no `latest` tag. The image contains one `/operator` binary, including
the optional Connector, and currently targets `linux/amd64`.

PR and tag builds retain the chart archive as an Actions artifact for 30 days.
The chart contains the CI image defaults; trusted builds publish that image
after checks pass. Fork PR images are built but not published,
so their packages need an accessible image override before installation.
CI uses `GITHUB_TOKEN` with `packages: write`; no registry password Secret is needed.
It does not deploy to existing clusters or create GitHub Releases.

Version tags must also pass the [real lifecycle E2E gate](workflows/e2e.yml)
before image publication. It creates and deletes a temporary kind cluster with
Core, Extensions and Arally. The same workflow supports manual dispatch; PRs
compile its test code but do not run the cluster test. See
[local E2E instructions](../docs/development.md#real-lifecycle-release-gate).

After first publication, set the GHCR package to **Public** for anonymous pulls.
A public repository does not make its package public automatically. See
[GHCR documentation](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry#pushing-container-images).

For manual builds and packaging, see [Development](../docs/development.md).

The separate [Build Codex Mobile image](workflows/codex-mobile.yml) workflow runs
only on manual request. It builds an optional agent image and publishes to the
user's selected registry. See [inputs, credentials and fork setup](../examples/codex-mobile/README.md).
