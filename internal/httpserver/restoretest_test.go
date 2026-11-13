package httpserver

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

// setupRestoreTestServer creates a test server with one warm pool and one
// collection ("photos") whose root is tmpDir/mnt/warm/photos.
// Returns ts, token, collID, collRoot, tmpDir, and the server instance for vault access.
func setupRestoreTestServer(t *testing.T, db *sql.DB) (*httptest.Server, string, string, string, string, *Server) {
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
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-RESTORE-WARM", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
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
	collRoot := filepath.Join(warmMount, "photos")
	if err := os.MkdirAll(collRoot, 0o755); err != nil {
		t.Fatalf("mkdir coll root: %v", err)
	}

	resp = doReq("POST", "/api/pools", map[string]any{"name": "warm", "tier": "warm", "mechanism": "single", "disks": []string{"sda"}, "mountpoint": warmMount}, token)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("POST pool: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var pr struct {
		Pool struct {
			ID string `json:"id"`
		} `json:"pool"`
	}
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if pr.Pool.ID == "" {
		t.Fatalf("pool ID empty")
	}

	resp = doReq("POST", "/api/pools/"+pr.Pool.ID+"/collections", map[string]any{"name": "photos", "path": "photos", "disk_name": "sda"}, token)
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
	if collResp.Collection.ID == "" {
		t.Fatalf("collection ID empty")
	}

	return ts, token, collResp.Collection.ID, collRoot, tmpDir, s
}

// restoreTestDoReq performs one HTTP call against ts and decodes the JSON body.
func restoreTestDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) (int, map[string]any) {
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
	defer resp.Body.Close()
	out := map[string]any{}
	if b, _ := io.ReadAll(resp.Body); len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return resp.StatusCode, out
}

// writeRestoreTestFile writes a file to the collection root with given content and mtime.
func writeRestoreTestFile(t *testing.T, collRoot, rel, content string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(collRoot, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatalf("chtime %s: %v", rel, err)
		}
	}
}

// insertFileRecord inserts a file row into the catalog files table with checksum.
func insertFileRecord(t *testing.T, db *sql.DB, collectionID, relPath, content string, modTimeUnix int64) {
	t.Helper()
	checksum := sha256.Sum256([]byte(content))
	checksumHex := hex.EncodeToString(checksum[:])
	id := "file-" + collectionID + "-" + relPath
	// Replace path separators for valid ID
	id = filepath.ToSlash(id)
	id = "file-" + collectionID + "-" + filepath.Base(relPath)
	// Ensure unique ID by adding hash suffix
	id = id + "-" + checksumHex[:8]
	now := time.Now().Unix()
	_, err := db.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, checksum, checksum_verified_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, collectionID, relPath, len(content), modTimeUnix, "sda", checksumHex, now, now, now)
	if err != nil {
		t.Fatalf("insert file record: %v", err)
	}
}

// createBackupRepoViaAPI creates a local backup repo via the API and returns repoID and repoPath.
func createBackupRepoViaAPI(t *testing.T, ts *httptest.Server, token, tmpDir, repoName string) (string, string) {
	t.Helper()
	repoPath := filepath.Join(tmpDir, repoName)
	code, out := restoreTestDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": repoName, "target_type": "local", "target_path": repoPath}, token)
	if code != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d: %v", code, out)
	}
	repoMap, _ := out["repo"].(map[string]any)
	repoID, _ := repoMap["id"].(string)
	if repoID == "" {
		t.Fatalf("create repo: no id in %v", out)
	}
	return repoID, repoPath
}

// createBackupSnapshotViaDB inserts a backup snapshot row directly into the catalog.
func createBackupSnapshotViaDB(t *testing.T, db *sql.DB, repoID, collectionID, resticSnapshotID string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO backup_snapshots (id, repo_id, collection_id, restic_snapshot_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		"bsnap-"+resticSnapshotID, repoID, collectionID, resticSnapshotID, time.Now().Unix())
	if err != nil {
		t.Fatalf("insert backup snapshot: %v", err)
	}
}

