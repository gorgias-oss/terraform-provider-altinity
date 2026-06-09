// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"fmt"
	"strconv"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm/wire"
)

// ProfileRequest is the body for POST /cluster/{cluster}/profiles. The request
// schema is inline in the spec; hand-modeled from the confirmed fields.
type ProfileRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// ListProfiles reads all settings profiles for a cluster
// (GET /cluster/{cluster}/profiles). Read matches by Name (design §5.1).
func (c *Client) ListProfiles(ctx context.Context, clusterID int64) ([]Profile, error) {
	var raw []wire.Profile
	args := map[string]string{"cluster": clusterIDArg(clusterID)}
	if err := c.doJSON(ctx, wire.OpProfileList, args, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Profile, 0, len(raw))
	for i := range raw {
		p, err := profileFromWire(&raw[i])
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// FindProfileByName locates a profile by name within its cluster via
// ListProfiles. Profile names are unique per cluster, so this makes Create
// idempotent: adopt an existing profile of the same name instead of minting a
// duplicate (ACM has no uniqueness constraint, so an unconditional create
// silently accumulates duplicates). The bool reports whether it exists.
func (c *Client) FindProfileByName(ctx context.Context, clusterID int64, name string) (Profile, bool, error) {
	profiles, err := c.ListProfiles(ctx, clusterID)
	if err != nil {
		return Profile{}, false, err
	}
	for _, p := range profiles {
		if p.Name == name {
			return p, true, nil
		}
	}
	return Profile{}, false, nil
}

// CreateProfile adds a settings profile to a cluster
// (POST /cluster/{cluster}/profiles) and returns the created profile.
//
// Guards against ACM's half-commit pattern: in some propagation-race states
// ACM commits the profile row to its DB but the response carries id=0. Without
// this guard, our resource state would persist `profile_id="0"` and every
// downstream operation (e.g. ProfileSettingsList) would hit "Cluster not
// found" forever because /profile/0/{anything} returns 404 by design.
// Surface as a transient-SQL error so RetryOnTransientCreateRace retries.
func (c *Client) CreateProfile(ctx context.Context, clusterID int64, req ProfileRequest) (Profile, error) {
	var w wire.Profile
	args := map[string]string{"cluster": clusterIDArg(clusterID)}
	if err := c.doJSON(ctx, wire.OpProfileAdd, args, req, &w); err != nil {
		return Profile{}, err
	}
	p, err := profileFromWire(&w)
	if err != nil {
		return Profile{}, err
	}
	if p.ID == 0 {
		return Profile{}, fmt.Errorf("acm: ProfileAdd returned a profile with id=0; the profile may have been committed in ACM's DB but the response was incomplete — check the ACM UI before re-applying")
	}
	return p, nil
}

// EditProfile updates a profile's name/description by its ACM-internal id
// (POST /profile/{id}, operationId ProfileEdit).
func (c *Client) EditProfile(ctx context.Context, profileID int64, req ProfileRequest) (Profile, error) {
	var w wire.Profile
	args := map[string]string{"id": strconv.FormatInt(profileID, 10)}
	if err := c.doJSON(ctx, wire.OpProfileEdit, args, req, &w); err != nil {
		return Profile{}, err
	}
	return profileFromWire(&w)
}

// DeleteProfile removes a profile by its ACM-internal id
// (DELETE /profile/{id}, operationId ProfileRemove). A 404 is surfaced as a
// *APIError so callers can treat already-deleted profiles as drift via
// IsNotFound.
func (c *Client) DeleteProfile(ctx context.Context, profileID int64) error {
	args := map[string]string{"id": strconv.FormatInt(profileID, 10)}
	return c.doJSON(ctx, wire.OpProfileRemove, args, nil, nil)
}
