# Environment Datadog + Maintenance Windows Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an optional `datadog {}` block and an optional `maintenance_windows` list to the existing `altinity_environment` resource, both configured via the `EnvironmentEdit` minimal-patch call.

**Architecture:** Additive to `altinity_environment` — no new resource/endpoint. A shared `buildEnvEditRequest(plan)` assembles `displayName` + `datadogSettings` + `applyToClusters` + `maintenanceWindowSchedules` from the model; Create's post-ready follow-up edit and Update both use it. `datadogSettings` decodes object-or-string on Read; `api_key` is Sensitive + write-only (preserved from config). `maintenanceWindowSchedules` isn't on `wire.Environment`, so Read decodes it via an embedding struct; the request uses a `*[]MaintenanceWindow` pointer so `[]`=clear is distinct from null=unmanaged.

**Tech Stack:** Go 1.26, terraform-plugin-framework v1.13, `httptest` tests.

**Spec:** `docs/superpowers/specs/2026-06-10-altinity-environment-datadog-design.md`

**⚠️ Secrets rule:** every new fixture/test value is hand-written synthetic. Never commit the `.context/` captures or any real key (the captures hold real GCP/k8s keys). The pre-commit hook (`make check-secrets`) backstops this.

---

## File Structure

**Modified:**
- `internal/acm/environments.go` — extend `EnvironmentEditRequest` (`DatadogSettings *DatadogSettings`, `ApplyToClusters json.RawMessage`, `MaintenanceWindowSchedules *[]MaintenanceWindow`); add `DatadogSettings`/`MaintenanceWindow` types.
- `internal/acm/domain.go` — domain `Environment` gains `Datadog *DatadogConfig` + `MaintenanceWindows []MaintenanceWindow`; `environmentFromWire` decodes `datadogSettings` (object-or-string); add `datadogConfigFromRaw` helper.
- `internal/acm/environments.go` (or a small `environment_show.go`) — `GetEnvironmentByID` decodes via an `environmentRaw` embedding struct that adds `maintenanceWindowSchedules`.
- `internal/provider/resource_environment.go` — model fields `Datadog *datadogModel`, `MaintenanceWindows types.List`; schema `datadog` SingleNested + `maintenance_windows` ListNested + weekday validator; `buildEnvEditRequest`; Create/Update use it; `applyEnvironmentToModel` merges (preserve `api_key`, map windows).
- `internal/provider/resource_environment_test.go` — new tests.
- `internal/acm/environments_test.go` — client tests for the new edit fields + decode.
- docs/examples.

**No specgen change** (`wire.Environment.DatadogSettings` already exists as `json.RawMessage`; `maintenanceWindowSchedules` handled by the hand-written `environmentRaw`).

---

## Task 1: ACM client — request types + edit fields

**Files:** `internal/acm/environments.go`; test `internal/acm/environments_test.go`

- [ ] **Step 1: failing test** — `EditEnvironment` with datadog + maintenance sends the right body:
```go
func TestEditEnvironment_DatadogAndMaintenance(t *testing.T) {
    var body map[string]any
    client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
        raw, _ := io.ReadAll(r.Body); _ = json.Unmarshal(raw, &body)
        w.Header().Set("Content-Type", "application/json")
        _, _ = w.Write([]byte(`{"data":{"id":"2293","status":"online"}}`))
    })
    empty := []MaintenanceWindow{}
    _, err := client.EditEnvironment(context.Background(), 2293, EnvironmentEditRequest{
        DatadogSettings: &DatadogSettings{Enabled: true, Key: "synthetic-key", Region: "datadoghq.com", Metrics: true, Logs: true, TableStats: true},
        ApplyToClusters: json.RawMessage(`{"datadog":true}`),
        MaintenanceWindowSchedules: &[]MaintenanceWindow{{Name: "w1", Enabled: true, Hour: 16, LengthInHours: 4, Days: []string{"FRIDAY"}}},
    })
    require.NoError(t, err)
    dd := body["datadogSettings"].(map[string]any)
    assert.Equal(t, true, dd["enabled"]); assert.Equal(t, "synthetic-key", dd["key"]); assert.Equal(t, true, dd["tableStats"])
    assert.Equal(t, map[string]any{"datadog": true}, body["applyToClusters"])
    mw := body["maintenanceWindowSchedules"].([]any)
    require.Len(t, mw, 1); assert.Equal(t, float64(16), mw[0].(map[string]any)["hour"])
    _ = empty
}
```

