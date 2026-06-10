// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

var (
	_ resource.Resource                = (*nodeTypeResource)(nil)
	_ resource.ResourceWithConfigure   = (*nodeTypeResource)(nil)
	_ resource.ResourceWithImportState = (*nodeTypeResource)(nil)
)

// nodeTypeResource is altinity_node_type: an environment node type (instance
// shape) a cluster can be scheduled onto. See the design spec
// (docs/superpowers/specs/2026-06-10-altinity-node-type-design.md).
type nodeTypeResource struct {
	client *acm.Client
}

// NewNodeTypeResource is the constructor registered with the provider.
func NewNodeTypeResource() resource.Resource {
	return &nodeTypeResource{}
}

type nodeTypeResourceModel struct {
	ID           types.String  `tfsdk:"id"`
	NodeTypeID   types.String  `tfsdk:"node_type_id"`
	Environment  types.String  `tfsdk:"environment"`
	Scope        types.String  `tfsdk:"scope"`
	Code         types.String  `tfsdk:"code"`
	CPU          types.Float64 `tfsdk:"cpu"`
	Memory       types.Int64   `tfsdk:"memory"`
	Capacity     types.Int64   `tfsdk:"capacity"`
	StorageClass types.String  `tfsdk:"storage_class"`
	IsSpot       types.Bool    `tfsdk:"is_spot"`
	Name         types.String  `tfsdk:"name"`
	Used         types.Bool    `tfsdk:"used"`
}

func (r *nodeTypeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_node_type"
}

