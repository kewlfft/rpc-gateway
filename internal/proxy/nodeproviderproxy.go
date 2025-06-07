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

	"github.com/pkg/errors"
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
	// Try to get existing transport with read lock first
	p.mu.RLock()
	t, exists := p.transports[timeout]
	p.mu.RUnlock()
	if exists {
		return t
	}

	// If not found, acquire write lock and double-check
	p.mu.Lock()
	defer p.mu.Unlock()
	
	if t, exists = p.transports[timeout]; exists {
		return t
	}

	// Create new transport with optimized settings
	t = &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:    timeout,
			KeepAlive:  30 * time.Second,
			DualStack:  true, // Enable IPv4/IPv6 dual-stack
		}).DialContext,
		TLSHandshakeTimeout:    timeout,
		ResponseHeaderTimeout:  timeout,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:      true,
		MaxConnsPerHost:        100,
		DisableCompression:     true,
		DisableKeepAlives:      false, // Explicitly enable keep-alives
		MaxResponseHeaderBytes: 4096,  // Limit header size
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
	defer buf.Reset()
	return buf.Bytes()
}

func (p *bufferPool) Put(b []byte) {
	p.pool.Put(bytes.NewBuffer(nil)) // reuse empty buffer, discard input
}

var defaultBufferPool = newBufferPool()

func NewNodeProviderProxy(cfg NodeProviderConfig, timeout time.Duration) (*httputil.ReverseProxy, error) {
	target, err := url.Parse(cfg.Connection.HTTP.URL)
	if err != nil {
		return nil, errors.Wrap(err, "cannot parse URL")
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

		// Ensure context is canceled when either timeout occurs or request is done
		go func() {
			<-ctx.Done()
			cancel()
		}()
	}

	// Add custom error handler that returns the error for failover
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		ctx := r.Context()
		statusCode := http.StatusInternalServerError
		
		switch {
		case err == context.DeadlineExceeded:
			statusCode = http.StatusGatewayTimeout
			// Clear any partial response by setting Content-Length to 0
			w.Header().Set("Content-Length", "0")
		case errors.As(err, new(*url.Error)):
			statusCode = http.StatusBadGateway
		}
		
		*r = *r.WithContext(context.WithValue(
			context.WithValue(ctx, "error", err),
			"statusCode", statusCode,
		))
	}

	proxy.Transport = pool.getTransport(timeout)
	proxy.BufferPool = defaultBufferPool

	return proxy, nil
}
