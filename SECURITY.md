# Security Policy

## Reporting a vulnerability

If you believe you have found a security issue in `terraform-provider-altinity`,
**please do not open a public GitHub issue.** Instead, email a description to:

**security@gorgias.com**

Include:

- A description of the vulnerability and its impact.
- A minimal reproduction (Terraform config, environment, or PoC code).
- The provider version (`bin/terraform-provider-altinity -version` or the
  registry version) and Terraform/OpenTofu version.
- Any logs or stack traces — **with secrets redacted**.

We aim to acknowledge reports within **2 business days** and to provide a fix
or mitigation timeline within **10 business days** for confirmed issues.
Critical issues affecting credential confidentiality or unauthorized cluster
access are triaged immediately.

You will receive credit in the release notes for any accepted report, unless
you prefer to remain anonymous.

## Scope

In scope:

- The provider binary (`cmd/terraform-provider-altinity`).
- The ACM REST client and wire layer (`internal/acm/`).
- The Terraform resources and data sources (`internal/provider/`).
- Examples and documentation that ship in this repository.

Out of scope:

- Vulnerabilities in the upstream Altinity Cloud Manager (ACM) API itself —
  please report those to Altinity directly.
- Vulnerabilities in Terraform, OpenTofu, or the
  `terraform-plugin-framework` SDK — report to HashiCorp / OpenTofu /
  the HashiCorp ecosystem.
- Vulnerabilities in third-party Go modules — report to the upstream
  maintainers. We will pick up fixes via `go.mod` updates.

## What we treat as a security issue

- **Credential disclosure** — any path by which the ACM API token, cluster
  admin password, DB user password, Datadog API key, or any cloud-provider
  credential in the ACM environment payload can be:
  - written to logs at any verbosity below `TRACE` (we hold ourselves to
    `DEBUG`-safe — see "Logging" in `CONTRIBUTING.md`),
  - persisted to Terraform state in unmarked-non-`Sensitive` form,
  - sent in cleartext over the wire (e.g. the token going over HTTP),
  - leaked in error messages or diagnostics shown to operators.
- **Injection** — path-segment, header, JSON-body, or query-string injection
  through user-controlled attributes (e.g. a keeper name with `/` or `..`).
- **Authentication/authorization bypass** — adopting another team's cluster,
  silently destroying state on auth errors, or any flow that takes a
  destructive action against a resource the operator did not declare.
- **Race conditions** — concurrent-apply scenarios that produce orphaned
  resources, lost state, or cross-team takeover.
- **Cryptographic misuse** — improper TLS configuration, certificate
  validation skipped, weak random sources for password defaults, etc.

## Security guarantees we make today

These are the invariants the provider currently enforces. Breaking any of
these without a corresponding change to this document is a security regression.

1. **The `X-Auth-Token` header is never logged.** Not in `DEBUG`, not anywhere.
2. **Sensitive fields are deep-redacted in `DEBUG` logs**, in both request and
   response bodies, at any nesting depth, case-insensitively. The redactor
   covers `adminPass`, `password`, `datadogSettings`, `awsKey`, `awsSecretKey`,
   `awsPrivateKey`, `kubeToken`, `pass`, `sshKey`, `sshPass`,
   `altinityPassword`, `apiKey`, `token`, `secret`, and their case variants.
3. **Path arguments are URL-escaped** before being substituted into request
   paths, so a user-controlled name (e.g. a keeper name with `/` or `..`)
   cannot reshape the request URL.
4. **Cluster adoption is opt-in** via `adopt_existing = true`. By default,
   `terraform apply` against an environment that already contains a cluster
   of the same name fails loudly rather than silently placing it under
   management.
5. **All opaque-JSON cluster attributes** (`datadog`, `backup_options`,
   `uptime_settings`, `alternate_endpoints`) are marked `Sensitive: true`
   so they never appear in plan output or are written unmasked to logs.
6. **`api_url`, when overridden, must use `http` or `https`.** Non-HTTPS,
   non-loopback hosts emit a configuration warning so operators see the
   downgrade.

## State-file caveats

Terraform state is **plaintext on disk by default**. Even with all `Sensitive`
attributes properly marked, the values are still written to state files
verbatim. Use encrypted remote state (OpenTofu's native state encryption,
S3+KMS, GCS+CMEK, etc.) when running this provider in any environment that
manages real credentials.

Never commit `terraform.tfstate*` or `*.tfvars` to source control. The
provider repo's `.gitignore` covers this for example configs; downstream
users are responsible for their own modules.

## Supported versions

Until the provider reaches `1.0.0`, only the latest minor release receives
security fixes. After `1.0.0`, we will support the current and previous
minor releases.

| Version | Supported |
| ------- | --------- |
| `0.x`   | latest minor only |
