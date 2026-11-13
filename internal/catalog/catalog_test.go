package catalog

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestCatalogMigrate(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Verify tables exist
	tables := []string{"meta", "devices", "volumes", "shares", "pools", "pool_disks", "collections", "collection_shares", "audit_log", "accounts", "sessions"}
	for _, table := range tables {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count)
		if err != nil || count == 0 {
			t.Errorf("table %s not created", table)
		}
	}

	// Verify schema version
	var version int
	err = db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&version)
	if err != nil || version != schemaVersion {
		t.Errorf("schema_version: got %d, want %d", version, schemaVersion)
	}
}

func TestWriteAndListAuditLog(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Write some audit logs
	WriteAuditLog(db, "local", "pool.create", "pool", "pool-1", "ok", "")
	WriteAuditLog(db, "local", "collection.create", "collection", "coll-1", "ok", "")
	WriteAuditLog(db, "local", "share.create", "share", "share-1", "error", "duplicate name")
	WriteAuditLog(db, "local", "pool.delete", "pool", "pool-1", "ok", "")

	// List all
	entries, total, err := ListAuditLog(db, 0, 0, "", "", "")
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	if total != 4 || len(entries) != 4 {
		t.Errorf("expected 4 entries, got %d (len %d)", total, len(entries))
	}

	// Verify order (newest first)
	if entries[0].Action != "pool.delete" || entries[3].Action != "pool.create" {
		t.Errorf("unexpected order: %+v", entries)
	}

	// Filter by action
	filtered, totalFiltered, err := ListAuditLog(db, 0, 0, "pool.create", "", "")
	if err != nil {
		t.Fatalf("ListAuditLog with action filter: %v", err)
	}
	if totalFiltered != 1 || len(filtered) != 1 || filtered[0].Action != "pool.create" {
		t.Errorf("expected 1 pool.create entry, got %d (len %d): %+v", totalFiltered, len(filtered), filtered)
	}

	// Filter by result
	filtered, totalFiltered, err = ListAuditLog(db, 0, 0, "", "", "error")
	if err != nil {
		t.Fatalf("ListAuditLog with result filter: %v", err)
	}
	if totalFiltered != 1 || len(filtered) != 1 || filtered[0].Result != "error" {
		t.Errorf("expected 1 error entry, got %d (len %d): %+v", totalFiltered, len(filtered), filtered)
	}

	// Filter by target_type
	filtered, totalFiltered, err = ListAuditLog(db, 0, 0, "", "share", "")
	if err != nil {
		t.Fatalf("ListAuditLog with target_type filter: %v", err)
	}
	if totalFiltered != 1 || len(filtered) != 1 || filtered[0].TargetType != "share" {
		t.Errorf("expected 1 share target entry, got %d (len %d): %+v", totalFiltered, len(filtered), filtered)
	}

	// Pagination
	page1, totalPage1, err := ListAuditLog(db, 2, 0, "", "", "")
	if err != nil {
		t.Fatalf("ListAuditLog page 1: %v", err)
	}
	if totalPage1 != 4 || len(page1) != 2 {
		t.Errorf("expected 2 entries on page 1, got %d (total %d)", len(page1), totalPage1)
	}

	page2, totalPage2, err := ListAuditLog(db, 2, 2, "", "", "")
	if err != nil {
		t.Fatalf("ListAuditLog page 2: %v", err)
	}
	if totalPage2 != 4 || len(page2) != 2 {
		t.Errorf("expected 2 entries on page 2, got %d (total %d)", len(page2), totalPage2)
	}

	if page1[0].ID == page2[0].ID {
		t.Errorf("pagination returned duplicate entries across pages")
	}

	// Empty list check
	emptyList, emptyTotal, err := ListAuditLog(db, 0, 0, "nonexistent.action", "", "")
	if err != nil {
		t.Fatalf("ListAuditLog empty: %v", err)
	}
	if emptyTotal != 0 || len(emptyList) != 0 {
		t.Errorf("expected empty list, got %d (len %d)", emptyTotal, len(emptyList))
	}
	if emptyList == nil {
		t.Errorf("empty list should be empty slice, not nil")
	}
}

func TestCatalogMigrateIdempotent(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Run migrate twice
	if err := Migrate(db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestCreatePoolAndGetPool(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		UsedBytes:  0,
		Encrypted:  false,
		Status:     "active",
	}
	diskNames := []string{"sda"}

	if err := CreatePool(db, pool, diskNames); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	gotPool, gotDisks, err := GetPool(db, "pool-1")
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	if gotPool == nil {
		t.Fatal("GetPool returned nil")
	}
	if gotPool.ID != pool.ID || gotPool.Name != pool.Name || gotPool.Tier != pool.Tier {
		t.Errorf("pool mismatch: got %+v", gotPool)
	}
	if len(gotDisks) != 1 || gotDisks[0] != "sda" {
		t.Errorf("disks mismatch: got %v", gotDisks)
	}
}

func TestCreatePoolDuplicateFails(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		UsedBytes:  0,
		Encrypted:  false,
		Status:     "active",
	}

	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("first CreatePool: %v", err)
	}
	// Duplicate ID should still fail (primary key constraint)
	if err := CreatePool(db, pool, []string{"sda"}); err == nil {
		t.Error("second CreatePool should fail (duplicate ID)")
	}
}

// TestCreatePoolDuplicateNameFails tests that creating a pool with a duplicate name
// (case-insensitive) returns ErrPoolNameExists.
func TestCreatePoolDuplicateNameFails(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool1 := &PoolRow{
		ID:         "pool-1",
		Name:       "Data",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/data",
		SizeBytes:  4000787030016,
		UsedBytes:  0,
		Encrypted:  false,
		Status:     "active",
	}

	if err := CreatePool(db, pool1, []string{"sda"}); err != nil {
		t.Fatalf("first CreatePool: %v", err)
	}

	// Try to create another pool with the same name (different ID)
	pool2 := &PoolRow{
		ID:         "pool-2",
		Name:       "Data",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/data2",
		SizeBytes:  4000787030016,
		UsedBytes:  0,
		Encrypted:  false,
		Status:     "active",
	}

	err = CreatePool(db, pool2, []string{"sdb"})
	if err == nil {
		t.Fatal("second CreatePool with same name should fail")
	}
	if !errors.Is(err, ErrPoolNameExists) {
		t.Errorf("expected ErrPoolNameExists, got: %v", err)
	}

	// Try with different case
	pool3 := &PoolRow{
		ID:         "pool-3",
		Name:       "data",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/data3",
		SizeBytes:  4000787030016,
		UsedBytes:  0,
		Encrypted:  false,
		Status:     "active",
	}

	err = CreatePool(db, pool3, []string{"sdc"})
	if err == nil {
		t.Fatal("third CreatePool with different case should fail")
	}
	if !errors.Is(err, ErrPoolNameExists) {
		t.Errorf("expected ErrPoolNameExists for different case, got: %v", err)
	}

	// Different name should succeed
	pool4 := &PoolRow{
		ID:         "pool-4",
		Name:       "Backup",
		NodeID:     "local",
		Tier:       "cold",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/backup",
		SizeBytes:  8001574060032,
		UsedBytes:  0,
		Encrypted:  false,
		Status:     "active",
	}

	if err := CreatePool(db, pool4, []string{"sdd"}); err != nil {
		t.Fatalf("CreatePool with different name should succeed: %v", err)
	}

	// Verify only 2 pools exist (Data and Backup)
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM pools").Scan(&count)
	if err != nil {
		t.Fatalf("count pools: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 pools, got %d", count)
	}
}

