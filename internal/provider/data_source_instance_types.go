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
	_ datasource.DataSource              = (*instanceTypesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*instanceTypesDataSource)(nil)
)

// instanceTypesDataSource lists the instance types available for a cloud
// provider in a region (altinity_instance_types). Use it to discover a valid
// `code` (and its cpu/memory) for an altinity_node_type.
type instanceTypesDataSource struct {
	client *acm.Client
}

// NewInstanceTypesDataSource is the constructor registered with the provider.
func NewInstanceTypesDataSource() datasource.DataSource {
	return &instanceTypesDataSource{}
}

type instanceTypesDataSourceModel struct {
	CloudProvider types.String        `tfsdk:"cloud_provider"`
	Region        types.String        `tfsdk:"region"`
	Zones         []types.String      `tfsdk:"zones"`
	InstanceTypes []instanceTypeModel `tfsdk:"instance_types"`
}

type instanceTypeModel struct {
	Name              types.String  `tfsdk:"name"`
	CPU               types.Float64 `tfsdk:"cpu"`
	CPUAllocatable    types.Float64 `tfsdk:"cpu_allocatable"`
	Memory            types.Float64 `tfsdk:"memory"`
	MemoryAllocatable types.Float64 `tfsdk:"memory_allocatable"`
}

func (d *instanceTypesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_instance_types"
}

func (d *instanceTypesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "List the instance types available for a cloud provider in a region " +
			"(GET /cloud/options?platform=<cloud_provider>&region=<region>&type=*). Use an " +
			"instance type's name as an altinity_node_type `code`, and its cpu/memory as the " +
			"node type's cpu/memory.",
		Attributes: map[string]schema.Attribute{
			"cloud_provider": schema.StringAttribute{
				Required:    true,
				Description: "Cloud provider (e.g. gcp, aws).",
			},
			"region": schema.StringAttribute{
				Required:    true,
				Description: "Region code (see the altinity_regions data source).",
			},
			"zones": schema.ListAttribute{
				ElementType: types.StringType,
				Computed:    true,
				Description: "Availability zones in the region.",
			},
			"instance_types": schema.ListNestedAttribute{
				Computed:    true,
				Description: "The available instance types.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name":               schema.StringAttribute{Computed: true, Description: "Instance type name (use as a node type code)."},
						"cpu":                schema.Float64Attribute{Computed: true, Description: "vCPUs."},
						"cpu_allocatable":    schema.Float64Attribute{Computed: true, Description: "Allocatable vCPUs (after system overhead)."},
						"memory":             schema.Float64Attribute{Computed: true, Description: "Memory in GiB."},
						"memory_allocatable": schema.Float64Attribute{Computed: true, Description: "Allocatable memory in GiB."},
					},
				},
			},
		},
	}
}

func (d *instanceTypesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *instanceTypesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg instanceTypesDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	zones, types_, err := d.client.ListInstanceTypes(ctx, cfg.CloudProvider.ValueString(), cfg.Region.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to list instance types", dataSourceErrorDetail("ListInstanceTypes", err))
		return
	}

	cfg.Zones = make([]types.String, 0, len(zones))
	for _, z := range zones {
		cfg.Zones = append(cfg.Zones, types.StringValue(z))
	}
	cfg.InstanceTypes = make([]instanceTypeModel, 0, len(types_))
	for _, it := range types_ {
		cfg.InstanceTypes = append(cfg.InstanceTypes, instanceTypeModel{
			Name:              types.StringValue(it.Name),
			CPU:               types.Float64Value(it.CPU),
			CPUAllocatable:    types.Float64Value(it.CPUAllocatable),
			Memory:            types.Float64Value(it.Memory),
			MemoryAllocatable: types.Float64Value(it.MemoryAllocatable),
		})
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
