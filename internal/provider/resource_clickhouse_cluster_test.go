// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	fwvalidator "github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Gorgias/terraform-provider-altinity/internal/acm"
)

// init zeroes the production warmup/settle durations for the entire
// `internal/provider` test binary.
//
// In production, postMutationWarmup (10s) and postCreateSettleDelay (30s)
// exist to bridge timing lag between ACM acknowledging an operation and
// downstream operator state being usable (live-observed). Tests use a
// `httptest.Server` with no such lag — zero is the only correct value, and
// it's the same value for every test in this package. There is no
// per-test variation to model, so a one-shot `init()` is simpler than the
// save/restore-via-defer pattern used in `internal/acm/poll_test.go`
// (which exists there because some tests need to assert retry-budget
// timing while others don't).
//
// CONTRACT: nothing in this package's tests should run in parallel with
// production-binary code paths that read these variables. If a future
// test deliberately needs a non-zero delay, switch to the save/restore
// pattern rather than re-setting these globals mid-test.
func init() {
	postMutationWarmup = 0
	postCreateSettleDelay = 0
}

// emptyObjectValue builds a fully-null tftypes object for the schema, used as
// the starting raw for a tfsdk.State/Plan before .Set writes the model.
func emptyObjectValue(ctx context.Context, s rschema.Schema) tftypes.Value {
	objType := s.Type().TerraformType(ctx).(tftypes.Object)
	return tftypes.NewValue(objType, nil)
}

// newState builds a tfsdk.State carrying the given model.
func newState(t *testing.T, s rschema.Schema, m clusterResourceModel) tfsdk.State {
	t.Helper()
	ctx := context.Background()
	st := tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}
	require.False(t, st.Set(ctx, &m).HasError())
	return st
}

// newPlan builds a tfsdk.Plan carrying the given model.
func newPlan(t *testing.T, s rschema.Schema, m clusterResourceModel) tfsdk.Plan {
	t.Helper()
	ctx := context.Background()
	pl := tfsdk.Plan{Schema: s, Raw: emptyObjectValue(ctx, s)}
	d := pl.Set(ctx, &m)
	require.Falsef(t, d.HasError(), "plan.Set diags: %v", d)
	return pl
}

// importState drives ImportState end-to-end.
func importState(t *testing.T, r *clusterResource, s rschema.Schema, id string) resource.ImportStateResponse {
	t.Helper()
	ctx := context.Background()
	resp := resource.ImportStateResponse{
		State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)},
	}
	r.ImportState(ctx, resource.ImportStateRequest{ID: id}, &resp)
	return resp
}

// getStateString reads a top-level string attr out of the import response state.
func getStateString(t *testing.T, resp resource.ImportStateResponse, attr string) string {
	t.Helper()
	var m clusterResourceModel
	require.False(t, resp.State.Get(context.Background(), &m).HasError())
	switch attr {
	case "id":
		return m.ID.ValueString()
	default:
		t.Fatalf("unsupported attr %q", attr)
		return ""
	}
}

// runUpdate drives the Update dispatcher with plan vs prior state.
func runUpdate(t *testing.T, r *clusterResource, plan, state clusterResourceModel) resource.UpdateResponse {
	t.Helper()
	ctx := context.Background()
	s := clusterSchema(t)
	req := resource.UpdateRequest{
		Plan:  newPlan(t, s, plan),
		State: newState(t, s, state),
	}
	resp := resource.UpdateResponse{
		State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)},
	}
	r.Update(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "update diags: %v", resp.Diagnostics)
	return resp
}

// ---- schema / metadata ----

func TestClusterResource_Metadata(t *testing.T) {
	r := NewClusterResource()
	var resp resource.MetadataResponse
	r.Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "altinity"}, &resp)
	assert.Equal(t, "altinity_clickhouse_cluster", resp.TypeName)
}

func clusterSchema(t *testing.T) rschema.Schema {
	t.Helper()
	r := NewClusterResource()
	var resp resource.SchemaResponse
	r.(*clusterResource).Schema(context.Background(), resource.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError(), "schema diags: %v", resp.Diagnostics)
	return resp.Schema
}

func TestClusterResource_SchemaValid(t *testing.T) {
	s := clusterSchema(t)
	// Spot-check that the key attributes exist with the expected shape.
	for _, name := range []string{
		"id", "environment", "name", "node_count", "shards", "replicas",
		"node_type", "memory", "size", "storage_class", "volume_type", "iops",
		"throughput", "disks", "version", "version_image", "zookeeper",
		"keeper_name", "zone_awareness", "azlist", "secure", "port", "http_port",
		"lb_type", "ip_whitelist", "admin_password", "datadog", "backup_options",
		"timeouts",
	} {
		_, ok := s.Attributes[name]
		assert.Truef(t, ok, "schema missing attribute %q", name)
	}

	// admin_password and datadog must be Sensitive (secrets, §9).
	assert.True(t, s.Attributes["admin_password"].IsSensitive(), "admin_password must be Sensitive")
	assert.True(t, s.Attributes["datadog"].IsSensitive(), "datadog must be Sensitive")

	// environment & name are Required.
	assert.True(t, s.Attributes["environment"].IsRequired())
	assert.True(t, s.Attributes["name"].IsRequired())
}

// requiresReplaceDescription is the stable public Description emitted by every
// typed stringplanmodifier/boolplanmodifier/int64planmodifier.RequiresReplace
// (they share the same wording). Detecting it identifies the RequiresReplace
// modifier without driving the full Plan/State machinery the modifier needs.
const requiresReplaceDescription = "If the value of this attribute changes, Terraform will destroy and recreate the resource."

