// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListInstanceTypes(t *testing.T) {
	var gotPath, gotQuery string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		http.ServeFile(w, r, "testdata/instance_types.json")
	})

	zones, types, err := client.ListInstanceTypes(context.Background(), "gcp", "us-east1")
	require.NoError(t, err)

	// Keyed on platform (NOT provider) + region + type=*.
	assert.Equal(t, "/cloud/options", gotPath)
	assert.Contains(t, gotQuery, "platform=gcp")
	assert.Contains(t, gotQuery, "region=us-east1")
	assert.Contains(t, gotQuery, "type=%2A") // "*" url-encoded

	assert.Equal(t, []string{"us-east1-b", "us-east1-c", "us-east1-d"}, zones)
	require.NotEmpty(t, types)
	first := types[0]
	assert.NotEmpty(t, first.Name)
	assert.Greater(t, first.CPU, float64(0))
	assert.Greater(t, first.Memory, float64(0)) // "mem" -> Memory
}
