// KelviOS Fase 1 WS10: auth enforcement on existing endpoints.
//
// Rule: every route under /api/* EXCEPT /api/auth/* requires a valid
// session (Authorization: Bearer <token> whose SHA-256 matches a
// non-expired row in sessions). Missing, malformed, unknown, or expired
// token → 401 with ONE generic body (no detail distinguishing the reason).
// /api/auth/* keeps its WS9 behavior; /healthz and the UI placeholder /
// stay open (not under /api/*).
//
// The middleware puts the session username into the request context;
// handlers that write audit rows use that principal as the audit actor
// (replacing the temporary "local" actor from WS8/WS9). "local" remains
// only as a defensive fallback when context carries no principal.

package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// principalCtxKey carries the session username for enforced routes.
type principalCtxKey struct{}

// authUnauthorizedBody is the ONE generic 401 body for all gate failures.
const authUnauthorizedBody = "unauthorized"

// writeUnauthorized writes the single generic 401 body.
func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": authUnauthorizedBody})
}

// actorFromRequest returns the session principal from context, or "local"
// as a defensive fallback (must not happen for enforced routes).
func actorFromRequest(r *http.Request) string {
	if v := r.Context().Value(principalCtxKey{}); v != nil {
		if name, ok := v.(string); ok && name != "" {
			return name
		}
	}
	return "local"
}

// requireAuth wraps h: requests under /api/* except /api/auth/*, plus /mcp,
// must carry a valid Bearer session token. Open paths (/healthz, /, /api/auth/*)
// pass through untouched. (WS45-B fix: /mcp is the only non-/api/ route
// wrapped in requireAuth, and the old /api/-only prefix rule let it through
// unauthenticated.)
func (s *Server) requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if (!strings.HasPrefix(p, "/api/") && p != "/mcp") || strings.HasPrefix(p, "/api/auth/") {
			h(w, r)
			return
		}
		username := s.sessionUsername(r)
		if username == "" {
			writeUnauthorized(w)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), principalCtxKey{}, username)))
	}
}
