package httpserver

// KelviOS Fase 1 WS9: auth core tests — setup, login, lockout, sessions, audit.
// Secrets (passwords, tokens) NEVER appear in audit detail — asserted below.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/chatbridge"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

const (
	testAuthUser = "admin"
	testAuthPass = "correct-horse-12"
	testWrongPw  = "wrong-password-1"
)

// fakeTransport captures replies for assertions (mirrors chatbridge test helper).
type fakeTransport struct {
	name string
	sent map[string][]string
	mu   sync.Mutex
}

func (f *fakeTransport) Name() string { return f.name }

func (f *fakeTransport) Send(chatID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sent == nil {
		f.sent = map[string][]string{}
	}
	f.sent[chatID] = append(f.sent[chatID], text)
	return nil
}

func (f *fakeTransport) count(chatID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent[chatID])
}

func (f *fakeTransport) last(chatID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs := f.sent[chatID]
	if len(msgs) == 0 {
		return ""
	}
	return msgs[len(msgs)-1]
}

func newAuthTestServer(t *testing.T, dbPath string) (*Server, *httptest.Server) {
	t.Helper()
	db, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		db.Close()
		t.Fatalf("migrate: %v", err)
	}
	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{Path: dbPath},
	}
	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	s := New(cfg, db, "test-version")
	s.SetDriver(storage.NewSingleDriver(disks))
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	ts := httptest.NewServer(s.srv.Handler)
	t.Cleanup(func() {
		ts.Close()
		s.Shutdown(context.Background())
		db.Close()
	})
	return s, ts
}

func postJSON(t *testing.T, url string, payload any, token string) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

func getWithToken(t *testing.T, url, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

func setupAdmin(t *testing.T, ts *httptest.Server) {
	t.Helper()
	st, _ := postJSON(t, ts.URL+"/api/auth/setup",
		map[string]string{"username": testAuthUser, "password": testAuthPass}, "")
	if st != http.StatusCreated {
		t.Fatalf("setup: got %d, want 201", st)
	}
}

func login(t *testing.T, ts *httptest.Server, user, pass string) (int, []byte) {
	t.Helper()
	return postJSON(t, ts.URL+"/api/auth/login",
		map[string]string{"username": user, "password": pass}, "")
}

// TestAuthSetupOnce: setup 201 once, then 409; short password 400.
func TestAuthSetupOnce(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")

	st, _ := postJSON(t, ts.URL+"/api/auth/setup",
		map[string]string{"username": testAuthUser, "password": "short"}, "")
	if st != http.StatusBadRequest {
		t.Errorf("short password: got %d, want 400", st)
	}

	setupAdmin(t, ts)

	st, _ = postJSON(t, ts.URL+"/api/auth/setup",
		map[string]string{"username": "other", "password": testAuthPass}, "")
	if st != http.StatusConflict {
		t.Errorf("second setup: got %d, want 409", st)
	}

	// Closed setup wins over password validation.
	st, _ = postJSON(t, ts.URL+"/api/auth/setup",
		map[string]string{"username": "other", "password": "short"}, "")
	if st != http.StatusConflict {
		t.Errorf("setup after closed with short pw: got %d, want 409", st)
	}
}

// TestAuthLoginMeLogout: login ok → me 200; logout invalidates (me 401);
// logout idempotent (unknown token 204).
func TestAuthLoginMeLogout(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)

	st, body := login(t, ts, testAuthUser, testAuthPass)
	if st != http.StatusOK {
		t.Fatalf("login: got %d, want 200", st)
	}
	var lr struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &lr); err != nil || lr.Token == "" {
		t.Fatalf("login response missing token: %v %s", err, body)
	}

	st, body = getWithToken(t, ts.URL+"/api/auth/me", lr.Token)
	if st != http.StatusOK {
		t.Fatalf("me: got %d, want 200", st)
	}
	var me struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(body, &me); err != nil || me.Username != testAuthUser {
		t.Fatalf("me response: %v %s", err, body)
	}

	st, _ = getWithToken(t, ts.URL+"/api/auth/me", "")
	if st != http.StatusUnauthorized {
		t.Errorf("me without token: got %d, want 401", st)
	}

	st, _ = postJSON(t, ts.URL+"/api/auth/logout", nil, lr.Token)
	if st != http.StatusNoContent {
		t.Errorf("logout: got %d, want 204", st)
	}

	st, _ = getWithToken(t, ts.URL+"/api/auth/me", lr.Token)
	if st != http.StatusUnauthorized {
		t.Errorf("me after logout: got %d, want 401", st)
	}

	st, _ = postJSON(t, ts.URL+"/api/auth/logout", nil, "bogus-token")
	if st != http.StatusNoContent {
		t.Errorf("logout unknown token: got %d, want 204", st)
	}
}

