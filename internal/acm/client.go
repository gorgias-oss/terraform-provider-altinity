// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

// Package acm is a hand-written, typed REST client for the Altinity Cloud
// Manager (ACM) API. It is split into a generated wire sub-package
// (internal/acm/wire) holding faithful loose-typed structs + the endpoint
// registry, and this hand-written domain layer which coerces loose scalars
// into clean Go types and exposes one method per endpoint group.
//
// The client never touches the network in tests: every request goes through
// the standard *http.Client, which tests replace with an httptest.Server.
package acm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/gorgias-oss/terraform-provider-altinity/internal/acm/wire"
)

// sensitiveBodyKeys are JSON keys whose values are masked in debug logs,
// regardless of nesting depth. Matched case-insensitively. Covers every
// secret-bearing field across requests and responses (wire.Cluster.AdminPass,
// wire.Environment.AWSSecretKey, wire.Node.SSHPass, wire.DbUser.Password, etc.).
// The X-Auth-Token header is never logged.
var sensitiveBodyKeys = map[string]bool{
	// Cluster / user secrets
	"adminpass":        true,
	"adminpassword":    true,
	"password":         true,
	"altinitypassword": true,
	// Datadog
	"datadogsettings": true,
	"datadogpassword": true,
	"apikey":          true,
	// Opaque passthrough blobs the provider schema marks Sensitive because
	// they may carry credentials (object-storage keys, signed URLs, ...).
	// Masked wholesale, like datadogsettings — they are config-authoritative
	// passthroughs, so masking loses nothing diagnostically.
	"backupoptions":      true,
	"uptimesettings":     true,
	"alternateendpoints": true,
	// AWS / cloud-provider keys carried in Environment payloads
	"awskey":         true,
	"awssecretkey":   true,
	"awsprivatekey":  true,
	"awskeypairname": true,
	// Kubernetes
	"kubetoken": true,
	// Node-level SSH / login credentials
	"pass":    true,
	"sshkey":  true,
	"sshpass": true,
	// Generic
	"token":  true,
	"secret": true,
}

// sensitiveKeySubstrings is the fallback for keys NOT in the exact-match list:
// any key containing one of these (case-insensitive) is masked. This catches
// realistic credential key names inside opaque payloads (secretAccessKey,
// s3SecretKey, bearerToken, clientSecret, ...) and future API fields the exact
// list hasn't caught up with. Over-masking harmless keys (e.g. awsKeyPairName,
// zoneTopologyKey) in DEBUG logs is the accepted trade-off — these logs exist
// for request-shape debugging, not for reading values back.
var sensitiveKeySubstrings = []string{"pass", "secret", "token", "key", "cred"}

// isSensitiveKey reports whether a JSON key must be masked in debug logs:
// exact match against sensitiveBodyKeys, else substring match against
// sensitiveKeySubstrings. Both case-insensitive.
func isSensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	if sensitiveBodyKeys[lk] {
		return true
	}
	for _, sub := range sensitiveKeySubstrings {
		if strings.Contains(lk, sub) {
			return true
		}
	}
	return false
}

// redactJSON deep-walks a decoded JSON value, replacing the value at any
// isSensitiveKey-matching key with "***". Matching is case-insensitive so
// API responses that differ in casing from the spec (or future spec drift) are
// still scrubbed.
func redactJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if isSensitiveKey(k) {
				x[k] = "***"
				continue
			}
			x[k] = redactJSON(val)
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = redactJSON(val)
		}
		return x
	default:
		return v
	}
}

// redactBody renders a body for debug logging with sensitive fields masked at
// any depth. Empty bodies return ""; non-JSON bodies are reported without
// content (we cannot redact what we cannot parse, so we refuse to log it).
func redactBody(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return "(non-JSON body omitted)"
	}
	v = redactJSON(v)
	out, err := json.Marshal(v)
	if err != nil {
		return "(redaction failed)"
	}
	return string(out)
}

// truncate bounds a debug-log string and is used after redaction so secrets
// never slip into a partial dump.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

const (
	// DefaultBaseURL is the production ACM API root. Overridable via the
	// provider's api_url attribute.
	DefaultBaseURL = "https://acm.altinity.cloud/api"

	// authHeader carries the ACM API token.
	authHeader = "X-Auth-Token"

	// defaultMaxRetries bounds the retry loop on 429/5xx.
	defaultMaxRetries = 4
	// retryBaseDelay is the first backoff step; doubles each attempt, capped.
	retryBaseDelay = 500 * time.Millisecond
	// retryMaxDelay caps the exponential backoff.
	retryMaxDelay = 8 * time.Second
)

// Client is the ACM REST client. Construct it with NewClient. Safe for
// concurrent use by multiple goroutines.
type Client struct {
	baseURL string
	token   string
	// httpClient is the underlying HTTP client. Defaults to a per-call 60s
	// timeout — override with WithHTTPClient or WithHTTPTimeout.
	httpClient *http.Client
	maxRetries int
	// clock indirection for deterministic backoff in tests.
	sleep func(context.Context, time.Duration) error
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the underlying *http.Client (used by tests to inject
// an httptest.Server transport).
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.httpClient = h }
}

