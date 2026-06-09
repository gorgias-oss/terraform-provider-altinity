// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPollUntilHealthy_ImmediateHealthy(t *testing.T) {
	ctx := context.Background()
	err := PollUntilHealthy(ctx, func(context.Context) (string, error) {
		return "ready", nil
	})
	require.NoError(t, err)
}

// TestPollUntilHealthy_EnvironmentOnline pins the environment Create poll's
// success signal: a ready Altinity.Cloud environment reports status "online"
// (live-confirmed, env 641), which must be treated as terminal-healthy.
func TestPollUntilHealthy_EnvironmentOnline(t *testing.T) {
	ctx := context.Background()
	err := PollUntilHealthy(ctx, func(context.Context) (string, error) {
		return "online", nil
	})
	require.NoError(t, err)
}

func TestPollUntilHealthy_TerminalError(t *testing.T) {
	ctx := context.Background()
	err := PollUntilHealthy(ctx, func(context.Context) (string, error) {
		return "failed", nil
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "terminal-error")
}

func TestPollUntilHealthy_StatusFetchError(t *testing.T) {
	ctx := context.Background()
	want := errors.New("boom")
	err := PollUntilHealthy(ctx, func(context.Context) (string, error) {
		return "", want
	})
	require.ErrorIs(t, err, want)
}

func TestPollUntilHealthy_ContextDeadline(t *testing.T) {
	// Never healthy + a short deadline: the immediate check is not-terminal,
	// then the deadline fires before the 15s tick.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := PollUntilHealthy(ctx, func(context.Context) (string, error) {
		return "provisioning", nil
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// TestPollUntilHealthy_PendingNotTerminal locks the rescue: a cluster
// undergoing a rescale flips cluster.status from "ready" → "pending" → "ready"
// (confirmed live). PollUntilHealthy must not treat "pending" as terminal.
func TestPollUntilHealthy_PendingNotTerminal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := PollUntilHealthy(ctx, func(context.Context) (string, error) {
		return "pending", nil
	})
	require.Error(t, err, "pending must not be treated as terminal-healthy")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestPollUntilGone_Immediate404(t *testing.T) {
	ctx := context.Background()
	err := PollUntilGone(ctx, func(context.Context) error {
		return &APIError{StatusCode: 404}
	})
	require.NoError(t, err)
}

func TestPollUntilGone_FetchError(t *testing.T) {
	ctx := context.Background()
	want := errors.New("transport")
	err := PollUntilGone(ctx, func(context.Context) error { return want })
	require.ErrorIs(t, err, want)
}

func TestPollUntilGoneBy_ImmediateAbsent(t *testing.T) {
	ctx := context.Background()
	// exists=false on the first check => gone, no error, no tick.
	err := PollUntilGoneBy(ctx, func(context.Context) (bool, error) { return false, nil })
	require.NoError(t, err)
}

func TestPollUntilGoneBy_SurfacesError(t *testing.T) {
	ctx := context.Background()
	// A real list error (not a deleted-cluster 403) must surface, not be
	// misread as "gone".
	want := errors.New("list failed")
	err := PollUntilGoneBy(ctx, func(context.Context) (bool, error) { return false, want })
	require.ErrorIs(t, err, want)
}

func busyErr() error { return &APIError{StatusCode: 200, Message: "Another operation is in progress"} }

func TestRetryWhileBusy_PassthroughSuccess(t *testing.T) {
	calls := 0
	err := RetryWhileBusy(context.Background(), func() error { calls++; return nil })
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "success returns immediately, no retry")
}

func TestRetryWhileBusy_PassthroughOtherError(t *testing.T) {
	calls := 0
	want := &APIError{StatusCode: 400, Message: "bad request"}
	err := RetryWhileBusy(context.Background(), func() error { calls++; return want })
	assert.Equal(t, want, err)
	assert.Equal(t, 1, calls, "non-busy error is not retried")
}

func TestRetryWhileBusy_RetriesUntilContextDone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	calls := 0
	err := RetryWhileBusy(ctx, func() error { calls++; return busyErr() })
	require.Error(t, err)
	assert.True(t, IsOperationInProgress(err), "returns the last busy error on ctx done")
	assert.GreaterOrEqual(t, calls, 1)
}

