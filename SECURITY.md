# Security Policy

## Supported versions

| Version | Supported |
|---------|-----------|
| 0.1.x   | Yes       |

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Instead, report them privately via [GitHub Security Advisories](https://github.com/schmitech-fr/kubecron/security/advisories/new).

Include:
- A description of the vulnerability
- Steps to reproduce
- Potential impact
- A suggested fix if you have one

You can expect an acknowledgement within 72 hours and a fix or mitigation plan within 14 days.

## Security model

KubeCron runs with a **dedicated Service Account** and a minimal RBAC ClusterRole:

- `get`, `list`, `watch` on `cronjobs`, `jobs`, `pods`
- `patch` on `cronjobs` (suspend/resume)
- `create` on `jobs` (manual trigger)

**No access to Secrets, ConfigMaps, Deployments, or any other resource.**

Additional security properties:

- **Distroless runtime** — `gcr.io/distroless/static:nonroot`; no shell, no package manager, non-root user
- **No CGO** — pure Go binary, no native libraries, no dynamic linking
- **OIDC authentication** — when `OIDC_ENABLED=true`, all routes require a valid session. The session key is stored in a Kubernetes Secret and never logged
- **Structured logging** — `log/slog` JSON output; kubeconfig data, OIDC secrets, and session keys are never logged
- **SQLite WAL** — database file is not exposed over the network; it lives on a PVC accessible only to the pod
- **Docker images signed** with cosign (keyless, OIDC-based); SBOM generated per release with anchore/sbom-action
