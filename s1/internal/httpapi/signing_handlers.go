package httpapi

import (
	"encoding/hex"
	"net/http"
	"time"

	"s1/internal/requests"
)

type signingRequestResponse struct {
	ID        int64     `json:"id"`
	Status    string    `json:"status"`
	SignedTx  *string   `json:"signed_tx"`
	CreatedAt time.Time `json:"created_at"`
}

func toSigningRequestResponse(req requests.SigningRequest) signingRequestResponse {
	resp := signingRequestResponse{ID: req.ID, Status: string(req.Status), CreatedAt: req.CreatedAt}
	if req.Status == requests.StatusSigned {
		hexTx := hex.EncodeToString(req.SignedTx[:])
		resp.SignedTx = &hexTx
	}
	return resp
}

type postSigningRequestRequest struct {
	SlotID         int     `json:"slot_id"`
	Digest         string  `json:"digest"`
	EstimatedUSD   float64 `json:"estimated_usd"`
	IdempotencyKey string  `json:"idempotency_key"`
}

// postSigningRequest is POST /v1/signing-requests -- C5's own entry
// point into SigningService.RequestSignature.
func (s *Server) postSigningRequest(w http.ResponseWriter, r *http.Request) {
	var req postSigningRequestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.IdempotencyKey == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "idempotency_key is required"))
		return
	}

	digestBytes, err := hex.DecodeString(req.Digest)
	if err != nil || len(digestBytes) != 32 {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "digest must be 32 bytes, hex-encoded"))
		return
	}
	var digest [32]byte
	copy(digest[:], digestBytes)

	result, err := s.Signing.RequestSignature(r.Context(), req.SlotID, digest, req.EstimatedUSD, req.IdempotencyKey)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Metrics.recordSigningRequest(result.Status)
	respondJSON(w, http.StatusCreated, toSigningRequestResponse(result))
}

type postDepositSigningRequestRequest struct {
	BSCDepositIndex uint32  `json:"bsc_deposit_index"`
	Digest          string  `json:"digest"`
	EstimatedUSD    float64 `json:"estimated_usd"`
	IdempotencyKey  string  `json:"idempotency_key"`
}

// postDepositSigningRequest is POST /v1/deposit-signing-requests --
// depositwatcher's own entry point into
// SigningService.RequestDepositSweepSignature (see
// internal/requests/requests.go's own doc comment for why this is a
// separate method rather than overloading postSigningRequest's own
// SlotID field). Otherwise an exact mirror of postSigningRequest above.
func (s *Server) postDepositSigningRequest(w http.ResponseWriter, r *http.Request) {
	var req postDepositSigningRequestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.IdempotencyKey == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "idempotency_key is required"))
		return
	}

	digestBytes, err := hex.DecodeString(req.Digest)
	if err != nil || len(digestBytes) != 32 {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "digest must be 32 bytes, hex-encoded"))
		return
	}
	var digest [32]byte
	copy(digest[:], digestBytes)

	result, err := s.Signing.RequestDepositSweepSignature(r.Context(), req.BSCDepositIndex, digest, req.EstimatedUSD, req.IdempotencyKey)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Metrics.recordSigningRequest(result.Status)
	respondJSON(w, http.StatusCreated, toSigningRequestResponse(result))
}

type pendingSummaryResponse struct {
	ID              int64     `json:"id"`
	SlotID          *int      `json:"slot_id,omitempty"`
	BSCDepositIndex *uint32   `json:"bsc_deposit_index,omitempty"`
	EstimatedUSD    float64   `json:"estimated_usd"`
	CreatedAt       time.Time `json:"created_at"`
}

// getSigningRequests is GET /v1/signing-requests?status=pending -- the
// ops console's own OC.7 (see docs/03-build/ops-console-build-prompts.md).
// status=pending is the only supported value today: this route exists so
// an approver can discover WHICH requests are awaiting them, and every
// other status is already terminal (SIGNED/REJECTED), which
// GetSignature's own get-by-id already serves once the id is known from
// elsewhere. C5-scope-authenticated like every other read on this
// service -- see this route's own entry in the build doc for why a read
// here is fine with a shared token even though approve/reject is not.
func (s *Server) getSigningRequests(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status != "pending" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, `status must be "pending"`))
		return
	}
	pending, err := s.Signing.ListPending(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]pendingSummaryResponse, len(pending))
	for i, p := range pending {
		out[i] = pendingSummaryResponse{ID: p.ID, SlotID: p.SlotID, BSCDepositIndex: p.BSCDepositIndex, EstimatedUSD: p.EstimatedUSD, CreatedAt: p.CreatedAt}
	}
	respondJSON(w, http.StatusOK, map[string]any{"signing_requests": out})
}

// getSigningRequest is GET /v1/signing-requests/{id}.
func (s *Server) getSigningRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	result, err := s.Signing.GetSignature(r.Context(), id)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, toSigningRequestResponse(result))
}

// getSlotAddress is GET /v1/slots/{id}/address -- SigningService's own
// SlotAddress, over HTTP.
func (s *Server) getSlotAddress(w http.ResponseWriter, r *http.Request) {
	slotID, ok := urlParamInt(w, r, "id")
	if !ok {
		return
	}
	addr, err := s.Signing.SlotAddress(r.Context(), slotID)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"tron_address": addr})
}

// getSlotEVMAddress is GET /v1/slots/{id}/evm-address --
// SigningService's own EVMAddress, over HTTP. Added for Model F's own
// BEP20->TRC20 relay direction (see s1/internal/slots/evm.go's own doc
// comment): the SAME slot key SlotAddress already reports a TRON
// address for, in its EVM (BSC included) encoding.
func (s *Server) getSlotEVMAddress(w http.ResponseWriter, r *http.Request) {
	slotID, ok := urlParamInt(w, r, "id")
	if !ok {
		return
	}
	addr, err := s.Signing.EVMAddress(r.Context(), slotID)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"evm_address": addr})
}

// postApprove is POST /v1/signing-requests/{id}/approve -- approver-only
// (see auth.go). actor is resolved from the bearer token, never taken
// from the request body, same posture every prior component's own
// reviewer-facing endpoint takes toward actor identity.
func (s *Server) postApprove(w http.ResponseWriter, r *http.Request) {
	id, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	result, err := s.Signing.Approve(r.Context(), id, actorFromContext(r.Context()))
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Metrics.recordApproval("APPROVE")
	respondJSON(w, http.StatusOK, toSigningRequestResponse(result))
}

// postReject is POST /v1/signing-requests/{id}/reject -- approver-only.
func (s *Server) postReject(w http.ResponseWriter, r *http.Request) {
	id, ok := urlParamInt64(w, r, "id")
	if !ok {
		return
	}
	result, err := s.Signing.Reject(r.Context(), id, actorFromContext(r.Context()))
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Metrics.recordApproval("REJECT")
	respondJSON(w, http.StatusOK, toSigningRequestResponse(result))
}
