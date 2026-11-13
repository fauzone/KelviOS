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

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

// setupMigrationServer mirrors setupCuratorServer but provisions a warm source
// pool and a cold target pool with temp-dir mountpoints, plus one collection
// on the warm pool with a fixture tree. Returns ts, token, warmPoolID,
// coldPoolID, collID, tmpDir.
func setupMigrationServer(t *testing.T, db *sql.DB) (*httptest.Server, string, string, string, string, string) {
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
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-MIG-WARM", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
		{Name: "sdb", Path: "/dev/sdb", Model: "WDC WD40EFRX", Serial: "WD-MIG-COLD", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
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
	if err := os.MkdirAll(filepath.Join(warmMount, "photos"), 0o755); err != nil {
		t.Fatalf("mkdir warm tree: %v", err)
	}
	if err := os.MkdirAll(coldMount, 0o755); err != nil {
		t.Fatalf("mkdir cold mount: %v", err)
	}
	if err := os.WriteFile(filepath.Join(warmMount, "photos", "a.txt"), []byte("hello"), 0o644); err != nil {
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

	resp = doReq("POST", "/api/pools/"+warmPoolID+"/collections", map[string]any{"name": "photos", "path": "photos", "disk_name": "sda"}, token)
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

	return ts, token, warmPoolID, coldPoolID, collResp.Collection.ID, tmpDir
}

func migDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) *http.Response {
	t.Helper()
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

func migDecode(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func migExpect(t *testing.T, resp *http.Response, want int, what string) {
	t.Helper()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("%s: expected %d, got %d: %s", what, want, resp.StatusCode, string(b))
	}
}

// migGenerateApprove runs generate + approve for the default collection and
// returns the approved recommendation id.
func migGenerateApprove(t *testing.T, ts *httptest.Server, token, collID string) string {
	t.Helper()
	resp := migDoReq(t, ts, "POST", "/api/curator/recommendations/generate", nil, token)
	migExpect(t, resp, http.StatusOK, "POST generate")
	var gen struct {
		Created int `json:"created"`
	}
	migDecode(t, resp, &gen)
	if gen.Created != 1 {
		t.Fatalf("generate: got created=%d, want 1", gen.Created)
	}
	resp = migDoReq(t, ts, "GET", "/api/curator/recommendations?status=pending", nil, token)
	migExpect(t, resp, http.StatusOK, "GET pending")
	var list struct {
		Recommendations []map[string]any `json:"recommendations"`
		Total           int              `json:"total"`
	}
	migDecode(t, resp, &list)
	if list.Total != 1 || len(list.Recommendations) != 1 {
		t.Fatalf("pending list: got total=%d len=%d, want 1", list.Total, len(list.Recommendations))
	}
	recID, _ := list.Recommendations[0]["id"].(string)
	if recID == "" {
		t.Fatalf("pending recommendation has no id: %v", list.Recommendations[0])
	}
	resp = migDoReq(t, ts, "POST", "/api/curator/recommendations/"+recID+"/approve", nil, token)
	migExpect(t, resp, http.StatusOK, "POST approve")
	resp.Body.Close()
	return recID
}

// TestMigrationExecuteListUndoRoundtrip tests execute/list/undo via HTTP.
func TestMigrationExecuteListUndoRoundtrip(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _, collID, tmpDir := setupMigrationServer(t, db)
	defer ts.Close()

	now := time.Unix(1700000000, 0)
	lastAcc := now.Add(-30 * 24 * time.Hour).Unix()
	if _, err := db.Exec(`UPDATE collections SET last_accessed_at = ? WHERE id = ?`, lastAcc, collID); err != nil {
		t.Fatalf("update collection last_access: %v", err)
	}

	recID := migGenerateApprove(t, ts, token, collID)

	resp := migDoReq(t, ts, "POST", "/api/curator/migrations/execute/"+recID, nil, token)
	migExpect(t, resp, http.StatusOK, "POST execute")
	var migRow map[string]any
	migDecode(t, resp, &migRow)
	if migRow["status"] != "completed" {
		t.Errorf("execute status: got %v, want completed", migRow["status"])
	}
	migID, _ := migRow["id"].(string)
	if migID == "" {
		t.Fatalf("execute response has no id: %v", migRow)
	}

	// File landed on the cold pool with content, source retained.
	if b, err := os.ReadFile(filepath.Join(tmpDir, "mnt", "cold", "photos", "a.txt")); err != nil || string(b) != "hello" {
		t.Errorf("cold copy: err=%v content=%q, want hello", err, string(b))
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "mnt", "warm", "photos", "a.txt")); os.IsNotExist(err) {
		t.Error("source tree should be retained (warm a.txt still exists)")
	}

	// List shows one completed migration.
	resp = migDoReq(t, ts, "GET", "/api/curator/migrations?collection_id="+collID, nil, token)
	migExpect(t, resp, http.StatusOK, "GET migrations")
	var migList struct {
		Migrations []map[string]any `json:"migrations"`
		Total      int              `json:"total"`
	}
	migDecode(t, resp, &migList)
	if migList.Total != 1 || len(migList.Migrations) != 1 {
		t.Fatalf("list migrations: got total=%d len=%d, want 1", migList.Total, len(migList.Migrations))
	}
	if migList.Migrations[0]["status"] != "completed" {
		t.Errorf("list status: got %v, want completed", migList.Migrations[0]["status"])
	}

	// Undo: pointer back, new copy deleted.
	resp = migDoReq(t, ts, "POST", "/api/curator/migrations/"+migID+"/undo", nil, token)
	migExpect(t, resp, http.StatusOK, "POST undo")
	var undoRow map[string]any
	migDecode(t, resp, &undoRow)
	if undoRow["status"] != "undone" {
		t.Errorf("undo status: got %v, want undone", undoRow["status"])
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "mnt", "cold", "photos", "a.txt")); !os.IsNotExist(err) {
		t.Error("cold copy should be deleted after undo")
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "mnt", "warm", "photos", "a.txt")); os.IsNotExist(err) {
		t.Error("warm file should still exist after undo")
	}

	// Undo again -> 409.
	resp = migDoReq(t, ts, "POST", "/api/curator/migrations/"+migID+"/undo", nil, token)
	migExpect(t, resp, http.StatusConflict, "POST undo again")
	resp.Body.Close()
}

