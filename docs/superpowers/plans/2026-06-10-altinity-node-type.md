# Node Type Management Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `altinity_node_type` (resource), `altinity_instance_types` (new data source), and `used`/`capacity` to the existing `altinity_node_types` data source, so an environment's node types can be discovered and managed in Terraform.

**Architecture:** Same three-layer split as the environment work — extend the specgen allowlist + regenerate, hand-written `acm` client/domain, terraform-plugin-framework resource/data sources. Node-type create/edit responses carry fields not in the OpenAPI schema (`used`, `cpu_alloc`, `memory_alloc`), so decode them with a hand-written struct that embeds `wire.NodeType`. Tolerations/nodeSelector/extraSpec are not operator-configurable: the resource mirrors the ACM UI's scope-default tolerations on create and preserves existing values on update.

**Tech Stack:** Go 1.26, terraform-plugin-framework v1.13, `httptest` tests, `tools/specgen` codegen.

**Spec:** `docs/superpowers/specs/2026-06-10-altinity-node-type-design.md`

---

## File Structure

**Created:**
- `internal/acm/nodetype_requests.go` — `NodeTypeRequest`, response struct, scope-default tolerations, Create/Edit/Remove/FindByCode client methods.
- `internal/acm/instance_types.go` — `ListInstanceTypes` + `InstanceType` domain type.
- `internal/provider/data_source_instance_types.go` — `altinity_instance_types`.
- `internal/provider/resource_node_type.go` — `altinity_node_type`.
- `internal/acm/testdata/nodetypes_withused.json`, `instance_types.json`, `nodetype_create_response.json` — captured fixtures.
- `docs/resources/node_type.md`, `docs/data-sources/instance_types.md`, `examples/resources/altinity_node_type/resource.tf`, `examples/data-sources/altinity_instance_types/data-source.tf`.

**Modified:**
- `tools/specgen/main.go` + `internal/acm/wire/codegen_guard_test.go` — allowlist `NodeTypeAdd`/`NodeTypeEdit`/`NodeTypeRemove`; regenerate `*_gen.go`.
- `internal/acm/nodetypes.go` — `ListNodeTypes` switches to `doRequest` with `?withUsed=1`; decode via the embedding struct.
- `internal/acm/domain.go` — extend `NodeType` with `Used bool` and raw passthrough `Tolerations`/`NodeSelector`/`ExtraSpec` (+ `CPUAlloc`/`MemoryAlloc` if OQ-3 needs them).
- `internal/provider/data_source_node_types.go` — surface `id`, `used`, `capacity`.
- `internal/provider/provider.go` — register `NewInstanceTypesDataSource` + `NewNodeTypeResource`.
- `internal/provider/provider_test.go` — bump the data-source (8→9) and resource (7→8) registration counts + names.

---

## Task 0: Capture zookeeper-scope toleration default (OQ-1, operator-run)

**Read-only — resolves the one inferred value.**

- [ ] **Step 1:** In the ACM UI for env 2293, add a **zookeeper-scope** node type and capture the `POST /environment/2293/nodetypes` payload's `tolerations`. Record whether it is `[{dedicated=zookeeper:NoSchedule}]` (inferred) or empty.
- [ ] **Step 2:** Record the finding in the spec §8 OQ-1 and use it for the `scopeDefaultTolerations` map (Task 2). If unavailable, implement clickhouse (confirmed) + system (confirmed empty) and default zookeeper to `dedicated=zookeeper:NoSchedule` with a `TODO(spike)` note.

---

## Task 1: Allowlist node-type write ops + regenerate

**Files:** `tools/specgen/main.go:53`, `internal/acm/wire/codegen_guard_test.go:18`, regenerated `*_gen.go`

- [ ] **Step 1:** Add to `allowedOps` (under a node-type comment):
  ```go
  "NodeTypeAdd",    // POST   /environment/{environment}/nodetypes
  "NodeTypeEdit",   // POST   /nodetype/{id}
  "NodeTypeRemove", // DELETE /nodetype/{id}
  ```
  `NodeTypeList` and `CloudOptionsGlobal` are already allowlisted; `NodeType` schema already emitted.
- [ ] **Step 2:** Mirror the three ops in `allowlistForGuard` (the guard asserts `len == len(Endpoints)`).
- [ ] **Step 3:** `go generate ./...` — expect `OpNodeTypeAdd`/`OpNodeTypeEdit`/`OpNodeTypeRemove` constants + registry entries; `models_gen.go` unchanged.
- [ ] **Step 4:** `go test ./internal/acm/wire/ -run 'TestAllowlistedOpsResolveInSpec|TestGenerateNoDiff'` → PASS.
- [ ] **Step 5:** `go build ./...` → success.
- [ ] **Step 6:** Commit: `feat(wire): allowlist NodeType{Add,Edit,Remove}`.

