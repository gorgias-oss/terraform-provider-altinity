// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"net/url"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm/wire"
)

// InstanceType is an instance shape available in a cloud provider+region,
// from GET /cloud/options?type=*. cpu/mem are real JSON numbers here (unlike
// the string-ints elsewhere in the ACM API). Memory is GiB.
type InstanceType struct {
	Name              string  `json:"name"`
	CPU               float64 `json:"cpu"`
	CPUAllocatable    float64 `json:"cpuAllocatable"`
	Memory            float64 `json:"mem"`
	MemoryAllocatable float64 `json:"memAllocatable"`
}

// instanceCatalog is the nested {zones, instanceTypes} object returned under the
// response envelope's "data" for type=*.
type instanceCatalog struct {
	Zones         []string       `json:"zones"`
	InstanceTypes []InstanceType `json:"instanceTypes"`
}

// ListInstanceTypes returns the availability zones and instance types available
// for a cloud provider in a region (GET /cloud/options?platform=<provider>&
// region=<region>&type=*). This is the catalog operators pick an
// altinity_node_type `code` from.
//
// NOTE: this endpoint keys on `platform=` (live-confirmed), NOT `provider=` like
// ListCloudOptionsGlobal's region lookup — the same operationId, different query
// vocabulary.
func (c *Client) ListInstanceTypes(ctx context.Context, provider, region string) ([]string, []InstanceType, error) {
	q := url.Values{}
	q.Set("type", "*")
	if provider != "" {
		q.Set("platform", provider)
	}
	if region != "" {
		q.Set("region", region)
	}
	var cat instanceCatalog
	if err := c.doRequest(ctx, wire.OpCloudOptionsGlobal, nil, q, nil, &cat); err != nil {
		return nil, nil, err
	}
	return cat.Zones, cat.InstanceTypes, nil
}
