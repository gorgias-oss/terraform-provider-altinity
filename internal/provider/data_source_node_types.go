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
	_ datasource.DataSource              = (*nodeTypesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*nodeTypesDataSource)(nil)
)

// nodeTypesDataSource lists the node (instance) types available in an
// environment (altinity_node_types). Use it to discover valid instance_type
// values per scope — e.g. "clickhouse" for clusters, "zookeeper" for keepers.
type nodeTypesDataSource struct {
	client *acm.Client
}

// NewNodeTypesDataSource is the constructor registered with the provider.
func NewNodeTypesDataSource() datasource.DataSource {
	return &nodeTypesDataSource{}
}

type nodeTypesDataSourceModel struct {
	Environment types.String        `tfsdk:"environment"`
	Scope       types.String        `tfsdk:"scope"`
	NodeTypes   []nodeTypeItemModel `tfsdk:"node_types"`
}

type nodeTypeItemModel struct {
	Code         types.String  `tfsdk:"code"`
	Name         types.String  `tfsdk:"name"`
	Scope        types.String  `tfsdk:"scope"`
	CPU          types.Float64 `tfsdk:"cpu"`
	Memory       types.Int64   `tfsdk:"memory"`
	StorageClass types.String  `tfsdk:"storage_class"`
	IsSpot       types.Bool    `tfsdk:"is_spot"`
}

func (d *nodeTypesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_node_types"
}

func (d *nodeTypesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "List the node (instance) types available in an environment " +
			"(GET /environment/{environment}/nodetypes). Filter by scope to find " +
			"valid instance_type values for clusters (clickhouse) or keepers (zookeeper).",
		Attributes: map[string]schema.Attribute{
			"environment": schema.StringAttribute{
				Required:    true,
				Description: "ACM environment id to list node types for.",
			},
			"scope": schema.StringAttribute{
				Optional:    true,
				Description: "Optional scope filter (e.g. clickhouse, zookeeper, system). When unset, all scopes are returned.",
			},
			"node_types": schema.ListNestedAttribute{
				Computed:    true,
				Description: "The matching node types.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"code":          schema.StringAttribute{Computed: true, Description: "Instance type code (use as instance_type/node_type)."},
						"name":          schema.StringAttribute{Computed: true},
						"scope":         schema.StringAttribute{Computed: true, Description: "clickhouse | zookeeper | system."},
						"cpu":           schema.Float64Attribute{Computed: true},
						"memory":        schema.Int64Attribute{Computed: true, Description: "Memory in MB."},
						"storage_class": schema.StringAttribute{Computed: true},
						"is_spot":       schema.BoolAttribute{Computed: true},
					},
				},
			},
		},
	}
}

func (d *nodeTypesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *nodeTypesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg nodeTypesDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	nts, err := d.client.ListNodeTypes(ctx, cfg.Environment.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to list node types", dataSourceErrorDetail("ListNodeTypes", err))
		return
	}

	scopeFilter := cfg.Scope.ValueString()
	cfg.NodeTypes = make([]nodeTypeItemModel, 0, len(nts))
	for _, nt := range nts {
		if scopeFilter != "" && nt.Scope != scopeFilter {
			continue
		}
		cfg.NodeTypes = append(cfg.NodeTypes, nodeTypeItemModel{
			Code:         types.StringValue(nt.Code),
			Name:         types.StringValue(nt.Name),
			Scope:        types.StringValue(nt.Scope),
			CPU:          types.Float64Value(nt.CPU),
			Memory:       types.Int64Value(nt.Memory),
			StorageClass: types.StringValue(nt.StorageClass),
			IsSpot:       types.BoolValue(nt.IsSpot),
		})
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
