// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// APIError is a typed error carrying the HTTP status and the ACM error body
// ({"error": "...", "code": "..."}). The provider surfaces .Message as a
// Terraform diagnostic.
type APIError struct {
	// StatusCode is the HTTP status returned by ACM.
	StatusCode int
	// Code is the ACM machine-readable error code (the "code" body field),
	// when present.
	Code string
	// Message is the ACM human-readable error (the "error" body field), when
	// present; otherwise a synthesized fallback.
	Message string
	// Operation is the operationId that produced the error, for context.
	Operation string
}

func (e *APIError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("acm: %s failed (HTTP %d, code %s): %s", e.Operation, e.StatusCode, e.Code, e.Message)
	case e.Message != "":
		return fmt.Sprintf("acm: %s failed (HTTP %d): %s", e.Operation, e.StatusCode, e.Message)
	default:
		return fmt.Sprintf("acm: %s failed (HTTP %d)", e.Operation, e.StatusCode)
	}
}

// IsNotFound reports whether err means "the resource is not there." Used by
// Read (to detect drift / out-of-band deletion) and by Delete (to treat a
// second delete attempt against an already-gone resource as a no-op).
//
// Two shapes match:
//
//  1. HTTP 404 — the canonical case per the OpenAPI spec.
//  2. HTTP 400 with "not found" in the error message — ACM's actual response
//     for at least one resource (ClickhouseKeeperDelete returns HTTP 400 /
//     "ClickHouse keeper \"...\" not found" when the keeper is already gone,
//     even though the spec documents 404). Without this, a cluster destroy
//     that cascades to the keeper would leave the keeper resource visible to
//     Terraform but un-deletable.
func IsNotFound(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		if ae.StatusCode == http.StatusNotFound {
			return true
		}
		if ae.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(ae.Message), "not found") {
			return true
		}
	}
	return false
}

// IsUnauthorized reports whether err is an APIError indicating an
// authentication/authorization failure (HTTP 401 or 403) — used to fail early
// and clearly on a missing, invalid, or revoked ACM API token.
func IsUnauthorized(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden
	}
	return false
}

// IsForbidden reports whether err is an APIError with HTTP 403 specifically.
// Used by child-resource Reads (profile/setting/user) to detect drift when
// the parent cluster has been deleted out-of-band: ACM returns 403 (not 404)
// for list endpoints under a non-existent cluster — `ProfileList`,
// `ClusterSettingList`, `DbuserList` all surface "Access denied" rather than
// "Not found." Distinct from IsUnauthorized so callers do NOT collapse 401
// (genuine token failure — must surface to the operator) with 403 (parent
// gone — drift cleanup).
func IsForbidden(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.StatusCode == http.StatusForbidden
	}
	return false
}

// IsOperationInProgress reports whether err is ACM's per-environment
// serialization lock ("Another operation is in progress"), returned (as HTTP
// 200 with that error message) when a mutation is attempted while another env
// operation — e.g. a keeper or cluster still settling — is running. It clears
// on its own, so callers retry rather than fail.
func IsOperationInProgress(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return strings.Contains(strings.ToLower(ae.Message), "operation is in progress")
	}
	return false
}

// IsTransientCreateRace reports whether err is a transient ACM-side Create
// failure caused by state-propagation timing rather than a real config
// problem. The five live-confirmed failure shapes are unified here because
// they all share a retry contract: the closure should re-run FindByName then
// either Create or Update — propagation finishes on its own.
//
// ClickHouse SQL error families:
//
//  1. Code 62 SYNTAX_ERROR — ACM's synchronous user/profile Create generates
//     SQL by joining cluster state. Right after cluster Create the operator
//     hasn't finished pushing the freshly-created profile to ClickHouse, so
//     ACM's lookup of id_profile → name returns empty and it emits
//     `SETTINGS PROFILE ''`. ClickHouse rejects at end-of-query.
//  2. Code 180 THERE_IS_NO_PROFILE — ACM resolved the name correctly but
//     ClickHouse's user_directories doesn't have the profile yet.
//  3. Code 511 UNKNOWN_ROLE — after CREATE USER ON CLUSTER returns, ACM
//     immediately fires GRANT/REVOKE; the user hasn't fanned out to all
//     replicas yet, so the GRANT hits a node that doesn't have the user.
//  4. Code 192 UNKNOWN_USER — same race as 511, surfaces on update paths.
//
// ACM data-layer race:
//
//  5. id=0 — CreateUser/CreateProfile's half-commit guard. ACM committed
//     the row BEFORE running the ClickHouse SQL; SQL fails, row stays,
//     response has id=0. The synthesized error message contains "id=0"
//     so this matcher catches it. Retry's FindByName picks up the orphan.
//
// Match is case-insensitive on stable substrings. Generic JSON decode
// errors are deliberately NOT matched — those signal a real wire-shape
// mismatch that retries will not fix.
func IsTransientCreateRace(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if isTransientCreateRaceMessage(msg) {
		return true
	}
	// Defensive: also check APIError.Message in case wrapping shape changes.
	var ae *APIError
	if errors.As(err, &ae) {
		return isTransientCreateRaceMessage(strings.ToLower(ae.Message))
	}
	return false
}

func isTransientCreateRaceMessage(msg string) bool {
	switch {
	case strings.Contains(msg, "code: 62"): // SYNTAX_ERROR (ACM-generated malformed SQL)
		return true
	case strings.Contains(msg, "code: 180"): // THERE_IS_NO_PROFILE (profile push lag)
		return true
	case strings.Contains(msg, "code: 511"): // UNKNOWN_ROLE (distributed DDL propagation)
		return true
	case strings.Contains(msg, "code: 192"): // UNKNOWN_USER (distributed DDL propagation)
		return true
	case strings.Contains(msg, "id=0"): // half-committed orphan
		return true
	}
	return false
}

// IsTransientHTTPError reports whether err looks transient at the HTTP layer
// — HTTP 429 (rate limited) or any 5xx — REGARDLESS of request method. This
// is a property of the error, not a retry decision: callers must apply their
// own method-sensitivity. The internal client (see client.retryable) only
// retries 5xx for idempotent GET, since a 5xx on a POST/PUT/DELETE may have
// reached the server and mutated state — re-firing risks a duplicate write.
//
// Use this exported helper when you want to classify an error's HTTP shape
// in test assertions or when reporting diagnostics. Do NOT use it to gate
// retry loops over non-idempotent operations.
func IsTransientHTTPError(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.StatusCode == http.StatusTooManyRequests || (ae.StatusCode >= 500 && ae.StatusCode <= 599)
	}
	return false
}
