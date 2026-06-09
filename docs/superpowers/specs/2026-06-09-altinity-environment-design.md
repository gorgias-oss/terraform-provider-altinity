# Design: `altinity_environment` resource + `altinity_regions` data source

- **Status:** Draft (pending spec review + author sign-off)
- **Date:** 2026-06-09
- **Author:** Vianney Foucault (with Claude)
- **Scope:** First sub-project of "support Altinity.Cloud environment and account creation".
  Approach 2 (decompose): **environment first**, account second. The
  `altinity_account` resource and `altinity_account_roles` data source are a
  separate spec cycle and are out of scope here.

## 1. Goal

Add the ability to **create and manage an Altinity.Cloud environment** — the
region-scoped unit in ACM that ClickHouse clusters are launched into — via
Terraform, plus a companion data source for discovering valid regions per cloud
provider.

Deliverables:

1. `altinity_environment` **resource** — full lifecycle (create→wait, read,
   update, delete), resumable across applies.
2. `altinity_regions` **data source** — list `{code, name}` regions for a given
   cloud provider.
3. The shared wire/ACM-client/domain plumbing for both.

## 2. Context: how this provider is built

The provider has a clean three-layer structure that this work follows exactly:

- **`internal/acm/wire`** — code-generated wire types. `tools/specgen` reads the
  vendored OpenAPI doc (`internal/acm/wire/reference.json`) and emits
  `endpoints_gen.go` (operation registry) and `models_gen.go` (faithful structs)
  for an **explicit allowlist** of operationIds (`allowedOps`) and schemas
  (`allowedSchemas`). A `codegen_guard_test` asserts `go generate ./...` produces
  no diff. Generated files **must not** be hand-edited.
- **`internal/acm`** — hand-written client methods + domain types. `domain.go`
  coerces the loosely-typed wire format (string-ints, `0|1` bools) into clean Go
  types. `poll.go` provides the polling toolkit. `errors.go` provides
  `IsNotFound`/`IsUnauthorized`/`IsOperationInProgress`/`IsTransientCreateRace`.
- **`internal/provider`** — terraform-plugin-framework resources & data sources.
  Protocol v6, floored at Terraform 1.5.7 / OpenTofu; deliberately avoids
  post-1.5 features (provider-defined functions, ephemeral resources, write-only
  attributes).

Existing reusable building blocks this design leans on:

- `Environment` domain type + `environmentFromWire` already exist
  (`domain.go:211`, `domain.go:353`) — today only consumed by the environment
  *data source* and the auth preflight.
- `GetEnvironmentByName` / `ListEnvironments` already exist (`environments.go`).
- `PollUntilHealthy`, `PollUntilGoneBy`, `RetryWhileBusy` already exist
  (`poll.go`).
- Idempotent **adopt-by-name** create pattern already exists for users
  (`FindUserByName`, `users.go:46`).
- Generic cloud-options client `ListCloudOptions` (`cloud.go:32`) already calls
  the env-scoped `CloudOptions` with `?type=versions|zones|...` and decodes
  `[{code,name}]`.

## 3. API surface

ACM has **no generic "create environment"**. Provisioning of an Altinity-hosted
environment goes through the *request* flow.

| Purpose | Method / path | operationId | In allowlist today? |
|---|---|---|---|
| Create (request) | `POST /environments/request` | `EnvironmentRequest` | No → add |
| Read | `GET /environment/{id}` | `EnvironmentShow` | No → add |
| Update | `POST /environment/{id}` | `EnvironmentEdit` | No → add |
| Delete | `DELETE /environment/{id}` | `EnvironmentRemove` | No → add |
| Adopt-by-name / resume | `GET /environments` | `EnvironmentList` | Yes |
| Delete pre-flight (cluster check) | `GET /environment/{environment}/clusters` | `ClusterList` | Yes |
| Regions data source | `GET /cloud/options` | `CloudOptionsGlobal` | No → add |

`EnvironmentRequest` request body (from `reference.json`): `name`,
`cloud_provider`, `aws_region`, `gcp_region`, `azure_region`, `hcloud_region`,
`first`. The provider sends `name`, `cloud_provider`, and exactly one
`*_region` field selected by `cloud_provider`. `first` is undocumented — see
Open Question OQ-2.

`CloudOptionsGlobal` is the **non-environment-scoped** variant of the options
endpoint the provider already uses; it is the correct one here because at
environment-creation time no environment exists yet. Query params: `type`,
`platform`, `provider`, `region`.

## 4. `altinity_environment` resource

### 4.1 Schema

