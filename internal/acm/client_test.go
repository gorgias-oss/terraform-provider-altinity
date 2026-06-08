// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Gorgias/terraform-provider-altinity/internal/acm/wire"
)

// noSleep is a backoff sleeper that returns immediately, keeping retry tests
// fast and deterministic.
func noSleep(_ context.Context, _ time.Duration) error { return nil }

func newTestClient(t *testing.T, h http.HandlerFunc, opts ...Option) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	all := append([]Option{WithHTTPClient(srv.Client()), withSleep(noSleep)}, opts...)
	return NewClient(srv.URL, "test-token", all...), srv
}

func TestRequestHelper_HeadersAndEnvelopeDecode(t *testing.T) {
	var gotAuth, gotAccept, gotContentType, gotMethod, gotPath string
	var gotBody string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get(authHeader)
		gotAccept = r.Header.Get("Accept")
		gotContentType = r.Header.Get("Content-Type")
		gotMethod = r.Method
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		// nodes is an array of node objects in responses (count = 2 here).
		_, _ = w.Write([]byte(`{"data":{"id":7,"name":"c1","nodes":[{"id":1},{"id":2}],"shards":"1","replicas":"1"}}`))
	})

	cluster, err := client.LaunchCluster(context.Background(), "2267", LaunchRequest{Name: "c1", Nodes: "2"})
	require.NoError(t, err)

	assert.Equal(t, "test-token", gotAuth)
	assert.Equal(t, "application/json", gotAccept)
	assert.Equal(t, "application/json", gotContentType)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/environment/2267/clusters/launch", gotPath)
	assert.Contains(t, gotBody, `"name":"c1"`)
	assert.Contains(t, gotBody, `"nodes":"2"`)

	// Envelope "data" was unwrapped and coerced.
	assert.Equal(t, int64(7), cluster.ID)
	assert.Equal(t, "c1", cluster.Name)
	assert.Equal(t, int64(2), cluster.Nodes)
	assert.Equal(t, int64(1), cluster.Shards)
}

func TestRequestHelper_NoBodyOmitsContentType(t *testing.T) {
	var hadContentType bool
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, hadContentType = r.Header["Content-Type"]
		_, _ = w.Write([]byte(`{"data":{"status":"online"}}`))
	})
	_, err := client.GetClusterStatus(context.Background(), 7)
	require.NoError(t, err)
	assert.False(t, hadContentType, "GET with no body must not set Content-Type")
}

func TestErrorParsing_TypedAPIError(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad cluster name","code":"invalid_name"}`))
	})

	_, err := client.GetCluster(context.Background(), 7)
	require.Error(t, err)
	var ae *APIError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, http.StatusBadRequest, ae.StatusCode)
	assert.Equal(t, "invalid_name", ae.Code)
	assert.Equal(t, "bad cluster name", ae.Message)
	assert.Equal(t, "ClusterShow", ae.Operation)
	assert.Contains(t, ae.Error(), "bad cluster name")
}

func TestErrorParsing_NumericCode(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"nope","code":403}`))
	})
	_, err := client.GetCluster(context.Background(), 7)
	var ae *APIError
	require.ErrorAs(t, err, &ae)
	assert.Equal(t, "403", ae.Code)
}

func TestIsNotFound(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	_, err := client.GetCluster(context.Background(), 7)
	assert.True(t, IsNotFound(err))
	assert.False(t, IsTransientHTTPError(err))
}

func TestRetry_On429ThenSuccess(t *testing.T) {
	var calls int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"slow down","code":"rate_limited"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":7}}`))
	})

	c, err := client.GetCluster(context.Background(), 7)
	require.NoError(t, err)
	assert.Equal(t, int64(7), c.ID)
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls), "should retry twice then succeed")
}

func TestRetry_On5xxExhausted(t *testing.T) {
	var calls int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}, WithMaxRetries(2))

	_, err := client.GetCluster(context.Background(), 7)
	require.Error(t, err)
	assert.True(t, IsTransientHTTPError(err))
	// 1 initial + 2 retries = 3 attempts.
	assert.Equal(t, int32(3), atomic.LoadInt32(&calls))
}

func TestRetry_4xxNotRetried(t *testing.T) {
	var calls int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad"}`))
	})
	_, err := client.GetCluster(context.Background(), 7)
	require.Error(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "4xx must be fatal, no retry")
}

func TestResolvePathSubstitution(t *testing.T) {
	var gotPath string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	})
	// Terminate exercises the {terminate} path-param substitution; the value is
	// the int flag "1" (terminate), not the literal word "terminate".
	err := client.TerminateCluster(context.Background(), 12345)
	require.NoError(t, err)
	assert.Equal(t, "/cluster/12345/1", gotPath)
}

