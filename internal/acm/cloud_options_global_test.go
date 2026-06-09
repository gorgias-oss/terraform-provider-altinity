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

func TestListCloudOptionsGlobal_Regions(t *testing.T) {
	body, err := os.ReadFile("testdata/cloud_options_regions_aws.json")
	require.NoError(t, err)

	var gotPath, gotType, gotProvider string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotType = r.URL.Query().Get("type")
		gotProvider = r.URL.Query().Get("provider")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})

	regions, err := client.ListCloudOptionsGlobal(context.Background(), "aws", "regions")
	require.NoError(t, err)

	// Per OQ-3: the non-env-scoped endpoint, keyed on provider, type=regions.
	assert.Equal(t, "/cloud/options", gotPath)
	assert.Equal(t, "regions", gotType)
	assert.Equal(t, "aws", gotProvider)

	require.NotEmpty(t, regions)
	// Fixture is the live AWS region list; spot-check a known entry.
	var found bool
	for _, rg := range regions {
		assert.NotEmpty(t, rg.Code)
		assert.NotEmpty(t, rg.Name)
		if rg.Code == "us-east-1" {
			found = true
			assert.Equal(t, "US East (N. Virginia)", rg.Name)
		}
	}
	assert.True(t, found, "expected us-east-1 in the AWS regions fixture")
}
