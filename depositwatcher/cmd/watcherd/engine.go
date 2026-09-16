package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"depositwatcher/internal/candidates"
	"depositwatcher/internal/chain"
	"depositwatcher/internal/db"
	"depositwatcher/internal/finality"
	"depositwatcher/internal/ledgerclient"
	"depositwatcher/internal/money"
	"depositwatcher/internal/orphaned"
)

// defaultDustFloor is the C2 build spec's own example ("< $1 equiv"),
// overridable via WATCHER_DUST_FLOOR (a decimal string, same convention
// as everywhere else money crosses a boundary in this codebase).
const defaultDustFloor = money.Amount(1_000000)

// defaultContractAddress is the real, verified Binance-Peg BSC-USD
// (USDT_BEP20) mainnet contract -- see internal/chain's own
// ParseTransferLog doc comment for how this was confirmed independently
// of any document's citation. Overridable via WATCHER_CONTRACT_ADDRESS
// for anything else the deposit watcher needs to watch (e.g. a testnet
// token, for a real end-to-end test against a chain that doesn't have
// the real USDT_BEP20 contract at all).
const defaultContractAddress = "0x55d398326f99059fF775485246999027B3197955"

// transferEventTopic is keccak256("Transfer(address,address,uint256)") --
// universal across every ERC20-shaped token, never configuration, since
// it is not specific to any one contract.
var transferEventTopic = common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")

// engine bundles the chain-watching pieces cmd/watcherd's own doc
// comment previously described as "not yet wired in": a real
// multi-provider RPC pool, a finality.Tracker wired with the REAL
// ledgerclient/orphaned implementations (never a fake, unlike this
// project's own tests), and the two background loops
// (chain.RunIngestionLoop, candidates.RunLoop) that make this a live
// service rather than just an HTTP boundary over a database.
type engine struct {
	chainPool    *chain.Pool
	tracker      *finality.Tracker
	ledger       *ledgerclient.Client
	cfg          candidates.Config
	pollInterval time.Duration
}

