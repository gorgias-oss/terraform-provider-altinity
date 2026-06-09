// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serveFixture returns a client whose handler replays the given testdata file
// at any path (verifying the requested path separately when needed).
func serveFixtureClient(t *testing.T, fixturePath string, wantPath *string) *Client {
	t.Helper()
	body, err := os.ReadFile(fixturePath)
	require.NoError(t, err)
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if wantPath != nil {
			*wantPath = r.URL.Path
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	return client
}

func TestListEnvironments_DecodesFixture(t *testing.T) {
	var gotPath string
	client := serveFixtureClient(t, "testdata/environments.json", &gotPath)

	envs, err := client.ListEnvironments(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "/environments", gotPath)
	require.Len(t, envs, 1)

	e := envs[0]
	assert.Equal(t, int64(1), e.ID)
	assert.Equal(t, "example-env", e.Name)
	assert.Equal(t, "kubernetes", e.Type)
	assert.Equal(t, "online", e.Status)
	assert.Equal(t, "example-env.altinity.cloud", e.Domain)
}

func TestGetEnvironmentByName(t *testing.T) {
	client := serveFixtureClient(t, "testdata/environments.json", nil)

	e, err := client.GetEnvironmentByName(context.Background(), "example-env")
	require.NoError(t, err)
	assert.Equal(t, int64(1), e.ID)

	_, err = client.GetEnvironmentByName(context.Background(), "does-not-exist")
	require.Error(t, err)
	assert.True(t, IsNotFound(err))
}

func TestRequestEnvironment(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"700","name":"tf-test-env","status":"provisioning","type":"kubernetes"}}`))
	})

	env, err := client.RequestEnvironment(context.Background(), EnvironmentRequest{
		Name:          "tf-test-env",
		CloudProvider: "gcp",
		GCPRegion:     "us-east1",
	})
	require.NoError(t, err)

	assert.Equal(t, "/environments/request", gotPath)
	assert.Equal(t, http.MethodPost, gotMethod)
	// The matching region field is sent (OQ-4 hypothesis); the other *_region
	// fields are omitted via omitempty.
	assert.Equal(t, "tf-test-env", gotBody["name"])
	assert.Equal(t, "gcp", gotBody["cloud_provider"])
	assert.Equal(t, "us-east1", gotBody["gcp_region"])
	_, hasAWS := gotBody["aws_region"]
	assert.False(t, hasAWS, "non-matching region fields must be omitted")

	assert.Equal(t, int64(700), env.ID)
	assert.Equal(t, "tf-test-env", env.Name)
	assert.Equal(t, "provisioning", env.Status)
}

func TestGetEnvironmentByID_DecodesFixture(t *testing.T) {
	var gotPath string
	client := serveFixtureClient(t, "testdata/environment_show.json", &gotPath)

	e, err := client.GetEnvironmentByID(context.Background(), 641)
	require.NoError(t, err)
	assert.Equal(t, "/environment/641", gotPath)
	assert.Equal(t, int64(641), e.ID)
	assert.Equal(t, "gorgias-prod-aus-se1-fcb9", e.Name)
	assert.Equal(t, "kubernetes", e.Type)
	assert.Equal(t, "online", e.Status)
}

func TestEditEnvironment(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string]any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"641","name":"x","displayName":"New Name"}}`))
	})

	e, err := client.EditEnvironment(context.Background(), 641, EnvironmentEditRequest{DisplayName: "New Name"})
	require.NoError(t, err)
	assert.Equal(t, "/environment/641", gotPath)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "New Name", gotBody["displayName"])
	assert.Equal(t, "New Name", e.DisplayName)
}

func TestRemoveEnvironment_NotFoundIsClassified(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/environment/641", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Not found","code":404}`))
	})

	err := client.RemoveEnvironment(context.Background(), 641)
	require.Error(t, err)
	assert.True(t, IsNotFound(err))
}

func TestListNodeTypes_DecodesFixture(t *testing.T) {
	var gotPath string
	client := serveFixtureClient(t, "testdata/nodetypes.json", &gotPath)

	nts, err := client.ListNodeTypes(context.Background(), "2267")
	require.NoError(t, err)
	assert.Equal(t, "/environment/2267/nodetypes", gotPath)
	require.NotEmpty(t, nts)

	// First node type is the smallest clickhouse-scope type.
	var found bool
	for _, nt := range nts {
		if nt.Code == "n2d-standard-2" && nt.Scope == "clickhouse" {
			found = true
			assert.Equal(t, float64(2), nt.CPU)
			assert.Equal(t, int64(6001), nt.Memory)
			assert.Equal(t, int64(10), nt.Capacity)
			assert.Equal(t, int64(2267), nt.IDEnvironment)
			assert.False(t, nt.IsSpot)
		}
	}
	assert.True(t, found, "expected n2d-standard-2 clickhouse node type in fixture")
}
