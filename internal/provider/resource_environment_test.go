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
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

func environmentSchema(t *testing.T) rschema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	NewEnvironmentResource().(*environmentResource).Schema(context.Background(), resource.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError(), "schema diags: %v", resp.Diagnostics)
	return resp.Schema
}

func newEnvPlan(t *testing.T, s rschema.Schema, m environmentResourceModel) tfsdk.Plan {
	t.Helper()
	ctx := context.Background()
	pl := tfsdk.Plan{Schema: s, Raw: emptyObjectValue(ctx, s)}
	require.False(t, pl.Set(ctx, &m).HasError())
	return pl
}

func newEnvState(t *testing.T, s rschema.Schema, m environmentResourceModel) tfsdk.State {
	t.Helper()
	ctx := context.Background()
	st := tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}
	require.False(t, st.Set(ctx, &m).HasError())
	return st
}

// freshEnvPlan is a create plan with all computed fields Unknown.
func freshEnvPlan() environmentResourceModel {
	return environmentResourceModel{
		ID:                 types.StringUnknown(),
		Name:               types.StringValue("tf-test-env"),
		CloudProvider:      types.StringValue("gcp"),
		Region:             types.StringValue("us-east1"),
		DisplayName:        types.StringUnknown(),
		NormalizedName:     types.StringUnknown(),
		Type:               types.StringUnknown(),
		Domain:             types.StringUnknown(),
		Status:             types.StringUnknown(),
		State:              types.StringUnknown(),
		Datadog:            nil,
		MaintenanceWindows: types.ListNull(types.ObjectType{AttrTypes: maintenanceWindowAttrTypes()}),
		Timeouts:           nullTimeouts(),
	}
}

// mwList builds a maintenance_windows list value from models, for test plans.
func mwList(t *testing.T, windows ...maintenanceWindowModel) types.List {
	t.Helper()
	objType := types.ObjectType{AttrTypes: maintenanceWindowAttrTypes()}
	if windows == nil {
		windows = []maintenanceWindowModel{}
	}
	l, d := types.ListValueFrom(context.Background(), objType, windows)
	require.False(t, d.HasError(), "mwList: %v", d)
	return l
}

// daysList builds a list(string) value for a window's days.
func daysList(t *testing.T, days ...string) types.List {
	t.Helper()
	l, d := types.ListValueFrom(context.Background(), types.StringType, days)
	require.False(t, d.HasError(), "daysList: %v", d)
	return l
}

// datadogPlan returns a fresh create plan with a configured datadog block.
func datadogPlan() environmentResourceModel {
	p := freshEnvPlan()
	p.Datadog = &datadogModel{
		Enabled:         types.BoolValue(true),
		APIKey:          types.StringValue("synthetic-key"),
		Region:          types.StringValue("datadoghq.com"),
		SendMetrics:     types.BoolValue(true),
		SendLogs:        types.BoolValue(true),
		SendTableStats:  types.BoolValue(false),
		ApplyToClusters: types.BoolUnknown(), // default-true path
	}
	return p
}

