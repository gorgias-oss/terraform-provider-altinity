# `altinity_environment` + `altinity_regions` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a resumable `altinity_environment` managed resource (Altinity-hosted request flow) and an `altinity_regions` data source to the Terraform provider.

**Architecture:** Follow the provider's existing three-layer split — generated `wire` (extend the specgen allowlist + regenerate), hand-written `acm` client + domain coercion, and terraform-plugin-framework resources/data sources. The environment resource creates via `EnvironmentRequest`, polls `EnvironmentShow` until ready, and is resumable across applies via adopt-by-name (it does **not** persist state on a poll timeout). Delete is guarded by a cluster pre-flight (no cascade).

**Tech Stack:** Go 1.26, terraform-plugin-framework v1.13, `httptest` for client tests, `tools/specgen` codegen, `terraform-plugin-log/tflog`.

**Spec:** `docs/superpowers/specs/2026-06-09-altinity-environment-design.md`

---

## File Structure

**Created:**
- `internal/acm/cloud_options_global.go` — `ListCloudOptionsGlobal` client method (regions).
- `internal/provider/data_source_regions.go` — `altinity_regions` data source.
- `internal/provider/resource_environment.go` — `altinity_environment` resource.
- `internal/acm/testdata/cloud_options_regions.json` — captured regions fixture.
- `internal/acm/testdata/environment_show.json` — captured `EnvironmentShow` fixture.
- `internal/acm/testdata/environment_request.json` — captured `EnvironmentRequest` response fixture.
- `docs/resources/environment.md`, `docs/data-sources/regions.md` — generated docs.
- `examples/resources/altinity_environment/resource.tf`, `examples/data-sources/altinity_regions/data-source.tf`.

**Modified:**
- `tools/specgen/main.go` — add ops to `allowedOps`.
- `internal/acm/wire/codegen_guard_test.go` — mirror new ops in `allowlistForGuard`.
- `internal/acm/wire/endpoints_gen.go`, `models_gen.go` — regenerated (not hand-edited).
- `internal/acm/environments.go` — add `RequestEnvironment`, `GetEnvironmentByID`, `EditEnvironment`, `RemoveEnvironment`, `EnvironmentRequest` body type.
- `internal/acm/domain.go` — extend `Environment` (add `HostedByAltinity`/`Created` if useful); add region option mapping helper if needed.
- `internal/acm/poll.go` — environment terminal-status sets (capture-driven).
- `internal/provider/resource_clickhouse_cluster.go` (or a new `internal/provider/timeouts.go`) — extract `resolveTimeoutsWithDefaults` so the environment can use 45m/30m defaults without touching cluster defaults.
- `internal/provider/provider.go` — register `NewRegionsDataSource` + `NewEnvironmentResource`.

---

## Task 0: Live API capture (operator-run prerequisite)

> **This task provisions billable infra (one real environment) and must be run by the operator against the live ACM instance.** It resolves OQ-1..5 from the spec. The rest of the plan has sensible defaults + `TODO(spike)` markers so code can be written in parallel, but the fixtures and status sets are finalized here.

**Files:**
- Create: `internal/acm/testdata/cloud_options_regions.json`, `environment_show.json`, `environment_request.json`

- [ ] **Step 1: Capture regions (read-only).** With `ALTINITYCLOUD_API_TOKEN` set, run for each provider:
  ```bash
  curl -s -H "X-Auth-Token: $ALTINITYCLOUD_API_TOKEN" \
    "https://acm.altinity.cloud/api/cloud/options?type=regions&provider=aws" | tee internal/acm/testdata/cloud_options_regions.json
  ```
  Record: does `type=regions` work (vs `region`)? Does it key on `provider` or `platform`? Is the body `{"data":[{"code","name"}]}`? → resolves **OQ-3**.

