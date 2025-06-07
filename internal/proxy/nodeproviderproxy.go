package proxy

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"github.com/kewlfft/rpc-gateway/internal/errors"
	pkgerrors "github.com/pkg/errors"
)

// transportPool manages a pool of transports with different timeouts
type transportPool struct {
	mu         sync.RWMutex
	transports map[time.Duration]*http.Transport
}

var pool = &transportPool{
	transports: make(map[time.Duration]*http.Transport),
}

// getTransport returns a transport with the specified timeout, creating it if necessary
func (p *transportPool) getTransport(timeout time.Duration) *http.Transport {
	p.mu.RLock()
	if t, exists := p.transports[timeout]; exists {
		p.mu.RUnlock()
		return t
	}
	p.mu.RUnlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	if t, exists := p.transports[timeout]; exists {
		return t
	}

	// Create new transport with specified timeout
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   timeout,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		MaxConnsPerHost:       100,
		DisableCompression:    true,
	}

	p.transports[timeout] = t
	return t
}

// bufferPool implements httputil.BufferPool interface
type bufferPool struct {
	pool sync.Pool
}

func newBufferPool() *bufferPool {
	return &bufferPool{
		pool: sync.Pool{
			New: func() interface{} { return new(bytes.Buffer) },
		},
	}
}

func (p *bufferPool) Get() []byte {
	buf := p.pool.Get().(*bytes.Buffer)
	b := buf.Bytes()
	buf.Reset()
	return b
}

func (p *bufferPool) Put(b []byte) {
	buf := bytes.NewBuffer(b)
	buf.Reset()
	p.pool.Put(buf)
}

var defaultBufferPool = newBufferPool()

func NewNodeProviderProxy(cfg NodeProviderConfig, timeout time.Duration) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(cfg.Connection.HTTP.URL)
	if err != nil {
		return nil, pkgerrors.Wrap(err, "cannot parse URL")
	}

	proxy := httputil.NewSingleHostReverseProxy(target)

	// Set up the request for the upstream server
	proxy.Director = func(r *http.Request) {
		r.Host = target.Host
		r.URL.Scheme = target.Scheme
		r.URL.Host = target.Host

		// Add timeout to request context
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		*r = *r.WithContext(ctx)

		// Ensure context is canceled when request is done
		go func() {
			<-r.Context().Done()
			cancel()
		}()
	}

	// Add custom error handler with better error reporting
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		status, message := http.StatusBadGateway, "Bad Gateway"
		switch err {
		case context.DeadlineExceeded:
			status, message = http.StatusGatewayTimeout, "Gateway Timeout"
		case context.Canceled:
			status, message = http.StatusServiceUnavailable, "Service Unavailable"
		}

		// Ensure we write a proper JSON-RPC error response
		w.Header().Set("Content-Type", "application/json")
		errors.WriteJSONRPCError(w, r, message, status)
	}

	// Use transport from pool
	proxy.Transport = pool.getTransport(timeout)

	// Add buffer pool for request/response handling
	proxy.BufferPool = defaultBufferPool

	return proxy, nil
}
