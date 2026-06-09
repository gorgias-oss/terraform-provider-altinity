// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// PollInterval is the fixed cadence for status polling. The design mandates a
// fixed 15s interval with no backoff; the only deadline is the caller's context
// (driven by the Terraform `timeouts` block).
const PollInterval = 15 * time.Second

// Lifecycle status strings. The Keeper set is spike-confirmed (the
// /keeper/{name}/status endpoint reports "pending" then "ready"). The cluster
// running statuses ("online"/"running") are included from the cluster status
// field; matching is case-insensitive over this small known set.
// TODO(spike): confirm the cluster /cluster/{id}/status terminal strings and
// the full error-status set from a successful cluster launch.
//
// Environments (altinity_environment Create poll, GET /environment/{id}.status):
// a ready environment reports status "online" (live-confirmed 2026-06-09, env
// 641), already covered by healthyStatuses below — so PollUntilHealthy works for
// environments unchanged. TODO(spike): capture the in-flight provisioning string
// and the terminal-error string from a real EnvironmentRequest and add the
// error string to errorStatuses (until then a failed provision is bounded only
// by the create timeout, which the resumable Create recovers from).
var (
	healthyStatuses = map[string]bool{
		"ready":     true, // keeper terminal-healthy (spike-confirmed)
		"online":    true, // cluster running
		"running":   true,
		"healthy":   true,
		"completed": true,
	}
	errorStatuses = map[string]bool{
		"failed":     true,
		"error":      true,
		"terminated": true,
	}
)

