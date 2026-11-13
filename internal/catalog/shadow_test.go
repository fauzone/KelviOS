package catalog

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// setupShadowDB opens an in-memory catalog at schema v23 with one warm pool,
// one cold pool, one collection on cold, and one indexed file.
func setupShadowDB(t *testing.T) (*sql.DB, string, string, string) {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	now := time.Now().Unix()

	// Warm pool (target for shadows)
	if _, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES ('pool-warm', 'warm', 'local', 'warm', 'single', 'single', '/mnt/warm', 1000000000, 100000000, 0, '', 'active', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert warm pool: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pool_disks (pool_id, disk_name, created_at) VALUES ('pool-warm', 'sda', ?)`, now); err != nil {
		t.Fatalf("insert pool_disk warm: %v", err)
	}

	// Cold pool (source of files)
	if _, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES ('pool-cold', 'cold', 'local', 'cold', 'single', 'single', '/mnt/cold', 1000000000, 500000000, 0, '', 'active', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert cold pool: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pool_disks (pool_id, disk_name, created_at) VALUES ('pool-cold', 'sdb', ?)`, now); err != nil {
		t.Fatalf("insert pool_disk cold: %v", err)
	}

	// Collection on cold pool
	if _, err := db.Exec(`INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, last_accessed_at, created_at, updated_at) VALUES ('coll-1', 'photos', 'pool-cold', '/photos', 'sdb', 0, ?, ?, ?)`, now, now, now); err != nil {
		t.Fatalf("insert collection: %v", err)
	}

	// Indexed file on cold pool
	if _, err := db.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, created_at, updated_at) VALUES ('file-1', 'coll-1', 'a.txt', 100, ?, 'sdb', ?, ?)`, now, now, now); err != nil {
		t.Fatalf("insert file: %v", err)
	}

	return db, "coll-1", "a.txt", "pool-warm"
}

// writeFixture creates a temp file with given content and returns its path.
func writeFixture(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "shadow-fixture-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	f.Close()
	return f.Name()
}

func TestCreateShadowHappyPath(t *testing.T) {
	db, collID, relPath, warmPoolID := setupShadowDB(t)
	defer db.Close()

	// Canonical file on disk
	canonicalPath := writeFixture(t, "shadow canonical content")
	defer os.Remove(canonicalPath)

	// Shadow destination
	tmpDir := t.TempDir()
	shadowPath := filepath.Join(tmpDir, ".kelvios-shadow", collID, relPath)

	copyCalled := false
	copyFunc := func(src, dst string) error {
		copyCalled = true
		return DefaultShadowCopyFunc(src, dst)
	}

	row, err := CreateShadow(db, collID, relPath, warmPoolID, collID+"/"+relPath, canonicalPath, shadowPath, copyFunc, "test-actor")
	if err != nil {
		t.Fatalf("CreateShadow: %v", err)
	}
	if row == nil {
		t.Fatal("CreateShadow returned nil row")
	}
	if row.CollectionID != collID {
		t.Errorf("collection_id: got %s, want %s", row.CollectionID, collID)
	}
	if row.RelPath != relPath {
		t.Errorf("rel_path: got %s, want %s", row.RelPath, relPath)
	}
	if row.ShadowPoolID != warmPoolID {
		t.Errorf("shadow_pool_id: got %s, want %s", row.ShadowPoolID, warmPoolID)
	}
	if row.SHA256 == "" {
		t.Error("sha256 is empty")
	}
	if row.SizeBytes <= 0 {
		t.Errorf("size_bytes: got %d, want > 0", row.SizeBytes)
	}
	if row.CreatedAt.IsZero() {
		t.Error("created_at is zero")
	}
	if !row.LastServedAt.IsZero() {
		t.Errorf("last_served_at: got %v, want zero", row.LastServedAt)
	}
	if !copyCalled {
		t.Error("copyFunc was not called")
	}

	// Verify shadow file exists on disk and content matches
	if _, err := os.Stat(shadowPath); os.IsNotExist(err) {
		t.Errorf("shadow file not created on disk: %s", shadowPath)
	}
	shadowSHA, shadowSize, err := hashFile(shadowPath)
	if err != nil {
		t.Fatalf("hash shadow file: %v", err)
	}
	canonicalSHA, _, err := hashFile(canonicalPath)
	if err != nil {
		t.Fatalf("hash canonical file: %v", err)
	}
	if shadowSHA != canonicalSHA {
		t.Errorf("shadow sha256 %s != canonical sha256 %s", shadowSHA, canonicalSHA)
	}
	if shadowSize != row.SizeBytes {
		t.Errorf("shadow size %d != row size %d", shadowSize, row.SizeBytes)
	}
}

func TestCreateShadowChecksumMismatch(t *testing.T) {
	db, collID, relPath, warmPoolID := setupShadowDB(t)
	defer db.Close()

	canonicalPath := writeFixture(t, "canonical content")
	defer os.Remove(canonicalPath)

	tmpDir := t.TempDir()
	shadowPath := filepath.Join(tmpDir, ".kelvios-shadow", collID, relPath)

	// copyFunc writes wrong bytes (different content)
	copyFunc := func(src, dst string) error {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, []byte("wrong content"), 0o644)
	}

	_, err := CreateShadow(db, collID, relPath, warmPoolID, collID+"/"+relPath, canonicalPath, shadowPath, copyFunc, "test-actor")
	if err == nil {
		t.Fatal("expected error for checksum mismatch, got nil")
	}
	if !errors.Is(err, nil) && err.Error() != "checksum mismatch" {
		t.Errorf("expected 'checksum mismatch' error, got %v", err)
	}

	// Shadow file should be cleaned up
	if _, err := os.Stat(shadowPath); !os.IsNotExist(err) {
		t.Errorf("shadow file should be removed after checksum mismatch, still exists: %s", shadowPath)
	}

	// No row should exist
	shadow, err := GetShadow(db, collID, relPath)
	if err != nil {
		t.Fatalf("GetShadow: %v", err)
	}
	if shadow != nil {
		t.Error("expected no shadow row after checksum mismatch, got one")
	}
}

func TestCreateShadowCopyFuncError(t *testing.T) {
	db, collID, relPath, warmPoolID := setupShadowDB(t)
	defer db.Close()

	canonicalPath := writeFixture(t, "canonical content")
	defer os.Remove(canonicalPath)

	tmpDir := t.TempDir()
	shadowPath := filepath.Join(tmpDir, ".kelvios-shadow", collID, relPath)

	// copyFunc always fails
	copyFunc := func(src, dst string) error {
		return errors.New("copy failed")
	}

	_, err := CreateShadow(db, collID, relPath, warmPoolID, collID+"/"+relPath, canonicalPath, shadowPath, copyFunc, "test-actor")
	if err == nil {
		t.Fatal("expected error for copyFunc failure, got nil")
	}
}

func TestGetShadowFoundAndMissing(t *testing.T) {
	db, collID, relPath, warmPoolID := setupShadowDB(t)
	defer db.Close()

	// Missing shadow
	shadow, err := GetShadow(db, collID, relPath)
	if err != nil {
		t.Fatalf("GetShadow missing: %v", err)
	}
	if shadow != nil {
		t.Error("expected nil for missing shadow, got one")
	}

	// Create a shadow
	canonicalPath := writeFixture(t, "get shadow content")
	defer os.Remove(canonicalPath)

	tmpDir := t.TempDir()
	shadowPath := filepath.Join(tmpDir, ".kelvios-shadow", collID, relPath)

	row, err := CreateShadow(db, collID, relPath, warmPoolID, collID+"/"+relPath, canonicalPath, shadowPath, DefaultShadowCopyFunc, "test-actor")
	if err != nil {
		t.Fatalf("CreateShadow: %v", err)
	}

	// GetShadow should find it
	shadow, err = GetShadow(db, collID, relPath)
	if err != nil {
		t.Fatalf("GetShadow found: %v", err)
	}
	if shadow == nil {
		t.Fatal("expected shadow row, got nil")
	}
	if shadow.ID != row.ID {
		t.Errorf("shadow ID: got %s, want %s", shadow.ID, row.ID)
	}
	if shadow.SHA256 != row.SHA256 {
		t.Errorf("shadow SHA256 mismatch")
	}
	if shadow.SizeBytes != row.SizeBytes {
		t.Errorf("shadow SizeBytes mismatch")
	}
}

func TestInvalidateShadow(t *testing.T) {
	db, collID, relPath, warmPoolID := setupShadowDB(t)
	defer db.Close()

	// Use a temp dir that matches the pool mountpoint
	tmpDir := t.TempDir()
	warmMount := filepath.Join(tmpDir, "mnt", "warm")
	if err := os.MkdirAll(warmMount, 0o755); err != nil {
		t.Fatalf("mkdir warm mount: %v", err)
	}
	// Update pool mountpoint in DB to match temp dir
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, warmMount, warmPoolID); err != nil {
		t.Fatalf("update pool mountpoint: %v", err)
	}

	canonicalPath := writeFixture(t, "invalidate content")
	defer os.Remove(canonicalPath)

	shadowPath := filepath.Join(warmMount, ".kelvios-shadow", collID, relPath)

	_, err := CreateShadow(db, collID, relPath, warmPoolID, collID+"/"+relPath, canonicalPath, shadowPath, DefaultShadowCopyFunc, "test-actor")
	if err != nil {
		t.Fatalf("CreateShadow: %v", err)
	}

	// Verify shadow file exists
	if _, err := os.Stat(shadowPath); os.IsNotExist(err) {
		t.Fatalf("shadow file not created: %s", shadowPath)
	}

	// Invalidate
	if err := InvalidateShadow(db, collID, relPath, "test-actor"); err != nil {
		t.Fatalf("InvalidateShadow: %v", err)
	}

	// Row should be gone
	shadow, err := GetShadow(db, collID, relPath)
	if err != nil {
		t.Fatalf("GetShadow after invalidate: %v", err)
	}
	if shadow != nil {
		t.Error("expected nil shadow after invalidate, got one")
	}

	// Shadow file should be removed
	if _, err := os.Stat(shadowPath); !os.IsNotExist(err) {
		t.Errorf("shadow file should be removed after invalidate, still exists: %s", shadowPath)
	}

	// Invalidate missing -> nil error (idempotent)
	if err := InvalidateShadow(db, collID, relPath, "test-actor"); err != nil {
		t.Errorf("InvalidateShadow missing: expected nil, got %v", err)
	}
}

func TestCountRecentAccesses(t *testing.T) {
	db, collID, relPath, _ := setupShadowDB(t)
	defer db.Close()

	now := time.Now().Unix()

	// Insert a recent access event (within 14 days)
	recent := now - 3600 // 1 hour ago
	if _, err := db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES ('acc-recent', ?, ?, 'test', ?, 'content')`, collID, relPath, recent); err != nil {
		t.Fatalf("insert recent access: %v", err)
	}

	// Insert an old access event (outside 14 days)
	old := now - 15*24*3600 // 15 days ago
	if _, err := db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES ('acc-old', ?, ?, 'test', ?, 'content')`, collID, relPath, old); err != nil {
		t.Fatalf("insert old access: %v", err)
	}

	// Count with default 14-day window
	count, err := CountRecentAccesses(db, collID, relPath, 14)
	if err != nil {
		t.Fatalf("CountRecentAccesses: %v", err)
	}
	if count != 1 {
		t.Errorf("count recent accesses: got %d, want 1 (old event excluded)", count)
	}

	// Count with 30-day window should include both
	count30, err := CountRecentAccesses(db, collID, relPath, 30)
	if err != nil {
		t.Fatalf("CountRecentAccesses 30d: %v", err)
	}
	if count30 != 2 {
		t.Errorf("count 30-day accesses: got %d, want 2", count30)
	}

	// No accesses for different file
	countNone, err := CountRecentAccesses(db, collID, "nonexistent.txt", 14)
	if err != nil {
		t.Fatalf("CountRecentAccesses nonexistent: %v", err)
	}
	if countNone != 0 {
		t.Errorf("count nonexistent: got %d, want 0", countNone)
	}
}

func TestPickWarmShadowPool(t *testing.T) {
	db, _, _, _ := setupShadowDB(t)
	defer db.Close()

	// Add a second warm pool with more free space
	now := time.Now().Unix()
	if _, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES ('pool-warm-2', 'warm2', 'local', 'warm', 'single', 'single', '/mnt/warm2', 2000000000, 100000000, 0, '', 'active', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert warm pool 2: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pool_disks (pool_id, disk_name, created_at) VALUES ('pool-warm-2', 'sdc', ?)`, now); err != nil {
		t.Fatalf("insert pool_disk warm2: %v", err)
	}

	// pool-warm: free = 1000000000 - 100000000 = 900000000
	// pool-warm-2: free = 2000000000 - 100000000 = 1900000000 (most free)
	best, err := PickWarmShadowPool(db)
	if err != nil {
		t.Fatalf("PickWarmShadowPool: %v", err)
	}
	if best.ID != "pool-warm-2" {
		t.Errorf("picked pool %s, want pool-warm-2 (most free)", best.ID)
	}

	// Add a hot pool - should never be picked
	if _, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES ('pool-hot', 'hot', 'local', 'hot', 'single', 'single', '/mnt/hot', 5000000000, 0, 0, '', 'active', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert hot pool: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pool_disks (pool_id, disk_name, created_at) VALUES ('pool-hot', 'sdd', ?)`, now); err != nil {
		t.Fatalf("insert pool_disk hot: %v", err)
	}

	best, err = PickWarmShadowPool(db)
	if err != nil {
		t.Fatalf("PickWarmShadowPool with hot: %v", err)
	}
	if best.ID != "pool-warm-2" {
		t.Errorf("picked pool %s with hot present, want pool-warm-2", best.ID)
	}

	// No warm pools -> error
	db2, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db2.Close()
	if err := Migrate(db2); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, err = PickWarmShadowPool(db2)
	if err == nil {
		t.Error("expected error for no warm pool, got nil")
	}
}

func TestUpdateShadowLastServed(t *testing.T) {
	db, collID, relPath, warmPoolID := setupShadowDB(t)
	defer db.Close()

	canonicalPath := writeFixture(t, "last served content")
	defer os.Remove(canonicalPath)

	tmpDir := t.TempDir()
	shadowPath := filepath.Join(tmpDir, ".kelvios-shadow", collID, relPath)

	row, err := CreateShadow(db, collID, relPath, warmPoolID, collID+"/"+relPath, canonicalPath, shadowPath, DefaultShadowCopyFunc, "test-actor")
	if err != nil {
		t.Fatalf("CreateShadow: %v", err)
	}
	if !row.LastServedAt.IsZero() {
		t.Error("last_served_at should be zero initially")
	}

	UpdateShadowLastServed(db, collID, relPath)

	shadow, err := GetShadow(db, collID, relPath)
	if err != nil {
		t.Fatalf("GetShadow: %v", err)
	}
	if shadow == nil {
		t.Fatal("expected shadow row, got nil")
	}
	if shadow.LastServedAt.IsZero() {
		t.Error("last_served_at should be set after UpdateShadowLastServed")
	}
}

// hexSHA256 computes SHA-256 hex of a string.
func hexSHA256(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

var _ = io.Copy
var _ = hexSHA256

// ============================================================================
// WS36 (FR-2.6): Dirty write-back shadow tests
// ============================================================================

// setupDirtyShadowDB opens an in-memory catalog at schema v25 with one warm pool,
// one cold pool, one collection on cold, one indexed file, and a pre-existing shadow.
func setupDirtyShadowDB(t *testing.T) (*sql.DB, string, string, string, string) {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	now := time.Now().Unix()

	// Warm pool (target for shadows)
	if _, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES ('pool-warm', 'warm', 'local', 'warm', 'single', 'single', '/mnt/warm', 1000000000, 100000000, 0, '', 'active', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert warm pool: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pool_disks (pool_id, disk_name, created_at) VALUES ('pool-warm', 'sda', ?)`, now); err != nil {
		t.Fatalf("insert pool_disk warm: %v", err)
	}

	// Cold pool (source of files)
	if _, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES ('pool-cold', 'cold', 'local', 'cold', 'single', 'single', '/mnt/cold', 1000000000, 500000000, 0, '', 'active', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert cold pool: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pool_disks (pool_id, disk_name, created_at) VALUES ('pool-cold', 'sdb', ?)`, now); err != nil {
		t.Fatalf("insert pool_disk cold: %v", err)
	}

	// Collection on cold pool
	if _, err := db.Exec(`INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, last_accessed_at, created_at, updated_at) VALUES ('coll-1', 'photos', 'pool-cold', '/photos', 'sdb', 0, ?, ?, ?)`, now, now, now); err != nil {
		t.Fatalf("insert collection: %v", err)
	}

	// Indexed file on cold pool
	if _, err := db.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, created_at, updated_at) VALUES ('file-1', 'coll-1', 'a.txt', 100, ?, 'sdb', ?, ?)`, now, now, now); err != nil {
		t.Fatalf("insert file: %v", err)
	}

	// Create a shadow for the file
	tmpDir := t.TempDir()
	warmMount := filepath.Join(tmpDir, "mnt", "warm")
	coldMount := filepath.Join(tmpDir, "mnt", "cold")
	if err := os.MkdirAll(filepath.Join(warmMount, ".kelvios-shadow", "coll-1"), 0o755); err != nil {
		t.Fatalf("mkdir shadow dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(coldMount, "photos"), 0o755); err != nil {
		t.Fatalf("mkdir cold photos: %v", err)
	}
	// Update pool mountpoints to match temp dirs
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, warmMount, "pool-warm"); err != nil {
		t.Fatalf("update warm pool mountpoint: %v", err)
	}
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, coldMount, "pool-cold"); err != nil {
		t.Fatalf("update cold pool mountpoint: %v", err)
	}
	// Create canonical file in cold mount
	canonicalPath := filepath.Join(coldMount, "photos", "a.txt")
	if err := os.WriteFile(canonicalPath, []byte("original canonical content"), 0o644); err != nil {
		t.Fatalf("write canonical file: %v", err)
	}
	shadowPath := filepath.Join(warmMount, ".kelvios-shadow", "coll-1", "a.txt")
	_, err = CreateShadow(db, "coll-1", "a.txt", "pool-warm", "coll-1/a.txt", canonicalPath, shadowPath, DefaultShadowCopyFunc, "test-actor")
	if err != nil {
		t.Fatalf("CreateShadow: %v", err)
	}

	return db, "coll-1", "a.txt", "pool-warm", warmMount
}