// TestPoolNameUniqueIndexExists tests that the unique index on pools(name) exists.
func TestPoolNameUniqueIndexExists(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_pools_name_unique'").Scan(&count)
	if err != nil {
		t.Fatalf("query index: %v", err)
	}
	if count != 1 {
		t.Errorf("expected unique index idx_pools_name_unique to exist, count=%d", count)
	}

	// Verify it's a unique index with NOCASE collation
	var sql string
	err = db.QueryRow("SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_pools_name_unique'").Scan(&sql)
	if err != nil {
		t.Fatalf("get index sql: %v", err)
	}
	if !strings.Contains(strings.ToUpper(sql), "UNIQUE") {
		t.Errorf("index should be UNIQUE: %s", sql)
	}
	if !strings.Contains(strings.ToUpper(sql), "NOCASE") {
		t.Errorf("index should have COLLATE NOCASE: %s", sql)
	}
}

func TestGetPoolNotFound(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool, disks, err := GetPool(db, "nonexistent")
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	if pool != nil || disks != nil {
		t.Errorf("expected nil for not found, got pool=%v disks=%v", pool, disks)
	}
}

func TestListPools(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pools := []*PoolRow{
		{ID: "pool-1", Name: "media", NodeID: "local", Tier: "warm", Mechanism: "single", Driver: "single", Mountpoint: "/mnt/media", SizeBytes: 4000787030016, Status: "active"},
		{ID: "pool-2", Name: "backup", NodeID: "local", Tier: "cold", Mechanism: "single", Driver: "single", Mountpoint: "/mnt/backup", SizeBytes: 8001574060032, Status: "active"},
	}
	for _, p := range pools {
		if err := CreatePool(db, p, []string{p.ID}); err != nil {
			t.Fatalf("CreatePool: %v", err)
		}
	}

	list, err := ListPools(db)
	if err != nil {
		t.Fatalf("ListPools: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("expected 2 pools, got %d", len(list))
	}
	if list[0].Name != "media" || list[1].Name != "backup" {
		t.Errorf("order wrong: got %v, %v", list[0].Name, list[1].Name)
	}
}

func TestDeletePool(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	if err := DeletePool(db, "pool-1"); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}

	gotPool, _, err := GetPool(db, "pool-1")
	if err != nil {
		t.Fatalf("GetPool after delete: %v", err)
	}
	if gotPool != nil {
		t.Error("pool should be deleted")
	}

	// Verify pool_disks cascaded
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM pool_disks WHERE pool_id=?", "pool-1").Scan(&count)
	if err != nil || count != 0 {
		t.Errorf("pool_disks not cascaded: count=%d", count)
	}
}

func TestDeletePoolNotFound(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	err = DeletePool(db, "nonexistent")
	if err == nil {
		t.Error("DeletePool should fail for nonexistent pool")
	}
}

func TestCreateCollectionAndList(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Create a pool first
	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	now := time.Now()
	lastAccessed := now.Add(-24 * time.Hour)
	coll := &CollectionRow{
		ID:             "coll-1",
		Name:           "photos",
		PoolID:         "pool-1",
		Path:           "/photos",
		DiskName:       "sda",
		SizeBytes:      1024 * 1024 * 100,
		LastAccessedAt: &lastAccessed,
	}

	if err := CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	collections, err := ListCollections(db, "pool-1")
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	if len(collections) != 1 {
		t.Errorf("expected 1 collection, got %d", len(collections))
	}
	if collections[0].Name != "photos" || collections[0].PoolID != "pool-1" {
		t.Errorf("collection mismatch: %+v", collections[0])
	}
	// Compare with second precision since SQLite stores Unix seconds
	if collections[0].LastAccessedAt == nil || collections[0].LastAccessedAt.Unix() != lastAccessed.Unix() {
		t.Errorf("last_accessed_at mismatch: got %v, want %v", collections[0].LastAccessedAt, lastAccessed)
	}
}

func TestListCollectionsEmpty(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	collections, err := ListCollections(db, "nonexistent")
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	if len(collections) != 0 {
		t.Errorf("expected empty list, got %d", len(collections))
	}
}

func TestCatalogWithFileDB(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	db.Close()

	// Reopen and verify persistence
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	gotPool, gotDisks, err := GetPool(db2, "pool-1")
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	if gotPool == nil || gotPool.Name != "media" {
		t.Errorf("pool not persisted: %+v", gotPool)
	}
	if len(gotDisks) != 1 || gotDisks[0] != "sda" {
		t.Errorf("disks not persisted: %v", gotDisks)
	}
}

