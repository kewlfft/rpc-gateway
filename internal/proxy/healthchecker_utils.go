package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	jsonv2 "encoding/json/v2"
)

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// parseHex parses a 0x-prefixed (or bare) hex quantity to uint64.
func parseHex(s string) (uint64, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(s) == 0 {
		return 0, errors.New("empty hex quantity")
	}
	return strconv.ParseUint(s, 16, 64)
}

// rateLimitedError is a health check answered with HTTP 429; wait is the
// provider's Retry-After hint (0 if none).
type rateLimitedError struct{ wait time.Duration }

func (e *rateLimitedError) Error() string { return "rate limited: HTTP 429" }

// statusError describes a non-200 health check response.
func statusError(resp *http.Response) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		return &rateLimitedError{retryAfter(resp.Header)}
	}
	return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
}

// isBrokenPipeError checks if an error is a broken pipe / client disconnect error.
func isBrokenPipeError(err error) bool {
	return errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, net.ErrClosed)
}

// sha256PrecompileProbe is eth_call against the EVM SHA-256 precompile (0x02).
// The precompile returns a deterministic hash of the calldata, proving the node
// executed EVM/precompile logic — not merely stubbed eth_call or echoed input.
const (
	sha256PrecompileInput  = "0xdeadbeef"
	sha256PrecompileExpect = "0x5f78c33274e43fa9de5659265c1d917e25c03722dcb0b8d27db8d5feaa813953"
)

func performEthCallHealthCheck(ctx context.Context, client *http.Client, url string, headers map[string]string) error {
	const ethCallRaw = `{
		"method": "eth_call",
		"params": [
			{
				"to": "0x0000000000000000000000000000000000000002",
				"data": "` + sha256PrecompileInput + `"
			},
			"latest"
		],
		"id": 1,
		"jsonrpc": "2.0"
	}`

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(ethCallRaw))
	if err != nil {
		return fmt.Errorf("ethCallHealth: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ethCallHealth: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ethCallHealth: %w", statusError(resp))
	}

	var result JSONRPCResponse
	if err := jsonv2.UnmarshalRead(resp.Body, &result); err != nil {
		return fmt.Errorf("ethCallHealth: decode: %w", err)
	}
	if result.Error != nil {
		return fmt.Errorf("ethCallHealth: rpc error: code=%d message=%s", result.Error.Code, result.Error.Message)
	}
	if result.Result == nil || result.Result == "" {
		return fmt.Errorf("ethCallHealth: empty result")
	}

	resultStr, ok := result.Result.(string)
	if !ok {
		return fmt.Errorf("invalid result type")
	}
	if !strings.EqualFold(resultStr, sha256PrecompileExpect) {
		return fmt.Errorf("ethCallHealth: unexpected result %q", resultStr)
	}
	return nil
}