- [ ] **Step 2: Capture an existing environment (read-only).**
  ```bash
  curl -s -H "X-Auth-Token: $ALTINITYCLOUD_API_TOKEN" \
    "https://acm.altinity.cloud/api/environments" | jq '.data[0]'
  # then by id:
  curl -s -H "X-Auth-Token: $ALTINITYCLOUD_API_TOKEN" \
    "https://acm.altinity.cloud/api/environment/<ID>" | tee internal/acm/testdata/environment_show.json
  ```
  Confirm the `EnvironmentShow` field set matches `environmentFromWire`. Note the **status/state values of a healthy environment** → feeds OQ-5.

- [ ] **Step 3: Capture one real create (billable — operator decision).** Trigger an `EnvironmentRequest` and save the response:
  ```bash
  curl -s -H "X-Auth-Token: $ALTINITYCLOUD_API_TOKEN" -H "Content-Type: application/json" \
    -X POST "https://acm.altinity.cloud/api/environments/request" \
    -d '{"name":"tf-spike-env","cloud_provider":"aws","aws_region":"<region-code>"}' \
    | tee internal/acm/testdata/environment_request.json
  ```
  Record: **does the response carry the new env id?** (→ OQ-1) — does omitting `first` work / what does it change? (→ OQ-2) — any required field beyond name+provider+region? (→ OQ-4). Then poll `GET /environment/<id>` every 15s and record the **status string sequence** until ready and the **wall-clock time** (→ OQ-5 + §4.5 timeout tuning). If it fails, record the terminal-error status string.

- [ ] **Step 4: Capture delete signal (on the spike env, once empty).**
  ```bash
  curl -s -H "X-Auth-Token: $ALTINITYCLOUD_API_TOKEN" -X DELETE \
    "https://acm.altinity.cloud/api/environment/<ID>"
  # confirm it then disappears from GET /environments
  ```

- [ ] **Step 5: Record findings** in the spec's §7 Open Questions (edit the spec doc, commit). Note exact: region option `type` value + keying; healthy status set; error status set; whether request returns id; meaning of `first`; observed provisioning time.

- [ ] **Step 6: Commit fixtures**
  ```bash
  git add internal/acm/testdata/cloud_options_regions.json internal/acm/testdata/environment_show.json internal/acm/testdata/environment_request.json docs/superpowers/specs/2026-06-09-altinity-environment-design.md
  git commit -m "test(acm): capture environment + regions API fixtures from live ACM"
  ```

---

## Task 1: Extend the specgen allowlist and regenerate

**Files:**
- Modify: `tools/specgen/main.go:53` (`allowedOps`)
- Modify: `internal/acm/wire/codegen_guard_test.go:18` (`allowlistForGuard`)
- Regenerated: `internal/acm/wire/endpoints_gen.go`, `internal/acm/wire/models_gen.go`

- [ ] **Step 1: Add ops to the generator allowlist.** In `tools/specgen/main.go`, add to `allowedOps` (group with a comment):
  ```go
  // Environment lifecycle (altinity_environment resource).
  "EnvironmentRequest", // POST   /environments/request
  "EnvironmentShow",    // GET    /environment/{id}
  "EnvironmentEdit",    // POST   /environment/{id}
  "EnvironmentRemove",  // DELETE /environment/{id}
  // Global cloud options (altinity_regions data source).
  "CloudOptionsGlobal", // GET    /cloud/options
  ```
  `EnvironmentList`, `ClusterList`, and the `Environment` schema are already present.

- [ ] **Step 2: Mirror the same five ops in the guard test** `allowlistForGuard` (`codegen_guard_test.go`). The guard asserts `len(allowlistForGuard) == len(Endpoints)`, so both lists must match.

- [ ] **Step 3: Regenerate**
  Run: `go generate ./...`
  Expected: `internal/acm/wire/endpoints_gen.go` gains `OpEnvironmentRequest`, `OpEnvironmentShow`, `OpEnvironmentEdit`, `OpEnvironmentRemove`, `OpCloudOptionsGlobal` constants + registry entries; `models_gen.go` unchanged (Environment already emitted).

