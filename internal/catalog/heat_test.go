package catalog

import (
	"database/sql"
	"testing"
	"time"
)

func setupHeatTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func insertPoolAndCollections(t *testing.T, db *sql.DB, collIDs ...string) {
	t.Helper()
	now := time.Now().Unix()
	_, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"pool-1", "media", "local", "warm", "single", "single", "/mnt/media", 4000787030016, 0, 0, "", "active", now, now)
	if err != nil {
		t.Fatalf("insert pool: %v", err)
	}
	for _, id := range collIDs {
		_, err := db.Exec(`INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, last_accessed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, "coll-"+id, "pool-1", "/"+id, "sda", 0, nil, now, now)
		if err != nil {
			t.Fatalf("insert collection %s: %v", id, err)
		}
	}
}

func insertFiles(t *testing.T, db *sql.DB, collectionID string, sizes ...int64) {
	t.Helper()
	now := time.Now().Unix()
	for i, size := range sizes {
		id := "file-" + collectionID + "-" + string(rune('a'+i))
		_, err := db.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, collectionID, string(rune('a'+i))+".txt", size, now, "sda", now, now)
		if err != nil {
			t.Fatalf("insert file: %v", err)
		}
	}
}

func insertAccessEvents(t *testing.T, db *sql.DB, collectionID string, events ...struct {
	kind       string
	accessedAt int64
}) {
	t.Helper()
	for i, e := range events {
		id := "acc-" + collectionID + "-" + string(rune('a'+i))
		_, err := db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
			id, collectionID, "file.txt", "test-user", e.accessedAt, e.kind)
		if err != nil {
			t.Fatalf("insert access event: %v", err)
		}
	}
}

// TestComputeHeatScoresBasic tests scoring math with fixed now.
func TestComputeHeatScoresBasic(t *testing.T) {
	db := setupHeatTestDB(t)
	defer db.Close()

	insertPoolAndCollections(t, db, "coll-1", "coll-2")

	now := time.Unix(1700000000, 0) // fixed now

	// coll-1: content event 7 days ago (weight 2, ageDays=7 → 0.5^(7/7) = 0.5 → score 1.0)
	// coll-1: preview event 0 days ago (weight 1, ageDays=0 → 0.5^0 = 1 → score 1.0)
	// coll-1 total: 2.0
	insertAccessEvents(t, db, "coll-1",
		struct{ kind string; accessedAt int64 }{"content", now.Add(-7 * 24 * time.Hour).Unix()},
		struct{ kind string; accessedAt int64 }{"preview", now.Unix()},
	)

	// coll-2: zero events → score 0 row

	written, err := ComputeHeatScores(db, now)
	if err != nil {
		t.Fatalf("ComputeHeatScores: %v", err)
	}
	if written != 2 {
		t.Errorf("written: got %d, want 2", written)
	}

	// Verify coll-1 score
	heat, err := GetCollectionHeat(db, "coll-1")
	if err != nil {
		t.Fatalf("GetCollectionHeat: %v", err)
	}
	if heat == nil {
		t.Fatal("GetCollectionHeat returned nil")
	}
	if heat.Score != 2.0 {
		t.Errorf("coll-1 score: got %v, want 2.0", heat.Score)
	}
	if heat.EventCount != 2 {
		t.Errorf("coll-1 event_count: got %d, want 2", heat.EventCount)
	}
	if heat.LastAccess != now.Unix() {
		t.Errorf("coll-1 last_access: got %d, want %d", heat.LastAccess, now.Unix())
	}
	if heat.ComputedAt.Unix() != now.Unix() {
		t.Errorf("coll-1 computed_at: got %d, want %d", heat.ComputedAt.Unix(), now.Unix())
	}

	// Verify coll-2 score (zero events)
	heat2, err := GetCollectionHeat(db, "coll-2")
	if err != nil {
		t.Fatalf("GetCollectionHeat coll-2: %v", err)
	}
	if heat2 == nil {
		t.Fatal("GetCollectionHeat coll-2 returned nil")
	}
	if heat2.Score != 0 {
		t.Errorf("coll-2 score: got %v, want 0", heat2.Score)
	}
	if heat2.EventCount != 0 {
		t.Errorf("coll-2 event_count: got %d, want 0", heat2.EventCount)
	}
	if heat2.LastAccess != 0 {
		t.Errorf("coll-2 last_access: got %d, want 0", heat2.LastAccess)
	}
}

// TestComputeHeatScoresRecency tests that older event weighs less than newer identical one.
func TestComputeHeatScoresRecency(t *testing.T) {
	db := setupHeatTestDB(t)
	defer db.Close()

	insertPoolAndCollections(t, db, "coll-a", "coll-b")

	now := time.Unix(1700000000, 0)

	// coll-a: content event 0 days ago → weight 2 × 0.5^0 = 2.0
	insertAccessEvents(t, db, "coll-a",
		struct{ kind string; accessedAt int64 }{"content", now.Unix()},
	)

	// coll-b: content event 14 days ago → weight 2 × 0.5^(14/7) = 2 × 0.25 = 0.5
	insertAccessEvents(t, db, "coll-b",
		struct{ kind string; accessedAt int64 }{"content", now.Add(-14 * 24 * time.Hour).Unix()},
	)

	if _, err := ComputeHeatScores(db, now); err != nil {
		t.Fatalf("ComputeHeatScores: %v", err)
	}

	heatA, err := GetCollectionHeat(db, "coll-a")
	if err != nil {
		t.Fatalf("GetCollectionHeat coll-a: %v", err)
	}
	heatB, err := GetCollectionHeat(db, "coll-b")
	if err != nil {
		t.Fatalf("GetCollectionHeat coll-b: %v", err)
	}

	if heatA.Score <= heatB.Score {
		t.Errorf("newer event should score higher: coll-a=%v coll-b=%v", heatA.Score, heatB.Score)
	}
}

// TestComputeHeatScoresSizeAggregation tests size_bytes aggregation across files.
func TestComputeHeatScoresSizeAggregation(t *testing.T) {
	db := setupHeatTestDB(t)
	defer db.Close()

	insertPoolAndCollections(t, db, "coll-1")
	insertFiles(t, db, "coll-1", 100, 200, 300)

	now := time.Unix(1700000000, 0)
	if _, err := ComputeHeatScores(db, now); err != nil {
		t.Fatalf("ComputeHeatScores: %v", err)
	}

	heat, err := GetCollectionHeat(db, "coll-1")
	if err != nil {
		t.Fatalf("GetCollectionHeat: %v", err)
	}
	if heat.SizeBytes != 600 {
		t.Errorf("size_bytes: got %d, want 600", heat.SizeBytes)
	}
}

// TestComputeHeatScoresIdempotent tests recompute at same now is idempotent.
func TestComputeHeatScoresIdempotent(t *testing.T) {
	db := setupHeatTestDB(t)
	defer db.Close()

	insertPoolAndCollections(t, db, "coll-1")
	insertAccessEvents(t, db, "coll-1",
		struct{ kind string; accessedAt int64 }{"content", time.Unix(1700000000, 0).Unix()},
	)

	now := time.Unix(1700000000, 0)

	written1, err := ComputeHeatScores(db, now)
	if err != nil {
		t.Fatalf("first ComputeHeatScores: %v", err)
	}
	written2, err := ComputeHeatScores(db, now)
	if err != nil {
		t.Fatalf("second ComputeHeatScores: %v", err)
	}

	if written1 != written2 {
		t.Errorf("idempotent: written1=%d written2=%d", written1, written2)
	}

	heat, err := GetCollectionHeat(db, "coll-1")
	if err != nil {
		t.Fatalf("GetCollectionHeat: %v", err)
	}
	if heat.ComputedAt.Unix() != now.Unix() {
		t.Errorf("computed_at: got %d, want %d", heat.ComputedAt.Unix(), now.Unix())
	}
}

// TestGetCollectionHeatNotFound returns nil for nonexistent collection.
func TestGetCollectionHeatNotFound(t *testing.T) {
	db := setupHeatTestDB(t)
	defer db.Close()

	heat, err := GetCollectionHeat(db, "nonexistent")
	if err != nil {
		t.Fatalf("GetCollectionHeat: %v", err)
	}
	if heat != nil {
		t.Errorf("expected nil for nonexistent collection, got %+v", heat)
	}
}

// TestGetCollectionHeatZeroRow returns zero score object when collection exists but no heat row.
func TestGetCollectionHeatZeroRow(t *testing.T) {
	db := setupHeatTestDB(t)
	defer db.Close()

	insertPoolAndCollections(t, db, "coll-1")

	heat, err := GetCollectionHeat(db, "coll-1")
	if err != nil {
		t.Fatalf("GetCollectionHeat: %v", err)
	}
	if heat == nil {
		t.Fatal("expected non-nil for existing collection without heat row")
	}
	if heat.Score != 0 {
		t.Errorf("score: got %v, want 0", heat.Score)
	}
	if heat.ComputedAt.Unix() != 0 {
		t.Errorf("computed_at: got %d, want 0", heat.ComputedAt.Unix())
	}
}

// TestCollectionHeatMigration tests that migration to v20 creates the collection_heat table.
func TestCollectionHeatMigration(t *testing.T) {
	db := setupHeatTestDB(t)
	defer db.Close()

	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", "collection_heat").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("table collection_heat not created")
	}

	var version int
	err = db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&version)
	if err != nil || version != schemaVersion {
		t.Errorf("schema_version: got %d, want %d", version, schemaVersion)
	}
}
