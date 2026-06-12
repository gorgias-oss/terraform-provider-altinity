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
		MaintenanceWindows: types.SetNull(types.ObjectType{AttrTypes: maintenanceWindowAttrTypes()}),
		Timeouts:           nullTimeouts(),
	}
}

// mwSet builds a maintenance_windows set value from models, for test plans.
func mwSet(t *testing.T, windows ...maintenanceWindowModel) types.Set {
	t.Helper()
	objType := types.ObjectType{AttrTypes: maintenanceWindowAttrTypes()}
	if windows == nil {
		windows = []maintenanceWindowModel{}
	}
	s, d := types.SetValueFrom(context.Background(), objType, windows)
	require.False(t, d.HasError(), "mwSet: %v", d)
	return s
}

// daysSet builds a set(string) value for a window's days.
func daysSet(t *testing.T, days ...string) types.Set {
	t.Helper()
	s, d := types.SetValueFrom(context.Background(), types.StringType, days)
	require.False(t, d.HasError(), "daysSet: %v", d)
	return s
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
			_, _ = w.Write([]byte(`{"data":[]}`)) // existence probe: not found
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
	plan.MaintenanceWindows = mwSet(t, maintenanceWindowModel{
		Name: types.StringValue("w1"), Enabled: types.BoolValue(true), Hour: types.Int64Value(16),
		LengthHours: types.Int64Value(4), Days: daysSet(t, "FRIDAY", "SATURDAY"),
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
	planEmpty.MaintenanceWindows = mwSet(t) // empty
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

// TestEnvironmentResource_ReadDetectsMaintenanceWindowDrift: when windows are
// managed (prior state non-null) and one is deleted out-of-band, acc-check
// returns an empty list and Read must blank maintenance_windows so the deletion
// shows as drift.
func TestEnvironmentResource_ReadDetectsMaintenanceWindowDrift(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/acc-check") {
			_, _ = w.Write([]byte(`{"data":{"maintenanceWindowSchedules":[]}}`)) // deleted out-of-band
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"700","name":"tf-test-env","type":"kubernetes","status":"online",` +
			`"kubeProvider":"gcp","options":{"region":"us-east1"}}}`))
	}))
	t.Cleanup(srv.Close)
	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	state := freshEnvPlan()
	state.ID = types.StringValue("700")
	state.MaintenanceWindows = mwSet(t, maintenanceWindowModel{
		Name: types.StringValue("weekend"), Enabled: types.BoolValue(true), Hour: types.Int64Value(16),
		LengthHours: types.Int64Value(8), Days: daysSet(t, "FRIDAY", "SATURDAY", "SUNDAY"),
	})
	req := resource.ReadRequest{State: newEnvState(t, s, state)}
	resp := resource.ReadResponse{State: newEnvState(t, s, state)}
	r.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "read diags: %v", resp.Diagnostics)

	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	// A confirmed-empty acc-check reconciles to an empty set (not null), so the
	// managed `[{weekend}]` config diffs against `[]` — the deletion shows as drift.
	require.False(t, out.MaintenanceWindows.IsNull(), "confirmed-empty must reconcile to an empty set, not null")
	assert.Equal(t, 0, len(out.MaintenanceWindows.Elements()), "deleted window leaves an empty set")
}

// TestEnvironmentResource_ReadKeepsWindowsWhenNotReported: a successful acc-check
// that returns maintenanceWindowSchedules:null (connector not synced) must NOT
// blank managed windows — that would be false drift.
func TestEnvironmentResource_ReadKeepsWindowsWhenNotReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/acc-check") {
			_, _ = w.Write([]byte(`{"data":{"cloudProvider":"GCP","maintenanceWindowSchedules":null}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"700","name":"tf-test-env","type":"kubernetes","status":"online",` +
			`"kubeProvider":"gcp","options":{"region":"us-east1"}}}`))
	}))
	t.Cleanup(srv.Close)
	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	state := freshEnvPlan()
	state.ID = types.StringValue("700")
	state.MaintenanceWindows = mwSet(t, maintenanceWindowModel{
		Name: types.StringValue("weekend"), Enabled: types.BoolValue(true), Hour: types.Int64Value(16),
		LengthHours: types.Int64Value(8), Days: daysSet(t, "FRIDAY", "SATURDAY", "SUNDAY"),
	})
	req := resource.ReadRequest{State: newEnvState(t, s, state)}
	resp := resource.ReadResponse{State: newEnvState(t, s, state)}
	r.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "read diags: %v", resp.Diagnostics)

	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	var mws []maintenanceWindowModel
	require.False(t, out.MaintenanceWindows.ElementsAs(ctx, &mws, false).HasError())
	require.Len(t, mws, 1, "an unreported (null) acc-check must keep the last-known windows")
	assert.Equal(t, "weekend", mws[0].Name.ValueString())
}

// TestEnvironmentResource_ReadUnmanagedWindowsNotProbed: when maintenance_windows
// is null (unmanaged), Read must NOT call acc-check and must leave it null.
func TestEnvironmentResource_ReadUnmanagedWindowsNotProbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/acc-check") {
			t.Errorf("acc-check must NOT be called when maintenance_windows is unmanaged")
		}
		_, _ = w.Write([]byte(`{"data":{"id":"700","name":"tf-test-env","type":"kubernetes","status":"online",` +
			`"kubeProvider":"gcp","options":{"region":"us-east1"}}}`))
	}))
	t.Cleanup(srv.Close)
	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	state := freshEnvPlan() // MaintenanceWindows is ListNull
	state.ID = types.StringValue("700")
	req := resource.ReadRequest{State: newEnvState(t, s, state)}
	resp := resource.ReadResponse{State: newEnvState(t, s, state)}
	r.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "read diags: %v", resp.Diagnostics)

	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.True(t, out.MaintenanceWindows.IsNull(), "unmanaged windows stay null")
}

// TestEnvironmentResource_ReadMaintenanceWindowsBestEffort: a transient acc-check
// failure on refresh must keep the last-known windows (no false drift) and warn.
func TestEnvironmentResource_ReadMaintenanceWindowsBestEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/acc-check") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom","code":500}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"700","name":"tf-test-env","type":"kubernetes","status":"online",` +
			`"kubeProvider":"gcp","options":{"region":"us-east1"}}}`))
	}))
	t.Cleanup(srv.Close)
	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()), acm.WithMaxRetries(0))}
	s := environmentSchema(t)
	ctx := context.Background()

	state := freshEnvPlan()
	state.ID = types.StringValue("700")
	state.MaintenanceWindows = mwSet(t, maintenanceWindowModel{
		Name: types.StringValue("weekend"), Enabled: types.BoolValue(true), Hour: types.Int64Value(16),
		LengthHours: types.Int64Value(8), Days: daysSet(t, "FRIDAY", "SATURDAY", "SUNDAY"),
	})
	req := resource.ReadRequest{State: newEnvState(t, s, state)}
	resp := resource.ReadResponse{State: newEnvState(t, s, state)}
	r.Read(ctx, req, &resp)

	require.False(t, resp.Diagnostics.HasError(), "read must not hard-fail on acc-check error: %v", resp.Diagnostics)
	assert.Equal(t, 1, resp.Diagnostics.WarningsCount(), "must warn that windows could not be refreshed")
	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	var mws []maintenanceWindowModel
	require.False(t, out.MaintenanceWindows.ElementsAs(ctx, &mws, false).HasError())
	require.Len(t, mws, 1, "last-known windows preserved on acc-check failure")
	assert.Equal(t, "weekend", mws[0].Name.ValueString())
}

