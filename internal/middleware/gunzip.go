package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/kewlfft/rpc-gateway/internal/errors"
)

func Gunzip(next http.Handler) http.Handler {
	fn := func(w http.ResponseWriter, r *http.Request) {
		// Skip if not gzip.
		//
		if !strings.Contains(r.Header.Get("Content-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		body := &bytes.Buffer{}

		g, err := gzip.NewReader(r.Body)
		if err != nil {
			errors.WriteJSONRPCError(w, r, "Failed to decompress request", http.StatusInternalServerError)
			return
		}

		if _, err := io.Copy(body, g); err != nil { // nolint:gosec
			errors.WriteJSONRPCError(w, r, "Failed to read decompressed data", http.StatusInternalServerError)
			return
		}

		r.Header.Del("Content-Encoding")
		r.Body = io.NopCloser(body)
		r.ContentLength = int64(body.Len())

		next.ServeHTTP(w, r)
	}

	return http.HandlerFunc(fn)
}