func normalizeStatus(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// terminalHealthy reports whether s is a terminal-healthy status.
func terminalHealthy(s string) bool { return healthyStatuses[normalizeStatus(s)] }

// terminalError reports whether s is a terminal-error status that should abort
// the poll with a diagnostic.
func terminalError(s string) bool { return errorStatuses[normalizeStatus(s)] }

// transientCreateRaceBaseDelay / transientCreateRaceMaxDelay / transientCreateRaceMaxAttempts
// govern RetryOnTransientCreateRace's exponential backoff
// (5s → 10s → 20s → 40s → 60s × 8 = ~555s ≈ 9 min).
//
// The user-Create flow against a fresh cluster hits TWO consecutive
// propagation races, observed back-to-back (2026-06-08, clusters 10076/79/82):
//
//   - Code 62 SYNTAX_ERROR for several minutes while ACM's operator pushes
//     the just-created profile to ClickHouse (`SETTINGS PROFILE ''` in the
//     audit log until the push lands; the API surfaces either the literal
//     error envelope OR a bare `{"data": false}` envelope).
//   - Then Code 511 UNKNOWN_ROLE for tens of seconds while the
//     CREATE USER ON CLUSTER distributed-DDL fans out to all replicas
//     before ACM fires the immediate follow-up GRANT.
//
// The budget MUST cover both stacked with margin. Operators with a
// stricter SLA can lower it via the create timeout block.
//
// `var` not `const` so tests can collapse them.
var (
	transientCreateRaceBaseDelay   = 5 * time.Second
	transientCreateRaceMaxDelay    = 60 * time.Second
	transientCreateRaceMaxAttempts = 12
)

// RetryOnTransientCreateRace runs op, retrying with exponential backoff on a
// transient ClickHouse SQL failure (IsTransientCreateRace — Code: 62 SYNTAX_ERROR
// from ACM-generated malformed SQL right after a cluster Create/rescale,
// when the operator hasn't finished propagating referenced state).
//
// Backoff doubles each attempt starting at transientCreateRaceBaseDelay, capped at
// transientCreateRaceMaxDelay. Total attempts = transientCreateRaceMaxAttempts (initial +
// retries). Non-transient errors and successes return immediately. Bounded
// by ctx; the last transient error is returned on deadline/cancellation.
//
// Use this for downstream Create paths (user / profile / setting) that ACM
// fulfills by running synchronously-generated SQL. The cluster Create's
// postCreateSettleDelay covers the common case; this retry handles the
// long-tail race when the settle isn't enough.
func RetryOnTransientCreateRace(ctx context.Context, op func() error) error {
	delay := transientCreateRaceBaseDelay
	var lastErr error
	for attempt := 1; attempt <= transientCreateRaceMaxAttempts; attempt++ {
		err := op()
		if err == nil || !IsTransientCreateRace(err) {
			return err
		}
		lastErr = err
		tflog.Debug(ctx, "acm transient SQL error; waiting to retry", map[string]any{
			"attempt":      attempt,
			"max_attempts": transientCreateRaceMaxAttempts,
			"retry_in":     delay.String(),
			"acm_message":  err.Error(),
		})
		// Surface a periodic INFO so operators without TF_LOG=DEBUG see we
		// are still retrying through a propagation race rather than
		// silently spinning. Cadence matches RetryWhileBusy (every 4th
		// attempt) — same operator-facing UX, different underlying race.
		if attempt%4 == 0 {
			tflog.Info(ctx, "acm operator state still propagating; retrying", map[string]any{
				"attempt":      attempt,
				"max_attempts": transientCreateRaceMaxAttempts,
				"retry_in":     delay.String(),
			})
		}
		if attempt == transientCreateRaceMaxAttempts {
			break
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return lastErr
		case <-timer.C:
		}
		delay *= 2
		if delay > transientCreateRaceMaxDelay {
			delay = transientCreateRaceMaxDelay
		}
	}
	return lastErr
}

// RetryWhileBusy runs op, retrying on PollInterval whenever it fails with ACM's
// per-environment "operation is in progress" lock (IsOperationInProgress).
// Other errors (and success) return immediately. It is bounded by ctx: on
// deadline/cancellation it returns the last busy error (or ctx.Err() if op was
// never attempted within the deadline). This serializes env mutations behind a
// concurrent keeper/cluster operation without failing the apply.
func RetryWhileBusy(ctx context.Context, op func() error) error {
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil || !IsOperationInProgress(err) {
			return err
		}
		tflog.Debug(ctx, "acm environment busy; waiting to retry", map[string]any{
			"attempt":     attempt,
			"retry_in":    PollInterval.String(),
			"acm_message": err.Error(),
		})
		// Surface a periodic INFO so operators without TF_LOG=DEBUG can see we
		// are still making progress (every 4 attempts ≈ once per minute at the
		// 15s cadence).
		if attempt%4 == 0 {
			tflog.Info(ctx, "acm environment busy, still retrying", map[string]any{
				"attempt": attempt,
				"elapsed": (time.Duration(attempt) * PollInterval).String(),
			})
		}
		timer := time.NewTimer(PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
	}
}

// pollUntilDone runs check on a fixed PollInterval until it returns done=true,
// the check returns an error, or ctx is cancelled. It performs an immediate
// check before the first tick so an already-done state returns without waiting.
//
// This is the shared body of every PollUntil* helper in this file — extracted
// so the ticker/select/defer-Stop pattern lives once.
func pollUntilDone(ctx context.Context, check func() (done bool, err error)) error {
	if done, err := check(); err != nil || done {
		return err
	}
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if done, err := check(); err != nil || done {
				return err
			}
		}
	}
}

// StatusFunc fetches the current status string for the polled resource.
type StatusFunc func(ctx context.Context) (string, error)

// PollUntilHealthy polls status on a fixed PollInterval until it reaches a
// terminal-healthy state, a terminal-error state (returned as an error), or the
// context deadline/cancellation fires. It polls immediately, then on each tick.
//
// TODO(spike): terminal status detection uses placeholder constants; see the
// status constants above.
func PollUntilHealthy(ctx context.Context, status StatusFunc) error {
	return pollUntilDone(ctx, func() (bool, error) {
		s, err := status(ctx)
		if err != nil {
			// 404 during delete polling is handled by the caller via a
			// dedicated loop; here a fetch error is surfaced.
			return false, err
		}
		tflog.Debug(ctx, "acm poll status", map[string]any{
			"status":  s,
			"healthy": terminalHealthy(s),
			"error":   terminalError(s),
		})
		if terminalError(s) {
			return false, fmt.Errorf("acm: cluster entered terminal-error status %q", s)
		}
		return terminalHealthy(s), nil
	})
}