// envCreateServer serves the request/list/show endpoints for a fresh create,
// capturing the follow-up EnvironmentEdit body in *editBody.
func envCreateServer(t *testing.T, editBody *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/environments" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`)) // adopt probe: not found
		case r.URL.Path == "/environments/request" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"data":{"id":"700","name":"tf-test-env","status":"provisioning"}}`))
		case strings.HasPrefix(r.URL.Path, "/environment/") && r.Method == http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, editBody)
			_, _ = w.Write([]byte(`{"data":{"id":"700","status":"online"}}`))
		case strings.HasPrefix(r.URL.Path, "/environment/") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(envReady))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEnvironmentResource_CreateWithDatadog(t *testing.T) {
	var editBody map[string]any
	srv := envCreateServer(t, &editBody)
	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	req := resource.CreateRequest{Plan: newEnvPlan(t, s, datadogPlan())}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.Create(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "create diags: %v", resp.Diagnostics)

	dd, ok := editBody["datadogSettings"].(map[string]any)
	require.True(t, ok, "datadogSettings must be sent")
	assert.Equal(t, true, dd["enabled"])
	assert.Equal(t, "synthetic-key", dd["key"])
	assert.Equal(t, true, dd["metrics"])
	assert.Equal(t, map[string]any{"datadog": true}, editBody["applyToClusters"], "apply_to_clusters default true")

	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	require.NotNil(t, out.Datadog)
	assert.Equal(t, "synthetic-key", out.Datadog.APIKey.ValueString(), "api_key preserved from config")
	assert.True(t, out.Datadog.ApplyToClusters.ValueBool(), "apply_to_clusters resolved to true")
}

func TestEnvironmentResource_CreateWithMaintenanceWindows(t *testing.T) {
	var editBody map[string]any
	srv := envCreateServer(t, &editBody)
	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	plan := freshEnvPlan()
	plan.MaintenanceWindows = mwList(t, maintenanceWindowModel{
		Name: types.StringValue("w1"), Enabled: types.BoolValue(true), Hour: types.Int64Value(16),
		LengthHours: types.Int64Value(4), Days: daysList(t, "FRIDAY", "SATURDAY"),
	})
	req := resource.CreateRequest{Plan: newEnvPlan(t, s, plan)}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.Create(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "create diags: %v", resp.Diagnostics)

	mw, ok := editBody["maintenanceWindowSchedules"].([]any)
	require.True(t, ok, "maintenanceWindowSchedules must be sent")
	require.Len(t, mw, 1)
	w := mw[0].(map[string]any)
	assert.Equal(t, "w1", w["name"])
	assert.Equal(t, float64(16), w["hour"])
	assert.Equal(t, float64(4), w["lengthInHours"])
	assert.ElementsMatch(t, []any{"FRIDAY", "SATURDAY"}, w["days"])
}

func TestEnvironmentResource_MaintenanceNullVsEmpty(t *testing.T) {
	ctx := context.Background()
	// null → field omitted (unmanaged).
	reqNull, dNull := buildEnvEditRequest(ctx, freshEnvPlan())
	require.False(t, dNull.HasError())
	assert.Nil(t, reqNull.MaintenanceWindowSchedules, "null list must not send the field")

	// [] → non-nil pointer to empty slice (clear all).
	planEmpty := freshEnvPlan()
	planEmpty.MaintenanceWindows = mwList(t) // empty
	reqEmpty, dEmpty := buildEnvEditRequest(ctx, planEmpty)
	require.False(t, dEmpty.HasError())
	require.NotNil(t, reqEmpty.MaintenanceWindowSchedules, "empty list must send [] (clear)")
	assert.Len(t, *reqEmpty.MaintenanceWindowSchedules, 0)
}

func TestEnvironmentResource_ReadPreservesApiKey(t *testing.T) {
	// GET returns datadog with a DIFFERENT key; api_key must stay the configured value.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"700","name":"tf-test-env","type":"kubernetes","status":"online",` +
			`"datadogSettings":{"enabled":true,"key":"SERVER-SIDE-DIFFERENT","region":"datadoghq.com","metrics":true,"logs":false,"tableStats":false}}}`))
	}))
	t.Cleanup(srv.Close)
	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	state := datadogPlan()
	state.ID = types.StringValue("700")
	state.Datadog.ApplyToClusters = types.BoolValue(true)
	req := resource.ReadRequest{State: newEnvState(t, s, state)}
	resp := resource.ReadResponse{State: newEnvState(t, s, state)}
	r.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "read diags: %v", resp.Diagnostics)

	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	require.NotNil(t, out.Datadog)
	assert.Equal(t, "synthetic-key", out.Datadog.APIKey.ValueString(), "api_key NOT overwritten by API")
	assert.True(t, out.Datadog.Enabled.ValueBool())
	assert.False(t, out.Datadog.SendLogs.ValueBool(), "send_logs reconciled from API")
}