| Attribute | Type | Mode | Notes |
|---|---|---|---|
| `name` | string | Required, **ForceNew** | Config-stable key → `EnvironmentRequest.name`; also the adopt-by-name key |
| `cloud_provider` | string | Required, **ForceNew** | One of `aws`, `gcp`, `azure`, `hcloud` (validated) |
| `region` | string | Required, **ForceNew** | Provider routes to the matching `*_region` wire field |
| `display_name` | string | Optional | Only updatable attribute → `EnvironmentEdit` |
| `id` | string | Computed | ACM environment id |
| `normalized_name` | string | Computed | From `EnvironmentShow` |
| `type` | string | Computed | From `EnvironmentShow` |
| `domain` | string | Computed | From `EnvironmentShow` |
| `status` | string | Computed | From `EnvironmentShow` |
| `state` | string | Computed | From `EnvironmentShow` |
| `timeouts` | block | Optional | `create` / `delete` |

Design decisions:

- **Single `cloud_provider` + `region`** rather than four mutually-exclusive
  `*_region` attributes — the provider maps to the right wire field. `region`
  is validated against `altinity_regions` only at apply time (no static enum).
- **Everything except `display_name` is ForceNew.** `name`/`cloud_provider`/
  `region` cannot change without re-provisioning, so a change replaces the
  environment.

### 4.2 Out of scope (v1)

- **Node types** (`NodeTypeAdd`). An environment with zero node types cannot
  launch a cluster yet; node-type management is a separate follow-up resource
  (`altinity_node_type`). A node-types *data source* already exists.
- Keepers, clusters (already separate resources).
- The ~50 other `EnvironmentEdit` knobs (domain, DNS, certs, BYOC creds,
  monitoring, backup options, …). BYOC / connect-your-own-cloud
  (`EnvironmentConnectTo`) is explicitly **not** modeled — this resource is the
  Altinity-hosted request flow only.

### 4.3 Lifecycle

**Create — resumable, adopt-by-name, wait-until-ready:**

1. **Adopt-by-name first.** Call `GetEnvironmentByName(name)`.
   - **Found** → treat as a *resumed* create (a prior apply timed out mid-poll,
     or the env already exists): skip `EnvironmentRequest`, take its id, go to
     step 4 (poll).
   - **Not found** → proceed to step 2.
2. Map `cloud_provider` + `region` → `EnvironmentRequest` body and
   `POST /environments/request`.
3. **Resolve the new id** — from the request response if it returns one
   (OQ-1), otherwise fall back to `GetEnvironmentByName(name)`.
4. **Poll `EnvironmentShow` until ready** on the existing 15s `PollUntilHealthy`
   loop, bounded by the `create` timeout. Mutating calls are wrapped in
   `RetryWhileBusy` for ACM's per-environment "operation in progress" lock.
5. **Ready** → write full state, success.
6. **Timeout / context cancel** → return an error **and deliberately do not save
   state** (see §4.4).

**Read** — `EnvironmentShow` by id → map computed fields via
`environmentFromWire`. `IsNotFound` (404) → remove from state (drift).

**Update** — only `display_name` differs in practice. `EnvironmentEdit`
(`POST /environment/{id}`) with the new display name, followed by a short
`RetryWhileBusy`/`PollUntilHealthy` settle. All other attributes are ForceNew so
no other update path exists.

**Delete — guarded, no cascade:**

