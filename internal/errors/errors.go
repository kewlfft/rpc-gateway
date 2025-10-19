package errors

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	
	"github.com/kewlfft/rpc-gateway/internal/proxy"
)

// WriteJSONRPCError writes an error response in JSON-RPC format and logs the error
func WriteJSONRPCError(w http.ResponseWriter, r *http.Request, message string, status int) {
	var requestID any

	if r.Body != nil {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))

		if len(body) > 0 && r.Header.Get("Content-Type") == "application/json" {
			var req map[string]any
			if json.Unmarshal(body, &req) == nil && req["jsonrpc"] != nil {
				requestID = req["id"]
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	
	// Check if headers have already been written to avoid "superfluous response.WriteHeader call"
	// We can detect this by checking if the response writer has already written headers
	// by using a type assertion to check if it's our custom ResponseWriter
	if rw, ok := w.(*proxy.ResponseWriter); ok {
		// This is our custom ResponseWriter, we can safely call WriteHeader
		w.WriteHeader(status)
	} else {
		// For standard http.ResponseWriter, we need to be more careful
		// Use defer with recover to handle the case where headers were already written
		defer func() {
			if r := recover(); r != nil {
				// Headers were already written, ignore the panic
			}
		}()
		w.WriteHeader(status)
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"error": map[string]any{
			"code":    -32000,
			"message": message,
		},
	})

	// Log the error with relevant context
	slog.Error("JSON-RPC error response",
		"message", message,
		"status", status,
		"method", r.Method,
		"path", r.URL.Path,
		"request_id", requestID,
		"remote_addr", r.RemoteAddr,
	)
} 