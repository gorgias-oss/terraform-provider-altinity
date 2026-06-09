// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

// Ensure the resource satisfies the framework interfaces.
var (
	_ resource.Resource                = (*settingResource)(nil)
	_ resource.ResourceWithConfigure   = (*settingResource)(nil)
	_ resource.ResourceWithImportState = (*settingResource)(nil)
)

// settingResource manages a single cluster-level ClickHouse setting
// (altinity_clickhouse_cluster_setting) via /cluster/{cluster}/settings (GET/POST).
//
// Identity (design §5.1): the resource is keyed by cluster_id + name; the
// Terraform ID is the composite "<cluster_id>:<name>". Read fetches the parent
// collection and matches by name (the config-stable key), carrying the
// ACM-internal setting id in computed state for subsequent operations.
type settingResource struct {
	client *acm.Client
}

// NewSettingResource is the constructor registered with the provider.
func NewSettingResource() resource.Resource {
	return &settingResource{}
}

// settingResourceModel maps the altinity_clickhouse_cluster_setting schema.
type settingResourceModel struct {
	ID        types.String `tfsdk:"id"`
	ClusterID types.String `tfsdk:"cluster_id"`
	Name      types.String `tfsdk:"name"`
	Value     types.String `tfsdk:"value"`
	SettingID types.String `tfsdk:"setting_id"`
}

func (r *settingResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_clickhouse_cluster_setting"
}

func (r *settingResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manage a single cluster-level ClickHouse setting via " +
			"/cluster/{cluster}/settings (GET/POST). Keyed by cluster_id + name.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: `Composite resource id "<cluster_id>:<name>".`,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"cluster_id": schema.StringAttribute{
				Required: true,
				Description: "The ACM cluster id (integer, stored as string) that owns " +
					"this setting. Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				Description: "The setting name (the config-stable key). Changing this " +
					"forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{noColonValidator{}},
			},
			"value": schema.StringAttribute{
				Required:    true,
				Description: "The setting value.",
			},
			"setting_id": schema.StringAttribute{
				Computed: true,
				Description: "The ACM-internal setting id (integer, stored as string). " +
					"Populated automatically — on Create from the POST response, and on " +
					"import via the first plan/refresh. Do not set manually.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *settingResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return // provider Configure not yet called
	}
	client, ok := req.ProviderData.(*acm.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *acm.Client, got %T. This is a provider bug.", req.ProviderData),
		)
		return
	}
	r.client = client
}

