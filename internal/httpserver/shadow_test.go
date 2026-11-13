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

// setupShadowServer creates a test server with:
// - warm pool (target for shadows) with temp mountpoint
// - cold pool (source) with temp mountpoint
// - one collection on cold pool with a fixture file
// Returns ts, token, warmPoolID, coldPoolID, collID, tmpDir, coldDiskName
func setupShadowServer(t *testing.T, db *sql.DB) (*httptest.Server, string, string, string, string, string, string) {
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
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-SHADOW-WARM", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
		{Name: "sdb", Path: "/dev/sdb", Model: "WDC WD40EFRX", Serial: "WD-SHADOW-COLD", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
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

	// Scan the collection so the fixture file is indexed (download looks up the file row).
	resp = doReq("POST", "/api/collections/"+collResp.Collection.ID+"/scan", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST scan: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	return ts, token, warmPoolID, coldPoolID, collResp.Collection.ID, tmpDir, "sdb"
}

func shadowDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) *http.Response {
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

func shadowExpect(t *testing.T, resp *http.Response, want int, what string) {
	t.Helper()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("%s: expected %d, got %d: %s", what, want, resp.StatusCode, string(b))
	}
}

func shadowDecode(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// TestShadowCreationChainViaHTTP tests the full shadow creation and serve flow:
// 1. Wake cold disk
// 2. Download file twice (1st access -> no shadow; 2nd access within window -> shadow created)
// 3. Sleep cold disk
// 4. Download again -> 200 with shadow content (serve from shadow, no wake)
func TestShadowCreationChainViaHTTP(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _, collID, tmpDir, coldDisk := setupShadowServer(t, db)
	defer ts.Close()

	// 1. Wake the cold disk via power API
	resp := shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/wake", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST wake cold disk")
	resp.Body.Close()

	// 2. First download (1st access -> no shadow yet)
	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET content 1st access")
	// Read body directly since it's file content, not JSON
	body1, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body1) != "hello from cold" {
		t.Errorf("1st download content: got %q, want %q", string(body1), "hello from cold")
	}

	// Verify no shadow row exists yet (1st access)
	var shadowCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM file_shadows WHERE collection_id = ?`, collID).Scan(&shadowCount)
	if err != nil {
		t.Fatalf("count shadows after 1st access: %v", err)
	}
	if shadowCount != 0 {
		t.Errorf("shadow count after 1st access: got %d, want 0", shadowCount)
	}

	// 3. Second download (2nd access within 14 days -> shadow should be created post-serve)
	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET content 2nd access")
	body2, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body2) != "hello from cold" {
		t.Errorf("2nd download content: got %q, want %q", string(body2), "hello from cold")
	}

	// Verify shadow row now exists
	err = db.QueryRow(`SELECT COUNT(*) FROM file_shadows WHERE collection_id = ?`, collID).Scan(&shadowCount)
	if err != nil {
		t.Fatalf("count shadows after 2nd access: %v", err)
	}
	if shadowCount != 1 {
		t.Errorf("shadow count after 2nd access: got %d, want 1", shadowCount)
	}

	// Verify shadow file exists on warm pool
	var shadowRelPath string
	err = db.QueryRow(`SELECT shadow_rel_path FROM file_shadows WHERE collection_id = ?`, collID).Scan(&shadowRelPath)
	if err != nil {
		t.Fatalf("query shadow_rel_path: %v", err)
	}
	shadowPath := filepath.Join(tmpDir, "mnt", "warm", ".kelvios-shadow", shadowRelPath)
	if _, err := os.Stat(shadowPath); os.IsNotExist(err) {
		t.Errorf("shadow file not found at %s", shadowPath)
	}
	shadowContent, err := os.ReadFile(shadowPath)
	if err != nil {
		t.Fatalf("read shadow file: %v", err)
	}
	if string(shadowContent) != "hello from cold" {
		t.Errorf("shadow content: got %q, want %q", string(shadowContent), "hello from cold")
	}

	// 4. Sleep the cold disk
	resp = shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/sleep", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST sleep cold disk")
	resp.Body.Close()

	// 5. Download again with cold asleep -> should serve from shadow (200, no wake)
	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET content cold asleep (shadow serve)")
	body3, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body3) != "hello from cold" {
		t.Errorf("shadow serve content: got %q, want %q", string(body3), "hello from cold")
	}

	// Verify cold disk is still asleep (power state should be off/standby)
	resp = shadowDoReq(t, ts, "GET", "/api/power/"+coldDisk, nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET power state cold disk")
	var powerResp struct {
		Disk struct {
			PowerState string `json:"power_state"`
		} `json:"disk"`
	}
	shadowDecode(t, resp, &powerResp)
	if powerResp.Disk.PowerState == "active" {
		t.Errorf("cold disk should not be active after sleep, got %s", powerResp.Disk.PowerState)
	}
}

// TestShadowCanonicalPreferredWhenColdAwake tests that when cold is awake,
// the canonical file is served even if a shadow exists.
func TestShadowCanonicalPreferredWhenColdAwake(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _, collID, tmpDir, coldDisk := setupShadowServer(t, db)
	defer ts.Close()

	// Wake cold disk
	resp := shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/wake", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST wake cold disk")
	resp.Body.Close()

	// First download
	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET content 1st")
	resp.Body.Close()

	// Second download -> shadow created
	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET content 2nd")
	resp.Body.Close()

	// Modify canonical file on cold (simulate upload overwrite)
	canonicalPath := filepath.Join(tmpDir, "mnt", "cold", "photos", "a.txt")
	if err := os.WriteFile(canonicalPath, []byte("updated canonical"), 0o644); err != nil {
		t.Fatalf("write updated canonical: %v", err)
	}

	// Cold is still awake -> should serve canonical (updated) content
	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET content canonical preferred")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "updated canonical" {
		t.Errorf("canonical preferred: got %q, want %q", string(body), "updated canonical")
	}
}

// TestShadowOrphanRowCleanup tests that if a shadow row exists but the shadow
// file is missing on disk, the row is cleaned up and download falls through to 409.
func TestShadowOrphanRowCleanup(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _, collID, tmpDir, coldDisk := setupShadowServer(t, db)
	defer ts.Close()

	// Seed a shadow row manually (simulate orphan: row exists, file missing)
	now := time.Now().Unix()
	// Get warm pool ID from DB
	var warmPoolID string
	err = db.QueryRow(`SELECT id FROM pools WHERE tier = 'warm' LIMIT 1`).Scan(&warmPoolID)
	if err != nil {
		t.Fatalf("get warm pool id: %v", err)
	}
	_, err = db.Exec(`
		INSERT INTO file_shadows (id, file_id, collection_id, rel_path, shadow_pool_id, shadow_rel_path, sha256, size_bytes, created_at, last_served_at)
		VALUES ('shdw-orphan', 'file-1', ?, 'a.txt', ?, 'coll-1/a.txt', 'deadbeef', 15, ?, 0)
	`, collID, warmPoolID, now)
	if err != nil {
		t.Fatalf("seed orphan shadow row: %v", err)
	}

	// Ensure shadow file does NOT exist on warm pool
	shadowPath := filepath.Join(tmpDir, "mnt", "warm", ".kelvios-shadow", "coll-1", "a.txt")
	if _, err := os.Stat(shadowPath); err == nil {
		os.Remove(shadowPath)
	}

	// Sleep cold disk
	resp := shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/sleep", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST sleep cold disk")
	resp.Body.Close()

	// Download -> should fall through to 409 (cold offline, shadow file missing)
	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("orphan shadow: expected 409, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Orphan row should be cleaned up
	var shadowCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM file_shadows WHERE collection_id = ?`, collID).Scan(&shadowCount)
	if err != nil {
		t.Fatalf("count shadows after orphan cleanup: %v", err)
	}
	if shadowCount != 0 {
		t.Errorf("orphan shadow row not cleaned up: count=%d", shadowCount)
	}
}

