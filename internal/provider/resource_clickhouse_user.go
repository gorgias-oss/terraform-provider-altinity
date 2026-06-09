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
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm"
)

// Ensure the resource satisfies the framework interfaces.
var (
	_ resource.Resource                = (*userResource)(nil)
	_ resource.ResourceWithConfigure   = (*userResource)(nil)
	_ resource.ResourceWithImportState = (*userResource)(nil)
)

// userResource manages a single ClickHouse DB user on a cluster
// (altinity_clickhouse_user). It owns exactly one user via
// /cluster/{cluster}/users (GET list / POST create) and /user/{id}
// (POST update / DELETE remove); see design §5 for the ownership boundary
// against the cluster and other satellite resources.
//
// Identity (design §5.1): the resource id is the composite "<cluster_id>:<name>"
// where name is the user login (the config-stable key). The ACM-internal user
// id is resolved by matching the parent collection by login on Read and carried
// in computed state (user_id) for subsequent update/delete.
type userResource struct {
	client *acm.Client
}

// NewUserResource is the constructor registered with the provider
// (altinity_clickhouse_user).
func NewUserResource() resource.Resource {
	return &userResource{}
}

// userResourceModel maps the altinity_clickhouse_user schema.
type userResourceModel struct {
	ID               types.String `tfsdk:"id"`
	ClusterID        types.String `tfsdk:"cluster_id"`
	Name             types.String `tfsdk:"name"`
	Networks         types.String `tfsdk:"networks"`
	Databases        types.List   `tfsdk:"databases"`
	AccessManagement types.Bool   `tfsdk:"access_management"`
	ProfileID        types.String `tfsdk:"profile_id"`
	Password         types.String `tfsdk:"password"`
	UserID           types.String `tfsdk:"user_id"`
}

func (r *userResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_clickhouse_user"
}