func TestBackoffDelayCapped(t *testing.T) {
	assert.Equal(t, retryBaseDelay, backoffDelay(1))
	assert.Equal(t, 2*retryBaseDelay, backoffDelay(2))
	assert.Equal(t, retryMaxDelay, backoffDelay(100), "must cap at retryMaxDelay")
}

func TestContextCancelDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate"}`))
	}))
	t.Cleanup(srv.Close)
	// Real sleeper so context cancellation interrupts the backoff wait.
	client := NewClient(srv.URL, "tok", WithHTTPClient(srv.Client()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled
	_, err := client.GetCluster(ctx, 7)
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "context canceled") || err == context.Canceled)
}

// TestDoJSON_HTTP200WithErrorEnvelope reproduces the bad-launch behavior: ACM
// (PHP) returns HTTP 200 with an error envelope ({"error":...,"code":403})
// instead of a 4xx. The client must treat a populated "error" as a failure,
// classified by the embedded numeric code, rather than decoding a zero-value
// "success" (which previously made LaunchCluster return id 0 with a nil error).
func TestDoJSON_HTTP200WithErrorEnvelope(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"error":"Access denied.","code":403}`))
	})
	_, err := c.GetClusterStatus(context.Background(), 1)
	require.Error(t, err, "HTTP 200 with an error envelope must be an error, not a zero-value success")
	assert.True(t, IsUnauthorized(err), "embedded code 403 must classify as unauthorized, got %v", err)
}

// TestFindClusterInEnv verifies list-based existence detection: a present id is
// found, an absent id is reported missing (not an error) so the caller treats
// it as drift.
func TestFindClusterInEnv(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":42,"name":"c1"},{"id":7,"name":"c2"}]}`))
	})
	cl, found, err := c.FindClusterInEnv(context.Background(), "2267", 42)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, int64(42), cl.ID)

	_, found, err = c.FindClusterInEnv(context.Background(), "2267", 999)
	require.NoError(t, err)
	assert.False(t, found, "absent id must be reported missing, not error")
}

// TestRetry_5xxNotRetriedForPost: a 5xx on a non-idempotent create (POST) must
// surface immediately, not retry — a retried launch hits ACM's per-env lock and
// busy-loops (observed with ClusterLaunch returning 500 then "operation in
// progress").
func TestRetry_5xxNotRetriedForPost(t *testing.T) {
	var calls int32
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}, WithMaxRetries(3))

	_, err := client.LaunchCluster(context.Background(), "2267", LaunchRequest{Name: "x"})
	require.Error(t, err)
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "5xx on a POST create must not be retried")
}

// TestGetClusterAction_DecodesLiveCapture locks the wire shape we infer for
// /cluster/{cluster}/status against a real payload captured from ACM during
// a rescale operation. The OpenAPI spec leaves this endpoint's response
// undefined, so this test is the schema contract. Two captures pinned:
//
//   - In-progress: action is a long descriptive string, actionProgress.percent
//     can read 100 while still mid-operation (must NOT be treated as terminal).
//   - Loose typing: actionProgress.total can be either a JSON int or a
//     string-int — we only decode `percent` (always int), so this passes
//     either way.
func TestGetClusterAction_DecodesLiveCapture(t *testing.T) {
	const liveBody = `{"data":{"health":{"total":7,"passed":7,"ts":"Fri, 05 Jun 2026 20:53:34 +0000","details":[]},"uptime":"4787","queryTimes":{"Select":"2026-06-05 19:36:13","Insert":null},"action":"Wait for host to catch replication lag - START Host/shard/cluster: 2/0/tf-demo","actionProgress":{"total":"3","completed":"3","percent":100},"latestBackup":"","backupInfo":{"version":"altinity/clickhouse-backup:stable"},"throughput":null,"crashReport":0,"usesInstallationTemplate":false}}`
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/cluster/10046/status", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(liveBody))
	})

	a, err := client.GetClusterAction(context.Background(), 10046)
	require.NoError(t, err)
	assert.Equal(t, "Wait for host to catch replication lag - START Host/shard/cluster: 2/0/tf-demo", a.Action,
		"in-progress action is a human-readable string, not an enum")
	assert.Equal(t, 100, a.Progress, "percent decodes as int even when total/completed are strings")
	assert.Equal(t, 7, a.HealthPassed)
	assert.Equal(t, 7, a.HealthTotal)
}

