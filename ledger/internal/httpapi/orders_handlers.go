package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"ledger/internal/db"
	"ledger/internal/journal"
	"ledger/internal/money"
	"ledger/internal/orders"
)

type postOrderRequest struct {
	ExternalID       string    `json:"external_id"`
	CustomerID       string    `json:"customer_id"`
	Tier             string    `json:"tier"`
	AmountIn         string    `json:"amount_in"`
	AmountOut        string    `json:"amount_out"`
	FeeUnits         string    `json:"fee_units"`
	NetworkFeeUnits  string    `json:"network_fee_units"`
	RecipientAddress string    `json:"recipient_address"`
	QuotedAt         time.Time `json:"quoted_at"`
	QuoteExpiresAt   time.Time `json:"quote_expires_at"`
	// AmountInAsset/AmountOutAsset are only consulted for tier=RELAY --
	// Model F's own bidirectional tier (see orders.CreateParams.validate's
	// own doc comment on why RELAY, alone, cannot assume a fixed
	// direction the way DIRECT/STANDARD/SWEEP always have). Every other
	// tier keeps the original implicit convention (amount_in always
	// USDT_BEP20, the other three always USDT_TRC20) and ignores these
	// two fields entirely, preserving every existing caller's contract
	// unchanged.
	AmountInAsset  string `json:"amount_in_asset,omitempty"`
	AmountOutAsset string `json:"amount_out_asset,omitempty"`
}

