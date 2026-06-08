// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Gorgias/terraform-provider-altinity/internal/acm"
)

func profilesSchema(t *testing.T) dschema.Schema {
	t.Helper()
	var resp datasource.SchemaResponse
	NewProfilesDataSource().Schema(context.Background(), datasource.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError())
	return resp.Schema
}

func TestProfilesDataSource_Read(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"8145","name":"default","description":"Default Profile","id_cluster":"10091"},
			{"id":"8146","name":"readonly","description":"Read-only","id_cluster":"10091"},
			{"id":"8160","name":"analytics_ro_profile","description":"For analytics","id_cluster":"10091"}
		]}`))
	}))
	t.Cleanup(srv.Close)

	d := &profilesDataSource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := profilesSchema(t)
	ctx := context.Background()

	objType := s.Type().TerraformType(ctx).(tftypes.Object)
	raw := tftypes.NewValue(objType, map[string]tftypes.Value{
		"cluster_id": tftypes.NewValue(tftypes.String, "10091"),
		"profiles":   tftypes.NewValue(objType.AttributeTypes["profiles"], nil),
	})
	req := datasource.ReadRequest{Config: tfsdk.Config{Schema: s, Raw: raw}}
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: emptyDSObjectValue(ctx, s)}}

	d.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "read diags: %v", resp.Diagnostics)
	assert.Equal(t, "/cluster/10091/profiles", gotPath)

	var out profilesDataSourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	require.Len(t, out.Profiles, 3)

	// Verify each entry maps correctly.
	assert.Equal(t, "8145", out.Profiles[0].ProfileID.ValueString())
	assert.Equal(t, "default", out.Profiles[0].Name.ValueString())
	assert.Equal(t, "Default Profile", out.Profiles[0].Description.ValueString())

	assert.Equal(t, "8146", out.Profiles[1].ProfileID.ValueString())
	assert.Equal(t, "readonly", out.Profiles[1].Name.ValueString())

	assert.Equal(t, "8160", out.Profiles[2].ProfileID.ValueString())
	assert.Equal(t, "analytics_ro_profile", out.Profiles[2].Name.ValueString())
}

// TestProfilesDataSource_InvalidClusterID locks the parse-error path.
func TestProfilesDataSource_InvalidClusterID(t *testing.T) {
	d := &profilesDataSource{client: acm.NewClient("http://unused", "t")}
	s := profilesSchema(t)
	ctx := context.Background()
	objType := s.Type().TerraformType(ctx).(tftypes.Object)
	raw := tftypes.NewValue(objType, map[string]tftypes.Value{
		"cluster_id": tftypes.NewValue(tftypes.String, "not-a-number"),
		"profiles":   tftypes.NewValue(objType.AttributeTypes["profiles"], nil),
	})
	req := datasource.ReadRequest{Config: tfsdk.Config{Schema: s, Raw: raw}}
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: emptyDSObjectValue(ctx, s)}}

	d.Read(ctx, req, &resp)
	require.True(t, resp.Diagnostics.HasError(), "expected an error for non-integer cluster_id")
}
