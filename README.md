# terraform-provider-altinity

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/Gorgias/terraform-provider-altinity.svg)](https://pkg.go.dev/github.com/Gorgias/terraform-provider-altinity)

Maintained by [Gorgias, Inc.](https://gorgias.com)

A Terraform / OpenTofu provider that manages **ClickHouse clusters inside an
existing Altinity.Cloud environment** via the Altinity Cloud Manager (ACM)
REST API.

The official `altinity/altinitycloud` provider manages only *environments*
(BYOC infrastructure). This provider fills the gap: it creates and manages
ClickHouse clusters and their satellite resources (settings, profiles,
users, keepers).

> **Status:** preview. Resource shapes are stable for the documented
> attributes; some object-valued cluster fields are intentionally
> raw-JSON-string passthroughs pending a typed-block migration (see
> [Outstanding work](#outstanding-work)).

## Quick links

- [SECURITY.md](SECURITY.md) — vulnerability disclosure, security invariants
- [CONTRIBUTING.md](CONTRIBUTING.md) — local dev, PR process, coding bar
- [CHANGELOG.md](CHANGELOG.md) — release notes
- [`examples/complete/`](examples/complete/) — end-to-end runnable config

## Requirements

| Tool | Version |
| ---- | ------- |
| Terraform | `>= 1.5.7` (protocol v6) |
| OpenTofu | `>= 1.6` (any version that speaks protocol v6) |
| Go (build only) | `>= 1.26` |

The provider deliberately avoids post-1.5 Terraform features
(provider-defined functions, ephemeral resources, write-only attributes) so
a single binary serves both Terraform and OpenTofu.

## Installation

### From the Terraform registry

```hcl
terraform {
  required_version = ">= 1.5.7"
  required_providers {
    altinity = {
      source  = "gorgias/altinity"
      version = "~> 0.1"
    }
  }
}
```

### From source (development)

```sh
git clone https://github.com/Gorgias/terraform-provider-altinity.git
cd terraform-provider-altinity
make build
cp examples/dev.tfrc.example examples/dev.tfrc   # edit the absolute path
export TF_CLI_CONFIG_FILE=$PWD/examples/dev.tfrc
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full dev loop.

## Provider configuration

```hcl
provider "altinity" {
  # The ACM API token. Mint one in the ACM UI under My Account ->
  # Anywhere API Access. Sent as the X-Auth-Token header.
  # May be omitted and supplied via ALTINITYCLOUD_API_TOKEN instead.
  api_token = var.altinity_api_token

  # Optional. Defaults to https://acm.altinity.cloud/api.
  # Must be http or https; non-HTTPS non-loopback URLs emit a warning.
  # api_url = "https://acm.altinity.cloud/api"
}
```

The token is never logged. See [Security](#security) for the full set of
guarantees.

### Environment-variable knobs

| Variable | Default | Purpose |
| -------- | ------- | ------- |
| `ALTINITYCLOUD_API_TOKEN` | (none) | Fallback for `provider.api_token`. |
| `ALTINITYCLOUD_CLUSTER_SETTLE_DELAY` | `30s` | Wait between cluster-Create convergence and downstream-resource Create. Any `time.ParseDuration` string. CI pipelines running many parallel applies can drop this — downstream Creates have their own transient-race retry, so a shorter settle just shifts the recovery to the retry path. Invalid values fall back to the default with a `WARN` log. |

## Resources and data sources

### Resources

| Resource | Manages |
| -------- | ------- |
| `altinity_clickhouse_cluster` | A ClickHouse cluster inside an environment |
| `altinity_clickhouse_keeper` | A CH Keeper coordination cluster |
| `altinity_clickhouse_user` | A DB user on a cluster |
| `altinity_clickhouse_profile` | A settings profile on a cluster |
| `altinity_clickhouse_profile_setting` | A single setting attached to a profile |
| `altinity_clickhouse_cluster_setting` | A single cluster-level ClickHouse setting |

### Data sources

| Data source | Resolves |
| ----------- | -------- |
| `altinity_environment` | An ACM environment by name (id, type, domain, state) |
| `altinity_clickhouse_versions` | Available ClickHouse versions, filterable by major/minor/stream (`altinity-stable` / `altinity-antalya` / `upstream`), with a `latest` selector |
| `altinity_node_types` | Valid instance-type codes per scope (`clickhouse` for clusters, `zookeeper` for keepers) |
| `altinity_storage_classes` | Valid storage-class codes (e.g. `pd-balanced`, `pd-ssd`) |
| `altinity_zones` | Availability zones for the environment |
| `altinity_clickhouse_profile` | A single settings profile on a cluster, by name |
| `altinity_clickhouse_profiles` | All settings profiles on a cluster (bootstrap + custom) |

## Minimum viable cluster

```hcl
data "altinity_environment" "this" {
  name = "your-environment-name"
}

data "altinity_node_types" "ch" {
  environment = data.altinity_environment.this.id
  scope       = "clickhouse"
}

data "altinity_clickhouse_versions" "ch" {
  environment = data.altinity_environment.this.id
  platform    = data.altinity_environment.this.type
  major       = 25
  stream      = "altinity-stable"
}

resource "altinity_clickhouse_keeper" "k" {
  environment   = data.altinity_environment.this.id
  name          = "demo-keeper"
  instance_type = "e2-standard-2"   # zookeeper-scoped — see altinity_node_types
}

resource "altinity_clickhouse_cluster" "ch" {
  environment = data.altinity_environment.this.id
  name        = "demo"
  role        = "prod"

  node_count    = 1
  shards        = 1
  replicas      = 1
  node_type     = data.altinity_node_types.ch.node_types[0].code
  size          = "10"
  storage_class = "pd-balanced"
  version       = data.altinity_clickhouse_versions.ch.latest

  keeper_name    = altinity_clickhouse_keeper.k.name
  admin_user     = "admin"
  admin_password = var.admin_password   # sensitive

  timeouts = {
    create = "30m"
    update = "20m"
    delete = "20m"
  }
}
```

See [`examples/complete/`](examples/complete/) for the full runnable config
including a user, profile, and cluster-level setting.

## Cluster lifecycle model

**Create** launches the cluster, persists the returned id to state
*immediately*, then polls the status endpoint until the cluster is healthy.
If the apply is interrupted between launch and convergence, re-running
`terraform apply` resumes polling rather than launching a duplicate.

**Read** refreshes the computed read-back attributes (`status`, `state`,
`system_version`, `endpoint`, `endpoint_http`). A 404 drops the resource
from state. Secrets (`admin_password`, `password`, `datadog`) are preserved
from the prior state because the API never returns them on read.

**Update** is dispatched to specific ACM endpoints in a fixed order:

1. `upgrade` (version changes — forward-only; downgrades are rejected at
   plan time)
2. `rescale` (compute + storage)
3. `backup` (schedule config, no poll)
4. `admin_password` (resolves the admin user id by login, then updates
   in-place via the DB user API)

Each poll-required step waits for terminal-healthy before the next. After
every successful sub-mutation the provider re-Reads and writes the
converged-so-far state, so a failure in a later step leaves state
reflecting the steps that succeeded — re-applying converges the remainder.

**Delete** terminates the cluster and polls the environment list until the
cluster is gone (ACM returns 403 — not 404 — for a deleted cluster id on
the per-id GET, so list-by-environment is the unambiguous "gone" signal).

### Adopting an existing cluster

By default, `terraform apply` against an environment that already contains
a cluster with the same name **fails loudly**:

```
a cluster named "analytics" already exists in environment 2267 (id=12345);
Terraform refuses to adopt it by default. Set adopt_existing = true to take
it over, or destroy the existing cluster first if it is unmanaged.
```

To take over a pre-existing cluster (e.g. when migrating an ACM-UI-created
cluster into IaC), set:

```hcl
resource "altinity_clickhouse_cluster" "this" {
  # ...
  adopt_existing = true
}
```

Adoption still validates that immutable topology fields (environment, type,
role, shards, replicas, keeper_name) match the plan. If any differ,
adoption fails — destroy the existing cluster or align your config first.

### Drift detection caveats

The provider's drift detection is **selective**, not exhaustive — by design.
A few attributes are configured-value-authoritative and out-of-band edits in
the ACM UI will not be corrected on the next apply:

- **`altinity_clickhouse_user.networks`** — ACM canonicalizes networks
  server-side in unpredictable ways (e.g. `0.0.0.0/0` → `::/0`, the
  ClickHouse match-all form). Comparing positionally would produce
  perpetual diffs, so the provider keeps the configured value as the
  source of truth and emits a `WARN`-level log when the API value has
  diverged. If you click "add a network" in the ACM UI, the next apply
  silently re-asserts the configured list.
- **`altinity_clickhouse_user.databases`** — same rationale. ACM treats
  the list as a multiset (order is not meaningful); the provider compares
  as a multiset before warning, and the configured list wins.
- **Cluster opaque JSON attributes** (`datadog`, `backup_options`,
  `uptime_settings`, `alternate_endpoints`) — these are passed through
  to ACM as raw JSON strings and the API doesn't echo them back in a
  drift-comparable shape. The configured value is authoritative; UI
  edits to these blocks will be overwritten.
- **Cluster `host`/`port`/`http_port`/`ssh_port`/`mysql_port`** — sent at
  launch but not returned on Read. The configured value is preserved in
  state; manual UI changes are not detected.

What **is** drift-detected and corrected on the next apply: cluster
topology (`shards`, `replicas`, `node_type`, `size`, `version`, `storage_class`,
`azlist`), profile membership (`profile_id` on users), profile and
cluster setting values, keeper instance type and zones. Out-of-band
changes to these will be reverted to the configured value.

If you need strict UI/Terraform parity, ensure that everything is
managed through Terraform — the provider is built for IaC-first workflows
and the canonicalization quirks above make positional drift comparison
unsafe for the listed attributes.

### Importing

```sh
terraform import altinity_clickhouse_cluster.this        12345
terraform import altinity_clickhouse_keeper.k            2267:demo-keeper
terraform import altinity_clickhouse_user.app            12345:app
terraform import altinity_clickhouse_cluster_setting.max_threads 12345:max_threads
terraform import altinity_clickhouse_profile.readonly    12345:readonly
```

Satellite resource IDs use the form `<cluster_id>:<name>`. Keeper IDs use
`<environment>:<name>`. The keeper split is on the **first** colon (env id
is numeric); satellite splits are on the **last** colon (defensive).

## Architecture

```
cmd/terraform-provider-altinity/   provider binary entrypoint
internal/acm/                      hand-written REST client + domain types
internal/acm/wire/                 generated wire types + endpoint registry
internal/provider/                 Terraform resources, data sources, schema
tools/specgen/                     OpenAPI -> wire-types code generator
```

The split between `acm/` (clean domain types) and `acm/wire/` (faithful to
the JSON shape) exists so the loose typing of the ACM REST API — string-ints,
`0|1` booleans, opaque-object fields — never leaks into the Terraform
layer.

### Code generation

`tools/specgen` reads `internal/acm/wire/reference.json` (vendored OpenAPI
spec) and emits `endpoints_gen.go` (the operation registry) and
`models_gen.go` (the wire structs) for an explicit allowlist of operations.

```sh
make generate
```

A guard test asserts the allowlist stays in sync with the generated
registry and that `go generate` produces no diff — a forgotten regeneration
fails CI.

## Security

This provider holds several invariants to keep credentials out of logs,
state, and the network. The short version:

- The `X-Auth-Token` header is **never** logged.
- Request and response bodies are **deep-redacted** before any `DEBUG` log,
  covering AWS keys, k8s tokens, Datadog API keys, SSH credentials, admin
  passwords — case-insensitive, at any nesting depth.
- HTTP path arguments are **URL-escaped** so a malicious name cannot
  reshape the request URL.
- Cluster adoption is **opt-in** (`adopt_existing = true`).
- All opaque-JSON cluster attributes (`datadog`, `backup_options`,
  `uptime_settings`, `alternate_endpoints`) are marked `Sensitive: true`.

See [SECURITY.md](SECURITY.md) for the full policy, the responsible-disclosure
contact (**security@gorgias.com**), and the complete list of invariants.

### Secrets in state

Sensitive attributes are still **persisted to Terraform state in plaintext**
— this is true of every provider. Use encrypted remote state (OpenTofu's
native state encryption, S3+KMS, GCS+CMEK, …) when managing real
credentials. The provider preserves prior-state secrets across reads
(because the API never returns them) so secrets are not wiped or
spuriously flagged as drift.

## Debugging

Enable structured debug logs of every ACM call:

```sh
TF_LOG=DEBUG terraform apply 2>&1 | grep acm
# or just the provider's logs:
TF_LOG_PROVIDER=DEBUG terraform apply
```

This distinguishes "still creating" = waiting on the per-env operation lock
(`environment busy; waiting to retry`) from "still creating" = polling
status (`acm poll status`).

## Development

```sh
make build      # build the provider binary into bin/
make test       # offline tests (uses httptest fixtures)
make testacc    # acceptance tests (TF_ACC=1; requires a live token + env)
make generate   # regenerate wire codegen
make lint       # go vet + staticcheck (if installed)
make docs       # tfplugindocs (if installed)
make install    # install into the local filesystem mirror for manual testing
```

All unit tests are offline: they use `net/http/httptest` with fixtures
captured under `internal/*/testdata/`. They never touch the live ACM API.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full developer guide,
coding conventions, and PR process.

## Outstanding work

These are deliberately stubbed and marked `TODO(spike)` in the source.
They must NOT be guessed — they require a spike against captured payloads
or the live API before implementation.

1. **Strongly-typed nested-block shapes for object-valued cluster fields.**
   The opaque attributes — `datadog`, `backup_options`, `uptime_settings`,
   `alternate_endpoints` — are currently modeled as raw JSON-string
   passthroughs, not typed nested blocks.
2. **Real terminal poll status strings.** `internal/acm/poll.go` uses
   placeholder healthy/error status constants for terminal-state detection.
3. **In-place vs. RequiresReplace** decisions on a few cluster networking
   and storage fields (`uptime`, `volume_type`, `data_path`, `ip_whitelist`,
   `lb_type`) are conservatively `RequiresReplace` pending confirmation of
   an in-place update path.

## License

Apache 2.0 — see [LICENSE](LICENSE).
