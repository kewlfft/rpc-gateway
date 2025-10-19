package proxy

import (
	"bytes"
	"net/http"
)

type ResponseWriter struct {
	body       *bytes.Buffer
	header     http.Header
	statusCode int
}

func (p *ResponseWriter) Header() http.Header {
	return p.header
}

func (p *ResponseWriter) Write(b []byte) (int, error) {
	return p.body.Write(b)
}

func (p *ResponseWriter) WriteHeader(statusCode int) {
	p.statusCode = statusCode
}

func NewResponseWriter() *ResponseWriter {
	return &ResponseWriter{
		header: http.Header{},
		body:   &bytes.Buffer{},
	}
}
