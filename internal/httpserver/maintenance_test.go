package httpserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
	_ "modernc.org/sqlite"
)

// setupMaintenanceServer creates a test server with:
//   - one warm pool + "photos" collection (several files)
//   - one cold pool + "archive" collection (for never-wake rule)
//   - teststub binary built at test time, KELVIOS_SMARTCTL_BIN pointed at it
// Returns ts, token, warmCollID, warmCollRoot, coldCollID, coldCollRoot, teststubPath.
func setupMaintenanceServer(t *testing.T, db *sql.DB) (*httptest.Server, string, string, string, string, string, string) {
	t.Helper()

	// Build teststub binary at test time (GOROOT-derived go binary, no hardcoded paths)
	teststubDir := t.TempDir()
	teststubPath := filepath.Join(teststubDir, "teststub.exe")
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		goBin += ".exe"
	}
	cmd := exec.Command(goBin, "build", "-o", teststubPath, "github.com/fauzone/kelvios/internal/wakecheck/teststub")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build teststub: %v: %s", err, string(out))
	}

	// Set env vars for the stub binaries
	os.Setenv("KELVIOS_SMARTCTL_BIN", teststubPath)
	os.Setenv("KELVIOS_SNAPRAID_BIN", teststubPath)
	os.Setenv("KELVIOS_SNAPRAID_CONF", "")
	t.Cleanup(func() {
		os.Unsetenv("KELVIOS_SMARTCTL_BIN")
		os.Unsetenv("KELVIOS_SNAPRAID_BIN")
		os.Unsetenv("KELVIOS_SNAPRAID_CONF")
	})

	// Create temp directory for pool mountpoints and collection roots
	tmpDir := t.TempDir()
	warmMount := filepath.Join(tmpDir, "mnt", "warm")
	coldMount := filepath.Join(tmpDir, "mnt", "cold")
	warmCollRoot := filepath.Join(warmMount, "photos")
	coldCollRoot := filepath.Join(coldMount, "archive")
	if err := os.MkdirAll(warmCollRoot, 0o755); err != nil {
		t.Fatalf("mkdir warm coll root: %v", err)
	}
	if err := os.MkdirAll(coldCollRoot, 0o755); err != nil {
		t.Fatalf("mkdir cold coll root: %v", err)
	}

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
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-MAINT-WARM", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
		{Name: "sdb", Path: "/dev/sdb", Model: "WDC WD40EFRX", Serial: "WD-MAINT-COLD", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	s := New(cfg, db, "test-version")
	s.SetDriver(storage.NewSingleDriver(disks))
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	ts := httptest.NewServer(s.srv.Handler)

	// Create test user and session for authentication
	token := "test-token-maintenance"
	tokenHash := sha256Hex(token)
	now := time.Now().Unix()
	_, err := db.Exec(`INSERT INTO accounts (id, username, password_hash, failed_count, locked_until, created_at) VALUES (?, ?, ?, 0, 0, ?)`,
		"acct-maintenance", "maintenanceuser", "dummy-hash", now)
	if err != nil {
		t.Fatalf("create test account: %v", err)
	}
	_, err = db.Exec(`INSERT INTO sessions (token_hash, username, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, "maintenanceuser", now, now+3600)
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}

	// Create warm pool via API
	code, resp := tagDoReq(t, ts, "POST", "/api/pools", map[string]any{
		"name":        "warm",
		"tier":        "warm",
		"mechanism":   "single",
		"disks":       []string{"sda"},
		"mountpoint":  warmMount,
	}, token)
	if code != http.StatusCreated {
		t.Fatalf("POST warm pool: expected 201, got %d: %v", code, resp)
	}
	var warmPoolID string
	err = db.QueryRow(`SELECT id FROM pools WHERE name = ?`, "warm").Scan(&warmPoolID)
	if err != nil {
		t.Fatalf("get warm pool id: %v", err)
	}

	// Create cold pool via API
	code, resp = tagDoReq(t, ts, "POST", "/api/pools", map[string]any{
		"name":        "cold",
		"tier":        "cold",
		"mechanism":   "single",
		"disks":       []string{"sdb"},
		"mountpoint":  coldMount,
	}, token)
	if code != http.StatusCreated {
		t.Fatalf("POST cold pool: expected 201, got %d: %v", code, resp)
	}
	var coldPoolID string
	err = db.QueryRow(`SELECT id FROM pools WHERE name = ?`, "cold").Scan(&coldPoolID)
	if err != nil {
		t.Fatalf("get cold pool id: %v", err)
	}

	// Create warm collection
	code, resp = tagDoReq(t, ts, "POST", "/api/pools/"+warmPoolID+"/collections", map[string]any{
		"name":       "photos",
		"path":       "photos",
		"disk_name":  "sda",
	}, token)
	if code != http.StatusCreated {
		t.Fatalf("POST warm collection: expected 201, got %d: %v", code, resp)
	}
	var warmCollID string
	err = db.QueryRow(`SELECT id FROM collections WHERE name = ? AND pool_id = ?`, "photos", warmPoolID).Scan(&warmCollID)
	if err != nil {
		t.Fatalf("get warm collection id: %v", err)
	}

	// Create cold collection
	code, resp = tagDoReq(t, ts, "POST", "/api/pools/"+coldPoolID+"/collections", map[string]any{
		"name":       "archive",
		"path":       "archive",
		"disk_name":  "sdb",
	}, token)
	if code != http.StatusCreated {
		t.Fatalf("POST cold collection: expected 201, got %d: %v", code, resp)
	}
	var coldCollID string
	err = db.QueryRow(`SELECT id FROM collections WHERE name = ? AND pool_id = ?`, "archive", coldPoolID).Scan(&coldCollID)
	if err != nil {
		t.Fatalf("get cold collection id: %v", err)
	}

	return ts, token, warmCollID, warmCollRoot, coldCollID, coldCollRoot, teststubPath
}

func writeMaintenanceFile(t *testing.T, collRoot, rel, content string) {
	t.Helper()
	p := filepath.Join(collRoot, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// maintenanceDoReq performs one HTTP call against ts and decodes the JSON body.
func maintenanceDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) (int, map[string]any) {
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

// TestMaintenanceRunEndpoint tests Case 1: POST /api/maintenance/run → 200 with summary
// containing smart results, scrub results, and skipped collections fields.
func TestMaintenanceRunEndpoint(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ts, token, warmCollID, warmCollRoot, coldCollID, coldCollRoot, _ := setupMaintenanceServer(t, db)
	defer ts.Close()

	// Write several files in the warm collection
	writeMaintenanceFile(t, warmCollRoot, "a.jpg", "content-a")
	writeMaintenanceFile(t, warmCollRoot, "b.jpg", "content-b")
	writeMaintenanceFile(t, warmCollRoot, "c.jpg", "content-c")
	writeMaintenanceFile(t, warmCollRoot, "d.jpg", "content-d")
	writeMaintenanceFile(t, warmCollRoot, "e.jpg", "content-e")

	// Write a file in the cold collection too
	writeMaintenanceFile(t, coldCollRoot, "x.bin", "cold-content")

	// Scan the warm collection to index files
	code, out := maintenanceDoReq(t, ts, "POST", "/api/collections/"+warmCollID+"/scan", nil, token)
	if code != http.StatusOK {
		t.Fatalf("scan warm: expected 200, got %d: %v", code, out)
	}
	if out["added"] != float64(5) {
		t.Fatalf("scan warm added: got %v, want 5", out["added"])
	}

	// Scan the cold collection
	code, out = maintenanceDoReq(t, ts, "POST", "/api/collections/"+coldCollID+"/scan", nil, token)
	if code != http.StatusOK {
		t.Fatalf("scan cold: expected 200, got %d: %v", code, out)
	}
	if out["added"] != float64(1) {
		t.Fatalf("scan cold added: got %v, want 1", out["added"])
	}

	// POST /api/maintenance/run → 200
	code, out = maintenanceDoReq(t, ts, "POST", "/api/maintenance/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("POST maintenance/run: expected 200, got %d: %v", code, out)
	}

	// Verify summary fields exist
	smartResults, ok := out["smart_results"].([]any)
	if !ok {
		t.Fatalf("response missing smart_results: %v", out)
	}
	scrubResults, ok := out["scrub_results"].([]any)
	if !ok {
		t.Fatalf("response missing scrub_results: %v", out)
	}
	skippedCollections, ok := out["skipped_collections"].([]any)
	if !ok {
		t.Fatalf("response missing skipped_collections: %v", out)
	}

	// Should have at least one smart result (disk sda)
	if len(smartResults) == 0 {
		t.Errorf("expected at least 1 smart result, got 0")
	}

	// Should have scrub results for warm collection
	if len(scrubResults) == 0 {
		t.Errorf("expected at least 1 scrub result, got 0")
	}

	// Cold collection should be skipped
	if len(skippedCollections) == 0 {
		t.Errorf("expected at least 1 skipped collection (cold pool), got 0")
	} else {
		// Verify the cold collection is in skipped
		foundCold := false
		for _, sc := range skippedCollections {
			m, _ := sc.(map[string]any)
			if m != nil && m["collection_id"] == coldCollID {
				foundCold = true
				if m["reason"] != "cold pool: never wake for scrub" {
					t.Errorf("cold skip reason: got %q, want 'cold pool: never wake for scrub'", m["reason"])
				}
			}
		}
		if !foundCold {
			t.Errorf("cold collection %s not found in skipped_collections: %v", coldCollID, skippedCollections)
		}
	}

	// Verify started_at and finished_at
	if _, ok := out["started_at"].(float64); !ok {
		t.Errorf("response missing started_at: %v", out)
	}
	if _, ok := out["finished_at"].(float64); !ok {
		t.Errorf("response missing finished_at: %v", out)
	}
}

// TestMaintenanceScrubRotation tests Case 2: scrub rotation + coverage.
// Run once → cursor advanced, checksums baselined. Run again → verified fraction grown.
func TestMaintenanceScrubRotation(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ts, token, warmCollID, warmCollRoot, _, _, _ := setupMaintenanceServer(t, db)
	defer ts.Close()

	// Write 6 files in the warm collection
	writeMaintenanceFile(t, warmCollRoot, "a.jpg", "content-a")
	writeMaintenanceFile(t, warmCollRoot, "b.jpg", "content-b")
	writeMaintenanceFile(t, warmCollRoot, "c.jpg", "content-c")
	writeMaintenanceFile(t, warmCollRoot, "d.jpg", "content-d")
	writeMaintenanceFile(t, warmCollRoot, "e.jpg", "content-e")
	writeMaintenanceFile(t, warmCollRoot, "f.jpg", "content-f")

	// Scan to index files
	code, out := maintenanceDoReq(t, ts, "POST", "/api/collections/"+warmCollID+"/scan", nil, token)
	if code != http.StatusOK {
		t.Fatalf("scan warm: expected 200, got %d: %v", code, out)
	}
	if out["added"] != float64(6) {
		t.Fatalf("scan warm added: got %v, want 6", out["added"])
	}

	// First run: should baseline checksums (all files have empty checksum)
	code, out = maintenanceDoReq(t, ts, "POST", "/api/maintenance/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("POST maintenance/run (1st): expected 200, got %d: %v", code, out)
	}

	scrubResults, _ := out["scrub_results"].([]any)
	var warmScrub map[string]any
	for _, sr := range scrubResults {
		m, _ := sr.(map[string]any)
		if m != nil && m["collection_id"] == warmCollID {
			warmScrub = m
			break
		}
	}
	if warmScrub == nil {
		t.Fatalf("warm collection scrub result not found: %v", scrubResults)
	}

	// First run: all files should be baselined (checksum was empty)
	baselined1 := int(warmScrub["baselined"].(float64))
	verified1 := int(warmScrub["verified"].(float64))
	cursor1 := warmScrub["cursor"].(string)
	totalFiles1 := int(warmScrub["total_files"].(float64))

	if totalFiles1 != 6 {
		t.Errorf("total_files: got %d, want 6", totalFiles1)
	}
	if baselined1 == 0 {
		t.Errorf("first run should baseline some checksums, got baselined=%d", baselined1)
	}
	if verified1 != 0 {
		t.Errorf("first run should have 0 verified (all baselined), got verified=%d", verified1)
	}
	if cursor1 == "" {
		t.Errorf("first run cursor should not be empty (not all files processed in one slice)")
	}

	// Second run: should verify the baselined checksums
	code, out = maintenanceDoReq(t, ts, "POST", "/api/maintenance/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("POST maintenance/run (2nd): expected 200, got %d: %v", code, out)
	}

	scrubResults, _ = out["scrub_results"].([]any)
	for _, sr := range scrubResults {
		m, _ := sr.(map[string]any)
		if m != nil && m["collection_id"] == warmCollID {
			warmScrub = m
			break
		}
	}
	if warmScrub == nil {
		t.Fatalf("warm collection scrub result not found on 2nd run: %v", scrubResults)
	}

	baselined2 := int(warmScrub["baselined"].(float64))
	verified2 := int(warmScrub["verified"].(float64))
	cursor2 := warmScrub["cursor"].(string)

	// Second run processes NEXT slice (files c.jpg, d.jpg) which also have empty checksums → baselined, not verified
	if baselined2 != 2 {
		t.Errorf("second run should baseline next slice (2 files), got baselined=%d", baselined2)
	}
	if verified2 != 0 {
		t.Errorf("second run should have 0 verified (next slice not baselined yet), got verified=%d", verified2)
	}

	// Status endpoint: D1 defect — returns 500 when no files have verified_at set.
	// Assert CORRECT contract (200), report D1 if it fails.
	code, out = maintenanceDoReq(t, ts, "GET", "/api/maintenance/status", nil, token)
	if code != http.StatusOK {
		t.Errorf("D1 exposed by TestMaintenanceScrubRotation: GET maintenance/status returned %d (expected 200)", code)
	} else {
		statuses, _ := out["statuses"].([]any)
		var warmStatus map[string]any
		for _, st := range statuses {
			m, _ := st.(map[string]any)
			if m != nil && m["collection_id"] == warmCollID {
				warmStatus = m
				break
			}
		}
		if warmStatus != nil {
			verifiedFraction := warmStatus["verified_fraction"].(float64)
			// After 2 runs, 4 of 6 files baselined, 0 verified → verified_fraction = 0
			if verifiedFraction != 0 {
				t.Errorf("verified_fraction after 2 runs: got %v, want 0 (no files verified yet)", verifiedFraction)
			}
		}
	}

	// Cursor should have advanced
	t.Logf("cursor after 1st run: %q, after 2nd run: %q", cursor1, cursor2)
}

// TestMaintenanceMismatch tests Case 3: after baselining all files and wrapping cursor,
// corrupt one file → next run that processes it reports mismatched > 0 AND audit row.
func TestMaintenanceMismatch(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ts, token, warmCollID, warmCollRoot, _, _, _ := setupMaintenanceServer(t, db)
	defer ts.Close()

	// Write 3 files (sliceSize=1 per run, 3 runs to wrap)
	writeMaintenanceFile(t, warmCollRoot, "a.jpg", "content-a")
	writeMaintenanceFile(t, warmCollRoot, "b.jpg", "content-b")
	writeMaintenanceFile(t, warmCollRoot, "c.jpg", "content-c")

	// Scan to index files
	code, out := maintenanceDoReq(t, ts, "POST", "/api/collections/"+warmCollID+"/scan", nil, token)
	if code != http.StatusOK {
		t.Fatalf("scan warm: expected 200, got %d: %v", code, out)
	}

	// Run 1: baseline a.jpg
	code, out = maintenanceDoReq(t, ts, "POST", "/api/maintenance/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("POST maintenance/run (1): expected 200, got %d: %v", code, out)
	}

	// Run 2: baseline b.jpg
	code, out = maintenanceDoReq(t, ts, "POST", "/api/maintenance/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("POST maintenance/run (2): expected 200, got %d: %v", code, out)
	}

	// Run 3: baseline c.jpg, cursor wraps to ""
	code, out = maintenanceDoReq(t, ts, "POST", "/api/maintenance/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("POST maintenance/run (3): expected 200, got %d: %v", code, out)
	}

	// Now all files have checksums, cursor wrapped. Corrupt a.jpg.
	writeMaintenanceFile(t, warmCollRoot, "a.jpg", "CORRUPTED-CONTENT")

	// Run 4: processes a.jpg again → should detect mismatch
	code, out = maintenanceDoReq(t, ts, "POST", "/api/maintenance/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("POST maintenance/run (4): expected 200, got %d: %v", code, out)
	}

	scrubResults, _ := out["scrub_results"].([]any)
	var warmScrub map[string]any
	for _, sr := range scrubResults {
		m, _ := sr.(map[string]any)
		if m != nil && m["collection_id"] == warmCollID {
			warmScrub = m
			break
		}
	}
	if warmScrub == nil {
		t.Fatalf("warm collection scrub result not found: %v", scrubResults)
	}

	mismatched := int(warmScrub["mismatched"].(float64))
	if mismatched == 0 {
		t.Errorf("expected mismatched > 0 after corrupting a.jpg and wrapping, got mismatched=%d", mismatched)
	}

	// Verify audit row exists (schema: action, not kind; detail format: "collection=<id> rel_path=<path>")
	var auditAction string
	err = db.QueryRow(`SELECT action FROM audit_log WHERE action = ? AND detail LIKE ? LIMIT 1`,
		"maintenance.scrub.mismatch", "%collection="+warmCollID+" rel_path=a.jpg%").Scan(&auditAction)
	if err != nil {
		t.Errorf("expected audit row maintenance.scrub.mismatch for collection %s file a.jpg: %v", warmCollID, err)
	}
}

// TestMaintenanceNeverWake tests Case 4: collection on a Cold-asleep pool → skipped,
// disk not woken.
func TestMaintenanceNeverWake(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ts, token, _, _, coldCollID, coldCollRoot, _ := setupMaintenanceServer(t, db)
	defer ts.Close()

	// Write a file in the cold collection
	writeMaintenanceFile(t, coldCollRoot, "x.bin", "cold-content")

	// Scan to index files
	code, out := maintenanceDoReq(t, ts, "POST", "/api/collections/"+coldCollID+"/scan", nil, token)
	if code != http.StatusOK {
		t.Fatalf("scan cold: expected 200, got %d: %v", code, out)
	}

	// Run maintenance
	code, out = maintenanceDoReq(t, ts, "POST", "/api/maintenance/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("POST maintenance/run: expected 200, got %d: %v", code, out)
	}

	// Cold collection should be in skipped_collections
	skippedCollections, ok := out["skipped_collections"].([]any)
	if !ok {
		t.Fatalf("response missing skipped_collections: %v", out)
	}
	foundCold := false
	for _, sc := range skippedCollections {
		m, _ := sc.(map[string]any)
		if m != nil && m["collection_id"] == coldCollID {
			foundCold = true
			if m["reason"] != "cold pool: never wake for scrub" {
				t.Errorf("cold skip reason: got %q, want 'cold pool: never wake for scrub'", m["reason"])
			}
		}
	}
	if !foundCold {
		t.Errorf("cold collection %s not found in skipped_collections: %v", coldCollID, skippedCollections)
	}

	// Cold collection should NOT be in scrub_results
	scrubResults, _ := out["scrub_results"].([]any)
	for _, sr := range scrubResults {
		m, _ := sr.(map[string]any)
		if m != nil && m["collection_id"] == coldCollID {
			t.Errorf("cold collection %s should not be in scrub_results (was skipped)", coldCollID)
		}
	}
}

// TestMaintenanceStatusAndAuth tests Case 5: status endpoint + auth + method checks.
func TestMaintenanceStatusAndAuth(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ts, token, warmCollID, warmCollRoot, _, _, _ := setupMaintenanceServer(t, db)
	defer ts.Close()

	// Write and scan a file
	writeMaintenanceFile(t, warmCollRoot, "a.jpg", "content-a")
	code, out := maintenanceDoReq(t, ts, "POST", "/api/collections/"+warmCollID+"/scan", nil, token)
	if code != http.StatusOK {
		t.Fatalf("scan warm: expected 200, got %d: %v", code, out)
	}

	// GET /api/maintenance/status → D1 defect: returns 500 when no files have verified_at set.
	// Assert CORRECT contract (200), report D1 if it fails.
	code, out = maintenanceDoReq(t, ts, "GET", "/api/maintenance/status", nil, token)
	if code != http.StatusOK {
		t.Errorf("D1 exposed by TestMaintenanceStatusAndAuth: GET maintenance/status returned %d (expected 200)", code)
	} else {
		statuses, ok := out["statuses"].([]any)
		if !ok {
			t.Fatalf("response missing statuses: %v", out)
		}
		if len(statuses) == 0 {
			t.Errorf("expected at least 1 status, got 0")
		} else {
			// Verify warm collection appears in statuses
			foundWarm := false
			for _, st := range statuses {
				m, _ := st.(map[string]any)
				if m != nil && m["collection_id"] == warmCollID {
					foundWarm = true
					if _, ok := m["verified_fraction"].(float64); !ok {
						t.Errorf("status missing verified_fraction: %v", m)
					}
					if _, ok := m["cursor"].(string); !ok {
						t.Errorf("status missing cursor: %v", m)
					}
					if _, ok := m["total_files"].(float64); !ok {
						t.Errorf("status missing total_files: %v", m)
					}
				}
			}
			if !foundWarm {
				t.Errorf("warm collection %s not found in statuses: %v", warmCollID, statuses)
			}
		}
	}

	// Unauth on both endpoints → 401
	code, _ = maintenanceDoReq(t, ts, "POST", "/api/maintenance/run", nil, "")
	if code != http.StatusUnauthorized {
		t.Errorf("POST maintenance/run unauth: expected 401, got %d", code)
	}
	code, _ = maintenanceDoReq(t, ts, "GET", "/api/maintenance/status", nil, "")
	if code != http.StatusUnauthorized {
		t.Errorf("GET maintenance/status unauth: expected 401, got %d", code)
	}

	// GET on run → 405
	code, _ = maintenanceDoReq(t, ts, "GET", "/api/maintenance/run", nil, token)
	if code != http.StatusMethodNotAllowed {
		t.Errorf("GET maintenance/run: expected 405, got %d", code)
	}

	// POST on status → 405
	code, _ = maintenanceDoReq(t, ts, "POST", "/api/maintenance/status", nil, token)
	if code != http.StatusMethodNotAllowed {
		t.Errorf("POST maintenance/status: expected 405, got %d", code)
	}
}

// TestMaintenanceEmptyCollectionStatus tests D1: GET /api/maintenance/status with an
// empty collection (zero files) must return 200 with verified_fraction = 0, not 500.
// Production defect D1: Status scans SUM(CASE...) into int; SQLite returns NULL for
// empty collections causing scan error. This test asserts the CORRECT contract.
func TestMaintenanceEmptyCollectionStatus(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	ts, token, _, _, _, _, _ := setupMaintenanceServer(t, db)
	defer ts.Close()

	// Create a NEW empty collection (no files written, no scan)
	var warmPoolID string
	err = db.QueryRow(`SELECT id FROM pools WHERE name = ?`, "warm").Scan(&warmPoolID)
	if err != nil {
		t.Fatalf("get warm pool id: %v", err)
	}

	code, resp := maintenanceDoReq(t, ts, "POST", "/api/pools/"+warmPoolID+"/collections", map[string]any{
		"name":       "empty",
		"path":       "empty",
		"disk_name":  "sda",
	}, token)
	if code != http.StatusCreated {
		t.Fatalf("POST empty collection: expected 201, got %d: %v", code, resp)
	}
	var emptyCollID string
	err = db.QueryRow(`SELECT id FROM collections WHERE name = ? AND pool_id = ?`, "empty", warmPoolID).Scan(&emptyCollID)
	if err != nil {
		t.Fatalf("get empty collection id: %v", err)
	}

	// GET /api/maintenance/status → must be 200 even with empty collection
	code, out := maintenanceDoReq(t, ts, "GET", "/api/maintenance/status", nil, token)
	if code != http.StatusOK {
		t.Errorf("D1 exposed by TestMaintenanceEmptyCollectionStatus: GET maintenance/status returned %d (expected 200) for empty collection %s", code, emptyCollID)
		return // don't continue checking body if status is wrong
	}

	statuses, ok := out["statuses"].([]any)
	if !ok {
		t.Fatalf("response missing statuses: %v", out)
	}

	foundEmpty := false
	for _, st := range statuses {
		m, _ := st.(map[string]any)
		if m != nil && m["collection_id"] == emptyCollID {
			foundEmpty = true
			vf, ok := m["verified_fraction"].(float64)
			if !ok {
				t.Errorf("empty collection status missing verified_fraction: %v", m)
			} else if vf != 0 {
				t.Errorf("empty collection verified_fraction: got %v, want 0", vf)
			}
			tf, ok := m["total_files"].(float64)
			if !ok {
				t.Errorf("empty collection status missing total_files: %v", m)
			} else if tf != 0 {
				t.Errorf("empty collection total_files: got %v, want 0", tf)
			}
		}
	}
	if !foundEmpty {
		t.Errorf("empty collection %s not found in statuses: %v", emptyCollID, statuses)
	}
}