// TestEnvironmentResource_ReadPopulatesCloudProviderRegion: Read must be able to
// reconstruct name/cloud_provider/region from the API (kubeProvider +
// options.region) even when they are null in the prior state, so a refresh never
// leaves them blank and forces a replacement. (ImportState now pre-fills these,
// but this pins the Read path independently as the backstop.)
func TestEnvironmentResource_ReadPopulatesCloudProviderRegion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"2293","name":"example-env","type":"kubernetes",` +
			`"status":"online","kubeProvider":"gcp","options":{"region":"us-east1"}}}`))
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	// Prior state with id known but name/cloud_provider/region null — Read must
	// fill them from the API.
	imported := freshEnvPlan()
	imported.ID = types.StringValue("2293")
	imported.Name = types.StringNull()
	imported.CloudProvider = types.StringNull()
	imported.Region = types.StringNull()
	req := resource.ReadRequest{State: newEnvState(t, s, imported)}
	resp := resource.ReadResponse{State: newEnvState(t, s, imported)}
	r.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "read diags: %v", resp.Diagnostics)

	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "gcp", out.CloudProvider.ValueString(), "cloud_provider reconstructed from kubeProvider")
	assert.Equal(t, "us-east1", out.Region.ValueString(), "region reconstructed from options.region")
	assert.Equal(t, "example-env", out.Name.ValueString())
}

// TestEnvironmentResource_ReadPreservesProviderWhenAPIOmits: a GET that does not
// echo kubeProvider/options must leave the state's existing cloud_provider/region
// untouched (no spurious replace), rather than blanking them.
func TestEnvironmentResource_ReadPreservesProviderWhenAPIOmits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(envReady)) // no kubeProvider / options
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	state := freshEnvPlan() // cloud_provider=gcp, region=us-east1
	state.ID = types.StringValue("700")
	req := resource.ReadRequest{State: newEnvState(t, s, state)}
	resp := resource.ReadResponse{State: newEnvState(t, s, state)}
	r.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "read diags: %v", resp.Diagnostics)

	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "gcp", out.CloudProvider.ValueString(), "cloud_provider preserved when API omits it")
	assert.Equal(t, "us-east1", out.Region.ValueString(), "region preserved when API omits it")
}

func TestEnvironmentResource_WeekdayValidatorRejectsBadDay(t *testing.T) {
	var resp validator.SetResponse
	days := daysSet(t, "FRIDAY", "funday")
	weekdaySetValidator{}.ValidateSet(context.Background(), validator.SetRequest{
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
			// existence probe: not found before request.
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

// TestEnvironmentResource_CreateRefusesExisting: an environment with the same
// name already exists -> Create errors (directing to `terraform import`),
// EnvironmentRequest is NOT called, and no state is recorded.
func TestEnvironmentResource_CreateRefusesExisting(t *testing.T) {
	requested := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/environments/request":
			requested = true
			t.Errorf("EnvironmentRequest must NOT be called when the env already exists")
			_, _ = w.Write([]byte(`{}`))
		case r.URL.Path == "/environments" && r.Method == http.MethodGet:
			// existence probe finds the pre-existing env.
			_, _ = w.Write([]byte(`{"data":[{"id":"700","name":"tf-test-env","status":"online"}]}`))
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

	require.True(t, resp.Diagnostics.HasError(), "create must fail when the env already exists")
	assert.False(t, requested, "must not request a new environment when one already exists")
	assert.Contains(t, resp.Diagnostics.Errors()[0].Summary(), "already exists")
	assert.Contains(t, resp.Diagnostics.Errors()[0].Detail(), "terraform import", "must point the operator to import")
	assert.True(t, resp.State.Raw.IsNull(), "no state must be recorded when create is refused")
}

// TestEnvironmentResource_ModifyPlanRefusesExisting: at PLAN time, a create
// whose name already exists must fail the plan (anchored to `name`, with an
// import hint) instead of rendering "+ create".
func TestEnvironmentResource_ModifyPlanRefusesExisting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/environments" && r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"id":"700","name":"tf-test-env","status":"online"}]}`))
			return
		}
		t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	// Create plan: null prior state, populated plan.
	req := resource.ModifyPlanRequest{
		State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)},
		Plan:  newEnvPlan(t, s, freshEnvPlan()),
	}
	resp := resource.ModifyPlanResponse{Plan: newEnvPlan(t, s, freshEnvPlan())}
	r.ModifyPlan(ctx, req, &resp)

	require.True(t, resp.Diagnostics.HasError(), "plan must fail when the env already exists")
	assert.Contains(t, resp.Diagnostics.Errors()[0].Summary(), "already exists")
	assert.Contains(t, resp.Diagnostics.Errors()[0].Detail(), "terraform import", "must point the operator to import")
}

