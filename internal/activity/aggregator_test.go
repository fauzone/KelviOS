// Package activity implements the Activity & Automation Core API (FR-4.7 backend).
package activity

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/compose"
	"github.com/fauzone/kelvios/internal/power"
	"github.com/fauzone/kelvios/internal/storage"
	_ "modernc.org/sqlite"
)

// TestAggregatorList tests the List method with seeded data.
func TestAggregatorList(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Seed compose artifacts
	seedComposeArtifacts(t, db)

	// Seed restore test data
	seedRestoreTestData(t, db)

	// Seed maintenance data
	seedMaintenanceData(t, db)

	// Create power engine with schedules
	powerEngine := power.New(nil, nil, 0)
	powerEngine.RegisterPool("pool1", storage.TierWarm, "Test Pool")
	powerEngine.RegisterDisk("disk1", "pool1")
	powerEngine.SetSchedules("disk1", []power.Schedule{
		{Days: "Mon,Tue,Wed,Thu,Fri", StartMin: 540, EndMin: 1020, Enabled: true}, // 9:00-17:00
	})

	// Create aggregator
	agg := NewAggregator(db, nil, powerEngine, storage.Inventory, time.Now, nil)

	ctx := context.Background()
	resp, err := agg.List(ctx, "", "")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if resp == nil {
		t.Fatal("response is nil")
	}

	// Verify we have items from all sources
	sources := make(map[string]bool)
	for _, item := range resp.Items {
		sources[item.Source] = true
	}

	// Check expected sources are present
	expectedSources := []string{SourceCompose, SourceRestoreTest, SourceMaintenance, SourcePower, SourceAgent}
	for _, src := range expectedSources {
		if !sources[src] {
			t.Errorf("missing source: %s", src)
		}
	}

	// Verify summary fields
	if resp.Summary.Total == 0 {
		t.Error("summary total is 0")
	}
	if resp.Summary.ByStatus == nil {
		t.Error("summary by_status is nil")
	}
	if resp.Summary.FailedIDs == nil {
		t.Error("summary failed_ids is nil")
	}
}

// TestAggregatorListWithFilters tests the List method with source and status filters.
func TestAggregatorListWithFilters(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	seedComposeArtifacts(t, db)
	seedRestoreTestData(t, db)
	seedMaintenanceData(t, db)

	powerEngine := power.New(nil, nil, 0)
	powerEngine.RegisterPool("pool1", storage.TierWarm, "Test Pool")
	powerEngine.RegisterDisk("disk1", "pool1")
	powerEngine.SetSchedules("disk1", []power.Schedule{
		{Days: "Mon,Tue,Wed,Thu,Fri", StartMin: 540, EndMin: 1020, Enabled: true},
	})

	agg := NewAggregator(db, nil, powerEngine, storage.Inventory, time.Now, nil)

	ctx := context.Background()

	// Test source filter
	resp, err := agg.List(ctx, SourceCompose, "")
	if err != nil {
		t.Fatalf("List with source filter failed: %v", err)
	}
	for _, item := range resp.Items {
		if item.Source != SourceCompose {
			t.Errorf("item source %s != expected %s", item.Source, SourceCompose)
		}
	}

	// Test status filter
	resp, err = agg.List(ctx, "", StatusScheduled)
	if err != nil {
		t.Fatalf("List with status filter failed: %v", err)
	}
	for _, item := range resp.Items {
		if item.Status != StatusScheduled {
			t.Errorf("item status %s != expected %s", item.Status, StatusScheduled)
		}
	}
}

// TestAggregatorGet tests the Get method for a single item.
func TestAggregatorGet(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	seedComposeArtifacts(t, db)

	powerEngine := power.New(nil, nil, 0)
	agg := NewAggregator(db, nil, powerEngine, storage.Inventory, time.Now, nil)

	ctx := context.Background()

	// Get a compose artifact item
	item, err := agg.Get(ctx, "compose:test-artifact-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if item == nil {
		t.Fatal("item is nil")
	}

	if item.ID != "compose:test-artifact-1" {
		t.Errorf("item ID %s != expected", item.ID)
	}
	if item.Source != SourceCompose {
		t.Errorf("item source %s != expected %s", item.Source, SourceCompose)
	}
	if item.Status == "" {
		t.Error("item status is empty")
	}
}