- [ ] **Step 2:** run, verify fail (types undefined).

- [ ] **Step 3: implement** in `environments.go`:
```go
type DatadogSettings struct {
    Enabled    bool   `json:"enabled"`
    Key        string `json:"key"`
    Region     string `json:"region,omitempty"`
    Metrics    bool   `json:"metrics"`
    Logs       bool   `json:"logs"`
    TableStats bool   `json:"tableStats"`
}
type MaintenanceWindow struct {
    Name          string   `json:"name"`
    Enabled       bool     `json:"enabled"`
    Hour          int      `json:"hour"`
    LengthInHours int      `json:"lengthInHours"`
    Days          []string `json:"days"`
}
// EnvironmentEditRequest — extend:
type EnvironmentEditRequest struct {
    DisplayName                string             `json:"displayName,omitempty"`
    DatadogSettings            *DatadogSettings   `json:"datadogSettings,omitempty"`
    ApplyToClusters            json.RawMessage    `json:"applyToClusters,omitempty"`
    MaintenanceWindowSchedules *[]MaintenanceWindow `json:"maintenanceWindowSchedules,omitempty"`
}
```
(Add `encoding/json` import if missing.)

- [ ] **Step 4:** run test, verify PASS.
- [ ] **Step 5:** commit `feat(acm): EnvironmentEdit datadog + maintenance window fields`.

---

## Task 2: ACM domain — Read decode (datadog object-or-string + maintenance via embedding struct)

**Files:** `internal/acm/domain.go`, `internal/acm/environments.go`; test `internal/acm/environments_test.go`

- [ ] **Step 1: failing tests:**
  - `datadogConfigFromRaw` decodes BOTH an object (`{"enabled":true,"region":"datadoghq.com","metrics":true,...}`) and a stringified object (`"{\"enabled\":true,...}"`) to the same `DatadogConfig{Enabled, Region, Metrics, Logs, TableStats}` (NO key).
  - `GetEnvironmentByID` maps `maintenanceWindowSchedules` from a GET fixture into `env.MaintenanceWindows` (a hand-written synthetic fixture).
  - `GetEnvironmentByID` when GET omits `maintenanceWindowSchedules` → `env.MaintenanceWindows` is nil (graceful).

- [ ] **Step 2:** run, verify fail.

- [ ] **Step 3: implement.**
  - domain `Environment` += `Datadog *DatadogConfig` and `MaintenanceWindows []MaintenanceWindow`, where `DatadogConfig{Enabled bool; Region string; Metrics bool; Logs bool; TableStats bool}` (no key — write-only).
  - `datadogConfigFromRaw(raw json.RawMessage) *DatadogConfig`: return nil for empty/`null`; try `json.Unmarshal(raw, &obj)`; if that fails, unmarshal `raw`→string then string→obj; map fields (ignore `key`).
  - In `environmentFromWire`, set `e.Datadog = datadogConfigFromRaw(w.DatadogSettings)`.
  - `GetEnvironmentByID` decodes into a hand-written embedding struct (the `nodeTypeRaw` pattern):
    ```go
    type environmentRaw struct {
        wire.Environment
        MaintenanceWindowSchedules json.RawMessage `json:"maintenanceWindowSchedules"`
    }
    ```
    After `environmentFromWire(&r.Environment)`, if `len(r.MaintenanceWindowSchedules)>0 && != "null"`, `json.Unmarshal` it into `[]MaintenanceWindow` and set `env.MaintenanceWindows`.

- [ ] **Step 4:** run, verify PASS.
- [ ] **Step 5:** commit `feat(acm): decode env datadog (object|string) + maintenance windows`.

---

## Task 3: provider schema — datadog block + maintenance_windows list + validator

**Files:** `internal/provider/resource_environment.go`; test `internal/provider/resource_environment_test.go`