// TestEnvironmentResource_ModifyPlanAllowsFresh: at PLAN time, a create whose
// name does not yet exist must NOT raise an error.
func TestEnvironmentResource_ModifyPlanAllowsFresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`)) // not found
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	req := resource.ModifyPlanRequest{
		State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)},
		Plan:  newEnvPlan(t, s, freshEnvPlan()),
	}
	resp := resource.ModifyPlanResponse{Plan: newEnvPlan(t, s, freshEnvPlan())}
	r.ModifyPlan(ctx, req, &resp)

	require.False(t, resp.Diagnostics.HasError(), "fresh create must plan cleanly: %v", resp.Diagnostics)
}

// TestEnvironmentResource_ModifyPlanSkipsUpdate: an update (non-null prior
// state) must not trigger the existence guard (no API call).
func TestEnvironmentResource_ModifyPlanSkipsUpdate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("ModifyPlan must not call the API on an update; got %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	prior := freshEnvPlan()
	prior.ID = types.StringValue("700")
	req := resource.ModifyPlanRequest{
		State: newEnvState(t, s, prior), // non-null prior state -> update
		Plan:  newEnvPlan(t, s, prior),
	}
	resp := resource.ModifyPlanResponse{Plan: newEnvPlan(t, s, prior)}
	r.ModifyPlan(ctx, req, &resp)

	require.False(t, resp.Diagnostics.HasError(), "update plan must not error: %v", resp.Diagnostics)
}

// TestEnvironmentResource_ModifyPlanBlocksReplace: changing a RequiresReplace
// field on an existing env must fail the plan (the env can't be deleted, so a
// destroy+create would strand the operator), without calling the API.
func TestEnvironmentResource_ModifyPlanBlocksReplace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("ModifyPlan must not call the API to detect a replace; got %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	state := freshEnvPlan()
	state.ID = types.StringValue("700")
	plan := state
	plan.Region = types.StringValue("us-west1") // RequiresReplace change

	req := resource.ModifyPlanRequest{State: newEnvState(t, s, state), Plan: newEnvPlan(t, s, plan)}
	resp := resource.ModifyPlanResponse{Plan: newEnvPlan(t, s, plan)}
	r.ModifyPlan(ctx, req, &resp)

	require.True(t, resp.Diagnostics.HasError(), "changing region on an existing env must be blocked")
	assert.Contains(t, resp.Diagnostics.Errors()[0].Summary(), "cannot be replaced")
}

// TestEnvironmentResource_ModifyPlanLookupErrorWarns: a non-404 lookup failure
// during a create plan degrades to a warning (the Create-time guard is
// authoritative), not a hard error.
func TestEnvironmentResource_ModifyPlanLookupErrorWarns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom","code":500}`))
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()), acm.WithMaxRetries(0))}
	s := environmentSchema(t)
	ctx := context.Background()

	req := resource.ModifyPlanRequest{
		State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}, // null -> create
		Plan:  newEnvPlan(t, s, freshEnvPlan()),
	}
	resp := resource.ModifyPlanResponse{Plan: newEnvPlan(t, s, freshEnvPlan())}
	r.ModifyPlan(ctx, req, &resp)

	require.False(t, resp.Diagnostics.HasError(), "a flaky lookup must not block the plan")
	assert.Equal(t, 1, resp.Diagnostics.WarningsCount(), "must warn that the existence check was inconclusive")
}