func TestIndexCollectionFilesRecordsFiles(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	coll := &CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	tmpDir := t.TempDir()
	mountpoint := filepath.Join(tmpDir, "mnt", "media")
	root := filepath.Join(mountpoint, "photos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	files := map[string]int64{
		"a.txt":     100,
		"sub/b.txt": 200,
		"sub/c.txt": 300,
	}
	for relPath, size := range files {
		fullPath := filepath.Join(root, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("mkdir dir: %v", err)
		}
		if err := os.WriteFile(fullPath, make([]byte, size), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}

	pool.Mountpoint = mountpoint

	result, err := IndexCollectionFiles(db, coll, pool)
	if err != nil {
		t.Fatalf("IndexCollectionFiles: %v", err)
	}
	if result.Added != 3 {
		t.Errorf("expected 3 added, got %d (result=%+v)", result.Added, result)
	}
	if result.Removed != 0 {
		t.Errorf("expected 0 removed, got %d", result.Removed)
	}

	rows, err := db.Query(`SELECT rel_path, size_bytes FROM files WHERE collection_id = ? ORDER BY rel_path`, coll.ID)
	if err != nil {
		t.Fatalf("query files: %v", err)
	}
	defer rows.Close()

	var seen []struct {
		relPath string
		size    int64
	}
	for rows.Next() {
		var relPath string
		var size int64
		if err := rows.Scan(&relPath, &size); err != nil {
			t.Fatalf("scan file: %v", err)
		}
		seen = append(seen, struct {
			relPath string
			size    int64
		}{relPath, size})
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 file rows, got %d: %+v", len(seen), seen)
	}
	if seen[0].relPath != "a.txt" || seen[0].size != 100 {
		t.Errorf("unexpected first file: %+v", seen[0])
	}
	if seen[1].relPath != "sub/b.txt" || seen[1].size != 200 {
		t.Errorf("unexpected second file: %+v", seen[1])
	}
	if seen[2].relPath != "sub/c.txt" || seen[2].size != 300 {
		t.Errorf("unexpected third file: %+v", seen[2])
	}
}

func TestIndexCollectionFilesPrunesDeleted(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	coll := &CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	tmpDir := t.TempDir()
	mountpoint := filepath.Join(tmpDir, "mnt", "media")
	root := filepath.Join(mountpoint, "photos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}

	pool.Mountpoint = mountpoint

	result, err := IndexCollectionFiles(db, coll, pool)
	if err != nil {
		t.Fatalf("first IndexCollectionFiles: %v", err)
	}
	if result.Added != 2 {
		t.Errorf("expected 2 added, got %d", result.Added)
	}

	if err := os.Remove(filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("remove a.txt: %v", err)
	}

	result, err = IndexCollectionFiles(db, coll, pool)
	if err != nil {
		t.Fatalf("second IndexCollectionFiles: %v", err)
	}
	if result.Removed != 1 {
		t.Errorf("expected 1 removed, got %d", result.Removed)
	}
	if result.Added != 0 {
		t.Errorf("expected 0 added, got %d", result.Added)
	}

	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM files WHERE collection_id = ?`, coll.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count files: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 file row, got %d", count)
	}
	var relPath string
	err = db.QueryRow(`SELECT rel_path FROM files WHERE collection_id = ?`, coll.ID).Scan(&relPath)
	if err != nil {
		t.Fatalf("query rel_path: %v", err)
	}
	if relPath != "b.txt" {
		t.Errorf("expected b.txt, got %s", relPath)
	}
}

func TestIndexCollectionFilesUpsertsAdded(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	coll := &CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	tmpDir := t.TempDir()
	mountpoint := filepath.Join(tmpDir, "mnt", "media")
	root := filepath.Join(mountpoint, "photos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	pool.Mountpoint = mountpoint

	result, err := IndexCollectionFiles(db, coll, pool)
	if err != nil {
		t.Fatalf("first IndexCollectionFiles: %v", err)
	}
	if result.Added != 1 {
		t.Errorf("expected 1 added, got %d", result.Added)
	}

	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}

	result, err = IndexCollectionFiles(db, coll, pool)
	if err != nil {
		t.Fatalf("second IndexCollectionFiles: %v", err)
	}
	if result.Added != 1 {
		t.Errorf("expected 1 added, got %d", result.Added)
	}
	if result.Removed != 0 {
		t.Errorf("expected 0 removed, got %d", result.Removed)
	}

	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM files WHERE collection_id = ?`, coll.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count files: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 file rows, got %d", count)
	}
}

func TestIndexCollectionFilesUnknownCollection(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	coll := &CollectionRow{
		ID:        "nonexistent-coll",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}

	tmpDir := t.TempDir()
	pool.Mountpoint = tmpDir

	_, err = IndexCollectionFiles(db, coll, pool)
	if err == nil {
		t.Error("expected error for unknown collection, got nil")
	}
}

func TestIndexCollectionFilesMissingRoot(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	coll := &CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	pool.Mountpoint = "C:\\nonexistent\\path"

	_, err = IndexCollectionFiles(db, coll, pool)
	if err == nil {
		t.Error("expected error for missing root, got nil")
	}
}

// TestShareLinkCreateGetByTokenHash tests CreateShareLink and GetShareLinkByTokenHash.
func TestShareLinkCreateGetByTokenHash(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Create pool and collection
	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	coll := &CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	// Create a file record (required for share link creation)
	fileID := generateFileID()
	now := time.Now().Unix()
	_, err = db.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fileID, coll.ID, "test.txt", 100, now, "sda", now, now)
	if err != nil {
		t.Fatalf("insert file: %v", err)
	}

	// Create share link
	token := "test-token-32-hex-chars-1234567890ab"
	tokenHash := sha256Hex(token)
	link, err := CreateShareLink(db, coll.ID, "test.txt", tokenHash, "", nil, "test-user")
	if err != nil {
		t.Fatalf("CreateShareLink: %v", err)
	}
	if link == nil {
		t.Fatal("CreateShareLink returned nil")
	}
	if link.ID == "" || !strings.HasPrefix(link.ID, "link-") {
		t.Errorf("unexpected link ID: %s", link.ID)
	}
	if link.CollectionID != coll.ID {
		t.Errorf("collection_id mismatch: got %s, want %s", link.CollectionID, coll.ID)
	}
	if link.RelPath != "test.txt" {
		t.Errorf("rel_path mismatch: got %s, want test.txt", link.RelPath)
	}
	if link.TokenHash != tokenHash {
		t.Errorf("token_hash mismatch: got %s, want %s", link.TokenHash, tokenHash)
	}
	if link.PasswordHash != "" {
		t.Errorf("password_hash should be empty, got %s", link.PasswordHash)
	}
	if link.ExpiresAt != nil {
		t.Errorf("expires_at should be nil, got %v", link.ExpiresAt)
	}
	if link.CreatedBy != "test-user" {
		t.Errorf("created_by mismatch: got %s, want test-user", link.CreatedBy)
	}
	if link.RevokedAt != nil {
		t.Errorf("revoked_at should be nil, got %v", link.RevokedAt)
	}

	// Get by token hash
	got, err := GetShareLinkByTokenHash(db, tokenHash)
	if err != nil {
		t.Fatalf("GetShareLinkByTokenHash: %v", err)
	}
	if got == nil {
		t.Fatal("GetShareLinkByTokenHash returned nil")
	}
	if got.ID != link.ID {
		t.Errorf("ID mismatch: got %s, want %s", got.ID, link.ID)
	}
	if got.TokenHash != tokenHash {
		t.Errorf("token_hash mismatch on get: got %s, want %s", got.TokenHash, tokenHash)
	}

	// Verify raw token is NOT stored in token_hash column (only hash)
	var storedTokenHash string
	err = db.QueryRow(`SELECT token_hash FROM share_links WHERE id = ?`, link.ID).Scan(&storedTokenHash)
	if err != nil {
		t.Fatalf("query token_hash: %v", err)
	}
	if storedTokenHash == token {
		t.Error("raw token found in token_hash column")
	}
	if storedTokenHash != tokenHash {
		t.Errorf("stored token_hash mismatch: got %s, want %s", storedTokenHash, tokenHash)
	}
}

// TestShareLinkList tests ListShareLinks.
func TestShareLinkList(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	coll := &CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	// Create file records
	fileID1 := generateFileID()
	fileID2 := generateFileID()
	now := time.Now().Unix()
	_, err = db.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fileID1, coll.ID, "a.txt", 100, now, "sda", now, now)
	if err != nil {
		t.Fatalf("insert file 1: %v", err)
	}
	_, err = db.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fileID2, coll.ID, "b.txt", 200, now, "sda", now, now)
	if err != nil {
		t.Fatalf("insert file 2: %v", err)
	}

	// Create two share links
	token1 := "token-one-32-hex-chars-1234567890ab"
	tokenHash1 := sha256Hex(token1)
	link1, err := CreateShareLink(db, coll.ID, "a.txt", tokenHash1, "", nil, "user1")
	if err != nil {
		t.Fatalf("CreateShareLink 1: %v", err)
	}

	token2 := "token-two-32-hex-chars-1234567890ab"
	tokenHash2 := sha256Hex(token2)
	link2, err := CreateShareLink(db, coll.ID, "b.txt", tokenHash2, "", nil, "user2")
	if err != nil {
		t.Fatalf("CreateShareLink 2: %v", err)
	}

	// List share links
	links, err := ListShareLinks(db, coll.ID)
	if err != nil {
		t.Fatalf("ListShareLinks: %v", err)
	}
	if len(links) != 2 {
		t.Errorf("expected 2 links, got %d", len(links))
	}

	// Verify order (created_at ascending)
	if links[0].ID != link1.ID || links[1].ID != link2.ID {
		t.Errorf("unexpected order: %v, %v", links[0].ID, links[1].ID)
	}

	// Verify token hashes are present but raw tokens are not
	for _, l := range links {
		if l.TokenHash == "" {
			t.Error("token_hash should not be empty in list")
		}
	}

	// List for non-existent collection returns empty
	emptyLinks, err := ListShareLinks(db, "coll-nonexistent")
	if err != nil {
		t.Fatalf("ListShareLinks nonexistent: %v", err)
	}
	if len(emptyLinks) != 0 {
		t.Errorf("expected empty list for nonexistent collection, got %d", len(emptyLinks))
	}
}

