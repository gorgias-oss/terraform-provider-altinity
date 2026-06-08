// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Gorgias/terraform-provider-altinity/internal/acm"
)

func newUserResource(t *testing.T, client *acm.Client) *userResource {
	t.Helper()
	r := NewUserResource().(*userResource)
	r.client = client
	return r
}

// userSchemaOf returns the framework schema and its tftypes.Object so tests can
// build Config/State values exactly as the runtime does.
func userSchemaOf(t *testing.T, r *userResource) (objType tftypes.Object, sch rschema.Schema) {
	t.Helper()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	require.False(t, resp.Diagnostics.HasError())
	return resp.Schema.Type().TerraformType(context.Background()).(tftypes.Object), resp.Schema
}

type userVals struct {
	clusterID, name, networks, databases, profileID, password, id, userID string
	accessManagement                                                      bool
	accessSet                                                             bool
}

// userPlanValue builds a plan-shaped value (computed id/user_id left null).
func userPlanValue(objType tftypes.Object, v userVals) tftypes.Value {
	return userTFValue(objType, v, true)
}

// userStateValue builds a state-shaped value (computed id/user_id populated).
func userStateValue(objType tftypes.Object, v userVals) tftypes.Value {
	return userTFValue(objType, v, false)
}

func optString(s string) tftypes.Value {
	if s == "" {
		return tftypes.NewValue(tftypes.String, nil)
	}
	return tftypes.NewValue(tftypes.String, s)
}

// listToStringSliceForTest projects a framework List<String> back to a
// plain []string for assertion. Mirrors provider-level stringListToSlice
// but in the test package's namespace.
func listToStringSliceForTest(t *testing.T, l basetypes.ListValue) []string {
	t.Helper()
	if l.IsNull() || l.IsUnknown() {
		return nil
	}
	out := make([]string, 0, len(l.Elements()))
	for _, e := range l.Elements() {
		s, ok := e.(basetypes.StringValue)
		require.True(t, ok, "list element is not a string")
		out = append(out, s.ValueString())
	}
	return out
}

// optStringList builds a tftypes.List<String>. Empty string → null list;
// a comma-separated value is split into its elements.
func optStringList(csv string) tftypes.Value {
	listT := tftypes.List{ElementType: tftypes.String}
	if csv == "" {
		return tftypes.NewValue(listT, nil)
	}
	parts := strings.Split(csv, ",")
	elems := make([]tftypes.Value, 0, len(parts))
	for _, p := range parts {
		elems = append(elems, tftypes.NewValue(tftypes.String, strings.TrimSpace(p)))
	}
	return tftypes.NewValue(listT, elems)
}

func userTFValue(objType tftypes.Object, v userVals, plan bool) tftypes.Value {
	vals := map[string]tftypes.Value{}
	for attr := range objType.AttributeTypes {
		switch attr {
		case "cluster_id":
			vals[attr] = tftypes.NewValue(tftypes.String, v.clusterID)
		case "name":
			vals[attr] = tftypes.NewValue(tftypes.String, v.name)
		case "networks":
			vals[attr] = optString(v.networks)
		case "databases":
			// databases is a list<string>. v.databases is a comma-separated
			// shorthand in the test fixture; split it into a tftypes.List.
			vals[attr] = optStringList(v.databases)
		case "profile_id":
			vals[attr] = optString(v.profileID)
		case "password":
			vals[attr] = optString(v.password)
		case "access_management":
			if v.accessSet {
				vals[attr] = tftypes.NewValue(tftypes.Bool, v.accessManagement)
			} else {
				vals[attr] = tftypes.NewValue(tftypes.Bool, nil)
			}
		case "id":
			if plan {
				vals[attr] = tftypes.NewValue(tftypes.String, nil)
			} else {
				vals[attr] = tftypes.NewValue(tftypes.String, v.id)
			}
		case "user_id":
			if plan {
				vals[attr] = tftypes.NewValue(tftypes.String, nil)
			} else {
				vals[attr] = tftypes.NewValue(tftypes.String, v.userID)
			}
		default:
			vals[attr] = tftypes.NewValue(tftypes.String, nil)
		}
	}
	return tftypes.NewValue(objType, vals)
}