// TestEnvironmentResource_CreatePollTimeoutLeavesNoState: a never-ready env + a
// tiny create timeout must error AND leave state empty (the request created the
// env in ACM, but with no readiness we record nothing — the operator imports it
// by id once it provisions).
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
	// State must be empty/null so Terraform records nothing on a timed-out create.
	assert.True(t, resp.State.Raw.IsNull(), "state must not be set on poll timeout")
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

// TestEnvironmentResource_ImportState: import reconstructs what the API returns —
// cloud_provider/region and the datadog integration (non-secret fields; api_key
// stays null). The ACM API does NOT return maintenance windows on read (GET sends
// maintenanceWindowSchedules: null), so windows are read from the acc-check
// endpoint instead; this test serves both and asserts everything is captured.
func TestEnvironmentResource_ImportState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/acc-check") {
			_, _ = w.Write([]byte(`{"data":{"maintenanceWindowSchedules":[` +
				`{"name":"weekend","enabled":true,"hour":16,"lengthInHours":8,"days":["FRIDAY","SATURDAY","SUNDAY"]}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"2293","name":"example-env","displayName":"Terraform Demo",` +
			`"normalizedName":"example-env","type":"kubernetes","domain":"x.altinity.cloud","status":"online",` +
			`"kubeProvider":"gcp","options":{"region":"us-east1"},` +
			`"datadogSettings":{"enabled":true,"key":"SERVER-SIDE","region":"datadoghq.eu","metrics":true,"logs":true,"tableStats":false},` +
			`"maintenanceWindowSchedules":null}}`))
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	req := resource.ImportStateRequest{ID: "2293"}
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.ImportState(ctx, req, &resp)

	require.False(t, resp.Diagnostics.HasError(), "import diags: %v", resp.Diagnostics)
	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())

	assert.Equal(t, "2293", out.ID.ValueString())
	assert.Equal(t, "gcp", out.CloudProvider.ValueString())
	assert.Equal(t, "us-east1", out.Region.ValueString())

	// datadog captured (non-secret); api_key is write-only -> null; apply_to_clusters defaults true.
	require.NotNil(t, out.Datadog, "datadog must be imported when defined")
	assert.True(t, out.Datadog.Enabled.ValueBool())
	assert.Equal(t, "datadoghq.eu", out.Datadog.Region.ValueString())
	assert.True(t, out.Datadog.SendLogs.ValueBool())
	assert.False(t, out.Datadog.SendTableStats.ValueBool())
	assert.True(t, out.Datadog.APIKey.IsNull(), "api_key must import as null (provider drops the API-returned key, keeping it out of state)")
	assert.True(t, out.Datadog.ApplyToClusters.ValueBool(), "apply_to_clusters defaults to true")

	// maintenance_windows imported from acc-check.
	var mws []maintenanceWindowModel
	require.False(t, out.MaintenanceWindows.ElementsAs(ctx, &mws, false).HasError())
	require.Len(t, mws, 1)
	assert.Equal(t, "weekend", mws[0].Name.ValueString())
	assert.Equal(t, int64(16), mws[0].Hour.ValueInt64())
	assert.Equal(t, int64(8), mws[0].LengthHours.ValueInt64())
	var days []string
	require.False(t, mws[0].Days.ElementsAs(ctx, &days, false).HasError())
	assert.ElementsMatch(t, []string{"FRIDAY", "SATURDAY", "SUNDAY"}, days)
}