// hasRequiresReplace exercises the RequiresReplace contract end-to-end: it runs
// every plan modifier on the attribute with a populated Plan/State (update
// scenario, value changed) and reports whether any sets RequiresReplace.
func hasRequiresReplace(t *testing.T, s rschema.Schema, name string) bool {
	t.Helper()
	ctx := context.Background()
	switch attr := s.Attributes[name].(type) {
	case rschema.StringAttribute:
		for _, pm := range attr.StringPlanModifiers() {
			if pm.Description(ctx) == requiresReplaceDescription {
				return true
			}
		}
	case rschema.BoolAttribute:
		for _, pm := range attr.BoolPlanModifiers() {
			if pm.Description(ctx) == requiresReplaceDescription {
				return true
			}
		}
	case rschema.Int64Attribute:
		for _, pm := range attr.Int64PlanModifiers() {
			if pm.Description(ctx) == requiresReplaceDescription {
				return true
			}
		}
	case rschema.ListAttribute:
		for _, pm := range attr.ListPlanModifiers() {
			if pm.Description(ctx) == requiresReplaceDescription {
				return true
			}
		}
	default:
		t.Fatalf("attribute %q has unsupported type %T", name, attr)
	}
	return false
}

func TestClusterResource_RequiresReplace(t *testing.T) {
	s := clusterSchema(t)

	// Identity / not-provably-mutable attrs must force replacement (conservative
	// default §7.2). The rank-4 fields (uptime, datadog, uptime_settings,
	// alternate_endpoints) have no confirmed in-place endpoint yet, so they
	// stay RequiresReplace rather than being silently dropped on Update
	// (review #2, design §7.2).
	for _, name := range []string{
		"environment", "name", "zookeeper", "keeper_name",
		"zone_awareness", "secure", "port", "http_port",
		"volume_type", "data_path", "lb_type", "ip_whitelist",
		"uptime", "datadog", "uptime_settings", "alternate_endpoints",
	} {
		assert.Truef(t, hasRequiresReplace(t, s, name),
			"%q should carry a RequiresReplace plan modifier", name)
	}

	// In-place-mutable attrs go through the rescale / upgrade / password
	// dispatcher and must NOT force replacement — destroying a production
	// cluster for a +1-replica scale-out is the bug the spec rejects.
	for _, name := range []string{
		"size", "storage_class", "node_type", "memory", "version",
		"version_image", "node_count", "iops", "throughput", "disks",
		"admin_password",
		"shards", "replicas", "azlist",
	} {
		assert.Falsef(t, hasRequiresReplace(t, s, name),
			"%q is mutable in place and must NOT RequiresReplace", name)
	}
}

// TestClusterResource_RequiresReplaceModifierFires drives the actual modifier
// with a populated update-scenario request to confirm it flips RequiresReplace.
func TestClusterResource_RequiresReplaceModifierFires(t *testing.T) {
	s := clusterSchema(t)
	attr := s.Attributes["name"].(rschema.StringAttribute)
	ctx := context.Background()

	// Build a minimal update-scenario Plan/State (non-null) so the modifier's
	// "planned for update" guard passes and the changed value triggers replace.
	plan := newPlan(t, s, func() clusterResourceModel { m := baseState(); m.Name = types.StringValue("renamed"); return m }())
	state := newState(t, s, baseState())

	var fired bool
	for _, pm := range attr.StringPlanModifiers() {
		req := planmodifier.StringRequest{
			Path:        path.Root("name"),
			ConfigValue: types.StringValue("renamed"),
			PlanValue:   types.StringValue("renamed"),
			StateValue:  types.StringValue("c1"),
			Plan:        plan,
			State:       state,
		}
		var resp planmodifier.StringResponse
		pm.PlanModifyString(ctx, req, &resp)
		if resp.RequiresReplace {
			fired = true
		}
	}
	assert.True(t, fired, "name change must trigger RequiresReplace when planned for update")
}

// ---- ImportState id parsing ----

func TestClusterResource_ImportState_ValidID(t *testing.T) {
	r := NewClusterResource().(*clusterResource)
	s := clusterSchema(t)
	resp := importState(t, r, s, "12345")
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)
	// The id attr was set via passthrough.
	idVal := getStateString(t, resp, "id")
	assert.Equal(t, "12345", idVal)
}

func TestClusterResource_ImportState_InvalidID(t *testing.T) {
	r := NewClusterResource().(*clusterResource)
	s := clusterSchema(t)
	resp := importState(t, r, s, "not-an-int")
	require.True(t, resp.Diagnostics.HasError(), "expected an error for non-integer import id")
	assert.Contains(t, strings.ToLower(resp.Diagnostics.Errors()[0].Summary()), "invalid import id")
}

// ---- Update dispatcher routing ----

// recordingHandler records which mutating endpoints were hit and serves a
// healthy status + a minimal cluster read so the dispatcher can run end-to-end.
type recordingHandler struct {
	mu       sync.Mutex
	upgrade  int
	rescale  int
	backup   int
	statusN  int
	clusterN int
	// User endpoints for the admin-password step.
	usersList   int
	userUpdate  int
	userUpdated string // last login passed to /user/{id} update
	// users returned by the list endpoint; defaults to one "admin" user with id=7
	// if nil so existing tests don't need to set this.
	usersResp string
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := r.URL.Path
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasSuffix(p, "/upgrade"):
		h.upgrade++
		_, _ = w.Write([]byte(`{}`))
	case strings.HasSuffix(p, "/rescale"):
		h.rescale++
		_, _ = w.Write([]byte(`{}`))
	case strings.HasSuffix(p, "/backup"):
		h.backup++
		_, _ = w.Write([]byte(`{}`))
	case strings.HasSuffix(p, "/status"):
		// /cluster/{id}/status is the ACTION endpoint, not the lifecycle one
		// (the lifecycle status is read via GET /cluster/{id} → the default
		// case below). PollUntilIdle queries this URL and waits for
		// action=="Completed" — anything else loops forever.
		h.statusN++
		_, _ = w.Write([]byte(`{"data":{"action":"Completed","actionProgress":{"percent":0},"health":{"total":7,"passed":7}}}`))
	case strings.HasSuffix(p, "/users") && r.Method == http.MethodGet:
		h.usersList++
		body := h.usersResp
		if body == "" {
			body = `{"data":[{"id":7,"login":"admin"}]}`
		}
		_, _ = w.Write([]byte(body))
	case strings.Contains(p, "/user/") && r.Method == http.MethodPost:
		h.userUpdate++
		// Parse {"login":"...","password":"..."} to record what was sent.
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Login string `json:"login"`
		}
		_ = json.Unmarshal(body, &req)
		h.userUpdated = req.Login
		_, _ = w.Write([]byte(`{"data":{"id":7,"login":"admin"}}`))
	default:
		// GET /cluster/{id}
		h.clusterN++
		_, _ = w.Write([]byte(`{"data":{"id":42,"name":"c1","nodes":[],"shards":"1","replicas":"1","systemVersion":"24.3","status":"online"}}`))
	}
}