func TestUserResource_Metadata(t *testing.T) {
	r := NewUserResource()
	var resp resource.MetadataResponse
	r.Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "altinity"}, &resp)
	assert.Equal(t, "altinity_clickhouse_user", resp.TypeName)
}

func TestUserResource_Create_SendsPasswordAndPreservesIt(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Create flow: FindUserByName lookup (GET) then CreateUser (POST).
		if req.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		gotPath = req.URL.Path
		gotMethod = req.Method
		b, _ := io.ReadAll(req.Body)
		gotBody = string(b)
		// API never echoes the password back.
		_, _ = w.Write([]byte(`{"data":{"id":"99","login":"app","networks":"::/0","databases":"default","id_cluster":"7","id_profile":"3"}}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newUserResource(t, client)
	objType, sch := userSchemaOf(t, r)

	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: userPlanValue(objType, userVals{
			clusterID: "7", name: "app", networks: "::/0", databases: "default",
			profileID: "3", password: "s3cret", accessManagement: true, accessSet: true,
		})},
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(objType, nil)}}
	r.Create(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/cluster/7/users", gotPath)
	assert.Contains(t, gotBody, `"password":"s3cret"`)
	assert.Contains(t, gotBody, `"login":"app"`)
	assert.Contains(t, gotBody, `"id_profile":3`)
	// Per ACM OpenAPI: accessManagement is integer enum [0,1], not bool.
	assert.Contains(t, gotBody, `"accessManagement":1`)
	// Per ACM OpenAPI: networks/databases are arrays<string>, not plain strings.
	assert.Contains(t, gotBody, `"networks":["::/0"]`)
	assert.Contains(t, gotBody, `"databases":["default"]`)

	var model userResourceModel
	require.False(t, resp.State.Get(context.Background(), &model).HasError())
	assert.Equal(t, "7:app", model.ID.ValueString())
	assert.Equal(t, "99", model.UserID.ValueString())
	assert.Equal(t, "app", model.Name.ValueString())
	assert.Equal(t, "::/0", model.Networks.ValueString())
	assert.Equal(t, "3", model.ProfileID.ValueString())
	// Password preserved from config even though the API did not return it.
	assert.Equal(t, "s3cret", model.Password.ValueString())
}

// TestUserResource_Create_PreservesConfigNetworksOverCanonicalization
// reproduces the reported failure: config networks "0.0.0.0/0" but ACM returns
// the canonicalized "::/0". The provider must keep the configured value so the
// post-apply state matches the plan (else: "Provider produced inconsistent
// result after apply: .networks was 0.0.0.0/0, but now ::/0").
func TestUserResource_Create_PreservesConfigNetworksOverCanonicalization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"99","login":"app","networks":["::/0"],"databases":["default"],"id_cluster":"7"}}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newUserResource(t, client)
	objType, sch := userSchemaOf(t, r)

	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: userPlanValue(objType, userVals{
			clusterID: "7", name: "app", networks: "0.0.0.0/0", databases: "default",
			password: "pw", accessSet: true,
		})},
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(objType, nil)}}
	r.Create(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	var model userResourceModel
	require.False(t, resp.State.Get(context.Background(), &model).HasError())
	assert.Equal(t, "0.0.0.0/0", model.Networks.ValueString(), "configured networks kept despite ACM canonicalization")
	assert.Equal(t, []string{"default"}, listToStringSliceForTest(t, model.Databases))
}

// TestUserResource_Create_OmitsAccessManagementOnFalse documents the workaround
// for ACM's SQL-generation bug: when access_management is false (the default
// or explicitly), the Create request omits the field entirely. ACM otherwise
// emits a stray REVOKE statement on a freshly-created user that has never
// been granted access management, producing "Code: 62. DB::Exception: Syntax
// error" from the ClickHouse parser.
func TestUserResource_Create_OmitsAccessManagementOnFalse(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		b, _ := io.ReadAll(req.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"data":{"id":"99","login":"app","id_cluster":"7"}}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newUserResource(t, client)
	objType, sch := userSchemaOf(t, r)
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: userPlanValue(objType, userVals{
			clusterID: "7", name: "app", password: "pw",
			accessManagement: false, accessSet: true,
		})},
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(objType, nil)}}
	r.Create(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	assert.NotContains(t, gotBody, "accessManagement",
		"Create request must omit accessManagement when false to avoid ACM SQL syntax error")
}

// TestUserResource_Create_AdoptsExistingUserByLogin reproduces the live
// failure recovery: ACM commits a user row before running its generated
// ClickHouse SQL, so a SQL syntax error leaves an orphaned user. On retry
// the provider must detect the orphan via FindUserByName and reconcile it
// via UpdateUser rather than POSTing another create that returns id=0.
func TestUserResource_Create_AdoptsExistingUserByLogin(t *testing.T) {
	var sawCreate, sawUpdate bool
	var updateBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet:
			// Adopt path: an existing user matches the login.
			_, _ = w.Write([]byte(`{"data":[{"id":"42","login":"app","id_cluster":"7"}]}`))
		case req.Method == http.MethodPost && req.URL.Path == "/cluster/7/users":
			sawCreate = true
			_, _ = w.Write([]byte(`{"data":{"id":"99","login":"app","id_cluster":"7"}}`))
		case req.Method == http.MethodPost && req.URL.Path == "/cluster/7/user/42":
			sawUpdate = true
			b, _ := io.ReadAll(req.Body)
			updateBody = string(b)
			_, _ = w.Write([]byte(`{"data":{"id":"42","login":"app","id_cluster":"7"}}`))
		}
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newUserResource(t, client)
	objType, sch := userSchemaOf(t, r)
	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: userPlanValue(objType, userVals{
			clusterID: "7", name: "app", password: "rotated",
			accessManagement: false, accessSet: true,
		})},
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(objType, nil)}}
	r.Create(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	assert.False(t, sawCreate, "must NOT POST a duplicate create when adopting")
	assert.True(t, sawUpdate, "must reconcile via UpdateUser when an existing user matches")
	assert.Contains(t, updateBody, `"password":"rotated"`, "adopt path must push the configured password")

	var model userResourceModel
	require.False(t, resp.State.Get(context.Background(), &model).HasError())
	assert.Equal(t, "42", model.UserID.ValueString(), "user_id reflects the ADOPTED user")
	assert.Equal(t, "7:app", model.ID.ValueString())
}

func TestUserResource_Create_InvalidClusterID(t *testing.T) {
	client := acm.NewClient("http://unused", "tok")
	r := newUserResource(t, client)
	objType, sch := userSchemaOf(t, r)

	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: userPlanValue(objType, userVals{clusterID: "not-an-int", name: "app"})},
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(objType, nil)}}
	r.Create(context.Background(), req, &resp)
	require.True(t, resp.Diagnostics.HasError())
}

func TestUserResource_Create_InvalidProfileID(t *testing.T) {
	client := acm.NewClient("http://unused", "tok")
	r := newUserResource(t, client)
	objType, sch := userSchemaOf(t, r)

	req := resource.CreateRequest{
		Plan: tfsdk.Plan{Schema: sch, Raw: userPlanValue(objType, userVals{clusterID: "7", name: "app", profileID: "abc"})},
	}
	resp := resource.CreateResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(objType, nil)}}
	r.Create(context.Background(), req, &resp)
	require.True(t, resp.Diagnostics.HasError())
}

func TestUserResource_Read_MatchesByNameAndPreservesPassword(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		w.Header().Set("Content-Type", "application/json")
		// The matched entry reports networks/databases differing from prior state
		// (as ACM's server-side canonicalization would). Read must NOT clobber
		// them, else config-vs-refreshed-state would diff perpetually.
		_, _ = w.Write([]byte(`{"data":[
			{"id":"11","login":"other","networks":"::/0","id_cluster":"7"},
			{"id":"99","login":"app","networks":"10.0.0.0/8","databases":"analytics","id_cluster":"7","id_profile":"3"}
		]}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newUserResource(t, client)
	objType, sch := userSchemaOf(t, r)

	prior := userVals{
		clusterID: "7", name: "app", networks: "::/0", databases: "default",
		profileID: "3", password: "s3cret", id: "7:app", userID: "99",
		accessManagement: false, accessSet: true,
	}
	req := resource.ReadRequest{State: tfsdk.State{Schema: sch, Raw: userStateValue(objType, prior)}}
	resp := resource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: userStateValue(objType, prior)}}
	r.Read(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	assert.Equal(t, "/cluster/7/users", gotPath)

	var model userResourceModel
	require.False(t, resp.State.Get(context.Background(), &model).HasError())
	assert.Equal(t, "99", model.UserID.ValueString())
	assert.Equal(t, "7:app", model.ID.ValueString())
	// networks/databases preserved from prior state (config-authoritative):
	// ACM canonicalizes them server-side, so reflecting the API value would
	// cause perpetual diffs. Out-of-band changes to these fields are not detected.
	assert.Equal(t, "::/0", model.Networks.ValueString())
	assert.Equal(t, []string{"default"}, listToStringSliceForTest(t, model.Databases))
	// password preserved from prior state (API never returns it).
	assert.Equal(t, "s3cret", model.Password.ValueString())
}

func TestUserResource_Read_RemovedOutOfBand(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"11","login":"other","id_cluster":"7"}]}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newUserResource(t, client)
	objType, sch := userSchemaOf(t, r)

	prior := userVals{clusterID: "7", name: "app", id: "7:app", userID: "99", accessSet: true}
	req := resource.ReadRequest{State: tfsdk.State{Schema: sch, Raw: userStateValue(objType, prior)}}
	resp := resource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: userStateValue(objType, prior)}}
	r.Read(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)
	assert.True(t, resp.State.Raw.IsNull(), "expected resource removed from state")
}

