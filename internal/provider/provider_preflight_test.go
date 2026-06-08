// Copyright (c) Gorgias, Inc.
// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Gorgias/terraform-provider-altinity/internal/acm"
)

// TestPreflightAuth verifies the provider's early auth check: an invalid token
// (ACM may signal it as HTTP 403 OR HTTP 200 with {"code":403}) yields an
// unauthorized error, while a valid token returns nil.
func TestPreflightAuth(t *testing.T) {
	t.Run("rejects HTTP 200 with 403 envelope", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"error":"Access denied.","code":403}`))
		}))
		t.Cleanup(srv.Close)
		c := acm.NewClient(srv.URL, "bad", acm.WithHTTPClient(srv.Client()))
		err := preflightAuth(context.Background(), c)
		require.Error(t, err)
		assert.True(t, acm.IsUnauthorized(err), "got %v", err)
	})

	t.Run("rejects HTTP 403", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"Access denied.","code":403}`))
		}))
		t.Cleanup(srv.Close)
		c := acm.NewClient(srv.URL, "bad", acm.WithHTTPClient(srv.Client()))
		err := preflightAuth(context.Background(), c)
		require.Error(t, err)
		assert.True(t, acm.IsUnauthorized(err), "got %v", err)
	})

	t.Run("accepts a valid token", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"2267","name":"env","type":"kubernetes"}]}`))
		}))
		t.Cleanup(srv.Close)
		c := acm.NewClient(srv.URL, "good", acm.WithHTTPClient(srv.Client()))
		assert.NoError(t, preflightAuth(context.Background(), c))
	})
}