func TestWriteDirtyShadowHappyPath(t *testing.T) {
	db, collID, relPath, warmPoolID, warmMount := setupDirtyShadowDB(t)
	defer db.Close()

	// Get shadow before write
	shadowBefore, err := GetShadow(db, collID, relPath)
	if err != nil {
		t.Fatalf("GetShadow before: %v", err)
	}
	if shadowBefore == nil {
		t.Fatal("expected shadow before write")
	}
	if shadowBefore.Dirty != 0 {
		t.Errorf("shadow dirty before write: got %d, want 0", shadowBefore.Dirty)
	}
	if shadowBefore.Version != 1 {
		t.Errorf("shadow version before write: got %d, want 1", shadowBefore.Version)
	}
	if !shadowBefore.DirtyAt.IsZero() {
		t.Error("dirty_at should be zero before write")
	}

	// Write dirty shadow
	newContent := []byte("dirty write content")
	shadow, err := WriteDirtyShadow(db, collID, relPath, warmPoolID, "coll-1/"+relPath, newContent, "test-actor")
	if err != nil {
		t.Fatalf("WriteDirtyShadow: %v", err)
	}
	if shadow == nil {
		t.Fatal("WriteDirtyShadow returned nil")
	}

	// Verify returned row
	if shadow.Dirty != 1 {
		t.Errorf("returned shadow dirty: got %d, want 1", shadow.Dirty)
	}
	if shadow.Version != 2 {
		t.Errorf("returned shadow version: got %d, want 2", shadow.Version)
	}
	if shadow.DirtyAt.IsZero() {
		t.Error("returned shadow dirty_at should be set")
	}
	if shadow.SHA256 == "" {
		t.Error("returned shadow sha256 empty")
	}
	if shadow.SizeBytes != int64(len(newContent)) {
		t.Errorf("returned shadow size: got %d, want %d", shadow.SizeBytes, len(newContent))
	}

	// Verify shadow file on disk has new content
	shadowPath := filepath.Join(warmMount, ".kelvios-shadow", "coll-1", relPath)
	shadowData, err := os.ReadFile(shadowPath)
	if err != nil {
		t.Fatalf("read shadow file: %v", err)
	}
	if string(shadowData) != string(newContent) {
		t.Errorf("shadow file content: got %q, want %q", string(shadowData), string(newContent))
	}

	// Verify canonical file is UNCHANGED
	canonicalPath := filepath.Join(warmMount, "..", "cold", "photos", relPath)
	canonicalPath = filepath.Clean(canonicalPath)
	canonicalData, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("read canonical file: %v", err)
	}
	if string(canonicalData) != "original canonical content" {
		t.Errorf("canonical file changed unexpectedly: got %q", string(canonicalData))
	}

	// Verify DB row matches
	shadowAfter, err := GetShadow(db, collID, relPath)
	if err != nil {
		t.Fatalf("GetShadow after: %v", err)
	}
	if shadowAfter.Dirty != 1 {
		t.Errorf("DB shadow dirty: got %d, want 1", shadowAfter.Dirty)
	}
	if shadowAfter.Version != 2 {
		t.Errorf("DB shadow version: got %d, want 2", shadowAfter.Version)
	}
	if shadowAfter.DirtyAt.IsZero() {
		t.Error("DB shadow dirty_at should be set")
	}
	if shadowAfter.SHA256 != shadow.SHA256 {
		t.Errorf("DB shadow sha256 mismatch")
	}
	if shadowAfter.SizeBytes != shadow.SizeBytes {
		t.Errorf("DB shadow size mismatch")
	}
}

