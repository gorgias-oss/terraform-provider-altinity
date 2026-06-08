// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Gorgias/terraform-provider-altinity/internal/acm/wire"
)

func TestScalarCoercion_RoundTrips(t *testing.T) {
	// string-int <-> int64
	v, err := stringToInt64("2")
	require.NoError(t, err)
	assert.Equal(t, int64(2), v)
	assert.Equal(t, "2", int64ToString(v))

	// empty -> 0
	v, err = stringToInt64("")
	require.NoError(t, err)
	assert.Equal(t, int64(0), v)

	// wire.Number forms: string-int, real int, bool, null(empty)
	assertJN := func(in string, want int64) {
		var n wire.Number
		require.NoError(t, json.Unmarshal([]byte(in), &n))
		got, err := jnToInt64(n)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	assertJN(`"13"`, 13)
	assertJN(`13`, 13)
	assertJN(`true`, 1)
	assertJN(`false`, 0)
	assertJN(`null`, 0)

	// 0|1 <-> bool
	assert.True(t, intBoolToBool(1))
	assert.False(t, intBoolToBool(0))
	assert.Equal(t, int64(1), boolToIntBool(true))
	assert.Equal(t, int64(0), boolToIntBool(false))
}

func TestClusterFromWire_CoercesKnownScalars(t *testing.T) {
	w := &wire.Cluster{
		ID:              wire.Number{Number: "12345"},
		Name:            "demo",
		Nodes:           json.RawMessage(`[{"id":1},{"id":2}]`),
		Shards:          "1",
		Replicas:        "3",
		IDEnvironment:   wire.Number{Number: "2267"},
		Secure:          wire.Bool{V: true},
		AdminPass:       "secret",
		DatadogSettings: json.RawMessage(`{"enabled":false}`),
	}
	c, err := clusterFromWire(w)
	require.NoError(t, err)
	assert.Equal(t, int64(12345), c.ID)
	assert.Equal(t, int64(2), c.Nodes)
	assert.Equal(t, int64(1), c.Shards)
	assert.Equal(t, int64(3), c.Replicas)
	assert.Equal(t, int64(2267), c.IDEnvironment)
	assert.True(t, c.Secure)
	assert.Equal(t, "secret", c.AdminPass)
	assert.JSONEq(t, `{"enabled":false}`, string(c.DatadogSettings))
}

func TestNilWireCoercion(t *testing.T) {
	c, err := clusterFromWire(nil)
	require.NoError(t, err)
	assert.Zero(t, c)
}
