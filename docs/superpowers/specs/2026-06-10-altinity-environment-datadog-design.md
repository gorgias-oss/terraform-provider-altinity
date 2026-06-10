# Design: Datadog metrics + maintenance windows on `altinity_environment`

- **Status:** Draft (pending spec review + author sign-off)
- **Date:** 2026-06-10
- **Author:** Vianney Foucault (with Claude)
- **Scope:** Add two environment-config features to the existing
  `altinity_environment` resource — the **Datadog integration** (`datadog {}`
  block) and **maintenance windows** (`maintenance_windows` list). Both ride the
  same `EnvironmentEdit` call the resource already uses for `display_name`; both
  are now backed by live captures (2026-06-10, env 2293).

## 1. Goal

Let operators configure, on `altinity_environment` and declaratively:
1. The environment's **Datadog integration** (ship ClickHouse metrics/logs to
   Datadog) — a single optional nested `datadog {}` block.
2. The environment's **maintenance windows** — an optional `maintenance_windows`
   list.

No new resource, no new endpoint — both ride the `EnvironmentEdit`
(`POST /environment/{id}`) call the resource already uses for `display_name`,
which merges a **minimal patch** server-side (proven by `displayName`-only and
`maintenanceWindowSchedules`-only edits — see §2).

## 2. API findings (live, env 2293, 2026-06-10)

`EnvironmentEdit` (`POST /environment/{id}`) request carries:

```json
"datadogSettings": {"enabled":true,"key":"<API_KEY>","region":"datadoghq.com",
                    "tableStats":true,"logs":true,"metrics":true}
```

and a top-level sibling that propagates the config to clusters:

```json
"applyToClusters": {"datadog": true}
```

Key behaviors:

- **Request vs response asymmetry:** the request sends `datadogSettings` as a
  JSON **object**; the `EnvironmentEdit` *response* echoes it back as a
  **stringified** JSON (`"datadogSettings":"{\"enabled\":true,...}"`). The
  `EnvironmentShow` (GET) returns it as an **object**. The domain layer must
  accept both forms.
- **`key` is the Datadog API key** — a secret. It is sent in full on each edit;
  `EnvironmentShow` returns it. It must be treated **write-only** (never stored
  from the API; preserved from config) and `Sensitive`.
- **`datadogPassword`** (a separate optional "app key") is `null` in the
  capture — out of scope for v1.
- **Partial update is safe:** a minimal `EnvironmentEdit` (we proved this with
  `displayName`-only) merges server-side without clobbering other env fields. So
  we send only `displayName` (existing) + `datadogSettings` + `applyToClusters`.
- **`metricStorage`** is actually an object (`{retentionPeriodInDays}`), not a
  string as the OpenAPI declares — noted but out of scope (this is the Datadog
  path, option A, not the metric-storage path).

Maintenance windows (`EnvironmentEdit`, env 2293, minimal patch):

```json
{"id":"2293","maintenanceWindowSchedules":[
  {"name":"Schedule_1","enabled":true,"hour":16,"lengthInHours":4,
   "days":["FRIDAY","SATURDAY","THURSDAY"]}]}
```
→ response `{"data":{"status":"online","id":"2293"}}`.

- `maintenanceWindowSchedules` is a real **array of objects** (despite the
  OpenAPI declaring it `string`): `{name, enabled, hour (0–23), lengthInHours,
  days:[UPPERCASE weekday]}`. `days` uses full uppercase names
  (`MONDAY`…`SUNDAY`); window order/day order is not semantically meaningful.
- The minimal `{id, maintenanceWindowSchedules}` POST succeeding **confirms the
  partial-merge behavior** the Datadog path relies on.
- **Server-side validation:** ACM rejects schedules that don't provide ≥48h over
  any 32-day window (`maintenanceWindow "…": must provide ≥ 48h over any 32-day
  window`). The provider does NOT recompute this — it surfaces ACM's error as a
  diagnostic. No `applyToClusters` is sent for maintenance windows (the capture
  omits it).

⚠️ **Fixtures:** the captured 26 KB payload contains real secrets (a GCP SA
private key and a k8s client RSA key). Any test fixture for this work MUST be
hand-written synthetic — never derived from the raw capture. (Reinforces the
`check-secrets` pre-commit guard.)

## 3. Schema — `datadog {}` nested block on `altinity_environment`

