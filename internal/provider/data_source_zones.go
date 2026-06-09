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
	_ datasource.DataSource              = (*zonesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*zonesDataSource)(nil)
)

// zonesDataSource lists the availability zones available in an environment
// (altinity_zones), for use in a cluster's azlist or a keeper's zones.
type zonesDataSource struct {
	client *acm.Client
}

// NewZonesDataSource is the constructor registered with the provider.
func NewZonesDataSource() datasource.DataSource {
	return &zonesDataSource{}
}

type zonesDataSourceModel struct {
	Environment types.String `tfsdk:"environment"`
	Platform    types.String `tfsdk:"platform"`
	Zones       []string     `tfsdk:"zones"`
}

func (d *zonesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_zones"
}

func (d *zonesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "List the availability zones available in an environment " +
			"(GET /cloud/{environment}/options?type=zones). Use for a cluster azlist " +
			"or a keeper's zones.",
		Attributes: map[string]schema.Attribute{
			"environment": schema.StringAttribute{
				Required:    true,
				Description: "ACM environment id to list zones for.",
			},
			"platform": schema.StringAttribute{
				Optional:    true,
				Description: "Platform to query (e.g. kubernetes). Pass the environment's `type`.",
			},
			"zones": schema.ListAttribute{
				ElementType: types.StringType,
				Computed:    true,
				Description: "The available availability-zone names.",
			},
		},
	}
}

func (d *zonesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *zonesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg zonesDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	zones, err := d.client.ListZones(ctx, cfg.Environment.ValueString(), cfg.Platform.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to list zones", dataSourceErrorDetail("ListZones", err))
		return
	}
	cfg.Zones = zones

	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
