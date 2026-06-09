// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"strconv"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm/wire"
)

// ProfileSetting is the domain representation of a settings-profile entry
// (ClickHouse SETTINGS PROFILE values). One ProfileSetting binds a single
// {name, value} pair to a parent Profile via id_profile.
//
// Why this resource exists: a profile created via /cluster/{id}/profiles with
// zero attached settings is metadata-only in ACM and never gets pushed to
// ClickHouse's user_directories. Any DbuserAdd referencing the profile then
// fails with Code 180 THERE_IS_NO_PROFILE. Attaching at least one setting via
// /profile/{profile}/settings makes ACM push the profile to the cluster.
type ProfileSetting struct {
	ID        int64
	IDProfile int64
	Name      string
	Value     string
}

// ProfileSettingRequest is the body for POST /profile/{profile}/settings and
// POST /setting/{id} (ProfileSettingsAdd / ProfileSettingsEdit). The spec
// declares only name + value; ACM ignores other fields.
type ProfileSettingRequest struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// ListProfileSettings reads all settings attached to a profile
// (GET /profile/{profile}/settings). Read matches by Name (the config-stable
// key) and carries the ACM-internal setting id for update/delete.
//
// Response envelope quirk: this endpoint returns `{"data": {"settings": [...]}}`
// (nested), NOT `{"data": [...]}` like the other list endpoints. We unmarshal
// into the wrapper struct below and return the inner slice.
func (c *Client) ListProfileSettings(ctx context.Context, profileID int64) ([]ProfileSetting, error) {
	var raw profileSettingsListWrapper
	args := map[string]string{"profile": strconv.FormatInt(profileID, 10)}
	if err := c.doJSON(ctx, wire.OpProfileSettingsList, args, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]ProfileSetting, 0, len(raw.Settings))
	for i := range raw.Settings {
		s, err := profileSettingFromWire(&raw.Settings[i])
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// profileSettingsListWrapper matches ACM's non-standard envelope shape for
// GET /profile/{profile}/settings: `{"data": {"settings": [...]}}` instead of
// the flat `{"data": [...]}` used by every other list endpoint we consume.
type profileSettingsListWrapper struct {
	Settings []wire.ProfileSetting `json:"settings"`
}

// FindProfileSettingByName locates a setting by name within a profile. Setting
// names are unique per profile, so this makes Create idempotent: adopt an
// existing setting of the same name instead of minting a duplicate.
func (c *Client) FindProfileSettingByName(ctx context.Context, profileID int64, name string) (ProfileSetting, bool, error) {
	settings, err := c.ListProfileSettings(ctx, profileID)
	if err != nil {
		return ProfileSetting{}, false, err
	}
	for _, s := range settings {
		if s.Name == name {
			return s, true, nil
		}
	}
	return ProfileSetting{}, false, nil
}

// CreateProfileSetting attaches a setting to a profile
// (POST /profile/{profile}/settings, operationId ProfileSettingsAdd).
func (c *Client) CreateProfileSetting(ctx context.Context, profileID int64, req ProfileSettingRequest) (ProfileSetting, error) {
	var w wire.ProfileSetting
	args := map[string]string{"profile": strconv.FormatInt(profileID, 10)}
	if err := c.doJSON(ctx, wire.OpProfileSettingsAdd, args, req, &w); err != nil {
		return ProfileSetting{}, err
	}
	return profileSettingFromWire(&w)
}

// EditProfileSetting updates a setting's value by its ACM-internal id
// (POST /setting/{id}, operationId ProfileSettingsEdit). Note: the path
// parameter is the SETTING id (returned from Create/List), not the profile id.
func (c *Client) EditProfileSetting(ctx context.Context, settingID int64, req ProfileSettingRequest) (ProfileSetting, error) {
	var w wire.ProfileSetting
	args := map[string]string{"id": strconv.FormatInt(settingID, 10)}
	if err := c.doJSON(ctx, wire.OpProfileSettingsEdit, args, req, &w); err != nil {
		return ProfileSetting{}, err
	}
	return profileSettingFromWire(&w)
}

// DeleteProfileSetting removes a setting from a profile by its ACM-internal id
// (DELETE /setting/{id}, operationId ProfileSettingsRemove). A 404 is a
// *APIError so callers can treat already-deleted entries as drift via
// IsNotFound.
func (c *Client) DeleteProfileSetting(ctx context.Context, settingID int64) error {
	args := map[string]string{"id": strconv.FormatInt(settingID, 10)}
	return c.doJSON(ctx, wire.OpProfileSettingsRemove, args, nil, nil)
}

func profileSettingFromWire(w *wire.ProfileSetting) (ProfileSetting, error) {
	if w == nil {
		return ProfileSetting{}, nil
	}
	id, err := jnToInt64(w.ID)
	if err != nil {
		return ProfileSetting{}, err
	}
	idProfile, err := jnToInt64(w.IDProfile)
	if err != nil {
		return ProfileSetting{}, err
	}
	return ProfileSetting{
		ID:        id,
		IDProfile: idProfile,
		Name:      w.Name,
		Value:     w.Value,
	}, nil
}
