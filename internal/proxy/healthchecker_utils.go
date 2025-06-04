package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-http-utils/headers"
	"github.com/pkg/errors"
)

type JSONRPCResponse struct {
	Jsonrpc string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Result  string `json:"result"`
}

func hexToUint(hexString string) (uint64, error) {
	hexString = strings.ReplaceAll(hexString, "0x", "")

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
func performGasLeftCall(c context.Context, client *http.Client, url string) (uint64, error) {
	gasLeftCallRaw := `{
        "method": "eth_call",
        "params": [
            {
                "from": "0x1111111111111111111111111111111111111111",
                "to": "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
                "data": "0x22222222",
                "gas": "0x3B9ACA00"
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

	r, err := http.NewRequestWithContext(c, http.MethodPost, url, bytes.NewBufferString(gasLeftCallRaw))
	if err != nil {
		return 0, fmt.Errorf("performGasLeftCall: NewRequestWithContext error: %w", err)
	}

	r.Header.Set(headers.ContentType, "application/json")
	r.Header.Set(headers.UserAgent, userAgent)

	resp, err := client.Do(r)
	if err != nil {
		return 0, fmt.Errorf("performGasLeftCall: client.Do error: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, errors.New("performGasLeftCall: non-200 HTTP response")
	}

	result := &JSONRPCResponse{}
	err = json.NewDecoder(resp.Body).Decode(result)
	if err != nil {
		return 0, fmt.Errorf("performGasLeftCall: json.Decode error: %w", err)
	}

	return hexToUint(result.Result)
}
