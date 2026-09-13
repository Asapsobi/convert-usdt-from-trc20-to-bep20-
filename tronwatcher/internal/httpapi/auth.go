package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
)

type contextKey int

const (
	actorContextKey contextKey = iota
	idempotencyKeyContextKey
)

// AuthConfig maps bearer tokens to the service identity (actor) that
// token authenticates as. Mirrors every sibling service's own
// AuthConfig.
type AuthConfig struct {
	Tokens map[string]string // token -> actor
}

// AuthConfigFromEnv reads TRONWATCHER_API_TOKENS, formatted as
// "token1:actor1,token2:actor2". At least one usable pair is required.
func AuthConfigFromEnv() (AuthConfig, error) {
	raw := os.Getenv("TRONWATCHER_API_TOKENS")
	if raw == "" {
		return AuthConfig{}, fmt.Errorf("httpapi: TRONWATCHER_API_TOKENS is not set")
	}
	tokens := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		token, actor, ok := strings.Cut(pair, ":")
		if !ok || token == "" || actor == "" {
			return AuthConfig{}, fmt.Errorf("httpapi: malformed TRONWATCHER_API_TOKENS entry %q, want token:actor", pair)
		}
		tokens[token] = actor
	}
	if len(tokens) == 0 {
		return AuthConfig{}, fmt.Errorf("httpapi: TRONWATCHER_API_TOKENS contained no usable token:actor pairs")
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

// requireIdempotencyKey rejects a write request with no Idempotency-Key
// header before any handler runs.
func requireIdempotencyKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			writeAPIError(w, newAPIError(http.StatusBadRequest, errInvalidRequest.Code, "Idempotency-Key header is required"))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), idempotencyKeyContextKey, key)))
	})
}