// TestAggregatorGetNotFound tests Get with invalid ID.
func TestAggregatorGetNotFound(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	powerEngine := power.New(nil, nil, 0)
	agg := NewAggregator(db, nil, powerEngine, storage.Inventory, time.Now, nil)

	ctx := context.Background()

	// Invalid format
	_, err := agg.Get(ctx, "invalid")
	if err == nil {
		t.Error("expected error for invalid ID format")
	}

	// Valid format but not found
	_, err = agg.Get(ctx, "compose:nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent item")
	}
}

// TestAggregatorExecuteActionPauseResume tests pause/resume on compose artifacts.
func TestAggregatorExecuteActionPauseResume(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	seedComposeArtifacts(t, db)

	powerEngine := power.New(nil, nil, 0)
	agg := NewAggregator(db, nil, powerEngine, storage.Inventory, time.Now, nil)

	ctx := context.Background()

	// Pause an active artifact
	item, err := agg.ExecuteAction(ctx, "compose:test-artifact-1", ActionPause, "testuser")
	if err != nil {
		t.Fatalf("Pause failed: %v", err)
	}

	if item.Status != StatusPaused {
		t.Errorf("status after pause: %s, expected %s", item.Status, StatusPaused)
	}

	// Verify in database
	art, err := compose.GetArtifact(db, "test-artifact-1")
	if err != nil {
		t.Fatalf("GetArtifact failed: %v", err)
	}
	if art.Status != compose.StatusDisabled {
		t.Errorf("artifact status in DB: %s, expected %s", art.Status, compose.StatusDisabled)
	}

	// Resume the artifact
	item, err = agg.ExecuteAction(ctx, "compose:test-artifact-1", ActionResume, "testuser")
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}

	if item.Status != StatusScheduled {
		t.Errorf("status after resume: %s, expected %s", item.Status, StatusScheduled)
	}

	// Verify in database
	art, err = compose.GetArtifact(db, "test-artifact-1")
	if err != nil {
		t.Fatalf("GetArtifact failed: %v", err)
	}
	if art.Status != compose.StatusActive {
		t.Errorf("artifact status in DB: %s, expected %s", art.Status, compose.StatusActive)
	}
}

// TestAggregatorExecuteActionUnsupported tests unsupported actions.
func TestAggregatorExecuteActionUnsupported(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	seedComposeArtifacts(t, db)

	powerEngine := power.New(nil, nil, 0)
	agg := NewAggregator(db, nil, powerEngine, storage.Inventory, time.Now, nil)

	ctx := context.Background()

	// Try to run-now on compose (not supported)
	_, err := agg.ExecuteAction(ctx, "compose:test-artifact-1", ActionRunNow, "testuser")
	if err == nil {
		t.Error("expected error for unsupported action run-now on compose")
	}
	if err.Error() != "control_not_supported" {
		t.Errorf("error message: %s, expected control_not_supported", err.Error())
	}

	// Try to cancel on compose (not supported)
	_, err = agg.ExecuteAction(ctx, "compose:test-artifact-1", ActionCancel, "testuser")
	if err == nil {
		t.Error("expected error for unsupported action cancel on compose")
	}
	if err.Error() != "control_not_supported" {
		t.Errorf("error message: %s, expected control_not_supported", err.Error())
	}
}

// TestAggregatorExecuteActionAlreadyRunning tests already_running error.
func TestAggregatorExecuteActionAlreadyRunning(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	seedRestoreTestData(t, db)

	powerEngine := power.New(nil, nil, 0)
	agg := NewAggregator(db, nil, powerEngine, storage.Inventory, time.Now, nil)

	ctx := context.Background()

	// Start a run-now (async)
	_, err := agg.ExecuteAction(ctx, "restore_test:repo1", ActionRunNow, "testuser")
	if err != nil {
		t.Fatalf("First run-now failed: %v", err)
	}

	// Try to run-now again while still running
	_, err = agg.ExecuteAction(ctx, "restore_test:repo1", ActionRunNow, "testuser")
	if err == nil {
		t.Error("expected already_running error")
	}
	if err.Error() != "already_running" {
		t.Errorf("error message: %s, expected already_running", err.Error())
	}
}

