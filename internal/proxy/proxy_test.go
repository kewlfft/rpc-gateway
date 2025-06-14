package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const contentLength = "Content-Length"

func createConfig() Config {
	return Config{
		Timeout: time.Second * 3,
		HealthChecks: HealthCheckConfig{
			Interval:         time.Second * 5,
			BlockDiffThreshold: 2,
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
					URL    string `yaml:"url"`
					APIKey string `yaml:"apiKey"`
				} `yaml:"http"`
				WebSocket struct {
					URL string `yaml:"url"`
				} `yaml:"websocket"`
			}{HTTP: struct {
				URL    string `yaml:"url"`
				APIKey string `yaml:"apiKey"`
			}{URL: fakeRPC1Server.URL, APIKey: ""}, WebSocket: struct{URL string `yaml:"url"`}{URL: ""}},
		},
		{
			Name: "Server2",
			Connection: struct {
				HTTP struct {
					URL    string `yaml:"url"`
					APIKey string `yaml:"apiKey"`
				} `yaml:"http"`
				WebSocket struct {
					URL string `yaml:"url"`
				} `yaml:"websocket"`
			}{HTTP: struct {
				URL    string `yaml:"url"`
				APIKey string `yaml:"apiKey"`
			}{URL: fakeRPC2Server.URL, APIKey: ""}, WebSocket: struct{URL string `yaml:"url"`}{URL: ""}},
		},
	}
	rpcGatewayConfig.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	rpcGatewayConfig.DisableHealthChecks = true

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

	var receivedHeaderContentEncoding, receivedHeaderContentLength string
	fakeRPC1Server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaderContentEncoding = r.Header.Get("Content-Encoding")
		receivedHeaderContentLength = r.Header.Get(contentLength)
		w.Header().Set("Content-Type", "application/json")
		// Write a gzipped response
		var respBuf bytes.Buffer
		gzipWriter := gzip.NewWriter(&respBuf)
		_, _ = gzipWriter.Write([]byte(`{"body": "content"}`))
		_ = gzipWriter.Close()
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", respBuf.Len()))
		w.Write(respBuf.Bytes())
	}))
	defer fakeRPC1Server.Close()
	rpcGatewayConfig := createConfig()
	rpcGatewayConfig.Targets = []NodeProviderConfig{
		{
			Name: "Server1",
			Connection: struct {
				HTTP struct {
					URL    string `yaml:"url"`
					APIKey string `yaml:"apiKey"`
				} `yaml:"http"`
				WebSocket struct {
					URL string `yaml:"url"`
				} `yaml:"websocket"`
			}{HTTP: struct {
				URL    string `yaml:"url"`
				APIKey string `yaml:"apiKey"`
			}{URL: fakeRPC1Server.URL, APIKey: ""}, WebSocket: struct{URL string `yaml:"url"`}{URL: ""}},
		},
	}
	rpcGatewayConfig.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	rpcGatewayConfig.DisableHealthChecks = true  // Disable health checks for this test

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

	gzippedBody := buf.Bytes()

	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader(gzippedBody))
	assert.NoError(t, err)

	req.Header.Add("Content-Encoding", "gzip")

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(httpFailoverProxy.ServeHTTP)
	handler.ServeHTTP(rr, req)

	// Decompress the response body before comparing
	reader, err := gzip.NewReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("Failed to create gzip reader: %v", err)
	}
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to decompress response body: %v", err)
	}

	assert.Equal(t, `{"body": "content"}`, string(decompressed))
	assert.Equal(t, "gzip", receivedHeaderContentEncoding)
	assert.Equal(t, strconv.Itoa(len(rr.Body.Bytes())), receivedHeaderContentLength)
}

