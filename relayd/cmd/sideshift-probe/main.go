// Command sideshift-probe is a standalone diagnostic tool, not part of
// relayd's own production surface: it talks to SideShift's real,
// production API (https://sideshift.ai/api/v2) directly, over its own
// raw HTTP calls -- deliberately NOT reusing
// internal/upstream/sideshift.go's own SideshiftProvider -- because its
// whole purpose is to print the FULL raw JSON response at every step,
// including fields that file's own typed sideShiftShiftResponse does
// not capture (see that file's own top-of-file doc comment: only
// status="waiting" has ever been observed live, and no output-tx-hash
// field has ever been seen, because the one real shift created while
// building that integration was deliberately never funded). This tool
// exists to close that gap by observing a real shift driven all the way
// to settled.
//
// SAFETY: this tool creates a real SideShift shift (a real, harmless,
// zero-cost API call -- no money moves) and prints a real deposit
// address. It never sends, funds, or moves anything itself, and never
// will -- funding that address with real crypto is a separate, manual
// action only the operator running this tool can take, using their own
// wallet. This tool only reads and reports what SideShift's own API
// says.
//
// Usage:
//
//	# Step 1: create a real, unfunded shift and see its own deposit address.
//	go run ./cmd/sideshift-probe -direction trc20-to-bep20 -amount 10 -destination <your-real-BSC-wallet>
//
//	# Step 2 (after YOU send real funds to the printed deposit address,
//	# using your own wallet -- this tool does not do that step):
//	go run ./cmd/sideshift-probe -poll <shift-id-from-step-1>
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const sideshiftBaseURL = "https://sideshift.ai/api/v2"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sideshift-probe:", err)
		os.Exit(1)
	}
}

func run() error {
	direction := flag.String("direction", "trc20-to-bep20", `"trc20-to-bep20" or "bep20-to-trc20"`)
	amount := flag.String("amount", "30", "decimal amount to quote/shift, in the FROM asset -- SideShift's own real quoted minimum for USDT_TRC20->USDT_BEP20 was 28.44 at last check, confirmed live")
	destination := flag.String("destination", "", "YOUR real wallet address on the destination chain -- where SideShift will send the swapped-out funds")
	pollID := flag.String("poll", "", "instead of creating a new shift, poll an existing shift id until it reaches a terminal status")
	pollInterval := flag.Duration("poll-interval", 15*time.Second, "how often to re-check the shift while polling")
	pollTimeout := flag.Duration("poll-timeout", 30*time.Minute, "give up polling after this long")
	flag.Parse()

	secret := os.Getenv("SIDESHIFT_SECRET")
	affiliateID := os.Getenv("SIDESHIFT_AFFILIATE_ID")
	if secret == "" || affiliateID == "" {
		return fmt.Errorf("SIDESHIFT_SECRET and SIDESHIFT_AFFILIATE_ID must both be set (never pass these as flags -- they'd end up in your shell history)")
	}
	client := &sideshiftClient{secret: secret, http: &http.Client{Timeout: 15 * time.Second}}

	if *pollID != "" {
		return pollShift(client, *pollID, *pollInterval, *pollTimeout)
	}

	var depositCoin, depositNetwork, settleCoin, settleNetwork string
	switch *direction {
	case "trc20-to-bep20":
		depositCoin, depositNetwork = "USDT", "tron"
		settleCoin, settleNetwork = "USDT", "bsc"
	case "bep20-to-trc20":
		depositCoin, depositNetwork = "USDT", "bsc"
		settleCoin, settleNetwork = "USDT", "tron"
	default:
		return fmt.Errorf("-direction must be %q or %q, got %q", "trc20-to-bep20", "bep20-to-trc20", *direction)
	}
	if *destination == "" {
		return fmt.Errorf("-destination is required -- YOUR real wallet address on the settle chain (%s)", settleNetworkLabel(settleNetwork))
	}

	return createShift(client, depositCoin, depositNetwork, settleCoin, settleNetwork, *amount, *destination, affiliateID)
}

func settleNetworkLabel(network string) string {
	if network == "bsc" {
		return "BSC/BEP20"
	}
	return "TRON/TRC20"
}

type sideshiftClient struct {
	secret string
	http   *http.Client
}

