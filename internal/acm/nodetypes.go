// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"

	"github.com/Gorgias/terraform-provider-altinity/internal/acm/wire"
)

// ListNodeTypes returns the node types available in an environment
// (GET /environment/{environment}/nodetypes). Shape is known, so results are
// coerced to domain types. environmentID is the ACM environment id (string
// form, as it appears in the path).
func (c *Client) ListNodeTypes(ctx context.Context, environmentID string) ([]NodeType, error) {
	var raw []wire.NodeType
	args := map[string]string{"environment": environmentID}
	if err := c.doJSON(ctx, wire.OpNodeTypeList, args, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]NodeType, 0, len(raw))
	for i := range raw {
		nt, err := nodeTypeFromWire(&raw[i])
		if err != nil {
			return nil, err
		}
		out = append(out, nt)
	}
	return out, nil
}
