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
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

func instanceTypesSchema(t *testing.T) dschema.Schema {
	t.Helper()
	var resp datasource.SchemaResponse
	NewInstanceTypesDataSource().Schema(context.Background(), datasource.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError())
	return resp.Schema
}

func TestInstanceTypesDataSource_Read(t *testing.T) {
	body, err := os.ReadFile("../acm/testdata/instance_types.json")
	require.NoError(t, err)

	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	d := &instanceTypesDataSource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := instanceTypesSchema(t)
	ctx := context.Background()

	objType := s.Type().TerraformType(ctx).(tftypes.Object)
	raw := tftypes.NewValue(objType, map[string]tftypes.Value{
		"cloud_provider": tftypes.NewValue(tftypes.String, "gcp"),
		"region":         tftypes.NewValue(tftypes.String, "us-east1"),
		"zones":          tftypes.NewValue(objType.AttributeTypes["zones"], nil),
		"instance_types": tftypes.NewValue(objType.AttributeTypes["instance_types"], nil),
	})
	req := datasource.ReadRequest{Config: tfsdk.Config{Schema: s, Raw: raw}}
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: emptyDSObjectValue(ctx, s)}}

	d.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "read diags: %v", resp.Diagnostics)

	assert.Equal(t, "/cloud/options", gotPath)
	assert.Contains(t, gotQuery, "platform=gcp")
	assert.Contains(t, gotQuery, "region=us-east1")

	var out instanceTypesDataSourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	require.NotEmpty(t, out.Zones)
	require.NotEmpty(t, out.InstanceTypes)
	assert.NotEmpty(t, out.InstanceTypes[0].Name.ValueString())
	assert.Greater(t, out.InstanceTypes[0].CPU.ValueFloat64(), float64(0))
	assert.Greater(t, out.InstanceTypes[0].Memory.ValueFloat64(), float64(0))
}