func TestHttpFailoverProxyWithCompressionSupportedTarget(t *testing.T) {
	// Create a test server that supports compression
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request headers and body
		if r.Header.Get("Content-Encoding") != "gzip" {
			t.Errorf("Expected Content-Encoding: gzip, got: %s", r.Header.Get("Content-Encoding"))
		}

		// Read and verify the request body
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("Failed to read request body: %v", err)
		}
		if len(body) == 0 {
			t.Error("Expected non-empty request body")
		}

		// Create a gzipped response
		var respBuf bytes.Buffer
		gzipWriter := gzip.NewWriter(&respBuf)
		if _, err := gzipWriter.Write([]byte(`{"result":"test"}`)); err != nil {
			t.Fatalf("Failed to write gzipped response: %v", err)
		}
		if err := gzipWriter.Close(); err != nil {
			t.Fatalf("Failed to close gzip writer: %v", err)
		}

		// Set response headers
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", respBuf.Len()))

		w.WriteHeader(http.StatusOK)

		// Write the gzipped response
		if _, err := w.Write(respBuf.Bytes()); err != nil {
			t.Fatalf("Failed to write response: %v", err)
		}
	}))
	defer server.Close()

	// Create proxy configuration
	config := Config{
		Targets: []NodeProviderConfig{
			{
				Name: "test",
				Connection: struct {
					HTTP struct {
						URL    string `yaml:"url"`
						APIKey string `yaml:"apiKey"`
					} `yaml:"http"`
					WebSocket struct {
						URL string `yaml:"url"`
					} `yaml:"websocket"`
				}{
					HTTP: struct {
						URL    string `yaml:"url"`
						APIKey string `yaml:"apiKey"`
					}{
						URL: server.URL,
						APIKey: "",
					},
					WebSocket: struct{URL string `yaml:"url"`}{URL: ""},
				},
			},
		},
		Timeout:         5 * time.Second,
		Logger:         slog.Default(),
		DisableHealthChecks: true,
	}

	// Create proxy
	proxy, err := NewProxy(context.Background(), config)
	if err != nil {
		t.Fatalf("Failed to create proxy: %v", err)
	}

	// Create a gzipped request
	var reqBuf bytes.Buffer
	gzipWriter := gzip.NewWriter(&reqBuf)
	if _, err := gzipWriter.Write([]byte(`{"method":"test"}`)); err != nil {
		t.Fatalf("Failed to write gzipped request: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("Failed to close gzip writer: %v", err)
	}

	// Create request
	req, err := http.NewRequest("POST", "/", bytes.NewReader(reqBuf.Bytes()))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")

	// Create response recorder
	rr := httptest.NewRecorder()

	// Send request through proxy
	proxy.ServeHTTP(rr, req)

	// Verify response
	if status := rr.Code; status != http.StatusOK {
		t.Errorf("Expected status code %d, got %d", http.StatusOK, status)
	}

	// Verify response headers
	fmt.Printf("Response headers: %+v\n", rr.Header())
	if contentEncoding := rr.Header().Get("Content-Encoding"); contentEncoding != "gzip" {
		t.Errorf("Expected Content-Encoding: gzip, got: %s", contentEncoding)
	}

	// Verify response body
	reader, err := gzip.NewReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatalf("Failed to create gzip reader: %v", err)
	}
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("Failed to decompress response body: %v", err)
	}
	expectedBody := `{"result":"test"}`
	if string(decompressed) != expectedBody {
		t.Errorf("Expected response body %s, got %s", expectedBody, string(decompressed))
	}
}