// TestShadowInvalidationViaHTTP tests shadow invalidation on upload overwrite and delete.
func TestShadowInvalidationViaHTTP(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _, collID, _, coldDisk := setupShadowServer(t, db)
	defer ts.Close()

	// Wake cold, create shadow via two downloads
	resp := shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/wake", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST wake")
	resp.Body.Close()

	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET 1st")
	resp.Body.Close()

	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET 2nd")
	resp.Body.Close()

	// Verify shadow exists
	var shadowCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM file_shadows WHERE collection_id = ?`, collID).Scan(&shadowCount)
	if err != nil {
		t.Fatalf("count shadows: %v", err)
	}
	if shadowCount != 1 {
		t.Errorf("expected 1 shadow before invalidation, got %d", shadowCount)
	}

	// Upload overwrite (POST /api/collections/{id}/files with same path)
	// This should invalidate the shadow
	uploadBody := bytes.NewReader([]byte("overwritten content"))
	req, _ := http.NewRequest("POST", ts.URL+"/api/collections/"+collID+"/files/content?path=a.txt&overwrite=true", uploadBody)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST upload overwrite: %v", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("upload overwrite: expected 201/200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Shadow should be invalidated
	err = db.QueryRow(`SELECT COUNT(*) FROM file_shadows WHERE collection_id = ?`, collID).Scan(&shadowCount)
	if err != nil {
		t.Fatalf("count shadows after upload: %v", err)
	}
	if shadowCount != 0 {
		t.Errorf("shadow not invalidated after upload overwrite: count=%d", shadowCount)
	}

	// Re-create shadow
	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET after upload")
	resp.Body.Close()
	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET 2nd after upload")
	resp.Body.Close()

	err = db.QueryRow(`SELECT COUNT(*) FROM file_shadows WHERE collection_id = ?`, collID).Scan(&shadowCount)
	if err != nil {
		t.Fatalf("count shadows after re-create: %v", err)
	}
	if shadowCount != 1 {
		t.Errorf("expected 1 shadow after re-create, got %d", shadowCount)
	}

	// Delete file via DELETE /api/collections/{id}/files/content?path=a.txt
	req, _ = http.NewRequest("DELETE", ts.URL+"/api/collections/"+collID+"/files/content?path=a.txt", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE file: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("DELETE file: expected 204/200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Shadow should be invalidated
	err = db.QueryRow(`SELECT COUNT(*) FROM file_shadows WHERE collection_id = ?`, collID).Scan(&shadowCount)
	if err != nil {
		t.Fatalf("count shadows after delete: %v", err)
	}
	if shadowCount != 0 {
		t.Errorf("shadow not invalidated after delete: count=%d", shadowCount)
	}
}

// TestShadowErrorPaths tests error paths: unknown file 404, list/browse without token 401.
func TestShadowErrorPaths(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _, collID, _, coldDisk := setupShadowServer(t, db)
	defer ts.Close()

	// Wake cold for these tests
	resp := shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/wake", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST wake")
	resp.Body.Close()

	// Download unknown file -> 404
	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=missing.txt", nil, token)
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("unknown file: expected 404, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Browse without token -> 401
	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files", nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("browse unauth: expected 401, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// List shadows (no such endpoint, but download without token -> 401)
	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("download unauth: expected 401, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()
}

// ============================================================================
// WS36 (FR-2.6): Dirty write-back shadow HTTP tests
// ============================================================================

// TestDirtyUploadWhileColdAsleep tests that uploading to a Cold collection
// while asleep with an existing shadow writes to the shadow (dirty write-back),
// returns 200 with dirty: true, and leaves canonical untouched.
func TestDirtyUploadWhileColdAsleep(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _, collID, tmpDir, coldDisk := setupShadowServer(t, db)
	defer ts.Close()

	// Create shadow via two downloads (cold awake)
	resp := shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/wake", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST wake")
	resp.Body.Close()

	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET 1st")
	resp.Body.Close()

	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET 2nd")
	resp.Body.Close()

	// Verify shadow exists
	var shadowCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM file_shadows WHERE collection_id = ?`, collID).Scan(&shadowCount)
	if err != nil {
		t.Fatalf("count shadows: %v", err)
	}
	if shadowCount != 1 {
		t.Fatalf("expected 1 shadow before dirty upload, got %d", shadowCount)
	}

	// Sleep cold disk
	resp = shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/sleep", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST sleep cold disk")
	resp.Body.Close()

	// Upload new content while cold asleep (dirty write-back to shadow)
	uploadBody := bytes.NewReader([]byte("dirty upload content"))
	req, _ := http.NewRequest("POST", ts.URL+"/api/collections/"+collID+"/files/content?path=a.txt&overwrite=true", uploadBody)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST dirty upload: %v", err)
	}
	shadowExpect(t, resp, http.StatusOK, "POST dirty upload")
	var uploadResp struct {
		Status  string `json:"status"`
		Path    string `json:"path"`
		Shadowed string `json:"shadowed"`
		Dirty   string `json:"dirty"`
	}
	shadowDecode(t, resp, &uploadResp)
	if uploadResp.Dirty != "true" {
		t.Errorf("upload response dirty: got %q, want true", uploadResp.Dirty)
	}
	resp.Body.Close()

	// Verify shadow file on warm pool has new content
	var shadowRelPath string
	err = db.QueryRow(`SELECT shadow_rel_path FROM file_shadows WHERE collection_id = ?`, collID).Scan(&shadowRelPath)
	if err != nil {
		t.Fatalf("query shadow_rel_path: %v", err)
	}
	shadowPath := filepath.Join(tmpDir, "mnt", "warm", ".kelvios-shadow", shadowRelPath)
	shadowData, err := os.ReadFile(shadowPath)
	if err != nil {
		t.Fatalf("read shadow file: %v", err)
	}
	if string(shadowData) != "dirty upload content" {
		t.Errorf("shadow file content: got %q, want dirty upload content", string(shadowData))
	}

	// Verify canonical file on cold is UNCHANGED
	canonicalPath := filepath.Join(tmpDir, "mnt", "cold", "photos", "a.txt")
	canonicalData, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("read canonical file: %v", err)
	}
	if string(canonicalData) != "hello from cold" {
		t.Errorf("canonical file changed: got %q, want hello from cold", string(canonicalData))
	}

	// Verify shadow row is dirty
	var dirty int
	err = db.QueryRow(`SELECT dirty FROM file_shadows WHERE collection_id = ?`, collID).Scan(&dirty)
	if err != nil {
		t.Fatalf("query dirty: %v", err)
	}
	if dirty != 1 {
		t.Errorf("shadow dirty: got %d, want 1", dirty)
	}

	// Browse should show dirty: true
	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET browse")
	var browseResp struct {
		Entries []map[string]any `json:"entries"`
		Tier    string           `json:"tier"`
	}
	shadowDecode(t, resp, &browseResp)
	foundDirty := false
	for _, entry := range browseResp.Entries {
		if entry["rel_path"] == "a.txt" {
			if entry["dirty"] == true {
				foundDirty = true
			}
			break
		}
	}
	if !foundDirty {
		t.Error("browse entry for a.txt should have dirty=true")
	}
}