---

## Task 2: ACM client — node type CRUD + domain extension

**Files:** Create `internal/acm/nodetype_requests.go`; modify `internal/acm/nodetypes.go`, `internal/acm/domain.go`; test `internal/acm/nodetypes_test.go`

- [ ] **Step 1: Failing tests** (httptest, using captured fixtures):
  - `ListNodeTypes` sends `?withUsed=1` and decodes `used`/`capacity`/`id`.
  - `CreateNodeType` for `scope=clickhouse` sends `tolerations:[{dedicated=clickhouse:NoSchedule}]`, `nodeSelector:""`, `extraSpec:""`, and the sizing fields; returns the created node type with its id.
  - `CreateNodeType` for `scope=system` sends `tolerations:[]`.
  - `EditNodeType` round-trips the preserved opaque fields unchanged (pass a node type whose tolerations differ from the scope default; assert the edit body carries them verbatim).
  - `RemoveNodeType` issues `DELETE /nodetype/{id}` and treats `{}` as success; a 4xx "in use" surfaces as an error.
  - `FindNodeTypeByCode(env, scope, code)` returns the match + found bool.

- [ ] **Step 2:** Run, verify fail.

- [ ] **Step 3: Implement.**
  - In `domain.go`, extend `NodeType`:
    ```go
    Used        bool
    // Opaque passthrough — preserved on update, never surfaced to Terraform.
    Tolerations  json.RawMessage
    NodeSelector json.RawMessage
    ExtraSpec    json.RawMessage
    ```
    and set them in `nodeTypeFromWire` (Tolerations/NodeSelector/ExtraSpec from the wire raw fields; `Used` cannot come from `wire.NodeType` — see the embedding struct below).
  - In `nodetypes.go`, decode via a hand-written struct (the spec response carries non-schema fields):
    ```go
    // nodeTypeRaw decodes a node type response, including the fields ACM returns
    // that are absent from the OpenAPI NodeType schema (used, *_alloc).
    type nodeTypeRaw struct {
        wire.NodeType
        Used        wire.Bool   `json:"used"`
        CPUAlloc    wire.Number `json:"cpu_alloc"`
        MemoryAlloc wire.Number `json:"memory_alloc"`
    }
    ```
    Add `nodeTypeFromRaw(&r)` that builds the domain `NodeType` (calls `nodeTypeFromWire(&r.NodeType)` then sets `Used = r.Used.Bool()`).
  - Switch `ListNodeTypes` to `doRequest` with `url.Values{"withUsed":{"1"}}`, decoding `[]nodeTypeRaw`.
  - `nodetype_requests.go`:
    ```go
    type Toleration struct {
        Key, Operator, Value, Effect string
    }
    // NodeTypeRequest is the create/edit body. Opaque fields are json.RawMessage
    // so they can be sent verbatim (scope defaults on create, preserved on update).
    type NodeTypeRequest struct {
        Name         string          `json:"name,omitempty"`
        Scope        string          `json:"scope,omitempty"`
        Code         string          `json:"code,omitempty"`
        CPU          float64         `json:"cpu"`
        Memory       int64           `json:"memory"`
        Capacity     int64           `json:"capacity,omitempty"`
        StorageClass string          `json:"storageClass"`
        IsSpot       bool            `json:"isSpot"`
        Tolerations  json.RawMessage `json:"tolerations,omitempty"`
        NodeSelector json.RawMessage `json:"nodeSelector,omitempty"`
        ExtraSpec    json.RawMessage `json:"extraSpec,omitempty"`
    }
    // scopeDefaultTolerations mirrors the ACM UI's per-scope tolerations on create.
    func scopeDefaultTolerations(scope string) json.RawMessage {
        switch scope {
        case "clickhouse":
            return json.RawMessage(`[{"key":"dedicated","operator":"Equal","value":"clickhouse","effect":"NoSchedule"}]`)
        case "zookeeper":
            return json.RawMessage(`[{"key":"dedicated","operator":"Equal","value":"zookeeper","effect":"NoSchedule"}]`) // OQ-1
        default: // system
            return json.RawMessage(`[]`)
        }
    }
    func (c *Client) CreateNodeType(ctx, environmentID string, req NodeTypeRequest) (NodeType, error) // POST .../nodetypes
    func (c *Client) EditNodeType(ctx, id int64, req NodeTypeRequest) (NodeType, error)               // POST /nodetype/{id}
    func (c *Client) RemoveNodeType(ctx, id int64) error                                              // DELETE /nodetype/{id}
    func (c *Client) FindNodeTypeByCode(ctx, environmentID, scope, code string) (NodeType, bool, error) // via ListNodeTypes
    ```
    Send `nodeSelector`/`extraSpec` as `""` on create (mirror UI). `CreateNodeType`/`EditNodeType` decode the response via `nodeTypeRaw`.

