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
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

// newSettingResource builds a configured resource against the given client.
func newSettingResource(client *acm.Client) *settingResource {
	r := NewSettingResource().(*settingResource)
	r.client = client
	return r
}

// settingObjType resolves the resource schema's object type.
func settingObjType(t *testing.T, r *settingResource) (tftypes.Object, resource.SchemaResponse) {
	t.Helper()
	var sr resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &sr)
	require.False(t, sr.Diagnostics.HasError(), "schema diags: %v", sr.Diagnostics)
	return sr.Schema.Type().TerraformType(context.Background()).(tftypes.Object), sr
}

// settingValue builds a fully-populated tftypes object value from a string map;
// any attr not present is set null.
func settingValue(objType tftypes.Object, attrs map[string]string) tftypes.Value {
	vals := map[string]tftypes.Value{}
	for name := range objType.AttributeTypes {
		if v, ok := attrs[name]; ok {
			vals[name] = tftypes.NewValue(tftypes.String, v)
		} else {
			vals[name] = tftypes.NewValue(tftypes.String, nil)
		}
	}
	return tftypes.NewValue(objType, vals)
}

func TestSettingResource_Metadata(t *testing.T) {
	r := NewSettingResource()
	var resp resource.MetadataResponse
	r.Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "altinity"}, &resp)
	assert.Equal(t, "altinity_clickhouse_cluster_setting", resp.TypeName)
}