- [ ] **Step 4: Run the guard test**
  Run: `go test ./internal/acm/wire/ -run 'TestAllowlistedOpsResolveInSpec|TestGenerateNoDiff' -v`
  Expected: PASS (registry length matches; `go generate` is a no-op diff).

- [ ] **Step 5: Build to confirm nothing else broke**
  Run: `go build ./...`
  Expected: success.

- [ ] **Step 6: Commit**
  ```bash
  git add tools/specgen/main.go internal/acm/wire/
  git commit -m "feat(wire): allowlist Environment{Request,Show,Edit,Remove} + CloudOptionsGlobal"
  ```

---

## Task 2: ACM client — `ListCloudOptionsGlobal`

**Files:**
- Create: `internal/acm/cloud_options_global.go`
- Test: `internal/acm/cloud_options_global_test.go`

- [ ] **Step 1: Write the failing test.** Mirror the existing `cloud`/`environments` test style (httptest server returning `{"data":[{"code","name"}]}` from the captured fixture). Assert the request path is `/cloud/options`, the query has `type=regions` and `provider=aws` (adjust to the OQ-3 finding), and the decoded slice matches.
  ```go
  func TestListCloudOptionsGlobal(t *testing.T) {
      srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          assert.Equal(t, "/cloud/options", r.URL.Path)
          assert.Equal(t, "regions", r.URL.Query().Get("type"))
          assert.Equal(t, "aws", r.URL.Query().Get("provider"))
          w.Write(mustReadFixture(t, "testdata/cloud_options_regions.json"))
      }))
      defer srv.Close()
      c := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
      got, err := c.ListCloudOptionsGlobal(context.Background(), "aws", "regions")
      require.NoError(t, err)
      assert.NotEmpty(t, got)
      assert.NotEmpty(t, got[0].Code)
  }
  ```

- [ ] **Step 2: Run it, verify it fails** (`ListCloudOptionsGlobal` undefined).
  Run: `go test ./internal/acm/ -run TestListCloudOptionsGlobal`

- [ ] **Step 3: Implement.** Reuse the `CloudOption` type and the `doRequest` query pattern from `cloud.go:32`:
  ```go
  // ListCloudOptionsGlobal returns the {code,name} options of a given type for a
  // cloud provider via the non-environment-scoped GET /cloud/options. Used by the
  // altinity_regions data source: at environment-creation time no environment
  // exists yet, so the env-scoped CloudOptions cannot be used.
  func (c *Client) ListCloudOptionsGlobal(ctx context.Context, provider, optType string) ([]CloudOption, error) {
      q := url.Values{}
      q.Set("type", optType)
      if provider != "" {
          q.Set("provider", provider) // confirm provider vs platform from OQ-3
      }
      var opts []CloudOption
      if err := c.doRequest(ctx, wire.OpCloudOptionsGlobal, nil, q, nil, &opts); err != nil {
          return nil, err
      }
      return opts, nil
  }
  ```

- [ ] **Step 4: Run the test, verify PASS.**

- [ ] **Step 5: Commit**
  ```bash
  git add internal/acm/cloud_options_global.go internal/acm/cloud_options_global_test.go internal/acm/testdata/cloud_options_regions.json
  git commit -m "feat(acm): add ListCloudOptionsGlobal for per-provider regions"
  ```

---

## Task 3: ACM client — environment CRUD

**Files:**
- Modify: `internal/acm/environments.go`
- Modify: `internal/acm/domain.go` (only if adding `HostedByAltinity`/`Created` to the `Environment` domain type)
- Test: `internal/acm/environments_test.go`

