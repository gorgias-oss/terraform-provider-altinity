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

func TestListVersions_QueryAndDecode(t *testing.T) {
	var gotQuery string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"code":"25.8.16.10002.altinitystable","name":"25.8 Altinity Stable","repo":"clickhouse/clickhouse-server"},
			{"code":"22.3.15.33","name":"[EOL] 22.3.15.33 ClickHouse LTS"}
		]}`))
	})

	vs, err := c.ListVersions(context.Background(), "2267", "kubernetes")
	require.NoError(t, err)
	assert.Contains(t, gotQuery, "type=versions")
	assert.Contains(t, gotQuery, "platform=kubernetes")
	require.Len(t, vs, 2)
	assert.Equal(t, "25.8.16.10002.altinitystable", vs[0].Code)
	assert.Equal(t, "clickhouse/clickhouse-server", vs[0].Repo)
}
