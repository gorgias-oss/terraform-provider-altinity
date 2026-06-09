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
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
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
		ID:             types.StringUnknown(),
		Name:           types.StringValue("tf-test-env"),
		CloudProvider:  types.StringValue("gcp"),
		Region:         types.StringValue("us-east1"),
		DisplayName:    types.StringUnknown(),
		NormalizedName: types.StringUnknown(),
		Type:           types.StringUnknown(),
		Domain:         types.StringUnknown(),
		Status:         types.StringUnknown(),
		State:          types.StringUnknown(),
		Timeouts:       nullTimeouts(),
	}
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
	// region routed to the matching provider field.
	assert.Equal(t, "gcp", reqBody["cloud_provider"])
	assert.Equal(t, "us-east1", reqBody["gcp_region"])

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

// TestEnvironmentResource_DeleteRefusedWithClusters: the no-cascade guard.
func TestEnvironmentResource_DeleteRefusedWithClusters(t *testing.T) {
	removed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/clusters") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[{"id":"1","name":"foo"},{"id":"2","name":"bar"}]}`))
		case r.Method == http.MethodDelete:
			removed = true
			_, _ = w.Write([]byte(`{"data":true}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
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

	require.True(t, resp.Diagnostics.HasError(), "delete must be refused with clusters present")
	assert.Contains(t, resp.Diagnostics.Errors()[0].Summary(), "not empty")
	assert.False(t, removed, "RemoveEnvironment must NOT be called when clusters remain")
}

// TestEnvironmentResource_DeleteEmpty: no clusters -> remove -> poll gone.
func TestEnvironmentResource_DeleteEmpty(t *testing.T) {
	removed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/clusters") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasPrefix(r.URL.Path, "/environment/") && r.Method == http.MethodDelete:
			removed = true
			_, _ = w.Write([]byte(`{"data":true}`))
		case r.URL.Path == "/environments" && r.Method == http.MethodGet:
			// gone immediately
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
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

	require.False(t, resp.Diagnostics.HasError(), "delete diags: %v", resp.Diagnostics)
	assert.True(t, removed, "RemoveEnvironment must be called when empty")
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
