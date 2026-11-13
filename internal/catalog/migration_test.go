package catalog

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/storage"
	_ "modernc.org/sqlite"
)

// setupMigrationTestDB creates an in-memory DB at schema v22 with pools,
// collections, and tier tables needed for migration tests.
func setupMigrationTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Create pools: warm (source) and cold (target)
	warmPool := &PoolRow{
		ID:         "pool-warm",
		Name:       "warm-pool",
		NodeID:     storage.DefaultNodeID,
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "local",
		Mountpoint: "",
		SizeBytes:  1000000000000,
		UsedBytes:  100000000000,
		Encrypted:  false,
		Status:     "active",
		CreatedAt:  time.Unix(1700000000, 0),
		UpdatedAt:  time.Unix(1700000000, 0),
	}
	coldPool := &PoolRow{
		ID:         "pool-cold",
		Name:       "cold-pool",
		NodeID:     storage.DefaultNodeID,
		Tier:       "cold",
		Mechanism:  "single",
		Driver:     "local",
		Mountpoint: "",
		SizeBytes:  2000000000000,
		UsedBytes:  500000000000,
		Encrypted:  false,
		Status:     "active",
		CreatedAt:  time.Unix(1700000000, 0),
		UpdatedAt:  time.Unix(1700000000, 0),
	}
	if err := CreatePool(db, warmPool, []string{"sda"}); err != nil {
		t.Fatalf("create warm pool: %v", err)
	}
	if err := CreatePool(db, coldPool, []string{"sdb"}); err != nil {
		t.Fatalf("create cold pool: %v", err)
	}

	// Create collections on warm pool
	coll := &CollectionRow{
		ID:             "coll-1",
		Name:           "photos",
		PoolID:         "pool-warm",
		Path:           "photos",
		DiskName:       "sda",
		SizeBytes:      0,
		CreatedAt:      time.Unix(1700000000, 0),
		UpdatedAt:      time.Unix(1700000000, 0),
	}
	if err := CreateCollection(db, coll); err != nil {
		t.Fatalf("create collection: %v", err)
	}

	// Create tier policies
	if _, err := db.Exec(`INSERT INTO tier_policies (pool_id, mode, cold_threshold_days, auto_move_wait_days) VALUES (?, 'recommend_only', 21, 7)`, "pool-warm"); err != nil {
		t.Fatalf("insert warm policy: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO tier_policies (pool_id, mode, cold_threshold_days, auto_move_wait_days) VALUES (?, 'recommend_only', 21, 7)`, "pool-cold"); err != nil {
		t.Fatalf("insert cold policy: %v", err)
	}

	return db
}

