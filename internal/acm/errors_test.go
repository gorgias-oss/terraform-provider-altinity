// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsUnauthorized(t *testing.T) {
	assert.True(t, IsUnauthorized(&APIError{StatusCode: 401}), "401 is unauthorized")
	assert.True(t, IsUnauthorized(&APIError{StatusCode: 403}), "403 is unauthorized")
	assert.False(t, IsUnauthorized(&APIError{StatusCode: 404}), "404 is not unauthorized")
	assert.False(t, IsUnauthorized(&APIError{StatusCode: 500}), "500 is not unauthorized")
	assert.False(t, IsUnauthorized(errors.New("plain error")), "non-APIError is not unauthorized")
	assert.False(t, IsUnauthorized(nil), "nil is not unauthorized")
}

// TestIsForbidden locks the 403-specific drift signal. Child-resource Reads
// (profile/setting/user) use this to detect a parent-cluster delete: ACM's
// list endpoints return 403 "Access denied" — not 404 — when the cluster
// no longer exists. Must NOT collapse with 401 (genuine token failure).
func TestIsForbidden(t *testing.T) {
	assert.True(t, IsForbidden(&APIError{StatusCode: 403, Message: "Access denied."}), "403 is forbidden")
	assert.False(t, IsForbidden(&APIError{StatusCode: 401}), "401 must not be treated as forbidden (different remediation)")
	assert.False(t, IsForbidden(&APIError{StatusCode: 404}), "404 is not forbidden")
	assert.False(t, IsForbidden(errors.New("plain error")), "non-APIError is not forbidden")
	assert.False(t, IsForbidden(nil), "nil is not forbidden")
}

// TestIsTransientCreateRace locks the two transient-Create failure shapes that
// RetryOnTransientCreateRace absorbs.
func TestIsTransientCreateRace(t *testing.T) {
	// ClickHouse Code 62 SYNTAX_ERROR — what ACM emits when its generator
	// produces malformed SQL because referenced state hasn't propagated.
	assert.True(t, IsTransientCreateRace(&APIError{
		Operation:  "DbuserAdd",
		StatusCode: 200,
		Message:    "Code: 62. DB::Exception: Syntax error: failed at position 211 (end of query): . Expected end of query.",
	}), "Code 62 from DbuserAdd is transient")

	// Half-committed orphan: id=0 from the CreateUser guard. Plain error,
	// not an APIError — the matcher must still catch it.
	assert.True(t, IsTransientCreateRace(errors.New(
		"acm: DbuserAdd returned a user with id=0; the user may have been created — check the ACM UI before re-applying",
	)), "id=0 guard is transient")

	// Code 180 THERE_IS_NO_PROFILE: ACM resolved the profile name correctly
	// (so the SQL is well-formed), but ClickHouse's user_directories hasn't
	// received the profile yet. Live-captured 2026-06-08 from cluster 10091:
	// "There is no settings profile `analytics_ro_profile` in `user
	// directories`."
	assert.True(t, IsTransientCreateRace(&APIError{
		Operation:  "DbuserAdd",
		StatusCode: 200,
		Message:    "Code: 180. DB::Exception: There is no settings profile `analytics_ro_profile` in `user directories`. (THERE_IS_NO_PROFILE)",
	}), "Code 180 THERE_IS_NO_PROFILE is transient (profile push lag)")

	// Code 511 UNKNOWN_ROLE: distributed-DDL propagation race after
	// CREATE USER ON CLUSTER returns but before the user has fanned out
	// to all replicas. Live-captured 2026-06-08 from cluster 10079:
	// "There is no role `analytics_ro` in `user directories`."
	assert.True(t, IsTransientCreateRace(&APIError{
		Operation:  "DbuserAdd",
		StatusCode: 200,
		Message:    "Code: 511. DB::Exception: There is no role `analytics_ro` in `user directories`. (UNKNOWN_ROLE)",
	}), "Code 511 UNKNOWN_ROLE is transient (DDL propagation race)")

	// Code 192 UNKNOWN_USER: same family as 511 — user exists in ACM but
	// not yet on the ClickHouse node servicing the request.
	assert.True(t, IsTransientCreateRace(&APIError{
		Operation:  "DbuserEditSql",
		StatusCode: 200,
		Message:    "Code: 192. DB::Exception: Unknown user: analytics_ro (UNKNOWN_USER)",
	}), "Code 192 UNKNOWN_USER is transient (DDL propagation race)")

	// Generic JSON decode errors are NOT transient: they indicate a real
	// wire-shape mismatch that retries will not fix.
	assert.False(t, IsTransientCreateRace(errors.New(
		"acm: decode DbuserAdd response: json: cannot unmarshal bool into Go value of type wire.DbUser",
	)), "decode errors must fail loudly, not silently retry")

	// The bool-envelope APIError that the client synthesizes when ACM
	// returns `{"data": false}` is tagged with "Code: 62" so it falls into
	// the transient bucket — the actual cause (per ACM audit log) is
	// always a propagation-race SQL failure.
	assert.True(t, IsTransientCreateRace(&APIError{
		Operation:  "DbuserAdd",
		StatusCode: 200,
		Message:    "Code: 62. ACM returned a bare-bool data envelope (SQL execution failed...).",
	}), "synthesized bool-envelope APIError is transient (Code 62 tag)")

	// Negatives.
	assert.False(t, IsTransientCreateRace(&APIError{StatusCode: 401, Message: "unauthorized"}), "401 is not transient")
	assert.False(t, IsTransientCreateRace(&APIError{StatusCode: 200, Message: "Code: 47 NOT_FOUND"}), "non-62 ClickHouse code is not transient")
	assert.False(t, IsTransientCreateRace(errors.New("plain error")), "unrelated plain error is not transient")
	assert.False(t, IsTransientCreateRace(nil), "nil is not transient")
}

func TestIsNotFound_NonStandardShapes(t *testing.T) {
	assert.True(t, IsNotFound(&APIError{StatusCode: 404}), "canonical 404")
	// ClickhouseKeeperDelete returns HTTP 400 + "keeper not found" when the
	// keeper was cascade-deleted with its parent cluster — the second delete
	// attempt must be treated as a no-op so cluster destroy doesn't leave the
	// keeper undeletable in Terraform state.
	assert.True(t, IsNotFound(&APIError{
		StatusCode: 400,
		Message:    `ClickHouse keeper "tf-demo-keeper" not found: keeper not found`,
	}), "ACM keeper-delete 400 with 'not found'")
	// Case-insensitive on the message substring.
	assert.True(t, IsNotFound(&APIError{StatusCode: 400, Message: "Resource Not Found"}), "case-insensitive")

	// Negative cases.
	assert.False(t, IsNotFound(&APIError{StatusCode: 400, Message: "bad request"}), "400 without 'not found'")
	assert.False(t, IsNotFound(&APIError{StatusCode: 500}), "500 is not not-found")
	assert.False(t, IsNotFound(errors.New("plain error")), "non-APIError is not not-found")
	assert.False(t, IsNotFound(nil), "nil is not not-found")
}
