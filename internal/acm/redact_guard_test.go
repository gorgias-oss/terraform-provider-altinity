// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package acm

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsonTagRe extracts the field name from every `json:"..."` struct tag in the
// generated wire models.
var jsonTagRe = regexp.MustCompile(`json:"([a-zA-Z0-9_]+)`)

// credentialShapedRe is the shape a secret-bearing field name takes. Kept in
// sync (deliberately broader-or-equal) with sensitiveKeySubstrings in
// client.go.
var credentialShapedRe = regexp.MustCompile(`(?i)pass|secret|token|key|cred`)

// TestRedaction_CoversAllWireCredentialFields is a codegen drift guard: every
// field name in the generated wire models that LOOKS credential-bearing must
// be masked by the debug-log redactor. When `make generate` introduces a new
// secret-shaped field (bearerToken, clientSecret, ...) that the redaction
// rules in client.go don't cover, this test fails instead of the field
// silently logging in cleartext at TF_LOG=DEBUG.
func TestRedaction_CoversAllWireCredentialFields(t *testing.T) {
	src, err := os.ReadFile("wire/models_gen.go")
	require.NoError(t, err, "read generated wire models")

	matches := jsonTagRe.FindAllStringSubmatch(string(src), -1)
	require.NotEmpty(t, matches, "no json tags found — did the generated file move?")

	seen := map[string]bool{}
	checked := 0
	for _, m := range matches {
		name := m[1]
		if seen[name] || !credentialShapedRe.MatchString(name) {
			seen[name] = true
			continue
		}
		seen[name] = true
		checked++
		assert.True(t, isSensitiveKey(name),
			"wire field %q looks credential-bearing but is not masked by the debug-log redactor; "+
				"add it to sensitiveBodyKeys (or extend sensitiveKeySubstrings) in client.go", name)
	}
	require.NotZero(t, checked, "guard checked no fields — pattern broken?")
}

// TestRedactBody_OpaqueBlobsMaskedWholesale: the cluster schema marks
// backup_options / uptime_settings / alternate_endpoints Sensitive because
// they may carry credentials; the debug-log redactor must mask the whole blob
// (the provider passes them through opaquely, so inner key names are not
// under our control).
func TestRedactBody_OpaqueBlobsMaskedWholesale(t *testing.T) {
	in := []byte(`{
		"name": "c1",
		"backupOptions": {"bucket": "b", "secretAccessKey": "LEAK1"},
		"uptimeSettings": {"url": "https://x", "authToken": "LEAK2"},
		"alternateEndpoints": [{"host": "h", "tlsKey": "LEAK3"}]
	}`)
	out := redactBody(in)
	assert.Contains(t, out, `"name":"c1"`)
	for _, leak := range []string{"LEAK1", "LEAK2", "LEAK3"} {
		assert.NotContains(t, out, leak)
	}
	assert.Contains(t, out, `"backupOptions":"***"`)
	assert.Contains(t, out, `"uptimeSettings":"***"`)
	assert.Contains(t, out, `"alternateEndpoints":"***"`)
}

// TestRedactBody_SubstringFallback: credential-shaped keys that are not in
// the exact-match list (the realistic object-storage / OAuth names) must be
// caught by the substring fallback at any depth.
func TestRedactBody_SubstringFallback(t *testing.T) {
	in := []byte(`{
		"settings": {
			"accessKeyId": "LEAK1",
			"s3SecretKey": "LEAK2",
			"bearerToken": "LEAK3",
			"clientSecret": "LEAK4",
			"credentialsFile": "LEAK5"
		},
		"host": "keep-me",
		"shards": 2
	}`)
	out := redactBody(in)
	for _, leak := range []string{"LEAK1", "LEAK2", "LEAK3", "LEAK4", "LEAK5"} {
		assert.NotContains(t, out, leak)
	}
	assert.Contains(t, out, `"host":"keep-me"`)
	assert.Contains(t, out, `"shards":2`)
}