// TestShareLinkRevoke tests RevokeShareLink.
func TestShareLinkRevoke(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	coll := &CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	fileID := generateFileID()
	now := time.Now().Unix()
	_, err = db.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fileID, coll.ID, "test.txt", 100, now, "sda", now, now)
	if err != nil {
		t.Fatalf("insert file: %v", err)
	}

	token := "test-token-32-hex-chars-1234567890ab"
	tokenHash := sha256Hex(token)
	link, err := CreateShareLink(db, coll.ID, "test.txt", tokenHash, "", nil, "test-user")
	if err != nil {
		t.Fatalf("CreateShareLink: %v", err)
	}

	// Verify not revoked initially
	if link.RevokedAt != nil {
		t.Errorf("revoked_at should be nil initially, got %v", link.RevokedAt)
	}

	// Revoke
	if err := RevokeShareLink(db, link.ID); err != nil {
		t.Fatalf("RevokeShareLink: %v", err)
	}

	// Verify revoked_at is set
	got, err := GetShareLinkByTokenHash(db, tokenHash)
	if err != nil {
		t.Fatalf("GetShareLinkByTokenHash after revoke: %v", err)
	}
	if got == nil {
		t.Fatal("GetShareLinkByTokenHash returned nil after revoke")
	}
	if got.RevokedAt == nil {
		t.Error("revoked_at should be set after revoke")
	}
	if got.RevokedAt.Before(link.CreatedAt) {
		t.Errorf("revoked_at %v should be after created_at %v", got.RevokedAt, link.CreatedAt)
	}

	// Revoke again (idempotent)
	if err := RevokeShareLink(db, link.ID); err != nil {
		t.Fatalf("RevokeShareLink second time: %v", err)
	}

	// Revoke non-existent (idempotent)
	if err := RevokeShareLink(db, "link-nonexistent"); err != nil {
		t.Fatalf("RevokeShareLink nonexistent: %v", err)
	}
}

// TestShareLinkWithPasswordAndExpiry tests share links with password and expiry.
func TestShareLinkWithPasswordAndExpiry(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	coll := &CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	fileID := generateFileID()
	now := time.Now().Unix()
	_, err = db.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		fileID, coll.ID, "secret.txt", 100, now, "sda", now, now)
	if err != nil {
		t.Fatalf("insert file: %v", err)
	}

	// Create share link with password and expiry
	token := "token-with-pw-32-hex-chars-12345678"
	tokenHash := sha256Hex(token)
	password := "secret-password-123"
	passwordHash, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	expiresAt := time.Now().Add(24 * time.Hour).Unix()
	link, err := CreateShareLink(db, coll.ID, "secret.txt", tokenHash, passwordHash, &expiresAt, "test-user")
	if err != nil {
		t.Fatalf("CreateShareLink: %v", err)
	}

	if link.PasswordHash != passwordHash {
		t.Errorf("password_hash mismatch: got %s, want %s", link.PasswordHash, passwordHash)
	}
	if link.ExpiresAt == nil {
		t.Error("expires_at should be set")
	}
	if link.ExpiresAt.Unix() != expiresAt {
		t.Errorf("expires_at mismatch: got %d, want %d", link.ExpiresAt.Unix(), expiresAt)
	}

	// Verify password hash is not raw password
	if link.PasswordHash == password {
		t.Error("raw password found in password_hash column")
	}

	// Verify verifyPassword works
	if !verifyPassword(password, link.PasswordHash) {
		t.Error("verifyPassword failed for correct password")
	}
	if verifyPassword("wrong-password", link.PasswordHash) {
		t.Error("verifyPassword should fail for wrong password")
	}
}

// sha256Hex is copied from auth.go for test independence.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hashPassword and verifyPassword are copied from auth.go for test independence.
const (
	pbkdf2Iterations = 100000
	pbkdf2SaltLen    = 16
	pbkdf2KeyLen     = 32
)

func hashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := pbkdf2Key([]byte(password), salt, pbkdf2Iterations, pbkdf2KeyLen)
	return fmt.Sprintf("v1$%d$%s$%s", pbkdf2Iterations, hex.EncodeToString(salt), hex.EncodeToString(key)), nil
}

func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "v1" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2Key([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func pbkdf2Key(password, salt []byte, iter, keyLen int) []byte {
	h := hmac.New(sha256.New, password)
	var out []byte
	for i := 1; len(out) < keyLen; i++ {
		h.Write(salt)
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], uint32(i))
		h.Write(buf[:])
		out = append(out, h.Sum(nil)...)
		h.Reset()
	}
	return out[:keyLen]
}

// ============================================================================
// WS18 LANGKAH-B: Supply-chain entries catalog tests
// ============================================================================

// TestSupplychainMigration tests that migration to v11 creates the supplychain_entries table.
func TestSupplychainMigration(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Verify supplychain_entries table exists
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", "supplychain_entries").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("table supplychain_entries not created")
	}

	// Verify schema version is 11
	var version int
	err = db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&version)
	if err != nil || version != schemaVersion {
		t.Errorf("schema_version: got %d, want %d", version, schemaVersion)
	}

	// Verify index exists
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", "idx_supplychain_entries_name").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("index idx_supplychain_entries_name not created")
	}
}

// TestSupplychainEntryCreateGet tests create and get round-trip with all fields.
func TestSupplychainEntryCreateGet(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	validDigest := "sha256:" + strings.Repeat("a", 64)
	entry := &SupplychainEntryRow{
		Name:                "test-app",
		Publisher:           "publisher",
		SourceRepo:          "https://github.com/org/repo",
		ImageRef:            "registry.io/app:latest",
		PinnedDigest:        validDigest,
		SignatureKind:       "cosign",
		SourceKind:          "official",
		UnverifiedConfirmed: false,
	}

	if err := CreateSupplychainEntry(db, entry); err != nil {
		t.Fatalf("CreateSupplychainEntry: %v", err)
	}
	if entry.ID == "" || !strings.HasPrefix(entry.ID, "sce-") {
		t.Errorf("unexpected entry ID: %s", entry.ID)
	}
	if entry.CreatedAt.IsZero() || entry.UpdatedAt.IsZero() {
		t.Errorf("timestamps not set: created=%v updated=%v", entry.CreatedAt, entry.UpdatedAt)
	}

	// Get by ID
	got, err := GetSupplychainEntry(db, entry.ID)
	if err != nil {
		t.Fatalf("GetSupplychainEntry: %v", err)
	}
	if got == nil {
		t.Fatal("GetSupplychainEntry returned nil")
	}
	if got.ID != entry.ID {
		t.Errorf("ID mismatch: got %s, want %s", got.ID, entry.ID)
	}
	if got.Name != entry.Name || got.Publisher != entry.Publisher || got.SourceRepo != entry.SourceRepo {
		t.Errorf("field mismatch: got %+v", got)
	}
	if got.ImageRef != entry.ImageRef || got.PinnedDigest != entry.PinnedDigest {
		t.Errorf("image_ref/pinned_digest mismatch: got %+v", got)
	}
	if got.SignatureKind != entry.SignatureKind || got.SourceKind != entry.SourceKind {
		t.Errorf("signature_kind/source_kind mismatch: got %+v", got)
	}
	if got.UnverifiedConfirmed != entry.UnverifiedConfirmed {
		t.Errorf("unverified_confirmed mismatch: got %v, want %v", got.UnverifiedConfirmed, entry.UnverifiedConfirmed)
	}
}

// TestSupplychainEntryListEmpty tests that list returns empty slice, not nil.
func TestSupplychainEntryListEmpty(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	entries, err := ListSupplychainEntries(db)
	if err != nil {
		t.Fatalf("ListSupplychainEntries: %v", err)
	}
	if entries == nil {
		t.Error("ListSupplychainEntries returned nil, expected empty slice")
	}
	if len(entries) != 0 {
		t.Errorf("expected empty list, got %d", len(entries))
	}
}