- [ ] **Step 1: failing test** — `environmentSchema(t)` exposes `datadog` (SingleNested, with `api_key` Sensitive) and `maintenance_windows` (ListNested); a plan with an invalid weekday fails validation. (Schema assertions + a ValidateConfig-style check on the weekday validator.)

- [ ] **Step 2:** run, verify fail.

- [ ] **Step 3: implement.**
  - Model:
    ```go
    // on environmentResourceModel:
    Datadog            *datadogModel `tfsdk:"datadog"`
    MaintenanceWindows types.List    `tfsdk:"maintenance_windows"` // of maintenanceWindowModel objects
    type datadogModel struct {
        Enabled         types.Bool   `tfsdk:"enabled"`
        APIKey          types.String `tfsdk:"api_key"`
        Region          types.String `tfsdk:"region"`
        SendMetrics     types.Bool   `tfsdk:"send_metrics"`
        SendLogs        types.Bool   `tfsdk:"send_logs"`
        SendTableStats  types.Bool   `tfsdk:"send_table_stats"`
        ApplyToClusters types.Bool   `tfsdk:"apply_to_clusters"`
    }
    type maintenanceWindowModel struct {
        Name        types.String `tfsdk:"name"`
        Enabled     types.Bool   `tfsdk:"enabled"`
        Hour        types.Int64  `tfsdk:"hour"`
        LengthHours types.Int64  `tfsdk:"length_hours"`
        Days        types.List   `tfsdk:"days"` // string
    }
    func maintenanceWindowAttrTypes() map[string]attr.Type { /* name,enabled,hour,length_hours,days(list string) */ }
    ```
  - Schema: `datadog` `schema.SingleNestedAttribute{Optional:true, Attributes:{enabled Bool Optional+Computed default false; api_key String Optional+Sensitive (Description notes write-only/excluded-from-drift); region String Optional+Computed default "datadoghq.com"; send_metrics/send_logs/send_table_stats Bool Optional+Computed default false; apply_to_clusters Bool Optional+Computed default true}}`. `maintenance_windows` `schema.ListNestedAttribute{Optional:true, NestedObject:{name String Required; enabled Bool Optional+Computed default true; hour Int64 Required; length_hours Int64 Required; days ListAttribute(String) Required with weekdayListValidator}}`.
  - `weekdayValidator` (mirrors `cloudProviderValidator`/`nodeTypeScopeValidator`): a `validator.List` (or per-element string validator) rejecting values outside `{MONDAY..SUNDAY}` (uppercase).
  - Use `booldefault`/`stringdefault`/`booldefault` static defaults where defaults are declared (import `resource/schema/defaults/*`), or `UseStateForUnknown` if simpler — match what the codebase already does.

- [ ] **Step 4:** run, verify PASS.
- [ ] **Step 5:** commit `feat(provider): altinity_environment datadog + maintenance_windows schema`.

---

## Task 4: provider lifecycle — build edit request, Create/Update, Read merge

**Files:** `internal/provider/resource_environment.go`; test `internal/provider/resource_environment_test.go`

- [ ] **Step 1: failing tests** (httptest provider harness, mirror existing env tests):
  - **Create with datadog**: env ready → follow-up `EnvironmentEdit` body has `datadogSettings{enabled,key,...}` + `applyToClusters{datadog:true}`; state keeps `api_key` as configured.
  - **Create with maintenance_windows**: follow-up edit body has `maintenanceWindowSchedules[]` with the window.
  - **Update changing datadog flags**: edit sends updated `datadogSettings`.
  - **Read write-only**: GET returns `datadogSettings` with a DIFFERENT key; state's `api_key` stays the configured value (not overwritten); `enabled`/`region`/`send_*` come from API.
  - **maintenance `[]` vs null**: config `maintenance_windows = []` → edit sends `maintenanceWindowSchedules:[]`; config omitted → field NOT in body.
  - **server validation**: edit returns 400 "must provide ≥ 48h…" → diagnostic surfaces.

- [ ] **Step 2:** run, verify fail.

