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

func zonesSchema(t *testing.T) dschema.Schema {
	t.Helper()
	var resp datasource.SchemaResponse
	NewZonesDataSource().Schema(context.Background(), datasource.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError())
	return resp.Schema
}

func TestZonesDataSource_Read(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":["us-east1-b","us-east1-c"]}`))
	}))
	t.Cleanup(srv.Close)

	d := &zonesDataSource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := zonesSchema(t)
	ctx := context.Background()

	objType := s.Type().TerraformType(ctx).(tftypes.Object)
	raw := tftypes.NewValue(objType, map[string]tftypes.Value{
		"environment": tftypes.NewValue(tftypes.String, "2267"),
		"platform":    tftypes.NewValue(tftypes.String, "kubernetes"),
		"zones":       tftypes.NewValue(objType.AttributeTypes["zones"], nil),
	})
	req := datasource.ReadRequest{Config: tfsdk.Config{Schema: s, Raw: raw}}
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: emptyDSObjectValue(ctx, s)}}

	d.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "read diags: %v", resp.Diagnostics)
	assert.Contains(t, gotQuery, "type=zones")

	var out zonesDataSourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	assert.Equal(t, []string{"us-east1-b", "us-east1-c"}, out.Zones)
}