- [ ] **Step 1: Write the failing tests** (one httptest case per method), using the captured fixtures:
  - `RequestEnvironment` — POST `/environments/request`, body has `name`+`cloud_provider`+`aws_region`; returns the env (id from response per OQ-1, else zero).
  - `GetEnvironmentByID` — GET `/environment/{id}`, decodes `environment_show.json`.
  - `EditEnvironment` — POST `/environment/{id}`, body has `displayName`.
  - `RemoveEnvironment` — DELETE `/environment/{id}`; a 404 returns an error for which `IsNotFound` is true.

- [ ] **Step 2: Run them, verify they fail.**

- [ ] **Step 3: Implement in `environments.go`:**
  ```go
  // EnvironmentRequest is the body for POST /environments/request. cloud_provider
  // selects which *_region field ACM reads; the resource sets exactly one.
  type EnvironmentRequest struct {
      Name          string `json:"name"`
      CloudProvider string `json:"cloud_provider"`
      AWSRegion     string `json:"aws_region,omitempty"`
      GCPRegion     string `json:"gcp_region,omitempty"`
      AzureRegion   string `json:"azure_region,omitempty"`
      HcloudRegion  string `json:"hcloud_region,omitempty"`
      // First is undocumented (OQ-2); omitted by default.
  }

  // EnvironmentEditRequest carries the only field the resource updates (v1).
  type EnvironmentEditRequest struct {
      DisplayName string `json:"displayName,omitempty"`
  }

  func (c *Client) RequestEnvironment(ctx context.Context, req EnvironmentRequest) (Environment, error) {
      var w wire.Environment
      if err := c.doJSON(ctx, wire.OpEnvironmentRequest, nil, req, &w); err != nil {
          return Environment{}, err
      }
      return environmentFromWire(&w) // may have id=0 if the response omits it; caller falls back to GetEnvironmentByName
  }

  func (c *Client) GetEnvironmentByID(ctx context.Context, id int64) (Environment, error) {
      var w wire.Environment
      args := map[string]string{"id": strconv.FormatInt(id, 10)}
      if err := c.doJSON(ctx, wire.OpEnvironmentShow, args, nil, &w); err != nil {
          return Environment{}, err
      }
      return environmentFromWire(&w)
  }

  func (c *Client) EditEnvironment(ctx context.Context, id int64, req EnvironmentEditRequest) (Environment, error) {
      var w wire.Environment
      args := map[string]string{"id": strconv.FormatInt(id, 10)}
      if err := c.doJSON(ctx, wire.OpEnvironmentEdit, args, req, &w); err != nil {
          return Environment{}, err
      }
      return environmentFromWire(&w)
  }

  func (c *Client) RemoveEnvironment(ctx context.Context, id int64) error {
      args := map[string]string{"id": strconv.FormatInt(id, 10)}
      return c.doJSON(ctx, wire.OpEnvironmentRemove, args, nil, nil)
  }
  ```
  Map `cloud_provider` → region field in the *provider* layer (Task 5) so the acm layer stays a faithful pass-through. Confirm the `{"id"}` path-arg name against `endpoints_gen.go` (the spec uses `{id}`).

- [ ] **Step 4: Run the tests, verify PASS.**

- [ ] **Step 5: Commit**
  ```bash
  git add internal/acm/environments.go internal/acm/domain.go internal/acm/environments_test.go internal/acm/testdata/environment_*.json
  git commit -m "feat(acm): add environment request/show/edit/remove client methods"
  ```

---

## Task 4: Environment poll status sets (capture-driven)

**Files:**
- Modify: `internal/acm/poll.go`
- Test: `internal/acm/poll_test.go`

- [ ] **Step 1: Write the failing test** asserting `environmentTerminalHealthy("<healthy-status-from-capture>")` is true and a known error status maps to terminal-error, and that an unrecognized status is treated as "not done, no error" (keep provisioning).

- [ ] **Step 2: Run it, verify it fails.**