type orderResponse struct {
	ID               int64     `json:"id"`
	ExternalID       string    `json:"external_id"`
	CustomerID       string    `json:"customer_id"`
	Tier             string    `json:"tier"`
	State            string    `json:"state"`
	AmountIn         string    `json:"amount_in"`
	AmountOut        string    `json:"amount_out"`
	FeeUnits         string    `json:"fee_units"`
	NetworkFeeUnits  string    `json:"network_fee_units"`
	RecipientAddress string    `json:"recipient_address"`
	SenderAddress    *string   `json:"sender_address"`
	QuotedAt         time.Time `json:"quoted_at"`
	QuoteExpiresAt   time.Time `json:"quote_expires_at"`
	Version          int32     `json:"version"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
	// AmountInAsset/AmountOutAsset are always present (unlike the
	// request's own omitempty pair) -- a RELAY order's own direction is
	// otherwise unrecoverable from this response alone, and a
	// fixed-direction order's values are simply the implicit constants
	// echoed back explicitly, never ambiguous either way.
	AmountInAsset  string `json:"amount_in_asset"`
	AmountOutAsset string `json:"amount_out_asset"`
}

func toOrderResponse(o orders.Order) orderResponse {
	amountIn, _ := money.Format(o.AmountIn)
	amountOut, _ := money.Format(o.AmountOut)
	feeUnits, _ := money.Format(o.FeeUnits)
	networkFeeUnits, _ := money.Format(o.NetworkFeeUnits)
	return orderResponse{
		ID: o.ID, ExternalID: o.ExternalID, CustomerID: o.CustomerID, Tier: string(o.Tier), State: string(o.State),
		AmountIn: amountIn, AmountOut: amountOut, FeeUnits: feeUnits, NetworkFeeUnits: networkFeeUnits,
		RecipientAddress: o.RecipientAddress, SenderAddress: o.SenderAddress,
		QuotedAt: o.QuotedAt, QuoteExpiresAt: o.QuoteExpiresAt,
		Version: o.Version, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
		AmountInAsset: string(o.AmountIn.Asset), AmountOutAsset: string(o.AmountOut.Asset),
	}
}

// postOrder is POST /v1/orders, always creating in Quoted. For every
// tier except RELAY, amount fields are parsed against the fixed assets
// orders.CreateParams itself documents (amount_in is always USDT_BEP20,
// the other three always USDT_TRC20) -- the request has no per-field
// asset to get wrong. A RELAY order instead requires the caller to name
// amount_in_asset/amount_out_asset explicitly (one of USDT_BEP20/
// USDT_TRC20, the other way around from each other) -- see
// orders.CreateParams.validate's own doc comment for why RELAY alone
// cannot assume a fixed direction.
func (s *Server) postOrder(w http.ResponseWriter, r *http.Request) {
	var req postOrderRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	amountInAsset, amountOutAsset := money.USDT_BEP20, money.USDT_TRC20
	if orders.Tier(req.Tier) == orders.Relay {
		var err error
		amountInAsset, err = parseAssetField(req.AmountInAsset, "amount_in_asset")
		if err != nil {
			writeErr(w, err)
			return
		}
		amountOutAsset, err = parseAssetField(req.AmountOutAsset, "amount_out_asset")
		if err != nil {
			writeErr(w, err)
			return
		}
	}

	amountIn, err := money.ParseDecimal(req.AmountIn, amountInAsset)
	if err != nil {
		writeErr(w, err)
		return
	}
	amountOut, err := money.ParseDecimal(req.AmountOut, amountOutAsset)
	if err != nil {
		writeErr(w, err)
		return
	}
	// fee_units/network_fee_units are parsed against amount_out's asset
	// for every tier except RELAY, which uses amount_in's instead -- see
	// orders.CreateParams.validate's own doc comment for exactly why
	// RELAY's fee is denominated in what relayd withholds before
	// forwarding (the deposited asset), never in what the upstream
	// vendor pays the customer directly.
	feeAsset := amountOutAsset
	if orders.Tier(req.Tier) == orders.Relay {
		feeAsset = amountInAsset
	}
	feeUnits, err := money.ParseDecimal(req.FeeUnits, feeAsset)
	if err != nil {
		writeErr(w, err)
		return
	}
	networkFeeUnits, err := money.ParseDecimal(req.NetworkFeeUnits, feeAsset)
	if err != nil {
		writeErr(w, err)
		return
	}

	order, err := orders.Create(r.Context(), s.Pool, orders.CreateParams{
		ExternalID:       req.ExternalID,
		CustomerID:       req.CustomerID,
		Tier:             orders.Tier(req.Tier),
		AmountIn:         amountIn,
		AmountOut:        amountOut,
		FeeUnits:         feeUnits,
		NetworkFeeUnits:  networkFeeUnits,
		RecipientAddress: req.RecipientAddress,
		QuotedAt:         req.QuotedAt,
		QuoteExpiresAt:   req.QuoteExpiresAt,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, toOrderResponse(order))
}

// getOrder is GET /v1/orders/{external_id}.
func (s *Server) getOrder(w http.ResponseWriter, r *http.Request) {
	externalID, ok := urlParam(w, r, "external_id")
	if !ok {
		return
	}
	order, err := orders.GetByExternalID(r.Context(), s.Pool, externalID)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toOrderResponse(order))
}

type listOrdersResponse struct {
	Orders     []orderResponse `json:"orders"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// getOrders is GET /v1/orders?state=<state>&updated_after=<cursor>&limit=<n>
// -- added for C3 (screening) discovery: C1's only way to say "which
// orders just entered a state", since there is no event bus in this
// system by design (see docs/03-build/c3-screening-build-prompts.md's
// "Read this first"). Deliberately generic (list-by-state, not
// "list-funded-for-screening") so it's a reusable primitive for ops
// tooling and C6 too, not a point-to-point coupling to one caller.
//
// next_cursor is always returned when there is a cursor to give,
// including an empty page (echoes the caller's own updated_after back so
// a poller never has to special-case "nothing new yet" versus "here's
// where you were") -- a poller can always feed it straight back in as
// its next updated_after, forever.
func (s *Server) getOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	stateParam := q.Get("state")
	if stateParam == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "state is required"))
		return
	}
	state := orders.State(stateParam)
	if !state.Valid() {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "unknown state "+stateParam))
		return
	}

	limit := orders.DefaultListLimit
	if raw := q.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "limit must be a positive integer"))
			return
		}
		limit = parsed
	}

	var after *orders.Cursor
	if raw := q.Get("updated_after"); raw != "" {
		parsed, err := orders.ParseCursor(raw)
		if err != nil {
			writeErr(w, err)
			return
		}
		after = &parsed
	}

	list, err := orders.ListByStateAfter(r.Context(), s.Pool, state, after, limit)
	if err != nil {
		writeErr(w, err)
		return
	}

	resp := listOrdersResponse{Orders: make([]orderResponse, len(list))}
	for i, o := range list {
		resp.Orders[i] = toOrderResponse(o)
	}
	switch {
	case len(list) > 0:
		last := list[len(list)-1]
		resp.NextCursor = (orders.Cursor{UpdatedAt: last.UpdatedAt, ID: last.ID}).String()
	case after != nil:
		resp.NextCursor = after.String()
	}
	respondJSON(w, http.StatusOK, resp)
}

