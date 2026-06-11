// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

var (
	_ resource.Resource                = (*environmentResource)(nil)
	_ resource.ResourceWithConfigure   = (*environmentResource)(nil)
	_ resource.ResourceWithImportState = (*environmentResource)(nil)
)

// Environment lifecycle timeout defaults. Provisioning a cloud Kubernetes
// environment (control plane + node groups + LB + DNS) realistically runs
// 10-30+ min; the create default is generous because the resumable Create
// (see below) recovers from an under-shoot on the next apply rather than
// destroying the in-flight environment.
const (
	envDefaultCreateTimeout = 45 * time.Minute
	envDefaultUpdateTimeout = 10 * time.Minute
	envDefaultDeleteTimeout = 30 * time.Minute
)

func envTimeoutDefaults() resolvedTimeouts {
	return resolvedTimeouts{
		create: envDefaultCreateTimeout,
		update: envDefaultUpdateTimeout,
		delete: envDefaultDeleteTimeout,
	}
}

// environmentResource is altinity_environment: an Altinity-hosted, region-scoped
// unit that ClickHouse clusters are launched into. Created via the request flow
// (POST /environments/request) and polled to ready.
type environmentResource struct {
	client *acm.Client
}

// NewEnvironmentResource is the constructor registered with the provider.
func NewEnvironmentResource() resource.Resource {
	return &environmentResource{}
}

type environmentResourceModel struct {
	ID                 types.String  `tfsdk:"id"`
	Name               types.String  `tfsdk:"name"`
	CloudProvider      types.String  `tfsdk:"cloud_provider"`
	Region             types.String  `tfsdk:"region"`
	DisplayName        types.String  `tfsdk:"display_name"`
	NormalizedName     types.String  `tfsdk:"normalized_name"`
	Type               types.String  `tfsdk:"type"`
	Domain             types.String  `tfsdk:"domain"`
	Status             types.String  `tfsdk:"status"`
	State              types.String  `tfsdk:"state"`
	Datadog            *datadogModel `tfsdk:"datadog"`
	MaintenanceWindows types.List    `tfsdk:"maintenance_windows"`
	Timeouts           types.Object  `tfsdk:"timeouts"`
}

// datadogModel maps the optional `datadog {}` block. api_key is write-only
// (sent, never read back); apply_to_clusters is a write-side directive (not
// echoed by GET). enabled/region/send_* are reconciled from EnvironmentShow.
type datadogModel struct {
	Enabled         types.Bool   `tfsdk:"enabled"`
	APIKey          types.String `tfsdk:"api_key"`
	Region          types.String `tfsdk:"region"`
	SendMetrics     types.Bool   `tfsdk:"send_metrics"`
	SendLogs        types.Bool   `tfsdk:"send_logs"`
	SendTableStats  types.Bool   `tfsdk:"send_table_stats"`
	ApplyToClusters types.Bool   `tfsdk:"apply_to_clusters"`
}

// maintenanceWindowModel maps one `maintenance_windows` entry.
type maintenanceWindowModel struct {
	Name        types.String `tfsdk:"name"`
	Enabled     types.Bool   `tfsdk:"enabled"`
	Hour        types.Int64  `tfsdk:"hour"`
	LengthHours types.Int64  `tfsdk:"length_hours"`
	Days        types.List   `tfsdk:"days"`
}

// maintenanceWindowAttrTypes is the object type of a maintenance_windows element.
func maintenanceWindowAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"name":         types.StringType,
		"enabled":      types.BoolType,
		"hour":         types.Int64Type,
		"length_hours": types.Int64Type,
		"days":         types.ListType{ElemType: types.StringType},
	}
}

func (r *environmentResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_environment"
}