- [ ] **Step 3: Implement** a capture-driven status helper, parallel to the existing `terminalHealthy`/`terminalError` but scoped to environments (the existing maps are cluster/keeper-tuned). Populate the sets from Task 0 findings. Provide an `EnvironmentStatusFunc`-compatible adapter so `PollUntilHealthy` can be reused, OR add a small `PollEnvironmentUntilReady(ctx, fetchStatus)` that uses the env-specific sets. Prefer reuse: if the captured healthy string is already in `healthyStatuses` (e.g. "running"/"online"), no new code is needed — just confirm with the test and add any missing string.
  ```go
  // Environment terminal statuses (capture-confirmed; see Task 0 / OQ-5).
  // TODO(spike): replace placeholders with the captured strings.
  ```

- [ ] **Step 4: Run the test, verify PASS.**

- [ ] **Step 5: Commit**
  ```bash
  git add internal/acm/poll.go internal/acm/poll_test.go
  git commit -m "feat(acm): environment terminal-status handling for create poll"
  ```

---

## Task 5: `altinity_regions` data source

**Files:**
- Create: `internal/provider/data_source_regions.go`
- Modify: `internal/provider/provider.go:204` (register)
- Test: `internal/provider/data_source_regions_test.go`

- [ ] **Step 1: Write the failing test** (mirror `data_source_zones_test.go` / `data_source_versions_test.go`): configure a mock ACM client returning the regions fixture, assert the data source maps `cloud_provider` → a `regions` list of `{code,name}`.

- [ ] **Step 2: Run it, verify it fails.**

- [ ] **Step 3: Implement** following `data_source_environment.go` structure (Metadata `_regions`, Configure casting `*acm.Client`, Read calling `ListCloudOptionsGlobal`). Schema:
  - `cloud_provider` (Required, String)
  - `regions` (Computed, ListNestedAttribute of `{code, name}`)
  Use `dataSourceErrorDetail` for errors.

- [ ] **Step 4: Register** `NewRegionsDataSource` in `provider.go` `DataSources()`.

- [ ] **Step 5: Run the test, verify PASS** (`go test ./internal/provider/ -run Regions`).

- [ ] **Step 6: Commit**
  ```bash
  git add internal/provider/data_source_regions.go internal/provider/data_source_regions_test.go internal/provider/provider.go
  git commit -m "feat(provider): add altinity_regions data source"
  ```

---

## Task 6: Shared timeout defaults helper

**Files:**
- Modify: `internal/provider/resource_clickhouse_cluster.go:1804` (extract) — or create `internal/provider/timeouts.go`
- Test: extend `resource_clickhouse_cluster_test.go` defaults test or add `timeouts_test.go`

- [ ] **Step 1: Write the failing test** asserting `resolveTimeoutsWithDefaults(ctx, nullObj, resolvedTimeouts{create: 45*time.Minute, delete: 30*time.Minute})` returns those defaults, and that an explicit `create="5m"` overrides.

- [ ] **Step 2: Run it, verify it fails.**

- [ ] **Step 3: Refactor.** Extract the body of `resolveTimeouts` into `resolveTimeoutsWithDefaults(ctx, obj, defaults resolvedTimeouts)`; keep `resolveTimeouts` as a thin wrapper passing the cluster defaults so existing callers are unchanged (DRY; no behavior change for clusters).

- [ ] **Step 4: Run the full provider test package** to confirm the cluster timeouts behavior is unchanged.
  Run: `go test ./internal/provider/ -run Timeout`

- [ ] **Step 5: Commit**
  ```bash
  git add internal/provider/
  git commit -m "refactor(provider): parameterize resolveTimeouts defaults"
  ```

---

## Task 7: `altinity_environment` resource — schema + Configure + Metadata + Read

**Files:**
- Create: `internal/provider/resource_environment.go`
- Test: `internal/provider/resource_environment_test.go`

