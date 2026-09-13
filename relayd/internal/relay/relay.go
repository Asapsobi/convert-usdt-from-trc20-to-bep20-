// Package relay is relayd's own state machine: one row per relay leg,
// tracking it from AWAITING_DEPOSIT through to SETTLED or FAILED. Mirrors
// dispatcher/internal/dispatch's own Store shape (idempotent Create via
// ON CONFLICT DO NOTHING, mark<State> via a conditional UPDATE treating
// "already there" as success, List<State> for the driving loop to scan)
// -- separate Go modules, no shared internal package, same convention as
// every other service here.
//
// Deliberately simpler than dispatch_state in one respect: this package
// does NOT re-track what C1's own order.state already knows (funded,
// screened, held) -- duplicating that would be redundant bookkeeping a
// second source of truth could drift from. A leg's own Status only
// starts changing once relayd itself takes an action C1 has no record
// of (creating the upstream swap order, forwarding funds) -- see
// internal/orchestrate's own doc comment for exactly when each
// transition fires.
//
// REFUND_PENDING/REFUNDED/UNRECOVERABLE (architecture doc §4) are
// deliberately NOT modeled here -- R5, this session's own explicitly
// deferred follow-up. FAILED is this pass's only terminal failure state,
// covering what R5 will later split into REFUNDED vs UNRECOVERABLE.
package relay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"relayd/internal/db"
	"relayd/internal/money"
)

// Direction is which chain the customer deposits on vs. receives on.
type Direction string

const (
	TRC20ToBEP20 Direction = "TRC20_TO_BEP20"
	BEP20ToTRC20 Direction = "BEP20_TO_TRC20"
)

// Status is this leg's own lifecycle stage.
type Status string

const (
	StatusAwaitingDeposit Status = "AWAITING_DEPOSIT"
	StatusForwarding      Status = "FORWARDING"
	StatusForwarded       Status = "FORWARDED"
	StatusSettled         Status = "SETTLED"
	StatusFailed          Status = "FAILED"

	// StatusRefundPending/StatusRefunded/StatusUnrecoverable are R5's own
	// addition (docs/02-architecture/model-f-relay-architecture.md §4's
	// state diagram). REFUND_PENDING means C1's own ledger entries
	// committing to a refund have posted but the on-chain refund transfer
	// has not yet confirmed; REFUNDED means it has. UNRECOVERABLE is a
	// FORWARDED leg whose upstream swap failed after this system's own
	// forward transfer already confirmed on-chain -- see
	// internal/orchestrate/settle.go's own doc comment for why no on-chain
	// lever is left to pull once that happens.
	StatusRefundPending Status = "REFUND_PENDING"
	StatusRefunded      Status = "REFUNDED"
	StatusUnrecoverable Status = "UNRECOVERABLE"
)

