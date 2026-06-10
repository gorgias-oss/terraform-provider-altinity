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

	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

func nodeTypeSchema(t *testing.T) rschema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	NewNodeTypeResource().(*nodeTypeResource).Schema(context.Background(), resource.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError(), "schema diags: %v", resp.Diagnostics)
	return resp.Schema
}

func newNodeTypePlan(t *testing.T, s rschema.Schema, m nodeTypeResourceModel) tfsdk.Plan {
	t.Helper()
	ctx := context.Background()
	pl := tfsdk.Plan{Schema: s, Raw: emptyObjectValue(ctx, s)}
	require.False(t, pl.Set(ctx, &m).HasError())
	return pl
}

func newNodeTypeState(t *testing.T, s rschema.Schema, m nodeTypeResourceModel) tfsdk.State {
	t.Helper()
	ctx := context.Background()
	st := tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}
	require.False(t, st.Set(ctx, &m).HasError())
	return st
}

// freshNodeTypePlan: a create plan (clickhouse, all computed unknown).
func freshNodeTypePlan() nodeTypeResourceModel {
	return nodeTypeResourceModel{
		ID:           types.StringUnknown(),
		NodeTypeID:   types.StringUnknown(),
		Environment:  types.StringValue("2293"),
		Scope:        types.StringValue("clickhouse"),
		Code:         types.StringValue("c4-standard-24-lssd"),
		CPU:          types.Float64Value(24),
		Memory:       types.Int64Value(80160),
		Capacity:     types.Int64Value(10),
		StorageClass: types.StringValue(""),
		IsSpot:       types.BoolValue(false),
		Name:         types.StringUnknown(),
		Used:         types.BoolUnknown(),
	}
}

func TestNodeTypeResource_Metadata(t *testing.T) {
	var resp resource.MetadataResponse
	NewNodeTypeResource().Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "altinity"}, &resp)
	assert.Equal(t, "altinity_node_type", resp.TypeName)
}

// CreateFresh: not found by (scope,code) -> CreateNodeType with the scope-default
// clickhouse toleration -> state set.
func TestNodeTypeResource_CreateFresh(t *testing.T) {
	created := false
	var createBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/nodetypes") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`)) // adopt probe: none
		case strings.HasSuffix(r.URL.Path, "/nodetypes") && r.Method == http.MethodPost:
			created = true
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &createBody)
			http.ServeFile(w, r, "../acm/testdata/nodetype_create_response.json")
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	r := &nodeTypeResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := nodeTypeSchema(t)
	ctx := context.Background()

	req := resource.CreateRequest{Plan: newNodeTypePlan(t, s, freshNodeTypePlan())}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.Create(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "create diags: %v", resp.Diagnostics)
	assert.True(t, created)

	// Mirror-UI: clickhouse scope-default toleration on the wire.
	tols, ok := createBody["tolerations"].([]any)
	require.True(t, ok)
	require.Len(t, tols, 1)
	assert.Equal(t, "clickhouse", tols[0].(map[string]any)["value"])

	var out nodeTypeResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "14140", out.NodeTypeID.ValueString())
	assert.Equal(t, "2293:14140", out.ID.ValueString())
	assert.Equal(t, "c4-standard-24-lssd", out.Code.ValueString())
}

