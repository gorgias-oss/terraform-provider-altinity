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

	"github.com/Gorgias/terraform-provider-altinity/internal/acm"
)

var (
	_ datasource.DataSource              = (*profilesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*profilesDataSource)(nil)
)

// profilesDataSource lists the settings profiles on a cluster
// (altinity_clickhouse_profiles), via GET /cluster/{cluster}/profiles.
//
// Common use: discover the ACM-bootstrapped `default` / `readonly` profile
// ids without hard-coding them. Managed (terraform-owned) profiles are
// included in the result the same as ACM-managed ones.
type profilesDataSource struct {
	client *acm.Client
}

// NewProfilesDataSource is the constructor registered with the provider.
func NewProfilesDataSource() datasource.DataSource {
	return &profilesDataSource{}
}

type profilesDataSourceModel struct {
	ClusterID types.String       `tfsdk:"cluster_id"`
	Profiles  []profileDataModel `tfsdk:"profiles"`
}

// profileDataModel is one entry in the `profiles` list returned by the
// data source. profile_id is the int-as-string id (matching the resource
// `profile_id` shape) so it can be passed directly to other resources.
type profileDataModel struct {
	ProfileID   types.String `tfsdk:"profile_id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
}

func (d *profilesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_clickhouse_profiles"
}

func (d *profilesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "List the settings profiles on a cluster " +
			"(GET /cluster/{cluster}/profiles). Returns both ACM-bootstrapped " +
			"profiles (`default`, `readonly`) and any profiles created via " +
			"`altinity_clickhouse_profile`. Use to look up bootstrap profile ids " +
			"without hard-coding them.",
		Attributes: map[string]schema.Attribute{
			"cluster_id": schema.StringAttribute{
				Required:    true,
				Description: "The ACM cluster id (integer, stored as string) whose profiles to list.",
			},
			"profiles": schema.ListNestedAttribute{
				Computed:    true,
				Description: "All profiles on the cluster, in ACM's returned order.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"profile_id": schema.StringAttribute{
							Computed:    true,
							Description: "The ACM-internal profile id (integer, stored as string).",
						},
						"name": schema.StringAttribute{
							Computed:    true,
							Description: "Profile name. ACM auto-creates `default` and `readonly`; other names belong to terraform-managed or ACM-UI-created profiles.",
						},
						"description": schema.StringAttribute{
							Computed:    true,
							Description: "Optional human-readable description set on the profile.",
						},
					},
				},
			},
		},
	}
}

func (d *profilesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *profilesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg profilesDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	clusterID, err := parseACMID("cluster_id", cfg.ClusterID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid cluster_id", err.Error())
		return
	}

	profiles, err := d.client.ListProfiles(ctx, clusterID)
	if err != nil {
		resp.Diagnostics.AddError("Failed to list profiles", dataSourceErrorDetail("ListProfiles", err))
		return
	}

	out := make([]profileDataModel, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, profileDataModel{
			ProfileID:   types.StringValue(strconv.FormatInt(p.ID, 10)),
			Name:        types.StringValue(p.Name),
			Description: types.StringValue(p.Description),
		})
	}
	cfg.Profiles = out

	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
