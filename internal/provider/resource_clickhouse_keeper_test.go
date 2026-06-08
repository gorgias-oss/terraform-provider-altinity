// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
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

	"github.com/Gorgias/terraform-provider-altinity/internal/acm"
)

func keeperSchema(t *testing.T) rschema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	NewKeeperResource().(*keeperResource).Schema(context.Background(), resource.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError(), "schema diags: %v", resp.Diagnostics)
	return resp.Schema
}

func newKeeperPlan(t *testing.T, s rschema.Schema, m keeperResourceModel) tfsdk.Plan {
	t.Helper()
	ctx := context.Background()
	pl := tfsdk.Plan{Schema: s, Raw: emptyObjectValue(ctx, s)}
	require.False(t, pl.Set(ctx, &m).HasError())
	return pl
}

func newKeeperState(t *testing.T, s rschema.Schema, m keeperResourceModel) tfsdk.State {
	t.Helper()
	ctx := context.Background()
	st := tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}
	require.False(t, st.Set(ctx, &m).HasError())
	return st
}

func TestKeeperResource_Metadata(t *testing.T) {
	var resp resource.MetadataResponse
	NewKeeperResource().Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "altinity"}, &resp)
	assert.Equal(t, "altinity_clickhouse_keeper", resp.TypeName)
}

func TestSplitKeeperCompositeID(t *testing.T) {
	env, name, err := splitKeeperCompositeID("2267:my-keeper")
	require.NoError(t, err)
	assert.Equal(t, "2267", env)
	assert.Equal(t, "my-keeper", name)

	// First-colon split: env is numeric, so a colon in the name is unambiguous.
	env, name, err = splitKeeperCompositeID("2267:weird:name")
	require.NoError(t, err)
	assert.Equal(t, "2267", env)
	assert.Equal(t, "weird:name", name)

	_, _, err = splitKeeperCompositeID("nocolon")
	assert.Error(t, err)
}

// TestKeeperResource_CreateLifecycle drives Create end-to-end: pre-check (empty
// list) -> launch -> poll healthy -> read back. Confirms typed settings survive
// and the post-create state has no unknowns.
func TestKeeperResource_CreateLifecycle(t *testing.T) {
	launched := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			_, _ = w.Write([]byte(`{"data":{"status":"` + "ready" + `"}}`))
		case strings.HasSuffix(r.URL.Path, "/keepers") && r.Method == http.MethodPost:
			launched = true
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/keepers"): // GET list
			if launched {
				_, _ = w.Write([]byte(`{"data":[{"name":"kpr","instanceType":"e2-standard-2","ha":true,"cpuLimits":0}]}`))
			} else {
				_, _ = w.Write([]byte(`{"data":[]}`))
			}
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	r := &keeperResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := keeperSchema(t)
	ctx := context.Background()

	plan := keeperResourceModel{
		Environment:     types.StringValue("2267"),
		Name:            types.StringValue("kpr"),
		InstanceType:    types.StringValue("e2-standard-2"),
		Image:           types.StringUnknown(),
		Ha:              types.BoolValue(true),
		Zones:           []string{"a", "b"},
		ID:              types.StringUnknown(),
		CPULimits:       types.StringUnknown(),
		CPURequests:     types.StringUnknown(),
		ZoneTopologyKey: types.StringUnknown(),
		Timeouts:        nullTimeouts(),
	}

	req := resource.CreateRequest{Plan: newKeeperPlan(t, s, plan)}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.Create(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "create diags: %v", resp.Diagnostics)
	assert.True(t, launched, "must launch when not pre-existing")

	var out keeperResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "2267:kpr", out.ID.ValueString())
	assert.Equal(t, "e2-standard-2", out.InstanceType.ValueString())
	assert.True(t, out.Ha.ValueBool())
	assert.Equal(t, []string{"a", "b"}, out.Zones)
}

func TestKeeperResource_ReadDriftRemoves(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"name":"other"}]}`))
	}))
	t.Cleanup(srv.Close)

	r := &keeperResource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := keeperSchema(t)
	ctx := context.Background()

	prior := keeperResourceModel{
		Environment: types.StringValue("2267"),
		Name:        types.StringValue("kpr"),
		Ha:          types.BoolValue(false),
		Timeouts:    nullTimeouts(),
	}
	req := resource.ReadRequest{State: newKeeperState(t, s, prior)}
	resp := resource.ReadResponse{State: newKeeperState(t, s, prior)}
	r.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError())
	assert.True(t, resp.State.Raw.IsNull(), "absent keeper must be removed from state")
}

func TestKeeperResource_ImportState(t *testing.T) {
	r := NewKeeperResource().(*keeperResource)
	s := keeperSchema(t)
	ctx := context.Background()
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: s, Raw: emptyObjectValue(ctx, s)}}
	r.ImportState(ctx, resource.ImportStateRequest{ID: "2267:kpr"}, &resp)
	require.False(t, resp.Diagnostics.HasError(), "import diags: %v", resp.Diagnostics)

	var out keeperResourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, "2267", out.Environment.ValueString())
	assert.Equal(t, "kpr", out.Name.ValueString())
	assert.Equal(t, "2267:kpr", out.ID.ValueString())
}