func TestGetClusterAction_DecodesIdleCapture(t *testing.T) {
	const idleBody = `{"data":{"health":{"total":7,"passed":7},"action":"Completed","actionProgress":{"total":2,"completed":"0","percent":0}}}`
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(idleBody))
	})

	a, err := client.GetClusterAction(context.Background(), 10046)
	require.NoError(t, err)
	assert.Equal(t, "Completed", a.Action)
	assert.Equal(t, 0, a.Progress)
}

// TestRedactBody_TopLevelSecrets confirms that the request-body redactor masks
// the explicit secret fields the ACM payloads carry.
func TestRedactBody_TopLevelSecrets(t *testing.T) {
	in := []byte(`{"name":"c1","adminPass":"hunter2","password":"x","datadogSettings":{"apiKey":"y"},"public":"keep"}`)
	out := redactBody(in)
	assert.Contains(t, out, `"name":"c1"`)
	assert.Contains(t, out, `"public":"keep"`)
	assert.NotContains(t, out, "hunter2")
	assert.NotContains(t, out, `"apiKey":"y"`,
		"nested apiKey inside datadogSettings must be masked along with the top-level key")
	assert.Contains(t, out, `"adminPass":"***"`)
}

// TestRedactBody_DeepNestedAndArrays is the response-body case: ACM returns
// arrays of objects (environments list, nodes array) carrying cloud-provider
// secrets at depth. The deep-walk redactor must scrub them all.
func TestRedactBody_DeepNestedAndArrays(t *testing.T) {
	// Synthetic ListEnvironments-shaped response with the secret-bearing fields
	// from wire.Environment and wire.Node.
	in := []byte(`{
		"data": [
			{
				"id": 1,
				"name": "prod",
				"awsKey": "AKIA...",
				"awsSecretKey": "secret-key-value",
				"awsPrivateKey": "-----BEGIN-----",
				"datadogPassword": "dd-pass",
				"kubeToken": "k8s-token",
				"nodes": [
					{"id": 10, "host": "h1", "pass": "node-pass", "sshKey": "ssh-key", "sshPass": "ssh-pass"}
				]
			}
		]
	}`)
	out := redactBody(in)
	// Public fields preserved.
	assert.Contains(t, out, `"name":"prod"`)
	assert.Contains(t, out, `"host":"h1"`)
	// Every secret value gone.
	for _, leak := range []string{
		"AKIA...", "secret-key-value", "-----BEGIN-----",
		"dd-pass", "k8s-token",
		"node-pass", "ssh-key", "ssh-pass",
	} {
		assert.NotContains(t, out, leak, "secret %q must not appear in redacted output", leak)
	}
}

// TestRedactBody_CaseInsensitive verifies the matcher is case-insensitive, so
// future API drift in field casing doesn't silently leak secrets.
func TestRedactBody_CaseInsensitive(t *testing.T) {
	in := []byte(`{"AdminPass":"v1","ADMINPASS":"v2","Password":"v3","SECRET":"v4"}`)
	out := redactBody(in)
	for _, leak := range []string{"v1", "v2", "v3", "v4"} {
		assert.NotContains(t, out, leak)
	}
}

// TestRedactBody_EmptyAndMalformed confirms the helper is safe on edge inputs.
func TestRedactBody_EmptyAndMalformed(t *testing.T) {
	assert.Equal(t, "", redactBody(nil))
	assert.Equal(t, "", redactBody([]byte("")))
	assert.Equal(t, "(non-JSON body omitted)", redactBody([]byte("<html>error</html>")))
}

// TestResolvePath_URLEscapesValues guards the path-injection defense: a value
// containing "/" or ".." must be %-encoded so the resulting URL stays in its
// declared path segment.
func TestResolvePath_URLEscapesValues(t *testing.T) {
	ep := wire.Endpoints[wire.OpClickhouseKeeperStatus] // /environment/{environment}/keeper/{name}/status

	// Sanity: normal value.
	p, err := resolvePath(ep, map[string]string{"environment": "2267", "name": "k1"})
	require.NoError(t, err)
	assert.Equal(t, "/environment/2267/keeper/k1/status", p)

	// Attack: "/" in name must be %2F-encoded, so the path doesn't break out.
	p, err = resolvePath(ep, map[string]string{"environment": "2267", "name": "evil/etc/passwd"})
	require.NoError(t, err)
	assert.NotContains(t, p, "evil/etc/passwd",
		"name with '/' must be escaped, not pass through verbatim")
	assert.Contains(t, p, "evil%2Fetc%2Fpasswd")
}
