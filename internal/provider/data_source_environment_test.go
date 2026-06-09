// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

// newEnvFixtureServer serves the captured /environments payload.
func newEnvFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	body, err := os.ReadFile("testdata/environments.json")
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/environments", r.URL.Path)
		assert.Equal(t, "test-token", r.Header.Get("X-Auth-Token"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// readEnvDataSource drives the data source Read end-to-end against an
// httptest-backed ACM client (no network, no TF binary). It builds the
// framework Config/State from the schema, mirroring what the runtime does.
func readEnvDataSource(t *testing.T, client *acm.Client, name string) (environmentDataSourceModel, diag.Diagnostics) {
	t.Helper()
	ctx := context.Background()

	ds := NewEnvironmentDataSource().(*environmentDataSource)
	ds.client = client

	var schemaResp datasource.SchemaResponse
	ds.Schema(ctx, datasource.SchemaRequest{}, &schemaResp)
	require.False(t, schemaResp.Diagnostics.HasError(), "schema diags: %v", schemaResp.Diagnostics)

	schema := schemaResp.Schema
	objType := schema.Type().TerraformType(ctx).(tftypes.Object)

	// Build a config value: name set, computed attrs null.
	vals := map[string]tftypes.Value{}
	for attr := range objType.AttributeTypes {
		switch attr {
		case "name":
			vals[attr] = tftypes.NewValue(tftypes.String, name)
		default:
			vals[attr] = tftypes.NewValue(tftypes.String, nil)
		}
	}
	cfgVal := tftypes.NewValue(objType, vals)

	req := datasource.ReadRequest{
		Config: tfsdk.Config{Raw: cfgVal, Schema: schema},
	}
	resp := datasource.ReadResponse{
		State: tfsdk.State{Schema: schema, Raw: tftypes.NewValue(objType, nil)},
	}
	ds.Read(ctx, req, &resp)

	var model environmentDataSourceModel
	if !resp.Diagnostics.HasError() {
		resp.State.Get(ctx, &model)
	}
	return model, resp.Diagnostics
}

func TestEnvironmentDataSource_ReadByName(t *testing.T) {
	srv := newEnvFixtureServer(t)
	client := acm.NewClient(srv.URL, "test-token", acm.WithHTTPClient(srv.Client()))

	model, diags := readEnvDataSource(t, client, "example-env")
	require.False(t, diags.HasError(), "diags: %v", diags)

	assert.Equal(t, "1", model.ID.ValueString())
	assert.Equal(t, "example-env", model.Name.ValueString())
	assert.Equal(t, "kubernetes", model.Type.ValueString())
	assert.Equal(t, "online", model.Status.ValueString())
	assert.Equal(t, "example-env.altinity.cloud", model.Domain.ValueString())
	assert.Equal(t, "running", model.State.ValueString())

	// parent_id is null when the upstream `id_parent` is null (top-level env).
	// Locks the design choice from CHANGELOG #11: callers can write
	// `data.altinity_environment.x.parent_id == null` rather than `== ""`.
	assert.True(t, model.ParentID.IsNull(),
		"parent_id should be null when id_parent is null in the response, got %q", model.ParentID.ValueString())

	// owner_id is always populated for a real environment (mirror the fixture).
	assert.Equal(t, "1", model.OwnerID.ValueString())
}

func TestEnvironmentDataSource_NotFound(t *testing.T) {
	srv := newEnvFixtureServer(t)
	client := acm.NewClient(srv.URL, "test-token", acm.WithHTTPClient(srv.Client()))

	_, diags := readEnvDataSource(t, client, "no-such-env")
	require.True(t, diags.HasError())
}

func TestEnvironmentDataSource_Metadata(t *testing.T) {
	ds := NewEnvironmentDataSource()
	var resp datasource.MetadataResponse
	ds.Metadata(context.Background(), datasource.MetadataRequest{ProviderTypeName: "altinity"}, &resp)
	assert.Equal(t, "altinity_environment", resp.TypeName)
}