// TestEnvironmentResource_ImportStateNoExtras: importing an environment with no
// datadog and no maintenance windows leaves both unmanaged (nil / null). acc-check
// returns an empty schedule list.
func TestEnvironmentResource_ImportStateNoExtras(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/acc-check") {
			_, _ = w.Write([]byte(`{"data":{"maintenanceWindowSchedules":[]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"2293","name":"plain-env","type":"kubernetes","status":"online",` +
			`"kubeProvider":"aws","options":{"region":"us-west-2"}}}`))
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := environmentSchema(t)
	ctx := context.Background()

	req := resource.ImportStateRequest{ID: "2293"}
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.ImportState(ctx, req, &resp)

	require.False(t, resp.Diagnostics.HasError(), "import diags: %v", resp.Diagnostics)
	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "aws", out.CloudProvider.ValueString())
	assert.Equal(t, "us-west-2", out.Region.ValueString())
	assert.Nil(t, out.Datadog, "datadog must stay unmanaged when not defined")
	assert.True(t, out.MaintenanceWindows.IsNull(), "maintenance_windows must stay unmanaged when none exist")
}

// TestEnvironmentResource_ImportStateMaintenanceWindowsBestEffort: if acc-check
// fails, import still succeeds (with a warning) and maintenance_windows is null.
func TestEnvironmentResource_ImportStateMaintenanceWindowsBestEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/acc-check") {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"boom","code":500}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"2293","name":"plain-env","type":"kubernetes","status":"online",` +
			`"kubeProvider":"gcp","options":{"region":"us-east1"}}}`))
	}))
	t.Cleanup(srv.Close)

	r := &environmentResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()), acm.WithMaxRetries(0))}
	s := environmentSchema(t)
	ctx := context.Background()

	req := resource.ImportStateRequest{ID: "2293"}
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.ImportState(ctx, req, &resp)

	require.False(t, resp.Diagnostics.HasError(), "import must not hard-fail on acc-check error: %v", resp.Diagnostics)
	assert.Equal(t, 1, resp.Diagnostics.WarningsCount(), "must warn that windows could not be imported")
	var out environmentResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.True(t, out.MaintenanceWindows.IsNull(), "maintenance_windows null when acc-check fails")
}

// TestEnvironmentResource_ImportStateRejectsNonNumericID: a non-numeric import id
// is rejected with guidance.
func TestEnvironmentResource_ImportStateRejectsNonNumericID(t *testing.T) {
	r := &environmentResource{client: acm.NewClient("http://unused", "t")}
	s := environmentSchema(t)
	ctx := context.Background()

	req := resource.ImportStateRequest{ID: "not-a-number"}
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.ImportState(ctx, req, &resp)

	require.True(t, resp.Diagnostics.HasError(), "non-numeric import id must be rejected")
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
