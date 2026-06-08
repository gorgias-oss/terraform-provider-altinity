// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
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