func (r *nodeTypeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "An Altinity.Cloud environment node type (instance shape) that clusters can be " +
			"scheduled onto. Discover valid `code`/`cpu`/`memory` values with the altinity_instance_types " +
			"data source.\n\n" +
			"Tolerations, nodeSelector, and extraSpec are NOT managed by this resource: on create it " +
			"mirrors the ACM UI's per-scope default tolerations, and on update it preserves whatever " +
			"ACM currently has. Managing them via Terraform is not supported.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   `Resource id, "<environment>:<node_type_id>".`,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"node_type_id": schema.StringAttribute{
				Computed:      true,
				Description:   "The ACM node type id (used by /nodetype/{id}).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"environment": schema.StringAttribute{
				Required:      true,
				Description:   "ACM environment id the node type belongs to.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"scope": schema.StringAttribute{
				Required:      true,
				Description:   "Node type scope: clickhouse, zookeeper, or system.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{nodeTypeScopeValidator{}},
			},
			"code": schema.StringAttribute{
				Required:    true,
				Description: "Instance type code (see the altinity_instance_types data source). Editable in place.",
			},
			"cpu": schema.Float64Attribute{
				Optional: true,
				Computed: true,
				Description: "vCPUs for this node type. Sent to ACM as a hint; ACM derives the " +
					"authoritative value from the instance type `code`, so the read-back value wins.",
				PlanModifiers: []planmodifier.Float64{nodeTypeSizeFloat64PlanModifier{}},
			},
			"memory": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Description: "Memory in MB for this node type. Sent to ACM as a hint; ACM derives the " +
					"authoritative value from the instance type `code` (the allocatable memory), so the " +
					"read-back value wins — do not expect the submitted value to be kept verbatim.",
				PlanModifiers: []planmodifier.Int64{nodeTypeSizeInt64PlanModifier{}},
			},
			"capacity": schema.Int64Attribute{
				Optional:      true,
				Computed:      true,
				Description:   "Maximum number of nodes of this type.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"storage_class": schema.StringAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "Storage class for this node type.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"is_spot": schema.BoolAttribute{
				Optional:      true,
				Computed:      true,
				Description:   "Whether to use spot/preemptible instances.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "Display name. Ignored by ACM on create (defaults to the code); a custom " +
					"name is applied via a follow-up edit when set.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"used": schema.BoolAttribute{
				Computed:      true,
				Description:   "True when a cluster currently uses this node type. Read-only.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *nodeTypeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func applyNodeTypeToModel(m *nodeTypeResourceModel, nt acm.NodeType) {
	m.NodeTypeID = types.StringValue(strconv.FormatInt(nt.ID, 10))
	m.ID = types.StringValue(m.Environment.ValueString() + ":" + strconv.FormatInt(nt.ID, 10))
	m.Code = types.StringValue(nt.Code)
	m.CPU = types.Float64Value(nt.CPU)
	m.Memory = types.Int64Value(nt.Memory)
	m.Capacity = types.Int64Value(nt.Capacity)
	m.StorageClass = types.StringValue(nt.StorageClass)
	m.IsSpot = types.BoolValue(nt.IsSpot)
	m.Name = types.StringValue(nt.Name)
	m.Used = types.BoolValue(nt.Used)
}

func (r *nodeTypeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, config nodeTypeResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	// config carries the operator's cpu/memory hints, which the plan modifier
	// blanks out of the plan (they are server-derived). They are sent to ACM as
	// the create hint; ACM's returned values are authoritative.
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	env := plan.Environment.ValueString()
	scope := plan.Scope.ValueString()
	code := plan.Code.ValueString()
	desiredName := config.Name.ValueString()

	// Adopt-by-(scope,code) then create — both inside one RetryWhileBusy so a
	// transient env lock retries the whole sequence (mirrors the keeper resource).
	var nt acm.NodeType
	err := acm.RetryWhileBusy(ctx, func() error {
		existing, found, ferr := r.client.FindNodeTypeByCode(ctx, env, scope, code)
		if ferr != nil {
			return ferr
		}
		if found {
			nt = existing
			return nil
		}
		// Fresh create: mirror the ACM UI by sending the scope-default tolerations.
		created, cerr := r.client.CreateNodeType(ctx, env, acm.NodeTypeRequest{
			Name:         desiredName,
			Scope:        scope,
			Code:         code,
			CPU:          config.CPU.ValueFloat64(),
			Memory:       config.Memory.ValueInt64(),
			Capacity:     plan.Capacity.ValueInt64(),
			StorageClass: plan.StorageClass.ValueString(),
			IsSpot:       plan.IsSpot.ValueBool(),
			Tolerations:  acm.ScopeDefaultTolerations(scope),
			NodeSelector: emptyJSONString,
			ExtraSpec:    emptyJSONString,
		})
		if cerr != nil {
			return cerr
		}
		nt = created
		return nil
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to create node type", err.Error())
		return
	}

	// ACM ignores `name` on create (the created name equals the code). If the
	// operator set a different name, apply it via a follow-up edit — carrying the
	// just-created opaque fields and authoritative sizing, and the operator's
	// name (NOT nt.Name, which is still the code).
	if desiredName != "" && desiredName != nt.Name {
		edited, eerr := r.client.EditNodeType(ctx, nt.ID, acm.NodeTypeRequest{
			Name:         desiredName,
			Scope:        nt.Scope,
			Code:         nt.Code,
			Capacity:     nt.Capacity,
			StorageClass: nt.StorageClass,
			IsSpot:       nt.IsSpot,
			Tolerations:  nt.Tolerations,
			NodeSelector: nt.NodeSelector,
			ExtraSpec:    nt.ExtraSpec,
		})
		if eerr != nil {
			resp.Diagnostics.AddError("Failed to set node type name after create", eerr.Error())
			return
		}
		nt = edited
	}

	applyNodeTypeToModel(&plan, nt)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *nodeTypeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state nodeTypeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	nt, found, err := r.findNodeType(ctx, state)
	if err != nil {
		// A gone parent environment surfaces as 404/403 on the list endpoint.
		if acm.IsNotFound(err) || acm.IsForbidden(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read node type", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	applyNodeTypeToModel(&state, nt)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// findNodeType locates the node type backing the state: by ACM id when known,
// else by (scope, code) — the latter covers the post-import case where only
// environment/scope/code are populated.
func (r *nodeTypeResource) findNodeType(ctx context.Context, state nodeTypeResourceModel) (acm.NodeType, bool, error) {
	env := state.Environment.ValueString()
	nts, err := r.client.ListNodeTypes(ctx, env)
	if err != nil {
		return acm.NodeType{}, false, err
	}
	wantID, hasID := int64(0), false
	if !state.NodeTypeID.IsNull() && state.NodeTypeID.ValueString() != "" {
		if id, perr := parseACMID("node_type_id", state.NodeTypeID.ValueString()); perr == nil {
			wantID, hasID = id, true
		}
	}
	for i := range nts {
		if hasID {
			if nts[i].ID == wantID {
				return nts[i], true, nil
			}
			continue
		}
		if nts[i].Scope == state.Scope.ValueString() && nts[i].Code == state.Code.ValueString() {
			return nts[i], true, nil
		}
	}
	return acm.NodeType{}, false, nil
}

func (r *nodeTypeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, config nodeTypeResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseACMID("node_type_id", plan.NodeTypeID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid node type id in state", err.Error())
		return
	}

	// Preserve ACM's tolerations/nodeSelector/extraSpec: read the current node
	// type and pass its opaque fields back unchanged so the edit never alters them.
	cur, found, ferr := r.findNodeType(ctx, plan)
	if ferr != nil {
		resp.Diagnostics.AddError("Failed to read node type before update", ferr.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	// cpu/memory are server-derived from code; send the operator's config hints
	// (ACM re-normalizes), and preserve the opaque fields from the current type.
	updated, eerr := r.client.EditNodeType(ctx, id, acm.NodeTypeRequest{
		Name:         plan.Name.ValueString(),
		Scope:        plan.Scope.ValueString(),
		Code:         plan.Code.ValueString(),
		CPU:          config.CPU.ValueFloat64(),
		Memory:       config.Memory.ValueInt64(),
		Capacity:     plan.Capacity.ValueInt64(),
		StorageClass: plan.StorageClass.ValueString(),
		IsSpot:       plan.IsSpot.ValueBool(),
		Tolerations:  cur.Tolerations,
		NodeSelector: cur.NodeSelector,
		ExtraSpec:    cur.ExtraSpec,
	})
	if eerr != nil {
		resp.Diagnostics.AddError("Failed to update node type", eerr.Error())
		return
	}
	applyNodeTypeToModel(&plan, updated)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *nodeTypeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state nodeTypeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	id, err := parseACMID("node_type_id", state.NodeTypeID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid node type id in state", err.Error())
		return
	}

	if err := r.client.RemoveNodeType(ctx, id); err != nil {
		if acm.IsNotFound(err) {
			return
		}
		resp.Diagnostics.AddError(
			"Failed to delete node type",
			fmt.Sprintf("%s\n\nA node type cannot be deleted while a cluster uses it. "+
				"Destroy or rescale the dependent cluster(s) off this node type first.", err.Error()),
		)
		return
	}
}

// ImportState parses "<environment>:<scope>:<code>"; Read then resolves the id.
func (r *nodeTypeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(strings.TrimSpace(req.ID), ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("altinity_node_type import expects \"<environment>:<scope>:<code>\", got %q", req.ID),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("environment"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("scope"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("code"), parts[2])...)
}

// emptyJSONString is the JSON literal "" the ACM UI sends for nodeSelector /
// extraSpec on create.
var emptyJSONString = []byte(`""`)

// nodeTypeSize{Float64,Int64}PlanModifier make cpu/memory server-authoritative:
// ACM derives them from the instance type `code` (memory is normalized to the
// node's allocatable, which the operator cannot predict). The modifier forces
// the planned value to "known after apply" on create or whenever `code` changes
// — so ACM's returned value is accepted without an inconsistent-result error —
// and otherwise keeps the prior state value (no spurious diffs when `code` is
// unchanged, even if the operator's cpu/memory hint in config differs).
//
// The bodies are deliberately identical across the two numeric types, which the
// framework models with distinct interfaces; mirror any change in both.
const nodeTypeSizePlanModifierDesc = "server-derived from code; known after apply on create or code change"

type nodeTypeSizeFloat64PlanModifier struct{}

func (nodeTypeSizeFloat64PlanModifier) Description(context.Context) string {
	return nodeTypeSizePlanModifierDesc
}
func (nodeTypeSizeFloat64PlanModifier) MarkdownDescription(context.Context) string {
	return nodeTypeSizePlanModifierDesc
}
func (nodeTypeSizeFloat64PlanModifier) PlanModifyFloat64(ctx context.Context, req planmodifier.Float64Request, resp *planmodifier.Float64Response) {
	if req.State.Raw.IsNull() {
		resp.PlanValue = types.Float64Unknown() // create
		return
	}
	if nodeTypeCodeChanged(ctx, req.Plan, req.State) {
		resp.PlanValue = types.Float64Unknown()
		return
	}
	resp.PlanValue = req.StateValue
}

type nodeTypeSizeInt64PlanModifier struct{}

func (nodeTypeSizeInt64PlanModifier) Description(context.Context) string {
	return nodeTypeSizePlanModifierDesc
}
func (nodeTypeSizeInt64PlanModifier) MarkdownDescription(context.Context) string {
	return nodeTypeSizePlanModifierDesc
}
func (nodeTypeSizeInt64PlanModifier) PlanModifyInt64(ctx context.Context, req planmodifier.Int64Request, resp *planmodifier.Int64Response) {
	if req.State.Raw.IsNull() {
		resp.PlanValue = types.Int64Unknown() // create
		return
	}
	if nodeTypeCodeChanged(ctx, req.Plan, req.State) {
		resp.PlanValue = types.Int64Unknown()
		return
	}
	resp.PlanValue = req.StateValue
}

// nodeTypeCodeChanged reports whether the planned `code` differs from state.
func nodeTypeCodeChanged(ctx context.Context, plan tfsdk.Plan, state tfsdk.State) bool {
	var planCode, stateCode types.String
	plan.GetAttribute(ctx, path.Root("code"), &planCode)
	state.GetAttribute(ctx, path.Root("code"), &stateCode)
	return !planCode.Equal(stateCode)
}

// nodeTypeScopeValidator restricts scope to the values ACM accepts.
type nodeTypeScopeValidator struct{}

func (nodeTypeScopeValidator) Description(context.Context) string {
	return "must be one of clickhouse, zookeeper, system"
}

func (v nodeTypeScopeValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (nodeTypeScopeValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	switch req.ConfigValue.ValueString() {
	case "clickhouse", "zookeeper", "system":
	default:
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid scope",
			fmt.Sprintf("scope must be one of clickhouse, zookeeper, system; got %q", req.ConfigValue.ValueString()),
		)
	}
}
