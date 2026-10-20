# Contributing

Read [Architecture](docs/architecture.md) for ownership boundaries and
[Development](docs/development.md) for tool versions and build commands.

Before opening a pull request:

1. Keep the change focused and describe the affected user behavior.
2. Add a regression test for a behavior change; avoid tests that only repeat constants.
3. Run `make check lint`. If API types or RBAC markers changed, run the single
   generation command, `make generate`, and include its outputs.
4. For CRD lifecycle or Manager/Connector changes, run the documented integration
   tests with `KUBEBUILDER_ASSETS`. CI requires these tests.

Explain the trigger, expected result and validation in the pull request. Separate
fake-client, API-server and live-cluster evidence. Changes to allocation, cleanup
and security must preserve UID ownership checks and upstream Core/Extensions
responsibility for Sandbox and Pod lifecycle.

Never include tokens, kubeconfigs, private values or application output containing
credentials. Follow [Security](SECURITY.md) for vulnerability reports. Contributions
are distributed under the repository's [Apache-2.0 license](LICENSE).

Keep discussions respectful, specific and focused on the code. Maintainers may
remove abusive content and restrict repeated disruptive participation. To report
conduct concerns, request a private contact from the repository maintainers.
