# TODO

## Security hardening

- [ ] **Stop following HTTP redirects in the ACM client** (`internal/acm/client.go`).
  The token is sent in the custom `X-Auth-Token` header, which Go's
  `http.Client` does NOT strip when following a redirect to a different
  host (only `Authorization`/`Cookie`/`Www-Authenticate` are protected) —
  so an open redirect on the API, a misconfigured proxy, or a MITM on a
  cleartext `api_url` could exfiltrate the token (and, on 307/308, the
  request body: `adminPass`, Datadog keys, …). The client never
  legitimately needs redirects — every call is a direct API hit. Fix:

  ```go
  httpClient: &http.Client{
      Timeout: 60 * time.Second,
      CheckRedirect: func(*http.Request, []*http.Request) error {
          return http.ErrUseLastResponse
      },
  }
  ```

  plus a test asserting a 302 from the mock server is surfaced as the
  response (or an error), not followed. Mind `WithHTTPClient`: apply the
  same policy (or document that callers supplying their own client own
  this risk).