// TestAggregatorSourceErrorHandling tests that one source error doesn't break others.
func TestAggregatorSourceErrorHandling(t *testing.T) {
	// Create aggregator with nil power engine to simulate error
	db := setupTestDB(t)
	defer db.Close()

	seedComposeArtifacts(t, db)

	// No power engine - should cause power source to be unavailable
	agg := NewAggregator(db, nil, nil, storage.Inventory, time.Now, nil)

	ctx := context.Background()
	resp, err := agg.List(ctx, "", "")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	// Should still have compose items
	foundCompose := false
	foundPowerUnavailable := false
	for _, item := range resp.Items {
		if item.Source == SourceCompose {
			foundCompose = true
		}
		if item.Source == SourcePower && item.Status == StatusUnavailable {
			foundPowerUnavailable = true
		}
	}

	if !foundCompose {
		t.Error("compose items should still be present")
	}
	if !foundPowerUnavailable {
		t.Error("power source should be marked unavailable")
	}
}

// TestAggregatorDetailFailedItem tests detail includes failure reason and recovery hint.
func TestAggregatorDetailFailedItem(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Seed a failed restore test
	seedFailedRestoreTest(t, db)

	powerEngine := power.New(nil, nil, 0)
	agg := NewAggregator(db, nil, powerEngine, storage.Inventory, time.Now, nil)

	ctx := context.Background()

	item, err := agg.Get(ctx, "restore_test:repo1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if item.Status != StatusFailed {
		t.Errorf("status: %s, expected %s", item.Status, StatusFailed)
	}
	if item.LastRun == nil {
		t.Fatal("LastRun is nil for failed item")
	}
	if item.LastRun.Result != "failed" {
		t.Errorf("LastRun.Result: %s, expected failed", item.LastRun.Result)
	}
	if item.LastRun.FailureReason == "" {
		t.Error("FailureReason is empty for failed item")
	}
	if item.LastRun.RecoveryHint == "" {
		t.Error("RecoveryHint is empty for failed item")
	}
}

// TestAggregatorDetailNonFailedItem tests detail doesn't include failure fields for non-failed.
func TestAggregatorDetailNonFailedItem(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	seedComposeArtifacts(t, db)

	powerEngine := power.New(nil, nil, 0)
	agg := NewAggregator(db, nil, powerEngine, storage.Inventory, time.Now, nil)

	ctx := context.Background()

	item, err := agg.Get(ctx, "compose:test-artifact-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if item.Status == StatusFailed {
		t.Error("compose artifact should not be failed")
	}
	// For non-failed items, FailureReason and RecoveryHint should be empty
	if item.LastRun != nil {
		if item.LastRun.FailureReason != "" {
			t.Errorf("FailureReason should be empty for non-failed: %s", item.LastRun.FailureReason)
		}
		if item.LastRun.RecoveryHint != "" {
			t.Errorf("RecoveryHint should be empty for non-failed: %s", item.LastRun.RecoveryHint)
		}
	}
}

// TestAggregatorControlsMatchActual tests that Controls field matches actual supported actions.
func TestAggregatorControlsMatchActual(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	seedComposeArtifacts(t, db)
	seedRestoreTestData(t, db)
	seedMaintenanceData(t, db)

	powerEngine := power.New(nil, nil, 0)
	powerEngine.RegisterPool("pool1", storage.TierWarm, "Test Pool")
	powerEngine.RegisterDisk("disk1", "pool1")
	powerEngine.SetSchedules("disk1", []power.Schedule{
		{Days: "Mon,Tue,Wed,Thu,Fri", StartMin: 540, EndMin: 1020, Enabled: true},
	})

	agg := NewAggregator(db, nil, powerEngine, storage.Inventory, time.Now, nil)

	ctx := context.Background()
	resp, err := agg.List(ctx, "", "")
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	for _, item := range resp.Items {
		// Verify each control in Controls is actually supported
		for _, control := range item.Controls {
			// Try to execute - should not return control_not_supported
			_, err := agg.ExecuteAction(ctx, item.ID, control, "testuser")
			if err != nil && err.Error() == "control_not_supported" {
				t.Errorf("item %s advertises control %s but it's not supported", item.ID, control)
			}
		}
	}
}