// TestAuthGenericFailureBody: unknown user and wrong password share ONE body.
func TestAuthGenericFailureBody(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)

	st1, body1 := login(t, ts, "nosuchuser", testWrongPw)
	st2, body2 := login(t, ts, testAuthUser, testWrongPw)
	if st1 != http.StatusUnauthorized || st2 != http.StatusUnauthorized {
		t.Fatalf("statuses: unknown=%d wrong-pw=%d, want 401/401", st1, st2)
	}
	if string(body1) != string(body2) {
		t.Errorf("bodies differ (enumeration oracle): %q vs %q", body1, body2)
	}
	if !strings.Contains(string(body1), authGenericFailBody) {
		t.Errorf("body missing generic message: %q", body1)
	}
}

// TestAuthLockoutFlow: 5 failures → 6th (correct pw) 423; expiry via SQL;
// persist across reopen; audit rows present without secrets.
func TestAuthLockoutFlow(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "catalog.db")

	s1, ts1 := newAuthTestServer(t, dbPath)
	setupAdmin(t, ts1)

	for i := 0; i < 5; i++ {
		st, _ := login(t, ts1, testAuthUser, testWrongPw)
		if st != http.StatusUnauthorized {
			t.Fatalf("failure %d: got %d, want 401", i+1, st)
		}
	}

	st, _ := login(t, ts1, testAuthUser, testAuthPass)
	if st != 423 {
		t.Fatalf("6th attempt with correct pw: got %d, want 423", st)
	}

	// Lockout persists across close/reopen of the same file DB.
	s1.Shutdown(context.Background())
	s1.db.Close()

	s2, ts2 := newAuthTestServer(t, dbPath)
	st, _ = login(t, ts2, testAuthUser, testAuthPass)
	if st != 423 {
		t.Fatalf("after reopen with correct pw: got %d, want 423", st)
	}

	// Expiry proven WITHOUT wall-clock waiting: move locked_until to past.
	if _, err := s2.db.Exec(`UPDATE accounts SET locked_until = ? WHERE username = ?`,
		time.Now().Unix()-1, testAuthUser); err != nil {
		t.Fatalf("move lockout to past: %v", err)
	}
	st, body := login(t, ts2, testAuthUser, testAuthPass)
	if st != http.StatusOK {
		t.Fatalf("login after expiry: got %d, want 200", st)
	}
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &lr); err != nil || lr.Token == "" {
		t.Fatalf("login after expiry missing token: %v", err)
	}

	// Audit: auth.login error rows + one auth.lockout row, no secrets.
	st, abody := getWithToken(t, ts2.URL+"/api/audit-log?action=auth.login&result=error", lr.Token)
	if st != http.StatusOK {
		t.Fatalf("audit-log list: got %d, want 200", st)
	}
	var al struct {
		Entries []struct {
			Action string `json:"action"`
			Detail string `json:"detail"`
		} `json:"entries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(abody, &al); err != nil {
		t.Fatalf("decode audit-log: %v", err)
	}
	if al.Total < 5 {
		t.Errorf("expected >=5 auth.login error rows, got %d", al.Total)
	}
	for _, e := range al.Entries {
		if strings.Contains(e.Detail, testAuthPass) || strings.Contains(e.Detail, testWrongPw) {
			t.Errorf("audit detail leaks password: %q", e.Detail)
		}
	}

	st, lbody := getWithToken(t, ts2.URL+"/api/audit-log?action=auth.lockout", lr.Token)
	if st != http.StatusOK {
		t.Fatalf("lockout audit list: got %d, want 200", st)
	}
	var ll struct {
		Entries []struct {
			Action string `json:"action"`
			Result string `json:"result"`
			Detail string `json:"detail"`
		} `json:"entries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(lbody, &ll); err != nil {
		t.Fatalf("decode lockout audit: %v", err)
	}
	if ll.Total != 1 {
		t.Errorf("expected exactly 1 auth.lockout row, got %d", ll.Total)
	} else {
		if ll.Entries[0].Result != "error" {
			t.Errorf("lockout result: got %q, want error", ll.Entries[0].Result)
		}
		if strings.Contains(ll.Entries[0].Detail, testAuthPass) || strings.Contains(ll.Entries[0].Detail, testWrongPw) {
			t.Errorf("lockout detail leaks password: %q", ll.Entries[0].Detail)
		}
	}
}