- [ ] **Step 1: Write the failing schema/Read tests.** Use the provider test harness (mirror `resource_clickhouse_keeper_test.go`): a mock client + `EnvironmentShow` fixture; assert Read maps computed fields and that a 404 removes the resource from state.

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Implement schema + scaffolding.** Model fields per spec §4.1:
  ```go
  type environmentResourceModel struct {
      ID             types.String `tfsdk:"id"`
      Name           types.String `tfsdk:"name"`
      CloudProvider  types.String `tfsdk:"cloud_provider"`
      Region         types.String `tfsdk:"region"`
      DisplayName    types.String `tfsdk:"display_name"`
      NormalizedName types.String `tfsdk:"normalized_name"`
      Type           types.String `tfsdk:"type"`
      Domain         types.String `tfsdk:"domain"`
      Status         types.String `tfsdk:"status"`
      State          types.String `tfsdk:"state"`
      Timeouts       types.Object `tfsdk:"timeouts"`
  }
  ```
  - `name`/`cloud_provider`/`region`: Required + `stringplanmodifier.RequiresReplace()`.
  - `cloud_provider`: validator allowing `aws|gcp|azure|hcloud`.
  - `display_name`: Optional+Computed with `UseStateForUnknown`.
  - computed read-backs: `UseStateForUnknown`.
  - `timeouts`: `create`/`delete` only.
  Implement `Metadata` (`_environment`), `Configure` (cast `*acm.Client`), and `Read` (call `GetEnvironmentByID` parsed from `id`; `IsNotFound` → `RemoveResource`). Add `applyEnvironmentToModel`.

- [ ] **Step 4: Run, verify PASS.**

- [ ] **Step 5: Commit**
  ```bash
  git add internal/provider/resource_environment.go internal/provider/resource_environment_test.go
  git commit -m "feat(provider): altinity_environment schema + Read"
  ```

---

## Task 8: `altinity_environment` resource — Create (resumable adopt-by-name + poll)

**Files:**
- Modify: `internal/provider/resource_environment.go`
- Test: `internal/provider/resource_environment_test.go`

- [ ] **Step 1: Write failing tests** for the three Create paths:
  1. **Fresh create:** no env by name → `RequestEnvironment` called → poll to ready → state set with id + computed fields.
  2. **Resume:** env already exists by name → `RequestEnvironment` NOT called → poll resumes → state set.
  3. **Poll timeout:** ready never reached within `create` timeout → Create returns an error **and state is NOT set** (assert `resp.State` is empty/null). This is the resumability contract.

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Implement Create:**
  ```go
  func (r *environmentResource) Create(ctx, req, resp) {
      // parse plan; resolveTimeoutsWithDefaults(create:45m, delete:30m)
      opCtx, cancel := context.WithTimeout(ctx, to.create); defer cancel()

      name := plan.Name.ValueString()
      var envID int64

      // 1. Adopt-by-name first (resume path). RetryWhileBusy guards the env lock.
      err := acm.RetryWhileBusy(opCtx, func() error {
          existing, gerr := r.client.GetEnvironmentByName(opCtx, name)
          if gerr == nil {
              envID = existing.ID
              return nil
          }
          if !acm.IsNotFound(gerr) {
              return gerr
          }
          // 2. Not found → request a new environment.
          created, cerr := r.client.RequestEnvironment(opCtx, buildEnvRequest(plan))
          if cerr != nil {
              return cerr
          }
          envID = created.ID
          return nil
      })
      if err != nil { resp.Diagnostics.AddError("Failed to request environment", err.Error()); return }

      // 3. Resolve id if the request didn't return one (OQ-1 fallback).
      if envID == 0 {
          env, gerr := r.client.GetEnvironmentByName(opCtx, name)
          if gerr != nil { resp.Diagnostics.AddError("Environment created but id not resolvable", gerr.Error()); return }
          envID = env.ID
      }

      // 4. Poll until ready. On timeout we return WITHOUT setting state →
      //    next apply adopts-by-name and resumes (spec §4.4).
      if err := acm.PollUntilHealthy(opCtx, func(c context.Context) (string, error) {
          e, gerr := r.client.GetEnvironmentByID(c, envID)
          return e.Status, gerr
      }); err != nil {
          resp.Diagnostics.AddError(
              "Environment did not become ready",
              fmt.Sprintf("Environment %q (id %d) is still provisioning: %s. "+
                  "Re-apply to resume waiting on the same environment (it is not destroyed), "+
                  "or raise the create timeout.", name, envID, err),
          )
          return // <-- deliberately no resp.State.Set
      }

      // 5. Read back full state.
      env, gerr := r.client.GetEnvironmentByID(ctx, envID)
      if gerr != nil { resp.Diagnostics.AddError("Failed to read environment after create", gerr.Error()); return }
      applyEnvironmentToModel(&plan, env)
      resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
  }
  ```
  `buildEnvRequest` maps `cloud_provider`+`region` to the right `*_region` field. Add a sentinel comment documenting the no-state-on-timeout contract (it is load-bearing — see spec §4.4).

