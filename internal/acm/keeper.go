// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm/wire"
)

// Keeper is the clean domain view of a CH Keeper coordination cluster (wire
// schema CHKeeper). Keepers are identified by name within an environment; there
// is no numeric id.
//
// Note: the ACM keeper write API does NOT accept a `settings` field (launch and
// edit both reject it as "unknown field", in every shape tried), so keeper
// settings are not modeled here.
type Keeper struct {
	Name         string
	InstanceType string
	Image        string
	Ha           bool
	Zones        []string

	// Read-back only. CPU values may be k8s quantities (e.g. "500m"), so they
	// are surfaced as strings.
	CPULimits       string
	CPURequests     string
	ZoneTopologyKey string
}

// KeeperLaunchRequest is the body for POST /environment/{environment}/keepers
// (ClickhouseKeeperLaunch).
//
// `ha` is intentionally omitted from the launch body. ACM auto-determines
// keeper HA from the bound cluster's replica count; explicitly sending
// `ha: false` on launch would either be ignored (best case) or downgrade a
// quorum-needing keeper to single-node (worst case). The schema exposes `ha`
// as Computed-only, so the operator cannot set it — kept off the wire for
// symmetry with KeeperEditRequest. See the Read path (applyKeeperToModel)
// for how the ACM-computed value is brought back into state.
type KeeperLaunchRequest struct {
	Name         string   `json:"name"`
	Zones        []string `json:"zones,omitempty"`
	InstanceType string   `json:"instanceType,omitempty"`
	Image        string   `json:"image,omitempty"`
}

// KeeperEditRequest is the body for POST /environment/{environment}/keeper/{name}
// (ClickhouseKeeperEdit) — the name is the path arg, not a body field.
//
// `ha` is intentionally omitted from this struct: ACM auto-manages keeper HA
// based on the bound cluster's replica count, and sending an explicit value
// on edit either bounces (ACM auto-promotes back if cluster needs HA) or
// downgrades a quorum-needing keeper to single-node. The schema exposes `ha`
// as Computed-only; the operator cannot set it.
type KeeperEditRequest struct {
	Zones        []string `json:"zones,omitempty"`
	InstanceType string   `json:"instanceType,omitempty"`
	Image        string   `json:"image,omitempty"`
}

func keeperFromWire(w *wire.CHKeeper) (Keeper, error) {
	return Keeper{
		Name:            w.Name,
		InstanceType:    w.InstanceType,
		Image:           w.Image,
		Ha:              w.Ha.Bool(),
		Zones:           w.Zones,
		ZoneTopologyKey: w.ZoneTopologyKey,
		CPULimits:       w.CPULimits.Number.String(),
		CPURequests:     w.CPURequests.Number.String(),
	}, nil
}

// LaunchKeeper creates a CH Keeper (POST /environment/{environment}/keepers).
// The caller polls GetKeeperStatus to healthy, then reads it back via
// FindKeeperInEnv.
func (c *Client) LaunchKeeper(ctx context.Context, environmentID string, req KeeperLaunchRequest) error {
	args := map[string]string{"environment": environmentID}
	return c.doJSON(ctx, wire.OpClickhouseKeeperLaunch, args, req, nil)
}

// ListKeepers returns every CH Keeper in an environment
// (GET /environment/{environment}/keepers).
func (c *Client) ListKeepers(ctx context.Context, environmentID string) ([]Keeper, error) {
	var ws []wire.CHKeeper
	args := map[string]string{"environment": environmentID}
	if err := c.doJSON(ctx, wire.OpClickhouseKeeperList, args, nil, &ws); err != nil {
		return nil, err
	}
	out := make([]Keeper, 0, len(ws))
	for i := range ws {
		k, err := keeperFromWire(&ws[i])
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

// FindKeeperInEnv locates a keeper by name within its environment. The bool
// reports whether it exists; absence is drift, not an error (list-based check,
// consistent with cluster reads).
func (c *Client) FindKeeperInEnv(ctx context.Context, environmentID, name string) (Keeper, bool, error) {
	keepers, err := c.ListKeepers(ctx, environmentID)
	if err != nil {
		return Keeper{}, false, err
	}
	for _, k := range keepers {
		if k.Name == name {
			return k, true, nil
		}
	}
	return Keeper{}, false, nil
}

// EditKeeper updates a keeper (POST /environment/{environment}/keeper/{name}).
func (c *Client) EditKeeper(ctx context.Context, environmentID, name string, req KeeperEditRequest) error {
	args := map[string]string{"environment": environmentID, "name": name}
	return c.doJSON(ctx, wire.OpClickhouseKeeperEdit, args, req, nil)
}

// DeleteKeeper removes a keeper (DELETE /environment/{environment}/keeper/{name}).
func (c *Client) DeleteKeeper(ctx context.Context, environmentID, name string) error {
	args := map[string]string{"environment": environmentID, "name": name}
	return c.doJSON(ctx, wire.OpClickhouseKeeperDelete, args, nil, nil)
}

// keeperStatusResponse models GET /environment/{environment}/keeper/{name}/status.
// TODO(spike): confirm the exact status response shape from a captured payload.
type keeperStatusResponse struct {
	Status string `json:"status,omitempty"`
}

// GetKeeperStatus reads a keeper's current status string (for the poll loop).
func (c *Client) GetKeeperStatus(ctx context.Context, environmentID, name string) (string, error) {
	var resp keeperStatusResponse
	args := map[string]string{"environment": environmentID, "name": name}
	if err := c.doJSON(ctx, wire.OpClickhouseKeeperStatus, args, nil, &resp); err != nil {
		return "", err
	}
	return resp.Status, nil
}