// setupMigrationTestFS creates temp dirs for source and target pool mountpoints
// and returns them. The caller must clean up.
func setupMigrationTestFS(t *testing.T) (srcRoot, dstRoot string) {
	t.Helper()
	tmpDir := t.TempDir()

	// Source: warm pool mountpoint + collection path
	srcRoot = filepath.Join(tmpDir, "mnt", "warm-pool", "photos")
	if err := os.MkdirAll(filepath.Join(srcRoot, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir src root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "a.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "sub", "b.txt"), []byte("nested file"), 0o644); err != nil {
		t.Fatalf("write sub/b.txt: %v", err)
	}

	// Target: cold pool mountpoint + collection path (empty)
	dstRoot = filepath.Join(tmpDir, "mnt", "cold-pool", "photos")
	if err := os.MkdirAll(dstRoot, 0o755); err != nil {
		t.Fatalf("mkdir dst root: %v", err)
	}

	// Update pool mountpoints in DB
	return srcRoot, dstRoot
}

// TestExecuteMigrationHappyPath tests the full execute flow: copy lands,
// manifest verifies, pointer switched, source retained, status completed,
// retain_until = executed + 7 days. This is the deadlock regression test.
func TestExecuteMigrationHappyPath(t *testing.T) {
	db := setupMigrationTestDB(t)

	srcRoot, dstRoot := setupMigrationTestFS(t)
	t.Cleanup(func() {
		os.RemoveAll(filepath.Dir(filepath.Dir(srcRoot)))
	})

	// Update pool mountpoints
	// srcRoot = tmpDir/mnt/warm-pool/photos, so mountpoint = tmpDir/mnt/warm-pool (one Dir)
	// dstRoot = tmpDir/mnt/cold-pool/photos, so mountpoint = tmpDir/mnt/cold-pool (one Dir)
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(srcRoot), "pool-warm"); err != nil {
		t.Fatalf("update warm mountpoint: %v", err)
	}
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(dstRoot), "pool-cold"); err != nil {
		t.Fatalf("update cold mountpoint: %v", err)
	}

	now := time.Unix(1700000000, 0)

	// Create a migration row directly (simulating CreateMigrationForRecommendation)
	migID := "mig-test-happy"
	if _, err := db.Exec(`
		INSERT INTO tier_migrations (id, collection_id, recommendation_id, from_pool_id, to_pool_id, direction, status, manifest, reason, created_at, executed_at, retain_until, finalized_at, undone_at)
		VALUES (?, ?, ?, ?, ?, ?, 'scheduled', '', '', ?, 0, 0, 0, 0)
	`, migID, "coll-1", "rec-1", "pool-warm", "pool-cold", "warm_to_cold", now.Unix()); err != nil {
		t.Fatalf("insert migration: %v", err)
	}

	// Execute migration
	err := ExecuteMigration(db, migID, now, DefaultCopyFunc)
	if err != nil {
		t.Fatalf("ExecuteMigration: %v", err)
	}

	// Verify status completed
	mig, err := GetMigration(db, migID)
	if err != nil || mig == nil {
		t.Fatalf("GetMigration: err=%v mig=%v", err, mig)
	}
	if mig.Status != MigrationStatusCompleted {
		t.Errorf("status: got %s, want %s", mig.Status, MigrationStatusCompleted)
	}
	if mig.ExecutedAt.IsZero() {
		t.Error("executed_at should be set")
	}
	expectedRetain := now.Unix() + int64(UndoWindowDays*24*3600)
	if mig.RetainUntil.Unix() != expectedRetain {
		t.Errorf("retain_until: got %d, want %d", mig.RetainUntil.Unix(), expectedRetain)
	}
	if mig.Manifest == "" {
		t.Error("manifest should be set")
	}

	// Verify pointer switched to target pool
	coll, err := GetCollection(db, "coll-1")
	if err != nil || coll == nil {
		t.Fatalf("GetCollection: err=%v coll=%v", err, coll)
	}
	if coll.PoolID != "pool-cold" {
		t.Errorf("collection pool_id: got %s, want pool-cold", coll.PoolID)
	}

	// Verify source tree retained (still exists on source)
	if _, err := os.Stat(filepath.Join(srcRoot, "a.txt")); os.IsNotExist(err) {
		t.Error("source tree should be retained (a.txt still exists)")
	}

	// Verify target tree exists (copy landed)
	if _, err := os.Stat(filepath.Join(dstRoot, "a.txt")); os.IsNotExist(err) {
		t.Error("target tree should exist after copy (a.txt on target)")
	}
}

