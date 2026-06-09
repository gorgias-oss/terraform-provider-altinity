// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

func newProfileResource(t *testing.T, client *acm.Client) *profileResource {
	t.Helper()
	r := NewProfileResource().(*profileResource)
	r.client = client
	return r
}

// profileSchemaOf returns the framework schema and its tftypes.Object so tests
// can build Config/State values exactly as the runtime does.
func profileSchemaOf(t *testing.T, r *profileResource) (objType tftypes.Object, sch rschema.Schema) {
	t.Helper()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError())
	return resp.Schema.Type().TerraformType(context.Background()).(tftypes.Object), resp.Schema
}

func planValue(objType tftypes.Object, clusterID, name, description string) tftypes.Value {
	vals := map[string]tftypes.Value{}
	for attr := range objType.AttributeTypes {
		switch attr {
		case "cluster_id":
			vals[attr] = tftypes.NewValue(tftypes.String, clusterID)
		case "name":
			vals[attr] = tftypes.NewValue(tftypes.String, name)
		case "description":
			if description == "" {
				vals[attr] = tftypes.NewValue(tftypes.String, nil)
			} else {
				vals[attr] = tftypes.NewValue(tftypes.String, description)
			}
		default:
			vals[attr] = tftypes.NewValue(tftypes.String, nil)
		}
	}
	return tftypes.NewValue(objType, vals)
}

func stateValue(objType tftypes.Object, clusterID, name, description, id, profileID string) tftypes.Value {
	vals := map[string]tftypes.Value{}
	for attr := range objType.AttributeTypes {
		switch attr {
		case "cluster_id":
			vals[attr] = tftypes.NewValue(tftypes.String, clusterID)
		case "name":
			vals[attr] = tftypes.NewValue(tftypes.String, name)
		case "description":
			if description == "" {
				vals[attr] = tftypes.NewValue(tftypes.String, nil)
			} else {
				vals[attr] = tftypes.NewValue(tftypes.String, description)
			}
		case "id":
			vals[attr] = tftypes.NewValue(tftypes.String, id)
		case "profile_id":
			vals[attr] = tftypes.NewValue(tftypes.String, profileID)
		default:
			vals[attr] = tftypes.NewValue(tftypes.String, nil)
		}
	}
	return tftypes.NewValue(objType, vals)
}

func TestProfileResource_Metadata(t *testing.T) {
	r := NewProfileResource()
	var resp resource.MetadataResponse
	r.Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "altinity"}, &resp)
	assert.Equal(t, "altinity_clickhouse_profile", resp.TypeName)
}