func TestFlushDirtyShadowsHappyPath(t *testing.T) {
	db, collID, relPath, warmPoolID, warmMount := setupDirtyShadowDB(t)
	defer db.Close()

	// First make the shadow dirty
	newContent := []byte("flushed content")
	_, err := WriteDirtyShadow(db, collID, relPath, warmPoolID, "coll-1/"+relPath, newContent, "test-actor")
	if err != nil {
		t.Fatalf("WriteDirtyShadow: %v", err)
	}

	// Create canonical directory structure
	coldMount := filepath.Join(warmMount, "..", "cold")
	if err := os.MkdirAll(filepath.Join(coldMount, "photos"), 0o755); err != nil {
		t.Fatalf("mkdir cold: %v", err)
	}
	canonicalPath := filepath.Join(coldMount, "photos", relPath)
	if err := os.WriteFile(canonicalPath, []byte("original canonical content"), 0o644); err != nil {
		t.Fatalf("write canonical: %v", err)
	}

	// Flush dirty shadows for the warm pool
	flushed, failed, err := FlushDirtyShadows(db, warmPoolID, "test-actor")
	if err != nil {
		t.Fatalf("FlushDirtyShadows: %v", err)
	}
	if flushed != 1 {
		t.Errorf("flushed count: got %d, want 1", flushed)
	}
	if failed != 0 {
		t.Errorf("failed count: got %d, want 0", failed)
	}

	// Verify canonical file now has shadow content
	canonicalData, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("read canonical after flush: %v", err)
	}
	if string(canonicalData) != string(newContent) {
		t.Errorf("canonical after flush: got %q, want %q", string(canonicalData), string(newContent))
	}

	// Verify shadow row is clean
	shadow, err := GetShadow(db, collID, relPath)
	if err != nil {
		t.Fatalf("GetShadow after flush: %v", err)
	}
	if shadow.Dirty != 0 {
		t.Errorf("shadow dirty after flush: got %d, want 0", shadow.Dirty)
	}
	if !shadow.DirtyAt.IsZero() {
		t.Error("dirty_at should be zero after flush")
	}
	// Version stays at the newest version number (2)
	if shadow.Version != 2 {
		t.Errorf("shadow version after flush: got %d, want 2", shadow.Version)
	}
}

