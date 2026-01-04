package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)


type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// parseHex parses a hex string to uint64 (optimized)
func parseHex(s string) (uint64, error) {
	if len(s) == 0 {
		return 0, nil
	}

	// Strip 0x/0X prefix fast
	if len(s) > 1 && s[0] == '0' && (s[1]|0x20) == 'x' { // lowercase via bit trick
		s = s[2:]
		if len(s) == 0 {
			return 0, nil
		}
	}

	// Fast validation (byte loop, no bounds branches)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9',
			c >= 'a' && c <= 'f',
			c >= 'A' && c <= 'F':
			continue
		default:
			return 0, fmt.Errorf("invalid hex char '%c' in \"%s\"", c, s)
		}
	}

	return strconv.ParseUint(s, 16, 64)
}


// isBrokenPipeError checks if an error is a broken pipe error
func isBrokenPipeError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "broken pipe") || 
		   strings.Contains(errStr, "connection reset by peer") ||
		   strings.Contains(errStr, "use of closed network connection")
}

// [constructor preamble&callvalue check][function dispatcher][selector matched function logic][STOP]
// 0x6080604052348015600f57600080fd5b506004361060285760003560e01c80632222222214602d575b600080fd5b603460005a60005260206000f35b5056

// 6080604052                    // memory: mstore(0x40, 0x80)
// 348015600f57600080fd5b        // CALLVALUE check and revert if not 0
// 5060043610602857...           // selector parsing
// ...                           // function selector jump logic
// ...                           // function logic (e.g., gasleft, return)
// 5b5056                        // STOP
// 0x6080604052348015600f57600080fd5b50601d80601d6000396000f3fe6040515a8152602081f350
func performGasLeftCall(ctx context.Context, client *http.Client, url string) (uint64, error) {
	const gasLeftCallRaw = `{
		"method": "eth_call",
		"params": [
			{
				"from": "0x1111111111111111111111111111111111111111",
				"to": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"data": "0x22222222",
				"gas": "0xF4240"
			},
			"latest",
			{
				"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": {
					"code": "0x6080604052348015600f57600080fd5b506004361060285760003560e01c80632222222214602d575b600080fd5b603460005a60005260206000f35b5056"
				}
			}
		],
		"id": 1,
		"jsonrpc": "2.0"
	}`

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(gasLeftCallRaw))
	if err != nil {
		return 0, fmt.Errorf("gasLeftCall: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("gasLeftCall: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("gasLeftCall: unexpected status %d", resp.StatusCode)
	}

	var result JSONRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("gasLeftCall: decode: %w", err)
	}
	if result.Error != nil {
		return 0, fmt.Errorf("gasLeftCall: rpc error: code=%d message=%s", result.Error.Code, result.Error.Message)
	}
	if result.Result == "" {
		return 0, fmt.Errorf("gasLeftCall: empty result")
	}

	// Type assert result to string
	resultStr, ok := result.Result.(string)
	if !ok {
		return 0, fmt.Errorf("invalid result type")
	}
	return parseHex(resultStr)
}

