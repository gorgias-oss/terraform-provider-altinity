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

func TestListNodeTypes_WithUsedDecode(t *testing.T) {
	var gotPath, gotQuery string
	client := func() *Client {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			http.ServeFile(w, r, "testdata/nodetypes_withused.json")
		})
		return c
	}()

	nts, err := client.ListNodeTypes(context.Background(), "2293")
	require.NoError(t, err)
	assert.Equal(t, "/environment/2293/nodetypes", gotPath)
	assert.Contains(t, gotQuery, "withUsed=1")
	require.Len(t, nts, 3)

	// id 14138 is in use.
	var used *NodeType
	for i := range nts {
		if nts[i].ID == 14138 {
			used = &nts[i]
		}
	}
	require.NotNil(t, used)
	assert.True(t, used.Used)
	assert.Equal(t, int64(10), used.Capacity)
	assert.Equal(t, "clickhouse", used.Scope)
	assert.NotEmpty(t, used.Tolerations) // opaque passthrough captured
}

func TestCreateNodeType_ClickhouseSendsScopeDefaultToleration(t *testing.T) {
	var gotPath string
	var body map[string]any
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		http.ServeFile(w, r, "testdata/nodetype_create_response.json")
	})

	nt, err := client.CreateNodeType(context.Background(), "2293", NodeTypeRequest{
		Name:         "c4-standard-24-lssd",
		Scope:        "clickhouse",
		Code:         "c4-standard-24-lssd",
		CPU:          24,
		Memory:       80160,
		Capacity:     10,
		Tolerations:  scopeDefaultTolerations("clickhouse"),
		NodeSelector: json.RawMessage(`""`),
		ExtraSpec:    json.RawMessage(`""`),
	})
	require.NoError(t, err)
	assert.Equal(t, "/environment/2293/nodetypes", gotPath)

	// The scope-default clickhouse toleration must be on the wire.
	tols, ok := body["tolerations"].([]any)
	require.True(t, ok, "tolerations must be a JSON array")
	require.Len(t, tols, 1)
	tol := tols[0].(map[string]any)
	assert.Equal(t, "dedicated", tol["key"])
	assert.Equal(t, "clickhouse", tol["value"])
	assert.Equal(t, "NoSchedule", tol["effect"])

	assert.Equal(t, int64(14140), nt.ID)
	assert.Equal(t, "clickhouse", nt.Scope)
}

func TestScopeDefaultTolerations(t *testing.T) {
	var ck []map[string]string
	require.NoError(t, json.Unmarshal(scopeDefaultTolerations("clickhouse"), &ck))
	require.Len(t, ck, 1)
	assert.Equal(t, "clickhouse", ck[0]["value"])

	var sys []map[string]string
	require.NoError(t, json.Unmarshal(scopeDefaultTolerations("system"), &sys))
	assert.Empty(t, sys)
}

func TestEditNodeType_PreservesOpaqueTolerations(t *testing.T) {
	var body map[string]any
	var gotPath string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"id":"14140","scope":"clickhouse","code":"c3d-highcpu-16","name":"c3d-highcpu-16","cpu":"16","memory":"27380","capacity":"10","isSpot":false}}`))
	})

	preserved := json.RawMessage(`[{"key":"dedicated","operator":"Equal","value":"clickhouse","effect":"NoSchedule"}]`)
	_, err := client.EditNodeType(context.Background(), 14140, NodeTypeRequest{
		Scope:       "clickhouse",
		Code:        "c3d-highcpu-16",
		CPU:         16,
		Memory:      27380,
		Capacity:    10,
		Tolerations: preserved,
	})
	require.NoError(t, err)
	assert.Equal(t, "/nodetype/14140", gotPath)

	tols, ok := body["tolerations"].([]any)
	require.True(t, ok, "preserved tolerations must be sent back verbatim")
	require.Len(t, tols, 1)
	assert.Equal(t, "clickhouse", tols[0].(map[string]any)["value"])
}

func TestRemoveNodeType(t *testing.T) {
	var gotMethod, gotPath string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	err := client.RemoveNodeType(context.Background(), 14139)
	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, "/nodetype/14139", gotPath)
}

func TestFindNodeTypeByCode(t *testing.T) {
	client := func() *Client {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			http.ServeFile(w, r, "testdata/nodetypes_withused.json")
		})
		return c
	}()
	nt, found, err := client.FindNodeTypeByCode(context.Background(), "2293", "clickhouse", "n2d-standard-32")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, int64(14138), nt.ID)

	_, found, err = client.FindNodeTypeByCode(context.Background(), "2293", "clickhouse", "does-not-exist")
	require.NoError(t, err)
	assert.False(t, found)
}
