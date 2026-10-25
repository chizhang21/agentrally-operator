# Codex Mobile image

Builds [friuns2/codex-mobile](https://github.com/friuns2/codex-mobile) from its
published `@brutalstrikedevs/codexapp` npm package, together with Codex CLI.

This is a third-party image recipe.
It is independent of the Operator image.

## Build in GitHub Actions

1. Fork this repository and enable Actions in your fork.
   The workflow must exist on its default branch.
   Running a workflow requires repository write access; users run it in their own fork,
   not in the upstream AgentRally repository.
2. For a registry other than GHCR, add repository Actions Secrets named
   `REGISTRY_USERNAME` and `REGISTRY_PASSWORD`.
   Use a registry access token as the password if supported.
   Enter credentials only in Secrets, not workflow inputs.
   GHCR defaults to the fork's `GITHUB_TOKEN`; no additional Secrets are needed when
   publishing to the fork owner's namespace.
   To override that login, set both Secrets.
3. Open **Actions → Build Codex Mobile image → Run workflow** and fill in:

| Input | Default | Meaning |
| --- | --- | --- |
| `registry` | `ghcr.io` | Registry hostname, optionally with a port; no scheme or path |
| `image_repository` | `<fork-owner>/codex-mobile` | Lowercase repository path, such as `your-name/codex-mobile` |
| `image_tag` | `latest` | Tag to publish |
| `codexapp_version` | `latest` | Exact Codex Mobile npm version or `latest` |
| `codex_version` | `latest` | Exact Codex CLI npm version or `latest` |

The workflow builds `linux/amd64`, checks Codex CLI and web login, then pushes the
image.
The resulting address appears in the run summary.
A failed check prevents publication.
Only manual runs build this image.

For reproducible builds, supply exact package versions and choose a distinct image tag.
With `latest`, each workflow run installs the current npm packages without reusing an
earlier installation layer.

After the first GHCR publication, set the package to **Public** if anonymous pulls are
wanted.
A public fork does not make its image public automatically.
See
[workflow access](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manually-run-a-workflow)
and
[package visibility](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility).

## Use the image

Set the resulting address as `AgentDefinition.spec.container.image` with
`AgentDefinition.spec.template: coding`.
The server listens on port 5900.

Configure its password and arguments through `AgentDefinition.spec.container`, using
environment variables or arguments supported by the image.
Declare port 5900 in `container.ports` for interaction endpoint discovery.
Build-time registry credentials and runtime agent credentials are separate.

AgentInstances reference that definition and configure their workspace, lifecycle and
optional interaction endpoint.
After changing a published definition, change the instance's `spec.restartToken` to
adopt its latest revision.

To select a project when starting the server, pass its directory as a positional
argument, such as
`codexapp /workspace/projects --port 5900 --no-open --no-tunnel --no-login`.
In Codex Mobile 0.1.91, `--open-project` registers the project and exits instead of
running the server.

The image runs as UID/GID 1000.
A mounted workspace must be writable by that user.

`HOME=/workspace` and `CODEX_HOME=/workspace/.codex`; configure a private workspace PVC
to retain projects and login state across suspend/resume.
With `workspace.volumeClaimTemplate`, replacing the Sandbox creates a new PVC.
To keep projects and login state across replacement and upgrades, bind an external PVC
with `workspace.existingClaimRef` and `exclusive: true` on the instance.
See the [retained workspace pattern](../openclaw/README.md#upgrade-and-retain-state).

Without an explicit password, the application generates one and stores it beneath
`CODEX_HOME`.
The image disables automatic browser opening, tunneling and startup login.

Model authentication is configured separately.
Codex defaults to `workspace-write` with approval policy `on-request`.

The integrated terminal uses Bash (`SHELL=/bin/bash`).

It includes Node.js, Codex CLI, Git and native build tools; it does not install a
browser.

Installed npm packages retain their license files in place.
Codex CLI's missing LICENSE and NOTICE are fetched from the matching upstream release
tag and stored under `/licenses/codex`; a missing notice fails the build.
Codex Mobile retains its MIT license, and Codex CLI its Apache-2.0 license.

## Build locally

From the repository root:

```sh
docker build -t codex-mobile:dev examples/codex-mobile
```

Pass `--build-arg CODEXAPP_VERSION=<version>` and `--build-arg CODEX_VERSION=<version>`
to select exact npm versions.

The workflow uses [input validation](validate-build-inputs.py), a
[terminal check](check-terminal.cjs) and a [web login check](check-web.cjs).
The Node checks run inside the built image; they can be passed to `node` through
standard input for local diagnosis.
See the [workflow](../../.github/workflows/codex-mobile.yml) for the container setup and
environment.