func TestEnvironmentResource_WeekdayValidatorRejectsBadDay(t *testing.T) {
	var resp validator.ListResponse
	days := daysList(t, "FRIDAY", "funday")
	weekdayListValidator{}.ValidateList(context.Background(), validator.ListRequest{
		Path: path.Root("maintenance_windows"), ConfigValue: days,
	}, &resp)
	assert.True(t, resp.Diagnostics.HasError(), "lowercase/invalid weekday must be rejected")
}

func TestEnvironmentResource_Metadata(t *testing.T) {
	var resp resource.MetadataResponse
	NewEnvironmentResource().Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "altinity"}, &resp)
	assert.Equal(t, "altinity_environment", resp.TypeName)
}

// envReady is the EnvironmentShow body of a ready environment.
const envReady = `{"data":{"id":"700","name":"tf-test-env","displayName":"tf-test-env","normalizedName":"tf-test-env","type":"kubernetes","domain":"tf-test-env.altinity.cloud","status":"online"}}`

// TestEnvironmentResource_CreateFresh: no env by name -> request -> poll -> state.
func TestEnvironmentResource_CreateFresh(t *testing.T) {
	requested := false
	var reqBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/environments/request" && r.Method == http.MethodPost:
			requested = true
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &reqBody)
			_, _ = w.Write([]byte(`{"data":{"id":"700","name":"tf-test-env","status":"provisioning"}}`))
		case r.URL.Path == "/environments" && r.Method == http.MethodGet:
			// adopt-by-name probe: not found before request.
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasPrefix(r.URL.Path, "/environment/") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(envReady))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	req := resource.CreateRequest{Plan: newEnvPlan(t, s, freshEnvPlan())}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.Create(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "create diags: %v", resp.Diagnostics)

	assert.True(t, requested, "must call EnvironmentRequest on a fresh create")
	// cloud_provider is upper-cased on the wire; region routed to the matching field.
	assert.Equal(t, "GCP", reqBody["cloud_provider"])
	assert.Equal(t, "us-east1", reqBody["gcp_region"])
	_, hasAWS := reqBody["aws_region"]
	assert.False(t, hasAWS, "non-matching region fields must be omitted")

	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "700", out.ID.ValueString())
	assert.Equal(t, "online", out.Status.ValueString())
	assert.Equal(t, "gcp", out.CloudProvider.ValueString()) // preserved from plan
	assert.Equal(t, "us-east1", out.Region.ValueString())   // preserved from plan
}

// TestEnvironmentResource_CreateResume: env already exists by name ->
// EnvironmentRequest is NOT called -> poll resumes -> state set.
func TestEnvironmentResource_CreateResume(t *testing.T) {
	requested := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/environments/request":
			requested = true
			t.Errorf("EnvironmentRequest must NOT be called when the env already exists")
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/environments" && r.Method == http.MethodGet:
			// adopt-by-name probe finds the in-flight env.
			_, _ = w.Write([]byte(`{"data":[{"id":"700","name":"tf-test-env","status":"online"}]}`))
		case strings.HasPrefix(r.URL.Path, "/environment/") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(envReady))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	req := resource.CreateRequest{Plan: newEnvPlan(t, s, freshEnvPlan())}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.Create(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "create diags: %v", resp.Diagnostics)
	assert.False(t, requested)

	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "700", out.ID.ValueString())
}