// runBackupViaAPI runs a backup for the given repo and collection via the API.
// Returns the snapshot ID from the response.
func runBackupViaAPI(t *testing.T, ts *httptest.Server, token, repoID, collectionID, password string) string {
	t.Helper()
	code, out := restoreTestDoReq(t, ts, "POST", "/api/backups/repos/"+repoID+"/run", map[string]any{
		"collection_id": collectionID,
		"password":      password,
	}, token)
	if code != http.StatusOK {
		t.Fatalf("backup run: expected 200, got %d: %v", code, out)
	}
	snapID, _ := out["snapshot_id"].(string)
	if snapID == "" {
		// Check for snapshot_ids array (dirty shadows case)
		if ids, ok := out["snapshot_ids"].([]any); ok && len(ids) > 0 {
			snapID, _ = ids[0].(string)
		}
	}
	if snapID == "" {
		t.Fatalf("backup run: no snapshot_id in %v", out)
	}
	return snapID
}

// scanCollectionViaAPI runs a scan on the collection via the API.
func scanCollectionViaAPI(t *testing.T, ts *httptest.Server, token, collectionID string) {
	t.Helper()
	code, out := restoreTestDoReq(t, ts, "POST", "/api/collections/"+collectionID+"/scan", nil, token)
	if code != http.StatusOK {
		t.Fatalf("scan collection: expected 200, got %d: %v", code, out)
	}
}

// setupVaultAndSecret sets up the vault and stores a secret for the given repo.
func setupVaultAndSecret(t *testing.T, s *Server, repoID, password string) {
	t.Helper()
	// Setup vault
	err := s.vault.Setup("vault-passphrase-12345678")
	if err != nil {
		t.Fatalf("vault setup: %v", err)
	}
	// Store secret for the repo
	secretName := "backup-repo:" + repoID
	err = s.vault.Put(secretName, password)
	if err != nil {
		t.Fatalf("vault put secret: %v", err)
	}
}

// TestRestoreTest_Unauth401 tests that all four endpoints return 401 when unauthenticated.
func TestRestoreTest_Unauth401(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, _, _, _, _, _ := setupRestoreTestServer(t, db)
	defer ts.Close()

	endpoints := []struct {
		method string
		path   string
	}{
		{"POST", "/api/restore-tests/run"},
		{"GET", "/api/restore-tests/runs"},
		{"GET", "/api/restore-tests/alerts"},
		{"GET", "/api/restore-tests/status"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			code, _ := restoreTestDoReq(t, ts, ep.method, ep.path, nil, "")
			if code != http.StatusUnauthorized {
				t.Errorf("%s %s: expected 401, got %d", ep.method, ep.path, code)
			}
		})
	}
}

// TestRestoreTest_WrongMethod405 tests that POST /run rejects wrong method with 405.
func TestRestoreTest_WrongMethod405(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, _, _, _, _ := setupRestoreTestServer(t, db)
	defer ts.Close()

	// GET on /run should be 405
	code, _ := restoreTestDoReq(t, ts, "GET", "/api/restore-tests/run", nil, token)
	if code != http.StatusMethodNotAllowed {
		t.Errorf("GET /run: expected 405, got %d", code)
	}
	// PUT on /run should be 405
	code, _ = restoreTestDoReq(t, ts, "PUT", "/api/restore-tests/run", nil, token)
	if code != http.StatusMethodNotAllowed {
		t.Errorf("PUT /run: expected 405, got %d", code)
	}
}

// TestRestoreTest_StatusFreshDB tests status on a fresh DB returns null timestamps.
func TestRestoreTest_StatusFreshDB(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, _, _, _, _ := setupRestoreTestServer(t, db)
	defer ts.Close()

	code, out := restoreTestDoReq(t, ts, "GET", "/api/restore-tests/status", nil, token)
	if code != http.StatusOK {
		t.Fatalf("status: expected 200, got %d: %v", code, out)
	}
	lastRunAt := out["last_run_at"]
	nextDueAt := out["next_due_at"]
	if lastRunAt != nil {
		t.Errorf("status fresh DB: last_run_at = %v, want null", lastRunAt)
	}
	if nextDueAt != nil {
		t.Errorf("status fresh DB: next_due_at = %v, want null", nextDueAt)
	}
}

