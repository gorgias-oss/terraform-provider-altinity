// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListUsers_DecodesAndMatchesByLogin(t *testing.T) {
	var gotPath string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":[
			{"id":"11","login":"app","networks":"::/0","databases":"default","id_cluster":"7","id_profile":"3"},
			{"id":12,"login":"ro","accessManagement":false,"id_cluster":7}
		]}`))
	})

	users, err := client.ListUsers(context.Background(), 7)
	require.NoError(t, err)
	assert.Equal(t, "/cluster/7/users", gotPath)
	require.Len(t, users, 2)
	assert.Equal(t, int64(11), users[0].ID)
	assert.Equal(t, "app", users[0].Login)
	assert.Equal(t, int64(7), users[0].IDCluster)
	assert.Equal(t, int64(3), users[0].IDProfile)
	assert.Equal(t, "::/0", users[0].Networks, "string-form networks decode as-is")
	assert.Equal(t, "default", users[0].Databases)
	assert.Equal(t, int64(12), users[1].ID)
}

// TestCreateUser_DecodesArrayNetworks reproduces the DbuserAdd response shape:
// networks/databases are SENT as JSON arrays per the OpenAPI spec, and the API
// echoes them back as arrays. The wire fields are json.RawMessage and the
// domain normalizes them to the operator-facing comma form (previously:
// "cannot unmarshal array into ... networks of type string").
func TestCreateUser_DecodesArrayNetworks(t *testing.T) {
	var sentBody string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sentBody = string(b)
		_, _ = w.Write([]byte(`{"data":{"id":99,"login":"app","networks":["10.0.0.0/8","::/0"],"databases":["default","logs"]}}`))
	})
	u, err := client.CreateUser(context.Background(), 7, UserRequest{
		Login:     "app",
		Networks:  []string{"10.0.0.0/8", "::/0"},
		Databases: []string{"default", "logs"},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(99), u.ID)
	assert.Equal(t, "10.0.0.0/8,::/0", u.Networks, "array networks normalized to comma form")
	assert.Equal(t, "default,logs", u.Databases, "array databases normalized to comma form")
	// The wire must carry JSON arrays — the spec is explicit, and a plain
	// string causes ACM to emit malformed ClickHouse SQL (Code 62 syntax error).
	assert.Contains(t, sentBody, `"networks":["10.0.0.0/8","::/0"]`)
	assert.Contains(t, sentBody, `"databases":["default","logs"]`)
}

func TestCreateUser_SendsPassword(t *testing.T) {
	var body string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = w.Write([]byte(`{"data":{"id":99,"login":"app"}}`))
	})
	u, err := client.CreateUser(context.Background(), 7, UserRequest{Login: "app", Password: "p@ss"})
	require.NoError(t, err)
	assert.Equal(t, int64(99), u.ID)
	assert.Contains(t, body, `"password":"p@ss"`)
	assert.Contains(t, body, `"login":"app"`)
}

func TestUpdateUser_PathByID(t *testing.T) {
	var gotPath string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":{"id":99,"login":"app"}}`))
	})
	// Cluster-scoped edit (DbuserEditSql): the bare /user/{id} variant 404s
	// "Cluster not found" live.
	_, err := client.UpdateUser(context.Background(), 7, 99, UserRequest{Networks: []string{"10.0.0.0/8"}})
	require.NoError(t, err)
	assert.Equal(t, "/cluster/7/user/99", gotPath)
}

func TestDeleteUser_ClusterScopedPath(t *testing.T) {
	var gotPath, gotMethod string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		_, _ = w.Write([]byte(`{"data":{}}`))
	})
	err := client.DeleteUser(context.Background(), 7, 99)
	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/cluster/7/user/99", gotPath)
}

func TestSettingsAndProfiles_RoundTrip(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cluster/7/settings":
			_, _ = w.Write([]byte(`{"data":[{"id":"1","name":"max_threads","value":"8"}]}`))
		case "/cluster/7/profiles":
			_, _ = w.Write([]byte(`{"data":[{"id":"5","name":"default","description":"d","id_cluster":"7"}]}`))
		default:
			http.NotFound(w, r)
		}
	})

	settings, err := client.ListSettings(context.Background(), 7)
	require.NoError(t, err)
	require.Len(t, settings, 1)
	assert.Equal(t, int64(1), settings[0].ID)
	assert.Equal(t, "max_threads", settings[0].Name)
	assert.Equal(t, "8", settings[0].Value)

	profiles, err := client.ListProfiles(context.Background(), 7)
	require.NoError(t, err)
	require.Len(t, profiles, 1)
	assert.Equal(t, int64(5), profiles[0].ID)
	assert.Equal(t, "default", profiles[0].Name)
	assert.Equal(t, int64(7), profiles[0].IDCluster)
}