func (r *environmentResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "An Altinity.Cloud environment — the region-scoped unit that ClickHouse " +
			"clusters are launched into. Created via the Altinity-hosted request flow " +
			"(POST /environments/request) and polled until ready.\n\n" +
			"Create is RESUMABLE: if the readiness poll exceeds the create timeout, the apply " +
			"fails WITHOUT recording state, and a subsequent apply adopts the still-provisioning " +
			"environment by name and resumes waiting — it is never destroyed and re-requested. " +
			"As a consequence, an unmanaged environment with the same `name` would be adopted.\n\n" +
			"Destroy does NOT delete the environment in Altinity.Cloud: environment deletion requires " +
			"an out-of-band email + MFA confirmation that cannot be automated, so `terraform destroy` " +
			"removes the resource from state and warns — delete the environment manually in the ACM UI.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "The ACM environment id (integer, stored as string).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Environment name (unique within the organization). Used as the adopt-by-name key for resumable create.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"cloud_provider": schema.StringAttribute{
				Required:    true,
				Description: "Cloud provider to provision into: aws, gcp, azure, or hcloud.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{cloudProviderValidator{}},
			},
			"region": schema.StringAttribute{
				Required: true,
				Description: "Region code to provision into (see the altinity_regions data source). " +
					"Routed to the provider-specific request field based on cloud_provider.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"display_name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "Human-readable display name. The request endpoint cannot set it, so when " +
					"specified the provider applies it via an edit immediately after the environment is ready. " +
					"Defaults to the environment name.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"normalized_name": schema.StringAttribute{
				Computed:    true,
				Description: "ACM's normalized form of the name.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"type": schema.StringAttribute{
				Computed:    true,
				Description: "Environment type (e.g. kubernetes).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"domain": schema.StringAttribute{
				Computed:    true,
				Description: "Environment domain.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"status": schema.StringAttribute{
				Computed:    true,
				Description: "Environment status (e.g. online once ready).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"state": schema.StringAttribute{
				Computed:    true,
				Description: "Environment state.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"datadog": schema.SingleNestedAttribute{
				Optional: true,
				Description: "Datadog integration for the environment (ship ClickHouse metrics/logs to Datadog). " +
					"Omit the block to leave Datadog unmanaged.",
				Attributes: map[string]schema.Attribute{
					"enabled": schema.BoolAttribute{
						Optional: true, Computed: true,
						Description:   "Whether the Datadog integration is enabled.",
						PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
					},
					"api_key": schema.StringAttribute{
						Optional:  true,
						Sensitive: true,
						Description: "Datadog API key. Write-only: sent on apply, never read back from the API, " +
							"and excluded from drift detection (an out-of-band change is not noticed).",
					},
					"region": schema.StringAttribute{
						Optional: true, Computed: true,
						Description:   "Datadog site, e.g. datadoghq.com (default) or datadoghq.eu.",
						PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
					},
					"send_metrics": schema.BoolAttribute{
						Optional: true, Computed: true,
						PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
					},
					"send_logs": schema.BoolAttribute{
						Optional: true, Computed: true,
						PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
					},
					"send_table_stats": schema.BoolAttribute{
						Optional: true, Computed: true,
						PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
					},
					"apply_to_clusters": schema.BoolAttribute{
						Optional: true, Computed: true,
						Description:   "Push the Datadog config to the environment's clusters (applyToClusters). Defaults to true.",
						PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
					},
				},
			},
			"maintenance_windows": schema.ListNestedAttribute{
				Optional: true,
				Description: "Maintenance windows for the environment. ACM requires the windows to provide " +
					"at least 48h over any 32-day window (rejected server-side otherwise). Omit (null) to leave " +
					"unmanaged; set `[]` to clear all windows. Not reconciled against the API on read.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name":         schema.StringAttribute{Required: true},
						"enabled":      schema.BoolAttribute{Required: true},
						"hour":         schema.Int64Attribute{Required: true, Description: "Start hour, 0–23 (UTC)."},
						"length_hours": schema.Int64Attribute{Required: true, Description: "Window length in hours."},
						"days": schema.ListAttribute{
							ElementType: types.StringType,
							Required:    true,
							Description: "Weekdays (uppercase): MONDAY…SUNDAY.",
							Validators:  []validator.List{weekdayListValidator{}},
						},
					},
				},
			},
			"timeouts": schema.SingleNestedAttribute{
				Optional:    true,
				Description: "Operation timeouts (Go duration strings). create defaults to 45m, delete to 30m.",
				Attributes: map[string]schema.Attribute{
					"create": schema.StringAttribute{Optional: true},
					"update": schema.StringAttribute{Optional: true},
					"delete": schema.StringAttribute{Optional: true},
				},
			},
		},
	}
}

