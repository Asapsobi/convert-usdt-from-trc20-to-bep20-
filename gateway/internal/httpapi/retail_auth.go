// Model D's own B2C channel (docs/01-strategy/model-d-model-f-product-separation.md,
// 14 Sep 2026 decision): registration, login, logout, and session
// auth for an individual end-user, entirely separate from
// authMiddleware's own B2B API-key auth above -- a retail customer never
// holds an API key, and a B2B customer never logs in with a password.
// Routes live under /v1/retail/... (see server.go's own NewRouter).
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"gateway/internal/retailcustomers"
	"gateway/internal/retailsessions"
)

const retailCustomerContextKey contextKey = iota + 100 // offset from customerContextKey's own iota block

// retailCustomerFromContext returns the authenticated retail customer
// retailAuthMiddleware placed on the request context. Panics if called
// on a request that never passed through retailAuthMiddleware -- a
// handler bug, mirroring customerFromContext's own panic-not-error
// posture for the identical class of mistake.
func retailCustomerFromContext(ctx context.Context) retailcustomers.RetailCustomer {
	c, ok := ctx.Value(retailCustomerContextKey).(retailcustomers.RetailCustomer)
	if !ok {
		panic("httpapi: retailCustomerFromContext called without retailAuthMiddleware having run")
	}
	return c
}

// retailAuthMiddleware resolves the bearer session token to a retail
// customer (401 if missing/invalid/expired/revoked) and places it on
// the request context. Unlike authMiddleware, there is no rate limiter
// here yet -- retail traffic shape (one browser, occasional requests)
// doesn't need C6.1's own per-partner throughput gate; revisit if retail
// volume ever needs one.
func retailAuthMiddleware(sessions *retailsessions.Store, retailCustomers *retailcustomers.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				writeAPIError(w, errRetailUnauthorized)
				return
			}
			sess, err := sessions.Validate(r.Context(), token, time.Now().UTC())
			if err != nil {
				writeAPIError(w, errRetailUnauthorized)
				return
			}
			rc, err := retailCustomers.Get(r.Context(), sess.RetailCustomerID)
			if err != nil {
				writeAPIError(w, errRetailUnauthorized)
				return
			}
			if rc.Status != retailcustomers.StatusActive {
				writeAPIError(w, errRetailSuspended)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), retailCustomerContextKey, rc)))
		})
	}
}

type postRetailRegisterRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type retailSessionResponse struct {
	SessionToken     string `json:"session_token"`
	RetailCustomerID int64  `json:"retail_customer_id"`
	ExpiresAt        string `json:"expires_at"`
}

// minPasswordLength is a floor, not a full strength policy -- this
// project has no product decision yet on password complexity rules;
// bcrypt's own cost already does the real work against brute-forcing
// whatever a user picks. Revisit if a real policy is ever specified.
const minPasswordLength = 8

// postRetailRegister is POST /v1/retail/register -- creates an account
// and immediately issues a session (auto-login on register, standard
// consumer-product UX), mirroring how customers.Store.Create returns a
// usable credential directly rather than requiring a separate
// activation step.
func (s *Server) postRetailRegister(w http.ResponseWriter, r *http.Request) {
	var req postRetailRegisterRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" || len(req.Password) < minPasswordLength {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code,
			"email is required and password must be at least 8 characters"))
		return
	}

	rc, err := s.RetailCustomers.Register(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, retailcustomers.ErrEmailTaken) {
			writeAPIError(w, errEmailTaken)
			return
		}
		writeAPIError(w, errInternal)
		return
	}

	sess, rawToken, err := s.RetailSessions.Create(r.Context(), rc.ID, time.Now().UTC())
	if err != nil {
		writeAPIError(w, errInternal)
		return
	}
	respondJSON(w, http.StatusCreated, retailSessionResponse{
		SessionToken: rawToken, RetailCustomerID: rc.ID, ExpiresAt: sess.ExpiresAt.Format(timeLayout),
	})
}

type postRetailLoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// postRetailLogin is POST /v1/retail/login.
func (s *Server) postRetailLogin(w http.ResponseWriter, r *http.Request) {
	var req postRetailLoginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" || req.Password == "" {
		writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "email and password are both required"))
		return
	}

	rc, err := s.RetailCustomers.Authenticate(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, retailcustomers.ErrInvalidCredentials) {
			writeAPIError(w, errInvalidCredentials)
			return
		}
		if errors.Is(err, retailcustomers.ErrSuspended) {
			writeAPIError(w, errRetailSuspended)
			return
		}
		writeAPIError(w, errInternal)
		return
	}

	sess, rawToken, err := s.RetailSessions.Create(r.Context(), rc.ID, time.Now().UTC())
	if err != nil {
		writeAPIError(w, errInternal)
		return
	}
	respondJSON(w, http.StatusOK, retailSessionResponse{
		SessionToken: rawToken, RetailCustomerID: rc.ID, ExpiresAt: sess.ExpiresAt.Format(timeLayout),
	})
}

// postRetailLogout is POST /v1/retail/logout -- revokes the presented
// session. Idempotent (see retailsessions.Store.Revoke's own doc
// comment): logging out an already-logged-out session is a success, not
// an error.
func (s *Server) postRetailLogout(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		writeAPIError(w, errRetailUnauthorized)
		return
	}
	if err := s.RetailSessions.Revoke(r.Context(), token); err != nil {
		writeAPIError(w, errInternal)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type retailMeResponse struct {
	RetailCustomerID int64  `json:"retail_customer_id"`
	Email            string `json:"email"`
}

// getRetailMe is GET /v1/retail/me -- confirms the current session and
// reports who it belongs to, the same "what does my own credential
// resolve to" convenience every session-based product offers.
func (s *Server) getRetailMe(w http.ResponseWriter, r *http.Request) {
	rc := retailCustomerFromContext(r.Context())
	respondJSON(w, http.StatusOK, retailMeResponse{RetailCustomerID: rc.ID, Email: rc.Email})
}
