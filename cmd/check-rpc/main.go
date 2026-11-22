// Probe every HTTP/WS URL in an rpc-gateway style config and report failures.
//
//	go run -C ~/dev/rpc-gateway ./cmd/check-rpc [flags] [config.yml]
//
// Exit 1 if any URL fails. WARN notes do not fail.
package main

import (
	"bytes"
	"cmp"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	jsonv2 "encoding/json/v2"

	"github.com/gorilla/websocket"
	"github.com/kewlfft/rpc-gateway/internal/rpcgateway"
	"gopkg.in/yaml.v3"
)

const burstN = 5

var (
	deadTx   = "0x0000000000000000000000000000000000000000000000000000000000000001"
	deadAddr = "0x0000000000000000000000000000000000000001"
	blockReq = []byte(`{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`)
)

type urlItem struct {
	Path, Name, Kind, URL, ChainType string
	Headers                          map[string]string
}

type result struct {
	urlItem
	OK            bool
	Ms            int64
	Result, Error string
	Flags         []string
}

type burstOut struct {
	ok     bool
	result string
	issue  string
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func containsAny(s string, needles ...string) bool {
	s = strings.ToLower(s)
	return slices.ContainsFunc(needles, func(n string) bool { return strings.Contains(s, n) })
}

func errJSON(errObj any) string {
	b, _ := jsonv2.Marshal(errObj)
	return string(b)
}

func rpcErr(data map[string]any) any {
	if data == nil {
		return nil
	}
	if e, ok := data["error"]; ok && e != nil {
		return e
	}
	return nil
}

func classify(status int, raw string, errObj any, transport error) string {
	if transport != nil {
		return "error: " + trunc(transport.Error(), 80)
	}
	blob := raw + " " + errJSON(errObj)
	if status == 429 || containsAny(blob, "rate", "limit", "too many", "quota", "throttl") {
		return "rate-limited"
	}
	if status == 401 || status == 403 || containsAny(blob,
		"archive", "historical", "personal token", "method not found", "method not supported",
		"not available", "not supported", "disabled", "unauthorized", "filter not found", "free tier") {
		return "unsupported"
	}
	if m, ok := errObj.(map[string]any); ok {
		if c, ok := m["code"].(float64); ok && (c == -32601 || c == -32602) {
			return "unsupported"
		}
	}
	if status != 0 && (status < 200 || status >= 300) {
		return fmt.Sprintf("HTTP %d", status)
	}
	if errObj != nil {
		return "rpc-error: " + trunc(errJSON(errObj), 80)
	}
	return "fail: " + trunc(raw, 80)
}

func flagMethod(name string, status int, raw string, data map[string]any, err error) string {
	if err != nil {
		return name + " error: " + trunc(err.Error(), 100)
	}
	errObj := rpcErr(data)
	badHTTP := status != 0 && (status < 200 || status >= 300)
	if badHTTP || errObj != nil {
		label := classify(status, raw, errObj, nil)
		if label == "rate-limited" || label == "unsupported" {
			return "no " + name + " (" + label + ")"
		}
		return name + " unexpected: " + trunc(label+" "+raw, 120)
	}
	if data == nil {
		return name + " invalid json"
	}
	if _, ok := data["result"]; !ok {
		return name + " missing result"
	}
	return ""
}

func burstSummary(kind string, issues []string) []string {
	if len(issues) == 0 {
		return nil
	}
	counts := map[string]int{}
	var order []string
	for _, i := range issues {
		if counts[i] == 0 {
			order = append(order, i)
		}
		counts[i]++
	}
	parts := make([]string, len(order))
	for i, k := range order {
		parts[i] = fmt.Sprintf("%dx %s", counts[k], k)
	}
	return []string{fmt.Sprintf("%s burst %d/%d failed: %s", kind, len(issues), burstN, strings.Join(parts, "; "))}
}

func collectBurst(kind string, outs []burstOut) (string, []string, error) {
	var block string
	var okN int
	var issues []string
	for _, o := range outs {
		if o.ok {
			okN++
			if block == "" {
				block = o.result
			}
			continue
		}
		issues = append(issues, o.issue)
	}
	if okN == 0 {
		msg := "all concurrent eth_blockNumber failed"
		if len(issues) > 0 {
			msg += ": " + issues[0]
		}
		return "", nil, fmt.Errorf("%s", trunc(msg, 200))
	}
	return block, burstSummary(kind, issues), nil
}

func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body []byte) (int, map[string]any, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	var data map[string]any
	_ = jsonv2.Unmarshal(raw, &data)
	return res.StatusCode, data, raw, nil
}

