package httpapi

import "net/http"

// corsMiddleware lets this API be called directly from a browser --
// mirrors gateway/internal/httpapi/cors.go's own identical middleware
// verbatim. Wildcarding Access-Control-Allow-Origin is safe here for
// the same reason that file's own comment gives: this service has no
// auth at all yet (see server.go's own doc comment), let alone a
// cookie-based one -- there's no ambient credential for a permissive
// origin to leak.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Idempotency-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
