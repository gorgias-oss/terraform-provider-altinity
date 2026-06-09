// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

var (
	_ datasource.DataSource              = (*profileDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*profileDataSource)(nil)
)

// profileDataSource resolves a single settings profile on a cluster by name
// (altinity_clickhouse_profile). Use to look up a specific bootstrap profile
// (e.g. `default` or `readonly`) without listing the whole cluster, or to
// reference an ACM-UI-created profile from Terraform without managing it.
//
// Errors with a clear diagnostic if the profile is not found, so a missing
// profile fails the plan immediately rather than silently producing empty
// values that propagate to downstream resources.
type profileDataSource struct {
	client *acm.Client
}

// NewProfileDataSource is the constructor registered with the provider.
func NewProfileDataSource() datasource.DataSource {
	return &profileDataSource{}
}

type profileDataSourceModel struct {
	ClusterID   types.String `tfsdk:"cluster_id"`
	Name        types.String `tfsdk:"name"`
	ProfileID   types.String `tfsdk:"profile_id"`
	Description types.String `tfsdk:"description"`
}

func (d *profileDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_clickhouse_profile"
}

func (d *profileDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Look up a single settings profile on a cluster by name " +
			"(GET /cluster/{cluster}/profiles, filtered to the matching name). " +
			"Errors if no profile with that name exists. Use this to reference " +
			"the ACM-bootstrapped `default` / `readonly` profiles or an " +
			"ACM-UI-created profile without managing it from Terraform.",
		Attributes: map[string]schema.Attribute{
			"cluster_id": schema.StringAttribute{
				Required:    true,
				Description: "The ACM cluster id (integer, stored as string) the profile belongs to.",
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "The profile name to resolve.",
			},
			"profile_id": schema.StringAttribute{
				Computed:    true,
				Description: "The ACM-internal profile id (integer, stored as string). Pass to `altinity_clickhouse_user.profile_id` or `altinity_clickhouse_profile_setting.profile_id`.",
			},
			"description": schema.StringAttribute{
				Computed:    true,
				Description: "Optional human-readable description set on the profile.",
			},
		},
	}
}

func (d *profileDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*acm.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Data Source Configure Type",
			fmt.Sprintf("Expected *acm.Client, got %T. This is a provider bug.", req.ProviderData),
		)
		return
	}
	d.client = client
}

func (d *profileDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg profileDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	clusterID, err := parseACMID("cluster_id", cfg.ClusterID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid cluster_id", err.Error())
		return
	}

	name := cfg.Name.ValueString()
	p, found, err := d.client.FindProfileByName(ctx, clusterID, name)
	if err != nil {
		resp.Diagnostics.AddError("Failed to look up profile", dataSourceErrorDetail("FindProfileByName", err))
		return
	}
	if !found {
		resp.Diagnostics.AddError(
			"Profile not found",
			fmt.Sprintf("No settings profile named %q exists on cluster %s. "+
				"Check `terraform output cluster_profiles` (or hit GET /cluster/%d/profiles) "+
				"to see what's available.", name, cfg.ClusterID.ValueString(), clusterID),
		)
		return
	}

	cfg.ProfileID = types.StringValue(strconv.FormatInt(p.ID, 10))
	cfg.Description = types.StringValue(p.Description)
	// Reflect ACM's canonical name back to state, in case ACM normalizes it.
	cfg.Name = types.StringValue(p.Name)

	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