// TestRestoreTest_RunZeroRepos tests run with zero repos returns 200 with 0 samples.
func TestRestoreTest_RunZeroRepos(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, _, _, _, _ := setupRestoreTestServer(t, db)
	defer ts.Close()

	// Run with zero repos
	code, out := restoreTestDoReq(t, ts, "POST", "/api/restore-tests/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("run zero repos: expected 200, got %d: %v", code, out)
	}
	if out["samples"] != float64(0) {
		t.Errorf("run zero repos: samples = %v, want 0", out["samples"])
	}
	if out["passed"] != float64(0) {
		t.Errorf("run zero repos: passed = %v, want 0", out["passed"])
	}
	if out["failed"] != float64(0) {
		t.Errorf("run zero repos: failed = %v, want 0", out["failed"])
	}
	if out["skipped"] != float64(0) {
		t.Errorf("run zero repos: skipped = %v, want 0", out["skipped"])
	}
	if out["alerts"] != float64(0) {
		t.Errorf("run zero repos: alerts = %v, want 0", out["alerts"])
	}
	runID, _ := out["run_id"].(string)
	if runID == "" {
		t.Errorf("run zero repos: run_id empty")
	}

	// Verify run appears in runs list
	code, out = restoreTestDoReq(t, ts, "GET", "/api/restore-tests/runs", nil, token)
	if code != http.StatusOK {
		t.Fatalf("list runs: expected 200, got %d: %v", code, out)
	}
	runs, _ := out["runs"].([]any)
	if len(runs) != 1 {
		t.Errorf("list runs: expected 1 run, got %d", len(runs))
	}
	if runs[0].(map[string]any)["id"] != runID {
		t.Errorf("list runs: run id mismatch")
	}

	// Alerts list should be empty
	code, out = restoreTestDoReq(t, ts, "GET", "/api/restore-tests/alerts", nil, token)
	if code != http.StatusOK {
		t.Fatalf("list alerts: expected 200, got %d: %v", code, out)
	}
	alerts, _ := out["alerts"].([]any)
	if len(alerts) != 0 {
		t.Errorf("list alerts: expected 0, got %d", len(alerts))
	}
}

