// Command fixedfloat-probe is a standalone diagnostic tool, not part of
// relayd's own production surface -- FixedFloat's own sideshift-probe
// sibling. It talks to FixedFloat's real, production API
// (https://ff.io/api/v2) directly, over its own raw HTTP calls,
// deliberately NOT reusing internal/upstream/fixedfloat.go's own
// FixedFloatProvider, for the same reason sideshift-probe doesn't reuse
// SideshiftProvider: this tool's whole purpose is to print the FULL raw
// JSON response at every step, including the real currency codes
// (internal/upstream/fixedfloat.go's own FixedFloatConfig doc comment
// flags USDTTRC20Ccy/USDTBEP20Ccy as never independently verified
// against a real response -- only inferred from public pages) and
// whatever else a real response carries beyond what that file's own
// typed ffOrderData captures.
//
// SAFETY: this tool creates a real FixedFloat order (a real, harmless,
// zero-cost API call -- no money moves) and prints a real deposit
// address. It never sends, funds, or moves anything itself, and never
// will -- funding that address with real crypto is a separate, manual
// action only the operator running this tool can take, using their own
// wallet. This tool only reads and reports what FixedFloat's own API
// says.
//
// Usage:
//
//	# Step 1: list real currency codes (find the right USDT ones for TRC20/BEP20).
//	go run ./cmd/fixedfloat-probe -ccies
//
//	# Step 2: create a real, unfunded order once you know the real codes.
//	go run ./cmd/fixedfloat-probe -from USDTTRC -to USDTBSC -amount 10 -destination <your-real-wallet>
//
//	# Step 3 (after YOU send real funds to the printed deposit address,
//	# using your own wallet -- this tool does not do that step):
//	go run ./cmd/fixedfloat-probe -poll <id-from-step-2> -token <token-from-step-2>
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const fixedFloatBaseURL = "https://ff.io/api/v2"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fixedfloat-probe:", err)
		os.Exit(1)
	}
}

func run() error {
	listCcies := flag.Bool("ccies", false, "list real currency codes instead of creating an order")
	from := flag.String("from", "", "FixedFloat's own currency code for the FROM asset (see -ccies output)")
	to := flag.String("to", "", "FixedFloat's own currency code for the TO asset (see -ccies output)")
	amount := flag.String("amount", "10", "decimal amount to quote/order, in the FROM asset")
	destination := flag.String("destination", "", "YOUR real wallet address on the destination chain -- where FixedFloat will send the swapped-out funds")
	pollID := flag.String("poll", "", "instead of creating a new order, poll an existing order id until it reaches a terminal status")
	pollToken := flag.String("token", "", "the security token CreateOrder printed alongside -poll's id -- required together")
	pollInterval := flag.Duration("poll-interval", 15*time.Second, "how often to re-check the order while polling")
	pollTimeout := flag.Duration("poll-timeout", 30*time.Minute, "give up polling after this long")
	flag.Parse()

	apiKey := os.Getenv("FIXEDFLOAT_API_KEY")
	apiSecret := os.Getenv("FIXEDFLOAT_API_SECRET")
	if apiKey == "" || apiSecret == "" {
		return fmt.Errorf("FIXEDFLOAT_API_KEY and FIXEDFLOAT_API_SECRET must both be set (never pass these as flags -- they'd end up in your shell history)")
	}
	client := &fixedFloatClient{apiKey: apiKey, apiSecret: apiSecret, http: &http.Client{Timeout: 20 * time.Second}}

	if *listCcies {
		return listCurrencies(client)
	}
	if *pollID != "" {
		if *pollToken == "" {
			return fmt.Errorf("-token is required alongside -poll (FixedFloat needs both id and token to check status)")
		}
		return pollOrder(client, *pollID, *pollToken, *pollInterval, *pollTimeout)
	}
	if *from == "" || *to == "" {
		return fmt.Errorf("-from and -to are required (run -ccies first to find the real currency codes)")
	}
	if *destination == "" {
		return fmt.Errorf("-destination is required -- YOUR real wallet address on the settle chain")
	}
	return createOrder(client, *from, *to, *amount, *destination)
}

type fixedFloatClient struct {
	apiKey    string
	apiSecret string
	http      *http.Client
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// do posts body (or an empty body if nil) to path, signed exactly the
// way internal/upstream/fixedfloat.go's own do() does, and returns the
// raw response bytes verbatim -- unparsed, so every field a real
// response carries is visible, not just the ones production code
// currently maps.
func (c *fixedFloatClient) do(ctx context.Context, path string, body any) (status int, respBody []byte, err error) {
	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding request body: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fixedFloatBaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-API-KEY", c.apiKey)
	req.Header.Set("X-API-SIGN", sign(c.apiSecret, payload))

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("reading response for POST %s: %w", path, err)
	}
	return resp.StatusCode, respBody, nil
}

func prettyPrint(label string, body []byte) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		fmt.Printf("--- %s (raw, not valid JSON) ---\n%s\n", label, body)
		return
	}
	pretty, _ := json.MarshalIndent(v, "", "  ")
	fmt.Printf("--- %s ---\n%s\n", label, pretty)
}

