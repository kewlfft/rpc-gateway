package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/go-http-utils/headers"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
)

func createConfig() Config {
	return Config{
		UpstreamTimeout: time.Second * 3,
		HealthChecks: HealthCheckConfig{
			Interval:         0,
			Timeout:          0,
			FailureThreshold: 0,
			SuccessThreshold: 0,
		},
		Targets: []NodeProviderConfig{},
	}
}

func TestHttpFailoverProxyRerouteRequests(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	fakeRPC1Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w,
			http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
	}))
	defer fakeRPC1Server.Close()

	fakeRPC2Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	}))
	defer fakeRPC2Server.Close()

	rpcGatewayConfig := createConfig()
	rpcGatewayConfig.Targets = []NodeProviderConfig{
		{
			Name: "Server1",
			Connection: struct {
				HTTP struct {
					URL         string `yaml:"url"`
					Compression bool   `yaml:"compression"`
				} `yaml:"http"`
			}{HTTP: struct {
				URL         string `yaml:"url"`
				Compression bool   `yaml:"compression"`
			}{URL: fakeRPC1Server.URL}},
		},
		{
			Name: "Server2",
			Connection: struct {
				HTTP struct {
					URL         string `yaml:"url"`
					Compression bool   `yaml:"compression"`
				} `yaml:"http"`
			}{HTTP: struct {
				URL         string `yaml:"url"`
				Compression bool   `yaml:"compression"`
			}{URL: fakeRPC2Server.URL}},
		},
	}
	rpcGatewayConfig.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Setup HttpFailoverProxy but not starting the HealthCheckManager
	// so the no target will be tainted or marked as unhealthy by the HealthCheckManager
	httpFailoverProxy, err := NewProxy(context.Background(), rpcGatewayConfig)
	assert.NoError(t, err)
	assert.NotNil(t, httpFailoverProxy)

	requestBody := bytes.NewBufferString(`{"this_is": "body"}`)
	req, err := http.NewRequest(http.MethodPost, "/", requestBody)

	assert.Nil(t, err)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(httpFailoverProxy.ServeHTTP)
	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)

	// This test makes sure that the request's body is forwarded to
	// the next RPC Provider
	//
	assert.Equal(t, `{"this_is": "body"}`, rr.Body.String())
}

func TestHttpFailoverProxyDecompressRequest(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	var receivedBody, receivedHeaderContentEncoding, receivedHeaderContentLength string
	fakeRPC1Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaderContentEncoding = r.Header.Get(headers.ContentEncoding)
		receivedHeaderContentLength = r.Header.Get(headers.ContentLength)
		body, _ := io.ReadAll(r.Body)
		receivedBody = string(body)
		w.Write([]byte("OK"))
	}))
	defer fakeRPC1Server.Close()
	rpcGatewayConfig := createConfig()
	rpcGatewayConfig.Targets = []NodeProviderConfig{
		{
			Name: "Server1",
			Connection: struct {
				HTTP struct {
					URL         string `yaml:"url"`
					Compression bool   `yaml:"compression"`
				} `yaml:"http"`
			}{HTTP: struct {
				URL         string `yaml:"url"`
				Compression bool   `yaml:"compression"`
			}{URL: fakeRPC1Server.URL}},
		},
	}
	rpcGatewayConfig.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Setup HttpFailoverProxy but not starting the HealthCheckManager
	// so the no target will be tainted or marked as unhealthy by the HealthCheckManager
	httpFailoverProxy, err := NewProxy(context.Background(), rpcGatewayConfig)
	assert.NotNil(t, httpFailoverProxy)
	assert.NoError(t, err)

	var buf bytes.Buffer
	g := gzip.NewWriter(&buf)

	_, err = g.Write([]byte(`{"body": "content"}`))
	assert.NoError(t, err)
	assert.NoError(t, g.Close())

	req, err := http.NewRequest(http.MethodPost, "/", &buf)
	assert.NoError(t, err)

	req.Header.Add(headers.ContentEncoding, "gzip")

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(httpFailoverProxy.ServeHTTP)
	handler.ServeHTTP(rr, req)

	assert.Equal(t, `{"body": "content"}`, receivedBody)
	assert.Equal(t, "", receivedHeaderContentEncoding)
	assert.Equal(t, strconv.Itoa(len(`{"body": "content"}`)), receivedHeaderContentLength)
}