// TestAuthLockoutNotification: lockout triggers exactly ONE Chat Bridge notification.
// Subsequent attempts while locked do NOT send another notification.
// No linked chats = no-op (no error).
func TestAuthLockoutNotification(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "catalog.db")

	s, ts := newAuthTestServer(t, dbPath)
	setupAdmin(t, ts)

	// Pair a chat via chatbridge so there's a linked chat to notify.
	// Use the chatbridge service directly to create a pairing code and pair.
	pc, err := s.chatBridge.CreatePairingCode("telegram")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	// Create a fake transport to capture the notification
	fakeTG := &fakeTransport{name: "telegram", sent: map[string][]string{}}
	s.chatBridge.RegisterTransport(fakeTG)
	// Simulate pairing
	_ = s.chatBridge.HandleInbound(context.Background(), chatbridge.InboundMessage{
		Transport: "telegram",
		ChatID:    "chat-1",
		SenderID:  "user-1",
		Text:      "/pair " + pc.Code + " 1234",
		At:        time.Now(),
	})
	// Verify pairing succeeded
	link, err := s.chatBridge.GetLink("telegram", "chat-1")
	if err != nil || link == nil {
		t.Fatalf("pairing failed: link=%v err=%v", link, err)
	}

	// Reset message count after pairing (pairing sends a reply)
	fakeTG.sent = map[string][]string{}

	// 5 failed attempts
	for i := 0; i < 5; i++ {
		st, _ := login(t, ts, testAuthUser, testWrongPw)
		if st != http.StatusUnauthorized {
			t.Fatalf("failure %d: got %d, want 401", i+1, st)
		}
		t.Logf("After failure %d: notifications = %d", i+1, fakeTG.count("chat-1"))
	}

	// 6th attempt with CORRECT password triggers lockout (transition)
	// This should send exactly ONE notification
	st, _ := login(t, ts, testAuthUser, testAuthPass)
	if st != 423 {
		t.Fatalf("6th attempt (correct pw) should be 423 lockout: got %d", st)
	}
	t.Logf("After 6th attempt (correct pw): notifications = %d", fakeTG.count("chat-1"))

	// Poll until notification arrives (goroutine fire-and-forget)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fakeTG.count("chat-1") >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("After poll: notifications = %d", fakeTG.count("chat-1"))

	// Verify exactly ONE notification sent
	if fakeTG.count("chat-1") != 1 {
		t.Errorf("expected exactly 1 notification, got %d", fakeTG.count("chat-1"))
	}
	msg := fakeTG.last("chat-1")
	if !strings.Contains(msg, "KelviOS: akun "+testAuthUser+" terkunci 15 menit setelah 5x gagal login") {
		t.Errorf("notification message mismatch: %q", msg)
	}
	// No secrets in message
	forbidden := []string{testAuthPass, testWrongPw, "password", "token", "hash"}
	for _, f := range forbidden {
		if strings.Contains(strings.ToLower(msg), f) {
			t.Errorf("notification contains forbidden substring %q", f)
		}
	}

	// Subsequent attempt while locked (7th) should NOT send another notification
	st, _ = login(t, ts, testAuthUser, testAuthPass)
	if st != 423 {
		t.Fatalf("7th attempt while locked: got %d, want 423", st)
	}
	// Poll briefly to confirm no new notification
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if fakeTG.count("chat-1") > 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if fakeTG.count("chat-1") != 1 {
		t.Errorf("expected still 1 notification after locked attempt, got %d", fakeTG.count("chat-1"))
	}

	// Test: no linked chats = no-op (no error)
	// Create a fresh server without pairing
	_, ts2 := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts2)
	// No chatbridge transport registered, no links
	for i := 0; i < 5; i++ {
		st, _ = login(t, ts2, testAuthUser, testWrongPw)
		if st != http.StatusUnauthorized {
			t.Fatalf("no-chat failure %d: got %d, want 401", i+1, st)
		}
	}
	st, _ = login(t, ts2, testAuthUser, testAuthPass)
	if st != 423 {
		t.Fatalf("no-chat lockout: got %d, want 423", st)
	}
	// Should not panic or error even without chatbridge transport
}

// TestAuthExpiredSession: session row moved to past → me 401.
func TestAuthExpiredSession(t *testing.T) {
	s, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)

	_, body := login(t, ts, testAuthUser, testAuthPass)
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &lr); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	sum := sha256.Sum256([]byte(lr.Token))
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at = ? WHERE token_hash = ?`,
		time.Now().Unix()-1, hex.EncodeToString(sum[:])); err != nil {
		t.Fatalf("expire session: %v", err)
	}
	st, _ := getWithToken(t, ts.URL+"/api/auth/me", lr.Token)
	if st != http.StatusUnauthorized {
		t.Errorf("me with expired session: got %d, want 401", st)
	}
}

// TestAuthTokenStoredHashed: plaintext token must not appear in sessions table.
func TestAuthTokenStoredHashed(t *testing.T) {
	s, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)

	_, body := login(t, ts, testAuthUser, testAuthPass)
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &lr); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`, lr.Token).Scan(&count); err != nil {
		t.Fatalf("query sessions: %v", err)
	}
	if count != 0 {
		t.Errorf("plaintext token found in sessions table")
	}
	sum := sha256.Sum256([]byte(lr.Token))
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`,
		hex.EncodeToString(sum[:])).Scan(&count); err != nil || count != 1 {
		t.Errorf("hashed token row missing: count=%d err=%v", count, err)
	}
}
