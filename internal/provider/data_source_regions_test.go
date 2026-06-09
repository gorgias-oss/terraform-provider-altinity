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

func regionsSchema(t *testing.T) dschema.Schema {
	t.Helper()
	var resp datasource.SchemaResponse
	NewRegionsDataSource().Schema(context.Background(), datasource.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError())
	return resp.Schema
}

func TestRegionsDataSource_Read(t *testing.T) {
	body, err := os.ReadFile("../acm/testdata/cloud_options_regions_aws.json")
	require.NoError(t, err)

	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	d := &regionsDataSource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := regionsSchema(t)
	ctx := context.Background()

	objType := s.Type().TerraformType(ctx).(tftypes.Object)
	raw := tftypes.NewValue(objType, map[string]tftypes.Value{
		"cloud_provider": tftypes.NewValue(tftypes.String, "aws"),
		"regions":        tftypes.NewValue(objType.AttributeTypes["regions"], nil),
	})
	req := datasource.ReadRequest{Config: tfsdk.Config{Schema: s, Raw: raw}}
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: emptyDSObjectValue(ctx, s)}}

	d.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "read diags: %v", resp.Diagnostics)

	// Per OQ-3: non-env-scoped endpoint, keyed on provider, type=regions.
	assert.Equal(t, "/cloud/options", gotPath)
	assert.Contains(t, gotQuery, "type=regions")
	assert.Contains(t, gotQuery, "provider=aws")

	var out regionsDataSourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	require.NotEmpty(t, out.Regions)

	var found bool
	for _, rg := range out.Regions {
		if rg.Code.ValueString() == "us-east-1" {
			found = true
			assert.Equal(t, "US East (N. Virginia)", rg.Name.ValueString())
		}
	}
	assert.True(t, found, "expected us-east-1 in regions output")
}