- [ ] **Step 3: implement.**
  - `buildEnvEditRequest(ctx, plan) (acm.EnvironmentEditRequest, diag.Diagnostics)`:
    - `DisplayName` from plan (as today).
    - If `plan.Datadog != nil`: set `DatadogSettings{Enabled, Key: APIKey, Region, Metrics: SendMetrics, Logs: SendLogs, TableStats: SendTableStats}`; if `ApplyToClusters` true → `req.ApplyToClusters = json.RawMessage('{"datadog":true}')`.
    - If `!plan.MaintenanceWindows.IsNull()`: `ElementsAs` → `[]maintenanceWindowModel` → convert to `[]acm.MaintenanceWindow` (Days via ElementsAs); set `req.MaintenanceWindowSchedules = &slice` (non-nil even when empty → sends `[]`). Null → leave nil (unmanaged).
  - Create: replace the `display_name`-only follow-up with: if `plan.DisplayName` set OR `plan.Datadog != nil` OR `!plan.MaintenanceWindows.IsNull()`, run one `RetryWhileBusy` edit using `buildEnvEditRequest`.
  - Update: replace the inline `EnvironmentEditRequest{DisplayName:...}` with `buildEnvEditRequest`.
  - `applyEnvironmentToModel(m, env)`: keep existing scalar mapping; THEN merge the new fields without clobbering secrets:
    - Datadog: if `env.Datadog != nil`, build/refresh `m.Datadog` non-secret fields (`Enabled`/`Region`/`SendMetrics`/`SendLogs`/`SendTableStats`) but **preserve `m.Datadog.APIKey`** (and `ApplyToClusters`) from the prior model; if `m.Datadog == nil` (unmanaged), leave nil — do not introduce a block the operator didn't declare.
    - Maintenance: if `m.MaintenanceWindows` was declared (non-null in prior state/plan), set it from `env.MaintenanceWindows` (compare days as a set via `stringSlicesEqualUnordered` to avoid spurious diffs); if unmanaged (null), leave null. If GET omits the field (OQ-4), keep the prior model value.
  - Note: `applyEnvironmentToModel` signature may need the prior model as the receiver (it already takes `*environmentResourceModel`, which carries the prior api_key on Read/Update) — preserve, don't overwrite.

- [ ] **Step 4:** run, verify PASS.
- [ ] **Step 5:** commit `feat(provider): wire datadog + maintenance windows into env CRUD`.

---

## Task 5: docs + examples + full verification

**Files:** `examples/resources/altinity_environment/resource.tf` (extend); regenerated `docs/`; whole suite

- [ ] **Step 1:** extend the env example with a `datadog {}` block (api_key via `var`, marked sensitive) and a `maintenance_windows` entry — **synthetic values only**, no real key.
- [ ] **Step 2:** `make docs`; verify `docs/resources/environment.md` shows both, and `api_key` is marked Sensitive + the drift note.
- [ ] **Step 3:** `go test ./...` (all green incl. codegen guard), `go vet ./...`, `gofmt -l` on new/changed files empty.
- [ ] **Step 4:** `make check-secrets` clean (no secret/prod data committed).
- [ ] **Step 5:** commit `docs: examples + docs for env datadog + maintenance windows`.
- [ ] **Step 6 (operator, live, resolves OQ-3/OQ-4):** `terraform apply` datadog + a maintenance window; `GET /environment/<id>` to confirm whether `maintenanceWindowSchedules` is echoed (OQ-4) and `[]`-clear behavior (OQ-3); confirm `applyToClusters` is harmless with zero clusters (OQ-1).

## Testing strategy
- Reuse `httptest` + the env resource test harness. All fixtures hand-written synthetic.
- Highest-value: write-only `api_key` preserved on Read; `[]` vs null maintenance (pointer); datadog object-or-string decode; weekday validator; server 48h-rejection surfaces.

## Open questions (from spec, live-test-resolved)
OQ-1 applyToClusters harmless w/ 0 clusters · OQ-2 always send full datadog block (n/a) · OQ-3 `[]`=clear + `hour` tz · OQ-4 does GET echo maintenanceWindowSchedules (Read reconcile vs preserve). None block; OQ-4 is the one to confirm before finalizing Read.