// TestSupplychainEntryDuplicateName tests that duplicate name returns distinct error.
func TestSupplychainEntryDuplicateName(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	validDigest := "sha256:" + strings.Repeat("a", 64)
	entry1 := &SupplychainEntryRow{
		Name:                "test-app",
		Publisher:           "publisher",
		SourceRepo:          "https://github.com/org/repo",
		ImageRef:            "registry.io/app:latest",
		PinnedDigest:        validDigest,
		SignatureKind:       "cosign",
		SourceKind:          "official",
		UnverifiedConfirmed: false,
	}

	if err := CreateSupplychainEntry(db, entry1); err != nil {
		t.Fatalf("first CreateSupplychainEntry: %v", err)
	}

	// Try to create another entry with the same name
	entry2 := &SupplychainEntryRow{
		Name:                "test-app", // same name
		Publisher:           "publisher2",
		SourceRepo:          "https://github.com/org/repo2",
		ImageRef:            "registry.io/app2:latest",
		PinnedDigest:        "sha256:" + strings.Repeat("b", 64),
		SignatureKind:       "none",
		SourceKind:          "custom",
		UnverifiedConfirmed: true,
	}

	err = CreateSupplychainEntry(db, entry2)
	if err == nil {
		t.Error("CreateSupplychainEntry should fail for duplicate name")
	}
	if !strings.Contains(err.Error(), "duplicate name") {
		t.Errorf("expected duplicate name error, got: %v", err)
	}
}

// TestSupplychainEntryInvalidValidation tests that invalid entries never persist.
func TestSupplychainEntryInvalidValidation(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	validDigest := "sha256:" + strings.Repeat("a", 64)
	baseEntry := &SupplychainEntryRow{
		Name:                "test-app",
		Publisher:           "publisher",
		SourceRepo:          "https://github.com/org/repo",
		ImageRef:            "registry.io/app:latest",
		PinnedDigest:        validDigest,
		SignatureKind:       "cosign",
		SourceKind:          "official",
		UnverifiedConfirmed: false,
	}

	tests := []struct {
		name       string
		modify     func(*SupplychainEntryRow)
		wantErrMsg string
	}{
		{"missing name", func(e *SupplychainEntryRow) { e.Name = "" }, "missing required field"},
		{"missing publisher", func(e *SupplychainEntryRow) { e.Publisher = "" }, "missing required field"},
		{"missing source_repo", func(e *SupplychainEntryRow) { e.SourceRepo = "" }, "missing required field"},
		{"missing image_ref", func(e *SupplychainEntryRow) { e.ImageRef = "" }, "missing required field"},
		{"whitespace-only name", func(e *SupplychainEntryRow) { e.Name = "  " }, "missing required field"},
		{"invalid source kind", func(e *SupplychainEntryRow) { e.SourceKind = "bad" }, "invalid source kind"},
		{"invalid signature kind", func(e *SupplychainEntryRow) { e.SignatureKind = "bad" }, "invalid signature kind"},
		{"invalid pinned digest (floating tag)", func(e *SupplychainEntryRow) { e.PinnedDigest = "latest" }, "invalid pinned digest"},
		{"invalid pinned digest (empty)", func(e *SupplychainEntryRow) { e.PinnedDigest = "" }, "invalid pinned digest"},
		{"invalid pinned digest (uppercase)", func(e *SupplychainEntryRow) { e.PinnedDigest = "sha256:" + strings.Repeat("A", 64) }, "invalid pinned digest"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := *baseEntry // copy
			tc.modify(&entry)
			err := CreateSupplychainEntry(db, &entry)
			if err == nil {
				t.Errorf("CreateSupplychainEntry should fail for %s", tc.name)
				return
			}
			if !strings.Contains(err.Error(), tc.wantErrMsg) {
				t.Errorf("expected error containing %q, got: %v", tc.wantErrMsg, err)
			}
			// Verify nothing was persisted
			entries, _ := ListSupplychainEntries(db)
			if len(entries) != 0 {
				t.Errorf("invalid entry should not persist, but list has %d entries", len(entries))
			}
		})
	}
}

// TestSupplychainEntryDelete tests delete and get 404 after delete.
func TestSupplychainEntryDelete(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	validDigest := "sha256:" + strings.Repeat("a", 64)
	entry := &SupplychainEntryRow{
		Name:                "test-app",
		Publisher:           "publisher",
		SourceRepo:          "https://github.com/org/repo",
		ImageRef:            "registry.io/app:latest",
		PinnedDigest:        validDigest,
		SignatureKind:       "cosign",
		SourceKind:          "official",
		UnverifiedConfirmed: false,
	}

	if err := CreateSupplychainEntry(db, entry); err != nil {
		t.Fatalf("CreateSupplychainEntry: %v", err)
	}

	// Delete
	if err := DeleteSupplychainEntry(db, entry.ID); err != nil {
		t.Fatalf("DeleteSupplychainEntry: %v", err)
	}

	// Get should return nil
	got, err := GetSupplychainEntry(db, entry.ID)
	if err != nil {
		t.Fatalf("GetSupplychainEntry after delete: %v", err)
	}
	if got != nil {
		t.Error("GetSupplychainEntry should return nil after delete")
	}

	// Delete missing should return ErrNoRows
	err = DeleteSupplychainEntry(db, entry.ID)
	if err == nil {
		t.Error("DeleteSupplychainEntry should fail for missing entry")
	}
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got: %v", err)
	}
}

// TestSupplychainEntryRestartPersistence tests that entries survive a DB close/reopen cycle.
func TestSupplychainEntryRestartPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	validDigest := "sha256:" + strings.Repeat("a", 64)
	entry := &SupplychainEntryRow{
		Name:                "test-app",
		Publisher:           "publisher",
		SourceRepo:          "https://github.com/org/repo",
		ImageRef:            "registry.io/app:latest",
		PinnedDigest:        validDigest,
		SignatureKind:       "cosign",
		SourceKind:          "official",
		UnverifiedConfirmed: false,
	}

	if err := CreateSupplychainEntry(db, entry); err != nil {
		t.Fatalf("CreateSupplychainEntry: %v", err)
	}
	entryID := entry.ID

	db.Close()

	// Reopen and verify persistence
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	got, err := GetSupplychainEntry(db2, entryID)
	if err != nil {
		t.Fatalf("GetSupplychainEntry: %v", err)
	}
	if got == nil {
		t.Fatal("entry not persisted across restart")
	}
	if got.Name != "test-app" || got.PinnedDigest != validDigest {
		t.Errorf("entry data mismatch: %+v", got)
	}
}

// ============================================================================
// WS19: UPS events catalog tests (FR-1.12)
// ============================================================================

// TestUPSMigration tests that migration to v12 creates the ups_events table.
func TestUPSMigration(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Verify ups_events table exists
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", "ups_events").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("table ups_events not created")
	}

	// Verify schema version is 12
	var version int
	err = db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&version)
	if err != nil || version != schemaVersion {
		t.Errorf("schema_version: got %d, want %d", version, schemaVersion)
	}

	// Verify index exists
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", "idx_ups_events_ts").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("index idx_ups_events_ts not created")
	}
}