type transitionEntryRequest struct {
	EntryType  string             `json:"entry_type"`
	OccurredAt time.Time          `json:"occurred_at"`
	Lines      []entryLineRequest `json:"lines"`
	Metadata   map[string]any     `json:"metadata,omitempty"`
}

type postTransitionRequest struct {
	ToState         string                  `json:"to_state"`
	ExpectedVersion int32                   `json:"expected_version"`
	Reason          string                  `json:"reason"`
	OccurredAt      time.Time               `json:"occurred_at"`
	Entry           *transitionEntryRequest `json:"entry,omitempty"`
	// EntryID names an entry already posted -- in practice a reversal
	// created via POST /v1/entries/{id}/reversal -- to be recorded as
	// this transition's cause without posting anything new.
	//
	// orders.TransitionParams has had this field since C1.5, but until
	// C1.11 nothing exposed it, so the only way to drive the two
	// reversal-shaped transitions (funded->quoted, dispatching->held)
	// over HTTP was to hand-build a negating entry and post it through
	// `entry`. That produces an ordinary entry with reversal_of NULL: it
	// balances, so nothing rejects it, but it is not linked to what it
	// undoes and it is not covered by the UNIQUE constraint that makes a
	// double-reversal impossible. Both halves of that guarantee are the
	// point of C1.6, and this field is what lets a remote caller keep
	// them.
	//
	// At most one of entry / entry_id may be set; TransitionParams
	// enforces that, and exactly one is required when the transition rule
	// requires an entry.
	EntryID *int64 `json:"entry_id,omitempty"`
	// SenderAddress is sibling to entry, not a line or metadata field
	// inside it: it isn't a ledger amount, and C3 needs to query it
	// structurally, not parse a jsonb blob this API makes no shape
	// promise about. Only accepted on a transition into funded -- see
	// orders.TransitionParams.SenderAddress.
	SenderAddress *string `json:"sender_address,omitempty"`
}

// postTransition is POST /v1/orders/{external_id}/transitions. When the
// request carries an entry, it is posted with THIS request's
// Idempotency-Key header as its own idempotency key (not a separately
// generated one) -- retrying the identical HTTP request therefore
// replays the identical journal entry too, the same idempotent-retry
// guarantee POST /entries gives standalone callers.
func (s *Server) postTransition(w http.ResponseWriter, r *http.Request) {
	externalID, ok := urlParam(w, r, "external_id")
	if !ok {
		return
	}

	var req postTransitionRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	order, err := orders.GetByExternalID(r.Context(), s.Pool, externalID)
	if err != nil {
		writeErr(w, err)
		return
	}

	var entryReq *journal.EntryRequest
	if req.Entry != nil {
		lines, err := entryLinesToJournalLines(req.Entry.Lines)
		if err != nil {
			writeErr(w, err)
			return
		}
		orderID := order.ID
		entryReq = &journal.EntryRequest{
			IdempotencyKey: idempotencyKeyFromContext(r.Context()),
			EntryType:      req.Entry.EntryType,
			Actor:          actorFromContext(r.Context()),
			OccurredAt:     req.Entry.OccurredAt,
			OrderID:        &orderID,
			Lines:          lines,
			Metadata:       req.Entry.Metadata,
		}
	}

	var updated orders.Order
	err = db.Tx(r.Context(), s.Pool, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		updated, err = orders.Transition(ctx, tx, order.ID, orders.State(req.ToState), req.ExpectedVersion, orders.TransitionParams{
			Actor:         actorFromContext(ctx),
			Reason:        req.Reason,
			OccurredAt:    req.OccurredAt,
			Entry:         entryReq,
			EntryID:       req.EntryID,
			SenderAddress: req.SenderAddress,
		})
		return err
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toOrderResponse(updated))
}

// parseAssetField parses a RELAY order's required amount_in_asset/
// amount_out_asset request field into a money.Asset -- required (unlike
// every other tier, which never sends this field at all) because RELAY
// is bidirectional and has no fixed convention to fall back on silently.
func parseAssetField(raw, fieldName string) (money.Asset, error) {
	if raw == "" {
		return "", newAPIError(http.StatusBadRequest, errInvalidRequest.Code, fieldName+" is required for tier=RELAY")
	}
	asset := money.Asset(raw)
	if !asset.Valid() {
		return "", newAPIError(http.StatusBadRequest, errInvalidRequest.Code, fieldName+" is not a known asset")
	}
	return asset, nil
}