// TestMigrationProcessConsents tests the silent-consent endpoint via HTTP.
func TestMigrationProcessConsents(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, warmPoolID, _, collID, tmpDir := setupMigrationServer(t, db)
	defer ts.Close()

	// Opt the warm pool into silent consent (7-day wait).
	if _, err := db.Exec(`INSERT INTO tier_policies (pool_id, mode, cold_threshold_days, auto_move_wait_days) VALUES (?, 'auto_if_no_response', 21, 7)`, warmPoolID); err != nil {
		t.Fatalf("insert policy: %v", err)
	}
	// Seed an aged pending recommendation (8 days old).
	aged := time.Now().Add(-8 * 24 * time.Hour).Unix()
	if _, err := db.Exec(`INSERT INTO tier_recommendations (id, collection_id, pool_id, direction, reason, size_bytes, last_access, score, status, created_at, decided_at) VALUES (?, ?, ?, 'warm_to_cold', 'never_accessed', 0, 0, 0, 'pending', ?, 0)`, "rec-aged", collID, warmPoolID, aged); err != nil {
		t.Fatalf("seed aged recommendation: %v", err)
	}

	resp := migDoReq(t, ts, "POST", "/api/curator/migrations/process-consents", nil, token)
	migExpect(t, resp, http.StatusOK, "POST process-consents")
	var pc struct {
		Created  int `json:"created"`
		Executed int `json:"executed"`
		Failed   int `json:"failed"`
	}
	migDecode(t, resp, &pc)
	if pc.Created < 1 {
		t.Errorf("process-consents created: got %d, want >= 1", pc.Created)
	}
	if pc.Executed < 1 {
		t.Errorf("process-consents executed: got %d, want >= 1", pc.Executed)
	}
	if pc.Failed != 0 {
		t.Errorf("process-consents failed: got %d, want 0", pc.Failed)
	}

	// The silent-consent migration copied the file to the cold pool.
	if b, err := os.ReadFile(filepath.Join(tmpDir, "mnt", "cold", "photos", "a.txt")); err != nil || string(b) != "hello" {
		t.Errorf("cold copy after process-consents: err=%v content=%q, want hello", err, string(b))
	}
}

// TestMigrationErrorPaths tests 404/409/401 paths via HTTP.
func TestMigrationErrorPaths(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _, collID, _ := setupMigrationServer(t, db)
	defer ts.Close()

	now := time.Unix(1700000000, 0)
	lastAcc := now.Add(-30 * 24 * time.Hour).Unix()
	if _, err := db.Exec(`UPDATE collections SET last_accessed_at = ? WHERE id = ?`, lastAcc, collID); err != nil {
		t.Fatalf("update collection last_access: %v", err)
	}

	// Execute unknown recommendation -> 404.
	resp := migDoReq(t, ts, "POST", "/api/curator/migrations/execute/rec-nope", nil, token)
	migExpect(t, resp, http.StatusNotFound, "POST execute unknown")
	resp.Body.Close()

	// Generate a pending recommendation, execute WITHOUT approving -> 409.
	resp = migDoReq(t, ts, "POST", "/api/curator/recommendations/generate", nil, token)
	migExpect(t, resp, http.StatusOK, "POST generate")
	resp.Body.Close()
	resp = migDoReq(t, ts, "GET", "/api/curator/recommendations?status=pending", nil, token)
	migExpect(t, resp, http.StatusOK, "GET pending")
	var list struct {
		Recommendations []map[string]any `json:"recommendations"`
		Total           int              `json:"total"`
	}
	migDecode(t, resp, &list)
	if list.Total != 1 {
		t.Fatalf("pending list: got total=%d, want 1", list.Total)
	}
	pendingID, _ := list.Recommendations[0]["id"].(string)
	resp = migDoReq(t, ts, "POST", "/api/curator/migrations/execute/"+pendingID, nil, token)
	migExpect(t, resp, http.StatusConflict, "POST execute pending")
	resp.Body.Close()

	// Undo a merely-scheduled migration -> 409 (undo requires completed).
	if _, err := db.Exec(`INSERT INTO tier_migrations (id, collection_id, recommendation_id, from_pool_id, to_pool_id, direction, status, manifest, reason, created_at, executed_at, retain_until, finalized_at, undone_at) VALUES ('mig-sched', ?, '', '', '', 'warm_to_cold', 'scheduled', '', '', ?, 0, 0, 0, 0)`, collID, now.Unix()); err != nil {
		t.Fatalf("seed scheduled migration: %v", err)
	}
	resp = migDoReq(t, ts, "POST", "/api/curator/migrations/mig-sched/undo", nil, token)
	migExpect(t, resp, http.StatusConflict, "POST undo scheduled")
	resp.Body.Close()

	// List without token -> 401.
	resp = migDoReq(t, ts, "GET", "/api/curator/migrations", nil, "")
	migExpect(t, resp, http.StatusUnauthorized, "GET migrations unauth")
	resp.Body.Close()
}
