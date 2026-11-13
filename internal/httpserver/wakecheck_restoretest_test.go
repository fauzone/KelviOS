package httpserver

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

// buildTeststub compiles internal/wakecheck/teststub once per test and
// points KELVIOS_SMARTCTL_BIN and KELVIOS_SNAPRAID_BIN at it.
func buildTeststub(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "teststub")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		goBin += ".exe"
	}
	cmd := exec.Command(goBin, "build", "-o", bin, "github.com/fauzone/kelvios/internal/wakecheck/teststub")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build teststub: %v\n%s", err, out)
	}
	t.Setenv("KELVIOS_SMARTCTL_BIN", bin)
	t.Setenv("KELVIOS_SNAPRAID_BIN", bin)
	t.Setenv("KELVIOS_SNAPRAID_CONF", "")
	return bin
}

// setupWakeChecklistServerWithRestoreTest creates a test server with a Cold pool,
// one disk, restic stub, teststub, and vault configured.
// Returns ts, token, poolID, diskName, teststubPath, resticStubPath, server instance, coldMountpoint.
func setupWakeChecklistServerWithRestoreTest(t *testing.T, db *sql.DB) (*httptest.Server, string, string, string, string, string, *Server, string) {
	t.Helper()

	// Build teststub (smartctl/snapraid)
	teststubPath := buildTeststub(t)

	// Build restic stub
	resticStubPath := buildResticStub(t)

	// Use a temp dir for cold mountpoint to avoid permission issues on /mnt
	coldMountpoint := filepath.Join(t.TempDir(), "cold")

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{Path: ":memory:"},
		UPS:      config.UPSConfig{},
	}

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-WAKE-COLD", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	s := New(cfg, db, "test-version")
	s.SetDriver(storage.NewSingleDriver(disks))
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	ts := httptest.NewServer(s.srv.Handler)

	// Create test user and session for authentication
	token := "test-token-wakecheck-rt"
	tokenHash := sha256Hex(token)
	now := time.Now().Unix()
	_, err := db.Exec(`INSERT INTO accounts (id, username, password_hash, failed_count, locked_until, created_at) VALUES (?, ?, ?, 0, 0, ?)`,
		"acct-wakecheck-rt", "wakecheckuser", "dummy-hash", now)
	if err != nil {
		t.Fatalf("create test account: %v", err)
	}
	_, err = db.Exec(`INSERT INTO sessions (token_hash, username, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, "wakecheckuser", now, now+3600)
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}

	// Create a Cold pool via API
	poolName := "coldpool-restore-test"
	code, resp := tagDoReq(t, ts, "POST", "/api/pools", map[string]any{
		"name":        poolName,
		"tier":        "cold",
		"mechanism":   "single",
		"disks":       []string{"sda"},
		"mountpoint":  coldMountpoint,
	}, token)
	if code != http.StatusCreated {
		t.Fatalf("POST pool: expected 201, got %d: %v", code, resp)
	}

	// Get pool ID from catalog
	var poolID string
	err = db.QueryRow(`SELECT id FROM pools WHERE name = ?`, poolName).Scan(&poolID)
	if err != nil {
		t.Fatalf("get pool id: %v", err)
	}

	// Register with power engine
	s.power.RegisterPool(poolID, storage.TierCold, poolName)
	s.power.RegisterDisk("sda", poolID)

	return ts, token, poolID, "sda", teststubPath, resticStubPath, s, coldMountpoint
}

// insertRestoreTestRun inserts a row into restore_test_runs with the given started_at.
func insertRestoreTestRun(t *testing.T, db *sql.DB, runID string, startedAt int64) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO restore_test_runs (id, started_at, samples, passed, failed, skipped, alerts) VALUES (?, ?, 0, 0, 0, 0, 0)`,
		runID, startedAt)
	if err != nil {
		t.Fatalf("insert restore_test_runs: %v", err)
	}
}