func TestHttpFailoverProxyWithCompressionSupportedTarget(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	var receivedHeaderContentEncoding string
	var receivedBody []byte
	fakeRPC1Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaderContentEncoding = r.Header.Get(headers.ContentEncoding)
		receivedBody, _ = io.ReadAll(r.Body)
		w.Write([]byte("OK"))
	}))
	defer fakeRPC1Server.Close()

	rpcGatewayConfig := createConfig()
	rpcGatewayConfig.Targets = []NodeProviderConfig{
		{
			Name: "Server1",
			Connection: struct {
				HTTP struct {
					URL         string `yaml:"url"`
					Compression bool   `yaml:"compression"`
				} `yaml:"http"`
			}{HTTP: struct {
				URL         string `yaml:"url"`
				Compression bool   `yaml:"compression"`
			}{URL: fakeRPC1Server.URL, Compression: true}},
		},
	}
	rpcGatewayConfig.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Setup HttpFailoverProxy but not starting the HealthCheckManager
	// so the no target will be tainted or marked as unhealthy by the HealthCheckManager
	httpFailoverProxy, err := NewProxy(context.Background(), rpcGatewayConfig)
	assert.NotNil(t, httpFailoverProxy)
	assert.NoError(t, err)

	var buf bytes.Buffer
	g := gzip.NewWriter(&buf)

	_, err = g.Write([]byte(`{"body": "content"}`))
	assert.NoError(t, err)
	assert.NoError(t, g.Close())

	req, err := http.NewRequest(http.MethodPost, "/", &buf)
	req.Header.Add(headers.ContentEncoding, "gzip")
	assert.NoError(t, err)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(httpFailoverProxy.ServeHTTP)
	handler.ServeHTTP(rr, req)

	assert.Equal(t, "gzip", receivedHeaderContentEncoding)

	var wantBody bytes.Buffer
	g = gzip.NewWriter(&wantBody)
	g.Write([]byte(`{"body": "content"}`))

	assert.NoError(t, g.Close())
	assert.Equal(t, wantBody.Bytes(), receivedBody)
}

func TestHTTPFailoverProxyWhenCannotConnectToPrimaryProvider(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	fakeRPCServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Write(body)
	}))
	defer fakeRPCServer.Close()

	rpcGatewayConfig := createConfig()

	rpcGatewayConfig.Targets = []NodeProviderConfig{
		{
			Name: "Server1",
			Connection: struct {
				HTTP struct {
					URL         string `yaml:"url"`
					Compression bool   `yaml:"compression"`
				} `yaml:"http"`
			}{HTTP: struct {
				URL         string `yaml:"url"`
				Compression bool   `yaml:"compression"`
			}{URL: "http://foo.bar"}},
		},
		{
			Name: "Server2",
			Connection: struct {
				HTTP struct {
					URL         string `yaml:"url"`
					Compression bool   `yaml:"compression"`
				} `yaml:"http"`
			}{HTTP: struct {
				URL         string `yaml:"url"`
				Compression bool   `yaml:"compression"`
			}{URL: fakeRPCServer.URL}},
		},
	}
	rpcGatewayConfig.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Setup HttpFailoverProxy but not starting the HealthCheckManager so the
	// no target will be tainted or marked as unhealthy by the
	// HealthCheckManager the failoverProxy should automatically reroute the
	// request to the second RPC Server by itself
	httpFailoverProxy, err := NewProxy(context.Background(), rpcGatewayConfig)
	assert.NotNil(t, httpFailoverProxy)
	assert.NoError(t, err)

	requestBody := bytes.NewBufferString(`{"this_is": "body"}`)
	req, err := http.NewRequest(http.MethodPost, "/", requestBody)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(httpFailoverProxy.ServeHTTP)

	handler.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, `{"this_is": "body"}`, rr.Body.String())
}
