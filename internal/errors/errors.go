package errors

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// WriteJSONRPCError writes an error response in JSON-RPC format and logs the error
// bodyBytes is used to extract the request ID from the JSON-RPC request
// providerDetails is optional diagnostic information about provider status
func WriteJSONRPCError(w http.ResponseWriter, r *http.Request, message string, status int, bodyBytes []byte, providerDetails ...string) {
	var requestID any

	// Extract request ID from body bytes if provided
	if bodyBytes != nil {
		if len(bodyBytes) > 0 && r.Header.Get("Content-Type") == "application/json" {
			var req map[string]any
			if json.Unmarshal(bodyBytes, &req) == nil && req["jsonrpc"] != nil {
				requestID = req["id"]
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	
	// Check if headers have already been written by checking if status was set
	if w.Header().Get("X-Status-Set") == "" {
		w.Header().Set("X-Status-Set", "true")
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
	logAttrs := []any{
		"message", message,
		"status", status,
		"method", r.Method,
		"path", r.URL.Path,
		"request_id", requestID,
		"remote_addr", r.RemoteAddr,
	}
	if len(providerDetails) > 0 {
		logAttrs = append(logAttrs, "providers", providerDetails)
	}
	slog.Error("JSON-RPC error response", logAttrs...)
} 