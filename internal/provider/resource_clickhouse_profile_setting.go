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
	_ resource.Resource                = (*profileSettingResource)(nil)
	_ resource.ResourceWithConfigure   = (*profileSettingResource)(nil)
	_ resource.ResourceWithImportState = (*profileSettingResource)(nil)
)

// profileSettingResource manages a single setting attached to a settings
// profile (altinity_clickhouse_profile_setting) via /profile/{profile}/settings
// (GET/POST) and /setting/{id} (POST/DELETE).
//
// Why this resource exists separately from altinity_clickhouse_cluster_setting:
// settings can be cluster-scoped (push to system_settings, applied to ALL
// users) or profile-scoped (push to settings_profiles, applied only to users
// referencing the profile). The ACM endpoints, ids, and resource lifecycles
// are distinct.
//
// Critically, attaching at least one setting to a profile is REQUIRED for ACM
// to actually push the profile to ClickHouse's user_directories — without
// any settings, the profile is metadata-only in ACM and any user referencing
// it fails with Code 180 THERE_IS_NO_PROFILE. So this resource is also the
// way to make altinity_clickhouse_profile actually function.
//
// Identity (design §5.1): keyed by profile_id + name; the Terraform ID is the
// composite "<profile_id>:<name>". Read matches the parent collection by name.
type profileSettingResource struct {
	client *acm.Client
}

// NewProfileSettingResource is the constructor registered with the provider.
func NewProfileSettingResource() resource.Resource {
	return &profileSettingResource{}
}

// profileSettingResourceModel maps the altinity_clickhouse_profile_setting schema.
type profileSettingResourceModel struct {
	ID        types.String `tfsdk:"id"`
	ProfileID types.String `tfsdk:"profile_id"`
	Name      types.String `tfsdk:"name"`
	Value     types.String `tfsdk:"value"`
	SettingID types.String `tfsdk:"setting_id"`
}

func (r *profileSettingResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_clickhouse_profile_setting"
}

