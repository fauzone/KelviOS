package httpserver

// KelviOS Fase 1 WS16: share-link HTTP tests.
// Covers create/list/revoke (auth) and public access (no auth).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

func newShareLinkTestServer(t *testing.T, dbPath string) (*Server, *httptest.Server) {
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

func setupAdminForShareLinks(t *testing.T, ts *httptest.Server) {
	t.Helper()
	st, _ := postJSON(t, ts.URL+"/api/auth/setup",
		map[string]string{"username": "admin", "password": "correct-horse-12"}, "")
	if st != http.StatusCreated {
		t.Fatalf("setup: got %d, want 201", st)
	}
}

func loginForShareLinks(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	st, body := postJSON(t, ts.URL+"/api/auth/login",
		map[string]string{"username": "admin", "password": "correct-horse-12"}, "")
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

func createPoolAndCollection(t *testing.T, ts *httptest.Server, token string, tier string) string {
	t.Helper()
	// Create pool
	resp := doReqJSON(t, ts, "POST", "/api/pools", map[string]any{
		"name":       "media",
		"tier":       tier,
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
	}, token)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("POST pool: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var poolResp struct {
		Pool struct {
			ID string `json:"id"`
		} `json:"pool"`
	}
	json.NewDecoder(resp.Body).Decode(&poolResp)
	resp.Body.Close()
	poolID := poolResp.Pool.ID

	// Create collection
	resp = doReqJSON(t, ts, "POST", "/api/pools/"+poolID+"/collections", map[string]any{
		"name":      "photos",
		"path":      "/photos",
		"disk_name": "sda",
	}, token)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("POST collection: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var collResp struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	json.NewDecoder(resp.Body).Decode(&collResp)
	resp.Body.Close()
	collID := collResp.Collection.ID

	return collID
}

func indexFileInCollection(t *testing.T, s *Server, collID string, relPath string, content string) {
	t.Helper()
	tmpDir := t.TempDir()
	mountpoint := filepath.Join(tmpDir, "mnt", "media")
	root := filepath.Join(mountpoint, "photos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, relPath), []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	// Get pool and collection from DB
	coll, err := catalog.GetCollection(s.db, collID)
	if err != nil {
		t.Fatalf("get collection: %v", err)
	}
	pool, _, err := catalog.GetPool(s.db, coll.PoolID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	pool.Mountpoint = mountpoint

	// Update pool mountpoint in DB so public share access can find the file
	if _, err := s.db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, mountpoint, pool.ID); err != nil {
		t.Fatalf("update pool mountpoint: %v", err)
	}

	if _, err := catalog.IndexCollectionFiles(s.db, coll, pool); err != nil {
		t.Fatalf("index collection files: %v", err)
	}
}

func doReqJSON(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) *http.Response {
	t.Helper()
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, path, err)
	}
	return resp
}

func readJSONBody(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("decode body: %v (body: %s)", err, string(b))
	}
}

// TestShareLinkCreateListRevoke tests create (201 + token), list (no token hash),
// unauth (401), past-expiry (400), file-not-indexed (404).
func TestShareLinkCreateListRevoke(t *testing.T) {
	s, ts := newShareLinkTestServer(t, ":memory:")
	setupAdminForShareLinks(t, ts)
	token := loginForShareLinks(t, ts)

	collID := createPoolAndCollection(t, ts, token, "warm")

	// Index a file so it exists in catalog
	indexFileInCollection(t, s, collID, "hello.txt", "hello world")

	// 1. Create share link -> 201 + token returned once
	resp := doReqJSON(t, ts, "POST", "/api/collections/"+collID+"/share-links", map[string]any{
		"rel_path": "hello.txt",
	}, token)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create share link: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var createResp struct {
		ShareLink struct {
			ID          string `json:"id"`
			RelPath     string `json:"rel_path"`
			Token       string `json:"token"`
			HasPassword bool   `json:"has_password"`
		} `json:"share_link"`
	}
	readJSONBody(t, resp, &createResp)
	if createResp.ShareLink.Token == "" {
		t.Error("create response missing raw token")
	}
	if createResp.ShareLink.RelPath != "hello.txt" {
		t.Errorf("rel_path mismatch: got %s, want hello.txt", createResp.ShareLink.RelPath)
	}
	if createResp.ShareLink.HasPassword {
		t.Error("has_password should be false when no password provided")
	}
	linkID := createResp.ShareLink.ID
	rawToken := createResp.ShareLink.Token
	_ = rawToken

	// 2. List share links -> 200, no token, no password hash
	resp = doReqJSON(t, ts, "GET", "/api/collections/"+collID+"/share-links", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list share links: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var listResp struct {
		ShareLinks []struct {
			ID          string `json:"id"`
			RelPath     string `json:"rel_path"`
			HasPassword bool   `json:"has_password"`
			Token       string `json:"token"`
		} `json:"share_links"`
	}
	readJSONBody(t, resp, &listResp)
	if len(listResp.ShareLinks) != 1 {
		t.Errorf("expected 1 share link, got %d", len(listResp.ShareLinks))
	}
	if listResp.ShareLinks[0].ID != linkID {
		t.Errorf("link ID mismatch: got %s, want %s", listResp.ShareLinks[0].ID, linkID)
	}
	if listResp.ShareLinks[0].Token != "" {
		t.Error("list response should NOT contain raw token")
	}
	if listResp.ShareLinks[0].HasPassword {
		t.Error("has_password should be false in list")
	}

	// 3. Unauth -> 401
	resp = doReqJSON(t, ts, "POST", "/api/collections/"+collID+"/share-links", map[string]any{
		"rel_path": "hello.txt",
	}, "")
	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Errorf("unauth create: expected 401, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	resp = doReqJSON(t, ts, "GET", "/api/collections/"+collID+"/share-links", nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Errorf("unauth list: expected 401, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 4. Past expiry -> 400
	past := time.Now().Add(-time.Hour).Unix()
	resp = doReqJSON(t, ts, "POST", "/api/collections/"+collID+"/share-links", map[string]any{
		"rel_path":  "hello.txt",
		"expires_at": past,
	}, token)
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Errorf("past expiry: expected 400, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 5. File not indexed -> 404
	resp = doReqJSON(t, ts, "POST", "/api/collections/"+collID+"/share-links", map[string]any{
		"rel_path": "not-indexed.txt",
	}, token)
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Errorf("file not indexed: expected 404, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 6. Revoke -> 204
	resp = doReqJSON(t, ts, "DELETE", "/api/collections/"+collID+"/share-links/"+linkID, nil, token)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Errorf("revoke: expected 204, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 7. List after revoke -> link still listed but revoked_at set
	resp = doReqJSON(t, ts, "GET", "/api/collections/"+collID+"/share-links", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("list after revoke: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var listResp2 struct {
		ShareLinks []struct {
			ID       string `json:"id"`
			RevokedAt *int64 `json:"revoked_at,omitempty"`
		} `json:"share_links"`
	}
	readJSONBody(t, resp, &listResp2)
	if len(listResp2.ShareLinks) != 1 {
		t.Errorf("expected 1 share link after revoke, got %d", len(listResp2.ShareLinks))
	}
	if listResp2.ShareLinks[0].RevokedAt == nil {
		t.Error("revoked_at should be set after revoke")
	}
}

// TestShareLinkPublicAccess tests public share access (no auth):
// no-password 200, with-password missing 401, wrong 401, correct 200,
// unknown token 404, revoked 410, expired 410.
func TestShareLinkPublicAccess(t *testing.T) {
	s, ts := newShareLinkTestServer(t, ":memory:")
	setupAdminForShareLinks(t, ts)
	token := loginForShareLinks(t, ts)

	collID := createPoolAndCollection(t, ts, token, "warm")

	// Index a file
	indexFileInCollection(t, s, collID, "hello.txt", "hello world")

	// Create share link WITHOUT password
	resp := doReqJSON(t, ts, "POST", "/api/collections/"+collID+"/share-links", map[string]any{
		"rel_path": "hello.txt",
	}, token)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create share link: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var createResp struct {
		ShareLink struct {
			ID     string `json:"id"`
			Token  string `json:"token"`
		} `json:"share_link"`
	}
	readJSONBody(t, resp, &createResp)
	linkID := createResp.ShareLink.ID
	rawToken := createResp.ShareLink.Token
	_ = rawToken

	// Public access without password -> 200
	resp = doReqJSON(t, ts, "GET", "/api/public/share/"+rawToken, nil, "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("public no-password: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Create share link WITH password
	resp = doReqJSON(t, ts, "POST", "/api/collections/"+collID+"/share-links", map[string]any{
		"rel_path":  "hello.txt",
		"password":  "secret123",
	}, token)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create share link with password: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var createResp2 struct {
		ShareLink struct {
			ID     string `json:"id"`
			Token  string `json:"token"`
		} `json:"share_link"`
	}
	readJSONBody(t, resp, &createResp2)
	pwLinkID := createResp2.ShareLink.ID
	pwToken := createResp2.ShareLink.Token

	// Public access with password, missing -> 401
	resp = doReqJSON(t, ts, "GET", "/api/public/share/"+pwToken, nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Errorf("public password missing: expected 401, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Public access with password, wrong -> 401
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/public/share/"+pwToken, nil)
	req.Header.Set("X-Share-Password", "wrong-password")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Errorf("public password wrong: expected 401, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Public access with password, correct -> 200
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/api/public/share/"+pwToken, nil)
	req.Header.Set("X-Share-Password", "secret123")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Errorf("public password correct: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Unknown token -> 404
	resp = doReqJSON(t, ts, "GET", "/api/public/share/unknown-token-12345", nil, "")
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Errorf("unknown token: expected 404, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Revoke the first link, then public access -> 410
	resp = doReqJSON(t, ts, "DELETE", "/api/collections/"+collID+"/share-links/"+linkID, nil, token)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("revoke: expected 204, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	resp = doReqJSON(t, ts, "GET", "/api/public/share/"+rawToken, nil, "")
	if resp.StatusCode != http.StatusGone {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Errorf("revoked public access: expected 410, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Expired link -> 410 (set expires_at to past via DB update)
	expired := time.Now().Add(-time.Hour).Unix()
	if _, err := s.db.Exec(`UPDATE share_links SET expires_at = ? WHERE id = ?`, expired, pwLinkID); err != nil {
		t.Fatalf("set expired: %v", err)
	}
	resp = doReqJSON(t, ts, "GET", "/api/public/share/"+pwToken, nil, "")
	if resp.StatusCode != http.StatusGone {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Errorf("expired public access: expected 410, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()
}

// TestShareLinkPublicColdTier tests that public access on a cold-tier pool
// returns 409 with wake_required:true.
func TestShareLinkPublicColdTier(t *testing.T) {
	s, ts := newShareLinkTestServer(t, ":memory:")
	setupAdminForShareLinks(t, ts)
	token := loginForShareLinks(t, ts)

	collID := createPoolAndCollection(t, ts, token, "cold")

	// Index a file
	indexFileInCollection(t, s, collID, "cold.txt", "cold data")

	// Create share link
	resp := doReqJSON(t, ts, "POST", "/api/collections/"+collID+"/share-links", map[string]any{
		"rel_path": "cold.txt",
	}, token)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create share link: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var createResp struct {
		ShareLink struct {
			Token string `json:"token"`
		} `json:"share_link"`
	}
	readJSONBody(t, resp, &createResp)
	rawToken := createResp.ShareLink.Token
	_ = rawToken

	// Public access on cold tier -> 409 wake_required:true
	resp = doReqJSON(t, ts, "GET", "/api/public/share/"+rawToken, nil, "")
	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Errorf("public cold tier: expected 409, got %d: %s", resp.StatusCode, string(b))
	}
	var body struct {
		Error         string `json:"error"`
		WakeRequired  bool   `json:"wake_required"`
	}
	readJSONBody(t, resp, &body)
	if !body.WakeRequired {
		t.Error("wake_required should be true for cold tier")
	}
	if !strings.Contains(body.Error, "cold") {
		t.Errorf("error should mention cold tier, got: %s", body.Error)
	}
}
