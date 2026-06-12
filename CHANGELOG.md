# Changelog

All notable changes to this provider will be documented in this file. The
format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and the provider follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

### BREAKING

- `altinity_clickhouse_cluster.public_endpoint` now defaults to **`false`**
  (private). A new cluster is only internet-reachable when the config
  explicitly sets `public_endpoint = true`. **Before upgrading**, any
  existing cluster that relied on the old public default must add
  `public_endpoint = true` to its config — otherwise the next plan shows
  `public_endpoint: true → false`, which forces **cluster replacement**.
  An explicitly public endpoint with no `ip_whitelist` now also raises a
  plan-time warning (set `ip_whitelist = "0.0.0.0/0"` to silence it when
  internet-wide exposure is intentional).
- `altinity_clickhouse_cluster` drops five attributes after an audit of the
  ACM API surface:
  - `volume_type` — appeared in **no** ACM request body (launch / edit /
    rescale / upgrade) and is never read back; it was a dead attribute whose
    only effect was forcing spurious replacements.
  - `host`, `port`, `http_port`, `ssh_port` — ACM-internal launch plumbing
    (internal bind host + native/HTTP/SSH service ports). The ACM UI always
    sends the fixed defaults (`localhost`/9900/5123/2222) and never exposes
    them to the operator; the provider now pins those values in the launch
    payload. **Before upgrading**, remove these attributes from configs (a
    config that overrode them was deviating from ACM's supported surface).
    Existing **state** is migrated automatically: the resource schema is now
    version 1 with a state upgrader that drops the removed attributes from
    v0 state files on the first plan/apply after upgrading.
- `altinity_clickhouse_keeper` no longer silently adopts a pre-existing
  keeper of the same name: like the cluster resource, Create now fails
  loudly unless `adopt_existing = true` is set. This also applies to
  resuming a keeper create that was interrupted after launch — re-apply
  with `adopt_existing = true`.

### Added

- `altinity_environment` resource — provisions an Altinity.Cloud environment
  via the request flow (`POST /environments/request`) and polls until ready.
  - Create **refuses to adopt** a pre-existing environment: if an environment
    with the same `name` already exists (names are unique per organization),
    the apply fails and directs you to `terraform import` it instead —
    Terraform never silently takes over infrastructure it did not create. The
    guard is enforced both at apply (Create) and at plan time (a `ModifyPlan`
    check, so `terraform plan` surfaces the error instead of rendering a
    misleading `+ create`). A create whose readiness poll exceeds the create
    timeout records **no** state; because the environment was already
    requested, re-applying hits the same guard — raise the create timeout, or
    `terraform import` the environment by id once it finishes provisioning.
  - Replacing an existing environment is **blocked at plan time**: changing
    `name`, `cloud_provider`, or `region` forces replacement, but Terraform
    cannot delete an Altinity.Cloud environment (destroy only drops it from
    state — see below), so a destroy+create would strand the operator with
    empty state and a still-live environment. The plan fails with
    manual-migration guidance instead.
  - `terraform import` (by numeric environment id) reconstructs full state from
    the API: `cloud_provider` (from `kubeProvider`), `region` (from
    `options.region`), the `datadog` block (non-secret fields), and
    `maintenance_windows`. The datadog `api_key` is write-only — sent on apply,
    never read back into state — so it imports as null and must be re-supplied
    in config.
  - `maintenance_windows` are read from the environment `acc-check` endpoint
    (`EnvironmentCloudCheck`, `GET /environment/{id}/acc-check`; the plain
    environment GET returns them as null) on both import and refresh, so
    out-of-band changes — including deleting a window — are drift-detected when
    the block is managed. Omit the block (null) to leave it unmanaged (never
    probed); set `[]` to clear all windows. Modeled as a **set** (as is each
    window's `days`), so neither window order nor day order produces a spurious
    diff (ACM reorders both). Reads are best-effort: a failed/unreported
    acc-check keeps the last-known windows rather than faking drift.
  - The environment `name` must start with the caller's organization slug — ACM
    rejects other names server-side with HTTP 400 (`Invalid Environment Name
    prefix`; undocumented in the API spec, the error names the exact required
    prefix). Documented on the attribute and example; there is no provider-side
    validator (the provider cannot know the org slug).
  - `terraform destroy` does **not** delete the environment (deletion requires
    an out-of-band email + MFA confirmation that cannot be automated); it warns
    and drops the resource from state.
  - Supports an optional `datadog` block (`api_key` is write-only and
    `Sensitive`).
- `altinity_node_type` resource — manages environment node types
  (instance shapes) with adopt-by-(scope, code) create, in-place `code`
  edits, and a self-healing id lookup. Tolerations / nodeSelector /
  extraSpec are deliberately not managed. Imports as
  `<environment>:<scope>:<code>`.
- `altinity_regions` data source — regions per cloud provider, feeding
  `altinity_environment.region`.
- `altinity_instance_types` data source — instance-type catalog per cloud
  provider + region, feeding `altinity_node_type`.
- `altinity_node_types` data source now exposes `id`, `used`, and
  `capacity` per node type.
- Versioned pre-commit secret scan (`scripts/check-secrets.sh`,
  `make install-hooks`, `make check-secrets`) blocking PEM keys, JWTs, and
  unmasked secret-bearing fixture fields.

### Changed

- `altinity_clickhouse_cluster` now updates these attributes **in place** via
  the ACM cluster edit API (`POST /cluster/{id}`, operation `ClusterEdit`)
  instead of forcing cluster replacement: `name` (NOTE: ACM derives endpoint
  hostnames from the name, so a rename changes `endpoint`/`endpoint_http`),
  `role`, `lb_type`, `ip_whitelist`, `mysql_port`, `timezone`, `uptime`,
  `uptime_settings`, `alternate_endpoints`, and `datadog`.
  - Removing `ip_whitelist` clears the allow-list explicitly at the API (the
    endpoint becomes unrestricted — pair with `public_endpoint` deliberately).
  - Removing one of the opaque JSON blocks (`uptime_settings`,
    `alternate_endpoints`, `datadog`) leaves the last-applied settings active
    at ACM (they are not read back); send `"{}"` / `"[]"` to clear explicitly.
  - `mysql_protocol` and `zone_awareness` are also in the `ClusterEdit` body
    but keep forcing replacement for now: the spec declares them int enum
    [0,1] while the launch endpoint live-rejects ints in favor of JSON
    booleans, so the edit-side wire encoding needs a live spike first.
  - Adoption (`adopt_existing`) no longer blocks on a `role` mismatch — role
    converges on the next apply now that it is mutable.
- Provider registry namespace corrected to `gorgias-oss` across the dev
  mirror, dev overrides, and all examples/docs.
- Removed the `make testacc` target and its README mention: no
  `resource.Test`-style acceptance tests exist yet, so the target only
  re-ran the offline unit suite while implying live-API coverage.

### Fixed

- Adding `admin_password` to an imported `altinity_clickhouse_cluster` now
  actually rotates the password. Previously the update was skipped (state
  held no prior value to diff against) while the new value was still
  recorded in state — so the API never received it and subsequent applies
  saw no diff (silent non-rotation).
- `altinity_clickhouse_keeper.zones` now accepts unknown values (e.g.
  `zones = data.altinity_zones.this.zones` chained behind a same-apply
  `altinity_environment`) instead of failing with a framework decode
  error, matching the cluster's `azlist` behavior.
- The `altinity_clickhouse_cluster` schema description referenced a
  nonexistent `altinity_clickhouse_setting` resource; it is
  `altinity_clickhouse_cluster_setting`.
- Added import documentation for `altinity_environment` and
  `altinity_node_type` (the latter imports as
  `<environment>:<scope>:<code>`, not its state id).

### Security

- Bumped `golang.org/x/net` to v0.56.0 (CVE-2024-45338, CVE-2025-22870,
  CVE-2025-22872 in the previous indirect pin v0.28.0).
- Debug-log redaction now masks the opaque `backup_options` /
  `uptime_settings` / `alternate_endpoints` blobs wholesale (the schema
  marks them Sensitive because they may carry credentials, but the
  `TF_LOG=DEBUG` redactor previously logged their contents verbatim), and
  falls back to substring matching (`pass`/`secret`/`token`/`key`/`cred`)
  for credential-shaped keys not in the exact-match list (e.g.
  `secretAccessKey`). A new guard test fails when wire codegen introduces
  a credential-shaped field the redactor doesn't cover.
- All GitHub Actions in CI/release workflows are now pinned to release
  commit SHAs (and bumped to their latest majors), including the action
  that handles the GPG release-signing key.
- Removed committed `examples/**/.terraform.lock.hcl` files (pinned a
  locally-built provider hash that can never match registry artifacts);
  lock files are now gitignored.

## [0.1.1] — 2026-06-09

Documentation release — no functional provider changes.

### Added

- Terraform Registry documentation under `docs/` (provider index, all
  resources and data sources), generated from the provider schema and the
  `examples/` tree with `tfplugindocs`. The Registry now renders full docs
  for the provider instead of "no documentation".
- `scripts/gen-docs.sh` and a working `make docs` target. Because the
  `main` package lives in `cmd/`, a bare `tfplugindocs generate` cannot
  build the provider; the script builds it, exports the schema via
  Terraform, and renders docs with `--providers-schema`.

### Changed

- Reworded several `altinity_clickhouse_cluster` attribute descriptions
  (`datadog`, `backup_options`, `uptime_settings`, `alternate_endpoints`,
  `volume_type`, `secure`, `lb_type`, `ip_whitelist`, `uptime`) to drop
  internal engineering notes that previously surfaced in `terraform plan`
  output and the Registry. Behavior is unchanged.

## [0.1.0] — 2026-06-09

Initial release.

### Resources

- `altinity_clickhouse_cluster` — manages a ClickHouse cluster inside an
  ACM environment (compute, storage, version, ZK/Keeper coordination, AZ
  layout, networking, backup schedule, Datadog/uptime integrations,
  admin user).
- `altinity_clickhouse_keeper` — CH Keeper coordination cluster, referenced
  by `cluster.keeper_name`. `ha` is Computed-only (ACM auto-promotes based
  on the bound cluster's replica count).
- `altinity_clickhouse_user` — DB user on a cluster, with optional profile
  attachment, network ACL, and per-database grants.
- `altinity_clickhouse_profile` — settings profile on a cluster.
- `altinity_clickhouse_profile_setting` — one setting attached to a
  profile. At least one is required for ACM to actually push the profile
  into ClickHouse's `user_directories` — an empty profile is metadata-only.
- `altinity_clickhouse_cluster_setting` — one cluster-level ClickHouse
  setting via `/cluster/{cluster}/settings`.

### Data sources

- `altinity_environment` — resolve an ACM environment by name; exposes
  `id`, `type`, `domain`, `status`, `state`, `parent_id`, `owner_id`.
  `parent_id` is `null` (not `""`) when the environment has no parent.
- `altinity_clickhouse_versions` — discover available ClickHouse versions
  filterable by major/minor/stream (`altinity-stable`, `altinity-antalya`,
  `upstream`) with a `latest` selector.
- `altinity_node_types` — valid instance-type codes per scope
  (`clickhouse` for clusters, `zookeeper` for keepers).
- `altinity_storage_classes` — valid storage-class codes (e.g.
  `pd-balanced`, `pd-ssd`).
- `altinity_zones` — availability zones for the environment.
- `altinity_clickhouse_profile` — resolve a single profile on a cluster by
  name. Errors at plan time if the profile does not exist.
- `altinity_clickhouse_profiles` — list every profile on a cluster
  (bootstrap + custom).

### Lifecycle

- Cluster Create persists the returned id to state **immediately**, then
  polls until healthy. A `Ctrl-C` between launch and convergence resumes
  polling on the next apply rather than launching a duplicate.
- Cluster Update is dispatched to specific ACM endpoints in a fixed order:
  `upgrade` → `rescale` → `backup` → `admin_password`. Each poll-required
  step waits for terminal-healthy before the next, and every successful
  sub-mutation re-Reads + writes the converged-so-far state, so a failure
  in a later step leaves state reflecting what already succeeded.
- Every mutating step polls `/cluster/{id}/status` for
  `action == "Completed"` in addition to the top-level cluster status
  (the top-level status stays `"online"` throughout long-running ops).
- Forward-only version upgrades. In-place downgrades are rejected at
  plan time via the version-code comparison (major.minor.patch.<build>).
- `adopt_existing` flag (default `false`) on `altinity_clickhouse_cluster`
  gates same-named cluster takeover. With the default, `terraform apply`
  errors loudly on a name collision instead of silently adopting another
  team's cluster.
- Adoption validates that immutable topology fields (`environment`,
  `type`, `role`, `shards`, `replicas`, `keeper_name`) match the plan.
- Cluster Delete polls the environment list until the cluster is gone
  (ACM returns 403 — not 404 — for a deleted cluster id on the per-id
  GET, so list-by-environment is the unambiguous "gone" signal).
- `shards`, `replicas`, `azlist`, `node_type`, and `memory` route through
  `ClusterRescale`. `azlist` order changes are treated as no-op (ACM
  canonicalizes ordering).

### Transient-race resilience

- `RetryOnTransientCreateRace` wraps every satellite-resource Create
  (cluster, profile, profile_setting, cluster_setting, user). It absorbs
  the ACM operator-push race against fresh clusters: Code 62 SYNTAX_ERROR
  from malformed generated SQL, Code 180 THERE_IS_NO_PROFILE, Code 192,
  Code 511, the `id == 0` half-commit, and the `{"data": false}` bare-bool
  envelope. The retry budget is ~9 minutes with exponential backoff capped
  at 60s. Periodic `INFO` log every fourth attempt keeps operators
  oriented without `TF_LOG=DEBUG`.
- Adopt-by-name on Create for cluster / profile / profile_setting /
  cluster_setting / user. An orphan from a half-committed prior Create
  is reconciled (Edit/Update) instead of duplicated.
- `RetryWhileBusy` wraps mutations against the per-environment operation
  lock (ACM serializes mutations per environment). Periodic `INFO` log
  every fourth attempt.

### Drift detection

The provider's drift detection is **selective**. The following attributes
are configured-value-authoritative — out-of-band changes are not corrected
on the next apply:

- `altinity_clickhouse_user.networks` and `.databases` — ACM canonicalizes
  these server-side (e.g. `0.0.0.0/0` → `::/0`); positional comparison
  would lie. The configured list wins; a `WARN` log surfaces divergence.
  `databases` is compared as a multiset (order-independent) before
  warning.
- Cluster opaque-JSON attributes (`datadog`, `backup_options`,
  `uptime_settings`, `alternate_endpoints`) — passed through as raw JSON
  and not echoed in a drift-comparable shape.
- Cluster `host`, `port`, `http_port`, `ssh_port`, `mysql_port` — sent at
  launch but not returned on Read.

What **is** drift-detected and corrected: cluster topology (`shards`,
`replicas`, `node_type`, `size`, `version`, `storage_class`, `azlist`),
profile membership on users, profile and cluster setting values, keeper
instance type and zones.

Parent-resource drift on satellite Reads: `404` and `403` are both
treated as drift (ACM returns 403 for list-against-a-deleted-parent
instead of 404). The cluster's per-id GET deliberately does NOT treat
403 as drift — a 403 there is far more likely a token problem than a
gone resource. See README's "Drift detection caveats" for the full list.

### Validation (plan time)

- `node_count` is rejected when it disagrees with `shards × replicas`.
- `zookeeper` and `keeper_name` are mutually exclusive — both set fails
  at plan time.
- Reserved profile names (`default`, `readonly`, case-insensitive) are
  rejected — ACM auto-creates and auto-maintains them; managing them
  from Terraform produces opaque ACM bookkeeping conflicts.
- Path-segment names (keeper / user / profile / setting names) reject
  `:`, `/`, whitespace, and control characters.
- `provider.api_url` validates scheme at configuration time; non-`http`/
  `https` errors, non-HTTPS non-loopback emits a warning.
- Provider preflight: `Configure` runs a single `ListEnvironments` call
  so a bad token fails before any apply work begins, not mid-launch.

### Security

- `X-Auth-Token` is never logged.
- Response bodies are deep-redacted in `DEBUG` logs — the redactor walks
  nested JSON objects and arrays, masks values at any depth,
  case-insensitive key matching. Covers AWS/k8s/Datadog credentials,
  SSH credentials on nodes, and admin/user passwords.
- HTTP path parameters are URL-escaped before substitution — a name with
  `/`, `..`, or `%` cannot reshape the request URL.
- `backup_options`, `uptime_settings`, `alternate_endpoints` are marked
  `Sensitive: true` (opaque-JSON blobs frequently carry S3 keys or TLS
  material).
- Admin/user passwords and Datadog API keys are preserved across Read —
  the API never returns them, and the provider does NOT overwrite the
  state value with empty.

### Wire-shape correctness

- `databases` and `networks` on `altinity_clickhouse_user` send as
  `array<string>` per the ACM OpenAPI spec. (A comma-string caused ACM's
  PHP backend to emit malformed `GRANT ... ON default.` SQL — empty
  after the dot — which ClickHouse rejected with Code 62.)
- `accessManagement` sends as integer `0`/`1` per the spec's `enum`. On
  Create the field is omitted when `false` so ACM doesn't emit a stray
  REVOKE clause against a user that was never granted access management;
  Update always sends it so the bit can be toggled either way.
- `KeeperLaunchRequest` omits `ha` — ACM auto-determines keeper HA from
  the bound cluster's replica count, and sending `ha: false` would
  either be ignored or downgrade a quorum-needing keeper.

### Tunables

| Env var | Default | Purpose |
| ------- | ------- | ------- |
| `ALTINITYCLOUD_API_TOKEN` | (none) | Fallback for `provider.api_token`. |
| `ALTINITYCLOUD_CLUSTER_SETTLE_DELAY` | `30s` | Wait between cluster-Create convergence and downstream-resource Create. Any `time.ParseDuration` string. CI pipelines running many parallel applies can drop this — downstream Creates have their own transient-race retry budget. Invalid values fall back to the default with a `WARN` log. |

`provider.api_url` defaults to `https://acm.altinity.cloud/api`. The HTTP
client honors `WithHTTPTimeout(d time.Duration)` (default 60s) for tuning
in tests.

### Composite IDs

| Resource | Import ID |
| -------- | --------- |
| `altinity_clickhouse_cluster` | `<cluster_id>` (numeric) |
| `altinity_clickhouse_keeper` | `<environment>:<name>` |
| `altinity_clickhouse_user` | `<cluster_id>:<name>` |
| `altinity_clickhouse_profile` | `<cluster_id>:<name>` |
| `altinity_clickhouse_profile_setting` | `<profile_id>:<name>` |
| `altinity_clickhouse_cluster_setting` | `<cluster_id>:<name>` |

Keeper IDs split on the **first** colon (env id is numeric, so the first
colon is unambiguous). Satellite IDs split on the **last** colon
(defensive — though path-safe-name validators reject `:` in names at plan
time, so it never matters in practice).

### Diagnostics

- Data-source errors append actionable remediation hints for the
  `Unauthorized` and `NotFound` cases.

### Internal

- Composite-ID splitters consolidated into
  `splitCompositeID(id, lastColon)`; per-resource splitters are thin shims.
- Plan modifiers `useStateOrNullString` / `useStateOrNullInt64` live in
  `helpers.go` for any Optional+Computed attribute the API does not echo
  back (otherwise plan diffs as `"+ (known after apply)"` every time).
- OpenAPI-driven wire-type code generation under `tools/specgen`.
- `internal/acm/` is hand-written domain types over generated wire types.

### Outstanding work

Tracked in `README.md` under "Outstanding work":

- Typed nested blocks for `datadog`, `backup_options`, `uptime_settings`,
  `alternate_endpoints` (currently raw JSON passthrough).
- Confirm in-place vs. RequiresReplace paths for `ip_whitelist`,
  `volume_type`, `lb_type`, several port attributes.
- Confirm the full set of terminal status strings recognized by
  `PollUntilIdle` against a live cluster.