// TestDownloadDirtyShadowWhileColdAwake tests that downloading a dirty file
// while Cold is awake serves the SHADOW content (newest wins).
func TestDownloadDirtyShadowWhileColdAwake(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _, collID, _, coldDisk := setupShadowServer(t, db)
	defer ts.Close()

	// Create shadow via two downloads (cold awake)
	resp := shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/wake", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST wake")
	resp.Body.Close()

	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET 1st")
	resp.Body.Close()

	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET 2nd")
	resp.Body.Close()

	// Sleep cold disk
	resp = shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/sleep", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST sleep cold disk")
	resp.Body.Close()

	// Upload dirty content while cold asleep
	uploadBody := bytes.NewReader([]byte("shadow newest content"))
	req, _ := http.NewRequest("POST", ts.URL+"/api/collections/"+collID+"/files/content?path=a.txt&overwrite=true", uploadBody)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST dirty upload: %v", err)
	}
	shadowExpect(t, resp, http.StatusOK, "POST dirty upload")
	resp.Body.Close()

	// Wake cold disk
	resp = shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/wake", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST wake cold disk")
	resp.Body.Close()

	// Download while cold awake -> should serve SHADOW content (newest wins)
	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET content dirty shadow")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "shadow newest content" {
		t.Errorf("download dirty shadow: got %q, want shadow newest content", string(body))
	}
}

