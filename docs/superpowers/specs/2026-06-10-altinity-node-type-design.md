# Design: node type management (`altinity_node_type` + node-type data sources)

- **Status:** Draft (pending spec review + author sign-off)
- **Date:** 2026-06-10
- **Author:** Vianney Foucault (with Claude)
- **Scope:** Second sub-project of "configure an Altinity.Cloud environment" (the
  first, `altinity_environment` + `altinity_regions`, shipped in PR #9). This
  sub-project covers **node types**. A follow-up sub-project will cover network.

## 1. Goal

Let operators define and manage the **node types** (instance shapes) of an
environment so it can host clusters, and discover both the node types already
defined and the instance types available in the environment's region.

Three cohesive deliverables, one spec / one PR:

1. **`altinity_node_types`** data source — *enhance* the existing one to surface
   `used`, `capacity`, and the ACM `id` (via `?withUsed=1`).
2. **`altinity_instance_types`** data source — *new*; the catalog of instance
   types available in a provider+region (what you pick a `code` from).
3. **`altinity_node_type`** resource — *new*; create/update/delete a node type.

## 2. Context

- `NodeType` wire struct + domain type already exist (`domain.go`, `models_gen.go`),
  and `NodeTypeList` is allowlisted with an `altinity_node_types` data source
  (`data_source_node_types.go`). This sub-project extends that foundation.
- Follows the established layering and the keeper/user resource patterns
  (adopt-by-key idempotent create, `RetryWhileBusy`, ACM id carried in state).
- All shapes below are live-confirmed (2026-06-10, env 2293).

## 3. API surface

| Purpose | Method / path | operationId | In allowlist? |
|---|---|---|---|
| List (with usage) | `GET /environment/{environment}/nodetypes?withUsed=1` | `NodeTypeList` | Yes |
| Create | `POST /environment/{environment}/nodetypes` | `NodeTypeAdd` | No → add |
| Update | `POST /nodetype/{id}` | `NodeTypeEdit` | No → add |
| Delete | `DELETE /nodetype/{id}` | `NodeTypeRemove` | No → add |
| Available instance types | `GET /cloud/options?platform=<p>&region=<r>&type=*` | `CloudOptionsGlobal` | Yes |

### 3.1 Captured payloads (env 2293)

**Create** `POST /environment/2293/nodetypes`:
```json
{"name":"c4-standard-24-lssd","scope":"clickhouse","code":"c4-standard-24-lssd",
 "used":false,"isSpot":false,"capacity":10,"storageClass":"","extraSpec":"",
 "tolerations":[{"key":"dedicated","operator":"Equal","effect":"NoSchedule","value":"clickhouse"}],
 "nodeSelector":"","memory":80160,"cpu":24}
```
Response `data`: `{id:"14140", scope, code, name (=code), storageClass, cpu:"24",
memory:"80160", id_environment:"2293", extraSpec, tolerations:[…], nodeSelector,
capacity:"10", isSpot:false, cpu_alloc:"24", memory_alloc:"80160"}`.

**Update** `POST /nodetype/14140` (changed instance type code in place):
```json
{"id":"14140","scope":"clickhouse","code":"c3d-highcpu-16","name":"c3d-highcpu-16",
 "storageClass":"","cpu":16,"memory":27380,"id_environment":"2293","extraSpec":"",
 "tolerations":[{…dedicated=clickhouse…}],"nodeSelector":"","capacity":"10",
 "isSpot":false,"cpu_alloc":"24","memory_alloc":"80160","used":false}
```

**List** `GET /environment/2293/nodetypes?withUsed=1` → array of node types, each
with `id, scope, code, name, storageClass, cpu, memory, id_environment, extraSpec,
tolerations[], nodeSelector, capacity, isSpot, cpu_alloc, memory_alloc, used`.
Observed: clickhouse-scope types carry `tolerations:[{dedicated=clickhouse:NoSchedule}]`;
system/zookeeper-scope carry `tolerations:[]`.

**Delete** `DELETE /nodetype/14139` → `{}`.

**Available** `GET /cloud/options?platform=gcp&region=us-east1&type=*` →
`{data:{zones:[…], instanceTypes:[{name,cpu,cpuAllocatable,mem,memAllocatable}]}}`
(367 entries for gcp/us-east1).

### 3.2 Key behaviors learned

- **`name` is ignored on create** — the created node type's `name` equals `code`
  regardless of the `name` sent. A custom `name` **is** respected on `NodeTypeEdit`.
  → the resource applies a non-default `name` via a follow-up edit after create.
- **`code` is editable in place** — the update capture changed the instance type
  (`c4-standard-24-lssd` → `c3d-highcpu-16`). → `code` is updatable, **not** ForceNew.
- **`used`** reflects whether a cluster currently uses the node type — treat as
  **read-only** (Computed). Read it via `?withUsed=1`.
- **Loose typing** — numbers come back as strings (`cpu:"24"`) and the update
  response returns `tolerations` as a JSON *string*. The domain layer coerces via
  the existing helpers; opaque fields are carried as `json.RawMessage`.

## 4. `altinity_node_types` data source (enhancement)

Add `?withUsed=1` to the list call and surface `id`, `used`, `capacity` on each
item (keep existing code/name/scope/cpu/memory/storage_class/is_spot). Backward
compatible (additive computed fields). Requires extending the `NodeType` domain
type with `Used bool` and `Capacity int64` (already on the wire schema).

## 5. `altinity_instance_types` data source (new)

| Field | Type | Mode |
|---|---|---|
| `cloud_provider` | string | Required |
| `region` | string | Required |
| `zones` | list(string) | Computed |
| `instance_types` | list(object{name, cpu, cpu_allocatable, memory, memory_allocatable}) | Computed |

New ACM client method `ListInstanceTypes(ctx, provider, region)` calling
`CloudOptionsGlobal` with `platform=<provider>&region=<region>&type=*`, decoding
`data.instanceTypes` (`{name, cpu, cpuAllocatable, mem, memAllocatable}`) and
`data.zones`. `cpu`/`mem` are JSON numbers here (not string-ints). Note the wire
field is `mem` → exposed as `memory`; `cpuAllocatable`/`memAllocatable` →
`cpu_allocatable`/`memory_allocatable`.

## 6. `altinity_node_type` resource (new)

### 6.1 Schema

| Attribute | Type | Mode | Notes |
|---|---|---|---|
| `environment` | string | Required, **ForceNew** | ACM env id |
| `scope` | string | Required, **ForceNew** | `clickhouse`/`zookeeper`/`system` (validated) |
| `code` | string | Required, **updatable** | instance type code; editable in place |
| `cpu` | number | Required, updatable | vCPUs |
| `memory` | int | Required, updatable | MB |
| `capacity` | int | Optional, updatable | max nodes of this type |
| `storage_class` | string | Optional, updatable | |
| `is_spot` | bool | Optional, updatable | default false |
| `name` | string | Optional+Computed | create ignores it (ACM sets = code); applied via follow-up edit when set ≠ code |
| `used` | bool | **Computed** (read-only) | true when a cluster currently uses this node type |
| `id` | string | Computed | `<environment>:<acm_id>` |
| `node_type_id` | string | Computed | raw ACM `/nodetype/{id}` id |

**Not exposed (managed to mirror ACM, see §6.3):** `tolerations`,
`node_selector`, `extra_spec`.

### 6.2 Lifecycle

- **Create** — adopt-by-`(scope, code)` within the env (idempotent re-apply,
  like `FindUserByName`); else `NodeTypeAdd`. **Synchronous** (a node type is a
  metadata record; real nodes spin up only when a cluster requests them) — no
  poll. Wrapped in `RetryWhileBusy` for the env lock. If `name` is set and differs
  from `code`, issue a follow-up `NodeTypeEdit` to apply it (create ignores `name`).
- **Read** — `NodeTypeList(env, withUsed=1)`, match by stored ACM id; map fields
  incl. `used`; capture opaque `tolerations`/`nodeSelector`/`extraSpec` for
  preservation (§6.3). Absent → drift (remove from state).
- **Update** — `NodeTypeEdit(id)` with the updatable fields **plus** the preserved
  opaque fields read back unchanged, so the edit never clears them.
- **Delete** — `NodeTypeRemove(id)`. If ACM rejects because the node type is in
  use, surface a clear diagnostic ("node type is in use — remove dependent
  clusters first"). No cascade. (`DELETE` returns `{}` on success.)
- **Import** — `terraform import altinity_node_type.x <environment>:<scope>:<code>`.

### 6.3 Tolerations / nodeSelector / extraSpec — mirror ACM, not operator-managed

Per decision: the resource does **not** let operators configure these, but it
**mirrors the ACM UI's behavior** so a TF-created node type is indistinguishable
from a UI-created one:

- **On create**, send the UI's scope-default tolerations:
  - `clickhouse` → `[{"key":"dedicated","operator":"Equal","value":"clickhouse","effect":"NoSchedule"}]` (live-confirmed).
  - `zookeeper` → `[{"key":"dedicated","operator":"Equal","value":"zookeeper","effect":"NoSchedule"}]` (inferred by analogy — **confirm via a UI capture during implementation**, OQ-1).
  - `system` → `[]` (live-confirmed empty).
  - `nodeSelector`/`extraSpec` → `""` (as the UI sends).
- **On update**, pass the *current* `tolerations`/`nodeSelector`/`extraSpec`
  (read from the API) back unchanged, so an edit never alters them.
- Docs state plainly: **managing tolerations/nodeSelector/extraSpec via this
  resource is not supported**; they are set to mirror ACM defaults on create and
  preserved as-is thereafter. Operators who need custom values set them in the
  ACM UI; the provider will not clobber them.

The domain `NodeType` gains raw-JSON passthrough fields for these (the wire
schema already types them as opaque object/array), used only for create defaults
and update preservation — never surfaced to Terraform.

## 7. Plumbing

1. **specgen allowlist** — add `NodeTypeAdd`, `NodeTypeEdit`, `NodeTypeRemove`
   (mirror in `codegen_guard_test`); regenerate. `NodeType` schema already emitted.
2. **ACM client** (`nodetypes.go`, new or extend) — `CreateNodeType`,
   `EditNodeType`, `RemoveNodeType`, `FindNodeTypeByCode`, and `ListNodeTypes`
   gains a `withUsed` variant. Add `ListInstanceTypes` (cloud.go). Extend domain
   `NodeType` with `Used`, `Capacity`, and opaque passthrough fields; coerce the
   loose types (string-ints; tolerations-as-string on edit response).
3. **Provider** — extend `data_source_node_types.go`; new
   `data_source_instance_types.go`; new `resource_node_type.go`. Register both new
   constructors in `provider.go`.
4. **Tests** — acm unit tests (create body incl. scope-default toleration, adopt,
   edit preserves opaque fields, delete, withUsed decode, instance-types decode) +
   provider tests (schema/plan, create + follow-up-name-edit, adopt/resume, update
   incl. code change, delete-in-use error, import, both data sources). Keep the
   codegen guard green.
5. **Docs & examples** — `docs/resources/node_type.md`,
   `docs/data-sources/instance_types.md`, regenerated `node_types.md`; examples.

## 8. Open questions

- **OQ-1** Exact UI scope-default tolerations for `zookeeper` and `system` scopes.
  `clickhouse` (`dedicated=clickhouse:NoSchedule`) and `system` (`[]`) are
  live-confirmed; `zookeeper` is inferred (`dedicated=zookeeper:NoSchedule`).
  Confirm with a UI node-type-add capture for a zookeeper-scope type before
  finalizing the create defaults. Does not block the resource for the
  clickhouse/system cases.
- **OQ-2** Is `NodeTypeAdd` truly synchronous (no provisioning to poll)? Assumed
  yes (metadata record). Confirm; if a status appears, add a short settle.
- **OQ-3** On update, does `NodeTypeEdit` require `cpu_alloc`/`memory_alloc` and
  `id` in the body (the UI sends them)? Carry them through from the read to be
  safe; confirm whether omitting is accepted.

None block writing the implementation plan; OQ-1 is a small capture that gates
only the zookeeper-scope create default.

## 9. Testing strategy

Mirrors the existing `httptest` + fixture style. New fixtures from §3.1 captures:
`testdata/nodetypes_withused.json`, `testdata/instance_types.json`,
`testdata/nodetype_create_response.json`. Highest-value tests: create sends the
scope-correct toleration; update preserves opaque fields unchanged; `name`
follow-up edit; delete-in-use surfaces a clear error.