func TestFlushDirtyShadowsChecksumMismatch(t *testing.T) {
	db, collID, relPath, warmPoolID, warmMount := setupDirtyShadowDB(t)
	defer db.Close()

	// Make the shadow dirty
	newContent := []byte("flushed content")
	_, err := WriteDirtyShadow(db, collID, relPath, warmPoolID, "coll-1/"+relPath, newContent, "test-actor")
	if err != nil {
		t.Fatalf("WriteDirtyShadow: %v", err)
	}

	// Create canonical directory structure
	coldMount := filepath.Join(warmMount, "..", "cold")
	if err := os.MkdirAll(filepath.Join(coldMount, "photos"), 0o755); err != nil {
		t.Fatalf("mkdir cold: %v", err)
	}
	canonicalPath := filepath.Join(coldMount, "photos", relPath)
	if err := os.WriteFile(canonicalPath, []byte("original canonical content"), 0o644); err != nil {
		t.Fatalf("write canonical: %v", err)
	}

	// Corrupt the shadow file on disk (simulate bit rot / partial write)
	shadowPath := filepath.Join(warmMount, ".kelvios-shadow", "coll-1", relPath)
	if err := os.WriteFile(shadowPath, []byte("corrupted content"), 0o644); err != nil {
		t.Fatalf("corrupt shadow file: %v", err)
	}

	// Flush should fail checksum verification
	flushed, failed, err := FlushDirtyShadows(db, warmPoolID, "test-actor")
	if err != nil {
		t.Fatalf("FlushDirtyShadows: %v", err)
	}
	if flushed != 0 {
		t.Errorf("flushed count: got %d, want 0", flushed)
	}
	if failed != 1 {
		t.Errorf("failed count: got %d, want 1", failed)
	}

	// Verify canonical file is UNCHANGED
	canonicalData, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("read canonical after failed flush: %v", err)
	}
	if string(canonicalData) != "original canonical content" {
		t.Errorf("canonical changed after failed flush: got %q", string(canonicalData))
	}

	// Verify shadow row is STILL dirty
	shadow, err := GetShadow(db, collID, relPath)
	if err != nil {
		t.Fatalf("GetShadow after failed flush: %v", err)
	}
	if shadow.Dirty != 1 {
		t.Errorf("shadow dirty after failed flush: got %d, want 1", shadow.Dirty)
	}
	if shadow.DirtyAt.IsZero() {
		t.Error("dirty_at should still be set after failed flush")
	}
}

