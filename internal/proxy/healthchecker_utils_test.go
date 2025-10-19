package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPerformGasLeftCallErrors(t *testing.T) {
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

		gas, err := performGasLeftCall(context.TODO(), &http.Client{}, server.URL)

		assert.Zero(t, gas)
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

		gas, err := performGasLeftCall(context.TODO(), &http.Client{}, server.URL)

		assert.Zero(t, gas)
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

		timeout, cancel := context.WithTimeout(context.TODO(), time.Second*1)
		defer cancel()

		gas, err := performGasLeftCall(timeout, &http.Client{}, server.URL)

		assert.Zero(t, gas)
		assert.Error(t, err)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
}
