// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm/wire"
)

// nodeTypeRaw decodes a node type response. It embeds the generated wire type
// and adds the fields ACM returns that are NOT part of the OpenAPI NodeType
// schema (so specgen does not emit them): `used` and the `*_alloc` mirrors.
type nodeTypeRaw struct {
	wire.NodeType
	Used        wire.Bool   `json:"used"`
	CPUAlloc    wire.Number `json:"cpu_alloc"`
	MemoryAlloc wire.Number `json:"memory_alloc"`
}

// nodeTypeFromRaw coerces a nodeTypeRaw into the domain NodeType, layering the
// non-schema `used` flag on top of the standard wire coercion.
func nodeTypeFromRaw(r *nodeTypeRaw) (NodeType, error) {
	nt, err := nodeTypeFromWire(&r.NodeType)
	if err != nil {
		return NodeType{}, err
	}
	nt.Used = r.Used.Bool()
	return nt, nil
}

// ListNodeTypes returns the node types of an environment
// (GET /environment/{environment}/nodetypes?withUsed=1). The withUsed flag adds
// the `used` field (whether a cluster currently uses each type); it is otherwise
// harmless, so it is always requested. environmentID is the ACM environment id
// (string form, as it appears in the path).
func (c *Client) ListNodeTypes(ctx context.Context, environmentID string) ([]NodeType, error) {
	var raw []nodeTypeRaw
	args := map[string]string{"environment": environmentID}
	q := url.Values{"withUsed": {"1"}}
	if err := c.doRequest(ctx, wire.OpNodeTypeList, args, q, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]NodeType, 0, len(raw))
	for i := range raw {
		nt, err := nodeTypeFromRaw(&raw[i])
		if err != nil {
			return nil, err
		}
		out = append(out, nt)
	}
	return out, nil
}

// FindNodeTypeByCode locates a node type by (scope, code) within an environment
// — the config-stable key the altinity_node_type resource adopts on create.
func (c *Client) FindNodeTypeByCode(ctx context.Context, environmentID, scope, code string) (NodeType, bool, error) {
	nts, err := c.ListNodeTypes(ctx, environmentID)
	if err != nil {
		return NodeType{}, false, err
	}
	for i := range nts {
		if nts[i].Scope == scope && nts[i].Code == code {
			return nts[i], true, nil
		}
	}
	return NodeType{}, false, nil
}

// CreateNodeType adds a node type to an environment
// (POST /environment/{environment}/nodetypes) and returns the created type.
func (c *Client) CreateNodeType(ctx context.Context, environmentID string, req NodeTypeRequest) (NodeType, error) {
	var r nodeTypeRaw
	args := map[string]string{"environment": environmentID}
	if err := c.doJSON(ctx, wire.OpNodeTypeAdd, args, req, &r); err != nil {
		return NodeType{}, err
	}
	return nodeTypeFromRaw(&r)
}

// EditNodeType updates a node type by its ACM id (POST /nodetype/{id}).
func (c *Client) EditNodeType(ctx context.Context, id int64, req NodeTypeRequest) (NodeType, error) {
	var r nodeTypeRaw
	args := map[string]string{"id": strconv.FormatInt(id, 10)}
	if err := c.doJSON(ctx, wire.OpNodeTypeEdit, args, req, &r); err != nil {
		return NodeType{}, err
	}
	return nodeTypeFromRaw(&r)
}

// RemoveNodeType deletes a node type by its ACM id (DELETE /nodetype/{id}). ACM
// returns {} on success; a 404 is surfaced as an *APIError (IsNotFound), and an
// in-use rejection surfaces as an *APIError the caller turns into a diagnostic.
func (c *Client) RemoveNodeType(ctx context.Context, id int64) error {
	args := map[string]string{"id": strconv.FormatInt(id, 10)}
	return c.doJSON(ctx, wire.OpNodeTypeRemove, args, nil, nil)
}

// Toleration is a Kubernetes toleration as ACM models it on a node type.
type Toleration struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
	Effect   string `json:"effect"`
}

// NodeTypeRequest is the create/edit body. The opaque fields are json.RawMessage
// so they are sent verbatim: the resource sets scope-default tolerations on
// create and passes the current values back unchanged on update (mirroring the
// ACM UI; managing them via Terraform is not supported).
type NodeTypeRequest struct {
	Name         string          `json:"name,omitempty"`
	Scope        string          `json:"scope,omitempty"`
	Code         string          `json:"code,omitempty"`
	CPU          float64         `json:"cpu"`
	Memory       int64           `json:"memory"`
	Capacity     int64           `json:"capacity,omitempty"`
	StorageClass string          `json:"storageClass"`
	IsSpot       bool            `json:"isSpot"`
	Tolerations  json.RawMessage `json:"tolerations,omitempty"`
	NodeSelector json.RawMessage `json:"nodeSelector,omitempty"`
	ExtraSpec    json.RawMessage `json:"extraSpec,omitempty"`
}

// scopeDefaultTolerations returns the tolerations the ACM UI sends for a node
// type of the given scope, so a Terraform-created node type behaves identically
// to a UI-created one. clickhouse/system are live-confirmed (2026-06-10);
// zookeeper is inferred by analogy with clickhouse.
// TODO(spike): confirm the zookeeper-scope default from a UI capture (OQ-1).
func scopeDefaultTolerations(scope string) json.RawMessage {
	switch scope {
	case "clickhouse":
		return json.RawMessage(`[{"key":"dedicated","operator":"Equal","value":"clickhouse","effect":"NoSchedule"}]`)
	case "zookeeper":
		return json.RawMessage(`[{"key":"dedicated","operator":"Equal","value":"zookeeper","effect":"NoSchedule"}]`)
	default: // system (and any unknown scope): no toleration
		return json.RawMessage(`[]`)
	}
}
