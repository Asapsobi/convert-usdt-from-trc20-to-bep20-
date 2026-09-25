package httpapi

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RateLimit bounds how often one client may call a public endpoint. Every
// order leases a deposit wallet from a small pool for its whole deposit
// window, so an unbounded caller could hold every wallet and turn real
// customers away.
type RateLimit struct {
	// PerMinute is the sustained rate; Burst how many may come at once.
	PerMinute float64
	Burst     float64
	// TrustProxy takes the client's address from X-Forwarded-For (set
	// only when relayd sits behind a proxy that sets it).
	TrustProxy bool

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func (l *RateLimit) clientIP(r *http.Request) string {
	if l.TrustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			first, _, _ := strings.Cut(fwd, ",")
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// allow takes one token from key's bucket.
func (l *RateLimit) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[string]*bucket{}
	}
	// Forget idle clients now and then, so the map can't grow forever.
	if now.Sub(l.swept) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.swept = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.Burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.Burst, b.tokens+now.Sub(b.last).Minutes()*l.PerMinute)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Middleware rejects a client over its rate with 429. A nil limit lets
// everything through.
func (l *RateLimit) Middleware(next http.Handler) http.Handler {
	if l == nil || l.PerMinute <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(l.clientIP(r)+" "+r.URL.Path, time.Now()) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, errors.New("too many requests -- please wait a minute and try again"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