| Attribute | Type | Mode | Wire (`datadogSettings.*`) |
|---|---|---|---|
| `enabled` | bool | Optional (default false) | `enabled` |
| `api_key` | string | Required-in-block, **Sensitive, write-only** | `key` |
| `region` | string | Optional (default `datadoghq.com`) | `region` |
| `send_metrics` | bool | Optional (default false) | `metrics` |
| `send_logs` | bool | Optional (default false) | `logs` |
| `send_table_stats` | bool | Optional (default false) | `tableStats` |
| `apply_to_clusters` | bool | Optional (default true) | top-level `applyToClusters.datadog` |

- The whole `datadog` block is **Optional**. When absent, the resource does not
  manage `datadogSettings` at all (never sends it).
- `api_key` is `Sensitive`; **write-only** in the codebase's existing sense —
  plain `Optional + Sensitive` with manual state-preservation (NOT the
  terraform-plugin-framework `WriteOnly` attribute flag). Sent on create/update,
  never read back from the API, preserved from config/state — same mechanism as
  cluster `admin_password` / clickhouse_user `password`. Consequently `api_key`
  is **excluded from drift detection** (an out-of-band key change isn't noticed);
  state this in the attribute `Description`, as the user resource's `password`
  does.

## 3a. Schema — `maintenance_windows` list on `altinity_environment`

A `maintenance_windows` optional list of nested objects:

| Attribute | Type | Mode | Wire (`maintenanceWindowSchedules[].*`) |
|---|---|---|---|
| `name` | string | Required-in-object | `name` |
| `enabled` | bool | Optional (default true) | `enabled` |
| `hour` | int | Required-in-object (0–23) | `hour` |
| `length_hours` | int | Required-in-object | `lengthInHours` |
| `days` | list(string) | Required-in-object | `days` (UPPERCASE weekdays) |

- The whole `maintenance_windows` list is **Optional**; when absent, the resource
  does not manage `maintenanceWindowSchedules` (never sends it). An empty list
  `[]` is a meaningful "clear all windows" (sends `maintenanceWindowSchedules:[]`)
  — vs. null = unmanaged. (Confirm `[]` semantics during the live test.)
- `days` values are full uppercase weekday names (`MONDAY`…`SUNDAY`). A plan-time
  validator rejects other values; order is not significant (compare as a set to
  avoid spurious diffs, like `stringSlicesEqualUnordered` does elsewhere).
- Plain (not Sensitive) — no secrets here.
- The ≥48h/32-day rule is **not** enforced client-side; ACM's rejection surfaces
  as a diagnostic.

## 4. Lifecycle integration

The `altinity_environment` resource already has Create/Read/Update/Delete with
`display_name` as the only mutable field. Both features extend Create + Update +
Read the same way (`display_name`'s existing edit path generalizes to "send the
fields the operator configured"):

- **Create:** the request flow (`EnvironmentRequest`) can't set these. So, as with
  `display_name`, if a `datadog {}` block and/or `maintenance_windows` are
  configured, apply them via the **same** post-ready `EnvironmentEdit` follow-up
  (one edit carrying all configured fields).
- **Update:** when `datadog {}` and/or `maintenance_windows` change, send the
  changed field(s) on `EnvironmentEdit` (with `displayName`): `datadogSettings` +
  `applyToClusters` for Datadog, `maintenanceWindowSchedules` for windows.
- **Read:**
  - Datadog → `EnvironmentShow.datadogSettings` → map `enabled`/`region`/`send_*`;
    **do not** read `api_key` back (write-only).
  - Maintenance → `EnvironmentShow.maintenanceWindowSchedules` → map the list
    (`days` compared as a set to avoid spurious diffs).
  - If a block/list is unset in config, leave it null. NOTE:
    `applyEnvironmentToModel` is shared by Create/Read/Update — it must overwrite
    only the non-secret fields and leave the model's existing `api_key` (and any
    null block/list) untouched, never doing a naive full overwrite that wipes the
    configured key.
- **Delete:** unchanged (both configs live on the env; deleting the env removes
  them; no separate teardown).

### 4.1 ACM client + domain changes

- Extend `acm.EnvironmentEditRequest` with:
  - `DatadogSettings *DatadogSettings` (`json:"datadogSettings,omitempty"`), a
    typed struct `{Enabled bool; Key string; Region string; Metrics bool;
    Logs bool; TableStats bool}`.
  - `ApplyToClusters json.RawMessage` (`json:"applyToClusters,omitempty"`).
  - `MaintenanceWindowSchedules *[]MaintenanceWindow` (`json:"maintenanceWindowSchedules,omitempty"`),
    where `MaintenanceWindow` is `{Name string; Enabled bool; Hour int;
    LengthInHours int; Days []string}`. **Pointer**, not a plain slice:
    Go's `json` `omitempty` drops *any* `len==0` slice (nil or not), so a plain
    slice can't distinguish "unmanaged" from "clear all". A `nil` pointer omits
    the field (unmanaged); a non-nil pointer to an empty slice marshals `[]`
    (clear). The resource sets the pointer only when `maintenance_windows` is
    non-null in config.
  - `DatadogSettings`/`ApplyToClusters` use `omitempty` (unmanaged → not sent).
- Extend the domain `Environment` (and `environmentFromWire`) to surface both
  configs read from `EnvironmentShow`:
  - `datadogSettings` IS on `wire.Environment` (`json.RawMessage`). It arrives as
    **object OR string**, so decode via a small helper (unmarshal as object; if
    that fails, unmarshal the string then the object). Do **not** carry the API
    key into the domain (write-only).
  - `maintenanceWindowSchedules` is **NOT** a field on `wire.Environment` (only
    `datadogSettings` is). So Read decodes it via a hand-written struct that
    **embeds `wire.Environment`** and adds
    `MaintenanceWindowSchedules json.RawMessage` — the same pattern as
    `nodeTypeRaw` (which adds `used`/`*_alloc` to `wire.NodeType`). `GetEnvironmentByID`
    decodes into that struct. No specgen change.
  - **UNCONFIRMED (OQ-4):** the captures are `EnvironmentEdit` *requests*; we have
    not verified that `EnvironmentShow` (GET) echoes `maintenanceWindowSchedules`.
    If it does NOT, maintenance windows are config-preserved (like a write-only
    field — keep the configured value, don't reconcile from the API). The Read
    mapping must handle "field absent in GET" gracefully either way.

## 5. Testing

- `acm` unit tests: `EnvironmentEdit` body includes `datadogSettings` (exact
  fields) + `applyToClusters:{datadog:true}`; `maintenanceWindowSchedules` array
  shape; the string-or-object datadog decode helper handles both response forms.
- `provider` tests: schema/plan validation (incl. the weekday validator);
  Create-with-datadog/-windows issues the follow-up edit; Update sends the
  changed settings; Read maps non-secret datadog fields + the windows list and
  leaves `api_key` as the configured value; block/list absent → not sent; a
  server-side maintenance rejection (≥48h rule) surfaces as a diagnostic.
- Fixtures: hand-written synthetic (no real keys) — the captures contained real
  GCP/k8s secrets.

## 6. Out of scope / deferred

- `datadogPassword` (Datadog app key), `metricStorage`, `logsStorage`,
  `logsOptions` — not part of this work (option A was Datadog metrics).

## 7. Open questions

- **OQ-1** Is `applyToClusters:{datadog:true}` required for the config to take
  effect, or only to push to existing clusters? Default to sending it (mirrors
  the UI); confirm it's harmless when there are no clusters.
- **OQ-2** Does omitting `datadogSettings.key` (sending the rest) keep the
  existing key, or wipe it? The UI sends the key in full every time; the resource
  will too (write-only value from config), so this only matters if we ever want
  "update flags without re-sending the key" — out of scope (we always send the
  full block).

- **OQ-3** Maintenance windows: does sending `maintenanceWindowSchedules:[]`
  clear all windows (vs. null = leave unmanaged)? And what timezone is `hour`
  in (UTC assumed)? Confirm during the live test; the design treats `[]` as
  "clear" and null as "unmanaged".
- **OQ-4** Does `EnvironmentShow` (GET) return `maintenanceWindowSchedules`? If
  not, treat the list as config-preserved (write-only-style). Confirm with a GET
  after setting a window (read-only — capture it during the live test).

OQ-1/OQ-2/OQ-3 don't block implementation. OQ-4 affects the maintenance Read
mapping (reconcile-from-API vs config-preserve) — the implementation handles
"absent in GET" gracefully, so it doesn't block either, but confirm before
finalizing the Read behavior. Also note: the existing `Update` runs unbounded on
the parent context (only `Create` wraps a timeout); since these edits ride one
`EnvironmentEdit` that's acceptable, but the plan should decide whether to bound
`Update` by the update timeout for consistency.