// WithMaxRetries overrides the retry cap for transient (429/5xx) failures.
// Negative values are clamped to 0 (one attempt, no retries).
func WithMaxRetries(n int) Option {
	if n < 0 {
		n = 0
	}
	return func(c *Client) { c.maxRetries = n }
}

// WithHTTPTimeout overrides the default 60s HTTP client timeout. Use a longer
// value if your environment commonly serves large environment lists or slow
// status polls. Zero disables the per-client timeout (rely on context only).
func WithHTTPTimeout(d time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = d }
}

// withSleep overrides the backoff sleeper (test seam).
func withSleep(f func(context.Context, time.Duration) error) Option {
	return func(c *Client) { c.sleep = f }
}

// NewClient builds an ACM client. baseURL defaults to DefaultBaseURL when
// empty; the token is sent as the X-Auth-Token header on every request.
func NewClient(baseURL, token string, opts ...Option) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: &http.Client{Timeout: 60 * time.Second},
		maxRetries: defaultMaxRetries,
		sleep:      sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// acmEnvelope is the common ACM response wrapper. Successful reads return
// {"data": ...}; errors return {"error": ..., "code": ...}. Fields are decoded
// best-effort.
type acmEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
	Code  json.RawMessage `json:"code"`
}

// doJSON resolves the allowlisted operationId against the generated endpoint
// registry, substitutes path params, marshals reqBody (nil for none), executes
// the request with retry/backoff on 429/5xx, and — on success — unmarshals the
// response envelope's "data" field into out (nil to discard). pathArgs maps
// each PathParam name to its value.
//
// Opaque request-body fields (the bare-object launch fields) are the caller's
// responsibility; this helper just serializes whatever struct it is given.
func (c *Client) doJSON(ctx context.Context, opID string, pathArgs map[string]string, reqBody, out any) error {
	return c.doRequest(ctx, opID, pathArgs, nil, reqBody, out)
}

// doRequest is doJSON with optional query parameters.
func (c *Client) doRequest(ctx context.Context, opID string, pathArgs map[string]string, query url.Values, reqBody, out any) error {
	ep, ok := wire.Endpoints[opID]
	if !ok {
		// Programmer error: operationId not in the generated registry.
		return fmt.Errorf("acm: unknown operationId %q (not in generated endpoint registry)", opID)
	}

	path, err := resolvePath(ep, pathArgs)
	if err != nil {
		return err
	}
	url := c.baseURL + path
	if len(query) > 0 {
		url += "?" + query.Encode()
	}

	var bodyBytes []byte
	if reqBody != nil {
		bodyBytes, err = json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("acm: marshal %s request: %w", opID, err)
		}
	}

	tflog.Debug(ctx, "acm request", map[string]any{
		"operation": opID,
		"method":    ep.Method,
		"url":       url,
		"body":      redactBody(bodyBytes),
	})

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			tflog.Debug(ctx, "acm retry (transient)", map[string]any{"operation": opID, "attempt": attempt})
			if err := c.sleep(ctx, backoffDelay(attempt)); err != nil {
				return err
			}
		}

		var bodyReader io.Reader
		if bodyBytes != nil {
			bodyReader = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, ep.Method, url, bodyReader)
		if err != nil {
			return fmt.Errorf("acm: build %s request: %w", opID, err)
		}
		req.Header.Set(authHeader, c.token)
		req.Header.Set("Accept", "application/json")
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			// Transport-level error (connection reset, etc.). Only retry for an
			// idempotent GET; a POST/PUT/DELETE may have reached the server.
			lastErr = fmt.Errorf("acm: %s request: %w", opID, err)
			if ep.Method == http.MethodGet {
				continue
			}
			return lastErr
		}

		data, apiErr := c.handleResponse(ctx, opID, resp)
		if apiErr != nil {
			lastErr = apiErr
			if c.retryable(ep.Method, apiErr) {
				continue
			}
			return apiErr
		}

		if out != nil && len(data) > 0 && string(data) != "null" {
			// ACM's PHP backend signals user/profile/setting create failures
			// by returning {"data": false} at the API layer — the actual
			// ClickHouse error is only in the audit log, NEVER in the API
			// response body. Live-confirmed 2026-06-08: same DbuserAdd body
			// returns `{"data": false}` during the profile-propagation race
			// window, then returns a proper user object once the operator's
			// push lands. We surface this as an APIError tagged with Code 62
			// so RetryOnTransientCreateRace recognizes and retries it.
			//
			// We deliberately match ONLY `false`. ACM uses `{"data": true}`
			// to signal SUCCESS on no-payload operations (e.g. some delete
			// endpoints) — treating it as failure here would synthesize
			// spurious errors for legitimate successes.
			if isBoolFalseEnvelope(data) {
				return &APIError{
					Operation:  opID,
					StatusCode: http.StatusOK,
					Message:    "Code: 62. ACM returned `{\"data\": false}` (SQL execution failed; check ACM audit log for the real error). Likely cause: distributed-DDL propagation race after recent profile/cluster create.",
				}
			}
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("acm: decode %s response: %w", opID, err)
			}
		}
		return nil
	}
	return lastErr
}