- [ ] **Step 4:** Run tests, verify PASS.
- [ ] **Step 5:** Commit: `feat(acm): node type create/edit/remove + used decode`.

---

## Task 3: ACM client — `ListInstanceTypes`

**Files:** Create `internal/acm/instance_types.go`; test `internal/acm/instance_types_test.go`; fixture `testdata/instance_types.json`

- [ ] **Step 1: Failing test** — serve the captured `{data:{zones,instanceTypes}}` fixture; assert `ListInstanceTypes(ctx,"gcp","us-east1")` returns the zones + a slice of `{Name, CPU, CPUAllocatable, Memory, MemoryAllocatable}`, and that the request is `/cloud/options?platform=gcp&region=us-east1&type=*`.

- [ ] **Step 2:** Run, verify fail.

- [ ] **Step 3: Implement.** Use `doRequest(wire.OpCloudOptionsGlobal, nil, query, nil, &out)` where `out` decodes the nested object:
  ```go
  type InstanceType struct {
      Name             string
      CPU              float64
      CPUAllocatable   float64
      Memory           float64 // GiB ("mem")
      MemoryAllocatable float64
  }
  // response: {"data":{"zones":[...],"instanceTypes":[{"name","cpu","cpuAllocatable","mem","memAllocatable"}]}}
  func (c *Client) ListInstanceTypes(ctx, provider, region string) (zones []string, types []InstanceType, err error)
  ```
  Query: `type=*`, `platform=<provider>`, `region=<region>`. Decode `mem`→Memory.
  **NOTE:** this endpoint keys on **`platform=`** (per the live capture), NOT
  `provider=` like the existing `ListCloudOptionsGlobal` regions call — do not
  copy that method's `provider=` key. `out` decodes the unwrapped `data` as a
  struct `{Zones []string; InstanceTypes []InstanceType}`.

- [ ] **Step 4:** Run, verify PASS.
- [ ] **Step 5:** Commit: `feat(acm): add ListInstanceTypes (available instance catalog)`.

---

## Task 4: Enhance `altinity_node_types` data source

**Files:** `internal/provider/data_source_node_types.go`; `internal/provider/data_source_node_types_test.go`

- [ ] **Step 1: Failing test** — extend the existing test: assert each item now exposes `id`, `used`, `capacity`.
- [ ] **Step 2:** Run, verify fail.
- [ ] **Step 3: Implement** — add `id` (`node_type_id` raw or `id`), `used` (bool), `capacity` (int) to `nodeTypeItemModel` + schema; map from the domain `NodeType` (now carrying `Used`/`Capacity`/`ID`). No new endpoint (ListNodeTypes already sends `withUsed`).
- [ ] **Step 4:** Run, verify PASS.
- [ ] **Step 5:** Commit: `feat(provider): surface used/capacity/id on altinity_node_types`.

---

## Task 5: `altinity_instance_types` data source

**Files:** Create `internal/provider/data_source_instance_types.go`; modify `provider.go`; test `data_source_instance_types_test.go`

- [ ] **Step 1: Failing test** — mirror `data_source_regions_test.go`: serve the instance-types fixture; assert `cloud_provider`+`region` inputs map to `zones` + `instance_types` list with `{name,cpu,cpu_allocatable,memory,memory_allocatable}`.
- [ ] **Step 2:** Run, verify fail.
- [ ] **Step 3: Implement** — schema: `cloud_provider` (Required), `region` (Required), `zones` (Computed list(string)), `instance_types` (Computed list nested object). Read calls `ListInstanceTypes`. Register `NewInstanceTypesDataSource` in `provider.go`.
- [ ] **Step 4:** Update `provider_test.go` data-source count (8→9) + add `altinity_instance_types`. Run `TestProvider_RegistersDataSources` + the new test → PASS.
- [ ] **Step 5:** Commit: `feat(provider): add altinity_instance_types data source`.

---

## Task 6: `altinity_node_type` resource

**Files:** Create `internal/provider/resource_node_type.go`; modify `provider.go`, `provider_test.go`; test `resource_node_type_test.go`

