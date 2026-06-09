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

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

func profileSchema(t *testing.T) dschema.Schema {
	t.Helper()
	var resp datasource.SchemaResponse
	NewProfileDataSource().Schema(context.Background(), datasource.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError())
	return resp.Schema
}

// readProfileDS exercises the Read path with a fake ACM and the given config
// values, returning the response state model + diagnostics.
func readProfileDS(t *testing.T, srv *httptest.Server, clusterID, name string) (profileDataSourceModel, datasource.ReadResponse) {
	t.Helper()
	d := &profileDataSource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := profileSchema(t)
	ctx := context.Background()

	objType := s.Type().TerraformType(ctx).(tftypes.Object)
	raw := tftypes.NewValue(objType, map[string]tftypes.Value{
		"cluster_id":  tftypes.NewValue(tftypes.String, clusterID),
		"name":        tftypes.NewValue(tftypes.String, name),
		"profile_id":  tftypes.NewValue(tftypes.String, nil),
		"description": tftypes.NewValue(tftypes.String, nil),
	})
	req := datasource.ReadRequest{Config: tfsdk.Config{Schema: s, Raw: raw}}
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: emptyDSObjectValue(ctx, s)}}
	d.Read(ctx, req, &resp)

	var out profileDataSourceModel
	if !resp.Diagnostics.HasError() {
		require.False(t, resp.State.Get(ctx, &out).HasError())
	}
	return out, resp
}

func TestProfileDataSource_FoundByName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"8145","name":"default","description":"Default Profile"},
			{"id":"8146","name":"readonly","description":"Read-only"},
			{"id":"8160","name":"analytics_ro_profile","description":"For analytics"}
		]}`))
	}))
	t.Cleanup(srv.Close)

	out, resp := readProfileDS(t, srv, "10091", "readonly")
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)
	assert.Equal(t, "8146", out.ProfileID.ValueString())
	assert.Equal(t, "readonly", out.Name.ValueString())
	assert.Equal(t, "Read-only", out.Description.ValueString())
}

func TestProfileDataSource_NotFoundErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"8145","name":"default"}]}`))
	}))
	t.Cleanup(srv.Close)

	_, resp := readProfileDS(t, srv, "10091", "no_such_profile")
	require.True(t, resp.Diagnostics.HasError(), "expected error for missing profile")
	assert.Contains(t, resp.Diagnostics[0].Summary(), "Profile not found")
}

func TestProfileDataSource_InvalidClusterID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server should never be hit — config validation fails first.
		t.Fatal("server should not be called for invalid cluster_id")
	}))
	t.Cleanup(srv.Close)

	_, resp := readProfileDS(t, srv, "not-a-number", "readonly")
	require.True(t, resp.Diagnostics.HasError())
}