func (r *settingResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan settingResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	clusterID, err := parseACMID("cluster_id", plan.ClusterID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("cluster_id"), "Invalid cluster_id", err.Error())
		return
	}

	req2 := acm.SettingRequest{Name: plan.Name.ValueString(), Value: plan.Value.ValueString()}

	// Idempotent adopt-by-name wrapped in transient-Create-race retry. Two
	// failure modes both resolve by re-running the find-or-create:
	//   1. ACM's operator is still propagating fresh-cluster state to ClickHouse
	//      (Code 62 / 180 / 192 / 511 / `{"data": false}` envelope).
	//   2. A prior failed Create left an orphan row (id=0); the retry's
	//      FindSettingByName picks it up and we adopt instead of duplicating.
	var created acm.Setting
	err = acm.RetryOnTransientCreateRace(ctx, func() error {
		existing, found, lerr := r.client.FindSettingByName(ctx, clusterID, plan.Name.ValueString())
		if lerr != nil {
			return lerr
		}
		if found {
			created = existing
			if plan.Value.ValueString() != existing.Value {
				c, eerr := r.client.EditSetting(ctx, existing.ID, req2)
				if eerr != nil {
					return eerr
				}
				created = c
			}
			return nil
		}
		c, cerr := r.client.CreateSetting(ctx, clusterID, req2)
		if cerr != nil {
			return cerr
		}
		created = c
		return nil
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to create setting", err.Error())
		return
	}

	plan.Name = types.StringValue(created.Name)
	plan.Value = types.StringValue(created.Value)
	plan.SettingID = types.StringValue(strconv.FormatInt(created.ID, 10))
	plan.ID = types.StringValue(settingCompositeID(plan.ClusterID.ValueString(), created.Name))

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *settingResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state settingResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	clusterID, err := parseACMID("cluster_id", state.ClusterID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("cluster_id"), "Invalid cluster_id", err.Error())
		return
	}

	settings, err := r.client.ListSettings(ctx, clusterID)
	if err != nil {
		// 404 is the canonical drift signal. 403 is ACM's actual response
		// for ClusterSettingList against a deleted cluster ("Access denied"
		// instead of "Not found") — treat as drift too. A genuine token
		// failure (401) is a different beast and surfaces via AddError.
		if acm.IsNotFound(err) || acm.IsForbidden(err) {
			tflog.Warn(ctx, "altinity_clickhouse_cluster_setting: parent cluster not accessible; removing from state (drift)",
				map[string]any{"cluster_id": state.ClusterID.ValueString(), "error": err.Error()})
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read settings", err.Error())
		return
	}

	wantName := state.Name.ValueString()
	var found *acm.Setting
	for i := range settings {
		if settings[i].Name == wantName {
			found = &settings[i]
			break
		}
	}
	if found == nil {
		// The setting no longer exists in the parent collection — drift removal.
		resp.State.RemoveResource(ctx)
		return
	}

	state.Name = types.StringValue(found.Name)
	state.Value = types.StringValue(found.Value)
	state.SettingID = types.StringValue(strconv.FormatInt(found.ID, 10))
	state.ID = types.StringValue(settingCompositeID(state.ClusterID.ValueString(), found.Name))

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *settingResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state settingResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// cluster_id and name are RequiresReplace, so only value can change here.
	// Edit by the ACM-internal id (POST /cluster-setting/{id}); the id is carried
	// in computed state (setting_id).
	settingID, err := parseACMID("setting_id", state.SettingID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid setting_id in state", err.Error())
		return
	}

	if _, err := r.client.EditSetting(ctx, settingID, acm.SettingRequest{
		Name:  plan.Name.ValueString(),
		Value: plan.Value.ValueString(),
	}); err != nil {
		resp.Diagnostics.AddError("Failed to update setting", err.Error())
		return
	}

	// The edit response can be sparse; the configured name/value are
	// authoritative and setting_id is stable, so keep them rather than trusting
	// the response.
	plan.SettingID = state.SettingID
	plan.ID = types.StringValue(settingCompositeID(plan.ClusterID.ValueString(), plan.Name.ValueString()))

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *settingResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state settingResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Remove the setting from the cluster via DELETE /cluster-setting/{id}
	// (ClusterSettingRemove). A 404 means it is already gone (treated as success).
	settingID, err := parseACMID("setting_id", state.SettingID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid setting_id in state", err.Error())
		return
	}
	if err := r.client.DeleteSetting(ctx, settingID); err != nil {
		if acm.IsNotFound(err) {
			return // already gone
		}
		resp.Diagnostics.AddError("Failed to delete setting", err.Error())
		return
	}
}

func (r *settingResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// ID format is the composite "<cluster_id>:<name>"; the splitter uses
	// LastIndex defensively in case stricter validators are relaxed in the
	// future (today noColonValidator forbids ':' in the name, so first vs last
	// is equivalent).
	clusterID, name, err := settingSplitCompositeID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("Expected import ID in the form \"<cluster_id>:<name>\", got %q.", req.ID),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("cluster_id"), clusterID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

// settingCompositeID builds the "<cluster_id>:<name>" Terraform ID.
func settingCompositeID(clusterID, name string) string {
	return clusterID + ":" + name
}

// settingSplitCompositeID splits "<cluster_id>:<name>"; uses LastIndex
// defensively in case stricter validators are relaxed in the future (today
// noColonValidator forbids ':' in the name, so first vs last is equivalent).
func settingSplitCompositeID(id string) (clusterID, name string, err error) {
	return splitCompositeID(id, true)
}
