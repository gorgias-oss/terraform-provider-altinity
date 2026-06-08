// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Gorgias/terraform-provider-altinity/internal/acm"
)

var (
	_ datasource.DataSource              = (*storageClassesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*storageClassesDataSource)(nil)
)

// storageClassesDataSource lists the storage classes available in an
// environment (altinity_storage_classes), so cluster `storage_class` can be
// chosen from real values (e.g. pd-balanced, pd-ssd) rather than guessed.
type storageClassesDataSource struct {
	client *acm.Client
}

// NewStorageClassesDataSource is the constructor registered with the provider.
func NewStorageClassesDataSource() datasource.DataSource {
	return &storageClassesDataSource{}
}

type storageClassesDataSourceModel struct {
	Environment    types.String            `tfsdk:"environment"`
	Platform       types.String            `tfsdk:"platform"`
	StorageClasses []storageClassItemModel `tfsdk:"storage_classes"`
}

type storageClassItemModel struct {
	Code types.String `tfsdk:"code"`
	Name types.String `tfsdk:"name"`
}

func (d *storageClassesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_storage_classes"
}

func (d *storageClassesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "List the storage classes available in an environment " +
			"(GET /cloud/{environment}/options?type=storageClasses). Use a code as a " +
			"cluster's storage_class.",
		Attributes: map[string]schema.Attribute{
			"environment": schema.StringAttribute{
				Required:    true,
				Description: "ACM environment id to list storage classes for.",
			},
			"platform": schema.StringAttribute{
				Optional:    true,
				Description: "Platform to query (e.g. kubernetes). Pass the environment's `type`.",
			},
			"storage_classes": schema.ListNestedAttribute{
				Computed:    true,
				Description: "The available storage classes.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"code": schema.StringAttribute{Computed: true, Description: "Storage class code (use as cluster storage_class)."},
						"name": schema.StringAttribute{Computed: true},
					},
				},
			},
		},
	}
}

func (d *storageClassesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *storageClassesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg storageClassesDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	opts, err := d.client.ListCloudOptions(ctx, cfg.Environment.ValueString(), cfg.Platform.ValueString(), "storageClasses")
	if err != nil {
		resp.Diagnostics.AddError("Failed to list storage classes", dataSourceErrorDetail("ListCloudOptions(storageClasses)", err))
		return
	}

	cfg.StorageClasses = make([]storageClassItemModel, 0, len(opts))
	for _, o := range opts {
		cfg.StorageClasses = append(cfg.StorageClasses, storageClassItemModel{
			Code: types.StringValue(o.Code),
			Name: types.StringValue(o.Name),
		})
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