// TestAggregatorNoRunTriggeredByGet tests that GET doesn't trigger runs.
func TestAggregatorNoRunTriggeredByGet(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	seedComposeArtifacts(t, db)
	seedRestoreTestData(t, db)
	seedMaintenanceData(t, db)

	powerEngine := power.New(nil, nil, 0)
	agg := NewAggregator(db, nil, powerEngine, storage.Inventory, time.Now, nil)

	ctx := context.Background()

	// Count runs before
	composeRunsBefore := countComposeRuns(t, db)
	restoreRunsBefore := countRestoreTestRuns(t, db)

	// Call List and Get multiple times
	for i := 0; i < 3; i++ {
		_, err := agg.List(ctx, "", "")
		if err != nil {
			t.Fatalf("List failed: %v", err)
		}
		_, err = agg.Get(ctx, "compose:test-artifact-1")
		if err != nil {
			t.Fatalf("Get failed: %v", err)
		}
	}

	// Count runs after
	composeRunsAfter := countComposeRuns(t, db)
	restoreRunsAfter := countRestoreTestRuns(t, db)

	if composeRunsAfter != composeRunsBefore {
		t.Errorf("compose runs changed: before=%d after=%d", composeRunsBefore, composeRunsAfter)
	}
	if restoreRunsAfter != restoreRunsBefore {
		t.Errorf("restore test runs changed: before=%d after=%d", restoreRunsBefore, restoreRunsAfter)
	}
}

// Helper functions

func setupTestDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)

	// Run migrations
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return db
}