func TestListDirtyShadowsOlderThanBoundary(t *testing.T) {
	db, collID, relPath, warmPoolID, _ := setupDirtyShadowDB(t)
	defer db.Close()

	// Make the shadow dirty
	_, err := WriteDirtyShadow(db, collID, relPath, warmPoolID, "coll-1/"+relPath, []byte("dirty content"), "test-actor")
	if err != nil {
		t.Fatalf("WriteDirtyShadow: %v", err)
	}

	// Add a second file + shadow, make it dirty with an OLD dirty_at (80 hours ago)
	now := time.Now().Unix()
	oldTime := now - 80*3600
	if _, err := db.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, created_at, updated_at) VALUES ('file-2', 'coll-1', 'b.txt', 100, ?, 'sdb', ?, ?)`, now, now, now); err != nil {
		t.Fatalf("insert file 2: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO file_shadows (id, file_id, collection_id, rel_path, shadow_pool_id, shadow_rel_path, sha256, size_bytes, created_at, last_served_at, dirty, version, dirty_at) VALUES ('shdw-old', 'file-2', 'coll-1', 'b.txt', 'pool-warm', 'coll-1/b.txt', 'deadbeef', 100, ?, 0, 1, 2, ?)`, now, oldTime); err != nil {
		t.Fatalf("insert old dirty shadow: %v", err)
	}

	// Add a third file + shadow, make it dirty with a RECENT dirty_at (10 hours ago)
	recentTime := now - 10*3600
	if _, err := db.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, created_at, updated_at) VALUES ('file-3', 'coll-1', 'c.txt', 100, ?, 'sdb', ?, ?)`, now, now, now); err != nil {
		t.Fatalf("insert file 3: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO file_shadows (id, file_id, collection_id, rel_path, shadow_pool_id, shadow_rel_path, sha256, size_bytes, created_at, last_served_at, dirty, version, dirty_at) VALUES ('shdw-recent', 'file-3', 'coll-1', 'c.txt', 'pool-warm', 'coll-1/c.txt', 'deadbeef', 100, ?, 0, 1, 2, ?)`, now, recentTime); err != nil {
		t.Fatalf("insert recent dirty shadow: %v", err)
	}

	// List dirty shadows older than 72 hours
	oldShadows, err := ListDirtyShadowsOlderThan(db, warmPoolID, 72)
	if err != nil {
		t.Fatalf("ListDirtyShadowsOlderThan: %v", err)
	}

	// Should only include the 80-hour-old shadow (b.txt), not the 10-hour-old (c.txt) or the fresh one (a.txt)
	if len(oldShadows) != 1 {
		t.Errorf("old shadows count: got %d, want 1", len(oldShadows))
	}
	if len(oldShadows) == 1 && oldShadows[0].RelPath != "b.txt" {
		t.Errorf("old shadow rel_path: got %s, want b.txt", oldShadows[0].RelPath)
	}

	// Verify the fresh dirty shadow (a.txt) is NOT in the list
	for _, s := range oldShadows {
		if s.RelPath == "a.txt" {
			t.Error("fresh dirty shadow (a.txt) should not be in old list")
		}
		if s.RelPath == "c.txt" {
			t.Error("recent dirty shadow (c.txt) should not be in old list")
		}
	}
}

func TestV24MigrationHasDirtyColumns(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	// Run full migration from scratch
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Verify schema_version is 41 (WS62: event_rules; WS59: ota_updates; 39 was WS55: app_installs columns + v39)
	var version int
	err = db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version)
	if err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version != 41 {
		t.Errorf("expected schema_version 41, got %d", version)
	}

	// Verify file_shadows table has dirty, version, dirty_at columns
	rows, err := db.Query(`PRAGMA table_info(file_shadows)`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer rows.Close()

	hasDirty := false
	hasVersion := false
	hasDirtyAt := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dfltValue sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan pragma: %v", err)
		}
		if name == "dirty" {
			hasDirty = true
			if ctype != "INTEGER" {
				t.Errorf("dirty column type: got %s, want INTEGER", ctype)
			}
			if notnull != 1 {
				t.Error("dirty column should be NOT NULL")
			}
		}
		if name == "version" {
			hasVersion = true
			if ctype != "INTEGER" {
				t.Errorf("version column type: got %s, want INTEGER", ctype)
			}
			if notnull != 1 {
				t.Error("version column should be NOT NULL")
			}
		}
		if name == "dirty_at" {
			hasDirtyAt = true
			if ctype != "INTEGER" {
				t.Errorf("dirty_at column type: got %s, want INTEGER", ctype)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate pragma: %v", err)
	}

	if !hasDirty {
		t.Error("file_shadows missing dirty column")
	}
	if !hasVersion {
		t.Error("file_shadows missing version column")
	}
	if !hasDirtyAt {
		t.Error("file_shadows missing dirty_at column")
	}
}

// ============================================================================
// WS37 (FR-2.8): Shadow eviction + promotion tests
// ============================================================================

