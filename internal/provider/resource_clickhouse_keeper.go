// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

var (
	_ resource.Resource                = (*keeperResource)(nil)
	_ resource.ResourceWithConfigure   = (*keeperResource)(nil)
	_ resource.ResourceWithImportState = (*keeperResource)(nil)
)

// keeperResource is altinity_clickhouse_keeper: a CH Keeper coordination
// cluster that a ClickHouse cluster references by name (cluster.keeper_name) to
// satisfy ACM's "CH Keeper or Zookeeper options must be specified" requirement.
type keeperResource struct {
	client *acm.Client
}

// NewKeeperResource is the constructor registered with the provider.
func NewKeeperResource() resource.Resource {
	return &keeperResource{}
}

// keeperResourceModel maps the altinity_clickhouse_keeper schema.
type keeperResourceModel struct {
	ID              types.String `tfsdk:"id"`
	Environment     types.String `tfsdk:"environment"`
	Name            types.String `tfsdk:"name"`
	InstanceType    types.String `tfsdk:"instance_type"`
	Image           types.String `tfsdk:"image"`
	Ha              types.Bool   `tfsdk:"ha"`
	// Zones is a framework List (not a Go slice) so the framework can
	// represent an "unknown" value sourced from a data source (e.g.
	// `zones = data.altinity_zones.this.zones` behind a same-apply
	// altinity_environment). Decoding unknown into a plain []string fails
	// with "Received unknown value, however the target type cannot handle
	// unknown values." Same rationale as clusterResourceModel.Azlist.
	Zones types.List `tfsdk:"zones"`
	CPULimits       types.String `tfsdk:"cpu_limits"`
	CPURequests     types.String `tfsdk:"cpu_requests"`
	ZoneTopologyKey types.String `tfsdk:"zone_topology_key"`
	AdoptExisting   types.Bool   `tfsdk:"adopt_existing"`
	Timeouts        types.Object `tfsdk:"timeouts"`
}

func (r *keeperResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_clickhouse_keeper"
}

func (r *keeperResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A ClickHouse Keeper (CH Keeper) coordination cluster. Reference it from " +
			"an altinity_clickhouse_cluster via keeper_name.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: `Resource id, "<environment>:<name>".`,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"environment": schema.StringAttribute{
				Required:    true,
				Description: "ACM environment id the keeper lives in.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Keeper name (unique within the environment). Used as a URL path segment, so must not contain `/`, `:`, or whitespace.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{pathSafeNameValidator{}},
			},
			"instance_type": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Node instance type — a zookeeper-scoped type (e.g. e2-standard-2); see the altinity_node_types data source.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"image": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Keeper image/version.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"ha": schema.BoolAttribute{
				Computed: true,
				Description: "High availability (3-node) keeper when true; single-node otherwise. " +
					"ACM-managed: the value is determined server-side from the bound cluster's " +
					"replica count (any cluster with replicas > 1 forces HA) and reported back here. " +
					"Cannot be set in config.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"zones": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "Availability zones to spread keeper nodes across.",
			},
			"cpu_limits": schema.StringAttribute{
				Computed:    true,
				Description: "Read-back CPU limit (k8s quantity).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"cpu_requests": schema.StringAttribute{
				Computed:    true,
				Description: "Read-back CPU request (k8s quantity).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"zone_topology_key": schema.StringAttribute{
				Computed:    true,
				Description: "Read-back zone topology key.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			// ---- adoption opt-in (default false) — same invariant as the
			// cluster resource: nothing that can take destroy authority over
			// compute is adopted silently. ----
			"adopt_existing": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				Description: "If true, Create will adopt an existing keeper of the same name in the " +
					"target environment instead of erroring. Default false — a keeper created " +
					"out-of-band (or by another team using the same ACM token) will NOT be silently " +
					"placed under Terraform management. Set to true when migrating an ACM-created " +
					"keeper into IaC, or to resume a create that was interrupted after launch.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"timeouts": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "Operation timeouts (Go duration strings).",
				Attributes: map[string]schema.Attribute{
					"create": schema.StringAttribute{Optional: true},
					"update": schema.StringAttribute{Optional: true},
					"delete": schema.StringAttribute{Optional: true},
				},
			},
		},
	}
}