func listCurrencies(c *fixedFloatClient) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fmt.Println("Requesting the real currency list from FixedFloat...")
	fmt.Println()
	status, body, err := c.do(ctx, "/ccies", nil)
	if err != nil {
		return fmt.Errorf("listing currencies: %w", err)
	}
	prettyPrint(fmt.Sprintf("POST /ccies (HTTP %d)", status), body)
	if status < 200 || status >= 300 {
		return fmt.Errorf("currency list request failed with HTTP %d -- see response above", status)
	}

	var envelope struct {
		Code int `json:"code"`
		Data []struct {
			Code    string `json:"code"`
			Coin    string `json:"coin"`
			Network string `json:"network"`
			Name    string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		fmt.Println()
		fmt.Println("USDT-looking entries (cross-check against TRC20/TRON and BEP20/BSC/BNB):")
		for _, e := range envelope.Data {
			if strings.Contains(strings.ToUpper(e.Coin), "USDT") || strings.Contains(strings.ToUpper(e.Name), "USDT") {
				fmt.Printf("  code=%-12s coin=%-8s network=%-10s name=%s\n", e.Code, e.Coin, e.Network, e.Name)
			}
		}
	}
	return nil
}

func createOrder(c *fixedFloatClient, fromCcy, toCcy, amount, destination string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fmt.Printf("Requesting a real quote: %s -> %s, amount %s...\n\n", fromCcy, toCcy, amount)
	status, body, err := c.do(ctx, "/price", map[string]string{
		"type": "fixed", "fromCcy": fromCcy, "toCcy": toCcy, "direction": "from", "amount": amount,
	})
	if err != nil {
		return fmt.Errorf("requesting price: %w", err)
	}
	prettyPrint(fmt.Sprintf("POST /price (HTTP %d)", status), body)
	if status < 200 || status >= 300 {
		return fmt.Errorf("price request failed with HTTP %d -- see response above (if it mentions a minimum, retry with -amount set higher)", status)
	}

	fmt.Println()
	fmt.Println("Creating a real, unfunded order from that price (no money moves yet)...")
	fmt.Println()

	status, body, err = c.do(ctx, "/create", map[string]string{
		"type": "fixed", "fromCcy": fromCcy, "toCcy": toCcy, "direction": "from", "amount": amount, "toAddress": destination,
	})
	if err != nil {
		return fmt.Errorf("creating order: %w", err)
	}
	prettyPrint(fmt.Sprintf("POST /create (HTTP %d)", status), body)
	if status < 200 || status >= 300 {
		return fmt.Errorf("order creation failed with HTTP %d -- see response above", status)
	}

	var envelope struct {
		Code int `json:"code"`
		Data struct {
			ID    string `json:"id"`
			Token string `json:"token"`
			From  struct {
				Address string `json:"address"`
				Amount  string `json:"amount"`
				Code    string `json:"code"`
			} `json:"from"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("parsing order response: %w", err)
	}

	fmt.Println()
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("Real order created. NOTHING has been funded -- no money has moved.")
	fmt.Println()
	fmt.Printf("  order id:        %s\n", envelope.Data.ID)
	fmt.Printf("  order token:     %s (needed together with the id to poll status)\n", envelope.Data.Token)
	fmt.Printf("  deposit address: %s\n", envelope.Data.From.Address)
	fmt.Printf("  deposit coin:    %s\n", envelope.Data.From.Code)
	fmt.Printf("  expected amount: %s\n", envelope.Data.From.Amount)
	fmt.Println()
	fmt.Println("To actually test the real vendor end to end, YOU would need to send")
	fmt.Println("real crypto to that deposit address, using your own wallet -- this tool")
	fmt.Println("will never do that for you, and you should not ask any AI agent to.")
	fmt.Println()
	fmt.Printf("Once (and only if) you've sent funds yourself, watch it with:\n\n")
	fmt.Printf("  go run ./cmd/fixedfloat-probe -poll %s -token %s\n\n", envelope.Data.ID, envelope.Data.Token)
	fmt.Println(strings.Repeat("=", 72))
	return nil
}

func pollOrder(c *fixedFloatClient, id, token string, interval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	lastStatus := ""
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		status, body, err := c.do(ctx, "/order", map[string]string{"id": id, "token": token})
		cancel()
		if err != nil {
			return fmt.Errorf("polling order %s: %w", id, err)
		}
		if status < 200 || status >= 300 {
			prettyPrint(fmt.Sprintf("POST /order (HTTP %d)", status), body)
			return fmt.Errorf("poll failed with HTTP %d", status)
		}

		var envelope struct {
			Data struct {
				Status string `json:"status"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &envelope)

		if envelope.Data.Status != lastStatus {
			fmt.Printf("[%s] status changed: %q -> %q\n", time.Now().UTC().Format(time.RFC3339), lastStatus, envelope.Data.Status)
			prettyPrint(fmt.Sprintf("POST /order (HTTP %d)", status), body)
			lastStatus = envelope.Data.Status
		}

		switch envelope.Data.Status {
		case "DONE", "EXPIRED", "EMERGENCY":
			fmt.Println()
			fmt.Println("Reached a terminal status -- stopping. The raw JSON above is the")
			fmt.Println("real, complete response shape at this status; compare it against")
			fmt.Println("internal/upstream/fixedfloat.go's own ffOrderData and")
			fmt.Println("statusFromFixedFloat to see what (if anything) needs updating.")
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("gave up after %s still in status %q", timeout, envelope.Data.Status)
		}
		time.Sleep(interval)
	}
}
