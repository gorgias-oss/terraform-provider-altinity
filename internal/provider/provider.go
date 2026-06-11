// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

// Package provider implements the Terraform Plugin Framework provider for
// Altinity.Cloud ClickHouse clusters. It serves protocol v6 (Terraform >= 1.0,
// floor 1.5.7, and OpenTofu) and deliberately avoids post-1.5 features
// (provider-defined functions, ephemeral resources, write-only attributes) so
// one binary serves both Terraform 1.5.7 and OpenTofu (design §9).
package provider

import (
	"context"
	"net/url"
	"os"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

// tokenEnvVar is the environment-variable fallback for the API token.
const tokenEnvVar = "ALTINITYCLOUD_API_TOKEN"

// Ensure the provider satisfies the framework interface.
var _ provider.Provider = (*altinityProvider)(nil)

// altinityProvider is the provider implementation.
type altinityProvider struct {
	// version is set by the build (main.version) and surfaced in Metadata.
	version string
}

// New returns a provider.Provider factory for the given build version. The cmd
// entrypoint passes this to providerserver.Serve.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &altinityProvider{version: version}
	}
}

// providerModel maps the provider configuration block.
type providerModel struct {
	APIToken types.String `tfsdk:"api_token"`
	APIURL   types.String `tfsdk:"api_url"`
}

func (p *altinityProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "altinity"
	resp.Version = p.version
}

func (p *altinityProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manage Altinity.Cloud ClickHouse clusters via the Altinity Cloud Manager (ACM) REST API.",
		Attributes: map[string]schema.Attribute{
			"api_token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				Description: "ACM API token (X-Auth-Token). Minted in the ACM UI under " +
					"My Account -> Anywhere API Access. Falls back to the " +
					tokenEnvVar + " environment variable when unset.",
			},
			"api_url": schema.StringAttribute{
				Optional: true,
				Description: "ACM API base URL. Defaults to " + acm.DefaultBaseURL +
					".",
			},
		},
	}
}

func (p *altinityProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Token: explicit config wins, else env fallback.
	token := cfg.APIToken.ValueString()
	if token == "" {
		token = os.Getenv(tokenEnvVar)
	}
	if token == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("api_token"),
			"Missing ACM API token",
			"Set the provider api_token attribute or the "+tokenEnvVar+" environment variable.",
		)
		return
	}

	baseURL := cfg.APIURL.ValueString()
	if baseURL != "" {
		if diag := validateAPIURL(baseURL); diag != nil {
			resp.Diagnostics.Append(diag...)
			if resp.Diagnostics.HasError() {
				return
			}
		}
	}
	if baseURL == "" {
		baseURL = acm.DefaultBaseURL
	}

	client := acm.NewClient(baseURL, token)

	// Fail early on a missing/invalid/expired token: a cheap authenticated call
	// here surfaces an auth problem during provider configuration instead of
	// deep inside a later create/apply (where a bad token previously launched a
	// phantom cluster and failed mid-poll).
	if err := preflightAuth(ctx, client); err != nil {
		if acm.IsUnauthorized(err) {
			resp.Diagnostics.AddAttributeError(
				path.Root("api_token"),
				"ACM API token rejected",
				"Altinity Cloud Manager rejected the configured credentials: "+err.Error()+
					"\n\nVerify api_token (or the "+tokenEnvVar+" environment variable) is a current, valid ACM API token.",
			)
			return
		}
		resp.Diagnostics.AddError(
			"Cannot reach Altinity Cloud Manager",
			"Failed to verify the ACM connection during provider configuration: "+err.Error()+
				"\n\nCheck api_url and network connectivity.",
		)
		return
	}

	// The client is handed to data sources and resources via their Configure.
	resp.DataSourceData = client
	resp.ResourceData = client
}

// preflightAuth makes a cheap authenticated call (list environments) so an
// invalid or expired token fails provider configuration immediately.
func preflightAuth(ctx context.Context, c *acm.Client) error {
	_, err := c.ListEnvironments(ctx)
	return err
}

// validateAPIURL verifies the api_url is well-formed: http or https scheme and
// a non-empty host. An http URL pointing at a non-loopback host triggers a
// warning (we allow it for mock servers but it sends the token in cleartext).
// Returns nil diagnostics when the URL is acceptable.
func validateAPIURL(raw string) diag.Diagnostics {
	var diags diag.Diagnostics
	u, err := url.Parse(raw)
	if err != nil {
		diags.AddAttributeError(
			path.Root("api_url"),
			"Invalid api_url",
			"api_url must be a valid URL with an http or https scheme; parse error: "+err.Error(),
		)
		return diags
	}
	switch u.Scheme {
	case "http", "https":
	default:
		diags.AddAttributeError(
			path.Root("api_url"),
			"Invalid api_url scheme",
			"api_url must have scheme http or https; got "+strconv.Quote(u.Scheme)+" in "+strconv.Quote(raw),
		)
		return diags
	}
	if u.Host == "" {
		diags.AddAttributeError(
			path.Root("api_url"),
			"Invalid api_url host",
			"api_url must include a non-empty host; got "+strconv.Quote(raw),
		)
		return diags
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		diags.AddAttributeWarning(
			path.Root("api_url"),
			"Insecure api_url",
			"api_url uses http with a non-loopback host ("+u.Host+"); the ACM API token will be sent over cleartext. "+
				"Use https in production; http is only appropriate for local test or mock servers.",
		)
	}
	return diags
}

// isLoopbackHost reports whether host refers to a local loopback address. Used
// by validateAPIURL to scope the cleartext-http warning to true remote hosts.
func isLoopbackHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// DataSources registers the provider's data sources.
func (p *altinityProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewEnvironmentDataSource,
		NewNodeTypesDataSource,
		NewVersionsDataSource,
		NewStorageClassesDataSource,
		NewZonesDataSource,
		NewRegionsDataSource,       // regions per cloud provider
		NewInstanceTypesDataSource, // available instance types per provider+region
		NewProfilesDataSource,      // list all
		NewProfileDataSource,       // find one by name
	}
}

// Resources registers the provider's managed resources.
//
// REGISTRATION LIST — resource agents add their constructors here during the
// integration step. Each entry is `NewXxxResource` from its resource file:
//
//	NewClusterResource,   // altinity_clickhouse_cluster (clusters.go / resource_cluster.go)
//	NewSettingResource,   // altinity_clickhouse_cluster_setting
//	NewProfileResource,   // altinity_clickhouse_profile
//	NewUserResource,      // altinity_clickhouse_user
//
// Keep this list the single source of truth for resource wiring.
func (p *altinityProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewEnvironmentResource,    // altinity_environment
		NewNodeTypeResource,       // altinity_node_type
		NewClusterResource,        // altinity_clickhouse_cluster
		NewKeeperResource,         // altinity_clickhouse_keeper
		NewUserResource,           // altinity_clickhouse_user
		NewSettingResource,        // altinity_clickhouse_cluster_setting
		NewProfileResource,        // altinity_clickhouse_profile
		NewProfileSettingResource, // altinity_clickhouse_profile_setting
	}
}
