// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"encoding/json"
	"net/url"
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

// DatadogSettings is the environment's Datadog integration config, sent as the
// `datadogSettings` object on EnvironmentEdit. Key is the Datadog API key
// (write-only at the resource layer). Live-confirmed shape (2026-06-10).
type DatadogSettings struct {
	Enabled    bool   `json:"enabled"`
	Key        string `json:"key"`
	Region     string `json:"region,omitempty"`
	Metrics    bool   `json:"metrics"`
	Logs       bool   `json:"logs"`
	TableStats bool   `json:"tableStats"`
}

// MaintenanceWindow is one entry of `maintenanceWindowSchedules`. Days are full
// uppercase weekday names (MONDAY…SUNDAY). Live-confirmed shape (2026-06-10).
type MaintenanceWindow struct {
	Name          string   `json:"name"`
	Enabled       bool     `json:"enabled"`
	Hour          int      `json:"hour"`
	LengthInHours int      `json:"lengthInHours"`
	Days          []string `json:"days"`
}

// EnvironmentEditRequest carries the fields the altinity_environment resource
// mutates (POST /environment/{id}, operationId EnvironmentEdit). EnvironmentEdit
// accepts ~50 fields and merges a minimal patch server-side (live-confirmed:
// displayName-only and maintenanceWindowSchedules-only edits don't clobber the
// rest), so the resource sends only what it manages — all omitempty/nil-pointer,
// so an unmanaged field is never sent.
//
// MaintenanceWindowSchedules is a POINTER so the resource can distinguish
// "unmanaged" (nil → field omitted) from "clear all windows" (non-nil empty
// slice → marshals `[]`); a plain slice's omitempty would drop both.
type EnvironmentEditRequest struct {
	DisplayName                string               `json:"displayName,omitempty"`
	DatadogSettings            *DatadogSettings     `json:"datadogSettings,omitempty"`
	ApplyToClusters            json.RawMessage      `json:"applyToClusters,omitempty"`
	MaintenanceWindowSchedules *[]MaintenanceWindow `json:"maintenanceWindowSchedules,omitempty"`
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

// environmentRaw decodes an EnvironmentShow response. It embeds the generated
// wire type and adds maintenanceWindowSchedules, which is NOT a field on the
// generated wire.Environment (the OpenAPI Environment schema omits it) — the
// same embedding trick as nodeTypeRaw.
type environmentRaw struct {
	wire.Environment
	MaintenanceWindowSchedules json.RawMessage `json:"maintenanceWindowSchedules"`
}

// GetEnvironmentByID reads a single environment by its ACM id
// (GET /environment/{id}). A 404 is surfaced as an *APIError so callers can use
// IsNotFound for drift / delete-confirmation.
func (c *Client) GetEnvironmentByID(ctx context.Context, id int64) (Environment, error) {
	var r environmentRaw
	args := map[string]string{"id": strconv.FormatInt(id, 10)}
	if err := c.doJSON(ctx, wire.OpEnvironmentShow, args, nil, &r); err != nil {
		return Environment{}, err
	}
	env, err := environmentFromWire(&r.Environment)
	if err != nil {
		return Environment{}, err
	}
	// maintenanceWindowSchedules is decoded here (not in environmentFromWire)
	// because it is not part of the wire.Environment struct. NOTE: EnvironmentShow
	// (this GET) returns maintenanceWindowSchedules: null even for an env that has
	// windows (live-confirmed 2026-06); the readable source is the acc-check
	// endpoint — see GetEnvironmentMaintenanceWindows. This decode is kept
	// defensively in case EnvironmentShow ever starts echoing them; in practice it
	// yields nil here.
	if len(r.MaintenanceWindowSchedules) > 0 && string(r.MaintenanceWindowSchedules) != "null" {
		var mws []MaintenanceWindow
		if uerr := json.Unmarshal(r.MaintenanceWindowSchedules, &mws); uerr == nil {
			env.MaintenanceWindows = mws
		}
	}
	return env, nil
}

// acc-check USAGE POLICY — read before reaching for this endpoint elsewhere.
//
// GET /environment/{id}/acc-check (EnvironmentCloudCheck) is a CONNECTIVITY PROBE
// ("checks the connection between Cloud Connector and Cloud Controller"), not a
// config-read endpoint. It happens to return a rich effective-config snapshot
// (node pools, storage classes, log/metric storage, CIDR, AZs, labels, and
// maintenanceWindowSchedules), but treating it as a general read source is unsafe:
// it can be slow / run a live probe, fail with 5xx when the connector is down,
// and return stale/empty data for a freshly-provisioned or disconnected env.
//
// Therefore acc-check is used ONLY for maintenance_windows, and ONLY best-effort
// (a failed read warns and keeps last-known / leaves null — never fails the op).
// Do NOT route identity or required attributes through it: cloud_provider/region
// already come from EnvironmentShow (authoritative, always available). It is a
// candidate only as the backing source for NEW, optional, Computed env facts —
// and those must keep the same best-effort, warn-don't-fail handling.

// accCheckResponse is the subset of GET /environment/{id}/acc-check
// (EnvironmentCloudCheck) the provider consumes. The pointer distinguishes the
// three shapes acc-check can return for the field, which carry different meaning:
//   - field absent / null  -> nil pointer: NOT reported (connector may not have
//     synced) — callers must NOT treat this as "no windows".
//   - []                   -> non-nil, empty: confirmed no windows.
//   - [ ... ]              -> non-nil, populated: the live windows.
type accCheckResponse struct {
	MaintenanceWindowSchedules *[]MaintenanceWindow `json:"maintenanceWindowSchedules"`
}

// GetEnvironmentMaintenanceWindows reads the environment's maintenance windows
// from the acc-check endpoint (GET /environment/{id}/acc-check). This is the only
// readable source: EnvironmentShow returns maintenanceWindowSchedules: null even
// when windows exist. noWait=true asks for the cached connection-check result
// rather than re-probing the connector (the windows are part of the env config,
// so the cached snapshot is sufficient — an assumption inferred from the param
// name; see the usage policy above).
//
// The returned `known` flag is false when acc-check did NOT report the field
// (absent/null) — distinct from a confirmed-empty []. Callers use it to avoid
// blanking managed state on an unreported probe (false drift).
func (c *Client) GetEnvironmentMaintenanceWindows(ctx context.Context, id int64) (windows []MaintenanceWindow, known bool, err error) {
	var r accCheckResponse
	args := map[string]string{"id": strconv.FormatInt(id, 10)}
	q := url.Values{"noWait": {"true"}}
	if err := c.doRequest(ctx, wire.OpEnvironmentCloudCheck, args, q, nil, &r); err != nil {
		return nil, false, err
	}
	if r.MaintenanceWindowSchedules == nil {
		return nil, false, nil // field absent/null: not reported
	}
	return *r.MaintenanceWindowSchedules, true, nil
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
