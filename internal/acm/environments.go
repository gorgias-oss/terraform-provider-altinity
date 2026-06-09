// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"strconv"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm/wire"
)

// EnvironmentRequest is the body for POST /environments/request
// (operationId EnvironmentRequest). CloudProvider selects which *_region field
// ACM reads; the provider layer populates exactly the one matching
// CloudProvider and leaves the rest empty (omitempty). Live-confirmed
// (2026-06-09): a request whose region is placed in the NON-matching field is
// rejected with HTTP 400 "One or more fields invalid: cloud_provider".
type EnvironmentRequest struct {
	Name          string `json:"name"`
	CloudProvider string `json:"cloud_provider"`
	AWSRegion     string `json:"aws_region,omitempty"`
	GCPRegion     string `json:"gcp_region,omitempty"`
	AzureRegion   string `json:"azure_region,omitempty"`
	HcloudRegion  string `json:"hcloud_region,omitempty"`
	// First is undocumented (OQ-2) and omitted by default.
}

// EnvironmentEditRequest carries the only field the altinity_environment
// resource mutates in v1 (POST /environment/{id}, operationId EnvironmentEdit).
// EnvironmentEdit accepts ~50 fields; the resource sends only displayName and
// leaves the rest untouched (omitempty), so an edit never clobbers ACM-managed
// configuration the resource does not model.
type EnvironmentEditRequest struct {
	DisplayName string `json:"displayName,omitempty"`
}

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

// RequestEnvironment requests provisioning of a new Altinity-hosted environment
// (POST /environments/request). ACM provisions asynchronously; the returned
// Environment may carry id=0 if the response omits it (OQ-1) — callers resolve
// the id via GetEnvironmentByName in that case.
func (c *Client) RequestEnvironment(ctx context.Context, req EnvironmentRequest) (Environment, error) {
	var w wire.Environment
	if err := c.doJSON(ctx, wire.OpEnvironmentRequest, nil, req, &w); err != nil {
		return Environment{}, err
	}
	return environmentFromWire(&w)
}

// GetEnvironmentByID reads a single environment by its ACM id
// (GET /environment/{id}). A 404 is surfaced as an *APIError so callers can use
// IsNotFound for drift / delete-confirmation.
func (c *Client) GetEnvironmentByID(ctx context.Context, id int64) (Environment, error) {
	var w wire.Environment
	args := map[string]string{"id": strconv.FormatInt(id, 10)}
	if err := c.doJSON(ctx, wire.OpEnvironmentShow, args, nil, &w); err != nil {
		return Environment{}, err
	}
	return environmentFromWire(&w)
}

// EditEnvironment updates an environment by id (POST /environment/{id}). The
// resource sends only the fields it models (displayName in v1).
func (c *Client) EditEnvironment(ctx context.Context, id int64, req EnvironmentEditRequest) (Environment, error) {
	var w wire.Environment
	args := map[string]string{"id": strconv.FormatInt(id, 10)}
	if err := c.doJSON(ctx, wire.OpEnvironmentEdit, args, req, &w); err != nil {
		return Environment{}, err
	}
	return environmentFromWire(&w)
}