func TestSettingResource_Create(t *testing.T) {
	var postPath, gotBody string
	var listed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case http.MethodGet: // idempotency pre-check: no existing setting
			listed = true
			_, _ = w.Write([]byte(`{"data":[]}`))
		case http.MethodPost: // create
			postPath = req.URL.Path
			buf, _ := io.ReadAll(req.Body)
			gotBody = string(buf)
			_, _ = w.Write([]byte(`{"data":{"id":"42","name":"max_threads","value":"8"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "test-token", acm.WithHTTPClient(srv.Client()))

	r := newSettingResource(client)
	objType, sr := settingObjType(t, r)

	planVal := settingValue(objType, map[string]string{
		"cluster_id": "7",
		"name":       "max_threads",
		"value":      "8",
	})
	req := resource.CreateRequest{Plan: tfsdk.Plan{Schema: sr.Schema, Raw: planVal}}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: sr.Schema, Raw: tftypes.NewValue(objType, nil)}}
	r.Create(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	assert.True(t, listed, "create must list first for idempotency")
	assert.Equal(t, "/cluster/7/settings", postPath)
	assert.Contains(t, gotBody, `"name":"max_threads"`)
	assert.Contains(t, gotBody, `"value":"8"`)

	var state settingResourceModel
	resp.State.Get(context.Background(), &state)
	assert.Equal(t, "7:max_threads", state.ID.ValueString())
	assert.Equal(t, "7", state.ClusterID.ValueString())
	assert.Equal(t, "max_threads", state.Name.ValueString())
	assert.Equal(t, "8", state.Value.ValueString())
	assert.Equal(t, "42", state.SettingID.ValueString())
}

func TestSettingResource_ReadMatchesByName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		assert.Equal(t, "/cluster/7/settings", req.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"1","name":"max_memory","value":"100"},
			{"id":"42","name":"max_threads","value":"16"}
		]}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "test-token", acm.WithHTTPClient(srv.Client()))

	r := newSettingResource(client)
	objType, sr := settingObjType(t, r)

	stateVal := settingValue(objType, map[string]string{
		"id":         "7:max_threads",
		"cluster_id": "7",
		"name":       "max_threads",
		"value":      "8",
		"setting_id": "42",
	})
	req := resource.ReadRequest{State: tfsdk.State{Schema: sr.Schema, Raw: stateVal}}
	resp := resource.ReadResponse{State: tfsdk.State{Schema: sr.Schema, Raw: stateVal}}
	r.Read(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	var state settingResourceModel
	resp.State.Get(context.Background(), &state)
	// Value picked up from the matched-by-name list entry (drift detection).
	assert.Equal(t, "16", state.Value.ValueString())
	assert.Equal(t, "42", state.SettingID.ValueString())
	assert.Equal(t, "7:max_threads", state.ID.ValueString())
}

func TestSettingResource_ReadMissingRemovesState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"1","name":"other","value":"x"}]}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "test-token", acm.WithHTTPClient(srv.Client()))

	r := newSettingResource(client)
	objType, sr := settingObjType(t, r)

	stateVal := settingValue(objType, map[string]string{
		"id":         "7:max_threads",
		"cluster_id": "7",
		"name":       "max_threads",
		"value":      "8",
		"setting_id": "42",
	})
	req := resource.ReadRequest{State: tfsdk.State{Schema: sr.Schema, Raw: stateVal}}
	resp := resource.ReadResponse{State: tfsdk.State{Schema: sr.Schema, Raw: stateVal}}
	r.Read(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)
	assert.True(t, resp.State.Raw.IsNull(), "expected state removed when setting absent")
}

func TestSettingResource_ReadClusterGoneRemovesState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no cluster"}}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "test-token", acm.WithHTTPClient(srv.Client()))

	r := newSettingResource(client)
	objType, sr := settingObjType(t, r)

	stateVal := settingValue(objType, map[string]string{
		"id":         "7:max_threads",
		"cluster_id": "7",
		"name":       "max_threads",
		"value":      "8",
		"setting_id": "42",
	})
	req := resource.ReadRequest{State: tfsdk.State{Schema: sr.Schema, Raw: stateVal}}
	resp := resource.ReadResponse{State: tfsdk.State{Schema: sr.Schema, Raw: stateVal}}
	r.Read(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)
	assert.True(t, resp.State.Raw.IsNull(), "expected state removed on cluster 404")
}

func TestSettingResource_Update(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		gotMethod = req.Method
		buf, _ := io.ReadAll(req.Body)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"42","name":"max_threads","value":"32"}}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "test-token", acm.WithHTTPClient(srv.Client()))

	r := newSettingResource(client)
	objType, sr := settingObjType(t, r)

	attrs := map[string]string{
		"id": "7:max_threads", "cluster_id": "7", "name": "max_threads", "value": "32", "setting_id": "42",
	}
	stateAttrs := map[string]string{
		"id": "7:max_threads", "cluster_id": "7", "name": "max_threads", "value": "8", "setting_id": "42",
	}
	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sr.Schema, Raw: settingValue(objType, attrs)},
		State: tfsdk.State{Schema: sr.Schema, Raw: settingValue(objType, stateAttrs)},
	}
	resp := resource.UpdateResponse{State: tfsdk.State{Schema: sr.Schema, Raw: settingValue(objType, stateAttrs)}}
	r.Update(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	// Edit goes to the per-id endpoint, not the collection.
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/cluster-setting/42", gotPath)
	assert.Contains(t, gotBody, `"value":"32"`)
	var state settingResourceModel
	resp.State.Get(context.Background(), &state)
	assert.Equal(t, "32", state.Value.ValueString())
	assert.Equal(t, "42", state.SettingID.ValueString())
}

func TestSettingResource_DeleteRemovesRemotely(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		gotMethod = req.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "test-token", acm.WithHTTPClient(srv.Client()))

	r := newSettingResource(client)
	objType, sr := settingObjType(t, r)

	stateVal := settingValue(objType, map[string]string{
		"id": "7:max_threads", "cluster_id": "7", "name": "max_threads", "value": "8", "setting_id": "42",
	})
	req := resource.DeleteRequest{State: tfsdk.State{Schema: sr.Schema, Raw: stateVal}}
	resp := resource.DeleteResponse{State: tfsdk.State{Schema: sr.Schema, Raw: stateVal}}
	r.Delete(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)
	assert.Equal(t, 0, resp.Diagnostics.WarningsCount(), "no more 'setting left on cluster' warning")
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/cluster-setting/42", gotPath)
}

func TestSettingResource_DeleteAlreadyGoneIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found","code":"NotFound"}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "test-token", acm.WithHTTPClient(srv.Client()))

	r := newSettingResource(client)
	objType, sr := settingObjType(t, r)
	stateVal := settingValue(objType, map[string]string{
		"id": "7:max_threads", "cluster_id": "7", "name": "max_threads", "value": "8", "setting_id": "42",
	})
	req := resource.DeleteRequest{State: tfsdk.State{Schema: sr.Schema, Raw: stateVal}}
	resp := resource.DeleteResponse{State: tfsdk.State{Schema: sr.Schema, Raw: stateVal}}
	r.Delete(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "404 on delete should be success: %v", resp.Diagnostics)
}

func TestSettingResource_ImportState(t *testing.T) {
	r := newSettingResource(nil)
	objType, sr := settingObjType(t, r)

	req := resource.ImportStateRequest{ID: "7:max_threads"}
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: sr.Schema, Raw: tftypes.NewValue(objType, nil)}}
	r.ImportState(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	var state settingResourceModel
	resp.State.Get(context.Background(), &state)
	assert.Equal(t, "7", state.ClusterID.ValueString())
	assert.Equal(t, "max_threads", state.Name.ValueString())
	assert.Equal(t, "7:max_threads", state.ID.ValueString())
}

func TestSettingResource_ImportStateRejectsBadID(t *testing.T) {
	r := newSettingResource(nil)
	objType, sr := settingObjType(t, r)

	for _, bad := range []string{"nocolon", ":name", "7:", ""} {
		req := resource.ImportStateRequest{ID: bad}
		resp := resource.ImportStateResponse{State: tfsdk.State{Schema: sr.Schema, Raw: tftypes.NewValue(objType, nil)}}
		r.ImportState(context.Background(), req, &resp)
		assert.True(t, resp.Diagnostics.HasError(), "expected error for %q", bad)
	}
}

func TestSettingResource_SplitCompositeIDLastColon(t *testing.T) {
	// design §5.1: split on the LAST ':'. cluster_id is always an integer, so
	// the only ambiguity would be a ':' in the name — the last-colon rule keeps
	// the name as the final segment.
	clusterID, name, err := settingSplitCompositeID("7:max_threads")
	require.NoError(t, err)
	assert.Equal(t, "7", clusterID)
	assert.Equal(t, "max_threads", name)
}