func TestUserResource_Read_ClusterNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no such cluster","code":"NotFound"}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newUserResource(t, client)
	objType, sch := userSchemaOf(t, r)

	prior := userVals{clusterID: "7", name: "app", id: "7:app", userID: "99", accessSet: true}
	req := resource.ReadRequest{State: tfsdk.State{Schema: sch, Raw: userStateValue(objType, prior)}}
	resp := resource.ReadResponse{State: tfsdk.State{Schema: sch, Raw: userStateValue(objType, prior)}}
	r.Read(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)
	assert.True(t, resp.State.Raw.IsNull(), "expected resource removed from state on 404")
}

func TestUserResource_Update_PostsToUserID(t *testing.T) {
	var gotPath, gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		gotMethod = req.Method
		b, _ := io.ReadAll(req.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"99","login":"app","networks":"172.16.0.0/12","databases":"default","id_cluster":"7"}}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newUserResource(t, client)
	objType, sch := userSchemaOf(t, r)

	state := userVals{clusterID: "7", name: "app", networks: "::/0", databases: "default", password: "old", id: "7:app", userID: "99", accessSet: true}
	plan := userVals{clusterID: "7", name: "app", networks: "172.16.0.0/12", databases: "default", password: "new", id: "7:app", userID: "99", accessSet: true}

	req := resource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: sch, Raw: userStateValue(objType, plan)},
		State: tfsdk.State{Schema: sch, Raw: userStateValue(objType, state)},
	}
	resp := resource.UpdateResponse{State: tfsdk.State{Schema: sch, Raw: userStateValue(objType, state)}}
	r.Update(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/cluster/7/user/99", gotPath)
	assert.Contains(t, gotBody, `"networks":["172.16.0.0/12"]`)
	assert.Contains(t, gotBody, `"password":"new"`)

	var model userResourceModel
	require.False(t, resp.State.Get(context.Background(), &model).HasError())
	assert.Equal(t, "99", model.UserID.ValueString())
	assert.Equal(t, "172.16.0.0/12", model.Networks.ValueString())
	// Password preserved from plan.
	assert.Equal(t, "new", model.Password.ValueString())
}