func (r *profileSettingResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manage a single setting attached to a ClickHouse settings " +
			"profile via /profile/{profile}/settings (GET/POST). Keyed by " +
			"profile_id + name. Attaching at least one setting to a profile is " +
			"REQUIRED for ACM to push the profile to ClickHouse's user " +
			"directories; an empty profile cannot be referenced by users.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: `Composite resource id "<profile_id>:<name>".`,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"profile_id": schema.StringAttribute{
				Required: true,
				Description: "The ACM profile id (integer, stored as string) that owns " +
					"this setting. Typically a reference to " +
					"`altinity_clickhouse_profile.<name>.profile_id`. Changing this " +
					"forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				Description: "The setting name (the config-stable key, e.g. `readonly`, " +
					"`max_memory_usage`). Changing this forces a new resource.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{noColonValidator{}},
			},
			"value": schema.StringAttribute{
				Required:    true,
				Description: "The setting value, as a string.",
			},
			"setting_id": schema.StringAttribute{
				Computed: true,
				Description: "The ACM-internal profile-setting id (integer, stored as " +
					"string). Populated automatically — on Create from the POST response, " +
					"and on import via the first plan/refresh. Do not set manually.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *profileSettingResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
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

func (r *profileSettingResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan profileSettingResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	profileID, err := parseACMID("profile_id", plan.ProfileID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("profile_id"), "Invalid profile_id", err.Error())
		return
	}

	apiReq := acm.ProfileSettingRequest{Name: plan.Name.ValueString(), Value: plan.Value.ValueString()}

	// Idempotent adopt-by-name wrapped in transient-Create-race retry. Settings
	// attached to a freshly-created profile race the same ACM operator-push
	// window as users/profiles — Code 62, 180, 192, 511, id=0 half-commits,
	// and the bare-bool data envelope all surface here too.
	var created acm.ProfileSetting
	err = acm.RetryOnTransientCreateRace(ctx, func() error {
		existing, found, lerr := r.client.FindProfileSettingByName(ctx, profileID, plan.Name.ValueString())
		if lerr != nil {
			return lerr
		}
		if found {
			created = existing
			if plan.Value.ValueString() != existing.Value {
				c, eerr := r.client.EditProfileSetting(ctx, existing.ID, apiReq)
				if eerr != nil {
					return eerr
				}
				created = c
			}
			return nil
		}
		c, cerr := r.client.CreateProfileSetting(ctx, profileID, apiReq)
		if cerr != nil {
			return cerr
		}
		created = c
		return nil
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to create profile setting", err.Error())
		return
	}

	plan.Name = types.StringValue(created.Name)
	plan.Value = types.StringValue(created.Value)
	plan.SettingID = types.StringValue(strconv.FormatInt(created.ID, 10))
	plan.ID = types.StringValue(profileSettingCompositeID(plan.ProfileID.ValueString(), created.Name))

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *profileSettingResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state profileSettingResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	profileID, err := parseACMID("profile_id", state.ProfileID.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("profile_id"), "Invalid profile_id", err.Error())
		return
	}

	settings, err := r.client.ListProfileSettings(ctx, profileID)
	if err != nil {
		// 404 / 403 — parent profile gone (its parent cluster was deleted).
		// See resource_clickhouse_setting.go for the IsForbidden rationale.
		if acm.IsNotFound(err) || acm.IsForbidden(err) {
			tflog.Warn(ctx, "altinity_clickhouse_profile_setting: parent profile not accessible; removing from state (drift)",
				map[string]any{"profile_id": state.ProfileID.ValueString(), "error": err.Error()})
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read profile settings", err.Error())
		return
	}

	wantName := state.Name.ValueString()
	var found *acm.ProfileSetting
	for i := range settings {
		if settings[i].Name == wantName {
			found = &settings[i]
			break
		}
	}
	if found == nil {
		resp.State.RemoveResource(ctx)
		return
	}

	state.Name = types.StringValue(found.Name)
	state.Value = types.StringValue(found.Value)
	state.SettingID = types.StringValue(strconv.FormatInt(found.ID, 10))
	state.ID = types.StringValue(profileSettingCompositeID(state.ProfileID.ValueString(), found.Name))

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *profileSettingResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state profileSettingResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// profile_id and name are RequiresReplace, so only value can change here.
	// Edit by the ACM-internal id (POST /setting/{id}); the id is carried
	// in computed state (setting_id).
	settingID, err := parseACMID("setting_id", state.SettingID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid setting_id in state", err.Error())
		return
	}

	if _, err := r.client.EditProfileSetting(ctx, settingID, acm.ProfileSettingRequest{
		Name:  plan.Name.ValueString(),
		Value: plan.Value.ValueString(),
	}); err != nil {
		resp.Diagnostics.AddError("Failed to update profile setting", err.Error())
		return
	}

	plan.SettingID = state.SettingID
	plan.ID = types.StringValue(profileSettingCompositeID(plan.ProfileID.ValueString(), plan.Name.ValueString()))

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *profileSettingResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state profileSettingResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	settingID, err := parseACMID("setting_id", state.SettingID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid setting_id in state", err.Error())
		return
	}
	if err := r.client.DeleteProfileSetting(ctx, settingID); err != nil {
		if acm.IsNotFound(err) {
			return // already gone
		}
		resp.Diagnostics.AddError("Failed to delete profile setting", err.Error())
		return
	}
}

func (r *profileSettingResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// ID format is the composite "<profile_id>:<name>". noColonValidator forbids
	// ':' in the name, so first-vs-last index is equivalent; use LastIndex
	// defensively in case the validator is relaxed in the future.
	profileID, name, err := profileSettingSplitCompositeID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("Expected import ID in the form \"<profile_id>:<name>\", got %q.", req.ID),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("profile_id"), profileID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}

func profileSettingCompositeID(profileID, name string) string {
	return profileID + ":" + name
}

func profileSettingSplitCompositeID(id string) (profileID, name string, err error) {
	return splitCompositeID(id, true)
}