func (r *keeperResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// keeperCompositeID builds the resource id "<environment>:<name>".
func keeperCompositeID(environment, name string) string {
	return environment + ":" + name
}

// splitKeeperCompositeID splits "<environment>:<name>" on the FIRST colon (the
// environment id is numeric, so a colon in the name is unambiguous).
func splitKeeperCompositeID(id string) (environment, name string, err error) {
	environment, name, err = splitCompositeID(id, false)
	if err != nil {
		return "", "", fmt.Errorf("expected \"<environment>:<name>\", got %q", id)
	}
	return environment, name, nil
}

// keeperLaunchReq builds the launch body from the plan. `ha` is deliberately
// not propagated — see KeeperLaunchRequest's doc comment.
func keeperLaunchReq(m keeperResourceModel) acm.KeeperLaunchRequest {
	return acm.KeeperLaunchRequest{
		Name:         m.Name.ValueString(),
		Zones:        stringListToSlice(m.Zones),
		InstanceType: m.InstanceType.ValueString(),
		Image:        m.Image.ValueString(),
	}
}

func keeperEditReq(m keeperResourceModel) acm.KeeperEditRequest {
	return acm.KeeperEditRequest{
		Zones:        stringListToSlice(m.Zones),
		InstanceType: m.InstanceType.ValueString(),
		Image:        m.Image.ValueString(),
	}
}

// applyKeeperToModel maps an API keeper onto the model. Scalar read-back
// fields are set from the API. `zones` is preserved from the desired config
// (a plain Optional list) to avoid list-ordering churn. `ha` is ACM-managed
// (Computed-only in the schema) so we always reflect the API value — ACM
// auto-promotes keepers to HA when the bound cluster has replicas > 1.
func applyKeeperToModel(m *keeperResourceModel, k acm.Keeper) {
	m.ID = types.StringValue(keeperCompositeID(m.Environment.ValueString(), k.Name))
	m.Name = types.StringValue(k.Name)
	m.InstanceType = types.StringValue(k.InstanceType)
	m.Image = types.StringValue(k.Image)
	m.Ha = types.BoolValue(k.Ha)
	m.CPULimits = types.StringValue(k.CPULimits)
	m.CPURequests = types.StringValue(k.CPURequests)
	m.ZoneTopologyKey = types.StringValue(k.ZoneTopologyKey)
}

func (r *keeperResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan keeperResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	to, diags := resolveTimeouts(ctx, plan.Timeouts)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	env := plan.Environment.ValueString()
	name := plan.Name.ValueString()

	// Bound the whole create (launch retries + poll) by the create timeout.
	opCtx, cancel := context.WithTimeout(ctx, to.create)
	defer cancel()

	// Idempotent launch behind a single RetryWhileBusy: check for an existing
	// keeper of this name, otherwise LaunchKeeper. The find+launch pair lives in
	// one closure so a transient "operation is in progress" between the two
	// re-tries the WHOLE sequence — the previous shape could observe "not found"
	// just before the env lock was released by another op and then race the
	// launch into the same lock anyway. Keepers are name-identified; adopting
	// an already-present keeper is gated on adopt_existing (same invariant as
	// the cluster resource — silent adoption hands Terraform destroy authority
	// over something it didn't create).
	adoptExisting := plan.AdoptExisting.ValueBool()
	err := acm.RetryWhileBusy(opCtx, func() error {
		if _, found, ferr := r.client.FindKeeperInEnv(opCtx, env, name); ferr != nil {
			return ferr
		} else if found {
			if !adoptExisting {
				return fmt.Errorf("a keeper named %q already exists in environment %s; "+
					"Terraform refuses to adopt it by default. Set adopt_existing = true to take it over "+
					"(also needed to resume a create that was interrupted after launch), or delete the "+
					"existing keeper first if it is unmanaged", name, env)
			}
			return nil
		}
		return r.client.LaunchKeeper(opCtx, env, keeperLaunchReq(plan))
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to launch keeper", err.Error())
		return
	}

	if err := acm.PollUntilHealthy(opCtx, func(c context.Context) (string, error) {
		return r.client.GetKeeperStatus(c, env, name)
	}); err != nil {
		resp.Diagnostics.AddError(
			"Keeper did not become healthy",
			fmt.Sprintf("Keeper %q was launched in environment %s but polling failed: %s. "+
				"Re-apply to resume, or destroy to remove it.", name, env, err),
		)
		return
	}

	keeper, found, err := r.client.FindKeeperInEnv(ctx, env, name)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read keeper after launch", err.Error())
		return
	}
	if !found {
		resp.Diagnostics.AddError("Keeper not found after launch",
			fmt.Sprintf("Keeper %q was not present in environment %s after launch.", name, env))
		return
	}
	applyKeeperToModel(&plan, keeper)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *keeperResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state keeperResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	keeper, found, err := r.client.FindKeeperInEnv(ctx, state.Environment.ValueString(), state.Name.ValueString())
	if err != nil {
		// FindKeeperInEnv goes through the per-environment list endpoint, so the
		// failure modes are environment-scoped: 404 = environment gone, 403 =
		// "Access denied" against a deleted environment (ACM's actual response
		// for list-against-deleted-parent). Both are drift — remove from state.
		//
		// This is intentionally DIFFERENT from the cluster Read's per-id GET
		// path (resource_clickhouse_cluster.go ~line 964), which treats 403 as
		// a hard error: a 403 on a per-id GET is far more likely a real token
		// problem than a gone resource, and silently removing from state would
		// destroy work on the next apply. The list-vs-id endpoint distinction
		// is the discriminator. See the comment in cluster Read for the full
		// rationale.
		//
		// A genuine token failure (401) still surfaces via AddError below.
		if acm.IsNotFound(err) || acm.IsForbidden(err) {
			tflog.Warn(ctx, "altinity_clickhouse_keeper: parent environment not accessible; removing from state (drift)",
				map[string]any{"environment": state.Environment.ValueString(), "error": err.Error()})
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read keeper", err.Error())
		return
	}
	if !found {
		// Drift: keeper removed out-of-band — drop from state (§7.1).
		resp.State.RemoveResource(ctx)
		return
	}
	applyKeeperToModel(&state, keeper)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *keeperResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan keeperResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	to, diags := resolveTimeouts(ctx, plan.Timeouts)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	env := plan.Environment.ValueString()
	name := plan.Name.ValueString()
	if err := r.client.EditKeeper(ctx, env, name, keeperEditReq(plan)); err != nil {
		resp.Diagnostics.AddError("Failed to update keeper", err.Error())
		return
	}

	pollCtx, cancel := context.WithTimeout(ctx, to.update)
	defer cancel()
	if err := acm.PollUntilHealthy(pollCtx, func(c context.Context) (string, error) {
		return r.client.GetKeeperStatus(c, env, name)
	}); err != nil {
		resp.Diagnostics.AddError("Keeper did not become healthy after update", err.Error())
		return
	}

	keeper, found, err := r.client.FindKeeperInEnv(ctx, env, name)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read keeper after update", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	applyKeeperToModel(&plan, keeper)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *keeperResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state keeperResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	to, diags := resolveTimeouts(ctx, state.Timeouts)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	env := state.Environment.ValueString()
	name := state.Name.ValueString()

	if err := r.client.DeleteKeeper(ctx, env, name); err != nil {
		if acm.IsNotFound(err) {
			return
		}
		resp.Diagnostics.AddError("Failed to delete keeper", err.Error())
		return
	}

	// Poll until gone: keeper deletion is async; without this the per-environment
	// operation lock is still held when the next apply starts, causing mutations
	// (e.g. cluster termination) to fail with "Another operation is in progress".
	pollCtx, cancel := context.WithTimeout(ctx, to.delete)
	defer cancel()
	if err := acm.PollUntilGoneBy(pollCtx, func(c context.Context) (bool, error) {
		_, found, err := r.client.FindKeeperInEnv(c, env, name)
		return found, err
	}); err != nil {
		resp.Diagnostics.AddError("Keeper did not terminate", err.Error())
	}
}

// ImportState parses "<environment>:<name>".
func (r *keeperResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	env, name, err := splitKeeperCompositeID(strings.TrimSpace(req.ID))
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", "altinity_clickhouse_keeper import: "+err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), keeperCompositeID(env, name))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("environment"), env)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
}