func TestUserResource_Delete_DeletesByUserID(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		gotMethod = req.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newUserResource(t, client)
	objType, sch := userSchemaOf(t, r)

	state := userVals{clusterID: "7", name: "app", id: "7:app", userID: "99", accessSet: true}
	req := resource.DeleteRequest{State: tfsdk.State{Schema: sch, Raw: userStateValue(objType, state)}}
	resp := resource.DeleteResponse{State: tfsdk.State{Schema: sch, Raw: userStateValue(objType, state)}}
	r.Delete(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/cluster/7/user/99", gotPath)
}

func TestUserResource_Delete_AlreadyGoneIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found","code":"NotFound"}`))
	}))
	t.Cleanup(srv.Close)
	client := acm.NewClient(srv.URL, "tok", acm.WithHTTPClient(srv.Client()))

	r := newUserResource(t, client)
	objType, sch := userSchemaOf(t, r)

	state := userVals{clusterID: "7", name: "app", id: "7:app", userID: "99", accessSet: true}
	req := resource.DeleteRequest{State: tfsdk.State{Schema: sch, Raw: userStateValue(objType, state)}}
	resp := resource.DeleteResponse{State: tfsdk.State{Schema: sch, Raw: userStateValue(objType, state)}}
	r.Delete(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "404 on delete should be treated as success: %v", resp.Diagnostics)
}