// TestExecuteMigrationChecksumMismatch tests that checksum mismatch aborts:
// source untouched, pointer unswitched, status failed.
func TestExecuteMigrationChecksumMismatch(t *testing.T) {
	db := setupMigrationTestDB(t)

	srcRoot, dstRoot := setupMigrationTestFS(t)
	t.Cleanup(func() {
		os.RemoveAll(filepath.Dir(filepath.Dir(srcRoot)))
	})

	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(srcRoot), "pool-warm"); err != nil {
		t.Fatalf("update warm mountpoint: %v", err)
	}
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(dstRoot), "pool-cold"); err != nil {
		t.Fatalf("update cold mountpoint: %v", err)
	}

	now := time.Unix(1700000000, 0)

	// Create migration row
	migID := "mig-test-checksum"
	if _, err := db.Exec(`
		INSERT INTO tier_migrations (id, collection_id, recommendation_id, from_pool_id, to_pool_id, direction, status, manifest, reason, created_at, executed_at, retain_until, finalized_at, undone_at)
		VALUES (?, ?, ?, ?, ?, ?, 'scheduled', '', '', ?, 0, 0, 0, 0)
	`, migID, "coll-1", "rec-1", "pool-warm", "pool-cold", "warm_to_cold", now.Unix()); err != nil {
		t.Fatalf("insert migration: %v", err)
	}

	// Use a copyFunc that corrupts a file on the target
	copyFunc := func(src, dst string) error {
		if err := DefaultCopyFunc(src, dst); err != nil {
			return err
		}
		// Corrupt a file on the target to trigger checksum mismatch
		return os.WriteFile(filepath.Join(dst, "a.txt"), []byte("corrupted"), 0o644)
	}

	err := ExecuteMigration(db, migID, now, copyFunc)
	if err == nil {
		t.Fatal("expected error from ExecuteMigration, got nil")
	}

	// Verify status failed
	mig, err := GetMigration(db, migID)
	if err != nil || mig == nil {
		t.Fatalf("GetMigration: err=%v mig=%v", err, mig)
	}
	if mig.Status != MigrationStatusFailed {
		t.Errorf("status: got %s, want %s", mig.Status, MigrationStatusFailed)
	}

	// Verify pointer NOT switched (still on warm)
	coll, err := GetCollection(db, "coll-1")
	if err != nil || coll == nil {
		t.Fatalf("GetCollection: err=%v coll=%v", err, coll)
	}
	if coll.PoolID != "pool-warm" {
		t.Errorf("collection pool_id: got %s, want pool-warm (pointer unswitched)", coll.PoolID)
	}

	// Verify source untouched
	if _, err := os.Stat(filepath.Join(srcRoot, "a.txt")); os.IsNotExist(err) {
		t.Error("source tree should be untouched (a.txt still exists)")
	}
}

// TestExecuteMigrationPinnedCollection tests that a pinned collection is skipped.
func TestExecuteMigrationPinnedCollection(t *testing.T) {
	db := setupMigrationTestDB(t)

	srcRoot, dstRoot := setupMigrationTestFS(t)
	t.Cleanup(func() {
		os.RemoveAll(filepath.Dir(filepath.Dir(srcRoot)))
	})

	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(srcRoot), "pool-warm"); err != nil {
		t.Fatalf("update warm mountpoint: %v", err)
	}
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(dstRoot), "pool-cold"); err != nil {
		t.Fatalf("update cold mountpoint: %v", err)
	}

	now := time.Unix(1700000000, 0)

	// Pin the collection
	if _, err := db.Exec(`INSERT INTO tier_pins (collection_id, pinned_at, note) VALUES (?, ?, 'test pin')`, "coll-1", now.Unix()); err != nil {
		t.Fatalf("insert pin: %v", err)
	}

	// Create migration row
	migID := "mig-test-pinned"
	if _, err := db.Exec(`
		INSERT INTO tier_migrations (id, collection_id, recommendation_id, from_pool_id, to_pool_id, direction, status, manifest, reason, created_at, executed_at, retain_until, finalized_at, undone_at)
		VALUES (?, ?, ?, ?, ?, ?, 'scheduled', '', '', ?, 0, 0, 0, 0)
	`, migID, "coll-1", "rec-1", "pool-warm", "pool-cold", "warm_to_cold", now.Unix()); err != nil {
		t.Fatalf("insert migration: %v", err)
	}

	err := ExecuteMigration(db, migID, now, DefaultCopyFunc)
	if err != nil {
		t.Fatalf("ExecuteMigration: %v", err)
	}

	// Verify status skipped_pinned
	mig, err := GetMigration(db, migID)
	if err != nil || mig == nil {
		t.Fatalf("GetMigration: err=%v mig=%v", err, mig)
	}
	if mig.Status != MigrationStatusSkippedPinned {
		t.Errorf("status: got %s, want %s", mig.Status, MigrationStatusSkippedPinned)
	}

	// Verify pointer NOT switched
	coll, err := GetCollection(db, "coll-1")
	if err != nil || coll == nil {
		t.Fatalf("GetCollection: err=%v coll=%v", err, coll)
	}
	if coll.PoolID != "pool-warm" {
		t.Errorf("collection pool_id: got %s, want pool-warm (not switched for pinned)", coll.PoolID)
	}
}