// isBoolFalseEnvelope reports whether data is the JSON literal `false`. ACM
// returns this shape under the standard `data` envelope to signal a failed
// SQL execution without surfacing the underlying error in the response body
// — see the callsite. `true` (success-without-payload) is NOT matched.
func isBoolFalseEnvelope(data []byte) bool {
	return strings.TrimSpace(string(data)) == "false"
}

// retryable decides whether to retry a failed request. HTTP 429 (rate limited,
// not processed) is always safe to retry. A 5xx is retried ONLY for an
// idempotent GET: a 5xx on a create/mutation (e.g. ClusterLaunch) may have
// applied server-side, and blindly retrying then hits ACM's per-environment
// "operation is in progress" lock and busy-loops until timeout. Surfacing the
// 5xx immediately is correct for non-GET.
func (c *Client) retryable(method string, err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if ae.StatusCode >= 500 && ae.StatusCode <= 599 {
		return method == http.MethodGet
	}
	return false
}

// handleResponse reads and closes the body, returning the envelope's data on
// 2xx or a typed *APIError otherwise.
func (c *Client) handleResponse(ctx context.Context, opID string, resp *http.Response) (json.RawMessage, error) {
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	tflog.Debug(ctx, "acm response", map[string]any{
		"operation": opID,
		"status":    resp.StatusCode,
		"body":      truncate(redactBody(body), 4000),
	})

	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		if len(body) == 0 {
			return nil, nil
		}
		var env acmEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			// Some endpoints may return a bare JSON value rather than an
			// envelope; hand the raw body back to the caller.
			return json.RawMessage(body), nil
		}
		// ACM (PHP) sometimes returns HTTP 200 with an error envelope
		// ({"error":...,"code":403}) instead of a 4xx — e.g. a rejected launch
		// or an auth failure. Treat a populated "error" as a failure so callers
		// don't proceed with a zero-value result (which previously made
		// LaunchCluster return id 0 with a nil error).
		if env.Error != "" {
			return nil, errorFromEnvelope(opID, resp.StatusCode, env)
		}
		if env.Data != nil {
			return env.Data, nil
		}
		// No "data" wrapper — return the whole body.
		return json.RawMessage(body), nil
	}

	// Error path (non-2xx): parse {"error","code"} best-effort.
	var env acmEnvelope
	_ = json.Unmarshal(body, &env)
	ae := errorFromEnvelope(opID, resp.StatusCode, env)
	if ae.Message == "" {
		ae.Message = strings.TrimSpace(string(body))
	}
	return nil, ae
}

// errorFromEnvelope builds an *APIError from a parsed ACM error envelope. When
// the body carries a numeric "code" that looks like an HTTP status (ACM returns
// HTTP 200 with {"code":403} on some failures), that code becomes the effective
// StatusCode so IsUnauthorized/IsNotFound classify correctly.
func errorFromEnvelope(opID string, httpStatus int, env acmEnvelope) *APIError {
	ae := &APIError{
		StatusCode: httpStatus,
		Operation:  opID,
		Message:    env.Error,
		Code:       decodeCode(env.Code),
	}
	if n, err := strconv.Atoi(ae.Code); err == nil && n >= 400 && n <= 599 {
		ae.StatusCode = n
	}
	return ae
}

// decodeCode normalizes the ACM "code" field, which is sometimes a string and
// sometimes a number, into a string.
func decodeCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return strings.Trim(string(raw), `"`)
}

// resolvePath substitutes {name} placeholders in the endpoint template with the
// supplied path-argument values, verifying every declared param is provided.
// Values are URL-path-escaped so a user-controlled name (e.g. a keeper name
// containing "/" or "..") cannot break out of its path segment and reshape the
// request URL. Schema-level validators provide defense-in-depth for the
// operator-visible attributes; this escape is the final hard guarantee.
func resolvePath(ep wire.Endpoint, args map[string]string) (string, error) {
	path := ep.PathTemplate
	for _, name := range ep.PathParams {
		val, ok := args[name]
		if !ok || val == "" {
			return "", fmt.Errorf("acm: %s missing path arg %q", ep.OperationID, name)
		}
		path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(val))
	}
	if strings.Contains(path, "{") {
		return "", fmt.Errorf("acm: %s unresolved path placeholders in %q", ep.OperationID, path)
	}
	return path, nil
}

// backoffDelay returns the capped exponential backoff for the given attempt
// (attempt >= 1).
func backoffDelay(attempt int) time.Duration {
	// Compare in the float domain before converting to time.Duration: a large
	// attempt makes the product exceed int64, which would overflow to a
	// negative Duration and defeat the cap check.
	d := float64(retryBaseDelay) * math.Pow(2, float64(attempt-1))
	if d >= float64(retryMaxDelay) {
		return retryMaxDelay
	}
	return time.Duration(d)
}
