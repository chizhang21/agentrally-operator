# Security

## Reporting a vulnerability

Use the repository host's private vulnerability reporting feature when available, or a
private channel provided by the maintainers.
If no private route is listed, open an issue requesting a security contact without
disclosing the vulnerability, exploit details or credentials.
Do not post sensitive reports in public issues.

Include the revision/image digest, Kubernetes and agent-sandbox versions, deployment
settings, a minimal reproduction, expected/actual behavior and impact.

Use synthetic credentials and redact cluster addresses and application data.

## Support and fixes

This project does not currently promise a maintained release series or response SLA.
Reports are evaluated against the current source; older installations may need an
upgrade.

Describe security fixes in release notes.
Do not publish exploit details before a fix is available.

AgentRally validates references and configuration.
Cluster RBAC, namespace trust, agent-sandbox controllers, container images, CNI and CSI
remain separate security boundaries.

`ClusterCapabilities` does not attest traffic enforcement.
See [Architecture](docs/architecture.md) and
[capability evidence](docs/cluster-capabilities.md).