// TestExecuteMigrationSecondLiveMigration tests that ExecuteMigration
// can be called directly even when a completed migration exists for the
// same collection (the duplicate check is in CreateMigrationForRecommendation).
// Since source files are retained, the copy succeeds and migration completes.
func TestExecuteMigrationSecondLiveMigration(t *testing.T) {
	db := setupMigrationTestDB(t)

	srcRoot, dstRoot := setupMigrationTestFS(t)
	t.Cleanup(func() {
		os.RemoveAll(filepath.Dir(filepath.Dir(srcRoot)))
	})

	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(srcRoot), "pool-warm"); err != nil {
		t.Fatalf("update warm mountpoint: %v", err)
	}
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(dstRoot), "pool-cold"); err != nil {
		t.Fatalf("update cold mountpoint: %v", err)
	}

	now := time.Unix(1700000000, 0)

	// Create first migration row (completed)
	if _, err := db.Exec(`
		INSERT INTO tier_migrations (id, collection_id, recommendation_id, from_pool_id, to_pool_id, direction, status, manifest, reason, created_at, executed_at, retain_until, finalized_at, undone_at)
		VALUES (?, ?, ?, ?, ?, ?, 'completed', '', '', ?, ?, ?, 0, 0)
	`, "mig-existing", "coll-1", "rec-1", "pool-warm", "pool-cold", "warm_to_cold", now.Unix(), now.Unix(), now.Unix()+int64(UndoWindowDays*24*3600)); err != nil {
		t.Fatalf("insert existing migration: %v", err)
	}

	// Switch collection pointer to cold (simulating completed migration)
	if _, err := db.Exec(`UPDATE collections SET pool_id = ?, updated_at = ? WHERE id = ?`, "pool-cold", now.Unix(), "coll-1"); err != nil {
		t.Fatalf("switch pointer to cold: %v", err)
	}

	// Create second migration row (scheduled) for same collection
	migID := "mig-test-second"
	if _, err := db.Exec(`
		INSERT INTO tier_migrations (id, collection_id, recommendation_id, from_pool_id, to_pool_id, direction, status, manifest, reason, created_at, executed_at, retain_until, finalized_at, undone_at)
		VALUES (?, ?, ?, ?, ?, ?, 'scheduled', '', '', ?, 0, 0, 0, 0)
	`, migID, "coll-1", "rec-2", "pool-warm", "pool-cold", "warm_to_cold", now.Unix()); err != nil {
		t.Fatalf("insert second migration: %v", err)
	}

	// Execute second migration - ExecuteMigration doesn't check for existing migrations
	// (that check is in CreateMigrationForRecommendation). Since source files are
	// retained on warm pool, the copy succeeds and migration completes.
	err := ExecuteMigration(db, migID, now, DefaultCopyFunc)
	if err != nil {
		t.Fatalf("ExecuteMigration: %v", err)
	}

	// Verify status completed
	mig, err := GetMigration(db, migID)
	if err != nil || mig == nil {
		t.Fatalf("GetMigration: err=%v mig=%v", err, mig)
	}
	if mig.Status != MigrationStatusCompleted {
		t.Errorf("status: got %s, want %s", mig.Status, MigrationStatusCompleted)
	}
	if mig.ExecutedAt.IsZero() {
		t.Error("executed_at should be set")
	}
	if mig.Manifest == "" {
		t.Error("manifest should be set")
	}

	// Verify pointer still on cold (no change)
	coll, err := GetCollection(db, "coll-1")
	if err != nil || coll == nil {
		t.Fatalf("GetCollection: err=%v coll=%v", err, coll)
	}
	if coll.PoolID != "pool-cold" {
		t.Errorf("collection pool_id: got %s, want pool-cold", coll.PoolID)
	}
}

