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

// Ensure the data source satisfies the framework interfaces.
var (
	_ datasource.DataSource              = (*environmentDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*environmentDataSource)(nil)
)

// environmentDataSource resolves an Altinity.Cloud environment by name
// (altinity_environment). It backs the `environment` reference used when
// launching clusters.
type environmentDataSource struct {
	client *acm.Client
}

// NewEnvironmentDataSource is the constructor registered with the provider.
func NewEnvironmentDataSource() datasource.DataSource {
	return &environmentDataSource{}
}

// environmentDataSourceModel maps the altinity_environment schema.
type environmentDataSourceModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	NormalizedName types.String `tfsdk:"normalized_name"`
	DisplayName    types.String `tfsdk:"display_name"`
	Type           types.String `tfsdk:"type"`
	Domain         types.String `tfsdk:"domain"`
	Status         types.String `tfsdk:"status"`
	State          types.String `tfsdk:"state"`
	ParentID       types.String `tfsdk:"parent_id"`
	OwnerID        types.String `tfsdk:"owner_id"`
}

func (d *environmentDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_environment"
}

func (d *environmentDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Look up an Altinity.Cloud environment by name (GET /environments).",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:    true,
				Description: "The environment name to resolve.",
			},
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "The ACM environment id (integer, stored as string).",
			},
			"normalized_name": schema.StringAttribute{
				Computed:    true,
				Description: "The environment's normalized name.",
			},
			"display_name": schema.StringAttribute{
				Computed:    true,
				Description: "The environment's display name.",
			},
			"type": schema.StringAttribute{
				Computed:    true,
				Description: "The environment type (e.g. kubernetes).",
			},
			"domain": schema.StringAttribute{
				Computed:    true,
				Description: "The environment domain.",
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "The environment status (e.g. online).",
			},
			"state": schema.StringAttribute{
				Computed:    true,
				Description: "The environment state (e.g. running).",
			},
			"parent_id": schema.StringAttribute{
				Computed:    true,
				Description: "The ACM id of the parent environment (integer, stored as string). `null` when there is no parent.",
			},
			"owner_id": schema.StringAttribute{
				Computed:    true,
				Description: "The ACM id of the environment owner (integer, stored as string). `null` if ACM does not return an owner.",
			},
		},
	}
}

func (d *environmentDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return // provider Configure not yet called
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

func (d *environmentDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg environmentDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	env, err := d.client.GetEnvironmentByName(ctx, cfg.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Failed to read environment", dataSourceErrorDetail("GetEnvironmentByName", err))
		return
	}

	cfg.ID = types.StringValue(strconv.FormatInt(env.ID, 10))
	cfg.Name = types.StringValue(env.Name)
	cfg.NormalizedName = types.StringValue(env.NormalizedName)
	cfg.DisplayName = types.StringValue(env.DisplayName)
	cfg.Type = types.StringValue(env.Type)
	cfg.Domain = types.StringValue(env.Domain)
	cfg.Status = types.StringValue(env.Status)
	cfg.State = types.StringValue(env.State)
	// Distinguish "no parent" (null) from "parent id is empty string" (impossible
	// for ACM, but Terraform users routinely check `== null` rather than `== ""`).
	// Same null semantics applied to owner_id for symmetry — ACM should always
	// populate it, but a zero echo would otherwise leak `"0"` into state.
	if env.IDParent != 0 {
		cfg.ParentID = types.StringValue(strconv.FormatInt(env.IDParent, 10))
	} else {
		cfg.ParentID = types.StringNull()
	}
	if env.IDOwner != 0 {
		cfg.OwnerID = types.StringValue(strconv.FormatInt(env.IDOwner, 10))
	} else {
		cfg.OwnerID = types.StringNull()
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