- [ ] **Step 1: Failing tests** (mirror `resource_clickhouse_keeper_test.go` harness):
  - **Metadata** → `altinity_node_type`.
  - **CreateFresh:** not found by `(scope,code)` → `CreateNodeType` called with scope-default toleration → state set with `id`/`node_type_id`/`used`. `name` unset → no follow-up edit.
  - **CreateWithName:** `name` set ≠ code → after create, a follow-up `NodeTypeEdit` applies `name`.
  - **CreateAdopt:** found by `(scope,code)` → `CreateNodeType` NOT called.
  - **Update (code change):** `NodeTypeEdit` called; the edit body preserves the opaque `tolerations` read from current state unchanged.
  - **DeleteInUse:** `RemoveNodeType` returns an "in use" 4xx → Delete surfaces a clear error.
  - **DeleteOK:** `{}` → success.
  - **ReadDrift:** id absent from list → resource removed from state.
  - **Import:** `<env>:<scope>:<code>`.

- [ ] **Step 2:** Run, verify fail.

- [ ] **Step 3: Implement.** Schema per spec §6.1 (`environment`/`scope` ForceNew; `code`/`cpu`/`memory`/`capacity`/`storage_class`/`is_spot`/`name` updatable; `used`/`id`/`node_type_id` computed). Model carries unexported preserved opaque fields populated on Read.
  - **Create:** `RetryWhileBusy` { find-by-(scope,code) → adopt; else `CreateNodeType` with `Tolerations = scopeDefaultTolerations(scope)`, `NodeSelector/ExtraSpec = ""` }. Then if `name` set & ≠ code → `EditNodeType` (carrying the just-created tolerations). Read back → set state. Synchronous (no poll).
  - **Read:** `ListNodeTypes(env)` (withUsed), match by `node_type_id`; map fields incl. `used`; stash raw `Tolerations`/`NodeSelector`/`ExtraSpec` into state for preservation. Absent → `RemoveResource`.
  - **Update:** `EditNodeType(id, req)` where `req` carries updatable fields **and** the preserved opaque fields from prior state (so they are never cleared). Read back → set state.
  - **Delete:** `RemoveNodeType(id)`; on in-use rejection, `AddError` with remediation; `IsNotFound` → no-op.
  - **Import:** parse `<env>:<scope>:<code>`; Read resolves the rest. (Store env+scope+code; Read needs id — resolve id by `FindNodeTypeByCode` in Read when `node_type_id` is empty.)
  - Add `cloudProviderValidator`-style `scope` validator (clickhouse/zookeeper/system).
  - Register `NewNodeTypeResource` in `provider.go`.

- [ ] **Step 4:** Update `provider_test.go` resource count (7→8) + add `altinity_node_type`. Run resource tests + registration → PASS.
- [ ] **Step 5:** Commit: `feat(provider): add altinity_node_type resource`.

---

## Task 7: Docs + examples

**Files:** `examples/resources/altinity_node_type/resource.tf`, `examples/data-sources/altinity_instance_types/data-source.tf`; regenerated `docs/`

- [ ] **Step 1:** Write examples — node type referencing `altinity_instance_types` for `code`/`cpu`/`memory`, and the instance-types data source.
- [ ] **Step 2:** `make docs`. Verify `docs/resources/node_type.md`, `docs/data-sources/instance_types.md`, updated `node_types.md`.
- [ ] **Step 3:** Eyeball: docs note that tolerations/nodeSelector/extraSpec are not managed (mirror ACM defaults; preserved on update), and `used` is read-only.
- [ ] **Step 4:** Commit: `docs: examples + generated docs for node type management`.

---

## Task 8: Full verification

- [ ] **Step 1:** `go test ./...` → all PASS incl. codegen guard.
- [ ] **Step 2:** `go vet ./... && go build ./...`.
- [ ] **Step 3:** Confirm new files gofmt-clean (`gofmt -l` on the created files is empty).
- [ ] **Step 4 (operator, optional):** live `terraform apply` on env 2293 — create a clickhouse node type, confirm it appears with the dedicated toleration and `used=false`; update its `code`; confirm tolerations preserved; destroy (unused) succeeds; destroy an in-use one is refused.
- [ ] **Step 5:** Push to the existing branch (PR #9 is environment-only; node types extend the same branch or a follow-up — confirm with the user before merging into #9 vs. a new PR).

## Testing strategy notes
- Reuse the `httptest` + fixture style; new fixtures from the spec §3.1 captures.
- Highest-value assertions: create sends scope-correct toleration; update preserves opaque fields unchanged; `name` follow-up edit; delete-in-use error; `used` decoded read-only.
- Keep the codegen guard green after the allowlist additions.

## Open questions (from spec)
OQ-1 zookeeper/system scope tolerations (Task 0 capture) · OQ-2 NodeTypeAdd synchronous (assumed) · OQ-3 whether edit needs `cpu_alloc`/`memory_alloc`/`id` in body (carry through from read if so; the `nodeTypeRaw` struct already captures them).