func (r *userResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manage a single ClickHouse DB user on an Altinity.Cloud cluster (/cluster/{cluster}/users and /user/{id}).",
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
				Description: "The ACM cluster id (integer, stored as string) that owns this user.",
				PlanModifiers: []planmodifier.String{
					// The user lives under a specific cluster; it cannot be
					// re-parented in place.
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "The user login (the config-stable key).",
				PlanModifiers: []planmodifier.String{
					// ACM exposes no rename; a changed login is a new user.
					stringplanmodifier.RequiresReplace(),
				},
				Validators: []validator.String{noColonValidator{}},
			},
			"networks": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "Allowed source networks for the user (e.g. `::/0`). Updatable in place via /user/{id}. " +
					"ACM canonicalizes this server-side (e.g. `0.0.0.0/0` becomes `::/0`), so the configured value is " +
					"treated as authoritative and kept verbatim in state; out-of-band changes are not detected.",
			},
			"databases": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				Description: "List of database NAMES the user may access (e.g. " +
					"`[\"default\"]` for one database, `[\"default\", \"analytics\"]` " +
					"for several). ACM expands each entry into a `GRANT ALL ON " +
					"\\`<db>\\`.* TO <user>` statement server-side, so do NOT " +
					"pre-qualify entries with `.*` or table names. Omit the attribute " +
					"(or pass `[]`) to grant access to ALL databases (`*.*`). " +
					"Updatable in place. The configured value is kept verbatim in " +
					"state for round-trip stability against ACM's server-side " +
					"canonicalization.",
			},
			"access_management": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
				Description: "Whether the user has ClickHouse access-management (RBAC) privileges. Updatable in place.",
			},
			"profile_id": schema.StringAttribute{
				Optional:    true,
				Description: "Optional ACM-internal settings profile id (integer, stored as string) to attach to the user.",
			},
			"password": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "The user password. Write-only at the API: sent on create/update but never returned on read, so it is preserved from prior state and excluded from drift detection.",
			},
			"user_id": schema.StringAttribute{
				Computed:    true,
				Description: "The ACM-internal user id (integer, stored as string).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *userResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *userResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	clusterID, err := parseACMID("cluster_id", plan.ClusterID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid cluster_id", err.Error())
		return
	}

	// Adopt-by-login is wrapped in transient-SQL retry. Two intertwined
	// failure modes both resolve by re-running the find-or-create:
	//   1. ACM's operator hasn't finished propagating the parent cluster's
	//      profile/setting state to ClickHouse yet → CreateUser's
	//      synchronous SQL emits malformed clauses, fails with Code 62
	//      SYNTAX_ERROR.
	//   2. A prior failed Create half-succeeded (ACM commits the user row
	//      BEFORE executing the SQL; a SQL error leaves the row in place)
	//      → on the next attempt FindUserByName detects the orphan and
	//      reconciles via UpdateUser instead of POSTing another create
	//      (which would return id=0 and trip the CreateUser guard).
	//
	// Re-running FindUserByName inside each retry iteration lets a half-
	// committed orphan from attempt N flip the path to UpdateUser on
	// attempt N+1.
	var created acm.User
	err = acm.RetryOnTransientCreateRace(ctx, func() error {
		existing, found, lerr := r.client.FindUserByName(ctx, clusterID, plan.Name.ValueString())
		if lerr != nil {
			return lerr
		}
		if found {
			// Reconcile the adopted user. Use the Update wire shape so
			// access_management is sent explicitly (the operator's plan is
			// the source of truth for an adopted resource — we just rebuild
			// ACM's view to match).
			apiReq, diagErr := r.buildUserRequest(plan, true)
			if diagErr != nil {
				return fmt.Errorf("invalid profile_id: %w", diagErr)
			}
			apiReq.Login = plan.Name.ValueString()
			c, uerr := r.client.UpdateUser(ctx, clusterID, existing.ID, apiReq)
			if uerr != nil {
				return uerr
			}
			created = c
			tflog.Info(ctx, "altinity_clickhouse_user: adopted existing user (matched by login)",
				map[string]any{"cluster_id": plan.ClusterID.ValueString(), "login": plan.Name.ValueString(), "user_id": existing.ID})
			return nil
		}
		// Fresh create: omit access_management when false (see
		// buildUserRequest docs — ACM's generated REVOKE on a brand-new
		// user breaks the SQL parser with "Code: 62 SYNTAX_ERROR").
		apiReq, diagErr := r.buildUserRequest(plan, false)
		if diagErr != nil {
			return fmt.Errorf("invalid profile_id: %w", diagErr)
		}
		c, cerr := r.client.CreateUser(ctx, clusterID, apiReq)
		if cerr != nil {
			return cerr
		}
		created = c
		return nil
	})
	if err != nil {
		resp.Diagnostics.AddError("Failed to create user", err.Error())
		return
	}

	r.applyUserToModel(&plan, &created)
	resolveComputedUserFields(&plan, &created)
	// Password is write-only; preserve the configured value (API never returns it).
	plan.UserID = types.StringValue(strconv.FormatInt(created.ID, 10))
	plan.ID = types.StringValue(userCompositeID(plan.ClusterID.ValueString(), plan.Name.ValueString()))

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	clusterID, err := parseACMID("cluster_id", state.ClusterID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid cluster_id", err.Error())
		return
	}

	users, err := r.client.ListUsers(ctx, clusterID)
	if err != nil {
		// 404 is canonical drift; 403 is ACM's actual response for
		// DbuserList against a deleted cluster ("Access denied" instead
		// of "Not found") — treat as drift too. A genuine token failure
		// (401) surfaces via AddError below.
		if acm.IsNotFound(err) || acm.IsForbidden(err) {
			tflog.Warn(ctx, "altinity_clickhouse_user: parent cluster not accessible; removing from state (drift)",
				map[string]any{"cluster_id": state.ClusterID.ValueString(), "error": err.Error()})
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Failed to read users", err.Error())
		return
	}

	// Match the parent collection by login (the config-stable key, §5.1).
	name := state.Name.ValueString()
	var found *acm.User
	for i := range users {
		if users[i].Login == name {
			found = &users[i]
			break
		}
	}
	if found == nil {
		// User no longer present in the cluster → removed out of band.
		resp.State.RemoveResource(ctx)
		return
	}

	r.applyUserToModel(&state, found)

	// networks/databases are kept verbatim from state (not overwritten from the
	// API) because ACM canonicalizes them server-side in unpredictable ways,
	// causing perpetual diffs. Warn when the API value has diverged so operators
	// are aware of out-of-band changes even though they are not corrected.
	if !state.Networks.IsNull() && !state.Networks.IsUnknown() && found.Networks != "" && found.Networks != state.Networks.ValueString() {
		tflog.Warn(ctx, "altinity_clickhouse_user: networks differs from ACM (out-of-band change not corrected; configured value kept in state)",
			map[string]any{"configured": state.Networks.ValueString(), "api_value": found.Networks})
	}
	if !state.Databases.IsNull() && !state.Databases.IsUnknown() && found.Databases != "" {
		// Compare as multisets — ACM stores the list as CSV and may return it in
		// a different order than the operator wrote (server-side sort/canonical
		// form). Element-order differences are not drift.
		configured := stringListToSlice(state.Databases)
		apiValue := splitCSVString(found.Databases)
		if !stringSlicesEqualUnordered(configured, apiValue) {
			tflog.Warn(ctx, "altinity_clickhouse_user: databases differs from ACM (out-of-band change not corrected; configured value kept in state)",
				map[string]any{"configured": strings.Join(configured, ","), "api_value": found.Databases})
		}
	}

	// ACM's user-list endpoint sometimes returns a null id for users created via
	// the API (observed live). Only refresh user_id from a real (non-zero) id;
	// otherwise preserve the value resolved at create time, so a subsequent
	// update/delete still targets the correct /cluster/{cluster}/user/{id}.
	if found.ID != 0 {
		state.UserID = types.StringValue(strconv.FormatInt(found.ID, 10))
	}
	state.ID = types.StringValue(userCompositeID(state.ClusterID.ValueString(), found.Login))
	// password is intentionally NOT touched here: the API never returns it, so
	// the prior state value is preserved and excluded from drift (design §9).

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *userResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan userResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state userResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The ACM-internal id is carried in state (cluster_id + name are
	// RequiresReplace, so they never change under Update).
	userID, err := parseACMID("user_id", state.UserID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid user_id", err.Error())
		return
	}
	// The edit endpoint is cluster-scoped; cluster_id is RequiresReplace so it
	// is identical in plan and state.
	clusterID, err := parseACMID("cluster_id", plan.ClusterID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid cluster_id", err.Error())
		return
	}

	// Update path: send access_management explicitly so the operator can
	// toggle it either direction (Create path omits a false value to dodge
	// ACM's stray-REVOKE SQL bug, but on Update the user already exists and
	// REVOKE is well-defined).
	apiReq, diagErr := r.buildUserRequest(plan, true)
	if diagErr != nil {
		resp.Diagnostics.AddError("Invalid profile_id", diagErr.Error())
		return
	}
	// Login is part of the update body (it is stable, but ACM expects it).
	apiReq.Login = plan.Name.ValueString()

	updated, err := r.client.UpdateUser(ctx, clusterID, userID, apiReq)
	if err != nil {
		resp.Diagnostics.AddError("Failed to update user", err.Error())
		return
	}

	r.applyUserToModel(&plan, &updated)
	resolveComputedUserFields(&plan, &updated)
	plan.UserID = state.UserID
	plan.ID = types.StringValue(userCompositeID(plan.ClusterID.ValueString(), plan.Name.ValueString()))

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state userResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	userID, err := parseACMID("user_id", state.UserID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid user_id", err.Error())
		return
	}
	clusterID, err := parseACMID("cluster_id", state.ClusterID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Invalid cluster_id", err.Error())
		return
	}

	if err := r.client.DeleteUser(ctx, clusterID, userID); err != nil {
		if acm.IsNotFound(err) {
			// Already gone: treat as success (the framework removes from state).
			return
		}
		resp.Diagnostics.AddError("Failed to delete user", err.Error())
		return
	}
}

// ImportState parses the composite "<cluster_id>:<name>" id. The splitter uses
// LastIndex defensively in case stricter validators are relaxed in the future
// (today noColonValidator forbids ':' in the name, so first vs last is
// equivalent).
func (r *userResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	clusterID, name, err := splitUserCompositeID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Invalid import ID", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("cluster_id"), clusterID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), userCompositeID(clusterID, name))...)
}

// buildUserRequest assembles the ACM UserRequest from the model. profile_id is
// optional; when set it must parse as an integer.
//
// alwaysSendAccessMgmt controls how access_management is wired into the
// request:
//   - Update path passes true: the field is always sent (0 OR 1) so an
//     operator can toggle it either direction.
//   - Create path passes false: the field is sent ONLY when the configured
//     value is true. ACM's generated CREATE USER + GRANT SQL chokes on a stray
//     REVOKE ACCESS MANAGEMENT clause it emits when accessManagement=0 is
//     present on a freshly-created user that has not yet been granted it
//     (Code 62 syntax error from ClickHouse).
//
// networks and databases are sent as JSON arrays per the ACM OpenAPI spec.
// The Terraform schema keeps them as operator-friendly comma-separated
// strings; we split on `,` here. Empty/null values become nil arrays so
// omitempty drops them from the body.
func (r *userResource) buildUserRequest(m userResourceModel, alwaysSendAccessMgmt bool) (acm.UserRequest, error) {
	req := acm.UserRequest{
		Login:     m.Name.ValueString(),
		Networks:  splitCSV(m.Networks),
		Databases: stringListToSlice(m.Databases),
		Password:  m.Password.ValueString(),
	}
	if !m.AccessManagement.IsNull() && !m.AccessManagement.IsUnknown() {
		on := m.AccessManagement.ValueBool()
		if on || alwaysSendAccessMgmt {
			v := 0
			if on {
				v = 1
			}
			req.AccessManagement = &v
		}
	}
	if !m.ProfileID.IsNull() && !m.ProfileID.IsUnknown() && m.ProfileID.ValueString() != "" {
		id, err := strconv.ParseInt(m.ProfileID.ValueString(), 10, 64)
		if err != nil {
			return acm.UserRequest{}, fmt.Errorf("profile_id %q is not a valid integer: %w", m.ProfileID.ValueString(), err)
		}
		req.IDProfile = id
	}
	return req, nil
}

// splitCSV splits an Optional+Computed comma-separated string attribute into a
// []string the ACM wire layer expects. Null/unknown/empty inputs become nil
// (omitempty drops the field). Whitespace around each element is trimmed.
func splitCSV(v types.String) []string {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	return splitCSVString(v.ValueString())
}

// splitCSVString is the plain-string variant of splitCSV. Used at the domain
// boundary when we already have the value (e.g. from a domain User struct).
//
// Tolerates both comma- and newline- (and CR/tab/semicolon-) separated input.
// This matters because operator HCL uses commas (`"default,system"`) but ACM
// stores multi-value fields like `networks` as newline-separated text and
// returns them that way in /users responses. Without this, fetching the admin
// user's existing networks ("0.0.0.0/0\n::/0") and re-sending them for the
// password update produces a single garbage CIDR ("0.0.0.0/0\n::/0") which
// ACM's pre-flight rejects as "Cluster check has failed".
func splitCSVString(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ';'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// applyUserToModel copies the server-returned (or list-matched) user fields into
// the model. It deliberately never touches password (write-only) and only sets
// profile_id when the API returns a non-zero one (avoids spurious drift when
// the API omits it).
//
// networks/databases are likewise NOT clobbered here: ACM canonicalizes them
// server-side (e.g. "0.0.0.0/0" -> "::/0"), and the rules are not predictable,
// so writing the API value back would (a) violate the plan on create/update
// ("inconsistent result after apply") and (b) cause perpetual diffs on read.
// The configured value is authoritative; the genuinely-computed case (config
// omitted the field) is filled from the API by resolveComputedUserFields.
func (r *userResource) applyUserToModel(m *userResourceModel, u *acm.User) {
	m.Name = types.StringValue(u.Login)
	m.AccessManagement = types.BoolValue(u.AccessManagement)
	if u.IDProfile != 0 {
		m.ProfileID = types.StringValue(strconv.FormatInt(u.IDProfile, 10))
	}
}

// resolveComputedUserFields fills networks/databases from the API ONLY when the
// plan left them unknown/null (Optional+Computed, config omitted). When the
// config provided a value it is kept verbatim so the post-apply state matches
// the plan despite ACM's server-side canonicalization. `databases` is a
// framework List<String>; ACM returns it as a comma/newline-separated string
// that we split via splitCSVString before lifting into the list type.
func resolveComputedUserFields(m *userResourceModel, u *acm.User) {
	if m.Networks.IsUnknown() || m.Networks.IsNull() {
		m.Networks = types.StringValue(u.Networks)
	}
	if m.Databases.IsUnknown() || m.Databases.IsNull() {
		m.Databases = sliceToStringList(splitCSVString(u.Databases))
	}
}

// userCompositeID builds the "<cluster_id>:<name>" resource id.
func userCompositeID(clusterID, name string) string {
	return clusterID + ":" + name
}

// splitUserCompositeID splits "<cluster_id>:<name>"; uses LastIndex defensively
// in case stricter validators are relaxed in the future (today noColonValidator
// forbids ':' in the name, so first vs last is equivalent).
func splitUserCompositeID(id string) (clusterID, name string, err error) {
	clusterID, name, err = splitCompositeID(id, true)
	if err != nil {
		return "", "", fmt.Errorf("expected import ID in the form <cluster_id>:<name>, got %q", id)
	}
	return clusterID, name, nil
}