// TestUndoMigrationWithinWindow tests undo within the retain window:
// pointer back, new copy deleted, status undone.
func TestUndoMigrationWithinWindow(t *testing.T) {
	db := setupMigrationTestDB(t)

	srcRoot, dstRoot := setupMigrationTestFS(t)
	t.Cleanup(func() {
		os.RemoveAll(filepath.Dir(filepath.Dir(srcRoot)))
	})

	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(srcRoot), "pool-warm"); err != nil {
		t.Fatalf("update warm mountpoint: %v", err)
	}
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(dstRoot), "pool-cold"); err != nil {
		t.Fatalf("update cold mountpoint: %v", err)
	}

	now := time.Unix(1700000000, 0)

	// First execute the migration
	migID := "mig-test-undo"
	if _, err := db.Exec(`
		INSERT INTO tier_migrations (id, collection_id, recommendation_id, from_pool_id, to_pool_id, direction, status, manifest, reason, created_at, executed_at, retain_until, finalized_at, undone_at)
		VALUES (?, ?, ?, ?, ?, ?, 'completed', '', '', ?, ?, ?, 0, 0)
	`, migID, "coll-1", "rec-1", "pool-warm", "pool-cold", "warm_to_cold", now.Unix(), now.Unix(), now.Unix()+int64(UndoWindowDays*24*3600)); err != nil {
		t.Fatalf("insert migration: %v", err)
	}

	// Copy source to target first (simulating what ExecuteMigration did)
	if err := DefaultCopyFunc(srcRoot, dstRoot); err != nil {
		t.Fatalf("copy source to target: %v", err)
	}

	// Switch pointer to cold (simulating what ExecuteMigration did)
	if _, err := db.Exec(`UPDATE collections SET pool_id = ?, updated_at = ? WHERE id = ?`, "pool-cold", now.Unix(), "coll-1"); err != nil {
		t.Fatalf("switch pointer to cold: %v", err)
	}

	// Undo within window (now is still within retain_until)
	undoNow := now.Add(time.Hour) // 1 hour later, still within 7-day window
	err := UndoMigration(db, migID, undoNow, DefaultDeleteFunc)
	if err != nil {
		t.Fatalf("UndoMigration: %v", err)
	}

	// Verify status undone
	mig, err := GetMigration(db, migID)
	if err != nil || mig == nil {
		t.Fatalf("GetMigration: err=%v mig=%v", err, mig)
	}
	if mig.Status != MigrationStatusUndone {
		t.Errorf("status: got %s, want %s", mig.Status, MigrationStatusUndone)
	}
	if mig.UndoneAt.IsZero() {
		t.Error("undone_at should be set")
	}

	// Verify pointer switched back to warm
	coll, err := GetCollection(db, "coll-1")
	if err != nil || coll == nil {
		t.Fatalf("GetCollection: err=%v coll=%v", err, coll)
	}
	if coll.PoolID != "pool-warm" {
		t.Errorf("collection pool_id: got %s, want pool-warm (pointer back)", coll.PoolID)
	}

	// Verify new copy deleted on target
	if _, err := os.Stat(filepath.Join(dstRoot, "a.txt")); !os.IsNotExist(err) {
		t.Error("target copy should be deleted after undo")
	}

	// Verify source still exists
	if _, err := os.Stat(filepath.Join(srcRoot, "a.txt")); os.IsNotExist(err) {
		t.Error("source tree should still exist after undo")
	}
}

