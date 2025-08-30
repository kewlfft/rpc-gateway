package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

const (
	contentType = "Content-Type"
)

type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type JSONRPCResponse struct {
	Jsonrpc string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Result  string        `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

func hexToUint(hexString string) (uint64, error) {
	if len(hexString) >= 2 && hexString[0:2] == "0x" {
		hexString = hexString[2:]
	}
	return strconv.ParseUint(hexString, 16, 64)
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
		return 0, fmt.Errorf("performGasLeftCall: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("performGasLeftCall: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("performGasLeftCall: unexpected status %d", resp.StatusCode)
	}

	var result JSONRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("performGasLeftCall: decode: %w", err)
	}
	if result.Error != nil {
		return 0, fmt.Errorf("performGasLeftCall: rpc error: code=%d message=%s", result.Error.Code, result.Error.Message)
	}
	if result.Result == "" {
		return 0, errors.New("performGasLeftCall: empty result")
	}

	return hexToUint(result.Result)
}