// TestUPSEventInsertList tests insert and list roundtrip newest-first with total.
func TestUPSEventInsertList(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Insert 3 events
	events := []*UPSEventRow{
		{Kind: "on_battery", State: "on_battery", ChargePercent: 90, Detail: "state=on_battery charge=90"},
		{Kind: "critical_shutdown", State: "on_battery", ChargePercent: 15, Detail: "state=on_battery charge=15"},
		{Kind: "online_restored", State: "online", ChargePercent: 100, Detail: "state=online charge=100"},
	}
	for _, e := range events {
		if err := InsertUPSEvent(db, e); err != nil {
			t.Fatalf("InsertUPSEvent: %v", err)
		}
		if e.ID == "" || !strings.HasPrefix(e.ID, "upse-") {
			t.Errorf("unexpected event ID: %s", e.ID)
		}
		if e.TS.IsZero() {
			t.Errorf("timestamp not set: %v", e.TS)
		}
	}

	// List all - newest first
	list, total, err := ListUPSEvents(db, 0, 0)
	if err != nil {
		t.Fatalf("ListUPSEvents: %v", err)
	}
	if total != 3 || len(list) != 3 {
		t.Errorf("expected 3 events, got total=%d len=%d", total, len(list))
	}
	// Newest first: online_restored (3rd inserted) should be first
	if list[0].Kind != "online_restored" || list[1].Kind != "critical_shutdown" || list[2].Kind != "on_battery" {
		t.Errorf("unexpected order: %v, %v, %v", list[0].Kind, list[1].Kind, list[2].Kind)
	}

	// Pagination
	page1, total1, err := ListUPSEvents(db, 1, 0)
	if err != nil {
		t.Fatalf("ListUPSEvents page1: %v", err)
	}
	if total1 != 3 || len(page1) != 1 || page1[0].Kind != "online_restored" {
		t.Errorf("page1: total=%d len=%d first=%s", total1, len(page1), page1[0].Kind)
	}

	page2, total2, err := ListUPSEvents(db, 1, 1)
	if err != nil {
		t.Fatalf("ListUPSEvents page2: %v", err)
	}
	if total2 != 3 || len(page2) != 1 || page2[0].Kind != "critical_shutdown" {
		t.Errorf("page2: total=%d len=%d first=%s", total2, len(page2), page2[0].Kind)
	}

	// Empty list check - use a fresh DB
	db2, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open2: %v", err)
	}
	defer db2.Close()
	if err := Migrate(db2); err != nil {
		t.Fatalf("Migrate2: %v", err)
	}
	emptyList, emptyTotal, err := ListUPSEvents(db2, 0, 0)
	if err != nil {
		t.Fatalf("ListUPSEvents empty: %v", err)
	}
	if emptyTotal != 0 || len(emptyList) != 0 {
		t.Errorf("expected empty list, got total=%d len=%d", emptyTotal, len(emptyList))
	}
	if emptyList == nil {
		t.Errorf("empty list should be empty slice, not nil")
	}
}

// TestUPSEventRestartPersistence tests that events survive a DB close/reopen cycle.
func TestUPSEventRestartPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	event := &UPSEventRow{
		Kind:          "on_battery",
		State:         "on_battery",
		ChargePercent: 90,
		Detail:        "state=on_battery charge=90",
	}

	if err := InsertUPSEvent(db, event); err != nil {
		t.Fatalf("InsertUPSEvent: %v", err)
	}
	eventID := event.ID

	db.Close()

	// Reopen and verify persistence
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	list, total, err := ListUPSEvents(db2, 0, 0)
	if err != nil {
		t.Fatalf("ListUPSEvents: %v", err)
	}
	if total != 1 || len(list) != 1 {
		t.Fatalf("expected 1 event, got total=%d len=%d", total, len(list))
	}
	if list[0].ID != eventID || list[0].Kind != "on_battery" || list[0].ChargePercent != 90 {
		t.Errorf("event data mismatch: %+v", list[0])
	}
}

// ============================================================================
// WS20 LANGKAH-B (2026-11-02): Import wiring (FR-1.10)
// ============================================================================

// TestImportMigration tests that migration to v13 creates the imports table.
func TestImportMigration(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Verify imports table exists
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", "imports").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("table imports not created")
	}

	// Verify schema version is 13
	var version int
	err = db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&version)
	if err != nil || version != schemaVersion {
		t.Errorf("schema_version: got %d, want %d", version, schemaVersion)
	}

	// Verify indexes exist
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", "idx_imports_collection").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("index idx_imports_collection not created")
	}
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", "idx_imports_ts").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("index idx_imports_ts not created")
	}
}

// TestImportInsertList tests insert and list roundtrip newest-first with total.
func TestImportInsertList(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Insert 3 imports for the same collection
	imports := []*ImportRow{
		{CollectionID: "coll-1", SourcePath: "/src/a", FSType: "ntfs", TotalFiles: 10, TotalBytes: 1000, Copied: 10, Failed: 0, BytesCopied: 1000},
		{CollectionID: "coll-1", SourcePath: "/src/b", FSType: "ext4", TotalFiles: 5, TotalBytes: 500, Copied: 5, Failed: 0, BytesCopied: 500},
		{CollectionID: "coll-1", SourcePath: "/src/c", FSType: "xfs", TotalFiles: 3, TotalBytes: 300, Copied: 2, Failed: 1, BytesCopied: 200},
	}
	for _, imp := range imports {
		if err := InsertImport(db, imp); err != nil {
			t.Fatalf("InsertImport: %v", err)
		}
		if imp.ID == "" || !strings.HasPrefix(imp.ID, "imp-") {
			t.Errorf("unexpected import ID: %s", imp.ID)
		}
		if imp.TS.IsZero() {
			t.Errorf("timestamp not set: %v", imp.TS)
		}
	}

	// Insert 1 import for a different collection (should not appear in coll-1 list)
	otherImport := &ImportRow{CollectionID: "coll-2", SourcePath: "/src/d", FSType: "ntfs", TotalFiles: 1, TotalBytes: 100, Copied: 1, Failed: 0, BytesCopied: 100}
	if err := InsertImport(db, otherImport); err != nil {
		t.Fatalf("InsertImport other: %v", err)
	}

	// List all for coll-1 - newest first
	list, total, err := ListImports(db, "coll-1", 0, 0)
	if err != nil {
		t.Fatalf("ListImports: %v", err)
	}
	if total != 3 || len(list) != 3 {
		t.Errorf("expected 3 imports for coll-1, got total=%d len=%d", total, len(list))
	}
	// Newest first: /src/c (3rd inserted) should be first
	if list[0].SourcePath != "/src/c" || list[1].SourcePath != "/src/b" || list[2].SourcePath != "/src/a" {
		t.Errorf("unexpected order: %v, %v, %v", list[0].SourcePath, list[1].SourcePath, list[2].SourcePath)
	}

	// Verify total counts only coll-1's rows
	if total != 3 {
		t.Errorf("total should be 3 (only coll-1), got %d", total)
	}

	// Pagination
	page1, total1, err := ListImports(db, "coll-1", 1, 0)
	if err != nil {
		t.Fatalf("ListImports page1: %v", err)
	}
	if total1 != 3 || len(page1) != 1 || page1[0].SourcePath != "/src/c" {
		t.Errorf("page1: total=%d len=%d first=%s", total1, len(page1), page1[0].SourcePath)
	}

	page2, total2, err := ListImports(db, "coll-1", 1, 1)
	if err != nil {
		t.Fatalf("ListImports page2: %v", err)
	}
	if total2 != 3 || len(page2) != 1 || page2[0].SourcePath != "/src/b" {
		t.Errorf("page2: total=%d len=%d first=%s", total2, len(page2), page2[0].SourcePath)
	}

	// Limit clamp behavior (same as ListUPSEvents)
	pageLarge, totalLarge, err := ListImports(db, "coll-1", 2000, 0)
	if err != nil {
		t.Fatalf("ListImports large limit: %v", err)
	}
	if len(pageLarge) != 3 {
		t.Errorf("limit clamp: expected 3 (max 1000), got %d", len(pageLarge))
	}
	if totalLarge != 3 {
		t.Errorf("total unchanged by limit: got %d", totalLarge)
	}

	// Empty list check - use a fresh DB
	db2, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open2: %v", err)
	}
	defer db2.Close()
	if err := Migrate(db2); err != nil {
		t.Fatalf("Migrate2: %v", err)
	}
	emptyList, emptyTotal, err := ListImports(db2, "coll-nonexistent", 0, 0)
	if err != nil {
		t.Fatalf("ListImports empty: %v", err)
	}
	if emptyTotal != 0 || len(emptyList) != 0 {
		t.Errorf("expected empty list, got total=%d len=%d", emptyTotal, len(emptyList))
	}
	if emptyList == nil {
		t.Errorf("empty list should be empty slice, not nil")
	}
}

