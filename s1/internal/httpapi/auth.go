package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
)

type contextKey int

const actorContextKey contextKey = iota

// AuthConfig maps bearer tokens to the service or human identity (actor)
// that token authenticates as. Same shape as every prior component's own
// AuthConfig -- S1 uses TWO separate instances, never one shared set:
// a token valid for C5's own signing-request calls must never also be
// valid for a human approve/reject call, and vice versa. Conflating them
// would mean a compromised C5 credential could approve its own
// over-threshold requests -- exactly the single-point-of-failure the
// 2-of-N human approval exists to rule out (see
// docs/02-architecture/s1-key-custody-architecture.md's own threat
// model).
type AuthConfig struct {
	Tokens map[string]string // token -> actor
}

// C5AuthConfigFromEnv reads S1_C5_API_TOKENS, formatted as
// "token1:actor1,token2:actor2" -- the credential set for
// RequestSignature/GetSignature/SlotAddress calls.
func C5AuthConfigFromEnv() (AuthConfig, error) {
	return authConfigFromEnv("S1_C5_API_TOKENS")
}

// ApproverAuthConfigFromEnv reads S1_APPROVER_API_TOKENS, same format --
// the credential set for POST .../approve and .../reject. Each token's
// own actor is the approver name recorded in signing_approvals.
func ApproverAuthConfigFromEnv() (AuthConfig, error) {
	return authConfigFromEnv("S1_APPROVER_API_TOKENS")
}

// ProvisioningAuthConfigFromEnv reads S1_PROVISIONING_API_TOKENS, same
// format -- the credential set for POST .../tron-deposit-keys. A
// separate scope from C5Auth/Approver: tronwatcher is a brand-new S1
// caller (previously only relayd talked to S1), and handing it a C5
// token would let a compromised tronwatcher credential request arbitrary
// slot signatures.
func ProvisioningAuthConfigFromEnv() (AuthConfig, error) {
	return authConfigFromEnv("S1_PROVISIONING_API_TOKENS")
}

func authConfigFromEnv(envVar string) (AuthConfig, error) {
	raw := os.Getenv(envVar)
	if raw == "" {
		return AuthConfig{}, fmt.Errorf("httpapi: %s is not set", envVar)
	}
	tokens := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		token, actor, ok := strings.Cut(pair, ":")
		if !ok || token == "" || actor == "" {
			return AuthConfig{}, fmt.Errorf("httpapi: malformed %s entry %q, want token:actor", envVar, pair)
		}
		tokens[token] = actor
	}
	if len(tokens) == 0 {
		return AuthConfig{}, fmt.Errorf("httpapi: %s contained no usable token:actor pairs", envVar)
	}
	return AuthConfig{Tokens: tokens}, nil
}

func authMiddleware(cfg AuthConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				writeAPIError(w, errUnauthorized)
				return
			}
			actor, ok := cfg.Tokens[token]
			if !ok {
				writeAPIError(w, errUnauthorized)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorContextKey, actor)))
		})
	}
}

func actorFromContext(ctx context.Context) string {
	actor, _ := ctx.Value(actorContextKey).(string)
	return actor
}