// Leg is a relay_legs row.
type Leg struct {
	ID                     int64
	ExternalID             string
	OrderID                int64
	Direction              Direction
	Status                 Status
	CustomerID             string
	DestinationAddress     string
	DepositAddress         string
	AmountIn               money.Amount
	AmountOutExpected      money.Amount
	AmountOutActual        *money.Amount
	UpstreamProviderName   *string
	UpstreamOrderID        *string
	UpstreamDepositAddress *string
	ForwardTxID            *string
	RefundTxID             *string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// ErrLegNotFound means no relay_legs row exists for the given key.
var ErrLegNotFound = errors.New("relay: no such leg")

// ErrNotInExpectedStatus means a mark<State> call's WHERE-status guard
// didn't match -- the leg exists but isn't in the state that transition
// assumes.
var ErrNotInExpectedStatus = errors.New("relay: leg is not in the expected status for this transition")

// Store is relay_legs' own entry point.
type Store struct {
	pool *db.Pool
}

// NewStore wires a Store.
func NewStore(pool *db.Pool) *Store {
	return &Store{pool: pool}
}

const selectSQL = `
	SELECT id, external_id, order_id, direction, status, customer_id, destination_address,
		deposit_address, amount_in, amount_in_asset, amount_out_expected, amount_out_expected_asset,
		amount_out_actual, upstream_provider_name, upstream_order_id, upstream_deposit_address,
		forward_tx_id, refund_tx_id, created_at, updated_at
	FROM relay_legs`

// Create inserts a new leg in AWAITING_DEPOSIT, idempotent on
// external_id: a second Create for an external_id that already has a
// row returns that row unchanged (the same "crash between the C1 call
// succeeding and this INSERT" recovery case every sibling Store's own
// Create is built around).
func (s *Store) Create(ctx context.Context, l Leg) (Leg, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO relay_legs
			(external_id, order_id, direction, status, customer_id, destination_address, deposit_address,
			 amount_in, amount_in_asset, amount_out_expected, amount_out_expected_asset)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (external_id) DO NOTHING
		RETURNING id, external_id, order_id, direction, status, customer_id, destination_address,
			deposit_address, amount_in, amount_in_asset, amount_out_expected, amount_out_expected_asset,
			amount_out_actual, upstream_provider_name, upstream_order_id, upstream_deposit_address,
			forward_tx_id, refund_tx_id, created_at, updated_at
	`, l.ExternalID, l.OrderID, string(l.Direction), string(StatusAwaitingDeposit), l.CustomerID, l.DestinationAddress, l.DepositAddress,
		l.AmountIn.Units, string(l.AmountIn.Asset), l.AmountOutExpected.Units, string(l.AmountOutExpected.Asset))

	leg, err := scanLeg(row)
	if err == nil {
		return leg, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Leg{}, fmt.Errorf("relay: creating leg %s: %w", l.ExternalID, err)
	}
	return s.GetByExternalID(ctx, l.ExternalID)
}

// GetByExternalID fetches the leg for externalID.
func (s *Store) GetByExternalID(ctx context.Context, externalID string) (Leg, error) {
	row := s.pool.QueryRow(ctx, selectSQL+` WHERE external_id = $1`, externalID)
	leg, err := scanLeg(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Leg{}, fmt.Errorf("%w: external_id %s", ErrLegNotFound, externalID)
	}
	if err != nil {
		return Leg{}, fmt.Errorf("relay: fetching leg %s: %w", externalID, err)
	}
	return leg, nil
}

// ListByStatus returns every leg currently in status, oldest first --
// what internal/orchestrate's own driving loop scans.
func (s *Store) ListByStatus(ctx context.Context, status Status) ([]Leg, error) {
	rows, err := s.pool.Query(ctx, selectSQL+` WHERE status = $1 ORDER BY id`, string(status))
	if err != nil {
		return nil, fmt.Errorf("relay: listing legs in %s: %w", status, err)
	}
	defer rows.Close()

	var out []Leg
	for rows.Next() {
		l, err := scanLeg(rows)
		if err != nil {
			return nil, fmt.Errorf("relay: listing legs in %s: %w", status, err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// List returns every leg matching status, or every leg (any status) if
// status is nil, most-recently-updated first -- the ops console's own
// general listing view. Mirrors
// screening/internal/holds.List's identical "status *Status, nil means
// everything" convention (that package's own List, not its
// oldest-first ListOpen review-queue variant, since this is a general
// visibility view, not a work queue).
func (s *Store) List(ctx context.Context, status *Status) ([]Leg, error) {
	var rows pgx.Rows
	var err error
	if status != nil {
		rows, err = s.pool.Query(ctx, selectSQL+` WHERE status = $1 ORDER BY updated_at DESC`, string(*status))
	} else {
		rows, err = s.pool.Query(ctx, selectSQL+` ORDER BY updated_at DESC`)
	}
	if err != nil {
		return nil, fmt.Errorf("relay: listing legs: %w", err)
	}
	defer rows.Close()

	var out []Leg
	for rows.Next() {
		l, err := scanLeg(rows)
		if err != nil {
			return nil, fmt.Errorf("relay: listing legs: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// MarkForwarding transitions externalID from AWAITING_DEPOSIT to
// FORWARDING, recording the upstream swap order relayd just created.
// Idempotent: a retry after this already succeeded (upstreamOrderID
// unchanged) is a no-op success, never an error -- the same replay-safe
// discipline every mark<State> in this project uses.
func (s *Store) MarkForwarding(ctx context.Context, externalID, upstreamProviderName, upstreamOrderID, upstreamDepositAddress string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE relay_legs
		SET status = $1, upstream_provider_name = $2, upstream_order_id = $3, upstream_deposit_address = $4, updated_at = now()
		WHERE external_id = $5 AND status = $6
	`, string(StatusForwarding), upstreamProviderName, upstreamOrderID, upstreamDepositAddress, externalID, string(StatusAwaitingDeposit))
	if err != nil {
		return fmt.Errorf("relay: marking %s forwarding: %w", externalID, err)
	}
	if tag.RowsAffected() == 0 {
		return s.checkAlreadyAt(ctx, externalID, StatusForwarding)
	}
	return nil
}