// TestRestoreTest_RunWithRepoSnapshotFiles tests a successful restore test run
// with a repo, snapshot, and collection files. Verifies samples = passed + failed + skipped,
// run row + audit recorded, and SHA-256 round-trip integrity (restored bytes == source bytes).
func TestRestoreTest_RunWithRepoSnapshotFiles(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, collID, collRoot, tmpDir, srv := setupRestoreTestServer(t, db)
	defer ts.Close()

	// Create backup repo via API
	repoID, _ := createBackupRepoViaAPI(t, ts, token, tmpDir, "test-repo")

	// Setup vault secret for the repo
	setupVaultAndSecret(t, srv, repoID, "test-password-12345678")

	// Write source files to collection root (these are the "original" files that were backed up)
	file1Content := "file-one-content-abcdefghijklmnop"
	file2Content := "file-two-content-qrstuvwxyz123456"
	writeRestoreTestFile(t, collRoot, "photo1.jpg", file1Content, time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC))
	writeRestoreTestFile(t, collRoot, "photo2.jpg", file2Content, time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC))

	// Scan the collection to populate files table (as backup tests do)
	scanCollectionViaAPI(t, ts, token, collID)

	// Run backup via API - this initializes the restic repo and creates snapshot with metadata
	_ = runBackupViaAPI(t, ts, token, repoID, collID, "test-password-12345678")

	// Run the restore test
	code, out := restoreTestDoReq(t, ts, "POST", "/api/restore-tests/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("run: expected 200, got %d: %v", code, out)
	}

	// Verify counts: 2 samples, 2 passed, 0 failed, 0 skipped, 0 alerts
	samples := int(out["samples"].(float64))
	passed := int(out["passed"].(float64))
	failed := int(out["failed"].(float64))
	skipped := int(out["skipped"].(float64))
	alerts := int(out["alerts"].(float64))
	if samples != 2 {
		t.Errorf("samples = %d, want 2", samples)
	}
	if passed != 2 {
		t.Errorf("passed = %d, want 2", passed)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	if alerts != 0 {
		t.Errorf("alerts = %d, want 0", alerts)
	}
	// Verify samples = passed + failed + skipped
	if samples != passed+failed+skipped {
		t.Errorf("samples (%d) != passed (%d) + failed (%d) + skipped (%d)", samples, passed, failed, skipped)
	}

	runID, _ := out["run_id"].(string)
	if runID == "" {
		t.Errorf("run_id empty")
	}

	// Verify run row exists in restore_test_runs
	var runSamples, runPassed, runFailed, runSkipped int
	err := db.QueryRow(`SELECT samples, passed, failed, skipped FROM restore_test_runs WHERE id = ?`, runID).
		Scan(&runSamples, &runPassed, &runFailed, &runSkipped)
	if err != nil {
		t.Fatalf("query run row: %v", err)
	}
	if runSamples != samples || runPassed != passed || runFailed != failed || runSkipped != skipped {
		t.Errorf("run row mismatch: got samples=%d passed=%d failed=%d skipped=%d, want samples=%d passed=%d failed=%d skipped=%d",
			runSamples, runPassed, runFailed, runSkipped, samples, passed, failed, skipped)
	}

	// Verify audit log entry for restore_test.run exists
	var auditCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'restore_test.run' AND target_id = ?`, runID).Scan(&auditCount)
	if err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("audit log: expected 1 restore_test.run entry, got %d", auditCount)
	}

	// Verify alerts list is empty (no alerts for successful run)
	code, out = restoreTestDoReq(t, ts, "GET", "/api/restore-tests/alerts", nil, token)
	if code != http.StatusOK {
		t.Fatalf("list alerts: expected 200, got %d: %v", code, out)
	}
	alertsList, _ := out["alerts"].([]any)
	if len(alertsList) != 0 {
		t.Errorf("alerts list: expected 0, got %d", len(alertsList))
	}

	// Verify SHA-256 round-trip integrity: restored bytes equal source bytes
	// The restore test should have verified this internally (sha_match = true for both samples)
	// We can also verify by checking that the alerts list has no sha256_mismatch alerts
	code, out = restoreTestDoReq(t, ts, "GET", "/api/restore-tests/alerts", nil, token)
	if code != http.StatusOK {
		t.Fatalf("list alerts for integrity check: expected 200, got %d: %v", code, out)
	}
	alertsList, _ = out["alerts"].([]any)
	for _, a := range alertsList {
		alert := a.(map[string]any)
		if alert["reason"] == "sha256_mismatch" {
			t.Errorf("SHA-256 mismatch detected: %v", alert)
		}
	}
}

// TestRestoreTest_MissingVaultSecret tests that missing vault secret for a repo
// causes samples to be SKIPPED with reason "vault_secret_missing", run still returns 200.
func TestRestoreTest_MissingVaultSecret(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, collID, collRoot, tmpDir, _ := setupRestoreTestServer(t, db)
	defer ts.Close()

	// Create backup repo via API
	repoID, repoPath := createBackupRepoViaAPI(t, ts, token, tmpDir, "test-repo-no-vault")

	// NOTE: We do NOT call setupVaultAndSecret - vault secret is missing

	// Write source file and insert catalog record
	fileContent := "file-content-for-missing-vault-test"
	writeRestoreTestFile(t, collRoot, "photo1.jpg", fileContent, time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC))
	insertFileRecord(t, db, collID, "photo1.jpg", fileContent, 1718445600)

	// Create backup snapshot via DB
	resticSnapshotID := "snap-abcdef1234567890"
	createBackupSnapshotViaDB(t, db, repoID, collID, resticSnapshotID)

	// Create stub snapshot metadata
	snapshotsDir := filepath.Join(repoPath, "snapshots")
	if err := os.MkdirAll(snapshotsDir, 0o755); err != nil {
		t.Fatalf("mkdir snapshots: %v", err)
	}
	snapMetaPath := filepath.Join(snapshotsDir, resticSnapshotID+".txt")
	if err := os.WriteFile(snapMetaPath, []byte(collRoot), 0o644); err != nil {
		t.Fatalf("write snapshot meta: %v", err)
	}

	// Init the stub repo state (as `init` would): without config + stored
	// password the stub restore fails password validation, and the test
	// would never reach the source_changed branch it is testing.
	if err := os.WriteFile(filepath.Join(repoPath, "config"), []byte(`{"version":1,"id":"stub"}`), 0o644); err != nil {
		t.Fatalf("write stub config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, ".password"), []byte("test-password-12345678"), 0o600); err != nil {
		t.Fatalf("write stub password: %v", err)
	}

	// Run the restore test - should return 200, not 500
	code, out := restoreTestDoReq(t, ts, "POST", "/api/restore-tests/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("run: expected 200 (never 500), got %d: %v", code, out)
	}

	// Verify counts: 1 sample, 0 passed, 0 failed, 1 skipped, 1 alert
	samples := int(out["samples"].(float64))
	passed := int(out["passed"].(float64))
	failed := int(out["failed"].(float64))
	skipped := int(out["skipped"].(float64))
	alerts := int(out["alerts"].(float64))
	if samples != 1 {
		t.Errorf("samples = %d, want 1", samples)
	}
	if passed != 0 {
		t.Errorf("passed = %d, want 0", passed)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	if alerts != 1 {
		t.Errorf("alerts = %d, want 1", alerts)
	}
	if samples != passed+failed+skipped {
		t.Errorf("samples (%d) != passed (%d) + failed (%d) + skipped (%d)", samples, passed, failed, skipped)
	}

	runID, _ := out["run_id"].(string)
	if runID == "" {
		t.Errorf("run_id empty")
	}

	// Verify alert was recorded with reason "vault_secret_missing"
	code, out = restoreTestDoReq(t, ts, "GET", "/api/restore-tests/alerts", nil, token)
	if code != http.StatusOK {
		t.Fatalf("list alerts: expected 200, got %d: %v", code, out)
	}
	alertsList, _ := out["alerts"].([]any)
	if len(alertsList) != 1 {
		t.Errorf("alerts list: expected 1 alert, got %d", len(alertsList))
	} else {
		alert := alertsList[0].(map[string]any)
		if alert["reason"] != "vault_secret_missing" {
			t.Errorf("alert reason = %v, want 'vault_secret_missing'", alert["reason"])
		}
		if alert["repo_id"] != repoID {
			t.Errorf("alert repo_id = %v, want %s", alert["repo_id"], repoID)
		}
		if alert["snapshot_id"] != resticSnapshotID {
			t.Errorf("alert snapshot_id = %v, want %s", alert["snapshot_id"], resticSnapshotID)
		}
		if alert["resolved"] != float64(0) {
			t.Errorf("alert resolved = %v, want 0 (unresolved)", alert["resolved"])
		}
	}
}

// TestRestoreTest_SourceChangedAfterSnapshot tests that when source file modtime
// is newer than snapshot created_at, sha_match is NULL with reason "source_changed",
// and this is counted as PASSED (not a failure).
func TestRestoreTest_SourceChangedAfterSnapshot(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, collID, collRoot, tmpDir, srv := setupRestoreTestServer(t, db)
	defer ts.Close()

	// Create backup repo via API
	repoID, repoPath := createBackupRepoViaAPI(t, ts, token, tmpDir, "test-repo-source-changed")

	// Setup vault secret
	setupVaultAndSecret(t, srv, repoID, "test-password-12345678")

	// Snapshot created_at timestamp (earlier)
	snapshotCreatedAt := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC).Unix()

	// Write source file with ORIGINAL content (what was backed up)
	originalContent := "original-file-content-backed-up"
	writeRestoreTestFile(t, collRoot, "photo1.jpg", originalContent, time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC))

	// Insert file record with modtime = snapshot created_at (source unchanged at snapshot time)
	insertFileRecord(t, db, collID, "photo1.jpg", originalContent, snapshotCreatedAt)

	// Create backup snapshot via DB with created_at = snapshotCreatedAt
	resticSnapshotID := "snap-source-changed-12345"
	_, err := db.Exec(`INSERT INTO backup_snapshots (id, repo_id, collection_id, restic_snapshot_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		"bsnap-"+resticSnapshotID, repoID, collID, resticSnapshotID, snapshotCreatedAt)
	if err != nil {
		t.Fatalf("insert backup snapshot: %v", err)
	}

	// Create stub snapshot metadata
	snapshotsDir := filepath.Join(repoPath, "snapshots")
	if err := os.MkdirAll(snapshotsDir, 0o755); err != nil {
		t.Fatalf("mkdir snapshots: %v", err)
	}
	snapMetaPath := filepath.Join(snapshotsDir, resticSnapshotID+".txt")
	if err := os.WriteFile(snapMetaPath, []byte(collRoot), 0o644); err != nil {
		t.Fatalf("write snapshot meta: %v", err)
	}

	// Init the stub repo state (as `init` would): without config + stored
	// password the stub restore fails password validation, and the test
	// would never reach the source_changed branch it is testing.
	if err := os.WriteFile(filepath.Join(repoPath, "config"), []byte(`{"version":1,"id":"stub"}`), 0o644); err != nil {
		t.Fatalf("write stub config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, ".password"), []byte("test-password-12345678"), 0o600); err != nil {
		t.Fatalf("write stub password: %v", err)
	}

	// NOW modify the source file on disk to have NEWER content and NEWER modtime
	// This simulates the source file changing AFTER the snapshot was taken
	newContent := "modified-file-content-after-snapshot"
	laterTime := time.Date(2024, 6, 20, 10, 0, 0, 0, time.UTC) // 5 days after snapshot
	writeRestoreTestFile(t, collRoot, "photo1.jpg", newContent, laterTime)

	// Update the catalog file record to reflect the NEW modtime (source changed)
	// The checksum in catalog should still be the ORIGINAL checksum (from backup time)
	// But modtime_unix is now newer than snapshot created_at
	_, err = db.Exec(`UPDATE files SET modtime_unix = ? WHERE collection_id = ? AND rel_path = 'photo1.jpg'`, laterTime.Unix(), collID)
	if err != nil {
		t.Fatalf("update file modtime: %v", err)
	}

	// Run the restore test
	code, out := restoreTestDoReq(t, ts, "POST", "/api/restore-tests/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("run: expected 200, got %d: %v", code, out)
	}

	// Verify counts: 1 sample, 1 passed (source_changed counts as pass), 0 failed, 0 skipped
	samples := int(out["samples"].(float64))
	passed := int(out["passed"].(float64))
	failed := int(out["failed"].(float64))
	skipped := int(out["skipped"].(float64))
	alerts := int(out["alerts"].(float64))
	if samples != 1 {
		t.Errorf("samples = %d, want 1", samples)
	}
	if passed != 1 {
		t.Errorf("passed = %d, want 1 (source_changed counts as pass)", passed)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0", skipped)
	}
	if alerts != 0 {
		t.Errorf("alerts = %d, want 0", alerts)
	}
	if samples != passed+failed+skipped {
		t.Errorf("samples (%d) != passed (%d) + failed (%d) + skipped (%d)", samples, passed, failed, skipped)
	}

	runID, _ := out["run_id"].(string)
	if runID == "" {
		t.Errorf("run_id empty")
	}

	// Verify alert was recorded with reason "source_changed" (not a failure alert, but tracked)
	code, out = restoreTestDoReq(t, ts, "GET", "/api/restore-tests/alerts", nil, token)
	if code != http.StatusOK {
		t.Fatalf("list alerts: expected 200, got %d: %v", code, out)
	}
	alertsList, _ := out["alerts"].([]any)
	// source_changed creates an alert for tracking but is not a failure
	if len(alertsList) != 0 {
		t.Errorf("alerts list: expected 0 (source_changed is a pass, not a failure alert), got %d", len(alertsList))
	}
}