func seedComposeArtifacts(t *testing.T, db *sql.DB) {
	now := time.Now().Unix()
	spec := []byte(`{"interval_hours": 24}`)

	// Active artifact
	_, err := db.Exec(`
		INSERT INTO compose_artifacts (id, name, kind, status, spec, current_version, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "test-artifact-1", "Daily Report", compose.KindReportSchedule, compose.StatusActive, spec, 1, "testuser", now, now)
	if err != nil {
		t.Fatalf("insert artifact 1: %v", err)
	}

	// Disabled artifact
	_, err = db.Exec(`
		INSERT INTO compose_artifacts (id, name, kind, status, spec, current_version, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "test-artifact-2", "Weekly Report", compose.KindReportSchedule, compose.StatusDisabled, spec, 1, "testuser", now, now)
	if err != nil {
		t.Fatalf("insert artifact 2: %v", err)
	}

	// Automation rule
	_, err = db.Exec(`
		INSERT INTO compose_artifacts (id, name, kind, status, spec, current_version, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "test-artifact-3", "Auto Backup", compose.KindAutomationRule, compose.StatusActive, spec, 1, "testuser", now, now)
	if err != nil {
		t.Fatalf("insert artifact 3: %v", err)
	}
}

func seedRestoreTestData(t *testing.T, db *sql.DB) {
	now := time.Now().Unix()

	// Insert backup repo
	_, err := db.Exec(`
		INSERT INTO backup_repos (id, name, target_type, target_path, config_json, secret_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "repo1", "Test Repo", "local", "/tmp/repo1", "", "", now)
	if err != nil {
		t.Fatalf("insert backup repo: %v", err)
	}

	// Insert collection (use unique ID to avoid conflict with maintenance seed)
	_, err = db.Exec(`
		INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "coll-restore-1", "Test Collection", "pool1", "/data", "disk1", 0, now, now)
	if err != nil {
		t.Fatalf("insert collection: %v", err)
	}

	// Insert snapshot
	_, err = db.Exec(`
		INSERT INTO backup_snapshots (id, repo_id, collection_id, restic_snapshot_id, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, "snap1", "repo1", "coll-restore-1", "snap-123", now)
	if err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	// Insert successful restore test run
	_, err = db.Exec(`
		INSERT INTO restore_test_runs (id, started_at, samples, passed, failed, skipped, alerts)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "run1", now-86400, 10, 10, 0, 0, 0)
	if err != nil {
		t.Fatalf("insert restore test run: %v", err)
	}

	// Insert alert for the run (to link repo_id)
	_, err = db.Exec(`
		INSERT INTO restore_test_alerts (id, run_id, repo_id, snapshot_id, rel_path, reason, created_at, resolved)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "alert1", "run1", "repo1", "snap-123", "test.txt", "ok", now, 1)
	if err != nil {
		t.Fatalf("insert restore test alert: %v", err)
	}
}

func seedFailedRestoreTest(t *testing.T, db *sql.DB) {
	now := time.Now().Unix()

	// Insert backup repo
	_, err := db.Exec(`
		INSERT INTO backup_repos (id, name, target_type, target_path, config_json, secret_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "repo1", "Test Repo", "local", "/tmp/repo1", "", "", now)
	if err != nil {
		t.Fatalf("insert backup repo: %v", err)
	}

	// Insert collection (use unique ID to avoid conflict with maintenance seed)
	_, err = db.Exec(`
		INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "coll-restore-1", "Test Collection", "pool1", "/data", "disk1", 0, now, now)
	if err != nil {
		t.Fatalf("insert collection: %v", err)
	}

	// Insert snapshot
	_, err = db.Exec(`
		INSERT INTO backup_snapshots (id, repo_id, collection_id, restic_snapshot_id, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, "snap1", "repo1", "coll-restore-1", "snap-123", now)
	if err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}

	// Insert FAILED restore test run
	_, err = db.Exec(`
		INSERT INTO restore_test_runs (id, started_at, samples, passed, failed, skipped, alerts)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "run1", now-86400, 10, 5, 5, 0, 1)
	if err != nil {
		t.Fatalf("insert restore test run: %v", err)
	}

	// Insert alert for failure
	_, err = db.Exec(`
		INSERT INTO restore_test_alerts (id, run_id, repo_id, snapshot_id, rel_path, reason, created_at, resolved)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "alert1", "run1", "repo1", "snap-123", "test.txt", "snapshot_not_found", now, 0)
	if err != nil {
		t.Fatalf("insert restore test alert: %v", err)
	}
}

func seedMaintenanceData(t *testing.T, db *sql.DB) {
	now := time.Now().Unix()

	// Insert pool
	_, err := db.Exec(`
		INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "pool1", "Test Pool", "node1", "warm", "single", "single", "/mnt/pool1", 1000000, 500000, 0, "", "active", now, now)
	if err != nil {
		t.Fatalf("insert pool: %v", err)
	}

	// Insert collection (use unique ID to avoid conflict with restore test seed)
	_, err = db.Exec(`
		INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "coll-maint-1", "Test Collection", "pool1", "/data", "disk1", 0, now, now)
	if err != nil {
		t.Fatalf("insert collection: %v", err)
	}

	// Insert scrub_state with successful run
	_, err = db.Exec(`
		INSERT INTO scrub_state (collection_id, last_run_at, verified_total)
		VALUES (?, ?, ?)
	`, "coll-maint-1", now-86400, 100)
	if err != nil {
		t.Fatalf("insert scrub_state: %v", err)
	}
}

func countComposeRuns(t *testing.T, db *sql.DB) int {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM compose_runs`).Scan(&count)
	if err != nil {
		t.Fatalf("count compose runs: %v", err)
	}
	return count
}

func countRestoreTestRuns(t *testing.T, db *sql.DB) int {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM restore_test_runs`).Scan(&count)
	if err != nil {
		t.Fatalf("count restore test runs: %v", err)
	}
	return count
}