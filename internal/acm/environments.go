// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"

	"github.com/Gorgias/terraform-provider-altinity/internal/acm/wire"
)

// ListEnvironments returns all environments visible to the token
// (GET /environments). Shape is known, so results are coerced to domain types.
func (c *Client) ListEnvironments(ctx context.Context) ([]Environment, error) {
	var raw []wire.Environment
	if err := c.doJSON(ctx, wire.OpEnvironmentList, nil, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Environment, 0, len(raw))
	for i := range raw {
		e, err := environmentFromWire(&raw[i])
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// GetEnvironmentByName resolves a single environment by its name (the
// config-stable key the altinity_environment data source looks up). Returns a
// 404-style not-found APIError when no environment matches, so callers can use
// IsNotFound.
func (c *Client) GetEnvironmentByName(ctx context.Context, name string) (Environment, error) {
	envs, err := c.ListEnvironments(ctx)
	if err != nil {
		return Environment{}, err
	}
	for _, e := range envs {
		if e.Name == name {
			return e, nil
		}
	}
	return Environment{}, &APIError{
		StatusCode: 404,
		Operation:  wire.OpEnvironmentList,
		Message:    "no environment named " + name,
	}
}