// setupEvictionDB opens an in-memory catalog at schema v25 with one warm pool,
// one cold pool, one collection on cold, one indexed file, and pre-created
// shadows on the warm pool. Returns db, collID, relPath, warmPoolID, warmMount.
func setupEvictionDB(t *testing.T) (*sql.DB, string, string, string, string) {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	now := time.Now().Unix()

	// Warm pool: 1 GB size, quota = 20% = 200 MB
	if _, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES ('pool-warm', 'warm', 'local', 'warm', 'single', 'single', '/mnt/warm', 1000000000, 100000000, 0, '', 'active', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert warm pool: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pool_disks (pool_id, disk_name, created_at) VALUES ('pool-warm', 'sda', ?)`, now); err != nil {
		t.Fatalf("insert pool_disk warm: %v", err)
	}

	// Cold pool
	if _, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES ('pool-cold', 'cold', 'local', 'cold', 'single', 'single', '/mnt/cold', 1000000000, 500000000, 0, '', 'active', ?, ?)`, now, now); err != nil {
		t.Fatalf("insert cold pool: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pool_disks (pool_id, disk_name, created_at) VALUES ('pool-cold', 'sdb', ?)`, now); err != nil {
		t.Fatalf("insert pool_disk cold: %v", err)
	}

	// Collection on cold pool
	if _, err := db.Exec(`INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, last_accessed_at, created_at, updated_at) VALUES ('coll-1', 'photos', 'pool-cold', '/photos', 'sdb', 0, ?, ?, ?)`, now, now, now); err != nil {
		t.Fatalf("insert collection: %v", err)
	}

	// Indexed file on cold pool
	if _, err := db.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, created_at, updated_at) VALUES ('file-1', 'coll-1', 'a.txt', 100, ?, 'sdb', ?, ?)`, now, now, now); err != nil {
		t.Fatalf("insert file: %v", err)
	}

	// Temp mountpoints
	tmpDir := t.TempDir()
	warmMount := filepath.Join(tmpDir, "mnt", "warm")
	coldMount := filepath.Join(tmpDir, "mnt", "cold")
	if err := os.MkdirAll(filepath.Join(warmMount, ".kelvios-shadow", "coll-1"), 0o755); err != nil {
		t.Fatalf("mkdir shadow dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(coldMount, "photos"), 0o755); err != nil {
		t.Fatalf("mkdir cold photos: %v", err)
	}
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, warmMount, "pool-warm"); err != nil {
		t.Fatalf("update warm pool mountpoint: %v", err)
	}
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, coldMount, "pool-cold"); err != nil {
		t.Fatalf("update cold pool mountpoint: %v", err)
	}
	// Canonical file on cold
	canonicalPath := filepath.Join(coldMount, "photos", "a.txt")
	if err := os.WriteFile(canonicalPath, []byte("canonical content"), 0o644); err != nil {
		t.Fatalf("write canonical file: %v", err)
	}

	return db, "coll-1", "a.txt", "pool-warm", warmMount
}

// createShadowForEviction creates a shadow row + file for the given relPath
// with the given size and last_served_at (0 means never served).
// warmMount is the warm pool's mountpoint where the shadow file should be created.
func createShadowForEviction(t *testing.T, db *sql.DB, shadowID, collID, relPath, poolID, shadowRelPath string, sizeBytes int64, lastServedAt int64, dirty int, warmMount string) string {
	t.Helper()
	now := time.Now().Unix()
	createdAt := now
	if lastServedAt == 0 {
		lastServedAt = createdAt
	}
	dirtyAt := int64(0)
	if dirty == 1 {
		dirtyAt = createdAt
	}
	if _, err := db.Exec(`INSERT INTO file_shadows (id, file_id, collection_id, rel_path, shadow_pool_id, shadow_rel_path, sha256, size_bytes, created_at, last_served_at, dirty, version, dirty_at) VALUES (?, 'file-1', ?, ?, ?, ?, 'deadbeef', ?, ?, ?, ?, 1, ?)`, shadowID, collID, relPath, poolID, shadowRelPath, sizeBytes, createdAt, lastServedAt, dirty, dirtyAt); err != nil {
		t.Fatalf("insert shadow %s: %v", shadowID, err)
	}
	// Create shadow file on disk at the pool's mountpoint
	shadowPath := filepath.Join(warmMount, ".kelvios-shadow", shadowRelPath)
	if err := os.MkdirAll(filepath.Dir(shadowPath), 0o755); err != nil {
		t.Fatalf("mkdir shadow dir: %v", err)
	}
	if err := os.WriteFile(shadowPath, []byte("shadow content"), 0o644); err != nil {
		t.Fatalf("write shadow file: %v", err)
	}
	return shadowPath
}

func TestEvictShadowsOverQuotaLRU(t *testing.T) {
	db, collID, _, warmPoolID, warmMount := setupEvictionDB(t)
	defer db.Close()

	// Quota = 1 GB * 20% = 200 MB = 200000000 bytes
	// Create 3 clean shadows, each 100 MB, total 300 MB > 200 MB quota
	// last_served_at: shdw-a = 1 hour ago (most recent), shdw-b = 2 hours ago, shdw-c = 3 hours ago (least recent)
	now := time.Now().Unix()
	shdwAPath := createShadowForEviction(t, db, "shdw-a", collID, "a.txt", warmPoolID, "coll-1/a.txt", 100000000, now-3600, 0, warmMount)
	shdwBPath := createShadowForEviction(t, db, "shdw-b", collID, "b.txt", warmPoolID, "coll-1/b.txt", 100000000, now-7200, 0, warmMount)
	shdwCPath := createShadowForEviction(t, db, "shdw-c", collID, "c.txt", warmPoolID, "coll-1/c.txt", 100000000, now-10800, 0, warmMount)

	// Track deletions
	var deleted []string
	deleteFunc := func(path string) error {
		deleted = append(deleted, path)
		return os.Remove(path)
	}

	evicted, err := EvictShadows(db, warmPoolID, deleteFunc, "test-actor")
	if err != nil {
		t.Fatalf("EvictShadows: %v", err)
	}
	if evicted != 1 {
		t.Errorf("evicted count: got %d, want 1 (evict down to quota)", evicted)
	}

	// LRU: least recently served first -> shdw-c (3h ago) should be evicted
	foundC := false
	for _, d := range deleted {
		if d == shdwCPath {
			foundC = true
		}
	}
	if !foundC {
		t.Errorf("LRU eviction: expected shdw-c (least recently served) to be evicted first, got %v", deleted)
	}

	// shdw-a and shdw-b should NOT be evicted (quota satisfied after evicting shdw-c)
	for _, d := range deleted {
		if d == shdwAPath || d == shdwBPath {
			t.Errorf("LRU eviction: shdw-a/shdw-b should not be evicted, but %s was deleted", d)
		}
	}

	// Verify rows: shdw-c gone, shdw-a and shdw-b remain
	for _, id := range []string{"shdw-a", "shdw-b"} {
		var exists int
		err := db.QueryRow(`SELECT COUNT(*) FROM file_shadows WHERE id = ?`, id).Scan(&exists)
		if err != nil {
			t.Fatalf("query shadow %s: %v", id, err)
		}
		if exists != 1 {
			t.Errorf("shadow %s should still exist after eviction", id)
		}
	}
	var exists int
	err = db.QueryRow(`SELECT COUNT(*) FROM file_shadows WHERE id = ?`, "shdw-c").Scan(&exists)
	if err != nil {
		t.Fatalf("query shadow shdw-c: %v", err)
	}
	if exists != 0 {
		t.Error("shadow shdw-c should be evicted (row deleted)")
	}
}

func TestEvictShadowsIdle(t *testing.T) {
	db, collID, _, warmPoolID, warmMount := setupEvictionDB(t)
	defer db.Close()

	// Quota = 200 MB. Create 2 clean shadows, each 50 MB, total 100 MB < quota.
	// shdw-old: last_served_at = 40 days ago (idle, >= 30 days)
	// shdw-new: last_served_at = 1 hour ago (not idle)
	now := time.Now().Unix()
	oldTime := now - 40*24*3600
	recentTime := now - 3600

	shdwOldPath := createShadowForEviction(t, db, "shdw-old", collID, "old.txt", warmPoolID, "coll-1/old.txt", 50000000, oldTime, 0, warmMount)
	shdwNewPath := createShadowForEviction(t, db, "shdw-new", collID, "new.txt", warmPoolID, "coll-1/new.txt", 50000000, recentTime, 0, warmMount)

	var deleted []string
	deleteFunc := func(path string) error {
		deleted = append(deleted, path)
		return os.Remove(path)
	}

	evicted, err := EvictShadows(db, warmPoolID, deleteFunc, "test-actor")
	if err != nil {
		t.Fatalf("EvictShadows: %v", err)
	}
	if evicted != 1 {
		t.Errorf("evicted count: got %d, want 1 (idle eviction)", evicted)
	}

	foundOld := false
	for _, d := range deleted {
		if d == shdwOldPath {
			foundOld = true
		}
	}
	if !foundOld {
		t.Errorf("idle eviction: expected shdw-old (40 days unserved) to be evicted, got %v", deleted)
	}

	for _, d := range deleted {
		if d == shdwNewPath {
			t.Errorf("idle eviction: shdw-new (recently served) should not be evicted, but %s was deleted", d)
		}
	}
}

func TestEvictShadowsNeverDirty(t *testing.T) {
	db, collID, relPath, warmPoolID, warmMount := setupEvictionDB(t)
	defer db.Close()

	// Quota = 200 MB. Create 1 dirty shadow (100 MB) + 1 clean shadow (100 MB).
	// Total = 200 MB = quota (not over quota). But even if over quota, dirty must never be evicted.
	now := time.Now().Unix()
	oldTime := now - 40*24*3600

	// Dirty shadow, unserved for 40 days (would be idle-evictable if clean)
	_ = createShadowForEviction(t, db, "shdw-dirty", collID, relPath, warmPoolID, "coll-1/dirty.txt", 100000000, oldTime, 1, warmMount)
	// Mark it dirty in DB
	if _, err := db.Exec(`UPDATE file_shadows SET dirty = 1 WHERE id = ?`, "shdw-dirty"); err != nil {
		t.Fatalf("mark shadow dirty: %v", err)
	}

	// Clean shadow, also unserved for 40 days
	_ = createShadowForEviction(t, db, "shdw-clean", collID, "clean.txt", warmPoolID, "coll-1/clean.txt", 100000000, oldTime, 0, warmMount)

	var deleted []string
	deleteFunc := func(path string) error {
		deleted = append(deleted, path)
		return os.Remove(path)
	}

	evicted, err := EvictShadows(db, warmPoolID, deleteFunc, "test-actor")
	if err != nil {
		t.Fatalf("EvictShadows: %v", err)
	}
	if evicted != 1 {
		t.Errorf("evicted count: got %d, want 1 (idle clean shadow evicted; dirty never evicted)", evicted)
	}
	if len(deleted) != 1 {
		t.Errorf("exactly the clean shadow should be deleted, got %v", deleted)
	}

	// Dirty shadow must survive; idle clean shadow must be gone
	for _, tc := range []struct {
		id   string
		want int
	}{{"shdw-dirty", 1}, {"shdw-clean", 0}} {
		var exists int
		err := db.QueryRow(`SELECT COUNT(*) FROM file_shadows WHERE id = ?`, tc.id).Scan(&exists)
		if err != nil {
			t.Fatalf("query shadow %s: %v", tc.id, err)
		}
		if exists != tc.want {
			t.Errorf("shadow %s: got exists=%d, want %d", tc.id, exists, tc.want)
		}
	}
}

func TestEvictShadowsPurelyDirtyOverQuota(t *testing.T) {
	db, _, _, warmPoolID, warmMount := setupEvictionDB(t)
	defer db.Close()

	// Quota = 200 MB. Create 3 dirty shadows, each 100 MB, total 300 MB > quota.
	// Pool is over quota purely because of dirty shadows -> evict nothing.
	now := time.Now().Unix()
	for i, id := range []string{"shdw-d1", "shdw-d2", "shdw-d3"} {
		relPath := fmt.Sprintf("coll-1/d%d.txt", i)
		_ = createShadowForEviction(t, db, id, "coll-1", relPath, warmPoolID, relPath, 100000000, now-int64(i)*3600, 1, warmMount)
		if _, err := db.Exec(`UPDATE file_shadows SET dirty = 1 WHERE id = ?`, id); err != nil {
			t.Fatalf("mark shadow %s dirty: %v", id, err)
		}
	}

	var deleted []string
	deleteFunc := func(path string) error {
		deleted = append(deleted, path)
		return os.Remove(path)
	}

	evicted, err := EvictShadows(db, warmPoolID, deleteFunc, "test-actor")
	if err != nil {
		t.Fatalf("EvictShadows: %v", err)
	}
	if evicted != 0 {
		t.Errorf("evicted count: got %d, want 0 (purely dirty over-quota evicts nothing)", evicted)
	}
	if len(deleted) != 0 {
		t.Errorf("no files should be deleted, got %v", deleted)
	}
}

func TestEvictShadowsLeavesCanonical(t *testing.T) {
	db, collID, relPath, warmPoolID, warmMount := setupEvictionDB(t)
	defer db.Close()

	// Create a shadow, then evict it. Canonical file must remain byte-identical.
	now := time.Now().Unix()
	oldTime := now - 40*24*3600
	shdwPath := createShadowForEviction(t, db, "shdw-leave", collID, relPath, warmPoolID, "coll-1/leave.txt", 50000000, oldTime, 0, warmMount)

	// Record canonical content before eviction
	canonicalPath := filepath.Join(warmMount, "..", "cold", "photos", relPath)
	canonicalPath = filepath.Clean(canonicalPath)
	canonicalBefore, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("read canonical before: %v", err)
	}

	var deleted []string
	deleteFunc := func(path string) error {
		deleted = append(deleted, path)
		return os.Remove(path)
	}

	evicted, err := EvictShadows(db, warmPoolID, deleteFunc, "test-actor")
	if err != nil {
		t.Fatalf("EvictShadows: %v", err)
	}
	if evicted != 1 {
		t.Errorf("evicted count: got %d, want 1", evicted)
	}

	// Shadow file should be deleted
	if _, err := os.Stat(shdwPath); !os.IsNotExist(err) {
		t.Errorf("shadow file should be deleted after eviction, still exists: %s", shdwPath)
	}

	// Shadow row should be deleted
	var exists int
	err = db.QueryRow(`SELECT COUNT(*) FROM file_shadows WHERE id = ?`, "shdw-leave").Scan(&exists)
	if err != nil {
		t.Fatalf("query shadow: %v", err)
	}
	if exists != 0 {
		t.Error("shadow row should be deleted after eviction")
	}

	// Canonical file must be byte-identical
	canonicalAfter, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("read canonical after: %v", err)
	}
	if string(canonicalBefore) != string(canonicalAfter) {
		t.Errorf("canonical file changed after eviction: before=%q after=%q", string(canonicalBefore), string(canonicalAfter))
	}
}

func TestGenerateShadowPromotions(t *testing.T) {
	db, collID, relPath, warmPoolID, warmMount := setupEvictionDB(t)
	defer db.Close()

	now := time.Now().Unix()

	// Create a clean shadow with >10 accesses in 30 days -> should generate PENDING promotion
	if _, err := db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES ('acc-1', ?, ?, 'test', ?, 'content')`, collID, relPath, now-3600); err != nil {
		t.Fatalf("insert access event: %v", err)
	}
	// Insert 11 more access events (total 12 > threshold of 10)
	for i := 2; i <= 12; i++ {
		if _, err := db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES (?, ?, ?, 'test', ?, 'content')`, fmt.Sprintf("acc-%d", i), collID, relPath, now-int64(i)*3600); err != nil {
			t.Fatalf("insert access event %d: %v", i, err)
		}
	}

	// Create the shadow
	shadowID := "shdw-hot"
	createShadowForEviction(t, db, shadowID, collID, relPath, warmPoolID, "coll-1/a.txt", 100, now-3600, 0, warmMount)

	created, err := GenerateShadowPromotions(db, "test-actor")
	if err != nil {
		t.Fatalf("GenerateShadowPromotions: %v", err)
	}
	if created != 1 {
		t.Errorf("created promotions: got %d, want 1 (>10 accesses in 30 days)", created)
	}

	// Verify PENDING row exists
	var status string
	err = db.QueryRow(`SELECT status FROM shadow_promotions WHERE collection_id = ? AND rel_path = ?`, collID, relPath).Scan(&status)
	if err != nil {
		t.Fatalf("query promotion: %v", err)
	}
	if status != "pending" {
		t.Errorf("promotion status: got %s, want pending", status)
	}

	// Second generate should create 0 (dedup: already has non-superseded row)
	created2, err := GenerateShadowPromotions(db, "test-actor")
	if err != nil {
		t.Fatalf("GenerateShadowPromotions second: %v", err)
	}
	if created2 != 0 {
		t.Errorf("second generate created: got %d, want 0 (dedup)", created2)
	}
}

func TestGenerateShadowPromotionsBelowThreshold(t *testing.T) {
	db, collID, relPath, warmPoolID, warmMount := setupEvictionDB(t)
	defer db.Close()

	now := time.Now().Unix()

	// Create a clean shadow with exactly 10 accesses (<= threshold, not >) -> no promotion
	for i := 1; i <= 10; i++ {
		if _, err := db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES (?, ?, ?, 'test', ?, 'content')`, fmt.Sprintf("acc-%d", i), collID, relPath, now-int64(i)*3600); err != nil {
			t.Fatalf("insert access event %d: %v", i, err)
		}
	}

	shadowID := "shdw-warm"
	createShadowForEviction(t, db, shadowID, collID, relPath, warmPoolID, "coll-1/a.txt", 100, now-3600, 0, warmMount)

	created, err := GenerateShadowPromotions(db, "test-actor")
	if err != nil {
		t.Fatalf("GenerateShadowPromotions: %v", err)
	}
	if created != 0 {
		t.Errorf("created promotions: got %d, want 0 (<=10 accesses, not > threshold)", created)
	}
}

