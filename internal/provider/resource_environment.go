// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
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
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	CloudProvider  types.String `tfsdk:"cloud_provider"`
	Region         types.String `tfsdk:"region"`
	DisplayName    types.String `tfsdk:"display_name"`
	NormalizedName types.String `tfsdk:"normalized_name"`
	Type           types.String `tfsdk:"type"`
	Domain         types.String `tfsdk:"domain"`
	Status         types.String `tfsdk:"status"`
	State          types.String `tfsdk:"state"`
	Timeouts       types.Object `tfsdk:"timeouts"`
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
// preserved from plan/state.
func applyEnvironmentToModel(m *environmentResourceModel, e acm.Environment) {
	m.ID = types.StringValue(strconv.FormatInt(e.ID, 10))
	m.Name = types.StringValue(e.Name)
	m.DisplayName = types.StringValue(e.DisplayName)
	m.NormalizedName = types.StringValue(e.NormalizedName)
	m.Type = types.StringValue(e.Type)
	m.Domain = types.StringValue(e.Domain)
	m.Status = types.StringValue(e.Status)
	m.State = types.StringValue(e.State)
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

	// If the operator specified a display_name, apply it now — the request
	// endpoint cannot set it, so without this Create would produce state that
	// disagrees with the plan.
	if !plan.DisplayName.IsUnknown() && !plan.DisplayName.IsNull() && plan.DisplayName.ValueString() != "" {
		dn := plan.DisplayName.ValueString()
		if err := acm.RetryWhileBusy(opCtx, func() error {
			_, e := r.client.EditEnvironment(opCtx, envID, acm.EnvironmentEditRequest{DisplayName: dn})
			return e
		}); err != nil {
			resp.Diagnostics.AddError("Failed to set environment display_name after create", err.Error())
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

	// display_name is the only mutable attribute (everything else is ForceNew).
	if err := acm.RetryWhileBusy(ctx, func() error {
		_, e := r.client.EditEnvironment(ctx, id, acm.EnvironmentEditRequest{DisplayName: plan.DisplayName.ValueString()})
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