// MarkForwarded transitions externalID from FORWARDING to FORWARDED,
// recording the broadcast forward-leg transaction id.
func (s *Store) MarkForwarded(ctx context.Context, externalID, forwardTxID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE relay_legs
		SET status = $1, forward_tx_id = $2, updated_at = now()
		WHERE external_id = $3 AND status = $4
	`, string(StatusForwarded), forwardTxID, externalID, string(StatusForwarding))
	if err != nil {
		return fmt.Errorf("relay: marking %s forwarded: %w", externalID, err)
	}
	if tag.RowsAffected() == 0 {
		return s.checkAlreadyAt(ctx, externalID, StatusForwarded)
	}
	return nil
}

// MarkSettled transitions externalID from FORWARDED to SETTLED,
// recording the upstream platform's own final payout amount.
func (s *Store) MarkSettled(ctx context.Context, externalID string, amountOutActual money.Amount) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE relay_legs
		SET status = $1, amount_out_actual = $2, updated_at = now()
		WHERE external_id = $3 AND status = $4
	`, string(StatusSettled), amountOutActual.Units, externalID, string(StatusForwarded))
	if err != nil {
		return fmt.Errorf("relay: marking %s settled: %w", externalID, err)
	}
	if tag.RowsAffected() == 0 {
		return s.checkAlreadyAt(ctx, externalID, StatusSettled)
	}
	return nil
}

// MarkFailed transitions externalID to FAILED from any non-terminal
// status -- unlike the other mark<State> calls, this one has no single
// expected FROM status: a leg can fail while FORWARDING (the upstream
// order was never successfully created) or while FORWARDED (the
// upstream platform itself reported failure after receiving the forward
// transfer -- R5's own UNRECOVERABLE case, tracked here only as FAILED
// for this pass, per this package's own doc comment).
func (s *Store) MarkFailed(ctx context.Context, externalID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE relay_legs
		SET status = $1, updated_at = now()
		WHERE external_id = $2 AND status IN ($3, $4)
	`, string(StatusFailed), externalID, string(StatusForwarding), string(StatusForwarded))
	if err != nil {
		return fmt.Errorf("relay: marking %s failed: %w", externalID, err)
	}
	if tag.RowsAffected() == 0 {
		return s.checkAlreadyAt(ctx, externalID, StatusFailed)
	}
	return nil
}

// MarkRefundPending transitions externalID to REFUND_PENDING from either
// AWAITING_DEPOSIT or FORWARDING -- the two pre-FORWARDED statuses R5's
// own automatic timeout-refund path (internal/orchestrate/refund.go) can
// reach: a leg whose upstream order was never successfully created
// (still AWAITING_DEPOSIT), or one stuck unable to broadcast its own
// forward transfer (still FORWARDING) past a configured deadline. Never
// reachable from FORWARDED -- once the forward transfer itself confirms
// on-chain, R5's own UNRECOVERABLE path applies instead (see
// MarkUnrecoverable), not a refund.
func (s *Store) MarkRefundPending(ctx context.Context, externalID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE relay_legs
		SET status = $1, updated_at = now()
		WHERE external_id = $2 AND status IN ($3, $4)
	`, string(StatusRefundPending), externalID, string(StatusAwaitingDeposit), string(StatusForwarding))
	if err != nil {
		return fmt.Errorf("relay: marking %s refund pending: %w", externalID, err)
	}
	if tag.RowsAffected() == 0 {
		return s.checkAlreadyAt(ctx, externalID, StatusRefundPending)
	}
	return nil
}