func TestGenerateShadowPromotionsDirtyExcluded(t *testing.T) {
	db, collID, relPath, warmPoolID, warmMount := setupEvictionDB(t)
	defer db.Close()

	now := time.Now().Unix()

	// Create a dirty shadow with >10 accesses -> should be excluded from promotion
	for i := 1; i <= 12; i++ {
		if _, err := db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES (?, ?, ?, 'test', ?, 'content')`, fmt.Sprintf("acc-%d", i), collID, relPath, now-int64(i)*3600); err != nil {
			t.Fatalf("insert access event %d: %v", i, err)
		}
	}

	shadowID := "shdw-dirty"
	createShadowForEviction(t, db, shadowID, collID, relPath, warmPoolID, "coll-1/a.txt", 100, now-3600, 1, warmMount)
	if _, err := db.Exec(`UPDATE file_shadows SET dirty = 1 WHERE id = ?`, shadowID); err != nil {
		t.Fatalf("mark shadow dirty: %v", err)
	}

	created, err := GenerateShadowPromotions(db, "test-actor")
	if err != nil {
		t.Fatalf("GenerateShadowPromotions: %v", err)
	}
	if created != 0 {
		t.Errorf("created promotions: got %d, want 0 (dirty shadows excluded)", created)
	}
}

func TestDecideShadowPromotionApproveReject(t *testing.T) {
	db, collID, relPath, warmPoolID, _ := setupEvictionDB(t)
	defer db.Close()

	now := time.Now().Unix()

	// Insert a PENDING promotion row directly
	promoID := "shp-test-1"
	if _, err := db.Exec(`INSERT INTO shadow_promotions (id, collection_id, rel_path, shadow_pool_id, access_count_30d, status, created_at, decided_at) VALUES (?, ?, ?, ?, 12, 'pending', ?, 0)`, promoID, collID, relPath, warmPoolID, now); err != nil {
		t.Fatalf("insert promotion: %v", err)
	}

	// Approve
	if err := DecideShadowPromotion(db, promoID, "approve", "test-actor"); err != nil {
		t.Fatalf("DecideShadowPromotion approve: %v", err)
	}
	var status string
	err := db.QueryRow(`SELECT status FROM shadow_promotions WHERE id = ?`, promoID).Scan(&status)
	if err != nil {
		t.Fatalf("query promotion: %v", err)
	}
	if status != "approve" {
		t.Errorf("promotion status after approve: got %s, want approve", status)
	}

	// Re-approve should error (not pending anymore)
	err = DecideShadowPromotion(db, promoID, "approve", "test-actor")
	if err == nil {
		t.Error("double-decide approve: expected error, got nil")
	}

	// Insert another pending for reject test
	promoID2 := "shp-test-2"
	if _, err := db.Exec(`INSERT INTO shadow_promotions (id, collection_id, rel_path, shadow_pool_id, access_count_30d, status, created_at, decided_at) VALUES (?, ?, ?, ?, 12, 'pending', ?, 0)`, promoID2, collID, "b.txt", warmPoolID, now); err != nil {
		t.Fatalf("insert promotion 2: %v", err)
	}

	// Reject
	if err := DecideShadowPromotion(db, promoID2, "reject", "test-actor"); err != nil {
		t.Fatalf("DecideShadowPromotion reject: %v", err)
	}
	err = db.QueryRow(`SELECT status FROM shadow_promotions WHERE id = ?`, promoID2).Scan(&status)
	if err != nil {
		t.Fatalf("query promotion 2: %v", err)
	}
	if status != "reject" {
		t.Errorf("promotion status after reject: got %s, want reject", status)
	}

	// Double-decide reject should error
	err = DecideShadowPromotion(db, promoID2, "reject", "test-actor")
	if err == nil {
		t.Error("double-decide reject: expected error, got nil")
	}
}

func TestDecideShadowPromotionUnknownID(t *testing.T) {
	db, _, _, _, _ := setupEvictionDB(t)
	defer db.Close()

	err := DecideShadowPromotion(db, "shp-nonexistent", "approve", "test-actor")
	if err == nil {
		t.Error("DecideShadowPromotion unknown ID: expected error, got nil")
	}
}

func TestV25MigrationHasShadowPromotions(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Verify schema_version is 41 (WS62: event_rules; WS59: ota_updates; 39 was WS55: app_installs columns + v39)
	var version int
	err = db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version)
	if err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version != 41 {
		t.Errorf("expected schema_version 41, got %d", version)
	}

	// Verify shadow_promotions table exists
	var tableExists int
	err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'shadow_promotions'`).Scan(&tableExists)
	if err != nil {
		t.Fatalf("query shadow_promotions table: %v", err)
	}
	if tableExists != 1 {
		t.Error("shadow_promotions table missing after migration")
	}

	// Verify indexes on shadow_promotions
	var indexCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'shadow_promotions'`).Scan(&indexCount)
	if err != nil {
		t.Fatalf("query shadow_promotions indexes: %v", err)
	}
	if indexCount == 0 {
		t.Error("shadow_promotions missing indexes after migration")
	}
}
