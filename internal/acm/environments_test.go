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
	assert.Equal(t, "example-env", e.Name)
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

func TestDatadogConfigFromRaw_ObjectAndString(t *testing.T) {
	obj := datadogConfigFromRaw(json.RawMessage(`{"enabled":true,"region":"datadoghq.com","metrics":true,"logs":false,"tableStats":true,"key":"shouldbeignored"}`))
	require.NotNil(t, obj)
	assert.True(t, obj.Enabled)
	assert.Equal(t, "datadoghq.com", obj.Region)
	assert.True(t, obj.Metrics)
	assert.False(t, obj.Logs)
	assert.True(t, obj.TableStats)

	// Stringified object (EnvironmentEdit response form).
	str := datadogConfigFromRaw(json.RawMessage(`"{\"enabled\":true,\"region\":\"datadoghq.eu\",\"metrics\":false,\"logs\":true,\"tableStats\":false}"`))
	require.NotNil(t, str)
	assert.Equal(t, "datadoghq.eu", str.Region)
	assert.True(t, str.Logs)

	assert.Nil(t, datadogConfigFromRaw(nil))
	assert.Nil(t, datadogConfigFromRaw(json.RawMessage(`null`)))
}

func TestGetEnvironmentByID_DecodesObservability(t *testing.T) {
	client := serveFixtureClient(t, "testdata/environment_show_observability.json", nil)
	env, err := client.GetEnvironmentByID(context.Background(), 2293)
	require.NoError(t, err)

	require.NotNil(t, env.Datadog)
	assert.True(t, env.Datadog.Enabled)
	assert.Equal(t, "datadoghq.com", env.Datadog.Region)
	assert.True(t, env.Datadog.Metrics)

	require.Len(t, env.MaintenanceWindows, 1)
	assert.Equal(t, "w1", env.MaintenanceWindows[0].Name)
	assert.Equal(t, 16, env.MaintenanceWindows[0].Hour)
	assert.Equal(t, []string{"FRIDAY", "SATURDAY"}, env.MaintenanceWindows[0].Days)
}

func TestGetEnvironmentByID_NoMaintenanceWindows(t *testing.T) {
	// environment_show.json has a (disabled) datadogSettings but NO
	// maintenanceWindowSchedules — the latter must decode to nil gracefully.
	client := serveFixtureClient(t, "testdata/environment_show.json", nil)
	env, err := client.GetEnvironmentByID(context.Background(), 641)
	require.NoError(t, err)
	assert.Nil(t, env.MaintenanceWindows)
	require.NotNil(t, env.Datadog)
	assert.False(t, env.Datadog.Enabled)
}

func TestEditEnvironment_DatadogAndMaintenance(t *testing.T) {
	var body map[string]any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"2293","status":"online"}}`))
	})

	_, err := client.EditEnvironment(context.Background(), 2293, EnvironmentEditRequest{
		DatadogSettings: &DatadogSettings{Enabled: true, Key: "synthetic-key", Region: "datadoghq.com", Metrics: true, Logs: true, TableStats: true},
		ApplyToClusters: json.RawMessage(`{"datadog":true}`),
		MaintenanceWindowSchedules: &[]MaintenanceWindow{
			{Name: "w1", Enabled: true, Hour: 16, LengthInHours: 4, Days: []string{"FRIDAY"}},
		},
	})
	require.NoError(t, err)

	dd, ok := body["datadogSettings"].(map[string]any)
	require.True(t, ok, "datadogSettings must be a JSON object")
	assert.Equal(t, true, dd["enabled"])
	assert.Equal(t, "synthetic-key", dd["key"])
	assert.Equal(t, true, dd["tableStats"])
	assert.Equal(t, map[string]any{"datadog": true}, body["applyToClusters"])

	mw, ok := body["maintenanceWindowSchedules"].([]any)
	require.True(t, ok, "maintenanceWindowSchedules must be a JSON array")
	require.Len(t, mw, 1)
	assert.Equal(t, float64(16), mw[0].(map[string]any)["hour"])
	assert.Equal(t, float64(4), mw[0].(map[string]any)["lengthInHours"])
}

func TestEditEnvironment_EmptyMaintenanceClears(t *testing.T) {
	var body map[string]any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"2293"}}`))
	})
	// Non-nil empty slice -> field present as [] (clear all); nil pointer would omit.
	empty := []MaintenanceWindow{}
	_, err := client.EditEnvironment(context.Background(), 2293, EnvironmentEditRequest{MaintenanceWindowSchedules: &empty})
	require.NoError(t, err)
	v, present := body["maintenanceWindowSchedules"]
	require.True(t, present, "empty maintenance list must still be sent (clear)")
	assert.Empty(t, v.([]any))
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