func TestHTTPFailoverProxyWhenCannotConnectToPrimaryProvider(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	var receivedBody []byte
	fakeRPCServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		var req map[string]interface{}
		_ = json.Unmarshal(receivedBody, &req)

		// Handle health check requests
		if method, ok := req["method"].(string); ok {
			switch method {
			case "eth_blockNumber":
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1234"}`))
				return
			case "eth_call":
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1000"}`))
				return
			}
		}

		// Default: echo back the request body
		w.Write(receivedBody)
	}))
	defer fakeRPCServer.Close()

	rpcGatewayConfig := createConfig()

	rpcGatewayConfig.Targets = []NodeProviderConfig{
		{
			Name: "Server1",
			Connection: struct {
				HTTP struct {
					URL    string `yaml:"url"`
					APIKey string `yaml:"apiKey"`
				} `yaml:"http"`
				WebSocket struct {
					URL string `yaml:"url"`
				} `yaml:"websocket"`
			}{HTTP: struct {
				URL    string `yaml:"url"`
				APIKey string `yaml:"apiKey"`
			}{URL: fakeRPCServer.URL}},
		},
	}

	rpcGatewayConfig.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	rpcGatewayConfig.DisableHealthChecks = true  // Disable health checks for this test

	// Setup HttpFailoverProxy but not starting the HealthCheckManager
	// so the no target will be tainted or marked as unhealthy by the HealthCheckManager
	httpFailoverProxy, err := NewProxy(context.Background(), rpcGatewayConfig)
	assert.NotNil(t, httpFailoverProxy)
	assert.NoError(t, err)

	req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"body": "content"}`)))
	assert.NoError(t, err)

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(httpFailoverProxy.ServeHTTP)
	handler.ServeHTTP(rr, req)

	// Verify request handling
	assert.Equal(t, []byte(`{"body": "content"}`), receivedBody, "Received body should match the original content")

	// Verify response
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "text/plain; charset=utf-8", rr.Header().Get("Content-Type"))
	assert.Equal(t, []byte(`{"body": "content"}`), rr.Body.Bytes())
}

func TestTronProxyURLRedirection(t *testing.T) {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()

	var receivedPath, receivedMethod string
	var receivedHeaders http.Header
	fakeRPCServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedHeaders = r.Header
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		_ = json.Unmarshal(body, &req)
		if method, ok := req["method"].(string); ok {
			receivedMethod = method
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"result":"test"}`))
	}))
	defer fakeRPCServer.Close()

	rpcGatewayConfig := createConfig()
	rpcGatewayConfig.ChainType = "tron"
	rpcGatewayConfig.Targets = []NodeProviderConfig{
		{
			Name: "Server1",
			Connection: struct {
				HTTP struct {
					URL    string `yaml:"url"`
					APIKey string `yaml:"apiKey"`
				} `yaml:"http"`
				WebSocket struct {
					URL string `yaml:"url"`
				} `yaml:"websocket"`
			}{HTTP: struct {
				URL    string `yaml:"url"`
				APIKey string `yaml:"apiKey"`
			}{URL: fakeRPCServer.URL, APIKey: "test-api-key"}, WebSocket: struct{URL string `yaml:"url"`}{URL: ""}},
		},
	}
	rpcGatewayConfig.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	rpcGatewayConfig.DisableHealthChecks = true

	proxy, err := NewProxy(context.Background(), rpcGatewayConfig)
	assert.NoError(t, err)
	assert.NotNil(t, proxy)

	t.Run("path-based request", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, "/wallet/getnowblock", bytes.NewReader([]byte(`{}`)))
		assert.NoError(t, err)

		rr := httptest.NewRecorder()
		proxy.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "/wallet/getnowblock", receivedPath)
		assert.Equal(t, "test-api-key", receivedHeaders.Get("TRON-PRO-API-KEY"))
	})

	t.Run("json-rpc method request", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`{"method":"wallet/getnowblock"}`)))
		assert.NoError(t, err)

		rr := httptest.NewRecorder()
		proxy.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "/wallet/getnowblock", receivedPath)
		assert.Equal(t, "wallet/getnowblock", receivedMethod)
		assert.Equal(t, "test-api-key", receivedHeaders.Get("TRON-PRO-API-KEY"))
	})

	t.Run("invalid json request", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte(`invalid json`)))
		assert.NoError(t, err)

		rr := httptest.NewRecorder()
		proxy.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusBadRequest, rr.Code)
		
		// Parse the response body to check the error message
		var response map[string]interface{}
		err = json.Unmarshal(rr.Body.Bytes(), &response)
		assert.NoError(t, err)
		
		// Check that the response contains the expected JSON-RPC error object
		errorObj, ok := response["error"].(map[string]interface{})
		assert.True(t, ok, "error field should be a map")
		assert.Equal(t, float64(-32000), errorObj["code"])
		assert.Equal(t, "Invalid JSON request", errorObj["message"])
	})
}