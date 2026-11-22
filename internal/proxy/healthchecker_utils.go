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

	jsonv2 "encoding/json/v2"
)

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// parseHex parses a hex string to uint64.
func parseHex(s string) (uint64, error) {
	if len(s) == 0 {
		return 0, nil
	}
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(s) == 0 {
		return 0, nil
	}
	return strconv.ParseUint(s, 16, 64)
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

func performEthCallHealthCheck(ctx context.Context, client *http.Client, url string) error {
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

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("ethCallHealth: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ethCallHealth: unexpected status %d", resp.StatusCode)
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