func (r *environmentResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// buildEnvRequest maps the model's cloud_provider + region onto the
// EnvironmentRequest. Two ACM quirks are handled here (both live-confirmed
// 2026-06-09 against the ACM UI's request-environment call):
//
//   - cloud_provider must be UPPERCASE on this endpoint ("GCP", not "gcp") — even
//     though the altinity_regions endpoint accepts the lowercase form. The schema
//     keeps the operator-facing value lowercase (consistent with altinity_regions)
//     and we upper-case it on the wire. ToUpper("gcp")="GCP" etc.
//   - the region must be placed in the field MATCHING cloud_provider; a region in
//     a non-matching field is rejected with HTTP 400 "fields invalid: cloud_provider".
func (r *environmentResource) buildEnvRequest(m environmentResourceModel) acm.EnvironmentRequest {
	provider := m.CloudProvider.ValueString()
	req := acm.EnvironmentRequest{
		Name:          m.Name.ValueString(),
		CloudProvider: strings.ToUpper(provider),
	}
	region := m.Region.ValueString()
	switch provider {
	case "aws":
		req.AWSRegion = region
	case "gcp":
		req.GCPRegion = region
	case "azure":
		req.AzureRegion = region
	case "hcloud":
		req.HcloudRegion = region
	}
	return req
}

// applyEnvironmentToModel writes the API's computed fields onto the model. It
// deliberately does NOT touch name/cloud_provider/region (RequiresReplace,
// operator-owned, and not echoed back in a directly-mappable form) — those are
// preserved from plan/state. The datadog block is reconciled in-place for its
// non-secret fields; `api_key`/`apply_to_clusters` and `maintenance_windows`
// are operator/write-owned and left as the caller's model already holds them.
func applyEnvironmentToModel(m *environmentResourceModel, e acm.Environment) {
	m.ID = types.StringValue(strconv.FormatInt(e.ID, 10))
	m.Name = types.StringValue(e.Name)
	m.DisplayName = types.StringValue(e.DisplayName)
	m.NormalizedName = types.StringValue(e.NormalizedName)
	m.Type = types.StringValue(e.Type)
	m.Domain = types.StringValue(e.Domain)
	m.Status = types.StringValue(e.Status)
	m.State = types.StringValue(e.State)

	// Datadog: reconcile the non-secret Computed fields from the API only when the
	// operator manages the block (m.Datadog != nil). Every Computed sub-field must
	// end up KNOWN (else "provider produced inconsistent result"): use the API
	// value, falling back to a default when the GET doesn't echo datadog. Preserve
	// api_key (write-only) as-is; resolve apply_to_clusters (write-side, not
	// echoed) to its effective default (true) when unset. If unmanaged, leave nil.
	if m.Datadog != nil {
		dd := e.Datadog
		if dd == nil {
			dd = &acm.DatadogConfig{}
		}
		m.Datadog.Enabled = types.BoolValue(dd.Enabled)
		m.Datadog.SendMetrics = types.BoolValue(dd.Metrics)
		m.Datadog.SendLogs = types.BoolValue(dd.Logs)
		m.Datadog.SendTableStats = types.BoolValue(dd.TableStats)
		region := dd.Region
		if region == "" {
			region = "datadoghq.com"
		}
		m.Datadog.Region = types.StringValue(region)
		if m.Datadog.ApplyToClusters.IsNull() || m.Datadog.ApplyToClusters.IsUnknown() {
			m.Datadog.ApplyToClusters = types.BoolValue(true)
		}
	}
	// maintenance_windows is config-authoritative (not reconciled from the API —
	// see OQ-4); leave m.MaintenanceWindows as the caller's model holds it.
}

func (r *environmentResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan environmentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	to, diags := resolveTimeoutsWithDefaults(ctx, plan.Timeouts, envTimeoutDefaults())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	name := plan.Name.ValueString()
	envReq := r.buildEnvRequest(plan)

	// Bound the whole create (request + poll) by the create timeout.
	opCtx, cancel := context.WithTimeout(ctx, to.create)
	defer cancel()

	// Adopt-by-name first, then request if absent — both inside one
	// RetryWhileBusy so a transient env lock retries the whole sequence. This is
	// what makes Create RESUMABLE: a prior apply that timed out mid-poll left a
	// provisioning environment in ACM; this re-entry adopts it by name and skips
	// the request entirely.
	var envID int64
	err := acm.RetryWhileBusy(opCtx, func() error {
		existing, gerr := r.client.GetEnvironmentByName(opCtx, name)
		if gerr == nil {
			envID = existing.ID
			return nil
		}
		if !acm.IsNotFound(gerr) {
			return gerr
		}
		created, cerr := r.client.RequestEnvironment(opCtx, envReq)
		if cerr != nil {
			return cerr
		}
		envID = created.ID
		return nil
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to request environment", err.Error())
		return
	}

	// Resolve the id if the request response did not carry one (OQ-1 fallback).
	if envID == 0 {
		env, gerr := r.client.GetEnvironmentByName(opCtx, name)
		if gerr != nil {
			resp.Diagnostics.AddError(
				"Environment requested but id not resolvable",
				fmt.Sprintf("ACM accepted the request for environment %q but it could not be located by name afterward: %s", name, gerr.Error()),
			)
			return
		}
		envID = env.ID
	}

	// Poll until ready. On timeout we return WITHOUT setting state so the next
	// apply adopts-by-name and resumes (the resumability contract — do NOT add a
	// resp.State.Set on this path).
	if err := acm.PollUntilHealthy(opCtx, func(c context.Context) (string, error) {
		e, gerr := r.client.GetEnvironmentByID(c, envID)
		return e.Status, gerr
	}); err != nil {
		resp.Diagnostics.AddError(
			"Environment did not become ready",
			fmt.Sprintf("Environment %q (id %d) is still provisioning: %s.\n\n"+
				"Re-apply to resume waiting on the SAME environment (it is not destroyed), "+
				"or raise the create timeout.", name, envID, err),
		)
		return
	}

	// The request endpoint can't set display_name / datadog / maintenance_windows,
	// so apply whatever the operator configured via one EnvironmentEdit follow-up
	// after the environment is ready.
	if envEditNeeded(plan) {
		editReq, d := buildEnvEditRequest(ctx, plan)
		resp.Diagnostics.Append(d...)
		if resp.Diagnostics.HasError() {
			return
		}
		if err := acm.RetryWhileBusy(opCtx, func() error {
			_, e := r.client.EditEnvironment(opCtx, envID, editReq)
			return e
		}); err != nil {
			resp.Diagnostics.AddError("Failed to apply environment configuration after create", err.Error())
			return
		}
	}

	env, gerr := r.client.GetEnvironmentByID(ctx, envID)
	if gerr != nil {
		resp.Diagnostics.AddError("Failed to read environment after create", gerr.Error())
		return
	}
	applyEnvironmentToModel(&plan, env)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *environmentResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state environmentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseACMID("id", state.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid environment id in state", err.Error())
		return
	}

	env, gerr := r.client.GetEnvironmentByID(ctx, id)
	if gerr != nil {
		if acm.IsNotFound(gerr) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read environment", gerr.Error())
		return
	}
	applyEnvironmentToModel(&state, env)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *environmentResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan environmentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseACMID("id", plan.ID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid environment id in state", err.Error())
		return
	}

	// Mutable: display_name, datadog, maintenance_windows (everything else is
	// ForceNew). One EnvironmentEdit carries whatever the operator manages.
	editReq, d := buildEnvEditRequest(ctx, plan)
	resp.Diagnostics.Append(d...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := acm.RetryWhileBusy(ctx, func() error {
		_, e := r.client.EditEnvironment(ctx, id, editReq)
		return e
	}); err != nil {
		resp.Diagnostics.AddError("Failed to update environment", err.Error())
		return
	}

	env, gerr := r.client.GetEnvironmentByID(ctx, id)
	if gerr != nil {
		resp.Diagnostics.AddError("Failed to read environment after update", gerr.Error())
		return
	}
	applyEnvironmentToModel(&plan, env)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *environmentResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state environmentResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Environment deletion is intentionally NOT automated. Live-confirmed
	// (2026-06-09): DELETE /environment/{id} does not delete synchronously — it
	// triggers an out-of-band email with an MFA confirmation link (op=deleteEnv)
	// that a human must click to actually tear the environment down. Terraform
	// cannot complete that flow, so `terraform destroy` removes the resource from
	// state and warns rather than issuing a delete that would never take effect.
	//
	// The framework removes the resource from state when Delete returns without
	// an error; we add a warning so the operator knows the environment still
	// exists in ACM and must be deleted manually.
	resp.Diagnostics.AddWarning(
		"Environment not deleted in Altinity.Cloud",
		fmt.Sprintf("Environment %q (id %s) was removed from Terraform state but NOT deleted in "+
			"Altinity.Cloud. Environment deletion requires an email + MFA confirmation that cannot be "+
			"automated by the provider. If you no longer need it, delete it manually in the ACM UI "+
			"(Environments → %s → Delete) and approve the emailed confirmation link.",
			state.Name.ValueString(), state.ID.ValueString(), state.Name.ValueString()),
	)
}

// ImportState imports by the ACM environment id.
func (r *environmentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// envEditNeeded reports whether the plan has anything the EnvironmentEdit
// follow-up should send (display_name / datadog / maintenance_windows).
func envEditNeeded(p environmentResourceModel) bool {
	if !p.DisplayName.IsNull() && !p.DisplayName.IsUnknown() && p.DisplayName.ValueString() != "" {
		return true
	}
	if p.Datadog != nil {
		return true
	}
	if !p.MaintenanceWindows.IsNull() && !p.MaintenanceWindows.IsUnknown() {
		return true
	}
	return false
}

// buildEnvEditRequest assembles the EnvironmentEdit body from the plan. Only the
// fields the operator manages are populated (all omitempty / nil-pointer), so
// the merge-patch leaves everything else untouched.
func buildEnvEditRequest(ctx context.Context, plan environmentResourceModel) (acm.EnvironmentEditRequest, diag.Diagnostics) {
	var diags diag.Diagnostics
	req := acm.EnvironmentEditRequest{DisplayName: plan.DisplayName.ValueString()}

	if dd := plan.Datadog; dd != nil {
		region := dd.Region.ValueString()
		if region == "" {
			region = "datadoghq.com"
		}
		req.DatadogSettings = &acm.DatadogSettings{
			Enabled:    dd.Enabled.ValueBool(),
			Key:        dd.APIKey.ValueString(),
			Region:     region,
			Metrics:    dd.SendMetrics.ValueBool(),
			Logs:       dd.SendLogs.ValueBool(),
			TableStats: dd.SendTableStats.ValueBool(),
		}
		// apply_to_clusters defaults to true (null/unknown → true).
		if dd.ApplyToClusters.IsNull() || dd.ApplyToClusters.IsUnknown() || dd.ApplyToClusters.ValueBool() {
			req.ApplyToClusters = json.RawMessage(`{"datadog":true}`)
		}
	}

	// null → unmanaged (leave nil). known (incl. empty []) → send a non-nil
	// pointer so an empty list marshals `[]` (clear all), distinct from omitted.
	if !plan.MaintenanceWindows.IsNull() && !plan.MaintenanceWindows.IsUnknown() {
		var models []maintenanceWindowModel
		diags.Append(plan.MaintenanceWindows.ElementsAs(ctx, &models, false)...)
		if diags.HasError() {
			return req, diags
		}
		windows := make([]acm.MaintenanceWindow, 0, len(models))
		for _, m := range models {
			var days []string
			diags.Append(m.Days.ElementsAs(ctx, &days, false)...)
			windows = append(windows, acm.MaintenanceWindow{
				Name:          m.Name.ValueString(),
				Enabled:       m.Enabled.ValueBool(),
				Hour:          int(m.Hour.ValueInt64()),
				LengthInHours: int(m.LengthHours.ValueInt64()),
				Days:          days,
			})
		}
		req.MaintenanceWindowSchedules = &windows
	}
	return req, diags
}

// weekdayListValidator rejects `days` entries that aren't uppercase weekday names.
type weekdayListValidator struct{}

func (weekdayListValidator) Description(context.Context) string {
	return "each day must be an uppercase weekday name (MONDAY…SUNDAY)"
}

func (v weekdayListValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (weekdayListValidator) ValidateList(ctx context.Context, req validator.ListRequest, resp *validator.ListResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	var days []string
	resp.Diagnostics.Append(req.ConfigValue.ElementsAs(ctx, &days, false)...)
	valid := map[string]bool{
		"MONDAY": true, "TUESDAY": true, "WEDNESDAY": true, "THURSDAY": true,
		"FRIDAY": true, "SATURDAY": true, "SUNDAY": true,
	}
	for _, d := range days {
		if !valid[d] {
			resp.Diagnostics.AddAttributeError(
				req.Path,
				"Invalid weekday",
				fmt.Sprintf("day %q must be an uppercase weekday name (MONDAY…SUNDAY)", d),
			)
		}
	}
}

// cloudProviderValidator rejects cloud_provider values ACM's request endpoint
// does not accept, surfacing the error at plan time against the attribute.
type cloudProviderValidator struct{}

func (cloudProviderValidator) Description(context.Context) string {
	return "must be one of aws, gcp, azure, hcloud"
}

func (v cloudProviderValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (cloudProviderValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	switch req.ConfigValue.ValueString() {
	case "aws", "gcp", "azure", "hcloud":
	default:
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid cloud_provider",
			fmt.Sprintf("cloud_provider must be one of aws, gcp, azure, hcloud; got %q", req.ConfigValue.ValueString()),
		)
	}
}