func TestProfileResource_Create(t *testing.T) {
	var postPath, postBody string
	var listed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case http.MethodGet: // idempotency pre-check: no existing profile
			listed = true
			_, _ = w.Write([]byte(`{"data":[]}`))
		case http.MethodPost: // create
			postPath = req.URL.Path
			b, _ := io.ReadAll(req.Body)
			postBody = string(b)
			_, _ = w.Write([]byte(`{"data":{"id":"42","name":"analytics","description":"analytics workloads","id_cluster":"7"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newProfileResource(t, client)
	objType, sch := profileSchemaOf(t, r)

	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: planValue(objType, "7", "analytics", "analytics workloads")},
	}
	resp := resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(objType, nil)},
	}
	r.Create(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	assert.True(t, listed, "create must list first for idempotency")
	assert.Equal(t, "/cluster/7/profiles", postPath)
	assert.Contains(t, postBody, `"name":"analytics"`)
	assert.Contains(t, postBody, `"description":"analytics workloads"`)

	var model profileResourceModel
	require.False(t, resp.State.Get(context.Background(), &model).HasError())
	assert.Equal(t, "7:analytics", model.ID.ValueString())
	assert.Equal(t, "42", model.ProfileID.ValueString())
	assert.Equal(t, "analytics", model.Name.ValueString())
	assert.Equal(t, "analytics workloads", model.Description.ValueString())
}

// TestProfileResource_Create_AdoptsExisting verifies idempotency: when a
// profile of the same name already exists, Create adopts it (no duplicate POST)
// rather than minting a second profile — the fix for the duplicate-profile
// accumulation observed live.
func TestProfileResource_Create_AdoptsExisting(t *testing.T) {
	var posted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[{"id":"42","name":"analytics","description":"analytics workloads","id_cluster":"7"}]}`))
		case http.MethodPost:
			posted = true
			_, _ = w.Write([]byte(`{"data":{"id":"99","name":"analytics","id_cluster":"7"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newProfileResource(t, client)
	objType, sch := profileSchemaOf(t, r)

	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: planValue(objType, "7", "analytics", "analytics workloads")},
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(objType, nil)}}
	r.Create(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	assert.False(t, posted, "must adopt the existing profile, not create a duplicate")
	var model profileResourceModel
	require.False(t, resp.State.Get(context.Background(), &model).HasError())
	assert.Equal(t, "42", model.ProfileID.ValueString(), "adopted the existing profile id")
}

func TestProfileResource_Update_EditsDescription(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		gotMethod = req.Method
		b, _ := io.ReadAll(req.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"42","name":"analytics","description":"new desc","id_cluster":"7"}}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newProfileResource(t, client)
	objType, sch := profileSchemaOf(t, r)

	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: stateValue(objType, "7", "analytics", "new desc", "7:analytics", "42")},
		State: tfsdk.State{Schema: sch, Raw: stateValue(objType, "7", "analytics", "old desc", "7:analytics", "42")},
	}
	resp := resource.UpdateResponse{State: tfsdk.State{Schema: sch, Raw: stateValue(objType, "7", "analytics", "old desc", "7:analytics", "42")}}
	r.Update(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/profile/42", gotPath)
	assert.Contains(t, gotBody, `"description":"new desc"`)

	var model profileResourceModel
	require.False(t, resp.State.Get(context.Background(), &model).HasError())
	assert.Equal(t, "42", model.ProfileID.ValueString())
	assert.Equal(t, "new desc", model.Description.ValueString())
}

func TestProfileResource_Delete_RemovesRemotely(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		gotMethod = req.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newProfileResource(t, client)
	objType, sch := profileSchemaOf(t, r)

	req := resource.DeleteRequest{State: tfsdk.State{Schema: sch, Raw: stateValue(objType, "7", "analytics", "", "7:analytics", "42")}}
	resp := resource.DeleteResponse{State: tfsdk.State{Schema: sch, Raw: stateValue(objType, "7", "analytics", "", "7:analytics", "42")}}
	r.Delete(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/profile/42", gotPath)
}

func TestProfileResource_Delete_AlreadyGoneIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found","code":"NotFound"}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newProfileResource(t, client)
	objType, sch := profileSchemaOf(t, r)

	req := resource.DeleteRequest{State: tfsdk.State{Schema: sch, Raw: stateValue(objType, "7", "analytics", "", "7:analytics", "42")}}
	resp := resource.DeleteResponse{State: tfsdk.State{Schema: sch, Raw: stateValue(objType, "7", "analytics", "", "7:analytics", "42")}}
	r.Delete(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "404 on delete should be treated as success: %v", resp.Diagnostics)
}

func TestProfileResource_Create_InvalidClusterID(t *testing.T) {
	client := acm.NewClient("http://unused", "tok")
	r := newProfileResource(t, client)
	objType, sch := profileSchemaOf(t, r)

	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: planValue(objType, "not-an-int", "analytics", "")},
	}
	resp := resource.CreateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(objType, nil)},
	}
	r.Create(context.Background(), req, &resp)
	require.True(t, resp.Diagnostics.HasError())
}

func TestProfileResource_Read_MatchesByName(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"5","name":"default","description":"d","id_cluster":"7"},
			{"id":"42","name":"analytics","description":"analytics workloads","id_cluster":"7"}
		]}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newProfileResource(t, client)
	objType, sch := profileSchemaOf(t, r)

	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: stateValue(objType, "7", "analytics", "analytics workloads", "7:analytics", "42")},
	}
	resp := resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: stateValue(objType, "7", "analytics", "analytics workloads", "7:analytics", "42")},
	}
	r.Read(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	assert.Equal(t, "/cluster/7/profiles", gotPath)

	var model profileResourceModel
	require.False(t, resp.State.Get(context.Background(), &model).HasError())
	assert.Equal(t, "42", model.ProfileID.ValueString())
	assert.Equal(t, "analytics", model.Name.ValueString())
	assert.Equal(t, "7:analytics", model.ID.ValueString())
	assert.Equal(t, "analytics workloads", model.Description.ValueString())
}

func TestProfileResource_Read_RemovedOutOfBand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"5","name":"default","id_cluster":"7"}]}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newProfileResource(t, client)
	objType, sch := profileSchemaOf(t, r)

	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: stateValue(objType, "7", "analytics", "", "7:analytics", "42")},
	}
	resp := resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: stateValue(objType, "7", "analytics", "", "7:analytics", "42")},
	}
	r.Read(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)
	assert.True(t, resp.State.Raw.IsNull(), "expected resource removed from state")
}

func TestProfileResource_Read_ClusterNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"NotFound","message":"no such cluster"}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newProfileResource(t, client)
	objType, sch := profileSchemaOf(t, r)

	req := resource.ReadRequest{
		State: tfsdk.State{Schema: sch, Raw: stateValue(objType, "7", "analytics", "", "7:analytics", "42")},
	}
	resp := resource.ReadResponse{
		State: tfsdk.State{Schema: sch, Raw: stateValue(objType, "7", "analytics", "", "7:analytics", "42")},
	}
	r.Read(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)
	assert.True(t, resp.State.Raw.IsNull(), "expected resource removed from state on 404")
}

func TestProfileResource_ImportState(t *testing.T) {
	r := newProfileResource(t, acm.NewClient("http://unused", "tok"))
	objType, sch := profileSchemaOf(t, r)

	req := resource.ImportStateRequest{ID: "7:analytics"}
	resp := resource.ImportStateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(objType, nil)},
	}
	r.ImportState(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	var model profileResourceModel
	require.False(t, resp.State.Get(context.Background(), &model).HasError())
	assert.Equal(t, "7", model.ClusterID.ValueString())
	assert.Equal(t, "analytics", model.Name.ValueString())
	assert.Equal(t, "7:analytics", model.ID.ValueString())
}

func TestProfileResource_ImportState_NameWithColon(t *testing.T) {
	clusterID, name, err := splitProfileCompositeID("7:weird:name")
	require.NoError(t, err)
	assert.Equal(t, "7:weird", clusterID)
	assert.Equal(t, "name", name)
}

func TestProfileResource_ImportState_Invalid(t *testing.T) {
	r := newProfileResource(t, acm.NewClient("http://unused", "tok"))
	objType, sch := profileSchemaOf(t, r)

	req := resource.ImportStateRequest{ID: "no-colon"}
	resp := resource.ImportStateResponse{
		State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(objType, nil)},
	}
	r.ImportState(context.Background(), req, &resp)
	require.True(t, resp.Diagnostics.HasError())
}

func TestSplitProfileCompositeID_Errors(t *testing.T) {
	for _, id := range []string{"", "nocolon", ":name", "cluster:"} {
		_, _, err := splitProfileCompositeID(id)
		require.Error(t, err, "id=%q", id)
	}
}
