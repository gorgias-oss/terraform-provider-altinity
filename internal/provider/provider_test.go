// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProvider_Metadata(t *testing.T) {
	p := New("1.2.3")()
	var resp provider.MetadataResponse
	p.Metadata(context.Background(), provider.MetadataRequest{}, &resp)
	assert.Equal(t, "altinity", resp.TypeName)
	assert.Equal(t, "1.2.3", resp.Version)
}

func TestProvider_SchemaHasTokenAndURL(t *testing.T) {
	p := New("dev")()
	var resp provider.SchemaResponse
	p.Schema(context.Background(), provider.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError())

	tok, ok := resp.Schema.Attributes["api_token"]
	require.True(t, ok)
	assert.True(t, tok.IsSensitive(), "api_token must be Sensitive")
	assert.True(t, tok.IsOptional())

	url, ok := resp.Schema.Attributes["api_url"]
	require.True(t, ok)
	assert.True(t, url.IsOptional())
}

func TestProvider_RegistersDataSources(t *testing.T) {
	p := New("dev")()
	dsFactories := p.DataSources(context.Background())
	require.Len(t, dsFactories, 9) // environment, node_types, versions, storage_classes, zones, regions, instance_types, clickhouse_profiles, clickhouse_profile

	want := map[string]bool{
		"altinity_environment":         false,
		"altinity_node_types":          false,
		"altinity_clickhouse_versions": false,
		"altinity_storage_classes":     false,
		"altinity_zones":               false,
		"altinity_regions":             false,
		"altinity_instance_types":      false,
		"altinity_clickhouse_profiles": false,
		"altinity_clickhouse_profile":  false,
	}
	for _, f := range dsFactories {
		ds := f()
		require.NotNil(t, ds)
		var resp datasource.MetadataResponse
		ds.Metadata(context.Background(), datasource.MetadataRequest{ProviderTypeName: "altinity"}, &resp)
		if _, ok := want[resp.TypeName]; ok {
			want[resp.TypeName] = true
		}
	}
	for name, seen := range want {
		assert.Truef(t, seen, "data source %q not registered", name)
	}
}

func TestValidateAPIURL(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantError bool
		wantWarn  bool
	}{
		{name: "valid https", input: "https://acm.altinity.cloud/api", wantError: false, wantWarn: false},
		{name: "https with port", input: "https://acm.example.com:8443/api", wantError: false, wantWarn: false},
		{name: "http localhost ok", input: "http://localhost:8080/api", wantError: false, wantWarn: false},
		{name: "http loopback IPv4 ok", input: "http://127.0.0.1:8080/api", wantError: false, wantWarn: false},
		{name: "http loopback IPv6 ok", input: "http://[::1]:8080/api", wantError: false, wantWarn: false},
		{name: "http remote warns", input: "http://acm.example.com/api", wantError: false, wantWarn: true},
		{name: "no scheme errors", input: "not-a-url", wantError: true, wantWarn: false},
		{name: "ftp scheme errors", input: "ftp://acm.example.com/api", wantError: true, wantWarn: false},
		{name: "empty host errors", input: "https:///api", wantError: true, wantWarn: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diags := validateAPIURL(tc.input)
			if tc.wantError {
				assert.True(t, diags.HasError(), "expected error diag for %q, got: %v", tc.input, diags)
			} else {
				assert.False(t, diags.HasError(), "unexpected error diag for %q: %v", tc.input, diags)
			}
			if tc.wantWarn {
				assert.True(t, diags.WarningsCount() > 0, "expected warning diag for %q, got: %v", tc.input, diags)
			} else {
				assert.Equal(t, 0, diags.WarningsCount(), "unexpected warning diag for %q: %v", tc.input, diags)
			}
		})
	}
}

func TestValidateAPIURL_EmptyAccepted(t *testing.T) {
	// Empty is handled by the caller (falls back to DefaultBaseURL); the helper
	// is never invoked with an empty string, but we document the contract here.
	// This test just verifies a sane response in case the contract is violated.
	diags := validateAPIURL("")
	// An empty string parses as a URL with empty scheme and host, so it errors —
	// that's fine because the caller doesn't pass empty to this helper.
	assert.True(t, diags.HasError())
}

func TestProvider_RegistersAllResources(t *testing.T) {
	p := New("dev")()
	resFactories := p.Resources(context.Background())

	// environment, node_type, cluster, keeper, user, setting, profile, profile_setting.
	require.Len(t, resFactories, 8)

	want := map[string]bool{
		"altinity_environment":                false,
		"altinity_node_type":                  false,
		"altinity_clickhouse_cluster":         false,
		"altinity_clickhouse_keeper":          false,
		"altinity_clickhouse_user":            false,
		"altinity_clickhouse_cluster_setting": false,
		"altinity_clickhouse_profile":         false,
		"altinity_clickhouse_profile_setting": false,
	}
	for _, f := range resFactories {
		r := f()
		require.NotNil(t, r)
		var resp resource.MetadataResponse
		r.Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "altinity"}, &resp)
		if _, ok := want[resp.TypeName]; ok {
			want[resp.TypeName] = true
		}
	}
	for name, seen := range want {
		assert.Truef(t, seen, "resource %q not registered", name)
	}
}
