// Package holds is C3.6: the hold queue and manual review path. Every
// Hold classification C3.4 reports to C1 gets exactly one row here
// (Open), and Release/Reject are the ONLY paths that ever move an order
// out of `held` -- invariant 1, §0 of
// docs/03-build/c3-screening-build-prompts.md. Nothing in C3.4's
// automatic pipeline touches a row here once opened.
//
// Free functions taking an explicit Queryer and explicit dependencies
// per call, matching every other package in this module (internal/cache,
// internal/discovery, internal/verdict) -- the build spec's own
// pseudocode signatures for this chunk omit both, the same way C3.1's
// pseudocode omitted the Queryer parameter every other chunk's build
// turned out to need.
package holds

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"screening/internal/db"
	"screening/internal/ledgerclient"
	"screening/internal/verdict"
)

// Queryer is db.Queryer under this package's own name, matching every
// other internal package's convention in this module.
type Queryer = db.Queryer

// Status is holds' closed set of states -- a plain string, not a Go-level
// enum, matching the column's own text type (enforced at the database
// level by a CHECK constraint instead, see migration 0004).
type Status string

const (
	// StatusOpen etc., not bare Open/Released/Rejected: Open is also
	// this package's own constructor function name (per the build
	// spec's literal signature), and Go doesn't allow a function and a
	// package-level constant to share an identifier.
	StatusOpen     Status = "OPEN"
	StatusReleased Status = "RELEASED"
	StatusRejected Status = "REJECTED"
)

// Hold is one holds row.
type Hold struct {
	ID                int64
	OrderID           int64
	ExternalID        string
	ReasonCode        string
	ScreeningResultID *int64
	OpenedAt          time.Time
	Status            Status
	ResolvedBy        *string
	ResolvedAt        *time.Time
	ResolutionNote    *string
}

var (
	// ErrNotFound means no holds row exists with the given id.
	ErrNotFound = errors.New("holds: no such hold")
	// ErrNotOpen guards Release/Reject against acting twice on the same
	// hold -- a second resolution attempt is a caller bug or a stale
	// UI, not something to silently no-op (unlike, say, discovery's
	// idempotent enqueue: resolving a hold is a deliberate human action
	// with its own audit trail, and a second one needs a person to
	// notice, not be swallowed).
	ErrNotOpen = errors.New("holds: hold is not open")
	// ErrEmptyReviewer guards the rule this chunk's own build spec
	// states explicitly: "A hold's reviewer identity is never inferred
	// or defaulted." Checked before calling C1 at all.
	ErrEmptyReviewer = errors.New("holds: reviewer must not be empty")
)

const insertReturning = `
	INSERT INTO holds (order_id, external_id, reason_code, screening_result_id)
	VALUES ($1, $2, $3, $4)
	ON CONFLICT (order_id) WHERE status = 'OPEN' DO NOTHING
	RETURNING id, order_id, external_id, reason_code, screening_result_id, opened_at, status, resolved_by, resolved_at, resolution_note
`

// Open records decision (which must be a Hold classification) as a new
// OPEN hold for order. Idempotent while a hold for order.OrderID is
// still OPEN: a second call (a retried C3.4 pipeline run, concretely)
// returns the EXISTING open hold rather than creating a duplicate or
// erroring -- enforced by a partial unique index
// (idx_holds_one_open_per_order, migration 0004), not just this
// function's own logic, so the guarantee holds even under concurrent
// callers.
func Open(ctx context.Context, q Queryer, order ledgerclient.OrderRef, decision verdict.Decision) (Hold, error) {
	if decision.Classification != verdict.Hold {
		return Hold{}, fmt.Errorf("holds: Open called with classification %v, want Hold", decision.Classification)
	}

	var screeningResultID *int64
	if decision.ScreeningResultID != 0 {
		id := decision.ScreeningResultID
		screeningResultID = &id
	}

	row := q.QueryRow(ctx, insertReturning, order.OrderID, order.ExternalID, decision.ReasonCode, screeningResultID)
	h, err := scanHold(row)
	if errors.Is(err, pgx.ErrNoRows) {
		// ON CONFLICT ... DO NOTHING fired: an OPEN hold for this order
		// already exists. Fetch and return it instead of the row this
		// call didn't insert.
		existing, getErr := getOpenByOrderID(ctx, q, order.OrderID)
		if getErr != nil {
			return Hold{}, fmt.Errorf("holds: order %d already has an open hold, but re-fetching it failed: %w", order.OrderID, getErr)
		}
		return existing, nil
	}
	if err != nil {
		return Hold{}, fmt.Errorf("holds: opening hold for order %d (%s): %w", order.OrderID, order.ExternalID, err)
	}
	return h, nil
}

// Releaser is the one call Release needs from C1 --
// ledgerclient.Client's real implementation, or a fake for testing.
type Releaser interface {
	ReleaseHold(ctx context.Context, externalID string, orderID, holdID int64) error
}