func newDispatcherResource(t *testing.T, h http.Handler) (*clusterResource, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))
	return &clusterResource{client: client}, srv
}

// nullTimeouts is a typed null object matching the timeouts block schema, so
// model -> tfsdk.Plan/State conversion does not fail on a zero-value Object.
func nullTimeouts() types.Object {
	return types.ObjectNull(map[string]attr.Type{
		"create": types.StringType,
		"update": types.StringType,
		"delete": types.StringType,
	})
}

// baseState is the prior state used by dispatcher routing tests.
func baseState() clusterResourceModel {
	return clusterResourceModel{
		ID:            types.StringValue("42"),
		Environment:   types.StringValue("2267"),
		Name:          types.StringValue("c1"),
		NodeCount:     types.Int64Value(2),
		Shards:        types.Int64Value(1),
		Replicas:      types.Int64Value(1),
		Size:          types.StringValue("10Gi"),
		Version:       types.StringValue("24.3"),
		BackupOptions: types.StringNull(),
		Azlist:        types.ListNull(types.StringType),
		Timeouts:      nullTimeouts(),
	}
}

func TestUpdateDispatch_UpgradeOnly(t *testing.T) {
	plan := baseState()
	plan.Version = types.StringValue("24.8") // version diff only

	h := &recordingHandler{}
	r, _ := newDispatcherResource(t, h)
	runUpdate(t, r, plan, baseState())

	assert.Equal(t, 1, h.upgrade, "version change must fire upgrade")
	assert.Equal(t, 0, h.rescale, "no compute/storage change => no rescale")
	assert.Equal(t, 0, h.backup, "no backup change => no backup call")
}

func TestUpdateDispatch_RescaleOnly(t *testing.T) {
	plan := baseState()
	plan.NodeType = types.StringValue("e2-standard-4") // compute diff

	h := &recordingHandler{}
	r, _ := newDispatcherResource(t, h)
	runUpdate(t, r, plan, baseState())

	assert.Equal(t, 0, h.upgrade)
	assert.Equal(t, 1, h.rescale, "node_type change must fire rescale")
	assert.Equal(t, 0, h.backup)
}

// TestValidateClusterModel_NodeCountMismatch locks in the rule that
// node_count must equal shards × replicas when all three are set. Stale
// node_count alongside an updated replicas previously caused inconsistent
// rescale wire bodies (observed live: `node_count: 2→1` + `replicas: 2→3`).
func TestValidateClusterModel_NodeCountMismatch(t *testing.T) {
	cfg := baseState()
	cfg.Shards = types.Int64Value(1)
	cfg.Replicas = types.Int64Value(3)
	cfg.NodeCount = types.Int64Value(1) // stale — should be 3

	diags := validateClusterModel(cfg)
	require.True(t, diags.HasError(), "mismatched node_count must error at validate time")
	assert.Contains(t, diags.Errors()[0].Detail(), "shards × replicas")
}

func TestValidateClusterModel_NodeCountAligned(t *testing.T) {
	cfg := baseState()
	cfg.Shards = types.Int64Value(2)
	cfg.Replicas = types.Int64Value(3)
	cfg.NodeCount = types.Int64Value(6) // matches

	diags := validateClusterModel(cfg)
	require.False(t, diags.HasError())
}

func TestValidateClusterModel_NodeCountUnset(t *testing.T) {
	// Operator left node_count out of HCL — provider computes it. No diag.
	cfg := baseState()
	cfg.Shards = types.Int64Value(2)
	cfg.Replicas = types.Int64Value(3)
	cfg.NodeCount = types.Int64Null()

	diags := validateClusterModel(cfg)
	require.False(t, diags.HasError())
}

// baseStateConsistent is baseState with node_count == shards × replicas, so
// downstream rules can be tested in isolation without the node_count rule
// firing first.
func baseStateConsistent() clusterResourceModel {
	cfg := baseState()
	cfg.NodeCount = types.Int64Value(1) // 1 shard × 1 replica
	return cfg
}

// TestValidateClusterModel_ZookeeperAndKeeperNameConflict locks in the
// mutual-exclusion check between `zookeeper` and `keeper_name`. ACM treats
// these as a one-of selector; setting both leads to an opaque mid-apply
// failure. Catch it at plan time.
func TestValidateClusterModel_ZookeeperAndKeeperNameConflict(t *testing.T) {
	cfg := baseStateConsistent()
	cfg.Zookeeper = types.StringValue("launch")
	cfg.KeeperName = types.StringValue("shared-keeper")

	diags := validateClusterModel(cfg)
	require.True(t, diags.HasError(), "setting both zookeeper and keeper_name must fail at plan time")
	assert.Contains(t, diags[0].Summary(), "Conflicting coordination")
}

// TestValidateClusterModel_KeeperNameOnly: only keeper_name set — valid.
func TestValidateClusterModel_KeeperNameOnly(t *testing.T) {
	cfg := baseStateConsistent()
	cfg.Zookeeper = types.StringNull()
	cfg.KeeperName = types.StringValue("shared-keeper")
	require.False(t, validateClusterModel(cfg).HasError())
}