// TestWakeFlushDirtyShadow tests that waking a cold pool flushes dirty shadows:
// canonical updated to shadow content, dirty cleared.
func TestWakeFlushDirtyShadow(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _, collID, tmpDir, coldDisk := setupShadowServer(t, db)
	defer ts.Close()

	// Create shadow via two downloads (cold awake)
	resp := shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/wake", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST wake")
	resp.Body.Close()

	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET 1st")
	resp.Body.Close()

	resp = shadowDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	shadowExpect(t, resp, http.StatusOK, "GET 2nd")
	resp.Body.Close()

	// Sleep cold disk
	resp = shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/sleep", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST sleep cold disk")
	resp.Body.Close()

	// Upload dirty content while cold asleep
	uploadBody := bytes.NewReader([]byte("flush me to canonical"))
	req, _ := http.NewRequest("POST", ts.URL+"/api/collections/"+collID+"/files/content?path=a.txt&overwrite=true", uploadBody)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST dirty upload: %v", err)
	}
	shadowExpect(t, resp, http.StatusOK, "POST dirty upload")
	resp.Body.Close()

	// Wake cold disk -> should trigger flush
	resp = shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/wake", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST wake cold disk")
	resp.Body.Close()

	// Verify canonical file now has shadow content
	canonicalPath := filepath.Join(tmpDir, "mnt", "cold", "photos", "a.txt")
	canonicalData, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("read canonical after flush: %v", err)
	}
	if string(canonicalData) != "flush me to canonical" {
		t.Errorf("canonical after flush: got %q, want flush me to canonical", string(canonicalData))
	}

	// Verify shadow row is clean
	var dirty int
	err = db.QueryRow(`SELECT dirty FROM file_shadows WHERE collection_id = ?`, collID).Scan(&dirty)
	if err != nil {
		t.Fatalf("query dirty: %v", err)
	}
	if dirty != 0 {
		t.Errorf("shadow dirty after flush: got %d, want 0", dirty)
	}
}

// TestUploadColdNoShadowWakeRequired tests that uploading while cold asleep
// with NO shadow returns 409 with wake_required: true.
func TestUploadColdNoShadowWakeRequired(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _, collID, _, coldDisk := setupShadowServer(t, db)
	defer ts.Close()

	// Sleep cold disk (no shadow exists yet)
	resp := shadowDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/sleep", nil, token)
	shadowExpect(t, resp, http.StatusOK, "POST sleep cold disk")
	resp.Body.Close()

	// Upload while cold asleep with NO shadow -> 409 wake_required
	uploadBody := bytes.NewReader([]byte("no shadow content"))
	req, _ := http.NewRequest("POST", ts.URL+"/api/collections/"+collID+"/files/content?path=a.txt&overwrite=true", uploadBody)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST upload no shadow: %v", err)
	}
	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("upload no shadow: expected 409, got %d: %s", resp.StatusCode, string(b))
	}
	var errResp struct {
		Error        string `json:"error"`
		WakeRequired bool   `json:"wake_required"`
	}
	shadowDecode(t, resp, &errResp)
	if !errResp.WakeRequired {
		t.Error("wake_required should be true")
	}
	resp.Body.Close()
}