// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"net/url"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm/wire"
)

// Version is an available ClickHouse version offered for a cluster
// (GET /cloud/{environment}/options?type=versions). Name carries human labels
// like "ClickHouse LTS", "Altinity Stable", or an "[EOL]" prefix.
type Version struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Repo string `json:"repo"`
}

// CloudOption is a generic {code, name} cloud option (storage classes, regions,
// …) from GET /cloud/{environment}/options?type=<optType>.
type CloudOption struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// ListCloudOptions returns the {code, name} options of a given type for an
// environment. platform (e.g. "kubernetes") should be the environment's type;
// without it ACM may return a stale/empty list.
func (c *Client) ListCloudOptions(ctx context.Context, environmentID, platform, optType string) ([]CloudOption, error) {
	q := url.Values{}
	q.Set("type", optType)
	if platform != "" {
		q.Set("platform", platform)
	}
	var opts []CloudOption
	if err := c.doRequest(ctx, wire.OpCloudOptions, map[string]string{"environment": environmentID}, q, nil, &opts); err != nil {
		return nil, err
	}
	return opts, nil
}

// ListZones returns the availability zones available in an environment
// (GET /cloud/{environment}/options?type=zones). The response is a bare string
// array (not {code,name}), hence a dedicated method.
func (c *Client) ListZones(ctx context.Context, environmentID, platform string) ([]string, error) {
	q := url.Values{}
	q.Set("type", "zones")
	if platform != "" {
		q.Set("platform", platform)
	}
	var zs []string
	if err := c.doRequest(ctx, wire.OpCloudOptions, map[string]string{"environment": environmentID}, q, nil, &zs); err != nil {
		return nil, err
	}
	return zs, nil
}

// ListVersions returns the ClickHouse versions available in an environment.
// platform (e.g. "kubernetes") is required to get the current list — without it
// ACM returns a stale/older default set. Pass the environment's type as the
// platform.
func (c *Client) ListVersions(ctx context.Context, environmentID, platform string) ([]Version, error) {
	q := url.Values{}
	q.Set("type", "versions")
	if platform != "" {
		q.Set("platform", platform)
	}
	var vs []Version
	if err := c.doRequest(ctx, wire.OpCloudOptions, map[string]string{"environment": environmentID}, q, nil, &vs); err != nil {
		return nil, err
	}
	return vs, nil
}