// TestValidateClusterModel_ZookeeperOnly: only zookeeper set — valid.
func TestValidateClusterModel_ZookeeperOnly(t *testing.T) {
	cfg := baseStateConsistent()
	cfg.Zookeeper = types.StringValue("launch")
	cfg.KeeperName = types.StringNull()
	require.False(t, validateClusterModel(cfg).HasError())
}

// TestUpdateDispatch_ReplicasRescale locks in the fix for the bug where
// changing `replicas` forced cluster replacement (and cascaded into satellites
// destroying users/profiles/settings). Replicas — and shards, azlist, and
// node_type — must route through the rescale endpoint, not RequiresReplace.
func TestUpdateDispatch_ReplicasRescale(t *testing.T) {
	plan := baseState()
	plan.Replicas = types.Int64Value(2) // 1 -> 2

	h := &recordingHandler{}
	r, _ := newDispatcherResource(t, h)
	runUpdate(t, r, plan, baseState())

	assert.Equal(t, 1, h.rescale, "replicas change must fire rescale, not replace")
	assert.Equal(t, 0, h.upgrade)
	assert.Equal(t, 0, h.backup)
}

func TestUpdateDispatch_ShardsRescale(t *testing.T) {
	plan := baseState()
	plan.Shards = types.Int64Value(2) // 1 -> 2

	h := &recordingHandler{}
	r, _ := newDispatcherResource(t, h)
	runUpdate(t, r, plan, baseState())

	assert.Equal(t, 1, h.rescale, "shards change must fire rescale, not replace")
}

func TestUpdateDispatch_StorageRescale(t *testing.T) {
	plan := baseState()
	plan.Size = types.StringValue("20Gi") // storage diff

	h := &recordingHandler{}
	r, _ := newDispatcherResource(t, h)
	runUpdate(t, r, plan, baseState())
	assert.Equal(t, 1, h.rescale, "size change must fire rescale")
	assert.Equal(t, 0, h.upgrade)
}

func TestUpdateDispatch_BackupOnly(t *testing.T) {
	plan := baseState()
	plan.BackupOptions = types.StringValue(`{"schedule":"daily"}`)

	h := &recordingHandler{}
	r, _ := newDispatcherResource(t, h)
	runUpdate(t, r, plan, baseState())

	assert.Equal(t, 0, h.upgrade)
	assert.Equal(t, 0, h.rescale)
	assert.Equal(t, 1, h.backup, "backup_options change must fire backup")
}

func TestUpdateDispatch_All(t *testing.T) {
	plan := baseState()
	plan.Version = types.StringValue("24.8")
	plan.Replicas = types.Int64Value(3) // topology change triggers rescale
	plan.BackupOptions = types.StringValue(`{"schedule":"daily"}`)

	h := &recordingHandler{}
	r, _ := newDispatcherResource(t, h)
	runUpdate(t, r, plan, baseState())

	assert.Equal(t, 1, h.upgrade)
	assert.Equal(t, 1, h.rescale)
	assert.Equal(t, 1, h.backup)
}

func TestUpdateDispatch_NoChange(t *testing.T) {
	plan := baseState()
	h := &recordingHandler{}
	r, _ := newDispatcherResource(t, h)
	runUpdate(t, r, plan, baseState())

	assert.Equal(t, 0, h.upgrade)
	assert.Equal(t, 0, h.rescale)
	assert.Equal(t, 0, h.backup)
	assert.GreaterOrEqual(t, h.clusterN, 1, "no-op update still performs a converged Read")
}

// TestUpdateDispatch_ResolvesUnknownComputed reproduces the apply-time failure
// "provider returned invalid result object ... unknown value": Computed attrs
// the API does not read back are unknown in the plan (UseStateForUnknown no-ops
// on a null prior-state value), and the Update path must resolve them to known
// (null) before writing state — exactly as Create does.
func TestUpdateDispatch_ResolvesUnknownComputed(t *testing.T) {
	plan := baseState()
	plan.Version = types.StringValue("24.8") // trigger the upgrade path
	plan.IOPS = types.Int64Unknown()
	plan.Memory = types.StringUnknown()
	plan.Throughput = types.Int64Unknown()
	plan.VersionImage = types.StringUnknown()

	h := &recordingHandler{}
	r, _ := newDispatcherResource(t, h)
	resp := runUpdate(t, r, plan, baseState())

	var out clusterResourceModel
	require.False(t, resp.State.Get(context.Background(), &out).HasError())
	assert.False(t, out.IOPS.IsUnknown(), "iops must be known (not unknown) after apply")
	assert.False(t, out.Memory.IsUnknown(), "memory must be known after apply")
	assert.False(t, out.Throughput.IsUnknown(), "throughput must be known after apply")
	assert.False(t, out.VersionImage.IsUnknown(), "version_image must be known after apply")
}

// ---- Admin password in-place update (step 4 of the Update dispatcher) ----

// passwordState carries an admin_user + admin_password pair on top of baseState.
func passwordState(user, pass string) clusterResourceModel {
	s := baseState()
	s.AdminUser = types.StringValue(user)
	s.AdminPass = types.StringValue(pass)
	return s
}

func TestUpdateDispatch_AdminPasswordChange(t *testing.T) {
	plan := passwordState("admin", "new-secret")
	state := passwordState("admin", "old-secret")

	h := &recordingHandler{}
	r, _ := newDispatcherResource(t, h)
	runUpdate(t, r, plan, state)

	assert.Equal(t, 0, h.upgrade)
	assert.Equal(t, 0, h.rescale)
	assert.Equal(t, 0, h.backup)
	assert.Equal(t, 1, h.usersList, "admin_password change must trigger user list")
	assert.Equal(t, 1, h.userUpdate, "admin_password change must call UpdateUser")
	assert.Equal(t, "admin", h.userUpdated, "UpdateUser body must carry the admin login")
}