- [ ] **Step 4: Run, verify PASS** (all three Create paths).

- [ ] **Step 5: Commit**
  ```bash
  git add internal/provider/resource_environment.go internal/provider/resource_environment_test.go
  git commit -m "feat(provider): altinity_environment resumable Create with adopt-by-name + poll"
  ```

---

## Task 9: `altinity_environment` resource — Update + Delete (guarded) + Import

**Files:**
- Modify: `internal/provider/resource_environment.go`
- Test: `internal/provider/resource_environment_test.go`

- [ ] **Step 1: Write failing tests:**
  - **Update:** changing `display_name` calls `EditEnvironment`; state reflects the new value. (Changing `region`/`name`/`cloud_provider` is RequiresReplace — assert via schema, not Update.)
  - **Delete with clusters:** `ListClusters` returns 2 → Delete returns an error naming them; `RemoveEnvironment` NOT called.
  - **Delete empty:** `ListClusters` returns 0 → `RemoveEnvironment` called → `PollUntilGoneBy` until absent.
  - **Import:** `ImportState("<id>")` sets `id`; subsequent Read populates the rest.

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Implement Update / Delete / ImportState:**
  ```go
  func (r *environmentResource) Update(ctx, req, resp) {
      // parse plan + id; EditEnvironment(displayName); short RetryWhileBusy settle; read-back; set state
  }

  func (r *environmentResource) Delete(ctx, req, resp) {
      // parse state + id + timeouts(delete:30m)
      // GUARD: no cascade.
      clusters, err := r.client.ListClusters(ctx, strconv.FormatInt(id, 10))
      if err != nil && !acm.IsNotFound(err) && !acm.IsForbidden(err) {
          resp.Diagnostics.AddError("Failed to check environment clusters before delete", err.Error()); return
      }
      if len(clusters) > 0 {
          names := clusterNames(clusters)
          resp.Diagnostics.AddError(
              "Environment is not empty",
              fmt.Sprintf("Environment %q (id %d) still contains %d cluster(s) [%s]; "+
                  "destroy them before destroying the environment. This provider never "+
                  "deletes clusters on your behalf.", name, id, len(clusters), strings.Join(names, ", ")),
          )
          return
      }
      if err := r.client.RemoveEnvironment(ctx, id); err != nil {
          if acm.IsNotFound(err) { return }
          resp.Diagnostics.AddError("Failed to delete environment", err.Error()); return
      }
      pollCtx, cancel := context.WithTimeout(ctx, to.delete); defer cancel()
      if err := acm.PollUntilGoneBy(pollCtx, func(c context.Context) (bool, error) {
          _, gerr := r.client.GetEnvironmentByID(c, id)
          if acm.IsNotFound(gerr) { return false, nil }
          if gerr != nil { return false, gerr }
          return true, nil
      }); err != nil {
          resp.Diagnostics.AddError("Environment did not terminate", err.Error())
      }
  }

  func (r *environmentResource) ImportState(ctx, req, resp) {
      resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
  }
  ```
  Note the `ListClusters` env-arg is the **environment id as string** (`ListClusters(ctx, environmentID string)`).
  Add the `resource.ResourceWithImportState` interface assertion var.