// MarkRefunded transitions externalID from REFUND_PENDING to REFUNDED,
// recording the broadcast refund transaction id.
func (s *Store) MarkRefunded(ctx context.Context, externalID, refundTxID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE relay_legs
		SET status = $1, refund_tx_id = $2, updated_at = now()
		WHERE external_id = $3 AND status = $4
	`, string(StatusRefunded), refundTxID, externalID, string(StatusRefundPending))
	if err != nil {
		return fmt.Errorf("relay: marking %s refunded: %w", externalID, err)
	}
	if tag.RowsAffected() == 0 {
		return s.checkAlreadyAt(ctx, externalID, StatusRefunded)
	}
	return nil
}

// MarkUnrecoverable transitions externalID from FORWARDED to
// UNRECOVERABLE -- the upstream platform's own swap failed after this
// system's own forward transfer already confirmed on-chain, so no
// on-chain lever is left to pull (see internal/orchestrate/settle.go's
// own doc comment). A human decision is required from here; reaching
// this state fires an alert (internal/alert), never just a log line.
func (s *Store) MarkUnrecoverable(ctx context.Context, externalID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE relay_legs
		SET status = $1, updated_at = now()
		WHERE external_id = $2 AND status = $3
	`, string(StatusUnrecoverable), externalID, string(StatusForwarded))
	if err != nil {
		return fmt.Errorf("relay: marking %s unrecoverable: %w", externalID, err)
	}
	if tag.RowsAffected() == 0 {
		return s.checkAlreadyAt(ctx, externalID, StatusUnrecoverable)
	}
	return nil
}

// checkAlreadyAt distinguishes "this call is a safe replay of a
// transition that already happened" (success) from "the leg is in some
// OTHER status this transition never expected" (a real error) --
// mirrors dispatcher/internal/dispatch.Store's own markStatus identical
// reasoning.
func (s *Store) checkAlreadyAt(ctx context.Context, externalID string, want Status) error {
	current, err := s.GetByExternalID(ctx, externalID)
	if err != nil {
		return err
	}
	if current.Status == want {
		return nil
	}
	return fmt.Errorf("%w: leg %s is in %s, not eligible for this transition", ErrNotInExpectedStatus, externalID, current.Status)
}

type scanRow interface {
	Scan(dest ...any) error
}

func scanLeg(row scanRow) (Leg, error) {
	var l Leg
	var direction, status string
	var amountInUnits, amountOutExpectedUnits int64
	var amountInAsset, amountOutExpectedAsset string
	var amountOutActualUnits *int64
	err := row.Scan(
		&l.ID, &l.ExternalID, &l.OrderID, &direction, &status, &l.CustomerID, &l.DestinationAddress,
		&l.DepositAddress, &amountInUnits, &amountInAsset, &amountOutExpectedUnits, &amountOutExpectedAsset,
		&amountOutActualUnits, &l.UpstreamProviderName, &l.UpstreamOrderID, &l.UpstreamDepositAddress,
		&l.ForwardTxID, &l.RefundTxID, &l.CreatedAt, &l.UpdatedAt,
	)
	if err != nil {
		return Leg{}, err
	}
	l.Direction = Direction(direction)
	l.Status = Status(status)
	l.AmountIn = money.Amount{Asset: money.Asset(amountInAsset), Units: amountInUnits}
	l.AmountOutExpected = money.Amount{Asset: money.Asset(amountOutExpectedAsset), Units: amountOutExpectedUnits}
	if amountOutActualUnits != nil {
		actual := money.Amount{Asset: money.Asset(amountOutExpectedAsset), Units: *amountOutActualUnits}
		l.AmountOutActual = &actual
	}
	return l, nil
}