1. **Pre-flight cluster check** via `ClusterList` for the environment.
2. **Any clusters remain** → return an error diagnostic naming them (e.g.
   *"environment 'prod-eu' still contains 2 clusters [foo, bar]; destroy them
   before destroying the environment"*) and **abort**. The provider never
   deletes clusters on the user's behalf.
3. **Empty** → `EnvironmentRemove`, then `PollUntilGoneBy` (list-based existence
   check) bounded by the `delete` timeout. Absent → success.

**Import** — `terraform import altinity_environment.x <id>`;
`ImportStatePassthroughID` on `id`, then Read populates state.

### 4.4 Resumability (core design property)

Environment provisioning is slow (cloud k8s control plane + node groups + LB +
DNS), so the `create` poll **will** sometimes exceed its timeout. The resource
is designed so a timed-out apply is **resumed**, not destroyed.

Terraform-plugin-framework behavior this relies on:

- Create returns error **with** state saved → resource **tainted** → next apply
  **destroy+recreate**. ❌ Would tear down the in-flight environment.
- Create returns error **without** saving state → Terraform has no record → next
  apply **calls Create again from scratch**. ✅

Therefore on timeout/cancel, Create returns an error and **does not set state**.
The environment continues provisioning in ACM but is untracked in TF state. The
next `terraform apply` re-enters Create, **adopts the env by name** (step 1),
and **resumes polling** on the same environment — no second `EnvironmentRequest`,
no duplicate, no destroy.

- Within a single apply: dependents still order correctly (we poll to ready).
- Across applies: the wait resumes.

**Accepted trade-off (documented in resource docs):** adopt-by-name means an
unmanaged environment with a colliding `name` would be adopted. This is the same
trade-off the existing user resource makes (`FindUserByName`).

### 4.5 Timeouts

| Timeout | Default | Rationale |
|---|---|---|
| `create` | **45m** | Covers typical cloud-k8s provisioning in one apply; resume covers the long tail |
| `delete` | **30m** | Teardown of node groups + LB + DNS |

Operator-overridable via the `timeouts` block. The live-capture task (§6)
measures a real provisioning wall-clock so the `create` default can be tuned to
observed p90 rather than a guess.

### 4.6 Status handling

`poll.go` notes that the environment terminal-status strings are **not**
spike-confirmed (only cluster/keeper are). The poll is therefore **capture-driven**:
a small known healthy-set and error-set are populated from the live capture
(§6). Unknown statuses are logged as "still provisioning" rather than guessed, so
an unexpected ACM status string never causes a false success or a false failure.

## 5. `altinity_regions` data source

| Field | Type | Mode | Notes |
|---|---|---|---|
| `cloud_provider` | string | Required | `aws`/`gcp`/`azure`/`hcloud` |
| `regions` | list(object{code, name}) | Computed | From `CloudOptionsGlobal` |

New ACM client method `ListCloudOptionsGlobal(ctx, provider, optType)` calling
`CloudOptionsGlobal` (`GET /cloud/options`), decoding the same `[{code,name}]`
shape as `ListCloudOptions`. Exact `type` value and `provider` vs `platform`
keying are confirmed by the capture (OQ-3).

Usage pattern: `data.altinity_regions.aws.regions[*].code` → feeds
`altinity_environment.region`.

## 6. Spike / live-capture plan

Run against the live ACM instance during implementation. Items 1–2 and 4 are
read-only; item 3 provisions billable infra and is triggered **only by the
operator**, never automatically.

1. `GET /cloud/options?type=regions&provider=aws` (+ gcp/azure/hcloud) — confirm
   exact `type` value, `provider` vs `platform` keying, `[{code,name}]` shape →
   finalizes `altinity_regions` and resolves **OQ-3**.
2. `GET /environments` and `GET /environment/{id}` — confirm `EnvironmentShow`
   field set against `environmentFromWire`.
3. **One real `EnvironmentRequest`** — capture: response body (does it return the
   id? → **OQ-1**), the effect of `first` (**OQ-2**), the **status string
   sequence** provisioning→ready and any **terminal-error** string (→ §4.6
   healthy/error sets), and **wall-clock provisioning time** (→ tune §4.5
   `create`). Confirms whether `EnvironmentRequest` needs org/billing context
   beyond name+provider+region (**OQ-4**).
4. `DELETE /environment/{id}` on an empty env — confirm teardown and the "gone"
   signal `PollUntilGoneBy` keys on.

Captured payloads become `testdata/*.json` fixtures (matching
`internal/acm/testdata/environments.json`).

## 7. Open questions

- **OQ-1** Does `EnvironmentRequest` return the new environment id in its
  response, or must Create resolve it via adopt-by-name? (Resolved by capture 3;
  design works either way.)
- **OQ-2** What does the `first` field do? Default behavior if omitted?
- **OQ-3** Exact `type` value (`regions` vs `region`) and `provider` vs
  `platform` keying for `CloudOptionsGlobal`.
- **OQ-4** Does `EnvironmentRequest` require org/billing context beyond
  name+provider+region?
- **OQ-5** Environment terminal-healthy and terminal-error status strings
  (feeds §4.6).

None block writing the implementation plan; all are resolved by the §6 captures
before the affected code is finalized, and the design degrades gracefully
(capture-driven status, adopt-by-name id resolution).

## 8. Implementation outline (for the plan phase)

1. **specgen allowlist** — add `EnvironmentRequest`, `EnvironmentShow`,
   `EnvironmentEdit`, `EnvironmentRemove`, `CloudOptionsGlobal` to `allowedOps`;
   confirm `Environment` schema already in `allowedSchemas`. Run
   `go generate ./...`; keep `codegen_guard_test` green.
2. **ACM client** — extend `environments.go`: `RequestEnvironment`,
   `GetEnvironmentByID`, `EditEnvironment`, `RemoveEnvironment`; add
   `ListCloudOptionsGlobal` to `cloud.go`. Domain `Environment` already exists.
3. **Provider** — `resource_environment.go` (`altinity_environment`),
   `data_source_regions.go` (`altinity_regions`); register `NewEnvironmentResource`
   and `NewRegionsDataSource` in `provider.go` (§ lines 204–236).
4. **Tests** — `acm` unit tests (request mapping, adopt-by-name resume, delete
   pre-flight refusal, status coercion) + `provider` tests (schema/plan, create-
   poll happy path, resume-after-timeout, delete-with-clusters refusal, import).
5. **Docs & examples** — `docs/resources/environment.md`,
   `docs/data-sources/regions.md` (repo doc-gen workflow);
   `examples/resources/altinity_environment/`,
   `examples/data-sources/altinity_regions/`.

## 9. Testing strategy

Mirrors the existing `*_test.go` + mock-server (`httptest`) style and JSON
fixtures. Key cases: provider→region field mapping; adopt-by-name resume path;
poll timeout leaves no state; delete refuses with remaining clusters; import;
codegen freshness guard after allowlist additions.