- [ ] **Step 4: Run, verify PASS** (all four cases).

- [ ] **Step 5: Register** `NewEnvironmentResource` in `provider.go` `Resources()` (after `NewKeeperResource`). Run `go build ./... && go test ./internal/...`.

- [ ] **Step 6: Commit**
  ```bash
  git add internal/provider/resource_environment.go internal/provider/resource_environment_test.go internal/provider/provider.go
  git commit -m "feat(provider): altinity_environment Update, guarded Delete, Import + register"
  ```

---

## Task 10: Examples + docs

**Files:**
- Create: `examples/resources/altinity_environment/resource.tf`, `examples/data-sources/altinity_regions/data-source.tf`
- Generated: `docs/resources/environment.md`, `docs/data-sources/regions.md`

- [ ] **Step 1: Write examples.** `resource.tf`:
  ```hcl
  data "altinity_regions" "aws" { cloud_provider = "aws" }

  resource "altinity_environment" "prod" {
    name           = "prod-eu"
    cloud_provider = "aws"
    region         = data.altinity_regions.aws.regions[0].code
    display_name   = "Production EU"
    timeouts { create = "45m" delete = "30m" }
  }
  ```

- [ ] **Step 2: Regenerate registry docs** using the repo's existing doc workflow (the one that produced `docs/data-sources/environment.md`). Check the Makefile target:
  Run: `make docs` (or `make generate` — confirm the target in `Makefile`)
  Expected: `docs/resources/environment.md` and `docs/data-sources/regions.md` created; descriptions sourced from the schema strings.

- [ ] **Step 3: Eyeball the generated docs** — ensure the resumability + no-cascade-delete behavior is described (add to the resource's schema `Description` / `MarkdownDescription` if missing, then regenerate).

- [ ] **Step 4: Commit**
  ```bash
  git add examples/ docs/
  git commit -m "docs: examples + generated docs for altinity_environment and altinity_regions"
  ```

---

## Task 11: Full verification + finish

- [ ] **Step 1: Full test suite**
  Run: `go test ./...`
  Expected: all PASS, including the codegen guard.

- [ ] **Step 2: Vet + build**
  Run: `go vet ./... && go build ./...`

- [ ] **Step 3: Lint/format** per the repo (`gofmt`, and whatever `make lint` runs).

- [ ] **Step 4: Optional live smoke test** (operator, billable): `terraform apply` a real `altinity_environment`, confirm it reaches ready; kill mid-apply and re-apply to confirm **resume** (no duplicate, no destroy); add a cluster and confirm `terraform destroy` of the env is **refused**; remove cluster, destroy succeeds.

- [ ] **Step 5: Use superpowers:finishing-a-development-branch** to open the PR (base `main`).

---

## Testing strategy notes

- All `acm` tests use `httptest.Server` + `WithHTTPClient`; no real network (`client.go:11`).
- Use the captured fixtures from Task 0 as the source of truth for response shapes.
- The resume contract (Task 8, path 3) is the highest-value test — assert state is empty after a Create poll timeout, because that is what makes re-apply adopt rather than replace.
- Keep the codegen guard green: any allowlist edit must be mirrored in `codegen_guard_test.go`.

## Open questions carried from the spec (resolved in Task 0)
OQ-1 request returns id? · OQ-2 `first` field · OQ-3 region option `type`/keying · OQ-4 required request fields · OQ-5 env status strings. None block implementation; defaults + `TODO(spike)` markers bridge until Task 0 lands.