// countRestoreTestRuns returns the number of rows in restore_test_runs.
func countRestoreTestRuns(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM restore_test_runs`).Scan(&count)
	if err != nil {
		t.Fatalf("count restore_test_runs: %v", err)
	}
	return count
}

// TestWakeChecklistRestoreTestNotDue tests the not-due path:
// a recent row in restore_test_runs (started_at = now) → step 5 records
// status "skipped", detail "not due", and NO restore test runs (run count unchanged).
func TestWakeChecklistRestoreTestNotDue(t *testing.T) {
	db := openTagDB(t)
	defer db.Close()

	ts, token, _, disk, _, _, _, _ := setupWakeChecklistServerWithRestoreTest(t, db)
	defer ts.Close()

	// Insert a recent restore_test_runs row (started_at = now)
	now := time.Now().Unix()
	insertRestoreTestRun(t, db, "rtr-recent-1", now)

	initialCount := countRestoreTestRuns(t, db)
	if initialCount != 1 {
		t.Fatalf("expected 1 initial run, got %d", initialCount)
	}

	// Run checklist synchronously
	code, out := wakeChecklistDoReq(t, ts, "POST", "/api/power/"+disk+"/wake-checklist/run", token)
	if code != http.StatusOK {
		t.Fatalf("POST wake-checklist/run: expected 200, got %d: %v", code, out)
	}

	run, ok := out["run"].(map[string]any)
	if !ok {
		t.Fatalf("response missing run: %v", out)
	}

	steps, ok := run["steps"].([]any)
	if !ok {
		t.Fatalf("run missing steps: %v", run)
	}

	if len(steps) != 5 {
		t.Fatalf("expected 5 steps, got %d: %v", len(steps), steps)
	}

	// Verify step 5 is restore_test with status "skipped" and detail "not due"
	restoreStep := steps[4].(map[string]any)
	if restoreStep["step"] != "restore_test" {
		t.Errorf("step 5: expected step=restore_test, got %v", restoreStep["step"])
	}
	if restoreStep["status"] != "skipped" {
		t.Errorf("restore_test status: expected skipped, got %s (detail: %v)", restoreStep["status"], restoreStep["detail"])
	}
	detail := restoreStep["detail"].(string)
	if detail != "not due" {
		t.Errorf("restore_test detail: expected 'not due', got %q", detail)
	}

	// Verify run count unchanged (no new restore test run)
	finalCount := countRestoreTestRuns(t, db)
	if finalCount != initialCount {
		t.Errorf("restore_test_runs count changed: expected %d, got %d", initialCount, finalCount)
	}

	// Verify step order: restore_test is AFTER the four existing steps
	expectedSteps := []string{"flush_dirty", "parity_sync", "backup_delta", "smart_quick", "restore_test"}
	for i, exp := range expectedSteps {
		step := steps[i].(map[string]any)
		if step["step"] != exp {
			t.Errorf("step %d: expected %s, got %s", i, exp, step["step"])
		}
	}
}

// TestWakeChecklistRestoreTestDueNoPriorRun tests the due path with no prior run:
// empty restore_test_runs → step 5 runs and records status "ok" with counts detail.
// Uses restic stub + vault secret so the run can complete. A zero-repo run
// completing with samples=0 is acceptable ONLY if we assert the step actually
// invoked RunScheduled (new run row appears in restore_test_runs).
func TestWakeChecklistRestoreTestDueNoPriorRun(t *testing.T) {
	db := openTagDB(t)
	defer db.Close()

	ts, token, _, disk, _, _, srv, _ := setupWakeChecklistServerWithRestoreTest(t, db)
	defer ts.Close()

	// Verify restore_test_runs is empty initially
	initialCount := countRestoreTestRuns(t, db)
	if initialCount != 0 {
		t.Fatalf("expected 0 initial runs, got %d", initialCount)
	}

	// Setup vault and create a backup repo with secret so restore test can run
	// (even with zero repos, RunScheduled will be invoked and create a run row)
	err := srv.vault.Setup("vault-passphrase-12345678")
	if err != nil {
		t.Fatalf("vault setup: %v", err)
	}

	// Create a backup repo via API
	repoPath := filepath.Join(t.TempDir(), "restic-repo")
	code, out := tagDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": "test-repo", "target_path": repoPath}, token)
	if code != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d: %v", code, out)
	}
	repoMap, _ := out["repo"].(map[string]any)
	repoID, _ := repoMap["id"].(string)
	if repoID == "" {
		t.Fatalf("create repo: no id in %v", out)
	}

	// Store vault secret for the repo
	secretName := "backup-repo:" + repoID
	err = srv.vault.Put(secretName, "test-password-12345678")
	if err != nil {
		t.Fatalf("vault put secret: %v", err)
	}

	// Run checklist synchronously
	code, out = wakeChecklistDoReq(t, ts, "POST", "/api/power/"+disk+"/wake-checklist/run", token)
	if code != http.StatusOK {
		t.Fatalf("POST wake-checklist/run: expected 200, got %d: %v", code, out)
	}

	run, ok := out["run"].(map[string]any)
	if !ok {
		t.Fatalf("response missing run: %v", out)
	}

	steps, ok := run["steps"].([]any)
	if !ok {
		t.Fatalf("run missing steps: %v", run)
	}

	if len(steps) != 5 {
		t.Fatalf("expected 5 steps, got %d: %v", len(steps), steps)
	}

	// Verify step 5 is restore_test with status "ok"
	restoreStep := steps[4].(map[string]any)
	if restoreStep["step"] != "restore_test" {
		t.Errorf("step 5: expected step=restore_test, got %v", restoreStep["step"])
	}
	if restoreStep["status"] != "ok" {
		t.Errorf("restore_test status: expected ok, got %s (detail: %v)", restoreStep["status"], restoreStep["detail"])
	}
	detail := restoreStep["detail"].(string)
	// Should contain samples=0 passed=0 failed=0 skipped=0 alerts=0
	if detail != "samples=0 passed=0 failed=0 skipped=0 alerts=0" {
		t.Errorf("restore_test detail: expected 'samples=0 passed=0 failed=0 skipped=0 alerts=0', got %q", detail)
	}

	// Verify a new run row was created in restore_test_runs (RunScheduled was invoked)
	finalCount := countRestoreTestRuns(t, db)
	if finalCount != initialCount+1 {
		t.Errorf("restore_test_runs count: expected %d (one new run), got %d", initialCount+1, finalCount)
	}

	// Verify step order
	expectedSteps := []string{"flush_dirty", "parity_sync", "backup_delta", "smart_quick", "restore_test"}
	for i, exp := range expectedSteps {
		step := steps[i].(map[string]any)
		if step["step"] != exp {
			t.Errorf("step %d: expected %s, got %s", i, exp, step["step"])
		}
	}
}

// TestWakeChecklistRestoreTestDueStalePriorRun tests the due path with stale prior run:
// a row with started_at older than 7 days → step 5 runs again (new run row appears).
func TestWakeChecklistRestoreTestDueStalePriorRun(t *testing.T) {
	db := openTagDB(t)
	defer db.Close()

	ts, token, _, disk, _, _, srv, _ := setupWakeChecklistServerWithRestoreTest(t, db)
	defer ts.Close()

	// Insert a stale restore_test_runs row (started_at = 8 days ago)
	staleTime := time.Now().Add(-8 * 24 * time.Hour).Unix()
	insertRestoreTestRun(t, db, "rtr-stale-1", staleTime)

	initialCount := countRestoreTestRuns(t, db)
	if initialCount != 1 {
		t.Fatalf("expected 1 initial run, got %d", initialCount)
	}

	// Setup vault
	err := srv.vault.Setup("vault-passphrase-12345678")
	if err != nil {
		t.Fatalf("vault setup: %v", err)
	}

	// Create a backup repo via API
	repoPath := filepath.Join(t.TempDir(), "restic-repo")
	code, out := tagDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": "test-repo", "target_path": repoPath}, token)
	if code != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d: %v", code, out)
	}
	repoMap, _ := out["repo"].(map[string]any)
	repoID, _ := repoMap["id"].(string)
	if repoID == "" {
		t.Fatalf("create repo: no id in %v", out)
	}

	// Store vault secret for the repo
	secretName := "backup-repo:" + repoID
	err = srv.vault.Put(secretName, "test-password-12345678")
	if err != nil {
		t.Fatalf("vault put secret: %v", err)
	}

	// Run checklist synchronously
	code, out = wakeChecklistDoReq(t, ts, "POST", "/api/power/"+disk+"/wake-checklist/run", token)
	if code != http.StatusOK {
		t.Fatalf("POST wake-checklist/run: expected 200, got %d: %v", code, out)
	}

	run, ok := out["run"].(map[string]any)
	if !ok {
		t.Fatalf("response missing run: %v", out)
	}

	steps, ok := run["steps"].([]any)
	if !ok {
		t.Fatalf("run missing steps: %v", run)
	}

	if len(steps) != 5 {
		t.Fatalf("expected 5 steps, got %d: %v", len(steps), steps)
	}

	// Verify step 5 is restore_test with status "ok"
	restoreStep := steps[4].(map[string]any)
	if restoreStep["step"] != "restore_test" {
		t.Errorf("step 5: expected step=restore_test, got %v", restoreStep["step"])
	}
	if restoreStep["status"] != "ok" {
		t.Errorf("restore_test status: expected ok, got %s (detail: %v)", restoreStep["status"], restoreStep["detail"])
	}

	// Verify a NEW run row was created (now 2 total)
	finalCount := countRestoreTestRuns(t, db)
	if finalCount != initialCount+1 {
		t.Errorf("restore_test_runs count: expected %d (one new run), got %d", initialCount+1, finalCount)
	}

	// Verify step order
	expectedSteps := []string{"flush_dirty", "parity_sync", "backup_delta", "smart_quick", "restore_test"}
	for i, exp := range expectedSteps {
		step := steps[i].(map[string]any)
		if step["step"] != exp {
			t.Errorf("step %d: expected %s, got %s", i, exp, step["step"])
		}
	}
}

// TestWakeChecklistRestoreTestStepIsolation tests step isolation:
// a failing restore-test run → step 5 records status "error" while steps 1–4
// still recorded their own statuses and the endpoint still returns 200.
func TestWakeChecklistRestoreTestStepIsolation(t *testing.T) {
	db := openTagDB(t)
	defer db.Close()

	ts, token, poolID, disk, _, _, srv, coldMountpoint := setupWakeChecklistServerWithRestoreTest(t, db)
	defer ts.Close()

	// Setup vault
	err := srv.vault.Setup("vault-passphrase-12345678")
	if err != nil {
		t.Fatalf("vault setup: %v", err)
	}

	// Create a backup repo via API
	repoPath := filepath.Join(t.TempDir(), "restic-repo-fail")
	code, out := tagDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": "fail-repo", "target_path": repoPath}, token)
	if code != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d: %v", code, out)
	}
	repoMap, _ := out["repo"].(map[string]any)
	repoID, _ := repoMap["id"].(string)
	if repoID == "" {
		t.Fatalf("create repo: no id in %v", out)
	}

	// Store WRONG vault secret (password mismatch will cause restore to fail)
	secretName := "backup-repo:" + repoID
	err = srv.vault.Put(secretName, "wrong-password")
	if err != nil {
		t.Fatalf("vault put secret: %v", err)
	}

	// Create a collection and file, scan, and backup to have a snapshot
	var collID string
	code, collResp := tagDoReq(t, ts, "POST", "/api/pools/"+poolID+"/collections", map[string]any{"name": "photos", "path": "photos", "disk_name": "sda"}, token)
	if code != http.StatusCreated {
		// Collection might already exist from pool creation, try to get it
		err = db.QueryRow(`SELECT id FROM collections WHERE pool_id = ? AND name = 'photos'`, poolID).Scan(&collID)
		if err != nil {
			t.Fatalf("get collection id: %v", err)
		}
	} else {
		collMap := collResp["collection"].(map[string]any)
		collID = collMap["id"].(string)
	}

	// Write a file to the collection root (use the pool's cold mountpoint)
	collRoot := filepath.Join(coldMountpoint, "photos")
	if err := os.MkdirAll(collRoot, 0o755); err != nil {
		t.Fatalf("mkdir coll root: %v", err)
	}
	fileContent := "test-file-content-for-restore-fail"
	writeRestoreTestFile(t, collRoot, "test.txt", fileContent, time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC))

	// Scan the collection
	code, out = tagDoReq(t, ts, "POST", "/api/collections/"+collID+"/scan", nil, token)
	if code != http.StatusOK {
		t.Fatalf("scan: expected 200, got %d: %v", code, out)
	}

	// Wake the disk via power engine so backup API allows the run (coldAwake check)
	_, _ = srv.power.Wake("sda")

	// Run backup to create a snapshot (now pool is awake)
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos/"+repoID+"/run", map[string]any{"collection_id": collID, "password": "test-password-12345678"}, token)
	if code != http.StatusOK {
		t.Fatalf("backup run: expected 200, got %d: %v", code, out)
	}

	// Now run checklist - restore test should fail due to wrong password
	code, out = wakeChecklistDoReq(t, ts, "POST", "/api/power/"+disk+"/wake-checklist/run", token)
	if code != http.StatusOK {
		t.Fatalf("POST wake-checklist/run: expected 200 (step isolation), got %d: %v", code, out)
	}

	run, ok := out["run"].(map[string]any)
	if !ok {
		t.Fatalf("response missing run: %v", out)
	}

	steps, ok := run["steps"].([]any)
	if !ok {
		t.Fatalf("run missing steps: %v", run)
	}

	if len(steps) != 5 {
		t.Fatalf("expected 5 steps, got %d: %v", len(steps), steps)
	}

	// Verify step 5 is restore_test with status "error"
	restoreStep := steps[4].(map[string]any)
	if restoreStep["step"] != "restore_test" {
		t.Errorf("step 5: expected step=restore_test, got %v", restoreStep["step"])
	}
	if restoreStep["status"] != "error" {
		t.Errorf("restore_test status: expected error, got %s (detail: %v)", restoreStep["status"], restoreStep["detail"])
	}

	// Verify steps 1-4 are NOT error (they should be ok or skipped)
	for i := 0; i < 4; i++ {
		step := steps[i].(map[string]any)
		status := step["status"].(string)
		if status == "error" {
			t.Errorf("step %d (%s): should not be error (isolation), got %s: %v", i, step["step"], status, step["detail"])
		}
	}

	// Verify step order
	expectedSteps := []string{"flush_dirty", "parity_sync", "backup_delta", "smart_quick", "restore_test"}
	for i, exp := range expectedSteps {
		step := steps[i].(map[string]any)
		if step["step"] != exp {
			t.Errorf("step %d: expected %s, got %s", i, exp, step["step"])
		}
	}
}

// TestWakeChecklistRestoreTestStepOrder tests that the recorded steps list
// shows restore_test AFTER the four existing steps.
func TestWakeChecklistRestoreTestStepOrder(t *testing.T) {
	db := openTagDB(t)
	defer db.Close()

	ts, token, _, disk, _, _, _, _ := setupWakeChecklistServerWithRestoreTest(t, db)
	defer ts.Close()

	// Insert a recent restore_test_runs row so it's skipped (simpler, no vault setup needed)
	now := time.Now().Unix()
	insertRestoreTestRun(t, db, "rtr-recent-order", now)

	// Run checklist synchronously
	code, out := wakeChecklistDoReq(t, ts, "POST", "/api/power/"+disk+"/wake-checklist/run", token)
	if code != http.StatusOK {
		t.Fatalf("POST wake-checklist/run: expected 200, got %d: %v", code, out)
	}

	run, ok := out["run"].(map[string]any)
	if !ok {
		t.Fatalf("response missing run: %v", out)
	}

	steps, ok := run["steps"].([]any)
	if !ok {
		t.Fatalf("run missing steps: %v", run)
	}

	if len(steps) != 5 {
		t.Fatalf("expected 5 steps, got %d: %v", len(steps), steps)
	}

	// Verify exact order
	expectedSteps := []string{"flush_dirty", "parity_sync", "backup_delta", "smart_quick", "restore_test"}
	for i, exp := range expectedSteps {
		step := steps[i].(map[string]any)
		if step["step"] != exp {
			t.Errorf("step %d: expected %s, got %s", i, exp, step["step"])
		}
	}

	// Explicitly verify restore_test is at index 4 (5th position)
	restoreStep := steps[4].(map[string]any)
	if restoreStep["step"] != "restore_test" {
		t.Errorf("restore_test not at position 5 (index 4): got %v", restoreStep["step"])
	}
}