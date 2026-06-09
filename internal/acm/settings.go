// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"strconv"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm/wire"
)

// Cluster `settings` is a bare-object field in the spec (no named
// components.schemas entry — see design §4.1), so there is no generated wire
// model. The Setting domain type (domain.go) and these methods hand-model it.

// SettingRequest is the body for POST /cluster/{cluster}/settings.
//
// TODO(spike): confirm the exact request/response shape for cluster settings
// from a captured /cluster/{cluster}/settings payload. The name/value pair is
// the expected minimum; additional fields are carried via Extra raw passthrough.
type SettingRequest struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// settingWire is the hand-modeled wire shape for a single setting list entry.
// TODO(spike): confirm fields.
type settingWire struct {
	ID    wire.Number `json:"id,omitempty"`
	Name  string      `json:"name,omitempty"`
	Value string      `json:"value,omitempty"`
}

// ListSettings reads all cluster-level settings
// (GET /cluster/{cluster}/settings). Read matches by Name (the config-stable
// key) and carries the ACM-internal id for update/delete (design §5.1).
func (c *Client) ListSettings(ctx context.Context, clusterID int64) ([]Setting, error) {
	var raw []settingWire
	args := map[string]string{"cluster": clusterIDArg(clusterID)}
	if err := c.doJSON(ctx, wire.OpClusterSettingList, args, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Setting, 0, len(raw))
	for i := range raw {
		id, err := jnToInt64(raw[i].ID)
		if err != nil {
			return nil, err
		}
		out = append(out, Setting{ID: id, Name: raw[i].Name, Value: raw[i].Value})
	}
	return out, nil
}

// FindSettingByName locates a setting by name within its cluster via
// ListSettings. Setting names are unique per cluster, so this makes Create
// idempotent: adopt an existing setting of the same name instead of minting a
// duplicate (ACM enforces no uniqueness — same lesson as profiles).
func (c *Client) FindSettingByName(ctx context.Context, clusterID int64, name string) (Setting, bool, error) {
	settings, err := c.ListSettings(ctx, clusterID)
	if err != nil {
		return Setting{}, false, err
	}
	for _, s := range settings {
		if s.Name == name {
			return s, true, nil
		}
	}
	return Setting{}, false, nil
}

// CreateSetting adds/sets a cluster-level setting
// (POST /cluster/{cluster}/settings) and returns the resulting setting.
func (c *Client) CreateSetting(ctx context.Context, clusterID int64, req SettingRequest) (Setting, error) {
	var w settingWire
	args := map[string]string{"cluster": clusterIDArg(clusterID)}
	if err := c.doJSON(ctx, wire.OpClusterSettingAdd, args, req, &w); err != nil {
		return Setting{}, err
	}
	id, err := jnToInt64(w.ID)
	if err != nil {
		return Setting{}, err
	}
	return Setting{ID: id, Name: w.Name, Value: w.Value}, nil
}

// EditSetting updates a setting's value by its ACM-internal id
// (POST /cluster-setting/{id}, operationId ClusterSettingEdit).
func (c *Client) EditSetting(ctx context.Context, settingID int64, req SettingRequest) (Setting, error) {
	var w settingWire
	args := map[string]string{"id": strconv.FormatInt(settingID, 10)}
	if err := c.doJSON(ctx, wire.OpClusterSettingEdit, args, req, &w); err != nil {
		return Setting{}, err
	}
	id, err := jnToInt64(w.ID)
	if err != nil {
		return Setting{}, err
	}
	return Setting{ID: id, Name: w.Name, Value: w.Value}, nil
}

// DeleteSetting removes a cluster-level setting by its ACM-internal id
// (DELETE /cluster-setting/{id}, operationId ClusterSettingRemove). A 404 is a
// *APIError so callers can treat already-deleted settings as drift via
// IsNotFound.
func (c *Client) DeleteSetting(ctx context.Context, settingID int64) error {
	args := map[string]string{"id": strconv.FormatInt(settingID, 10)}
	return c.doJSON(ctx, wire.OpClusterSettingRemove, args, nil, nil)
}