// do makes one raw call and returns the response body verbatim
// (unparsed) alongside the HTTP status -- the whole point of this tool
// is to see every field a real response carries, not just the ones
// production code currently maps.
func (c *sideshiftClient) do(ctx context.Context, method, path string, body any) (status int, respBody []byte, err error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encoding request body: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, sideshiftBaseURL+path, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("building request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	// Go's default client identifies itself as "Go-http-client/1.1",
	// which some API gateways' bot-detection auto-flags -- identifying
	// honestly as what this actually is (not spoofing a browser) is
	// standard REST client practice, not evasion.
	req.Header.Set("User-Agent", "relayd-sideshift-probe/1.0 (+diagnostic tool, not a browser)")
	req.Header.Set("x-sideshift-secret", c.secret)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err = io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("reading response for %s %s: %w", method, path, err)
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

func createShift(c *sideshiftClient, depositCoin, depositNetwork, settleCoin, settleNetwork, amount, destination, affiliateID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fmt.Printf("Requesting a real quote: %s (%s) -> %s (%s), amount %s...\n\n", depositCoin, depositNetwork, settleCoin, settleNetwork, amount)

	status, body, err := c.do(ctx, http.MethodPost, "/quotes", map[string]string{
		"depositCoin": depositCoin, "depositNetwork": depositNetwork,
		"settleCoin": settleCoin, "settleNetwork": settleNetwork,
		"depositAmount": amount,
	})
	if err != nil {
		return fmt.Errorf("requesting quote: %w", err)
	}
	prettyPrint(fmt.Sprintf("POST /quotes (HTTP %d)", status), body)
	if status < 200 || status >= 300 {
		return fmt.Errorf("quote request failed with HTTP %d -- see response above (if it mentions a minimum amount, retry with -amount set higher)", status)
	}

	var quote struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &quote); err != nil || quote.ID == "" {
		return fmt.Errorf("quote response has no usable id: %w", err)
	}

	fmt.Println()
	fmt.Println("Creating a real, unfunded shift from that quote (no money moves yet)...")
	fmt.Println()

	status, body, err = c.do(ctx, http.MethodPost, "/shifts/fixed", map[string]string{
		"settleAddress": destination, "quoteId": quote.ID, "affiliateId": affiliateID,
	})
	if err != nil {
		return fmt.Errorf("creating shift: %w", err)
	}
	prettyPrint(fmt.Sprintf("POST /shifts/fixed (HTTP %d)", status), body)
	if status < 200 || status >= 300 {
		return fmt.Errorf("shift creation failed with HTTP %d -- see response above", status)
	}

	var shift struct {
		ID             string `json:"id"`
		DepositAddress string `json:"depositAddress"`
		DepositCoin    string `json:"depositCoin"`
		DepositNetwork string `json:"depositNetwork"`
		DepositAmount  string `json:"depositAmount"`
		DepositMin     string `json:"depositMin"`
		DepositMax     string `json:"depositMax"`
	}
	if err := json.Unmarshal(body, &shift); err != nil {
		return fmt.Errorf("parsing shift response: %w", err)
	}

	fmt.Println()
	fmt.Println(strings.Repeat("=", 72))
	fmt.Println("Real shift created. NOTHING has been funded -- no money has moved.")
	fmt.Println()
	fmt.Printf("  shift id:        %s\n", shift.ID)
	fmt.Printf("  deposit address: %s\n", shift.DepositAddress)
	fmt.Printf("  deposit coin:    %s on %s\n", shift.DepositCoin, shift.DepositNetwork)
	fmt.Printf("  expected amount: %s (min %s, max %s)\n", shift.DepositAmount, shift.DepositMin, shift.DepositMax)
	fmt.Println()
	fmt.Println("To actually test the real vendor end to end, YOU would need to send")
	fmt.Println("real crypto to that deposit address, using your own wallet -- this tool")
	fmt.Println("will never do that for you, and you should not ask any AI agent to.")
	fmt.Println()
	fmt.Printf("Once (and only if) you've sent funds yourself, watch it with:\n\n")
	fmt.Printf("  go run ./cmd/sideshift-probe -poll %s\n\n", shift.ID)
	fmt.Println(strings.Repeat("=", 72))
	return nil
}

func pollShift(c *sideshiftClient, id string, interval, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	lastStatus := ""
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		status, body, err := c.do(ctx, http.MethodGet, "/shifts/"+id, nil)
		cancel()
		if err != nil {
			return fmt.Errorf("polling shift %s: %w", id, err)
		}
		if status < 200 || status >= 300 {
			prettyPrint(fmt.Sprintf("GET /shifts/%s (HTTP %d)", id, status), body)
			return fmt.Errorf("poll failed with HTTP %d", status)
		}

		var parsed struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(body, &parsed)

		if parsed.Status != lastStatus {
			fmt.Printf("[%s] status changed: %q -> %q\n", time.Now().UTC().Format(time.RFC3339), lastStatus, parsed.Status)
			prettyPrint(fmt.Sprintf("GET /shifts/%s (HTTP %d)", id, status), body)
			lastStatus = parsed.Status
		}

		switch parsed.Status {
		case "settled", "refund", "refunding", "expired", "review", "multiple":
			fmt.Println()
			fmt.Println("Reached a terminal-ish status -- stopping. The raw JSON above is the")
			fmt.Println("real, complete response shape at this status; compare it against")
			fmt.Println("internal/upstream/sideshift.go's own sideShiftShiftResponse and")
			fmt.Println("statusFromSideShift to see what (if anything) needs updating.")
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("gave up after %s still in status %q", timeout, parsed.Status)
		}
		time.Sleep(interval)
	}
}