// When plan and state both omit admin_password (null), the dispatcher must NOT
// touch the user endpoints. This guards against the import-flow false positive
// where state.AdminPass is null and a configured plan password would spuriously
// trigger an update against a state we don't know.
func TestUpdateDispatch_AdminPasswordNullInState_SkipsUpdate(t *testing.T) {
	plan := baseState()
	plan.AdminUser = types.StringValue("admin")
	plan.AdminPass = types.StringValue("first-time-password") // user just added it
	// state still has AdminPass as the zero value (null) — simulating import.
	state := baseState()

	h := &recordingHandler{}
	r, _ := newDispatcherResource(t, h)
	runUpdate(t, r, plan, state)

	assert.Equal(t, 0, h.usersList, "null state password must not trigger list")
	assert.Equal(t, 0, h.userUpdate, "null state password must not trigger update")
}

// Same login resolution after import: when only plan.AdminUser is populated
// (state.AdminUser is null because the imported state never wrote it), the
// dispatcher must use the plan value, not error with "Admin user not found".
func TestUpdateDispatch_AdminPasswordChange_UsesPlanAdminUser(t *testing.T) {
	plan := baseState()
	plan.AdminUser = types.StringValue("admin")
	plan.AdminPass = types.StringValue("new-secret")
	state := baseState()
	state.AdminPass = types.StringValue("old-secret")
	// state.AdminUser remains null — the imported state never wrote it.

	h := &recordingHandler{}
	r, _ := newDispatcherResource(t, h)
	runUpdate(t, r, plan, state)

	assert.Equal(t, 1, h.usersList)
	assert.Equal(t, 1, h.userUpdate)
	assert.Equal(t, "admin", h.userUpdated)
}

// When the cluster has no user matching the configured admin_user login,
// the dispatcher must error rather than picking the wrong user.
func TestUpdateDispatch_AdminPasswordChange_NoMatchingUser(t *testing.T) {
	plan := passwordState("admin", "new-secret")
	state := passwordState("admin", "old-secret")

	h := &recordingHandler{
		usersResp: `{"data":[{"id":9,"login":"someone-else"}]}`,
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	r := &clusterResource{client: acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))}

	ctx := context.Background()
	s := clusterSchema(t)
	req := resource.UpdateRequest{
		Plan:  newPlan(t, s, plan),
		State: newState(t, s, state),
	}
	resp := resource.UpdateResponse{
		State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)},
	}
	r.Update(ctx, req, &resp)

	assert.True(t, resp.Diagnostics.HasError(), "missing admin user must surface an error")
	assert.Equal(t, 1, h.usersList)
	assert.Equal(t, 0, h.userUpdate, "no update call on missing-user error")
}

// ---- pure mapping unit tests (no framework wiring) ----

func TestLaunchRequestFromPlan_Coercions(t *testing.T) {
	p := clusterResourceModel{
		Name:          types.StringValue("c1"),
		NodeCount:     types.Int64Value(3),
		Shards:        types.Int64Value(2),
		Replicas:      types.Int64Value(1),
		Secure:        types.BoolValue(true),
		ZoneAwareness: types.BoolValue(false),
		MysqlProtocol: types.BoolValue(true),
		Port:          types.Int64Value(9440),
		Datadog:       types.StringValue(`{"apiKey":"x"}`),
	}
	req := launchRequestFromPlan(p)
	assert.Equal(t, "c1", req.Name)
	assert.Equal(t, "3", req.Nodes, "node_count must be sent as a string-int")
	assert.Equal(t, "2", req.Shards)
	assert.Equal(t, "1", req.Replicas)
	assert.Equal(t, true, req.Secure, "secure is a JSON boolean")
	assert.Equal(t, false, req.ZoneAwareness)
	assert.Equal(t, true, req.MysqlProtocol)
	assert.Equal(t, 9440, req.Port)
	assert.Equal(t, `{"apiKey":"x"}`, string(req.DatadogSettings))
}

func TestApplyClusterToModel_PreservesSecrets(t *testing.T) {
	m := clusterResourceModel{
		AdminPass: types.StringValue("super-secret"),
		Datadog:   types.StringValue(`{"apiKey":"keep"}`),
		NodeCount: types.Int64Value(2), // desired config, preserved (not read back)
	}
	c := acm.Cluster{
		ID:            42,
		Name:          "c1",
		Shards:        1,
		Replicas:      1,
		SystemVersion: "24.3",
		Status:        "online",
		IDEnvironment: 2267,
	}
	applyClusterToModel(&m, c)

	assert.Equal(t, "42", m.ID.ValueString())
	assert.Equal(t, int64(2), m.NodeCount.ValueInt64(), "node_count preserved from config")
	// version was unset (Optional+Computed) so it is populated from the running
	// version; system_version always reflects the running version.
	assert.Equal(t, "24.3", m.Version.ValueString())
	assert.Equal(t, "24.3", m.SystemVersion.ValueString())
	assert.Equal(t, "2267", m.Environment.ValueString())
	// Secrets preserved, never overwritten by the API read (§7.1/§9).
	assert.Equal(t, "super-secret", m.AdminPass.ValueString())
	assert.Equal(t, `{"apiKey":"keep"}`, m.Datadog.ValueString())
}

// review #1: a DESIRED version set by the user must NOT be overwritten by the
// running version reported by the API, or every Read produces a perpetual diff
// that re-triggers upgrade.
func TestApplyClusterToModel_PreservesDesiredVersion(t *testing.T) {
	m := clusterResourceModel{Version: types.StringValue("24.8")}
	c := acm.Cluster{ID: 7, SystemVersion: "24.8.1.2"}

	applyClusterToModel(&m, c)

	assert.Equal(t, "24.8", m.Version.ValueString(), "desired version preserved")
	assert.Equal(t, "24.8.1.2", m.SystemVersion.ValueString(), "running version reflected separately")
}

