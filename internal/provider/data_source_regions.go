// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

var (
	_ datasource.DataSource              = (*regionsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*regionsDataSource)(nil)
)

// regionsDataSource lists the regions available for a cloud provider
// (altinity_regions), so an altinity_environment's `region` can be chosen from
// real values rather than guessed. It uses the non-environment-scoped
// GET /cloud/options endpoint because region selection happens before any
// environment exists.
type regionsDataSource struct {
	client *acm.Client
}

// NewRegionsDataSource is the constructor registered with the provider.
func NewRegionsDataSource() datasource.DataSource {
	return &regionsDataSource{}
}

type regionsDataSourceModel struct {
	CloudProvider types.String      `tfsdk:"cloud_provider"`
	Regions       []regionItemModel `tfsdk:"regions"`
}

type regionItemModel struct {
	Code types.String `tfsdk:"code"`
	Name types.String `tfsdk:"name"`
}

func (d *regionsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_regions"
}

func (d *regionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "List the regions available for a cloud provider " +
			"(GET /cloud/options?type=regions&provider=<cloud_provider>). Use a region " +
			"code as an altinity_environment's `region`.",
		Attributes: map[string]schema.Attribute{
			"cloud_provider": schema.StringAttribute{
				Required:    true,
				Description: "Cloud provider to list regions for (e.g. aws, gcp).",
			},
			"regions": schema.ListNestedAttribute{
				Computed:    true,
				Description: "The available regions for the provider.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"code": schema.StringAttribute{Computed: true, Description: "Region code (use as an environment's region)."},
						"name": schema.StringAttribute{Computed: true, Description: "Human-readable region name."},
					},
				},
			},
		},
	}
}

func (d *regionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *regionsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg regionsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	opts, err := d.client.ListCloudOptionsGlobal(ctx, cfg.CloudProvider.ValueString(), "regions")
	if err != nil {
		resp.Diagnostics.AddError("Failed to list regions", dataSourceErrorDetail("ListCloudOptionsGlobal(regions)", err))
		return
	}

	cfg.Regions = make([]regionItemModel, 0, len(opts))
	for _, o := range opts {
		cfg.Regions = append(cfg.Regions, regionItemModel{
			Code: types.StringValue(o.Code),
			Name: types.StringValue(o.Name),
		})
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