// TestUndoMigrationAfterWindow tests that undo after the retain window returns an error.
func TestUndoMigrationAfterWindow(t *testing.T) {
	db := setupMigrationTestDB(t)

	srcRoot, dstRoot := setupMigrationTestFS(t)
	t.Cleanup(func() {
		os.RemoveAll(filepath.Dir(filepath.Dir(srcRoot)))
	})

	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(srcRoot), "pool-warm"); err != nil {
		t.Fatalf("update warm mountpoint: %v", err)
	}
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(dstRoot), "pool-cold"); err != nil {
		t.Fatalf("update cold mountpoint: %v", err)
	}

	now := time.Unix(1700000000, 0)

	// Create completed migration with retain_until in the past
	retainUntil := now.Unix() - 3600 // 1 hour ago
	if _, err := db.Exec(`
		INSERT INTO tier_migrations (id, collection_id, recommendation_id, from_pool_id, to_pool_id, direction, status, manifest, reason, created_at, executed_at, retain_until, finalized_at, undone_at)
		VALUES (?, ?, ?, ?, ?, ?, 'completed', '', '', ?, ?, ?, 0, 0)
	`, "mig-test-expired", "coll-1", "rec-1", "pool-warm", "pool-cold", "warm_to_cold", now.Unix(), now.Unix(), retainUntil); err != nil {
		t.Fatalf("insert migration: %v", err)
	}

	// Try to undo after window
	err := UndoMigration(db, "mig-test-expired", now.Add(time.Hour), DefaultDeleteFunc)
	if err == nil {
		t.Fatal("expected error for undo after window, got nil")
	}
}

// TestFinalizeExpiredMigrations tests that finalize deletes retained source
// trees and marks migrations finalized.
func TestFinalizeExpiredMigrations(t *testing.T) {
	db := setupMigrationTestDB(t)

	srcRoot, dstRoot := setupMigrationTestFS(t)
	t.Cleanup(func() {
		os.RemoveAll(filepath.Dir(filepath.Dir(srcRoot)))
	})

	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(srcRoot), "pool-warm"); err != nil {
		t.Fatalf("update warm mountpoint: %v", err)
	}
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(dstRoot), "pool-cold"); err != nil {
		t.Fatalf("update cold mountpoint: %v", err)
	}

	now := time.Unix(1700000000, 0)

	// Create completed migration with retain_until in the past
	retainUntil := now.Unix() - 3600 // 1 hour ago
	if _, err := db.Exec(`
		INSERT INTO tier_migrations (id, collection_id, recommendation_id, from_pool_id, to_pool_id, direction, status, manifest, reason, created_at, executed_at, retain_until, finalized_at, undone_at)
		VALUES (?, ?, ?, ?, ?, ?, 'completed', '', '', ?, ?, ?, 0, 0)
	`, "mig-test-finalize", "coll-1", "rec-1", "pool-warm", "pool-cold", "warm_to_cold", now.Unix(), now.Unix(), retainUntil); err != nil {
		t.Fatalf("insert migration: %v", err)
	}

	// Create source tree on warm pool (retained source)
	if err := os.MkdirAll(srcRoot, 0o755); err != nil {
		t.Fatalf("mkdir src root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "retained.txt"), []byte("retained data"), 0o644); err != nil {
		t.Fatalf("write retained.txt: %v", err)
	}

	// Finalize expired migrations
	count, err := FinalizeExpiredMigrations(db, now, DefaultDeleteFunc)
	if err != nil {
		t.Fatalf("FinalizeExpiredMigrations: %v", err)
	}
	if count != 1 {
		t.Errorf("finalized count: got %d, want 1", count)
	}

	// Verify status finalized
	mig, err := GetMigration(db, "mig-test-finalize")
	if err != nil || mig == nil {
		t.Fatalf("GetMigration: err=%v mig=%v", err, mig)
	}
	if mig.Status != MigrationStatusFinalized {
		t.Errorf("status: got %s, want %s", mig.Status, MigrationStatusFinalized)
	}
	if mig.FinalizedAt.IsZero() {
		t.Error("finalized_at should be set")
	}

	// Verify retained source deleted
	if _, err := os.Stat(filepath.Join(srcRoot, "retained.txt")); !os.IsNotExist(err) {
		t.Error("retained source should be deleted after finalize")
	}
}

