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

func nodeTypesSchema(t *testing.T) dschema.Schema {
	t.Helper()
	var resp datasource.SchemaResponse
	NewNodeTypesDataSource().Schema(context.Background(), datasource.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError())
	return resp.Schema
}

func emptyDSObjectValue(ctx context.Context, s dschema.Schema) tftypes.Value {
	objType := s.Type().TerraformType(ctx).(tftypes.Object)
	return tftypes.NewValue(objType, nil)
}

func TestNodeTypesDataSource_ReadFiltersByScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"1","code":"n2d-standard-2","scope":"clickhouse","cpu":"2","memory":"6001","capacity":"10","isSpot":false,"used":true},
			{"id":"2","code":"e2-standard-2","scope":"zookeeper","cpu":"2","memory":"6001","capacity":"10","isSpot":false,"used":false},
			{"id":"3","code":"e2-standard-2","scope":"system","cpu":"2","memory":"6001"}
		]}`))
	}))
	t.Cleanup(srv.Close)

	d := &nodeTypesDataSource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := nodeTypesSchema(t)
	ctx := context.Background()

	// tfsdk.Config has no Set(); build the input Raw directly with the two
	// configured attributes and a null computed node_types.
	objType := s.Type().TerraformType(ctx).(tftypes.Object)
	raw := tftypes.NewValue(objType, map[string]tftypes.Value{
		"environment": tftypes.NewValue(tftypes.String, "2267"),
		"scope":       tftypes.NewValue(tftypes.String, "zookeeper"),
		"node_types":  tftypes.NewValue(objType.AttributeTypes["node_types"], nil),
	})
	req := datasource.ReadRequest{Config: tfsdk.Config{Schema: s, Raw: raw}}
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: emptyDSObjectValue(ctx, s)}}

	d.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "read diags: %v", resp.Diagnostics)

	var out nodeTypesDataSourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	require.Len(t, out.NodeTypes, 1, "only the zookeeper-scoped node type matches")
	assert.Equal(t, "e2-standard-2", out.NodeTypes[0].Code.ValueString())
	assert.Equal(t, "zookeeper", out.NodeTypes[0].Scope.ValueString())
	assert.Equal(t, float64(2), out.NodeTypes[0].CPU.ValueFloat64())
	assert.Equal(t, int64(6001), out.NodeTypes[0].Memory.ValueInt64())
	// Enhancement: id, capacity, used surfaced.
	assert.Equal(t, "2", out.NodeTypes[0].ID.ValueString())
	assert.Equal(t, int64(10), out.NodeTypes[0].Capacity.ValueInt64())
	assert.False(t, out.NodeTypes[0].Used.ValueBool())
}