// CreateWithName: name set & != code -> follow-up NodeTypeEdit applies it.
func TestNodeTypeResource_CreateWithNameDoesFollowUpEdit(t *testing.T) {
	edited := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/nodetypes") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "/nodetypes") && r.Method == http.MethodPost:
			http.ServeFile(w, r, "../acm/testdata/nodetype_create_response.json")
		case strings.HasPrefix(r.URL.Path, "/nodetype/") && r.Method == http.MethodPost:
			edited = true
			_, _ = w.Write([]byte(`{"data":{"id":"14140","scope":"clickhouse","code":"c4-standard-24-lssd","name":"my-pool","cpu":"24","memory":"80160","capacity":"10","isSpot":false}}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	r := &nodeTypeResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := nodeTypeSchema(t)
	ctx := context.Background()

	plan := freshNodeTypePlan()
	plan.Name = types.StringValue("my-pool")
	req := resource.CreateRequest{Plan: newNodeTypePlan(t, s, plan)}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.Create(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "create diags: %v", resp.Diagnostics)
	assert.True(t, edited, "a custom name must trigger a follow-up edit")

	var out nodeTypeResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "my-pool", out.Name.ValueString())
}

// CreateAdopt: found by (scope,code) -> CreateNodeType NOT called.
func TestNodeTypeResource_CreateAdopts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/nodetypes") && r.Method == http.MethodGet:
			http.ServeFile(w, r, "../acm/testdata/nodetypes_withused.json")
		case r.Method == http.MethodPost:
			t.Errorf("must not POST-create when the node type already exists")
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	r := &nodeTypeResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := nodeTypeSchema(t)
	ctx := context.Background()

	plan := freshNodeTypePlan()
	plan.Code = types.StringValue("n2d-standard-32") // present in fixture (id 14138)
	plan.CPU = types.Float64Value(32)
	plan.Memory = types.Int64Value(117494)
	req := resource.CreateRequest{Plan: newNodeTypePlan(t, s, plan)}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.Create(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "create diags: %v", resp.Diagnostics)

	var out nodeTypeResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "14138", out.NodeTypeID.ValueString())
	assert.True(t, out.Used.ValueBool())
}

// Update preserves the opaque tolerations read from ACM, unchanged.
func TestNodeTypeResource_UpdatePreservesTolerations(t *testing.T) {
	var editBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/nodetypes") && r.Method == http.MethodGet:
			http.ServeFile(w, r, "../acm/testdata/nodetypes_withused.json")
		case strings.HasPrefix(r.URL.Path, "/nodetype/") && r.Method == http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &editBody)
			_, _ = w.Write([]byte(`{"data":{"id":"14138","scope":"clickhouse","code":"c3d-highcpu-16","name":"c3d-highcpu-16","cpu":"16","memory":"27380","capacity":"10","isSpot":false}}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	r := &nodeTypeResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := nodeTypeSchema(t)
	ctx := context.Background()

	plan := freshNodeTypePlan()
	plan.NodeTypeID = types.StringValue("14138")
	plan.ID = types.StringValue("2293:14138")
	plan.Code = types.StringValue("c3d-highcpu-16") // changed instance type in place
	plan.CPU = types.Float64Value(16)
	plan.Memory = types.Int64Value(27380)
	plan.Name = types.StringValue("n2d-standard-32")
	plan.Used = types.BoolValue(true)

	req := resource.UpdateRequest{Plan: newNodeTypePlan(t, s, plan)}
	resp := resource.UpdateResponse{State: newNodeTypeState(t, s, plan)}
	r.Update(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "update diags: %v", resp.Diagnostics)

	// The current node type's clickhouse toleration must be echoed back unchanged.
	tols, ok := editBody["tolerations"].([]any)
	require.True(t, ok, "preserved tolerations must be sent on update")
	require.Len(t, tols, 1)
	assert.Equal(t, "clickhouse", tols[0].(map[string]any)["value"])
}

// DeleteInUse: ACM rejects -> clear error.
func TestNodeTypeResource_DeleteInUseSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"Node type is in use","code":400}`))
	}))
	t.Cleanup(srv.Close)

	r := &nodeTypeResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := nodeTypeSchema(t)
	ctx := context.Background()

	state := freshNodeTypePlan()
	state.NodeTypeID = types.StringValue("14138")
	req := resource.DeleteRequest{State: newNodeTypeState(t, s, state)}
	resp := resource.DeleteResponse{State: newNodeTypeState(t, s, state)}
	r.Delete(ctx, req, &resp)

	require.True(t, resp.Diagnostics.HasError())
	assert.Contains(t, resp.Diagnostics.Errors()[0].Detail(), "in use")
}

// DeleteOK: {} -> success.
func TestNodeTypeResource_DeleteOK(t *testing.T) {
	deleted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deleted = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	r := &nodeTypeResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := nodeTypeSchema(t)
	ctx := context.Background()

	state := freshNodeTypePlan()
	state.NodeTypeID = types.StringValue("14140")
	req := resource.DeleteRequest{State: newNodeTypeState(t, s, state)}
	resp := resource.DeleteResponse{State: newNodeTypeState(t, s, state)}
	r.Delete(ctx, req, &resp)

	require.False(t, resp.Diagnostics.HasError(), "delete diags: %v", resp.Diagnostics)
	assert.True(t, deleted)
}

// ReadDrift: id absent from list -> removed from state.
func TestNodeTypeResource_ReadDriftRemovesState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`)) // node type gone
	}))
	t.Cleanup(srv.Close)

	r := &nodeTypeResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := nodeTypeSchema(t)
	ctx := context.Background()

	state := freshNodeTypePlan()
	state.NodeTypeID = types.StringValue("99999")
	req := resource.ReadRequest{State: newNodeTypeState(t, s, state)}
	resp := resource.ReadResponse{State: newNodeTypeState(t, s, state)}
	r.Read(ctx, req, &resp)

	require.False(t, resp.Diagnostics.HasError())
	assert.True(t, resp.State.Raw.IsNull(), "drift must remove the resource from state")
}

// Import parses <env>:<scope>:<code>.
func TestNodeTypeResource_ImportState(t *testing.T) {
	r := &nodeTypeResource{}
	s := nodeTypeSchema(t)
	ctx := context.Background()

	req := resource.ImportStateRequest{ID: "2293:clickhouse:n2d-standard-2"}
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.ImportState(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "import diags: %v", resp.Diagnostics)

	var out nodeTypeResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "2293", out.Environment.ValueString())
	assert.Equal(t, "clickhouse", out.Scope.ValueString())
	assert.Equal(t, "n2d-standard-2", out.Code.ValueString())
}