// TestImportSameSecondOrdering tests that imports with same timestamp are ordered by rowid DESC.
func TestImportSameSecondOrdering(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Insert multiple imports with the same timestamp (manually set TS to same second)
	baseTime := time.Unix(1700000000, 0)
	imports := []*ImportRow{
		{CollectionID: "coll-1", SourcePath: "/src/first", FSType: "ntfs", TotalFiles: 1, TotalBytes: 100, Copied: 1, Failed: 0, BytesCopied: 100, TS: baseTime},
		{CollectionID: "coll-1", SourcePath: "/src/second", FSType: "ntfs", TotalFiles: 1, TotalBytes: 100, Copied: 1, Failed: 0, BytesCopied: 100, TS: baseTime},
		{CollectionID: "coll-1", SourcePath: "/src/third", FSType: "ntfs", TotalFiles: 1, TotalBytes: 100, Copied: 1, Failed: 0, BytesCopied: 100, TS: baseTime},
	}
	for _, imp := range imports {
		if err := InsertImport(db, imp); err != nil {
			t.Fatalf("InsertImport: %v", err)
		}
	}

	// List - should be ordered by rowid DESC (newest insert first)
	list, total, err := ListImports(db, "coll-1", 0, 0)
	if err != nil {
		t.Fatalf("ListImports: %v", err)
	}
	if total != 3 || len(list) != 3 {
		t.Errorf("expected 3 imports, got total=%d len=%d", total, len(list))
	}
	// rowid DESC means last inserted (third) comes first
	if list[0].SourcePath != "/src/third" || list[1].SourcePath != "/src/second" || list[2].SourcePath != "/src/first" {
		t.Errorf("same-second ordering by rowid DESC failed: %v, %v, %v", list[0].SourcePath, list[1].SourcePath, list[2].SourcePath)
	}
}

// TestImportRestartPersistence tests that imports survive a DB close/reopen cycle.
func TestImportRestartPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	imp := &ImportRow{
		CollectionID: "coll-1",
		SourcePath:   "/src/test",
		FSType:       "ntfs",
		TotalFiles:   5,
		TotalBytes:   500,
		Copied:       5,
		Failed:       0,
		BytesCopied:  500,
	}

	if err := InsertImport(db, imp); err != nil {
		t.Fatalf("InsertImport: %v", err)
	}
	importID := imp.ID

	db.Close()

	// Reopen and verify persistence
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	list, total, err := ListImports(db2, "coll-1", 0, 0)
	if err != nil {
		t.Fatalf("ListImports: %v", err)
	}
	if total != 1 || len(list) != 1 {
		t.Fatalf("expected 1 import, got total=%d len=%d", total, len(list))
	}
	if list[0].ID != importID || list[0].SourcePath != "/src/test" || list[0].FSType != "ntfs" {
		t.Errorf("import data mismatch: %+v", list[0])
	}
}

// ============================================================================
// WS27 (2026-10-31): Heat catalog from access log (FR-1.3)
// ============================================================================

// TestAccessEventsMigration tests that migration to v17 creates the access_events table and indexes.
// Note: current schemaVersion is 18 (WS29 added pool name uniqueness), but this test
// verifies the v17 migration step still works correctly within the full migration chain.
func TestAccessEventsMigration(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Verify access_events table exists
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", "access_events").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("table access_events not created")
	}

	// Verify schema version is 17
	var version int
	err = db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&version)
	if err != nil || version != schemaVersion {
		t.Errorf("schema_version: got %d, want %d", version, schemaVersion)
	}

	// Verify indexes exist
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", "idx_access_events_collection_path_time").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("index idx_access_events_collection_path_time not created")
	}
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", "idx_access_events_collection_time").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("index idx_access_events_collection_time not created")
	}
}

// TestAccessEventsMigrationIdempotent tests that running migration twice doesn't fail.
// Note: current schemaVersion is 18 (WS29 added pool name uniqueness), but this test
// verifies the full migration chain is idempotent.
func TestAccessEventsMigrationIdempotent(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Run migrate twice
	if err := Migrate(db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	// Verify schema version is still 18 (current schemaVersion)
	var version int
	err = db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&version)
	if err != nil || version != schemaVersion {
		t.Errorf("schema_version after second migrate: got %d, want %d", version, schemaVersion)
	}
}

// TestRecordAccessEvent tests recording access events.
func TestRecordAccessEvent(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Create pool and collection
	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	coll := &CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	// Record access events with small delays to ensure different timestamps
	RecordAccessEvent(db, coll.ID, "test.txt", "user1", "preview")
	time.Sleep(10 * time.Millisecond)
	RecordAccessEvent(db, coll.ID, "test.txt", "user1", "content")
	time.Sleep(10 * time.Millisecond)
	RecordAccessEvent(db, coll.ID, "other.txt", "share:abc12345", "share")

	// Verify events were recorded
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM access_events WHERE collection_id = ?", coll.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count access_events: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 access events, got %d", count)
	}

	// Verify event details (order by accessed_at)
	rows, err := db.Query("SELECT rel_path, actor, kind FROM access_events WHERE collection_id = ? ORDER BY accessed_at, rowid", coll.ID)
	if err != nil {
		t.Fatalf("query access_events: %v", err)
	}
	defer rows.Close()

	var events []struct {
		relPath string
		actor   string
		kind    string
	}
	for rows.Next() {
		var e struct {
			relPath string
			actor   string
			kind    string
		}
		if err := rows.Scan(&e.relPath, &e.actor, &e.kind); err != nil {
			t.Fatalf("scan access_event: %v", err)
		}
		events = append(events, e)
	}

	if len(events) != 3 {
		t.Errorf("expected 3 events, got %d", len(events))
	}
	// First two should be test.txt (preview then content), third should be other.txt (share)
	if events[0].relPath != "test.txt" || events[0].actor != "user1" || events[0].kind != "preview" {
		t.Errorf("event 0 mismatch: %+v", events[0])
	}
	if events[1].relPath != "test.txt" || events[1].actor != "user1" || events[1].kind != "content" {
		t.Errorf("event 1 mismatch: %+v", events[1])
	}
	if events[2].relPath != "other.txt" || events[2].actor != "share:abc12345" || events[2].kind != "share" {
		t.Errorf("event 2 mismatch: %+v", events[2])
	}
}

