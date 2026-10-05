// quota_middleware.go — wires quota.Engine.Charge into the aigw chain's
// RateLimit hook. Called from buildV04Stack (v04_wiring.go) after the chain
// and the quota engine have both been constructed.
package main

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"

	"github.com/ankoehn/burrow/internal/aigw"
	"github.com/ankoehn/burrow/internal/audit"
	"github.com/ankoehn/burrow/internal/quota"
)

// buildQuotaMiddleware returns a func(http.Handler) http.Handler that:
//  1. Reads quota.Subjects from the request context (injected by chain.go
//     step 3 before this middleware is called).
//  2. Calls engine.Charge(ctx, subjects, "rpm", 1).
//  3. On denial: writes 429 with a JSON body and emits a ratelimit.enforced
//     audit event; does NOT call next.
//  4. On allow: calls next.ServeHTTP(w, r).
//
// The auditLogger parameter may be nil; in that case the audit emission is
// skipped (nil-safe, consistent with the rest of the codebase).
func buildQuotaMiddleware(e *quota.Engine, auditLogger *audit.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if e == nil {
				next.ServeHTTP(w, r)
				return
			}

			ctx := r.Context()
			who := quota.SubjectsFromCtx(ctx)
			dec := e.Charge(ctx, who, quota.DimensionRPM, 1)
			if dec.Allow {
				next.ServeHTTP(w, r)
				return
			}

			// Denied — emit audit event (best-effort; nil-safe).
			if auditLogger != nil {
				ip, _, _ := net.SplitHostPort(r.RemoteAddr)
				_ = auditLogger.Append(ctx, audit.Event{
					Action:       audit.ActionRateLimitEnforced,
					SubjectID:    who.ServiceID,
					SubjectLabel: who.APIKeyID,
					Result:       "denied",
					SourceIP:     ip,
					UserAgent:    r.UserAgent(),
					Payload: audit.MustJSON(map[string]any{
						"scope":       dec.LimitingScope,
						"limit_id":    dec.LimitingID,
						"retry_after": dec.RetryAfter,
					}),
				})
			}

			// Write 429.
			if dec.RetryAfter > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(dec.RetryAfter))
			}
			// An entry point with its own error shape (the /ai/ gateway)
			// supplies the writer; the host route keeps the body below.
			if ew := aigw.ErrorWriterFrom(ctx); ew != nil {
				ew(w, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			body429, _ := json.Marshal(map[string]string{"error": "rate limit exceeded"})
			_, _ = w.Write(body429)
		})
	}
}