// withFastTransientRetry collapses the production backoff to ~0 for tests.
// Returns a restore func.
func withFastTransientRetry(t *testing.T) func() {
	t.Helper()
	oldBase, oldMax, oldMaxAttempts := transientCreateRaceBaseDelay, transientCreateRaceMaxDelay, transientCreateRaceMaxAttempts
	transientCreateRaceBaseDelay = time.Millisecond
	transientCreateRaceMaxDelay = time.Millisecond
	return func() {
		transientCreateRaceBaseDelay = oldBase
		transientCreateRaceMaxDelay = oldMax
		transientCreateRaceMaxAttempts = oldMaxAttempts
	}
}

func TestRetryOnTransientCreateRace_PassthroughSuccess(t *testing.T) {
	defer withFastTransientRetry(t)()
	calls := 0
	err := RetryOnTransientCreateRace(context.Background(), func() error { calls++; return nil })
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "success returns immediately, no retry")
}

func TestRetryOnTransientCreateRace_PassthroughNonTransient(t *testing.T) {
	defer withFastTransientRetry(t)()
	calls := 0
	want := &APIError{StatusCode: 401, Message: "Unauthorized"}
	err := RetryOnTransientCreateRace(context.Background(), func() error { calls++; return want })
	assert.Equal(t, want, err)
	assert.Equal(t, 1, calls, "non-transient error is not retried")
}

func TestRetryOnTransientCreateRace_RetriesCode62ThenSucceeds(t *testing.T) {
	defer withFastTransientRetry(t)()
	calls := 0
	err := RetryOnTransientCreateRace(context.Background(), func() error {
		calls++
		if calls < 3 {
			return &APIError{
				Operation:  "DbuserAdd",
				StatusCode: 200,
				Message:    "Code: 62. DB::Exception: Syntax error ...",
			}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, 3, calls, "retries until success")
}

func TestRetryOnTransientCreateRace_ExhaustsAttempts(t *testing.T) {
	defer withFastTransientRetry(t)()
	calls := 0
	err := RetryOnTransientCreateRace(context.Background(), func() error {
		calls++
		return &APIError{StatusCode: 200, Message: "Code: 62 SYNTAX_ERROR"}
	})
	require.Error(t, err)
	assert.True(t, IsTransientCreateRace(err), "returns the last transient error after exhausting")
	assert.Equal(t, transientCreateRaceMaxAttempts, calls, "tries exactly max-attempts times")
}

func TestIsOperationInProgress(t *testing.T) {
	assert.True(t, IsOperationInProgress(busyErr()))
	assert.True(t, IsOperationInProgress(&APIError{Message: "Another OPERATION IS IN PROGRESS now"}))
	assert.False(t, IsOperationInProgress(&APIError{Message: "not found"}))
	assert.False(t, IsOperationInProgress(errors.New("x")))
}

func TestPollUntilIdle_ImmediateCompleted(t *testing.T) {
	err := PollUntilIdle(context.Background(), func(context.Context) (ClusterAction, error) {
		return ClusterAction{Action: "Completed", Progress: 0, HealthPassed: 7, HealthTotal: 7}, nil
	})
	require.NoError(t, err)
}

// TestPollUntilIdle_InProgressPercentIs100 reproduces a live capture: during
// a rescale, ACM reports actionProgress.percent=100 while the operation is
// still finishing (e.g. "Wait for host to catch replication lag..."). The
// poll must NOT treat percent=100 as terminal — only Action == "Completed"
// is the idle signal.
func TestPollUntilIdle_InProgressPercentIs100(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := PollUntilIdle(ctx, func(context.Context) (ClusterAction, error) {
		return ClusterAction{
			Action:       "Wait for host to catch replication lag - START Host/shard/cluster: 2/0/tf-demo",
			Progress:     100,
			HealthPassed: 7,
			HealthTotal:  7,
		}, nil
	})
	require.Error(t, err, "percent=100 with non-Completed action must not be treated as terminal")
	assert.True(t, errors.Is(err, context.DeadlineExceeded))
}

func TestPollUntilIdle_FetchError(t *testing.T) {
	want := errors.New("transport boom")
	err := PollUntilIdle(context.Background(), func(context.Context) (ClusterAction, error) {
		return ClusterAction{}, want
	})
	assert.Equal(t, want, err)
}
