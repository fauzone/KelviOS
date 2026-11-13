package httpserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

// setupDirtyShadowServer creates a test server with:
// - warm pool (target for shadows) with temp mountpoint
// - cold pool (source) with temp mountpoint
// - one collection on cold pool with a fixture file
// - a shadow created on warm pool for the cold file
// - the shadow made dirty via upload-overwrite while cold is asleep
// Returns ts, token, warmPoolID, coldPoolID, collID, tmpDir, coldDiskName, shadowRelPath
func setupDirtyShadowServer(t *testing.T, db *sql.DB) (*httptest.Server, string, string, string, string, string, string, string) {
	t.Helper()
	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{Path: ":memory:"},
	}
	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-DIRTY-WARM", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
		{Name: "sdb", Path: "/dev/sdb", Model: "WDC WD40EFRX", Serial: "WD-DIRTY-COLD", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	s := New(cfg, db, "test-version")
	s.SetDriver(storage.NewSingleDriver(disks))
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	ts := httptest.NewServer(s.srv.Handler)

	doReq := func(method, path string, payload any, token string) *http.Response {
		var body io.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+path, body)
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

	// Auth: setup + login
	resp := doReq("POST", "/api/auth/setup", map[string]any{"username": "admin", "password": "adminpassword123"}, "")
	resp.Body.Close()
	resp = doReq("POST", "/api/auth/login", map[string]any{"username": "admin", "password": "adminpassword123"}, "")
	var login struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	resp.Body.Close()
	token := login.Token

	tmpDir := t.TempDir()
	warmMount := filepath.Join(tmpDir, "mnt", "warm")
	coldMount := filepath.Join(tmpDir, "mnt", "cold")
	if err := os.MkdirAll(filepath.Join(coldMount, "photos"), 0o755); err != nil {
		t.Fatalf("mkdir cold tree: %v", err)
	}
	if err := os.MkdirAll(warmMount, 0o755); err != nil {
		t.Fatalf("mkdir warm mount: %v", err)
	}
	// Fixture file on cold pool
	if err := os.WriteFile(filepath.Join(coldMount, "photos", "a.txt"), []byte("hello from cold"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	createPool := func(name, tier, disk, mount string) string {
		resp := doReq("POST", "/api/pools", map[string]any{"name": name, "tier": tier, "mechanism": "single", "disks": []string{disk}, "mountpoint": mount}, token)
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST pool %s: expected 201, got %d: %s", name, resp.StatusCode, string(b))
		}
		var pr struct {
			Pool struct {
				ID string `json:"id"`
			} `json:"pool"`
		}
		json.NewDecoder(resp.Body).Decode(&pr)
		resp.Body.Close()
		return pr.Pool.ID
	}
	warmPoolID := createPool("warm", "warm", "sda", warmMount)
	coldPoolID := createPool("cold", "cold", "sdb", coldMount)

	resp = doReq("POST", "/api/pools/"+coldPoolID+"/collections", map[string]any{"name": "photos", "path": "photos", "disk_name": "sdb"}, token)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
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

	// Scan the collection so the fixture file is indexed
	resp = doReq("POST", "/api/collections/"+collID+"/scan", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST scan: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Wake cold disk to allow shadow creation (downloads need cold awake)
	resp = doReq("POST", "/api/power/sdb/wake", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST wake cold: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// First download (1st access -> no shadow yet)
	resp = doReq("GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET 1st download: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Second download (2nd access within window -> shadow created on warm)
	resp = doReq("GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET 2nd download: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Sleep cold disk to ensure it's asleep for dirty write-back
	resp = doReq("POST", "/api/power/sdb/sleep", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST sleep cold: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Make the shadow dirty via upload-overwrite while cold is asleep
	// The upload endpoint is POST /api/collections/{id}/files/content?path=<rel_path>&overwrite=true
	uploadBody := bytes.NewReader([]byte("dirty shadow content"))
	req, _ := http.NewRequest("POST", ts.URL+"/api/collections/"+collID+"/files/content?path=a.txt&overwrite=true", uploadBody)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST dirty upload: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST dirty upload: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var uploadResp struct {
		Status   string `json:"status"`
		Path     string `json:"path"`
		Shadowed string `json:"shadowed"`
		Dirty    string `json:"dirty"`
	}
	json.NewDecoder(resp.Body).Decode(&uploadResp)
	resp.Body.Close()
	if uploadResp.Shadowed != "true" || uploadResp.Dirty != "true" {
		t.Fatalf("dirty upload response unexpected: %v", uploadResp)
	}

	// Shadow rel path is collectionID + "/a.txt"
	shadowRelPath := collID + "/a.txt"

	return ts, token, warmPoolID, coldPoolID, collID, tmpDir, "sdb", shadowRelPath
}

func TestBackupRunDirtyShadows(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, _, _, collID, tmpDir, _, shadowRelPath := setupDirtyShadowServer(t, db)
	defer ts.Close()

	// Create a backup repo
	repoPath := filepath.Join(t.TempDir(), "restic-repo-dirty")
	code, out := tagDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": "dirty-repo", "target_path": repoPath}, token)
	if code != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d: %v", code, out)
	}
	repoMap, _ := out["repo"].(map[string]any)
	repoID, _ := repoMap["id"].(string)

	// Run backup while cold is asleep and dirty shadows exist
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos/"+repoID+"/run", map[string]any{"collection_id": collID, "password": "pw-1234567890"}, token)
	if code != http.StatusOK {
		t.Fatalf("dirty shadow run: expected 200, got %d: %v", code, out)
	}

	// Verify response has source == "dirty_shadows" and non-empty snapshot_ids
	source, _ := out["source"].(string)
	if source != "dirty_shadows" {
		t.Errorf("source: got %q, want \"dirty_shadows\"", source)
	}
	snapIDs, ok := out["snapshot_ids"].([]any)
	if !ok || len(snapIDs) == 0 {
		t.Errorf("snapshot_ids: got %v, want non-empty array", out["snapshot_ids"])
	}

	// Verify snapshots are listed via GET /api/backups/repos/{repoID}/snapshots
	code, out = tagDoReq(t, ts, "GET", "/api/backups/repos/"+repoID+"/snapshots", nil, token)
	if code != http.StatusOK {
		t.Fatalf("list snapshots: expected 200, got %d: %v", code, out)
	}
	total, _ := out["total"].(float64)
	if total == 0 {
		t.Errorf("snapshots total: got %v, want >0", total)
	}

	// Verify the shadow file on warm pool has the dirty content
	shadowPath := filepath.Join(tmpDir, "mnt", "warm", ".kelvios-shadow", shadowRelPath)
	shadowData, err := os.ReadFile(shadowPath)
	if err != nil {
		t.Fatalf("read shadow file: %v", err)
	}
	if string(shadowData) != "dirty shadow content" {
		t.Errorf("shadow content: got %q, want \"dirty shadow content\"", string(shadowData))
	}

	// Verify warm pool mountpoint was used (shadow root exists)
	shadowRoot := filepath.Join(tmpDir, "mnt", "warm", ".kelvios-shadow", collID)
	if _, err := os.Stat(shadowRoot); err != nil {
		t.Errorf("shadow root missing: %v", err)
	}
}

func TestBackupRunColdAwakeCanonical(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, _, _, collID, _, coldDisk, _ := setupDirtyShadowServer(t, db)
	defer ts.Close()

	// Create a backup repo
	repoPath := filepath.Join(t.TempDir(), "restic-repo-canonical")
	code, out := tagDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": "canonical-repo", "target_path": repoPath}, token)
	if code != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d: %v", code, out)
	}
	repoMap, _ := out["repo"].(map[string]any)
	repoID, _ := repoMap["id"].(string)

	// Wake the cold disk before backup
	code, resp := tagDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/wake", nil, token)
	if code != http.StatusOK {
		t.Fatalf("wake cold disk failed: %v", resp)
	}

	// Run backup while cold is awake -> canonical backup (no dirty_shadows source)
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos/"+repoID+"/run", map[string]any{"collection_id": collID, "password": "pw-1234567890"}, token)
	if code != http.StatusOK {
		t.Fatalf("canonical run: expected 200, got %d: %v", code, out)
	}

	// Verify response has snapshot (canonical) and NO dirty_shadows source
	source, _ := out["source"].(string)
	if source == "dirty_shadows" {
		t.Errorf("source: got \"dirty_shadows\", want canonical (no source field or different)")
	}
	snapID, _ := out["snapshot_id"].(string)
	if snapID == "" {
		t.Errorf("snapshot_id: got empty, want non-empty")
	}

	// Verify snapshot recorded and listed
	code, out = tagDoReq(t, ts, "GET", "/api/backups/repos/"+repoID+"/snapshots", nil, token)
	if code != http.StatusOK {
		t.Fatalf("list snapshots: expected 200, got %d: %v", code, out)
	}
	total, _ := out["total"].(float64)
	if total == 0 {
		t.Errorf("snapshots total: got %v, want >0", total)
	}
}

func TestBackupRunUnauth(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, _, _, collID, _, _, _ := setupDirtyShadowServer(t, db)
	defer ts.Close()

	// Create a backup repo
	repoPath := filepath.Join(t.TempDir(), "restic-repo-unauth")
	code, out := tagDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": "unauth-repo", "target_path": repoPath}, token)
	if code != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d: %v", code, out)
	}
	repoMap, _ := out["repo"].(map[string]any)
	repoID, _ := repoMap["id"].(string)

	// Run backup without auth token
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos/"+repoID+"/run", map[string]any{"collection_id": collID, "password": "pw-1234567890"}, "")
	if code != http.StatusUnauthorized {
		t.Errorf("unauth run: expected 401, got %d: %v", code, out)
	}
}

func TestBackupRunDirtyShadowsAuditLog(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, _, _, collID, _, _, _ := setupDirtyShadowServer(t, db)
	defer ts.Close()

	// Create a backup repo
	repoPath := filepath.Join(t.TempDir(), "restic-repo-audit")
	code, out := tagDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": "audit-repo", "target_path": repoPath}, token)
	if code != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d: %v", code, out)
	}
	repoMap, _ := out["repo"].(map[string]any)
	repoID, _ := repoMap["id"].(string)

	// Run backup while cold is asleep and dirty shadows exist
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos/"+repoID+"/run", map[string]any{"collection_id": collID, "password": "pw-1234567890"}, token)
	if code != http.StatusOK {
		t.Fatalf("dirty shadow run: expected 200, got %d: %v", code, out)
	}

	// Query audit log for backup.run with detail mentioning dirty shadows
	code, out = tagDoReq(t, ts, "GET", "/api/audit-log?action=backup.run&result=ok", nil, token)
	if code != http.StatusOK {
		t.Fatalf("audit log: expected 200, got %d: %v", code, out)
	}
	entries, _ := out["entries"].([]any)
	found := false
	for _, e := range entries {
		entry, _ := e.(map[string]any)
		detail, _ := entry["detail"].(string)
		if detail != "" && (contains(detail, "dirty shadow") || contains(detail, "dirty_shadows")) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("audit log: no backup.run entry with dirty shadow detail found; entries: %v", entries)
	}
}

// contains is a simple substring check
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}