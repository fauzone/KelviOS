package httpserver

// KelviOS Fase 1 WS10: auth enforcement tests — 401 gate on every
// /api/* route except /api/auth/*, valid-token behavior intact,
// logout invalidation, expired sessions, and session-principal audit actor.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mustToken provisions an admin via /api/auth/setup + /api/auth/login and
// returns a Bearer token for ts. Setup is closed after the first call;
// login uses the same credentials.
func mustToken(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	st, _ := postJSON(t, ts.URL+"/api/auth/setup",
		map[string]string{"username": testAuthUser, "password": testAuthPass}, "")
	if st != http.StatusCreated && st != http.StatusConflict {
		t.Fatalf("setup: got %d, want 201 or 409", st)
	}
	st, body := login(t, ts, testAuthUser, testAuthPass)
	if st != http.StatusOK {
		t.Fatalf("login: got %d, want 200", st)
	}
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &lr); err != nil || lr.Token == "" {
		t.Fatalf("login response missing token: %v %s", err, body)
	}
	return lr.Token
}

// TestAuthEnforcementGate: unauthenticated, invalid, expired, and
// logged-out tokens all get the ONE generic 401 body; valid token keeps
// prior behavior; /healthz and / stay open.
func TestAuthEnforcementGate(t *testing.T) {
	s, ts := newAuthTestServer(t, ":memory:")
	token := mustToken(t, ts)

	// 1. Unauthenticated GET /api/pools → 401 with generic body.
	st, body := getWithToken(t, ts.URL+"/api/pools", "")
	if st != http.StatusUnauthorized {
		t.Fatalf("GET /api/pools without token: got %d, want 401", st)
	}
	var eb struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &eb); err != nil || eb.Error != authUnauthorizedBody {
		t.Fatalf("401 body: got %q, want %q", body, authUnauthorizedBody)
	}

	// 2. Unauthenticated POST /api/pools → 401.
	st, _ = postJSON(t, ts.URL+"/api/pools", map[string]any{"name": "x"}, "")
	if st != http.StatusUnauthorized {
		t.Fatalf("POST /api/pools without token: got %d, want 401", st)
	}

	// 3. Invalid token → 401 with the SAME body (no oracle).
	st, ibody := getWithToken(t, ts.URL+"/api/pools", "bogus-token")
	if st != http.StatusUnauthorized || string(ibody) != string(body) {
		t.Fatalf("invalid token: got %d %q, want 401 %q", st, ibody, body)
	}

	// 4. Expired session (row moved to past) → 401.
	sum := sha256.Sum256([]byte(token))
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at = ? WHERE token_hash = ?`,
		time.Now().Unix()-1, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("expire session: %v", err)
	}
	st, ebody := getWithToken(t, ts.URL+"/api/pools", token)
	if st != http.StatusUnauthorized || string(ebody) != string(body) {
		t.Fatalf("expired token: got %d %q, want 401 %q", st, ebody, body)
	}

	// 5. Fresh login → valid token keeps prior behavior (pool CRUD + audit-log + daemon-config read).
	token = mustToken(t, ts)
	st, _ = getWithToken(t, ts.URL+"/api/pools", token)
	if st != http.StatusOK {
		t.Fatalf("GET /api/pools with token: got %d, want 200", st)
	}
	st, _ = getWithToken(t, ts.URL+"/api/audit-log", token)
	if st != http.StatusOK {
		t.Fatalf("GET /api/audit-log with token: got %d, want 200", st)
	}
	st, _ = getWithToken(t, ts.URL+"/api/daemon-config", token)
	if st != http.StatusOK {
		t.Fatalf("GET /api/daemon-config with token: got %d, want 200", st)
	}

	// 6. Token after logout → 401 on the next call.
	st, _ = postJSON(t, ts.URL+"/api/auth/logout", nil, token)
	if st != http.StatusNoContent {
		t.Fatalf("logout: got %d, want 204", st)
	}
	st, lbody := getWithToken(t, ts.URL+"/api/pools", token)
	if st != http.StatusUnauthorized || string(lbody) != string(body) {
		t.Fatalf("after logout: got %d %q, want 401 %q", st, lbody, body)
	}

	// 7. /healthz stays open without token.
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz without token: got %d, want 200", resp.StatusCode)
	}
}

// TestAuthEnforcementAuditActor: rows written via an authenticated request
// carry actor = the session username, never "local".
func TestAuthEnforcementAuditActor(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	token := mustToken(t, ts)

	st, body := postJSON(t, ts.URL+"/api/pools",
		map[string]any{"name": "bad", "tier": "warm", "mechanism": "single",
			"disks": []string{"nonexistent-disk"}, "mountpoint": "/mnt/x"}, token)
	if st != http.StatusBadRequest {
		t.Fatalf("bad pool create: got %d, want 400", st)
	}

	st, abody := getWithToken(t, ts.URL+"/api/audit-log?action=pool.create&result=error", token)
	if st != http.StatusOK {
		t.Fatalf("audit-log: got %d, want 200", st)
	}
	var al struct {
		Entries []struct {
			Actor string `json:"actor"`
		} `json:"entries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(abody, &al); err != nil {
		t.Fatalf("decode audit-log: %v", err)
	}
	if al.Total != 1 {
		t.Fatalf("expected 1 audit row, got %d", al.Total)
	}
	if al.Entries[0].Actor != testAuthUser {
		t.Fatalf("audit actor: got %q, want %q", al.Entries[0].Actor, testAuthUser)
	}
	_ = body
}
