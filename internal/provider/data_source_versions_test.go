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
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

func TestParseVersionMajorMinor(t *testing.T) {
	maj, min := parseVersionMajorMinor("25.8.16.10002.altinitystable")
	assert.Equal(t, 25, maj)
	assert.Equal(t, 8, min)
	maj, min = parseVersionMajorMinor("26.1.11.20001.altinityantalya")
	assert.Equal(t, 26, maj)
	assert.Equal(t, 1, min)
	maj, _ = parseVersionMajorMinor("garbage")
	assert.Equal(t, -1, maj)
}

func versionsSchema(t *testing.T) dschema.Schema {
	t.Helper()
	var resp datasource.SchemaResponse
	NewVersionsDataSource().Schema(context.Background(), datasource.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError())
	return resp.Schema
}

func TestVersionsDataSource_FilterAndLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"code":"24.8.14.10545.altinitystable","name":"24.8 Altinity Stable"},
			{"code":"25.3.14.14","name":"25.3.14.14"},
			{"code":"25.8.16.10002.altinitystable","name":"25.8 Altinity Stable"},
			{"code":"25.8.23.13","name":"25.8.23.13"},
			{"code":"26.3.10.62","name":"26.3.10.62"}
		]}`))
	}))
	t.Cleanup(srv.Close)

	d := &versionsDataSource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := versionsSchema(t)
	ctx := context.Background()

	objType := s.Type().TerraformType(ctx).(tftypes.Object)
	raw := tftypes.NewValue(objType, map[string]tftypes.Value{
		"environment": tftypes.NewValue(tftypes.String, "2267"),
		"platform":    tftypes.NewValue(tftypes.String, "kubernetes"),
		"major":       tftypes.NewValue(tftypes.Number, 25),
		"minor":       tftypes.NewValue(tftypes.Number, nil),
		"stream":      tftypes.NewValue(tftypes.String, nil),
		"latest":      tftypes.NewValue(tftypes.String, nil),
		"versions":    tftypes.NewValue(objType.AttributeTypes["versions"], nil),
	})
	req := datasource.ReadRequest{Config: tfsdk.Config{Schema: s, Raw: raw}}
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: emptyDSObjectValue(ctx, s)}}

	d.Read(ctx, req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "read diags: %v", resp.Diagnostics)

	var out versionsDataSourceModel
	require.False(t, resp.State.Get(ctx, &out).HasError())
	require.Len(t, out.Versions, 3, "only major=25 versions")
	for _, v := range out.Versions {
		assert.Equal(t, int64(25), v.Major.ValueInt64())
	}
	assert.Equal(t, "25.8.23.13", out.Latest.ValueString(), "latest among the major=25 set")
}

func TestVersionStream(t *testing.T) {
	// stream is the single classifier (3 values: altinity-stable | altinity-antalya | upstream).
	assert.Equal(t, "altinity-antalya", versionStream("26.1.11.20001.altinityantalya", "", ""))
	assert.Equal(t, "altinity-stable", versionStream("25.8.16.10002.altinitystable", "25.8 Altinity Stable", "altinity/clickhouse-server"))
	assert.Equal(t, "upstream", versionStream("22.3.15.33", "[EOL] 22.3.15.33 ClickHouse LTS", "clickhouse/clickhouse-server"))
	assert.Equal(t, "upstream", versionStream("25.8.23.13", "25.8.23.13", "clickhouse/clickhouse-server"))

	// repo is authoritative: an Altinity repo with no "altinity" in code/name
	// still classifies as an Altinity build.
	assert.Equal(t, "altinity-stable", versionStream("25.8.23.13", "25.8.23.13", "altinity/clickhouse-server"))
}

// versionsRawConfig builds a config value for the versions data source with the
// given stream filter (nil = unset). major/minor are left unset.
func versionsRawConfig(ctx context.Context, s dschema.Schema, stream *string) tftypes.Value {
	objType := s.Type().TerraformType(ctx).(tftypes.Object)
	streamVal := tftypes.NewValue(tftypes.String, nil)
	if stream != nil {
		streamVal = tftypes.NewValue(tftypes.String, *stream)
	}
	return tftypes.NewValue(objType, map[string]tftypes.Value{
		"environment": tftypes.NewValue(tftypes.String, "2267"),
		"platform":    tftypes.NewValue(tftypes.String, "kubernetes"),
		"major":       tftypes.NewValue(tftypes.Number, nil),
		"minor":       tftypes.NewValue(tftypes.Number, nil),
		"stream":      streamVal,
		"latest":      tftypes.NewValue(tftypes.String, nil),
		"versions":    tftypes.NewValue(objType.AttributeTypes["versions"], nil),
	})
}

func versionsTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"code":"25.8.16.10002.altinitystable","name":"25.8 Altinity Stable","repo":"altinity/clickhouse-server"},
			{"code":"25.8.23.13","name":"25.8.23.13","repo":"clickhouse/clickhouse-server"},
			{"code":"25.8.16.20002.altinityantalya","name":"25.8 Antalya","repo":"altinity/clickhouse-server"},
			{"code":"26.1.11.20001.altinityantalya","name":"26.1 Antalya","repo":"altinity/clickhouse-server"}
		]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func readVersions(t *testing.T, d *versionsDataSource, s dschema.Schema, stream *string) (versionsDataSourceModel, diag.Diagnostics) {
	t.Helper()
	ctx := context.Background()
	req := datasource.ReadRequest{Config: tfsdk.Config{Schema: s, Raw: versionsRawConfig(ctx, s, stream)}}
	resp := datasource.ReadResponse{State: tfsdk.State{Schema: s, Raw: emptyDSObjectValue(ctx, s)}}
	d.Read(ctx, req, &resp)
	var out versionsDataSourceModel
	if !resp.Diagnostics.HasError() {
		require.False(t, resp.State.Get(ctx, &out).HasError())
	}
	return out, resp.Diagnostics
}

func TestVersionsDataSource_StreamFilter(t *testing.T) {
	srv := versionsTestServer(t)
	d := &versionsDataSource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := versionsSchema(t)

	// altinity-antalya: only the two antalya builds; latest is the newest.
	antalya := "altinity-antalya"
	out, diags := readVersions(t, d, s, &antalya)
	require.False(t, diags.HasError(), "diags: %v", diags)
	require.Len(t, out.Versions, 2)
	for _, v := range out.Versions {
		assert.Equal(t, "altinity-antalya", v.Stream.ValueString(), "code %s", v.Code.ValueString())
	}
	assert.Equal(t, "26.1.11.20001.altinityantalya", out.Latest.ValueString())

	// altinity-stable: only the one stable build (NOT outranked by antalya).
	stable := "altinity-stable"
	out, diags = readVersions(t, d, s, &stable)
	require.False(t, diags.HasError(), "diags: %v", diags)
	require.Len(t, out.Versions, 1)
	assert.Equal(t, "25.8.16.10002.altinitystable", out.Latest.ValueString(), "latest stays within the stable line")

	// upstream: only the plain ClickHouse build.
	upstream := "upstream"
	out, diags = readVersions(t, d, s, &upstream)
	require.False(t, diags.HasError(), "diags: %v", diags)
	require.Len(t, out.Versions, 1)
	assert.Equal(t, "upstream", out.Versions[0].Stream.ValueString())
}

// TestVersionsDataSource_InvalidStream asserts the stream value is validated.
func TestVersionsDataSource_InvalidStream(t *testing.T) {
	srv := versionsTestServer(t)
	d := &versionsDataSource{client: acm.NewClient(srv.URL, "t", acm.WithHTTPClient(srv.Client()))}
	s := versionsSchema(t)

	bogus := "antalya" // not one of altinity-stable | altinity-antalya | upstream
	_, diags := readVersions(t, d, s, &bogus)
	require.True(t, diags.HasError(), "an invalid stream must error")
	assert.Contains(t, diags.Errors()[0].Summary(), "Invalid stream")
}

func TestVersionDowngradeGuard_NullState(t *testing.T) {
	// When version is null in state (first apply, version unset by user),
	// the guard must not fire — it has nothing to compare against.
	req := planmodifier.StringRequest{
		StateValue: basetypes.NewStringNull(),
		PlanValue:  basetypes.NewStringValue("25.8.16.10002.altinitystable"),
	}
	var resp planmodifier.StringResponse
	versionDowngradeGuard{}.PlanModifyString(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "downgrade guard must not fire when prior state is null")
}