func TestDiffHelpers(t *testing.T) {
	base := baseState()

	// upgrade
	up := base
	up.Version = types.StringValue("24.8")
	_, changed := upgradeDiff(up, base)
	assert.True(t, changed)
	_, changed = upgradeDiff(base, base)
	assert.False(t, changed)

	// rescale — node_count is NOT a trigger any more (derived from shards × replicas);
	// changing replicas alone must fire rescale and put the new value on the wire.
	rs := base
	rs.Replicas = types.Int64Value(3)
	req, changed := rescaleDiff(rs, base)
	assert.True(t, changed)
	assert.Equal(t, "3", req.Replicas)

	// A node_count-only change must NOT trigger rescale (the validator rejects
	// inconsistent configs at plan time, so this only fires for self-consistent
	// no-op redundancy in HCL).
	nc := base
	nc.NodeCount = types.Int64Value(8)
	_, changed = rescaleDiff(nc, base)
	assert.False(t, changed, "node_count is derived; changing it alone must not trigger rescale")

	// unknown plan value => treated as no change (avoids spurious mutation)
	un := base
	un.Version = types.StringUnknown()
	_, changed = upgradeDiff(un, base)
	assert.False(t, changed, "unknown plan value must not trigger a sub-mutation")
}

// ---- Create / Read / Delete lifecycle (httptest) ----

func TestClusterResource_CreateLifecycle(t *testing.T) {
	var launched, read int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/clusters/launch"):
			launched++
			_, _ = w.Write([]byte(`{"data":{"id":99,"name":"c1","nodes":"2","shards":"1","replicas":"1"}}`))
		case strings.HasSuffix(r.URL.Path, "/clusters"): // idempotency pre-check (env list)
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "/status"):
			// Operation-status endpoint: report idle so Create's PollUntilIdle
			// belt-and-suspenders check returns immediately.
			_, _ = w.Write([]byte(`{"data":{"action":"Completed","actionProgress":{"percent":0},"health":{"total":7,"passed":7}}}`))
		default: // GET /cluster/99 — both the lifecycle poll (GetClusterStatus->GetCluster) and the final Read hit this
			read++
			_, _ = w.Write([]byte(`{"data":{"id":99,"name":"c1","nodes":"2","shards":"1","replicas":"1","systemVersion":"24.3","status":"online","idEnvironment":2267}}`))
		}
	}))
	t.Cleanup(srv.Close)

	r := &clusterResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := clusterSchema(t)
	ctx := context.Background()

	plan := baseState()
	plan.ID = types.StringNull() // unknown on create
	plan.AdminPass = types.StringValue("pw")

	req := resource.CreateRequest{Plan: newPlan(t, s, plan)}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.Create(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "create diags: %v", resp.Diagnostics)

	assert.Equal(t, 1, launched)
	// The lifecycle poll reads the cluster object's status (GetClusterStatus ->
	// GetCluster -> GET /cluster/99), then a final converged Read hits it again,
	// so the GET endpoint is exercised at least twice.
	assert.GreaterOrEqual(t, read, 2, "must poll status to healthy, then full-Read")

	var out clusterResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "99", out.ID.ValueString(), "id persisted to state")
	assert.Equal(t, "online", out.Status.ValueString())
	assert.Equal(t, "pw", out.AdminPass.ValueString(), "secret preserved from plan")
}

func TestClusterResource_ReadDriftRemoves(t *testing.T) {
	// List-based existence check: the cluster (id 42) is absent from its
	// environment's cluster list, so Read treats it as drift and removes it.
	// (ACM 403s a GET of a missing id, so we never rely on per-id GET here.)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":7,"name":"other"}]}`))
	}))
	t.Cleanup(srv.Close)

	r := &clusterResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := clusterSchema(t)
	ctx := context.Background()

	req := resource.ReadRequest{State: newState(t, s, baseState())}
	resp := resource.ReadResponse{State: newState(t, s, baseState())}
	r.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError())
	assert.True(t, resp.State.Raw.IsNull(), "absent-from-env-list read must remove the resource from state")
}

func TestClusterResource_ReadPreservesSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// List-based read: the env's cluster list includes id 42. API never
		// returns adminPass.
		_, _ = w.Write([]byte(`{"data":[{"id":42,"name":"c1","nodes":"2","shards":"1","replicas":"1","systemVersion":"24.3","status":"online","idEnvironment":2267}]}`))
	}))
	t.Cleanup(srv.Close)

	r := &clusterResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := clusterSchema(t)
	ctx := context.Background()

	prior := baseState()
	prior.AdminPass = types.StringValue("kept-secret")
	prior.Datadog = types.StringValue(`{"apiKey":"kept"}`)

	req := resource.ReadRequest{State: newState(t, s, prior)}
	resp := resource.ReadResponse{State: newState(t, s, prior)}
	r.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError())

	var out clusterResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "kept-secret", out.AdminPass.ValueString(), "secret preserved from prior state on Read")
	assert.Equal(t, `{"apiKey":"kept"}`, out.Datadog.ValueString())
	assert.Equal(t, "online", out.Status.ValueString())
}

func TestClusterResource_DeleteLifecycle(t *testing.T) {
	var terminated int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			terminated++
			_, _ = w.Write([]byte(`{}`))
			return
		}
		// GET /environment/{env}/clusters -> the cluster is absent from the env
		// list (the unambiguous "gone" signal; ACM returns 403 — not 404 — for a
		// per-id GET of a deleted cluster, which is why Delete polls the list).
		// PollUntilGoneBy's immediate check succeeds, so the 15s tick never fires.
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	t.Cleanup(srv.Close)

	r := &clusterResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := clusterSchema(t)
	ctx := context.Background()

	state := baseState()
	req := resource.DeleteRequest{State: newState(t, s, state)}
	resp := resource.DeleteResponse{State: newState(t, s, state)}
	r.Delete(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "delete diags: %v", resp.Diagnostics)
	assert.Equal(t, 1, terminated, "must call terminate once")
}

