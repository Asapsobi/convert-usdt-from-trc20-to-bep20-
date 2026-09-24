package httpapi

import "net/http"

type postTronDepositKeyRequest struct {
	DepositIndex uint32 `json:"deposit_index"`
}

type tronDepositKeyResponse struct {
	DepositIndex uint32 `json:"deposit_index"`
	Address      string `json:"address"`
}

// postTronDepositKey is POST /v1/tron-deposit-keys -- tronwatcher's own
// entry point for provisioning the real custody (a Privy Server Wallet)
// backing one TRON deposit index, called once at address-issuance time
// (see tronwatcher/internal/addresses/store.go's own Assign). Idempotent
// purely by deposit_index, so tronwatcher needs no separate
// idempotency-key field the way postSigningRequest does -- a retried
// call for the same index always returns the same address, never mints
// a second wallet. Responds 200, not 201, matching tronwatcher's own
// postAddress convention ("always 200, idempotent") rather than
// postSigningRequest's own "each call may create a new resource" 201.
func (s *Server) postTronDepositKey(w http.ResponseWriter, r *http.Request) {
	var req postTronDepositKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	result, err := s.Signing.ProvisionTronDepositKey(r.Context(), req.DepositIndex)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, tronDepositKeyResponse{DepositIndex: result.Index, Address: result.Address})
}

type postBSCDepositKeyRequest struct {
	DepositIndex uint32 `json:"deposit_index"`
}

type bscDepositKeyResponse struct {
	DepositIndex uint32 `json:"deposit_index"`
	Address      string `json:"address"`
}

// postBSCDepositKey is postTronDepositKey's own BSC-deposit counterpart
// -- POST /v1/bsc-deposit-keys, depositwatcher's own entry point for
// provisioning the real custody (a Privy Server Wallet) backing one BSC
// deposit index, called once at address-issuance time (see
// depositwatcher/internal/addresses/store.go's own Assign). See that
// method's own doc comment for the idempotency/response-code reasoning
// -- identical here.
func (s *Server) postBSCDepositKey(w http.ResponseWriter, r *http.Request) {
	var req postBSCDepositKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	result, err := s.Signing.ProvisionBSCDepositKey(r.Context(), req.DepositIndex)
	if err != nil {
		writeErr(w, err)
		return
	}
	respondJSON(w, http.StatusOK, bscDepositKeyResponse{DepositIndex: result.Index, Address: result.Address})
}