// idleAction is the terminal-idle value of ClusterAction.Action observed on
// /cluster/{cluster}/status. Captured live: ACM reports "Completed" when no
// operation is running. In-progress states are human-readable descriptive
// strings (e.g. "Wait for host to catch replication lag - START Host/shard/
// cluster: 2/0/tf-demo"), NOT a fixed enum — so we can only treat the exact
// "Completed" string as terminal-idle.
//
// Notable: ActionProgress.Percent CANNOT be used as a terminal signal. A
// live capture showed percent=100 while the operation was still in flight
// (action was still mid-step). Always gate on Action.
//
// TODO(spike): document the terminal-error strings (e.g. "Failed"?) so
// PollUntilIdle can surface errors instead of waiting for context deadline.
const idleAction = "Completed"

// ActionFetcher fetches the current operation-level status for the polled
// cluster from /cluster/{cluster}/status (use acm.Client.GetClusterAction).
type ActionFetcher func(ctx context.Context) (ClusterAction, error)

// PollUntilIdle polls /cluster/{cluster}/status until the cluster reports no
// operation in progress (Action == "Completed"). Used after every mutating
// Update step (rescale / upgrade / backup / admin_password) and as a
// belt-and-suspenders check after Create's PollUntilHealthy — the top-level
// cluster.status field stays "online" during operations, so it is NOT a
// reliable signal that a long-running operation has actually finished.
//
// The poll surfaces transport/auth errors from the fetch. Two terminal-OK
// conditions must BOTH hold:
//
//   - Action == "Completed" — no operation in progress.
//   - HealthPassed == HealthTotal (with HealthTotal > 0) — all of ACM's
//     post-operation health checks pass (cluster endpoints reachable,
//     distributed-query check, ZK availability, replica readiness, etc.).
//     Live observation: right after a fresh cluster Create, action flips
//     to "Completed" several seconds BEFORE health.passed reaches
//     health.total. Returning early causes downstream Creates
//     (DbuserAdd / ProfileAdd) to hit Code 62 SYNTAX_ERROR because ACM
//     generates SQL referencing not-yet-propagated cluster state.
//
// Any other value is treated as in-progress and the loop continues until
// the caller's context fires (the timeouts block bounds this). Every
// fourth poll (every minute at the 15s PollInterval) we emit an Info log
// so the operator sees progress without TF_LOG=DEBUG.
func PollUntilIdle(ctx context.Context, fetch ActionFetcher) error {
	attempt := 0
	return pollUntilDone(ctx, func() (bool, error) {
		attempt++
		a, err := fetch(ctx)
		if err != nil {
			return false, err
		}
		tflog.Debug(ctx, "acm poll action", map[string]any{
			"action":        a.Action,
			"percent":       a.Progress,
			"health_passed": a.HealthPassed,
			"health_total":  a.HealthTotal,
		})
		healthOK := a.HealthTotal > 0 && a.HealthPassed >= a.HealthTotal
		if a.Action == idleAction && healthOK {
			return true, nil
		}
		if attempt%4 == 0 {
			tflog.Info(ctx, "acm operation still in progress", map[string]any{
				"action":        a.Action,
				"percent":       a.Progress,
				"health_passed": a.HealthPassed,
				"health_total":  a.HealthTotal,
				"elapsed":       (time.Duration(attempt) * PollInterval).String(),
			})
		}
		return false, nil
	})
}

// PollUntilGoneBy polls until exists reports false, used by Delete to confirm
// termination via a list-based existence check. Prefer this over PollUntilGone
// for clusters: ACM returns 403 (not 404) for a per-id GET of a deleted cluster,
// so a GetCluster-based poll cannot tell "gone" from "access denied" and aborts
// with a spurious "Cluster did not terminate" error. Listing the environment
// (FindClusterInEnv) is unambiguous — absence from the list means gone.
func PollUntilGoneBy(ctx context.Context, exists func(ctx context.Context) (bool, error)) error {
	return pollUntilDone(ctx, func() (bool, error) {
		present, err := exists(ctx)
		if err != nil {
			return false, err
		}
		return !present, nil
	})
}

// PollUntilGone polls until the resource fetch reports not-found (HTTP 404),
// used by Delete to confirm termination. A nil error from fetch means the
// resource still exists; an IsNotFound error means it is gone (success).
func PollUntilGone(ctx context.Context, fetch func(ctx context.Context) error) error {
	return pollUntilDone(ctx, func() (bool, error) {
		err := fetch(ctx)
		if err == nil {
			return false, nil
		}
		if IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}