// TestFileHeat tests the FileHeat function.
func TestFileHeat(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Create pool and collection
	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	coll := &CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	// Use time.Now() as reference for all timestamps to match FileHeat's cutoff logic
	now := time.Now().Unix()
	day := int64(24 * 60 * 60)

	// 3 accesses within 30 days (1, 2, 5 days ago)
	_, err = db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
		"acc-1", coll.ID, "test.txt", "user1", now-day, "preview")
	if err != nil {
		t.Fatalf("insert access 1: %v", err)
	}
	_, err = db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
		"acc-2", coll.ID, "test.txt", "user1", now-2*day, "content")
	if err != nil {
		t.Fatalf("insert access 2: %v", err)
	}
	_, err = db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
		"acc-3", coll.ID, "test.txt", "user2", now-5*day, "preview")
	if err != nil {
		t.Fatalf("insert access 3: %v", err)
	}

	// 1 access outside 30 days (40 days ago)
	_, err = db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
		"acc-4", coll.ID, "test.txt", "user1", now-40*day, "preview")
	if err != nil {
		t.Fatalf("insert access 4: %v", err)
	}

	// Different file (1 day ago)
	_, err = db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
		"acc-5", coll.ID, "other.txt", "user1", now-day, "preview")
	if err != nil {
		t.Fatalf("insert access 5: %v", err)
	}

	// Test FileHeat with default window (30 days)
	count, lastAccess, err := FileHeat(db, coll.ID, "test.txt", 0)
	if err != nil {
		t.Fatalf("FileHeat: %v", err)
	}
	if count != 3 {
		t.Errorf("expected count=3 within 30 days, got %d", count)
	}
	if lastAccess != now-day {
		t.Errorf("expected last_access=%d (most recent), got %d", now-day, lastAccess)
	}

	// Test FileHeat with explicit 30-day window
	count, lastAccess, err = FileHeat(db, coll.ID, "test.txt", 30)
	if err != nil {
		t.Fatalf("FileHeat 30 days: %v", err)
	}
	if count != 3 {
		t.Errorf("expected count=3 within 30 days, got %d", count)
	}

	// Test FileHeat with 10-day window (should only count accesses within 10 days: 1, 2, 5 days ago = 3)
	// Note: all 3 are within 10 days, so count should be 3
	count, lastAccess, err = FileHeat(db, coll.ID, "test.txt", 10)
	if err != nil {
		t.Fatalf("FileHeat 10 days: %v", err)
	}
	if count != 3 {
		t.Errorf("expected count=3 within 10 days (1,2,5 days ago), got %d", count)
	}
	if lastAccess != now-day {
		t.Errorf("expected last_access=%d, got %d", now-day, lastAccess)
	}

	// Test FileHeat with 3-day window (should only count 1 and 2 days ago = 2)
	count, lastAccess, err = FileHeat(db, coll.ID, "test.txt", 3)
	if err != nil {
		t.Fatalf("FileHeat 3 days: %v", err)
	}
	if count != 2 {
		t.Errorf("expected count=2 within 3 days (1,2 days ago), got %d", count)
	}
	if lastAccess != now-day {
		t.Errorf("expected last_access=%d, got %d", now-day, lastAccess)
	}

	// Test FileHeat for other.txt (1 access in 30 days)
	count, lastAccess, err = FileHeat(db, coll.ID, "other.txt", 30)
	if err != nil {
		t.Fatalf("FileHeat other.txt: %v", err)
	}
	if count != 1 {
		t.Errorf("expected count=1 for other.txt, got %d", count)
	}

	// Test FileHeat for non-existent file (0 count)
	count, lastAccess, err = FileHeat(db, coll.ID, "nonexistent.txt", 30)
	if err != nil {
		t.Fatalf("FileHeat nonexistent: %v", err)
	}
	if count != 0 {
		t.Errorf("expected count=0 for nonexistent file, got %d", count)
	}
	if lastAccess != 0 {
		t.Errorf("expected last_access=0 for nonexistent file, got %d", lastAccess)
	}
}

// TestTopHeated tests the TopHeated function.
func TestTopHeated(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Create pool and collection
	pool := &PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	coll := &CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	// Insert access events with different counts
	now := time.Now().Unix()
	day := int64(24 * 60 * 60)

	// file_a.txt: 5 accesses
	for i := 0; i < 5; i++ {
		_, err = db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("acc-a-%d", i), coll.ID, "file_a.txt", "user1", now-int64(i)*day, "preview")
		if err != nil {
			t.Fatalf("insert access a-%d: %v", i, err)
		}
	}

	// file_b.txt: 3 accesses
	for i := 0; i < 3; i++ {
		_, err = db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("acc-b-%d", i), coll.ID, "file_b.txt", "user1", now-int64(i)*day, "content")
		if err != nil {
			t.Fatalf("insert access b-%d: %v", i, err)
		}
	}

	// file_c.txt: 3 accesses but older last access
	for i := 0; i < 3; i++ {
		_, err = db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("acc-c-%d", i), coll.ID, "file_c.txt", "user1", now-10*day-int64(i)*day, "preview")
		if err != nil {
			t.Fatalf("insert access c-%d: %v", i, err)
		}
	}

	// file_d.txt: 1 access outside 30 days (should not count)
	_, err = db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
		"acc-d-1", coll.ID, "file_d.txt", "user1", now-40*day, "preview")
	if err != nil {
		t.Fatalf("insert access d: %v", err)
	}

	// Test TopHeated with default limit (50)
	entries, total, err := TopHeated(db, coll.ID, 0)
	if err != nil {
		t.Fatalf("TopHeated: %v", err)
	}
	if total != 3 {
		t.Errorf("expected total=3 distinct files in window, got %d", total)
	}
	if len(entries) != 3 {
		t.Errorf("expected 3 entries, got %d", len(entries))
	}

	// Verify order: file_a.txt (5) first, then file_b.txt (3, newer), then file_c.txt (3, older)
	if entries[0].RelPath != "file_a.txt" || entries[0].Count != 5 {
		t.Errorf("expected first entry file_a.txt count=5, got %+v", entries[0])
	}
	if entries[1].RelPath != "file_b.txt" || entries[1].Count != 3 {
		t.Errorf("expected second entry file_b.txt count=3, got %+v", entries[1])
	}
	if entries[2].RelPath != "file_c.txt" || entries[2].Count != 3 {
		t.Errorf("expected third entry file_c.txt count=3, got %+v", entries[2])
	}

	// Verify last_access ordering for same count (file_b.txt newer than file_c.txt)
	if entries[1].LastAccess <= entries[2].LastAccess {
		t.Errorf("expected file_b.txt last_access > file_c.txt last_access for tie-break: %d vs %d", entries[1].LastAccess, entries[2].LastAccess)
	}

	// Test limit
	entries, total, err = TopHeated(db, coll.ID, 2)
	if err != nil {
		t.Fatalf("TopHeated limit 2: %v", err)
	}
	if total != 3 {
		t.Errorf("total should still be 3, got %d", total)
	}
	if len(entries) != 2 {
		t.Errorf("expected 2 entries with limit=2, got %d", len(entries))
	}
	if entries[0].RelPath != "file_a.txt" || entries[1].RelPath != "file_b.txt" {
		t.Errorf("limit 2 entries mismatch: %+v, %+v", entries[0], entries[1])
	}

	// Test limit clamp (max 500)
	entries, total, err = TopHeated(db, coll.ID, 1000)
	if err != nil {
		t.Fatalf("TopHeated limit 1000: %v", err)
	}
	if len(entries) != 3 {
		t.Errorf("expected 3 entries (clamped to 500, but only 3 exist), got %d", len(entries))
	}

	// Test empty collection
	entries, total, err = TopHeated(db, "coll-nonexistent", 50)
	if err != nil {
		t.Fatalf("TopHeated empty: %v", err)
	}
	if total != 0 || len(entries) != 0 {
		t.Errorf("expected empty for nonexistent collection, got total=%d len=%d", total, len(entries))
	}
	if entries == nil {
		t.Errorf("entries should be empty slice, not nil")
	}
}