// Release resolves holdID as RELEASED: calls releaser.ReleaseHold
// (held->screened on C1), then records reviewer and note locally. Fails
// before ever calling C1 if reviewer is empty or the hold is not
// currently OPEN.
//
// Ordering matters: C1 is called BEFORE this function's own local
// UPDATE. If the local write then fails -- rare, but possible (a
// transient DB error) -- C1 has already released the order but this
// package's own record does not yet reflect it. That specific window
// needs manual reconciliation (the same posture C2 takes for its own
// narrow failure windows, e.g. HandleUnreportable); it is not silently
// retried here, because retrying would call ReleaseHold a second time
// for an order that already left held, which correctly surfaces as
// ErrIllegalTransition rather than being treated as success.
func Release(ctx context.Context, q Queryer, releaser Releaser, holdID int64, reviewer, note string) error {
	if reviewer == "" {
		return fmt.Errorf("%w: release", ErrEmptyReviewer)
	}

	h, err := Get(ctx, q, holdID)
	if err != nil {
		return err
	}
	if h.Status != StatusOpen {
		return fmt.Errorf("%w: hold %d is %s", ErrNotOpen, holdID, h.Status)
	}

	if err := releaser.ReleaseHold(ctx, h.ExternalID, h.OrderID, h.ID); err != nil {
		return fmt.Errorf("holds: releasing hold %d: %w", holdID, err)
	}

	if err := resolve(ctx, q, holdID, StatusReleased, reviewer, note); err != nil {
		return fmt.Errorf("holds: hold %d released on C1 but recording it locally failed -- needs manual reconciliation: %w", holdID, err)
	}
	return nil
}

// Rejecter is the one call Reject needs from C1 --
// ledgerclient.Client's real implementation, or a fake for testing.
type Rejecter interface {
	RejectHold(ctx context.Context, externalID string, orderID, holdID int64, entry map[string]any) error
}

// RefundEntryBuilder constructs the journal entry a manual reject's
// held->refunded transition requires (C1.5's table marks it
// RequiresEntry: true). Behind an interface because this chunk's own
// original build spec was explicit that no real implementation could
// exist yet: nothing in this system owned constructing, signing, or
// broadcasting the physical refund (§A's own "out of scope" note).
// That is still true for Model D's own DIRECT/STANDARD/SWEEP tiers --
// StubRefundEntryBuilder is still what those get -- but a real owner now
// exists for RELAY-tier orders: relayd, which already builds/signs/
// broadcasts the physical refund transfer for its own R5 (see
// relay_refund.go's own RelayAwareRefundEntryBuilder, added once relayd
// had that capability). cmd/screend wires whichever one is actually
// configured; this interface itself did not need to change for it.
type RefundEntryBuilder interface {
	BuildRefundEntry(ctx context.Context, order ledgerclient.OrderRef) (map[string]any, error)
}

// ErrRefundEntryNotImplemented is StubRefundEntryBuilder's only possible
// result -- still the real, unchanged outcome for Model D's own
// DIRECT/STANDARD/SWEEP tiers (see RefundEntryBuilder's own doc
// comment), and RelayAwareRefundEntryBuilder's own fallback for any
// external_id relayd reports it has no relay leg for.
var ErrRefundEntryNotImplemented = errors.New(
	"holds: refund entry construction is not implemented -- no owner exists yet for the physical BEP20 refund (see docs/03-build/c3-screening-build-prompts.md §A)")

// StubRefundEntryBuilder is the RefundEntryBuilder every non-RELAY tier
// still gets (see RefundEntryBuilder's own doc comment) -- and
// RelayAwareRefundEntryBuilder's own fallback target for an order that
// turns out not to be RELAY-tier at all. Every call fails with
// ErrRefundEntryNotImplemented.
type StubRefundEntryBuilder struct{}

// BuildRefundEntry implements RefundEntryBuilder.
func (StubRefundEntryBuilder) BuildRefundEntry(ctx context.Context, order ledgerclient.OrderRef) (map[string]any, error) {
	return nil, ErrRefundEntryNotImplemented
}

// Reject resolves holdID as REJECTED: builds the refund entry via
// entryBuilder, calls rejecter.RejectHold (held->refunded on C1) with
// it, then records reviewer and note locally. Fails before ever calling
// C1 if reviewer is empty or the hold is not currently OPEN -- and, in
// practice today, fails at the entry-building step itself against the
// only shipped RefundEntryBuilder (see StubRefundEntryBuilder). Same
// reconciliation posture as Release if the local write fails after a
// successful C1 call -- see Release's own doc comment.
func Reject(ctx context.Context, q Queryer, rejecter Rejecter, entryBuilder RefundEntryBuilder, holdID int64, reviewer, note string) error {
	if reviewer == "" {
		return fmt.Errorf("%w: reject", ErrEmptyReviewer)
	}

	h, err := Get(ctx, q, holdID)
	if err != nil {
		return err
	}
	if h.Status != StatusOpen {
		return fmt.Errorf("%w: hold %d is %s", ErrNotOpen, holdID, h.Status)
	}

	entry, err := entryBuilder.BuildRefundEntry(ctx, ledgerclient.OrderRef{OrderID: h.OrderID, ExternalID: h.ExternalID})
	if err != nil {
		return fmt.Errorf("holds: building refund entry for hold %d: %w", holdID, err)
	}

	if err := rejecter.RejectHold(ctx, h.ExternalID, h.OrderID, h.ID, entry); err != nil {
		return fmt.Errorf("holds: rejecting hold %d: %w", holdID, err)
	}

	if err := resolve(ctx, q, holdID, StatusRejected, reviewer, note); err != nil {
		return fmt.Errorf("holds: hold %d rejected on C1 but recording it locally failed -- needs manual reconciliation: %w", holdID, err)
	}
	return nil
}

