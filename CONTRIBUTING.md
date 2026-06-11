# Contributing to terraform-provider-altinity

Thanks for taking the time to contribute. This document describes how to
propose changes, how the codebase is structured, and the bar we hold for
merging.

## Code of conduct

Be respectful. Assume good intent. Critique code, not people. We follow the
spirit of the [Contributor Covenant](https://www.contributor-covenant.org/).

## Reporting security issues

**Do not file security issues as public GitHub issues.** Email
**security@gorgias.com** instead. See [SECURITY.md](SECURITY.md) for the full
disclosure process and the security invariants we maintain.

## Reporting bugs and requesting features

For non-security issues, open a [GitHub issue](https://github.com/gorgias-oss/terraform-provider-altinity/issues).
Include:

- The provider version and Terraform/OpenTofu version.
- A minimal `.tf` file that reproduces the problem.
- The exact error message and, if relevant, the output of `TF_LOG=DEBUG`
  (with secrets redacted — the provider redacts them at the source, but
  always double-check).
- For feature requests: the use case, not just the proposed solution. We
  often have a different approach in mind that solves the same problem.

## Development setup

Prerequisites:

- **Go 1.26** or newer (see `go.mod` for the exact minimum).
- **Terraform >= 1.5.7** or **OpenTofu**. The provider speaks protocol v6
  and runs against both.
- (Optional) `staticcheck`, `tfplugindocs`, `golangci-lint` for local quality
  gates — `make lint` and `make docs` no-op cleanly if they aren't installed.

Clone and build:

```sh
git clone https://github.com/gorgias-oss/terraform-provider-altinity.git
cd terraform-provider-altinity
make install-hooks  # enable the pre-commit secret scan (run once per clone)
make build          # produces bin/terraform-provider-altinity
make test           # offline test suite (~5s)
make lint           # go vet + staticcheck if installed
```

**Enable the git hooks** (`make install-hooks`, one-time per clone): this points
`core.hooksPath` at `.githooks/`, whose `pre-commit` runs `scripts/check-secrets.sh`
to block commits containing secrets or unredacted production data (PEM keys,
JWT/bearer tokens, unmasked secret fields, Datadog-style keys). Test fixtures must
be built from **sanitized** captures — redact credentials *and* prod identifiers
(env names, hostnames, bucket names, IPs, org/owner details) and use synthetic
values (`example-env`, `REDACTED-…`, `203.0.113.x`). Scan the whole tree anytime
with `make check-secrets`. A genuine false positive can be bypassed with
`git commit --no-verify`.

To run the example against your locally built binary:

```sh
cp examples/dev.tfrc.example examples/dev.tfrc
# Edit examples/dev.tfrc and set the absolute path to bin/
export TF_CLI_CONFIG_FILE=$PWD/examples/dev.tfrc
cd examples/complete
terraform init    # or `tofu init`
terraform plan    # or `tofu plan`
```

## Project layout

```
cmd/terraform-provider-altinity/   provider binary entrypoint
internal/acm/                      hand-written ACM REST client + domain types
internal/acm/wire/                 generated wire types + endpoint registry
internal/provider/                 Terraform resources, data sources, schema
tools/specgen/                     OpenAPI -> wire-types code generator
examples/                          end-to-end and per-resource example configs
```

The two layers (`acm/` and `acm/wire/`) exist so the loose typing of the ACM
REST API never leaks into the Terraform layer. Wire types are faithful to
the JSON shape; domain types are clean Go.

## How to make a change

1. **Open an issue first** for anything beyond a small bugfix or doc change.
   We will agree on the approach before you spend time writing code.

2. **Branch from `main`** with a descriptive name:

   ```sh
   git checkout -b fix/cluster-rescale-timeout
   ```

3. **Make focused commits.** One logical change per commit. Avoid mixing
   refactors with behavior changes — they're hard to review together.

4. **Add or update tests** for any behavior change. The provider's test
   suite is offline (it uses `net/http/httptest` with fixtures) so tests
   run in seconds and never need a live token.

5. **Run the full suite** before pushing:

   ```sh
   make test     # all unit tests
   make lint     # vet + staticcheck
   make build    # ensures the binary still compiles
   ```

6. **Push and open a PR.** Describe what changed, why, and how it was
   tested. Link the issue.

## What we look for in a PR

- **Tests that fail without the fix.** If the change is to fix a bug, the
  diff should include a test that demonstrates the bug before the fix.
- **No unrelated changes.** Don't bundle a typo fix with a feature; don't
  reformat files you didn't touch.
- **Public-facing changes documented.** New resources, attributes, or
  behavior changes should land with a `CHANGELOG.md` entry and updated
  schema descriptions.
- **Security-relevant changes flagged.** If the diff touches secret handling,
  the HTTP client, validators, or any auth/permission flow, say so in the
  PR description. We'll route it through a security review.

## Coding conventions

- **Standard Go formatting** — `gofmt -s` (run `make fmt`).
- **Errors carry context.** `fmt.Errorf("acm: launch cluster: %w", err)`,
  not bare `return err`.
- **No bare panics** in non-test code. Type assertions use `, ok`.
- **No silent failures.** Every error path either returns the error or
  produces a Terraform diagnostic.
- **Comments explain WHY, not WHAT.** Well-named identifiers cover the WHAT.
  Reserve comments for invariants, edge cases, or non-obvious decisions.
- **Sensitive attributes must be marked `Sensitive: true`** at the schema
  level, even for opaque-JSON passthroughs that may contain credentials.

## Logging policy

The ACM client logs at `DEBUG` level. The invariants:

- **Never log the `X-Auth-Token` header.** Period.
- **Always route request and response bodies through `redactBody`** before
  logging. The redactor deep-walks JSON and masks any field matching
  `sensitiveBodyKeys` (case-insensitive, any nesting depth). If you add a
  field that carries a secret, extend `sensitiveBodyKeys` in
  `internal/acm/client.go` and add a test in `client_test.go`.
- **Don't add new logging in error paths** without checking that the error
  message itself doesn't contain secrets. ACM error envelopes occasionally
  echo the request body.

When in doubt, run `go test -run TestRedactBody -v` and add a case for the
new field.

## Generated code

The wire layer (`internal/acm/wire/endpoints_gen.go` and `models_gen.go`) is
generated from a vendored OpenAPI spec at
`internal/acm/wire/reference.json`. To add or change an endpoint:

1. Update the allowlist in `tools/specgen/main.go`.
2. Run `make generate` (= `go generate ./...`).
3. Run `make test` — the codegen guard test asserts the registry matches
   the allowlist and that `go generate` produces no diff.

Do not hand-edit the generated files. The CI guard will fail.

## Documentation

User-facing docs live in two places:

- **`README.md`** — the entry point. Update it when adding a resource,
  data source, or major capability.
- **`examples/`** — runnable Terraform configs. Keep the snippets minimal;
  they're the first thing operators copy-paste.

We do not vendor generated Terraform registry docs in the repo. If
`tfplugindocs` is installed, `make docs` regenerates them from the schema
descriptions and the `examples/` snippets.

## Releasing

Releases are cut by Gorgias maintainers. The flow is fully automated by
[GoReleaser](https://goreleaser.com/) via `.github/workflows/release.yml` —
the maintainer's job is only to (a) curate `CHANGELOG.md` and (b) push a
semver tag.

### Prerequisites (one-time)

- A GPG keypair for signing Terraform Registry releases (Ed25519 or RSA).
  Set the following GitHub repository secrets:
  - `GPG_PRIVATE_KEY` — ASCII-armored private key
    (`gpg --armor --export-secret-key <fingerprint>`).
  - `PASSPHRASE` — passphrase for the key (empty if the key is unencrypted).
- The public key uploaded to the Terraform Registry account so the registry
  can verify uploaded archives.

### Cutting a release

1. **Promote `Unreleased` in `CHANGELOG.md`.** Rename the heading from
   `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`, then add a fresh empty
   `## [Unreleased]` heading at the top. The release workflow extracts the
   `## [X.Y.Z]` section verbatim and uses it as the GitHub release body —
   anything not in that section won't appear in the release notes.

   Verify locally:

   ```sh
   bash scripts/release-notes.sh X.Y.Z
   ```

2. **Commit** the CHANGELOG promotion as `chore: prepare X.Y.Z release` on
   `main`.

3. **Tag** the commit with the matching semver tag (always `v`-prefixed):

   ```sh
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

4. The `release` workflow fires:
   - Extracts the `## [X.Y.Z]` section from `CHANGELOG.md`.
   - Builds cross-platform binaries (linux/darwin/windows × amd64/arm64).
   - GPG-signs `SHA256SUMS`.
   - Publishes a GitHub release with the binaries, sums, signature, and
     `terraform-registry-manifest.json`.
5. The Terraform Registry picks the new release up on its next sync.

### Versioning

The provider follows [Semantic Versioning](https://semver.org/):

- `MAJOR` — breaking schema changes (renamed attributes, removed resources,
  changed RequiresReplace semantics that would force destruction).
- `MINOR` — new resources, new attributes, new data sources, additive
  behavior changes.
- `PATCH` — bug fixes, internal refactors, docs.

Until `1.0.0` we use `0.MINOR.PATCH` and reserve the right to break minor
versions if required — those breaks are called out under `### Changed` and
`### Removed` in CHANGELOG.

Pre-releases use the `vX.Y.Z-rc.N` form. The release workflow marks any tag
matching that pattern as a GitHub pre-release automatically.

## Questions

Open a [discussion](https://github.com/gorgias-oss/terraform-provider-altinity/discussions)
or reach out via the issue tracker. We try to respond within a few business
days.