func dialWS(ctx context.Context, url string, timeout time.Duration) (*websocket.Conn, int, error) {
	d := websocket.Dialer{HandshakeTimeout: timeout}
	conn, resp, err := d.DialContext(ctx, url, nil)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if err != nil {
		return nil, status, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	return conn, status, nil
}

func readRPCByID(conn *websocket.Conn, wantID int) (map[string]any, error) {
	for {
		var data map[string]any
		if err := conn.ReadJSON(&data); err != nil {
			return nil, err
		}
		if id, ok := data["id"].(float64); ok && int(id) == wantID {
			return data, nil
		}
	}
}

func callWS(conn *websocket.Conn, id int, method string, params []any) (map[string]any, error) {
	if err := conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	return readRPCByID(conn, id)
}

func httpBurst(ctx context.Context, item urlItem, client *http.Client) (string, []string, error) {
	outs := make([]burstOut, burstN)
	var wg sync.WaitGroup
	for i := range outs {
		wg.Go(func() {
			st, d, raw, err := postJSON(ctx, client, item.URL, item.Headers, blockReq)
			if err != nil || st < 200 || st >= 300 || rpcErr(d) != nil || d == nil {
				outs[i].issue = classify(st, string(raw), rpcErr(d), err)
				return
			}
			res, ok := d["result"]
			if !ok {
				outs[i].issue = "missing result"
				return
			}
			outs[i].ok, outs[i].result = true, trunc(fmt.Sprint(res), 48)
		})
	}
	wg.Wait()
	return collectBurst("http", outs)
}

func wsBurst(ctx context.Context, item urlItem, timeout time.Duration) (string, []string, error) {
	outs := make([]burstOut, burstN)
	var wg sync.WaitGroup
	for i := range outs {
		wg.Go(func() {
			conn, status, err := dialWS(ctx, item.URL, timeout)
			if err != nil {
				outs[i].issue = classify(status, "", nil, err)
				return
			}
			defer conn.Close()
			msg, err := callWS(conn, 1, "eth_blockNumber", []any{})
			if err != nil || rpcErr(msg) != nil {
				outs[i].issue = classify(0, errJSON(rpcErr(msg)), rpcErr(msg), err)
				return
			}
			res, ok := msg["result"]
			if !ok {
				outs[i].issue = "missing result"
				return
			}
			outs[i].ok, outs[i].result = true, trunc(fmt.Sprint(res), 48)
		})
	}
	wg.Wait()
	return collectBurst("ws", outs)
}

func httpProbe(ctx context.Context, item urlItem, client *http.Client) (string, []string, error) {
	switch item.ChainType {
	case "tron":
		st, d, raw, err := postJSON(ctx, client, strings.TrimRight(item.URL, "/")+"/wallet/getnowblock", item.Headers, []byte("{}"))
		if err != nil {
			return "", nil, err
		}
		if st < 200 || st >= 300 {
			return "", nil, fmt.Errorf("HTTP %d: %s", st, trunc(string(raw), 140))
		}
		if _, ok := d["block_header"]; !ok {
			if _, ok := d["blockID"]; !ok {
				return "", nil, fmt.Errorf("unexpected tron body")
			}
		}
		return "tron-block", nil, nil

	case "solana":
		st, d, raw, err := postJSON(ctx, client, item.URL, item.Headers, []byte(`{"jsonrpc":"2.0","id":1,"method":"getHealth","params":[]}`))
		if err != nil {
			return "", nil, err
		}
		if st < 200 || st >= 300 {
			return "", nil, fmt.Errorf("HTTP %d: %s", st, trunc(string(raw), 140))
		}
		if e := rpcErr(d); e != nil {
			return "", nil, fmt.Errorf("%s", trunc(errJSON(e), 160))
		}
		res, ok := d["result"]
		if !ok {
			return "", nil, fmt.Errorf("no result: %s", trunc(string(raw), 120))
		}
		if fmt.Sprint(res) != "ok" {
			return "", nil, fmt.Errorf("getHealth=%v", res)
		}
		return trunc(fmt.Sprint(res), 48), nil, nil
	}

	out, flags, err := httpBurst(ctx, item, client)
	if err != nil {
		return "", nil, err
	}
	for _, p := range []struct {
		name string
		body []byte
	}{
		{"eth_getTransactionReceipt", []byte(`{"jsonrpc":"2.0","id":2,"method":"eth_getTransactionReceipt","params":["` + deadTx + `"]}`)},
		{"eth_getLogs", []byte(`{"jsonrpc":"2.0","id":3,"method":"eth_getLogs","params":[{"fromBlock":"latest","toBlock":"latest","address":"` + deadAddr + `"}]}`)},
	} {
		st, d, raw, e := postJSON(ctx, client, item.URL, item.Headers, p.body)
		if f := flagMethod(p.name, st, string(raw), d, e); f != "" {
			flags = append(flags, f)
		}
	}
	return out, flags, nil
}

func wsProbe(ctx context.Context, item urlItem, timeout time.Duration) (string, []string, error) {
	if item.ChainType == "solana" {
		conn, _, err := dialWS(ctx, item.URL, timeout)
		if err != nil {
			return "", nil, err
		}
		defer conn.Close()
		msg, err := callWS(conn, 1, "slotSubscribe", []any{})
		if err != nil {
			return "", nil, err
		}
		if e := rpcErr(msg); e != nil {
			return "", nil, fmt.Errorf("%s", trunc(errJSON(e), 160))
		}
		res, ok := msg["result"]
		if !ok {
			return "", nil, fmt.Errorf("no result")
		}
		return trunc(fmt.Sprint(res), 48), nil, nil
	}

	block, flags, err := wsBurst(ctx, item, timeout)
	if err != nil {
		return "", nil, err
	}
	conn, _, err := dialWS(ctx, item.URL, timeout)
	if err != nil {
		return block, append(flags, "subscribe dial: "+trunc(err.Error(), 100)), nil
	}
	defer conn.Close()

	sub, err := callWS(conn, 2, "eth_subscribe", []any{"newHeads"})
	if err != nil {
		return block, append(flags, "eth_subscribe error: "+trunc(err.Error(), 100)), nil
	}
	if e := rpcErr(sub); e != nil {
		return block, append(flags, "eth_subscribe: "+trunc(errJSON(e), 100)), nil
	}
	subID, ok := sub["result"]
	if !ok || fmt.Sprint(subID) == "" {
		return block, append(flags, "eth_subscribe: no subscription id"), nil
	}

	msg, err := callWS(conn, 3, "eth_getTransactionReceipt", []any{deadTx})
	if f := flagMethod("eth_getTransactionReceipt", 0, "", msg, err); f != "" {
		flags = append(flags, f)
	}
	return trunc(fmt.Sprintf("%v+sub:%v", block, subID), 48), flags, nil
}

func probeOne(item urlItem, timeout time.Duration, client *http.Client) result {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	t0 := time.Now()
	var val string
	var flags []string
	var err error
	if item.Kind == "http" {
		val, flags, err = httpProbe(ctx, item, client)
	} else {
		val, flags, err = wsProbe(ctx, item, timeout)
	}
	r := result{urlItem: item, Ms: time.Since(t0).Milliseconds(), Flags: flags}
	if err != nil {
		r.Error = trunc(err.Error(), 200)
		return r
	}
	r.OK, r.Result = true, val
	return r
}

func collectURLs(cfg rpcgateway.RPCGatewayConfig) []urlItem {
	var out []urlItem
	for _, p := range cfg.Proxies {
		ct := cmp.Or(p.ChainType, "evm")
		for _, t := range p.Targets {
			if u := t.Connection.HTTP.URL; u != "" {
				out = append(out, urlItem{Path: p.Path, Name: t.Name, Kind: "http", URL: u, Headers: t.Connection.HTTP.Headers, ChainType: ct})
			}
			if u := t.Connection.WebSocket.URL; u != "" {
				out = append(out, urlItem{Path: p.Path, Name: t.Name, Kind: "websocket", URL: u, ChainType: ct})
			}
		}
	}
	return out
}

func main() {
	timeout := flag.Duration("timeout", 4*time.Second, "per-URL probe timeout")
	concurrency := flag.Int("c", 20, "max concurrent probes")
	home, _ := os.UserHomeDir()
	defaultCfg := filepath.Join(home, ".config", "bots", "rpc_config.yml")
	probesHelp := fmt.Sprintf(`probes:
  evm HTTP  %d× concurrent eth_blockNumber (WARN if any blocked) + eth_getTransactionReceipt + eth_getLogs
  evm WS    %d× concurrent dial+eth_blockNumber (WARN if any blocked) + eth_subscribe(newHeads) + eth_getTransactionReceipt
  solana    HTTP getHealth / WS slotSubscribe
  tron      HTTP /wallet/getnowblock
`, burstN, burstN)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: check-rpc [flags] [config.yml]\n\nDefault: %s\n\n%s", defaultCfg, probesHelp)
		flag.PrintDefaults()
	}
	flag.Parse()

	cfgPath := defaultCfg
	if flag.NArg() > 0 {
		cfgPath = flag.Arg(0)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	var cfg rpcgateway.RPCGatewayConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	urls := collectURLs(cfg)
	*concurrency = max(*concurrency, 1)

	fmt.Printf("config: %s\nurls: %d  timeout: %s  concurrency: %d\n%s\n", cfgPath, len(urls), timeout, *concurrency, probesHelp)

	client := &http.Client{Timeout: *timeout}
	sem := make(chan struct{}, *concurrency)
	results := make([]result, len(urls))
	var wg sync.WaitGroup
	for i, item := range urls {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = probeOne(item, *timeout, client)
		})
	}
	wg.Wait()

	slices.SortFunc(results, func(a, b result) int {
		if c := cmp.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return cmp.Compare(a.Kind, b.Kind)
	})

	fail, warned := 0, 0
	for _, r := range results {
		status := "OK  "
		switch {
		case !r.OK:
			status, fail = "FAIL", fail+1
		case len(r.Flags) > 0:
			status, warned = "WARN", warned+1
		}
		fmt.Printf("%s %-4s %-16s %-9s %5dms  %s\n", status, r.Path, r.Name, r.Kind, r.Ms, r.URL)
		if !r.OK {
			fmt.Printf("     → %s\n", r.Error)
		}
		for _, f := range r.Flags {
			fmt.Printf("     ⚠ %s\n", f)
		}
	}
	fmt.Printf("\n%d ok, %d warn, %d fail\n", len(results)-fail-warned, warned, fail)
	if fail > 0 {
		os.Exit(1)
	}
}
