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
	_ resource.ResourceWithModifyPlan  = (*environmentResource)(nil)
)

// Environment lifecycle timeout defaults. Provisioning a cloud Kubernetes
// environment (control plane + node groups + LB + DNS) realistically runs
// 10-30+ min; the create default is generous because under-shooting it fails
// the apply mid-provisioning — the environment is left running in ACM and must
// then be imported by id (Create refuses to re-adopt it on a plain re-apply).
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
	MaintenanceWindows types.Set     `tfsdk:"maintenance_windows"`
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
	Days        types.Set    `tfsdk:"days"`
}

// maintenanceWindowAttrTypes is the object type of a maintenance_windows element.
func maintenanceWindowAttrTypes() map[string]attr.Type {
	return map[string]attr.Type{
		"name":         types.StringType,
		"enabled":      types.BoolType,
		"hour":         types.Int64Type,
		"length_hours": types.Int64Type,
		"days":         types.SetType{ElemType: types.StringType},
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
			"Create REFUSES to adopt a pre-existing environment: if an environment with the same " +
			"`name` already exists in Altinity.Cloud (names are unique per organization), the apply " +
			"fails and directs you to `terraform import` it instead — Terraform never silently takes " +
			"over infrastructure it did not create. If the readiness poll exceeds the create timeout " +
			"the apply also fails without recording state; because the environment was already " +
			"requested, re-applying hits the same guard — raise the create timeout, or import the " +
			"environment by id once it finishes provisioning.\n\n" +
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
				Description: "Environment name (unique within the organization). Must start with your " +
					"organization slug — ACM rejects other names server-side with HTTP 400 (\"Invalid Environment " +
					"Name prefix\"); the error names the exact required prefix. Create also fails if an environment " +
					"with this name already exists — `terraform import` the existing one instead.",
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
						Description: "Datadog API key. Write-only: sent on apply but never read back into Terraform " +
							"state (the API returns it, but the provider deliberately drops it to keep the secret out of " +
							"state and masks it in debug logs), and excluded from drift detection (an out-of-band change " +
							"is not noticed). On import it comes in as null and must be re-supplied in config.",
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
			"maintenance_windows": schema.SetNestedAttribute{
				Optional: true,
				Description: "Maintenance windows for the environment. ACM requires the windows to provide " +
					"at least 48h over any 32-day window (rejected server-side otherwise). Omit (null) to leave " +
					"unmanaged; set `[]` to clear all windows.\n\n" +
					"Read from the environment's acc-check endpoint (the plain environment GET returns them as " +
					"null), so when you manage the block, out-of-band changes — including deleting a window — ARE " +
					"refreshed and shown as drift. Leaving the block unset keeps it unmanaged (never probed or " +
					"populated). Modeled as a set, so neither the order of the windows nor of each window's days " +
					"matters for diffs (ACM may reorder both).",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name":         schema.StringAttribute{Required: true},
						"enabled":      schema.BoolAttribute{Required: true},
						"hour":         schema.Int64Attribute{Required: true, Description: "Start hour, 0–23 (UTC)."},
						"length_hours": schema.Int64Attribute{Required: true, Description: "Window length in hours."},
						"days": schema.SetAttribute{
							ElementType: types.StringType,
							Required:    true,
							Description: "Weekdays (uppercase): MONDAY…SUNDAY. Unordered (set).",
							Validators:  []validator.Set{weekdaySetValidator{}},
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

// ModifyPlan adds two plan-time guards. The client is nil during validation
// walks before Configure runs, so bail early in that case.
//
//   - CREATE (null prior state): look the environment up by name and, if one
//     already exists, fail the plan with an import hint — so `terraform plan`
//     shows the error instead of a misleading "+ create". Best-effort: skipped
//     when name is unknown, and a transient lookup failure degrades to a warning.
//     The Create-time guard remains the authoritative defense.
//   - REPLACE (non-null prior state, a RequiresReplace field changed): block it.
//     Destroy cannot actually delete the environment in ACM (it only drops state
//     and warns), so a destroy+create would strand the operator — the create half
//     hits the "already exists" guard and the resource is gone from state with the
//     environment still live. Fail at plan with manual-migration guidance.
func (r *environmentResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	// Destroy (null plan) or pre-Configure validation walk: nothing to guard.
	if req.Plan.Raw.IsNull() || r.client == nil {
		return
	}

	var plan environmentResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Update/replace path: prior state exists. Block a replacement of the
	// (still-live, undeletable) environment before the destroy half runs.
	if !req.State.Raw.IsNull() {
		var state environmentResourceModel
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
		for _, f := range []struct {
			attr        string
			plan, state types.String
		}{
			{"name", plan.Name, state.Name},
			{"cloud_provider", plan.CloudProvider, state.CloudProvider},
			{"region", plan.Region, state.Region},
		} {
			if f.plan.IsUnknown() || f.state.IsNull() {
				continue
			}
			if f.plan.ValueString() != f.state.ValueString() {
				resp.Diagnostics.AddAttributeError(
					path.Root(f.attr),
					"Environment cannot be replaced",
					fmt.Sprintf("Changing %s forces replacement, but Terraform cannot delete an Altinity.Cloud "+
						"environment (destroy only removes it from state). A destroy+create would leave the existing "+
						"environment %q live in ACM while the create fails the \"already exists\" guard, stranding you "+
						"with empty state.\n\nTo change %s: delete the environment manually in the ACM UI (Environments "+
						"→ %s → Delete, approve the emailed confirmation), then apply to create the new one — or stand up "+
						"the replacement as a separate resource first and migrate.",
						f.attr, state.Name.ValueString(), f.attr, state.Name.ValueString()),
				)
			}
		}
		return
	}

	// Create path: refuse to plan a create over an environment that already exists.
	// name may be unknown (interpolated); only the apply-time guard can catch that.
	if plan.Name.IsUnknown() || plan.Name.IsNull() {
		return
	}
	name := plan.Name.ValueString()

	existing, err := r.client.GetEnvironmentByName(ctx, name)
	if err != nil {
		if acm.IsNotFound(err) {
			return
		}
		// Inconclusive lookup: don't block the plan — the Create-time guard still
		// applies — but tell the operator the check could not be completed.
		resp.Diagnostics.AddWarning(
			"Could not check whether the environment already exists",
			fmt.Sprintf("Looking up environment %q during planning failed: %s.\n\n"+
				"The plan continues; the create will still fail if the environment turns out to exist.", name, err.Error()),
		)
		return
	}

	resp.Diagnostics.AddAttributeError(
		path.Root("name"),
		"Environment already exists",
		environmentExistsDetail(name, existing.ID),
	)
}

// environmentExistsDetail is the shared "refuse to adopt, import instead"
// diagnostic detail used by both the plan-time guard (ModifyPlan) and the
// apply-time guard (Create), so the wording and import hint stay in sync.
func environmentExistsDetail(name string, id int64) string {
	return fmt.Sprintf("An environment named %q already exists in Altinity.Cloud (id %d). Terraform "+
		"will not adopt an environment it did not create. Import it into state instead:\n\n"+
		"  terraform import altinity_environment.<resource_name> %d", name, id, id)
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

// applyEnvironmentToModel writes the API's computed fields onto the model.
// name/cloud_provider/region are RequiresReplace and operator-owned; they are
// reconciled from the API ONLY when it echoes a non-empty value (so importing an
// environment populates them) and otherwise preserved from plan/state (so a GET
// that omits them never forces a spurious replace). The datadog block is
// reconciled in-place for its non-secret fields; `api_key`/`apply_to_clusters`
// and `maintenance_windows` are operator/write-owned and left as the caller's
// model already holds them.
func applyEnvironmentToModel(m *environmentResourceModel, e acm.Environment) {
	m.ID = types.StringValue(strconv.FormatInt(e.ID, 10))
	// name/cloud_provider/region are operator-owned; only overwrite from the API
	// when it actually echoes a value, else preserve the plan/state value.
	if e.Name != "" {
		m.Name = types.StringValue(e.Name)
	}
	if e.CloudProvider != "" {
		m.CloudProvider = types.StringValue(e.CloudProvider)
	}
	if e.Region != "" {
		m.Region = types.StringValue(e.Region)
	}
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

	// Refuse to create when an environment with this name already exists. ACM
	// environment names are unique per organization, so a name match means an
	// environment Terraform did not create — silently adopting it would put
	// unmanaged (or another state's) infrastructure under this resource. The
	// operator must `terraform import` it instead. The lookup runs inside
	// RetryWhileBusy so a transient env lock retries rather than failing the apply.
	var existing acm.Environment
	found := false
	if err := acm.RetryWhileBusy(opCtx, func() error {
		found = false // reset per attempt so a retry never leaves a stale true
		e, gerr := r.client.GetEnvironmentByName(opCtx, name)
		if gerr == nil {
			existing, found = e, true
			return nil
		}
		if acm.IsNotFound(gerr) {
			return nil
		}
		return gerr
	}); err != nil {
		resp.Diagnostics.AddError("Failed to check for an existing environment", err.Error())
		return
	}
	if found {
		resp.Diagnostics.AddError(
			"Environment already exists",
			environmentExistsDetail(name, existing.ID),
		)
		return
	}

	// Request the new environment.
	var envID int64
	if err := acm.RetryWhileBusy(opCtx, func() error {
		created, cerr := r.client.RequestEnvironment(opCtx, envReq)
		if cerr != nil {
			return cerr
		}
		envID = created.ID
		return nil
	}); err != nil {
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

	// Poll until ready. On timeout we return WITHOUT setting state. The request
	// already created the environment in ACM, so a plain re-apply would now fail
	// the "already exists" guard above; direct the operator to raise the create
	// timeout or import the environment by id once it finishes provisioning.
	if err := acm.PollUntilHealthy(opCtx, func(c context.Context) (string, error) {
		e, gerr := r.client.GetEnvironmentByID(c, envID)
		return e.Status, gerr
	}); err != nil {
		resp.Diagnostics.AddError(
			"Environment did not become ready",
			fmt.Sprintf("Environment %q (id %d) is still provisioning: %s.\n\n"+
				"It was created in Altinity.Cloud but did not become ready within the create timeout. "+
				"Raise the create timeout, or once it finishes provisioning import it into state:\n\n"+
				"  terraform import altinity_environment.<resource_name> %d", name, envID, err, envID),
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

	// Reconcile maintenance windows from acc-check ONLY when the operator manages
	// them (prior state non-null). This detects out-of-band changes/deletions
	// (EnvironmentShow returns them null; acc-check is the readable source) while
	// preserving "omit = unmanaged": a null stays null with no probe, so an
	// unmanaged env never has windows pulled into state.
	//
	// Two paths KEEP the last-known windows rather than blanking them (which would
	// surface as false drift): a transient acc-check error, and a "not reported"
	// response (acc-check returned null for the field — connector not synced). Only
	// a confirmed result (known) — including a confirmed empty [] (deletion) — is
	// reconciled into state.
	if !state.MaintenanceWindows.IsNull() && !state.MaintenanceWindows.IsUnknown() {
		windows, known, werr := r.client.GetEnvironmentMaintenanceWindows(ctx, id)
		switch {
		case werr != nil:
			resp.Diagnostics.AddWarning(
				"Could not refresh maintenance windows",
				fmt.Sprintf("Reading maintenance windows for environment %d via acc-check failed: %s.\n\n"+
					"Keeping the last-known windows in state; maintenance_windows drift is not detected this run.", id, werr.Error()),
			)
		case !known:
			// acc-check did not report the field (null) — keep prior, don't blank.
		default:
			mws, d := maintenanceWindowsToSet(ctx, windows)
			resp.Diagnostics.Append(d...)
			if resp.Diagnostics.HasError() {
				return
			}
			state.MaintenanceWindows = mws
		}
	}

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

// ImportState imports by the ACM environment id, reconstructing as much state
// from the API as it returns: the computed fields and cloud_provider/region (via
// applyEnvironmentToModel), plus the datadog integration when present. Terraform
// calls Read immediately after import with this state as the prior, and Read
// preserves datadog (config-authoritative) so it survives.
//
// maintenance_windows are sourced from the acc-check endpoint (EnvironmentShow
// returns them as null) — see GetEnvironmentMaintenanceWindows. That read is
// best-effort: if acc-check fails, import still succeeds and emits a warning, and
// the windows show as a diff on the first plan instead.
//
// One field still cannot be imported: datadog `api_key`. The API returns it, but
// the provider deliberately never stores it (kept out of Terraform state, masked
// in debug logs), so it imports as null and must be re-supplied in config — the
// expected diff on the first post-import plan.
func (r *environmentResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	id, err := parseACMID("id", req.ID)
	if err != nil {
		resp.Diagnostics.AddError(
			"Invalid environment import id",
			fmt.Sprintf("Import id must be the numeric ACM environment id "+
				"(e.g. `terraform import altinity_environment.example 2293`): %s", err.Error()),
		)
		return
	}

	env, gerr := r.client.GetEnvironmentByID(ctx, id)
	if gerr != nil {
		resp.Diagnostics.AddError("Failed to read environment for import", gerr.Error())
		return
	}

	var m environmentResourceModel
	// Mark datadog as managed when the environment has it defined, so
	// applyEnvironmentToModel reconciles its non-secret fields. api_key is
	// write-only (null here) and apply_to_clusters is write-side (resolved to its
	// default true by applyEnvironmentToModel).
	if env.Datadog != nil {
		m.Datadog = &datadogModel{
			APIKey:          types.StringNull(),
			ApplyToClusters: types.BoolNull(),
		}
	}
	applyEnvironmentToModel(&m, env)

	// Capture existing maintenance windows from acc-check (EnvironmentShow returns
	// them null). Import policy: only manage them when acc-check confirms windows
	// exist (known && non-empty). On error, an unreported (null) response, OR a
	// confirmed-empty env, leave maintenance_windows null (unmanaged) — importing
	// an env with no windows must not force the operator to manage an empty block.
	// A failed read warns; the windows then surface as a diff on the first plan.
	m.MaintenanceWindows = types.SetNull(types.ObjectType{AttrTypes: maintenanceWindowAttrTypes()})
	windows, known, werr := r.client.GetEnvironmentMaintenanceWindows(ctx, id)
	if werr != nil {
		resp.Diagnostics.AddWarning(
			"Could not import maintenance windows",
			fmt.Sprintf("Reading maintenance windows for environment %d via acc-check failed: %s.\n\n"+
				"Import succeeded without them; re-declare maintenance_windows in config and apply to re-sync.", id, werr.Error()),
		)
	} else if known && len(windows) > 0 {
		mws, d := maintenanceWindowsToSet(ctx, windows)
		resp.Diagnostics.Append(d...)
		if resp.Diagnostics.HasError() {
			return
		}
		m.MaintenanceWindows = mws
	}

	// timeouts are provider-side config only (no API representation); leave null
	// so the operator's configured block applies without showing as drift.
	m.Timeouts = types.ObjectNull(map[string]attr.Type{
		"create": types.StringType,
		"update": types.StringType,
		"delete": types.StringType,
	})

	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// maintenanceWindowsToSet converts the API's maintenance windows (read from the
// acc-check endpoint) into the resource's set value. Modeled as a set so neither
// window order nor day order (both of which ACM may reorder) produces a diff.
//
// An empty input yields an EMPTY set (`[]`), NOT null: this is the reconcile path
// (Read), where a confirmed-empty response on a managed attribute means "no
// windows" — which must equal a config of `[]` (clear-all) to avoid churn, and
// must differ from a config of `[{...}]` to surface a deletion as drift. The
// "empty -> null (unmanaged)" decision is the IMPORT policy and lives in
// ImportState, not here.
func maintenanceWindowsToSet(ctx context.Context, windows []acm.MaintenanceWindow) (types.Set, diag.Diagnostics) {
	objType := types.ObjectType{AttrTypes: maintenanceWindowAttrTypes()}
	if len(windows) == 0 {
		return types.SetValueFrom(ctx, objType, []maintenanceWindowModel{})
	}
	models := make([]maintenanceWindowModel, 0, len(windows))
	for _, w := range windows {
		days, d := types.SetValueFrom(ctx, types.StringType, w.Days)
		if d.HasError() {
			return types.SetNull(objType), d
		}
		models = append(models, maintenanceWindowModel{
			Name:        types.StringValue(w.Name),
			Enabled:     types.BoolValue(w.Enabled),
			Hour:        types.Int64Value(int64(w.Hour)),
			LengthHours: types.Int64Value(int64(w.LengthInHours)),
			Days:        days,
		})
	}
	return types.SetValueFrom(ctx, objType, models)
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
			if diags.HasError() {
				return req, diags
			}
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

// weekdaySetValidator rejects `days` entries that aren't uppercase weekday names.
type weekdaySetValidator struct{}

func (weekdaySetValidator) Description(context.Context) string {
	return "each day must be an uppercase weekday name (MONDAY…SUNDAY)"
}

func (v weekdaySetValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (weekdaySetValidator) ValidateSet(ctx context.Context, req validator.SetRequest, resp *validator.SetResponse) {
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