// newEngineFromEnv builds an engine from WATCHER_RPC_PROVIDERS,
// WATCHER_CONTRACT_ADDRESS (optional), WATCHER_LEDGER_BASE_URL, and
// WATCHER_LEDGER_TOKEN. Returns (nil, nil) -- not an error -- if
// WATCHER_RPC_PROVIDERS is unset: a deployment that only wants the HTTP
// boundary (address assignment, orphaned-deposit review) without the
// live chain-watching engine is a legitimate, already-supported
// configuration (see httpapi.Server's own doc comment on ChainPool/
// Tracker being nil-safe) -- this is an opt-in upgrade to a full
// service, not a new requirement on every deployment.
//
// Deliberately built WITHOUT httpapi.Metrics wired into the tracker/
// ledger client: Metrics is only known once httpapi.NewRouter runs, but
// NewRouter's own reactive gauges (provider_agreement_failures_total)
// need Server.ChainPool already set to read the right value -- attaching
// engine.tracker's/ledger's Metrics field after the fact would need a
// setter neither type has. Reactive event counters (candidates_detected
// _total etc.) simply stay at zero until that's wired up; every other
// behavior here is identical either way. cmd/watcherd's main() sequences
// construction to avoid this ordering problem for ChainPool/Tracker
// themselves (the part that actually matters for detection to work).
func newEngineFromEnv(pool *db.Pool) (*engine, error) {
	raw := os.Getenv("WATCHER_RPC_PROVIDERS")
	if raw == "" {
		return nil, nil
	}

	var providers []chain.Provider
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		name, url, ok := strings.Cut(pair, "=")
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("watcherd: malformed WATCHER_RPC_PROVIDERS entry %q, want name=url", pair)
		}
		client, err := ethclient.DialContext(context.Background(), url)
		if err != nil {
			return nil, fmt.Errorf("watcherd: dialing RPC provider %q (%s): %w", name, url, err)
		}
		providers = append(providers, chain.Provider{Name: name, Client: client})
	}
	// WATCHER_MIN_AGREEMENT is optional -- unset means 0, which
	// chain.NewPool already treats as "use its own built-in default of
	// 2" (chain/pool.go), so omitting this var reproduces today's exact,
	// previously-hardcoded chain.Config{} behavior byte for byte. This
	// only makes chain.Pool's own already-general N-of-M quorum support
	// reachable from configuration -- it introduces no new default and
	// no new algorithm (see chain/agreement.go).
	var minAgreement int
	if raw := os.Getenv("WATCHER_MIN_AGREEMENT"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("watcherd: WATCHER_MIN_AGREEMENT: %w", err)
		}
		minAgreement = parsed
	}
	chainPool, err := chain.NewPool(providers, chain.Config{MinAgreement: minAgreement})
	if err != nil {
		return nil, fmt.Errorf("watcherd: building RPC provider pool: %w", err)
	}
	// effectiveMinAgreement mirrors chain.NewPool's own "<=0 means 2"
	// default resolution (chain/pool.go) -- computed here, once, so
	// Design B's own AsyncMinAgreement (finality.Config, below) uses the
	// SAME effective quorum size Design A's chainPool just applied,
	// rather than silently regressing to NewCandidateConfirmations' own
	// unrelated "<1 means 1" clamp (async.go), which would make async
	// mode's default quorum a single provider -- exactly the
	// height-alone-is-a-vote failure mode this whole redesign exists to
	// prevent.
	effectiveMinAgreement := minAgreement
	if effectiveMinAgreement <= 0 {
		effectiveMinAgreement = 2
	}

	// WATCHER_FINALITY_MODE=async is double-gated behind
	// WATCHER_ALLOW_ASYNC_FINALITY=true, the same convention this
	// project already uses for SCREENING_PROVIDER=always_clean and
	// UPSTREAM_PROVIDER=placeholder -- a safety-relevant, unproven-in-
	// production mode must never be reachable through one mistyped env
	// var. Unset (the default) is Design A, byte-identical to every
	// deployment's behavior before Phase 2.
	asyncFinality := false
	switch mode := os.Getenv("WATCHER_FINALITY_MODE"); mode {
	case "", "sync":
		// Design A, the default.
	case "async":
		if os.Getenv("WATCHER_ALLOW_ASYNC_FINALITY") != "true" {
			return nil, errors.New("watcherd: WATCHER_FINALITY_MODE=async also requires " +
				"WATCHER_ALLOW_ASYNC_FINALITY=true (Design B is not yet proven in production; " +
				"this double gate exists so it can't be reached by one mistyped env var)")
		}
		asyncFinality = true
	default:
		return nil, fmt.Errorf("watcherd: WATCHER_FINALITY_MODE: unrecognized value %q (want \"\", \"sync\", or \"async\")", mode)
	}

	if err := bootstrapCursorIfFresh(context.Background(), chainPool, pool); err != nil {
		return nil, fmt.Errorf("watcherd: %w", err)
	}

	ledgerBaseURL := os.Getenv("WATCHER_LEDGER_BASE_URL")
	ledgerToken := os.Getenv("WATCHER_LEDGER_TOKEN")
	if ledgerBaseURL == "" || ledgerToken == "" {
		return nil, errors.New("watcherd: WATCHER_RPC_PROVIDERS is set, so a live engine was requested, " +
			"but WATCHER_LEDGER_BASE_URL and WATCHER_LEDGER_TOKEN are also required (the engine reports to a real C1)")
	}
	ledger := ledgerclient.New(ledgerBaseURL, ledgerToken)

	contractAddress := os.Getenv("WATCHER_CONTRACT_ADDRESS")
	if contractAddress == "" {
		contractAddress = defaultContractAddress
	}

	dustFloor := defaultDustFloor
	if raw := os.Getenv("WATCHER_DUST_FLOOR"); raw != "" {
		parsed, err := money.ParseDecimal(raw)
		if err != nil {
			return nil, fmt.Errorf("watcherd: WATCHER_DUST_FLOOR: %w", err)
		}
		dustFloor = parsed
	}

	pollInterval := candidates.DefaultInterval
	if raw := os.Getenv("WATCHER_POLL_INTERVAL"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("watcherd: WATCHER_POLL_INTERVAL: %w", err)
		}
		pollInterval = parsed
	}

	e := &engine{
		chainPool:    chainPool,
		ledger:       ledger,
		pollInterval: pollInterval,
		cfg: candidates.Config{
			ContractAddress: common.HexToAddress(contractAddress),
			TransferTopic:   transferEventTopic,
			DustFloor:       dustFloor,
			AsyncFinality:   asyncFinality,
		},
	}

	tracker, err := finality.New(finality.Config{
		ContractAddress:         e.cfg.ContractAddress,
		TransferTopic:           e.cfg.TransferTopic,
		OnFinal:                 ledger.ReportDepositFinal,
		ReorgReporter:           ledger,
		OrphanedDepositRecorder: orphanedRecorder{pool: pool, ledger: ledger},
		AsyncMinAgreement:       effectiveMinAgreement,
	})
	if err != nil {
		return nil, fmt.Errorf("watcherd: building finality tracker: %w", err)
	}
	e.tracker = tracker
	return e, nil
}

