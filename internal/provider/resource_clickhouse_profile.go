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
	_ resource.Resource                = (*profileResource)(nil)
	_ resource.ResourceWithConfigure   = (*profileResource)(nil)
	_ resource.ResourceWithImportState = (*profileResource)(nil)
)

// profileResource manages a single ClickHouse settings profile on a cluster
// (altinity_clickhouse_profile). It owns exactly one profile via
// /cluster/{cluster}/profiles (GET list / POST create); see design §5 for the
// ownership boundary against the cluster and other satellite resources.
//
// ACM exposes list + create (/cluster/{cluster}/profiles) plus per-profile
// edit (POST /profile/{id}) and delete (DELETE /profile/{id}). Create is
// idempotent (adopt-by-name) because ACM enforces no name uniqueness;
// description is updatable in place; cluster_id and name remain RequiresReplace
// (no reparent, and name is the config-stable identity key, §5.1).
type profileResource struct {
	client *acm.Client
}

// NewProfileResource is the constructor registered with the provider
// (altinity_clickhouse_profile).
func NewProfileResource() resource.Resource {
	return &profileResource{}
}

// profileResourceModel maps the altinity_clickhouse_profile schema.
//
// Identity (design §5.1): ID is the composite "<cluster_id>:<name>". cluster_id
// + name form the config-stable key; profile_id carries the ACM-internal id
// resolved by matching the parent collection by name on Read.
type profileResourceModel struct {
	ID          types.String `tfsdk:"id"`
	ClusterID   types.String `tfsdk:"cluster_id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	ProfileID   types.String `tfsdk:"profile_id"`
}

func (r *profileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_clickhouse_profile"
}

func (r *profileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manage a single ClickHouse settings profile on an Altinity.Cloud cluster (/cluster/{cluster}/profiles).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Composite identifier `<cluster_id>:<name>`.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"cluster_id": schema.StringAttribute{
				Required:    true,
				Description: "The ACM cluster id (integer, stored as string) that owns this profile.",
				PlanModifiers: []planmodifier.String{
					// The profile lives under a specific cluster; it cannot be
					// re-parented in place. (design §7.2 conservative posture.)
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required: true,
				Description: "The settings profile name (the config-stable key). " +
					"AVOID `default` and `readonly` — ACM auto-creates and auto-maintains " +
					"profiles with those names at cluster launch; managing them from " +
					"Terraform produces opaque ACM errors as ACM and Terraform fight over " +
					"the same row.",
				PlanModifiers: []planmodifier.String{
					// No rename endpoint; a changed name is a new profile.
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{
					noColonValidator{},
					reservedProfileNameValidator{},
				},
			},
			"description": schema.StringAttribute{
				Optional:    true,
				Description: "Optional human-readable description for the profile. Updatable in place via POST /profile/{id}.",
			},
			"profile_id": schema.StringAttribute{
				Computed:    true,
				Description: "The ACM-internal profile id (integer, stored as string).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *profileResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *profileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan profileResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	clusterID, err := parseACMID("cluster_id", plan.ClusterID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid cluster_id", err.Error())
		return
	}

	req2 := acm.ProfileRequest{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
	}

	// Idempotent adopt-by-name wrapped in transient-SQL retry. Two failure
	// modes both resolve by re-running find-or-create:
	//   1. ACM half-commits the profile (DB row written, response carries
	//      id=0). CreateProfile's guard turns this into an error tagged with
	//      "id=0"; on retry the just-committed orphan is picked up by
	//      FindProfileByName and we adopt it.
	//   2. ACM's auto-bootstrap of `default`/`readonly` profiles races our
	//      Create — fresh cluster has no profile, then ACM creates one with
	//      the same name we want. Retry's FindProfileByName picks up the
	//      bootstrap one and we adopt it instead of duplicating.
	var created acm.Profile
	err = acm.RetryOnTransientCreateRace(ctx, func() error {
		existing, found, lerr := r.client.FindProfileByName(ctx, clusterID, plan.Name.ValueString())
		if lerr != nil {
			return lerr
		}
		if found {
			created = existing
			if !plan.Description.IsNull() && plan.Description.ValueString() != existing.Description {
				c, eerr := r.client.EditProfile(ctx, existing.ID, req2)
				if eerr != nil {
					return eerr
				}
				created = c
			}
			return nil
		}
		c, cerr := r.client.CreateProfile(ctx, clusterID, req2)
		if cerr != nil {
			return cerr
		}
		created = c
		return nil
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to create profile", err.Error())
		return
	}

	plan.ProfileID = types.StringValue(strconv.FormatInt(created.ID, 10))
	plan.ID = types.StringValue(profileCompositeID(plan.ClusterID.ValueString(), plan.Name.ValueString()))
	// The create response is authoritative for description if returned.
	if created.Description != "" {
		plan.Description = types.StringValue(created.Description)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *profileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state profileResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	clusterID, err := parseACMID("cluster_id", state.ClusterID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid cluster_id", err.Error())
		return
	}

	profiles, err := r.client.ListProfiles(ctx, clusterID)
	if err != nil {
		if acm.IsNotFound(err) || acm.IsForbidden(err) {
			// 404 — canonical drift. 403 — ACM's actual response for a list
			// endpoint under a deleted cluster ("Access denied" instead of
			// "Not found"); treat as drift too. A genuine token failure
			// (401) is a different beast and surfaces via the AddError
			// below.
			tflog.Warn(ctx, "altinity_clickhouse_profile: parent cluster not accessible; removing from state (drift)",
				map[string]any{"cluster_id": state.ClusterID.ValueString(), "error": err.Error()})
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read profiles", err.Error())
		return
	}

	// Match the parent collection by name (the config-stable key, §5.1).
	name := state.Name.ValueString()
	var found *acm.Profile
	for i := range profiles {
		if profiles[i].Name == name {
			found = &profiles[i]
			break
		}
	}
	if found == nil {
		// Profile no longer present in the cluster → removed out of band.
		resp.State.RemoveResource(ctx)
		return
	}

	state.ProfileID = types.StringValue(strconv.FormatInt(found.ID, 10))
	state.Name = types.StringValue(found.Name)
	state.ID = types.StringValue(profileCompositeID(state.ClusterID.ValueString(), found.Name))
	// Description is only updated from the API when ACM actually returns one;
	// otherwise the prior config/state value is preserved (avoids spurious
	// drift when the API omits an empty description).
	if found.Description != "" {
		state.Description = types.StringValue(found.Description)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update applies an in-place description change via POST /profile/{id}
// (cluster_id and name are RequiresReplace, so only description can change
// here). The ACM-internal id is carried in state (profile_id).
func (r *profileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan profileResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var state profileResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	profileID, err := parseACMID("profile_id", state.ProfileID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid profile_id", err.Error())
		return
	}

	updated, err := r.client.EditProfile(ctx, profileID, acm.ProfileRequest{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to update profile", err.Error())
		return
	}

	plan.ProfileID = state.ProfileID
	plan.ID = types.StringValue(profileCompositeID(plan.ClusterID.ValueString(), plan.Name.ValueString()))
	if updated.Description != "" {
		plan.Description = types.StringValue(updated.Description)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete removes the profile via DELETE /profile/{id}. A 404 means it is
// already gone (treated as success); the framework removes it from state.
func (r *profileResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state profileResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	profileID, err := parseACMID("profile_id", state.ProfileID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid profile_id", err.Error())
		return
	}

	if err := r.client.DeleteProfile(ctx, profileID); err != nil {
		if acm.IsNotFound(err) {
			return // already gone
		}
		resp.Diagnostics.AddError("Failed to delete profile", err.Error())
		return
	}
}

// ImportState parses the composite "<cluster_id>:<name>" id. The splitter uses
// LastIndex defensively in case stricter validators are relaxed in the future
// (today noColonValidator forbids ':' in the name, so first vs last is
// equivalent).
func (r *profileResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	clusterID, name, err := splitProfileCompositeID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("cluster_id"), clusterID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), profileCompositeID(clusterID, name))...)
}

// profileCompositeID builds the "<cluster_id>:<name>" resource id.
func profileCompositeID(clusterID, name string) string {
	return clusterID + ":" + name
}

// splitProfileCompositeID splits "<cluster_id>:<name>"; uses LastIndex
// defensively in case stricter validators are relaxed in the future (today
// noColonValidator forbids ':' in the name, so first vs last is equivalent).
func splitProfileCompositeID(id string) (clusterID, name string, err error) {
	clusterID, name, err = splitCompositeID(id, true)
	if err != nil {
		return "", "", fmt.Errorf("expected import ID in the form <cluster_id>:<name>, got %q", id)
	}
	return clusterID, name, nil
}