func TestUserResource_ImportState(t *testing.T) {
	r := newUserResource(t, acm.NewClient("http://unused", "tok"))
	objType, sch := userSchemaOf(t, r)

	req := resource.ImportStateRequest{ID: "7:app"}
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(objType, nil)}}
	r.ImportState(context.Background(), req, &resp)
	require.False(t, resp.Diagnostics.HasError(), "diags: %v", resp.Diagnostics)

	var model userResourceModel
	require.False(t, resp.State.Get(context.Background(), &model).HasError())
	assert.Equal(t, "7", model.ClusterID.ValueString())
	assert.Equal(t, "app", model.Name.ValueString())
	assert.Equal(t, "7:app", model.ID.ValueString())
}

func TestUserResource_ImportState_NameWithColon(t *testing.T) {
	clusterID, name, err := splitUserCompositeID("7:weird:login")
	require.NoError(t, err)
	assert.Equal(t, "7:weird", clusterID)
	assert.Equal(t, "login", name)
}

func TestUserResource_ImportState_Invalid(t *testing.T) {
	r := newUserResource(t, acm.NewClient("http://unused", "tok"))
	objType, sch := userSchemaOf(t, r)

	req := resource.ImportStateRequest{ID: "no-colon"}
	resp := resource.ImportStateResponse{State: tfsdk.State{Schema: sch, Raw: tftypes.NewValue(objType, nil)}}
	r.ImportState(context.Background(), req, &resp)
	require.True(t, resp.Diagnostics.HasError())
}

func TestSplitUserCompositeID_Errors(t *testing.T) {
	for _, id := range []string{"", "nocolon", ":name", "cluster:"} {
		_, _, err := splitUserCompositeID(id)
		require.Error(t, err, "id=%q", id)
	}
}

// TestSplitCSVString locks the bug we hit live: ACM stores multi-value fields
// like `networks` as a newline-separated string and returns them that way in
// /users responses. Re-sending the existing networks without splitting on
// newlines produces a single garbage CIDR that ACM rejects as "Cluster check
// has failed".
func TestSplitCSVString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   \n\t  ", nil},
		{"single value", "default", []string{"default"}},
		{"comma-separated (HCL form)", "default,system", []string{"default", "system"}},
		{"comma with spaces", "default, system , app", []string{"default", "system", "app"}},
		{"newline-separated (ACM response form)", "0.0.0.0/0\n::/0", []string{"0.0.0.0/0", "::/0"}},
		{"mixed", "a,b\nc;d\te", []string{"a", "b", "c", "d", "e"}},
		{"trailing newline", "default\n", []string{"default"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitCSVString(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}
