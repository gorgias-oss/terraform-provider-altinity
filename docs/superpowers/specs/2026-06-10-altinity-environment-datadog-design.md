# Design: Datadog metrics configuration on `altinity_environment`

- **Status:** Draft (pending spec review + author sign-off)
- **Date:** 2026-06-10
- **Author:** Vianney Foucault (with Claude)
- **Scope:** Add Datadog integration config to the existing `altinity_environment`
  resource. **Maintenance windows are deferred** to a separate cycle (their
  `EnvironmentEdit` payload shape — `maintenanceWindowSchedules` — has not been
  captured yet).

## 1. Goal

Let operators configure an environment's **Datadog integration** (ship
ClickHouse metrics/logs to Datadog) declaratively on `altinity_environment`, via
a single optional nested `datadog {}` block. No new resource, no new endpoint —
it rides on the `EnvironmentEdit` call the resource already uses for
`display_name`.

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
- `api_key` is `Sensitive`; **write-only**: sent on create/update, never read
  back from the API, preserved from config/state — same pattern as cluster
  `admin_password` / clickhouse_user `password`.

## 4. Lifecycle integration

The `altinity_environment` resource already has Create/Read/Update/Delete with
`display_name` as the only mutable field. Datadog extends Create + Update + Read:

- **Create:** the request flow (`EnvironmentRequest`) can't set Datadog. So, as
  with `display_name`, if a `datadog {}` block is configured, apply it via an
  `EnvironmentEdit` follow-up after the environment is ready (reuse/extend the
  existing post-create edit path).
- **Update:** when the `datadog {}` block changes, send `datadogSettings` +
  `applyToClusters` on `EnvironmentEdit` (together with `displayName`).
- **Read:** `EnvironmentShow.datadogSettings` → map `enabled`/`region`/`send_*`
  into the block; **do not** read `api_key` back (write-only). If the block is
  unset in config, leave it null.
- **Delete:** unchanged (Datadog config lives on the env; deleting the env
  removes it; no separate teardown).

### 4.1 ACM client + domain changes

- Extend `acm.EnvironmentEditRequest` with `DatadogSettings *DatadogSettings`
  (`json:"datadogSettings,omitempty"`) and `ApplyToClusters json.RawMessage`
  (`json:"applyToClusters,omitempty"`), where `DatadogSettings` is a typed
  struct `{Enabled bool; Key string; Region string; Metrics bool; Logs bool;
  TableStats bool}`.
- Extend the domain `Environment` (and `environmentFromWire`) to surface the
  Datadog config read from `EnvironmentShow`. Because `datadogSettings` arrives
  as **object OR string**, decode via a small helper (`json.RawMessage` →
  unmarshal as object; if that fails, unmarshal the string then the object).
  Do **not** carry the API key into the domain (write-only).
- `wire.Environment.DatadogSettings` is already `json.RawMessage` (opaque) — the
  coercion is hand-written in the domain layer (no specgen change needed).

## 5. Testing

- `acm` unit tests: `EnvironmentEdit` body includes `datadogSettings` (exact
  fields) + `applyToClusters:{datadog:true}`; the string-or-object decode helper
  handles both response forms.
- `provider` tests: schema/plan validation; Create-with-datadog issues the
  follow-up edit; Update sends the changed settings; Read maps `enabled`/`region`/
  `send_*` and leaves `api_key` as the configured (write-only) value; block-absent
  → no `datadogSettings` sent.
- Fixtures: hand-written synthetic `datadogSettings` (no real keys).

## 6. Out of scope / deferred

- **Maintenance windows** (`maintenanceWindowSchedules`) — separate spec; needs a
  UI capture of that edit payload.
- `datadogPassword` (Datadog app key), `metricStorage`, `logsStorage`,
  `logsOptions` — not part of "Datadog metrics" (option A).

## 7. Open questions

- **OQ-1** Is `applyToClusters:{datadog:true}` required for the config to take
  effect, or only to push to existing clusters? Default to sending it (mirrors
  the UI); confirm it's harmless when there are no clusters.
- **OQ-2** Does omitting `datadogSettings.key` (sending the rest) keep the
  existing key, or wipe it? The UI sends the key in full every time; the resource
  will too (write-only value from config), so this only matters if we ever want
  "update flags without re-sending the key" — out of scope (we always send the
  full block).

Neither blocks implementation; both are confirmable during the live test.
