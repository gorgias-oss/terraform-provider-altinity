// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"net/url"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm/wire"
)

// ListCloudOptionsGlobal returns the {code, name} options of a given type for a
// cloud provider via the non-environment-scoped GET /cloud/options.
//
// This is the sibling of ListCloudOptions (which is scoped to an existing
// environment). Region discovery happens BEFORE any environment exists — at
// `altinity_environment` plan/apply time — so the env-scoped endpoint cannot be
// used. Live-confirmed (2026-06-09): `?type=regions&provider=aws` returns the
// per-provider region list as the standard {"data":[{code,name}]} envelope
// (the response also carries id==code and a metadata.default we do not consume).
func (c *Client) ListCloudOptionsGlobal(ctx context.Context, provider, optType string) ([]CloudOption, error) {
	q := url.Values{}
	q.Set("type", optType)
	if provider != "" {
		q.Set("provider", provider)
	}
	var opts []CloudOption
	if err := c.doRequest(ctx, wire.OpCloudOptionsGlobal, nil, q, nil, &opts); err != nil {
		return nil, err
	}
	return opts, nil
}