// ---- timeouts parsing ----

func TestResolveTimeouts(t *testing.T) {
	ctx := context.Background()

	// null block => defaults.
	to, d := resolveTimeouts(ctx, nullTimeouts())
	require.False(t, d.HasError())
	assert.Equal(t, defaultCreateTimeout, to.create)
	assert.Equal(t, defaultUpdateTimeout, to.update)
	assert.Equal(t, defaultDeleteTimeout, to.delete)

	// custom values parse.
	obj, diags := types.ObjectValue(
		map[string]attr.Type{"create": types.StringType, "update": types.StringType, "delete": types.StringType},
		map[string]attr.Value{
			"create": types.StringValue("5m"),
			"update": types.StringValue("10m"),
			"delete": types.StringNull(),
		},
	)
	require.False(t, diags.HasError())
	to, d = resolveTimeouts(ctx, obj)
	require.False(t, d.HasError())
	assert.Equal(t, 5*time.Minute, to.create)
	assert.Equal(t, 10*time.Minute, to.update)
	assert.Equal(t, defaultDeleteTimeout, to.delete, "unset delete falls back to default")

	// invalid duration => diagnostic.
	bad, _ := types.ObjectValue(
		map[string]attr.Type{"create": types.StringType, "update": types.StringType, "delete": types.StringType},
		map[string]attr.Value{
			"create": types.StringValue("not-a-duration"),
			"update": types.StringNull(),
			"delete": types.StringNull(),
		},
	)
	_, d = resolveTimeouts(ctx, bad)
	assert.True(t, d.HasError(), "invalid duration must produce a diagnostic")
}

// TestClusterResource_Create_NoUnknownState reproduces the "Provider returned
// invalid result object after apply" bug: Optional+Computed attributes the user
// omits are unknown in the plan, and neither the user nor applyClusterToModel
// sets them, so they stay unknown in the state Create writes. Terraform requires
// all values known after apply. This omits the attributes the API does not read
// back and asserts none remain unknown.
func TestClusterResource_Create_NoUnknownState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/clusters/launch"):
			_, _ = w.Write([]byte(`{"data":{"id":99,"name":"c1"}}`))
		case strings.HasSuffix(r.URL.Path, "/clusters"): // idempotency pre-check (env list)
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "/status"):
			// Operation-status endpoint; idle so PollUntilIdle returns immediately.
			_, _ = w.Write([]byte(`{"data":{"action":"Completed","actionProgress":{"percent":0},"health":{"total":7,"passed":7}}}`))
		default:
			_, _ = w.Write([]byte(`{"data":{"id":99,"name":"c1","systemVersion":"24.3","status":"online","idEnvironment":2267}}`))
		}
	}))
	t.Cleanup(srv.Close)

	r := &clusterResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := clusterSchema(t)
	ctx := context.Background()

	// Minimal plan: required attrs + admin_password known. Computed attrs the API
	// does not read back are unknown in the plan (as if omitted). volume_type/
	// data_path/zookeeper/ip_whitelist are Optional-only (NOT Computed) — a real
	// plan supplies them as null, never unknown — so they are set null here.
	plan := baseState()
	plan.ID = types.StringNull()
	plan.AdminPass = types.StringValue("pw")
	plan.NodeType = types.StringUnknown()
	plan.Memory = types.StringUnknown()
	plan.Size = types.StringUnknown()
	plan.StorageClass = types.StringUnknown()
	plan.VolumeType = types.StringNull()
	plan.IOPS = types.Int64Unknown()
	plan.Throughput = types.Int64Unknown()
	plan.Disks = types.Int64Unknown()
	plan.DataPath = types.StringNull()
	plan.VersionImage = types.StringUnknown()
	plan.Zookeeper = types.StringNull()
	plan.Port = types.Int64Unknown()
	plan.HTTPPort = types.Int64Unknown()
	plan.LBType = types.StringUnknown()
	plan.IPWhitelist = types.StringNull()

	req := resource.CreateRequest{Plan: newPlan(t, s, plan)}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.Create(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "create diags: %v", resp.Diagnostics)

	var out clusterResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	// Every attribute the API does not read back must be resolved to a known
	// value (null), not left unknown.
	unknowns := map[string]bool{
		"node_type": out.NodeType.IsUnknown(), "memory": out.Memory.IsUnknown(),
		"size": out.Size.IsUnknown(), "storage_class": out.StorageClass.IsUnknown(),
		"volume_type": out.VolumeType.IsUnknown(), "iops": out.IOPS.IsUnknown(),
		"throughput": out.Throughput.IsUnknown(), "disks": out.Disks.IsUnknown(),
		"data_path": out.DataPath.IsUnknown(), "version_image": out.VersionImage.IsUnknown(),
		"zookeeper": out.Zookeeper.IsUnknown(),
		"port":      out.Port.IsUnknown(), "http_port": out.HTTPPort.IsUnknown(),
		"lb_type": out.LBType.IsUnknown(), "ip_whitelist": out.IPWhitelist.IsUnknown(),
	}
	for name, isUnknown := range unknowns {
		assert.Falsef(t, isUnknown, "%q must be known (not unknown) after apply", name)
	}
}

func TestClusterRoleValidator(t *testing.T) {
	ctx := context.Background()
	check := func(v string) bool {
		var resp fwvalidator.StringResponse
		clusterRoleValidator{}.ValidateString(ctx, fwvalidator.StringRequest{ConfigValue: types.StringValue(v)}, &resp)
		return resp.Diagnostics.HasError()
	}
	assert.False(t, check("dev"), "dev is valid")
	assert.False(t, check("prod"), "prod is valid")
	assert.True(t, check("development"), "UI label is rejected by the API")
	assert.True(t, check("production"), "UI label is rejected by the API")
	assert.True(t, check("PROD"), "case-sensitive")
}

