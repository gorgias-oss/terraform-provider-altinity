// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunchKeeper_OmitsHaAndSettings(t *testing.T) {
	var gotBody []byte
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		assert.Equal(t, http.MethodPost, r.Method)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})

	err := c.LaunchKeeper(context.Background(), "2267", KeeperLaunchRequest{
		Name:         "kpr",
		InstanceType: "e2-standard-2",
		Zones:        []string{"a", "b"},
	})
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(gotBody, &body))
	assert.Equal(t, "kpr", body["name"])
	// `ha` must NOT be sent — ACM auto-determines HA from the bound cluster's
	// replica count. Sending `ha: false` would either be ignored (best case)
	// or downgrade a quorum-needing keeper to single-node.
	_, hasHa := body["ha"]
	assert.False(t, hasHa, "ha must not be sent on launch; ACM auto-manages it")
	// `settings` must NOT be sent — the keeper write API rejects it as "unknown field".
	_, hasSettings := body["settings"]
	assert.False(t, hasSettings, "settings must not be sent to the keeper write API")
}

func TestFindKeeperInEnv(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"name":"kpr","instanceType":"e2-standard-2","ha":true,"zones":["a"],"cpuLimits":0},
			{"name":"other","ha":false}
		]}`))
	})

	k, found, err := c.FindKeeperInEnv(context.Background(), "2267", "kpr")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "e2-standard-2", k.InstanceType)
	assert.True(t, k.Ha)
	assert.Equal(t, []string{"a"}, k.Zones)

	_, found, err = c.FindKeeperInEnv(context.Background(), "2267", "missing")
	require.NoError(t, err)
	assert.False(t, found, "absent keeper is drift, not an error")
}