// TestProcessSilentConsents tests silent-consent timing with fixed now.
func TestProcessSilentConsents(t *testing.T) {
	db := setupMigrationTestDB(t)

	srcRoot, dstRoot := setupMigrationTestFS(t)
	t.Cleanup(func() {
		os.RemoveAll(filepath.Dir(filepath.Dir(srcRoot)))
	})

	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(srcRoot), "pool-warm"); err != nil {
		t.Fatalf("update warm mountpoint: %v", err)
	}
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, filepath.Dir(dstRoot), "pool-cold"); err != nil {
		t.Fatalf("update cold mountpoint: %v", err)
	}

	now := time.Unix(1700000000, 0)

	// Create a recommendation that is pending, older than wait period, in auto_if_no_response pool
	// First, set the warm pool policy to auto_if_no_response with short wait
	if _, err := db.Exec(`UPDATE tier_policies SET mode = 'auto_if_no_response', auto_move_wait_days = 1 WHERE pool_id = 'pool-warm'`); err != nil {
		t.Fatalf("update warm policy: %v", err)
	}

	// Insert pending recommendation (created 2 days ago, past the 1-day wait)
	createdAt := now.Add(-48 * time.Hour).Unix()
	if _, err := db.Exec(`
		INSERT INTO tier_recommendations (id, collection_id, pool_id, direction, reason, size_bytes, last_access, score, status, created_at, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, 0)
	`, "rec-pending-old", "coll-1", "pool-warm", "warm_to_cold", "test", 1000, createdAt, 0.5, createdAt); err != nil {
		t.Fatalf("insert pending recommendation: %v", err)
	}

	// Insert pending recommendation (created 1 hour ago, NOT past the 1-day wait)
	createdAt2 := now.Add(-1 * time.Hour).Unix()
	if _, err := db.Exec(`
		INSERT INTO tier_recommendations (id, collection_id, pool_id, direction, reason, size_bytes, last_access, score, status, created_at, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, 0)
	`, "rec-pending-new", "coll-1", "pool-warm", "warm_to_cold", "test", 1000, createdAt2, 0.5, createdAt2); err != nil {
		t.Fatalf("insert pending recommendation 2: %v", err)
	}

	// Insert already-approved recommendation (should not create duplicate)
	createdAt3 := now.Add(-48 * time.Hour).Unix()
	if _, err := db.Exec(`
		INSERT INTO tier_recommendations (id, collection_id, pool_id, direction, reason, size_bytes, last_access, score, status, created_at, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'approved', ?, ?)
	`, "rec-approved", "coll-1", "pool-warm", "warm_to_cold", "test", 1000, createdAt3, 0.5, createdAt3, now.Unix()); err != nil {
		t.Fatalf("insert approved recommendation: %v", err)
	}

	// Run ProcessSilentConsents
	created, err := ProcessSilentConsents(db, now)
	if err != nil {
		t.Fatalf("ProcessSilentConsents: %v", err)
	}
	if created != 1 {
		t.Errorf("created count: got %d, want 1 (only the old pending rec)", created)
	}

	// Verify the old pending rec now has a scheduled migration
	migs, _, err := ListMigrations(db, MigrationStatusScheduled, "")
	if err != nil {
		t.Fatalf("ListMigrations: %v", err)
	}
	if len(migs) != 1 {
		t.Errorf("scheduled migrations: got %d, want 1", len(migs))
	}
	if migs[0].RecommendationID != "rec-pending-old" {
		t.Errorf("migration recommendation_id: got %s, want rec-pending-old", migs[0].RecommendationID)
	}
}