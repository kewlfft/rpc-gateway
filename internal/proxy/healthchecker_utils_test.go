package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPerformEthCallHealthCheckSuccess(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x5f78c33274e43fa9de5659265c1d917e25c03722dcb0b8d27db8d5feaa813953"}`))
		}),
	)
	defer server.Close()

	client := CreateOptimizedHTTPClient("test-client-success", 30*time.Second)
	err := performEthCallHealthCheck(t.Context(), client, server.URL, nil)

	assert.NoError(t, err)
}

func TestPerformEthCallHealthCheckRejectsStubbedResult(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			// Echo stub (identity-style) must not pass the SHA-256 probe.
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xdeadbeef"}`))
		}),
	)
	defer server.Close()

	client := CreateOptimizedHTTPClient("test-client-stub", 30*time.Second)
	err := performEthCallHealthCheck(t.Context(), client, server.URL, nil)

	assert.ErrorContains(t, err, "unexpected result")
}

func TestPerformEthCallHealthCheckErrors(t *testing.T) {
	t.Parallel()

	t.Run("expect error when HTTP status is not 200", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if assert.Contains(t, r.Header, "Content-Type") {
					assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
				}

				w.WriteHeader(http.StatusServiceUnavailable)
			}),
		)
		defer server.Close()

		client := CreateOptimizedHTTPClient("test-client", 30*time.Second)
		err := performEthCallHealthCheck(t.Context(), client, server.URL, nil)

		assert.Error(t, err)
		assert.ErrorContains(t, err, "unexpected status")
	})

	t.Run("expect error when JSON payload is invalid", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if assert.Contains(t, r.Header, "Content-Type") {
					assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
				}

				w.Write([]byte(`{{}`))
				w.WriteHeader(http.StatusOK)
			}),
		)
		defer server.Close()

		client := CreateOptimizedHTTPClient("test-client", 30*time.Second)
		err := performEthCallHealthCheck(t.Context(), client, server.URL, nil)

		assert.Error(t, err)
		assert.ErrorContains(t, err, "decode:")
	})

	t.Run("expect error when server timeouts", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if assert.Contains(t, r.Header, "Content-Type") {
					assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
				}
				<-time.After(time.Second * 3)

				w.WriteHeader(http.StatusServiceUnavailable)
			}),
		)
		defer server.Close()

		timeout, cancel := context.WithTimeout(t.Context(), time.Second*1)
		defer cancel()

		client := CreateOptimizedHTTPClient("test-client-timeout", 30*time.Second)
		err := performEthCallHealthCheck(timeout, client, server.URL, nil)

		assert.Error(t, err)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestPerformEthCallHealthCheckSendsProviderHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + sha256PrecompileExpect + `"}`))
	}))
	defer server.Close()

	client := CreateOptimizedHTTPClient("test-client-headers", 30*time.Second)
	assert.NoError(t, performEthCallHealthCheck(t.Context(), client, server.URL, map[string]string{"X-Api-Key": "secret"}))
	assert.ErrorContains(t, performEthCallHealthCheck(t.Context(), client, server.URL, nil), "unexpected status 401")
}