func resolve(ctx context.Context, q Queryer, holdID int64, status Status, reviewer, note string) error {
	var notePtr *string
	if note != "" {
		notePtr = &note
	}
	tag, err := q.Exec(ctx, `
		UPDATE holds SET status = $1, resolved_by = $2, resolved_at = now(), resolution_note = $3
		WHERE id = $4 AND status = 'OPEN'
	`, string(status), reviewer, notePtr, holdID)
	if err != nil {
		return fmt.Errorf("holds: resolving hold %d: %w", holdID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: hold %d", ErrNotOpen, holdID)
	}
	return nil
}

// ListOpen returns every OPEN hold, oldest first -- the natural order
// for a human review queue. Never returns a RELEASED or REJECTED hold.
func ListOpen(ctx context.Context, q Queryer) ([]Hold, error) {
	rows, err := q.Query(ctx, `
		SELECT id, order_id, external_id, reason_code, screening_result_id, opened_at, status, resolved_by, resolved_at, resolution_note
		FROM holds WHERE status = 'OPEN' ORDER BY opened_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("holds: listing open holds: %w", err)
	}
	defer rows.Close()

	var out []Hold
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, fmt.Errorf("holds: listing open holds: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("holds: listing open holds: %w", err)
	}
	return out, nil
}

// List returns holds matching status, or every hold (any status) if
// status is nil -- newest first, the natural order for a general audit
// listing (as opposed to ListOpen's oldest-first review-queue order).
// Backs C3.8's GET /v1/holds?status= filter -- kept as its own function
// rather than folded into ListOpen so ListOpen's existing oldest-first
// contract (already relied on for the review-queue use case) never
// silently changes order underneath an existing caller.
func List(ctx context.Context, q Queryer, status *Status) ([]Hold, error) {
	var rows pgx.Rows
	var err error
	if status != nil {
		rows, err = q.Query(ctx, `
			SELECT id, order_id, external_id, reason_code, screening_result_id, opened_at, status, resolved_by, resolved_at, resolution_note
			FROM holds WHERE status = $1 ORDER BY opened_at DESC
		`, string(*status))
	} else {
		rows, err = q.Query(ctx, `
			SELECT id, order_id, external_id, reason_code, screening_result_id, opened_at, status, resolved_by, resolved_at, resolution_note
			FROM holds ORDER BY opened_at DESC
		`)
	}
	if err != nil {
		return nil, fmt.Errorf("holds: listing holds: %w", err)
	}
	defer rows.Close()

	var out []Hold
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, fmt.Errorf("holds: listing holds: %w", err)
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("holds: listing holds: %w", err)
	}
	return out, nil
}

// Get looks up one hold by id.
func Get(ctx context.Context, q Queryer, holdID int64) (Hold, error) {
	row := q.QueryRow(ctx, `
		SELECT id, order_id, external_id, reason_code, screening_result_id, opened_at, status, resolved_by, resolved_at, resolution_note
		FROM holds WHERE id = $1
	`, holdID)
	h, err := scanHold(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Hold{}, fmt.Errorf("%w: id %d", ErrNotFound, holdID)
	}
	if err != nil {
		return Hold{}, fmt.Errorf("holds: getting hold %d: %w", holdID, err)
	}
	return h, nil
}

func getOpenByOrderID(ctx context.Context, q Queryer, orderID int64) (Hold, error) {
	row := q.QueryRow(ctx, `
		SELECT id, order_id, external_id, reason_code, screening_result_id, opened_at, status, resolved_by, resolved_at, resolution_note
		FROM holds WHERE order_id = $1 AND status = 'OPEN'
	`, orderID)
	return scanHold(row)
}

type scanRow interface {
	Scan(dest ...any) error
}

func scanHold(row scanRow) (Hold, error) {
	var h Hold
	var status string
	err := row.Scan(&h.ID, &h.OrderID, &h.ExternalID, &h.ReasonCode, &h.ScreeningResultID,
		&h.OpenedAt, &status, &h.ResolvedBy, &h.ResolvedAt, &h.ResolutionNote)
	if err != nil {
		return Hold{}, err
	}
	h.Status = Status(status)
	return h, nil
}