// TestEnvironmentResource_CreatePollTimeoutLeavesNoState: the resumability
// contract — a never-ready env + a tiny create timeout must error AND leave
// state empty so the next apply adopts-by-name rather than replacing.
func TestEnvironmentResource_CreatePollTimeoutLeavesNoState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/environments/request":
			_, _ = w.Write([]byte(`{"data":{"id":"700","name":"tf-test-env","status":"provisioning"}}`))
		case r.URL.Path == "/environments":
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasPrefix(r.URL.Path, "/environment/"):
			// never ready
			_, _ = w.Write([]byte(`{"data":{"id":"700","name":"tf-test-env","status":"provisioning"}}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	plan := freshEnvPlan()
	plan.Timeouts = shortCreateTimeout(t)

	req := resource.CreateRequest{Plan: newEnvPlan(t, s, plan)}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.Create(ctx, req, &resp)

	require.True(t, resp.Diagnostics.HasError(), "expected a timeout error")
	// State must be empty/null so Terraform records nothing -> resume on re-apply.
	assert.True(t, resp.State.Raw.IsNull(), "state must not be set on poll timeout (resumability)")
}

// TestEnvironmentResource_DeleteWarnsAndMakesNoAPICall: environment deletion is
// not automated (it needs an out-of-band email + MFA confirmation), so Delete
// must NOT call the API — it returns a warning and lets the framework drop the
// resource from state.
func TestEnvironmentResource_DeleteWarnsAndMakesNoAPICall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("Delete must not call the ACM API; got %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	state := freshEnvPlan()
	state.ID = types.StringValue("700")
	req := resource.DeleteRequest{State: newEnvState(t, s, state)}
	resp := resource.DeleteResponse{State: newEnvState(t, s, state)}
	r.Delete(ctx, req, &resp)

	require.False(t, resp.Diagnostics.HasError(), "delete must not error: %v", resp.Diagnostics)
	require.Equal(t, 1, resp.Diagnostics.WarningsCount(), "delete must warn that the env is not deleted")
	assert.Contains(t, resp.Diagnostics.Warnings()[0].Summary(), "not deleted")
}

// TestEnvironmentResource_ReadDriftRemovesState: a 404 on read drops the resource.
func TestEnvironmentResource_ReadDriftRemovesState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Not found","code":404}`))
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	state := freshEnvPlan()
	state.ID = types.StringValue("700")
	req := resource.ReadRequest{State: newEnvState(t, s, state)}
	resp := resource.ReadResponse{State: newEnvState(t, s, state)}
	r.Read(ctx, req, &resp)

	require.False(t, resp.Diagnostics.HasError())
	assert.True(t, resp.State.Raw.IsNull(), "404 read must remove the resource from state")
}

// TestEnvironmentResource_UpdateDisplayName: editing display_name calls
// EnvironmentEdit and reflects the read-back value.
func TestEnvironmentResource_UpdateDisplayName(t *testing.T) {
	edited := false
	var editBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/environment/") && r.Method == http.MethodPost:
			edited = true
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &editBody)
			_, _ = w.Write([]byte(`{"data":{"id":"700","name":"tf-test-env","displayName":"New Name","status":"online"}}`))
		case strings.HasPrefix(r.URL.Path, "/environment/") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":{"id":"700","name":"tf-test-env","displayName":"New Name","status":"online"}}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	plan := freshEnvPlan()
	plan.ID = types.StringValue("700")
	plan.DisplayName = types.StringValue("New Name")

	req := resource.UpdateRequest{Plan: newEnvPlan(t, s, plan)}
	resp := resource.UpdateResponse{State: newEnvState(t, s, plan)}
	r.Update(ctx, req, &resp)

	require.False(t, resp.Diagnostics.HasError(), "update diags: %v", resp.Diagnostics)
	assert.True(t, edited)
	assert.Equal(t, "New Name", editBody["displayName"])

	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "New Name", out.DisplayName.ValueString())
}

// TestEnvironmentResource_ImportState: import by id passes the id through.
func TestEnvironmentResource_ImportState(t *testing.T) {
	r := &environmentResource{}
	s := environmentSchema(t)
	ctx := context.Background()

	req := resource.ImportStateRequest{ID: "700"}
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.ImportState(ctx, req, &resp)

	require.False(t, resp.Diagnostics.HasError(), "import diags: %v", resp.Diagnostics)
	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "700", out.ID.ValueString())
}

// shortCreateTimeout returns a timeouts object with a 1ms create timeout to
// force the poll to deadline immediately.
func shortCreateTimeout(t *testing.T) types.Object {
	t.Helper()
	obj, d := types.ObjectValue(timeoutsAttrTypes(), map[string]attr.Value{
		"create": types.StringValue("1ms"),
		"update": types.StringNull(),
		"delete": types.StringNull(),
	})
	require.False(t, d.HasError())
	return obj
}