// bootstrapCursorIfFresh seeds ingestion_cursor.last_scanned to just
// behind the current chain tip on a genuinely fresh watcher database
// (last_scanned still at migration 0003's seeded default, 0).
// chain.RunIngestionLoop walks forward one height at a time with no way
// to skip ahead, and on a real chain with tens or hundreds of millions
// of blocks of history, starting from block 1 would mean hours or days
// before this service ever reached the present -- discovered by actually
// running this against live BSC testnet, not from reading the code. A
// watcher's job is to observe deposits from the moment it starts
// watching, not audit a chain's entire past. On any real chain,
// last_scanned == 0 unambiguously means "never bootstrapped": genesis
// was years ago.
func bootstrapCursorIfFresh(ctx context.Context, chainPool *chain.Pool, pool *db.Pool) error {
	current, err := chain.LastScannedHeight(ctx, pool)
	if err != nil {
		return err
	}
	if current != 0 {
		return nil
	}
	tip, err := chainPool.LatestBlockHeader(ctx)
	if err != nil {
		return fmt.Errorf("fetching chain tip to bootstrap the ingestion cursor: %w", err)
	}
	height := tip.Number.Uint64()
	if height == 0 {
		return nil
	}
	start := height - 1
	if _, err := pool.Exec(ctx, `UPDATE ingestion_cursor SET last_scanned = $1 WHERE id = 1`, int64(start)); err != nil {
		return fmt.Errorf("seeding ingestion cursor to %d: %w", start, err)
	}
	slog.Info("watcherd: bootstrapped a fresh ingestion cursor to the current chain tip", "height", start)
	return nil
}

// run starts both background loops (chain.RunIngestionLoop,
// candidates.RunLoop) and blocks until ctx is cancelled.
func (e *engine) run(ctx context.Context, pool *db.Pool) {
	go func() {
		cfg := chain.IngestionConfig{Interval: e.pollInterval}
		if err := chain.RunIngestionLoop(ctx, e.chainPool, pool, cfg); err != nil && ctx.Err() == nil {
			// RunIngestionLoop only returns non-nil for a cancelled
			// context in normal operation (per-tick errors are logged
			// and retried, never fatal here) -- surfacing anything else
			// loudly rather than letting the process silently stop
			// ingesting blocks.
			panic(fmt.Sprintf("watcherd: ingestion loop exited unexpectedly: %v", err))
		}
	}()
	go func() {
		if err := candidates.RunLoop(ctx, e.chainPool, pool, e.ledger, e.tracker, e.cfg, e.pollInterval); err != nil && ctx.Err() == nil {
			panic(fmt.Sprintf("watcherd: candidate loop exited unexpectedly: %v", err))
		}
	}()
}

// orphanedRecorder implements finality.OrphanedDepositRecorder: fetches
// the order's current state from C1 (best-effort -- "unknown" if even
// that fails) and persists via orphaned.Record. Same shape as
// internal/replay's own harness adapter, since this is the real,
// production version of exactly that.
type orphanedRecorder struct {
	pool   *db.Pool
	ledger *ledgerclient.Client
}

func (o orphanedRecorder) RecordOrphanedDeposit(ctx context.Context, c finality.Candidate, c1Error error) error {
	state := "unknown"
	if order, err := o.ledger.GetOrder(ctx, c.ExternalID); err == nil {
		state = order.State
	}
	return orphaned.Record(ctx, o.pool, orphaned.Deposit{
		OrderID: c.OrderID, ExternalID: c.ExternalID, TxHash: c.TxHash.Hex(), LogIndex: int(c.LogIndex),
		Amount: int64(c.Amount), DetectedAt: time.Now().UTC(), OrderStateAtDetection: state,
	})
}