func TestValidateAdoptedCluster(t *testing.T) {
	// fullPlan is a plan with every checked attribute set to a known value.
	fullPlan := func() clusterResourceModel {
		return clusterResourceModel{
			Environment: types.StringValue("42"),
			Type:        types.StringValue("clickhouse"),
			Role:        types.StringValue("prod"),
			Shards:      types.Int64Value(3),
			Replicas:    types.Int64Value(2),
			KeeperName:  types.StringValue("keeper-a"),
		}
	}
	// matchingAPI returns a Cluster that matches fullPlan.
	matchingAPI := func() acm.Cluster {
		return acm.Cluster{
			IDEnvironment: 42,
			Type:          "clickhouse",
			Role:          "prod",
			Shards:        3,
			Replicas:      2,
			KeeperName:    "keeper-a",
		}
	}

	cases := []struct {
		name        string
		plan        clusterResourceModel
		api         acm.Cluster
		wantErr     bool
		errContains []string
		errExcludes []string
	}{
		{
			name:    "all matching - no error",
			plan:    fullPlan(),
			api:     matchingAPI(),
			wantErr: false,
		},
		{
			name: "type mismatch errors on type",
			plan: fullPlan(),
			api: func() acm.Cluster {
				c := matchingAPI()
				c.Type = "kafka"
				return c
			}(),
			wantErr:     true,
			errContains: []string{"type"},
		},
		{
			name: "env mismatch errors on environment",
			plan: fullPlan(),
			api: func() acm.Cluster {
				c := matchingAPI()
				c.IDEnvironment = 99
				return c
			}(),
			wantErr:     true,
			errContains: []string{"environment"},
		},
		{
			name: "plan null fields skip their checks",
			plan: func() clusterResourceModel {
				p := fullPlan()
				p.Type = types.StringNull()
				p.Role = types.StringNull()
				return p
			}(),
			api: func() acm.Cluster {
				c := matchingAPI()
				c.Type = "kafka"
				c.Role = "dev"
				return c
			}(),
			wantErr: false,
		},
		{
			name: "plan unknown fields skip their checks",
			plan: func() clusterResourceModel {
				p := fullPlan()
				p.Shards = types.Int64Unknown()
				return p
			}(),
			api: func() acm.Cluster {
				c := matchingAPI()
				c.Shards = 99
				return c
			}(),
			wantErr: false,
		},
		{
			name: "API empty string skipped (env, type, role, keeper_name)",
			plan: fullPlan(),
			api: acm.Cluster{
				// IDEnvironment=0 also means "ACM didn't return it"
				Type:       "",
				Role:       "",
				KeeperName: "",
				Shards:     3,
				Replicas:   2,
			},
			wantErr: false,
		},
		{
			name: "API zero shards/replicas skipped",
			plan: fullPlan(),
			api: func() acm.Cluster {
				c := matchingAPI()
				c.Shards = 0
				c.Replicas = 0
				return c
			}(),
			wantErr: false,
		},
		{
			name: "multiple mismatches enumerated",
			plan: fullPlan(),
			api: func() acm.Cluster {
				c := matchingAPI()
				c.Type = "kafka"
				c.Role = "dev"
				c.Shards = 5
				return c
			}(),
			wantErr:     true,
			errContains: []string{"type", "role", "shards"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAdoptedCluster(tc.plan, tc.api)
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, s := range tc.errContains {
				assert.Contains(t, err.Error(), s, "expected error to mention %q", s)
			}
			for _, s := range tc.errExcludes {
				assert.NotContains(t, err.Error(), s, "did not expect error to mention %q", s)
			}
		})
	}
}

func TestVersionDowngradeGuard(t *testing.T) {
	ctx := context.Background()
	// isBlocked reports whether changing version old->new is rejected.
	isBlocked := func(old, new types.String) bool {
		var resp planmodifier.StringResponse
		versionDowngradeGuard{}.PlanModifyString(ctx, planmodifier.StringRequest{
			Path:       path.Root("version"),
			StateValue: old,
			PlanValue:  new,
		}, &resp)
		return resp.Diagnostics.HasError()
	}
	v := types.StringValue

	// Genuine downgrades are blocked.
	assert.True(t, isBlocked(v("25.8.16.10002.altinitystable"), v("25.3.8.10042.altinitystable")), "25.8 -> 25.3 is a downgrade")
	assert.True(t, isBlocked(v("25.8.23.13"), v("25.8.16.10002.altinitystable")), "patch 23 -> 16 is a downgrade")
	// Antalya (higher build) -> same-version Stable (lower build) is a downgrade.
	assert.True(t, isBlocked(v("25.8.16.20002.altinityantalya"), v("25.8.16.10002.altinitystable")), "Antalya -> Stable is a downgrade")

	// Upgrades and no-ops are allowed.
	assert.False(t, isBlocked(v("25.3.8.10042.altinitystable"), v("25.8.16.10002.altinitystable")), "25.3 -> 25.8 is an upgrade")
	assert.False(t, isBlocked(v("25.8.16.10002.altinitystable"), v("25.8.16.20002.altinityantalya")), "Stable -> Antalya is allowed")
	assert.False(t, isBlocked(v("25.8.16.10002.altinitystable"), v("25.8.16.10002.altinitystable")), "same version is a no-op")

	// Create (null prior state) and unresolved values must never error.
	assert.False(t, isBlocked(types.StringNull(), v("25.8.16.10002.altinitystable")), "create has no prior version")
	assert.False(t, isBlocked(v("25.8.16.10002.altinitystable"), types.StringUnknown()), "unknown plan value is skipped")
}
