package catalog

import (
	"database/sql"
	"testing"
	"time"
)

func setupCuratorTestDB(t *testing.T) *sql.DB {
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

func insertCuratorPoolAndCollections(t *testing.T, db *sql.DB, pools map[string]string, colls map[string]struct {
	poolID   string
	tier     string
	lastAcc  int64
	size     int64
}) {
	t.Helper()
	now := time.Now().Unix()
	for poolID, tier := range pools {
		_, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			poolID, "pool-"+poolID, "local", tier, "single", "single", "/mnt/"+poolID, 4000787030016, 0, 0, "", "active", now, now)
		if err != nil {
			t.Fatalf("insert pool %s: %v", poolID, err)
		}
	}
	for collID, c := range colls {
		var lastAcc interface{}
		if c.lastAcc > 0 {
			lastAcc = c.lastAcc
		}
		_, err := db.Exec(`INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, last_accessed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			collID, "coll-"+collID, c.poolID, "/"+collID, "sda", c.size, lastAcc, now, now)
		if err != nil {
			t.Fatalf("insert collection %s: %v", collID, err)
		}
		// Also insert into collection_heat so GenerateRecommendations sees last_access
		var heatLastAcc int64
		if c.lastAcc > 0 {
			heatLastAcc = c.lastAcc
		}
		_, err = db.Exec(`INSERT INTO collection_heat (collection_id, last_access, score, event_count, size_bytes, computed_at) VALUES (?, ?, 0, 0, ?, 0)`,
			collID, heatLastAcc, c.size)
		if err != nil {
			t.Fatalf("insert collection_heat %s: %v", collID, err)
		}
	}
}

func insertCuratorAccessEvents(t *testing.T, db *sql.DB, collectionID string, count int, baseTime int64) {
	t.Helper()
	for i := 0; i < count; i++ {
		id := "acc-" + collectionID + "-" + string(rune('a'+i))
		_, err := db.Exec(`INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
			id, collectionID, "file.txt", "test-user", baseTime, "content")
		if err != nil {
			t.Fatalf("insert access event: %v", err)
		}
	}
}

// TestCuratorWarmIdleQualifies tests that a warm collection idle beyond
// cold_threshold_days gets a warm_to_cold recommendation.
func TestCuratorWarmIdleQualifies(t *testing.T) {
	db := setupCuratorTestDB(t)
	defer db.Close()

	now := time.Unix(1700000000, 0)
	// 30 days ago, well beyond default cold_threshold_days (21)
	lastAcc := now.Add(-30 * 24 * time.Hour).Unix()

	insertCuratorPoolAndCollections(t, db,
		map[string]string{"pool-1": "warm"},
		map[string]struct {
			poolID   string
			tier     string
			lastAcc  int64
			size     int64
		}{
			"coll-1": {poolID: "pool-1", tier: "warm", lastAcc: lastAcc, size: 1000},
		})

	created, err := GenerateRecommendations(db, now)
	if err != nil {
		t.Fatalf("GenerateRecommendations: %v", err)
	}
	if created != 1 {
		t.Errorf("created: got %d, want 1", created)
	}

	recs, total, err := ListRecommendations(db, "pending", "")
	if err != nil {
		t.Fatalf("ListRecommendations: %v", err)
	}
	if total != 1 || len(recs) != 1 {
		t.Errorf("recommendations: got %d (len %d), want 1", total, len(recs))
	}
	if recs[0].Direction != "warm_to_cold" {
		t.Errorf("direction: got %s, want warm_to_cold", recs[0].Direction)
	}
	if recs[0].Reason != "idle_30d" {
		t.Errorf("reason: got %s, want idle_30d", recs[0].Reason)
	}
	if recs[0].Status != "pending" {
		t.Errorf("status: got %s, want pending", recs[0].Status)
	}
}

// TestCuratorWarmRecentDoesNotQualifies tests that a warm collection accessed
// recently does not get a recommendation.
func TestCuratorWarmRecentDoesNotQualifies(t *testing.T) {
	db := setupCuratorTestDB(t)
	defer db.Close()

	now := time.Unix(1700000000, 0)
	// 5 days ago, within default cold_threshold_days (21)
	lastAcc := now.Add(-5 * 24 * time.Hour).Unix()

	insertCuratorPoolAndCollections(t, db,
		map[string]string{"pool-1": "warm"},
		map[string]struct {
			poolID   string
			tier     string
			lastAcc  int64
			size     int64
		}{
			"coll-1": {poolID: "pool-1", tier: "warm", lastAcc: lastAcc, size: 1000},
		})

	created, err := GenerateRecommendations(db, now)
	if err != nil {
		t.Fatalf("GenerateRecommendations: %v", err)
	}
	if created != 0 {
		t.Errorf("created: got %d, want 0", created)
	}

	recs, _, err := ListRecommendations(db, "pending", "")
	if err != nil {
		t.Fatalf("ListRecommendations: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("expected no pending recommendations, got %d", len(recs))
	}
}

// TestCuratorNeverAccessedQualifies tests that a warm collection never accessed
// gets a never_accessed recommendation.
func TestCuratorNeverAccessedQualifies(t *testing.T) {
	db := setupCuratorTestDB(t)
	defer db.Close()

	now := time.Unix(1700000000, 0)

	insertCuratorPoolAndCollections(t, db,
		map[string]string{"pool-1": "warm"},
		map[string]struct {
			poolID   string
			tier     string
			lastAcc  int64
			size     int64
		}{
			"coll-1": {poolID: "pool-1", tier: "warm", lastAcc: 0, size: 1000},
		})

	created, err := GenerateRecommendations(db, now)
	if err != nil {
		t.Fatalf("GenerateRecommendations: %v", err)
	}
	if created != 1 {
		t.Errorf("created: got %d, want 1", created)
	}

	recs, _, err := ListRecommendations(db, "pending", "")
	if err != nil {
		t.Fatalf("ListRecommendations: %v", err)
	}
	if len(recs) != 1 {
		t.Errorf("expected 1 pending recommendation, got %d", len(recs))
	}
	if recs[0].Direction != "warm_to_cold" {
		t.Errorf("direction: got %s, want warm_to_cold", recs[0].Direction)
	}
	if recs[0].Reason != "never_accessed" {
		t.Errorf("reason: got %s, want never_accessed", recs[0].Reason)
	}
}

// TestCuratorColdHeatingQualifies tests that a cold collection with >=3 events
// in 7 days gets a cold_to_warm recommendation.
func TestCuratorColdHeatingQualifies(t *testing.T) {
	db := setupCuratorTestDB(t)
	defer db.Close()

	now := time.Unix(1700000000, 0)
	sevenDaysAgo := now.Add(-7 * 24 * time.Hour).Unix()

	insertCuratorPoolAndCollections(t, db,
		map[string]string{"pool-1": "cold"},
		map[string]struct {
			poolID   string
			tier     string
			lastAcc  int64
			size     int64
		}{
			"coll-1": {poolID: "pool-1", tier: "cold", lastAcc: 0, size: 1000},
		})

	insertCuratorAccessEvents(t, db, "coll-1", 3, sevenDaysAgo)

	created, err := GenerateRecommendations(db, now)
	if err != nil {
		t.Fatalf("GenerateRecommendations: %v", err)
	}
	if created != 1 {
		t.Errorf("created: got %d, want 1", created)
	}

	recs, _, err := ListRecommendations(db, "pending", "")
	if err != nil {
		t.Fatalf("ListRecommendations: %v", err)
	}
	if len(recs) != 1 {
		t.Errorf("expected 1 pending recommendation, got %d", len(recs))
	}
	if recs[0].Direction != "cold_to_warm" {
		t.Errorf("direction: got %s, want cold_to_warm", recs[0].Direction)
	}
	if recs[0].Reason != "heating_3_in_7d" {
		t.Errorf("reason: got %s, want heating_3_in_7d", recs[0].Reason)
	}
}

// TestCuratorColdIdleDoesNotQualifies tests that a cold collection with <3
// events in 7 days does not get a recommendation.
func TestCuratorColdIdleDoesNotQualifies(t *testing.T) {
	db := setupCuratorTestDB(t)
	defer db.Close()

	now := time.Unix(1700000000, 0)
	sevenDaysAgo := now.Add(-7 * 24 * time.Hour).Unix()

	insertCuratorPoolAndCollections(t, db,
		map[string]string{"pool-1": "cold"},
		map[string]struct {
			poolID   string
			tier     string
			lastAcc  int64
			size     int64
		}{
			"coll-1": {poolID: "pool-1", tier: "cold", lastAcc: 0, size: 1000},
		})

	insertCuratorAccessEvents(t, db, "coll-1", 2, sevenDaysAgo)

	created, err := GenerateRecommendations(db, now)
	if err != nil {
		t.Fatalf("GenerateRecommendations: %v", err)
	}
	if created != 0 {
		t.Errorf("created: got %d, want 0", created)
	}

	recs, _, err := ListRecommendations(db, "pending", "")
	if err != nil {
		t.Fatalf("ListRecommendations: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("expected no pending recommendations, got %d", len(recs))
	}
}

// TestCuratorPinnedExcluded tests that a pinned collection is excluded
// from recommendations.
func TestCuratorPinnedExcluded(t *testing.T) {
	db := setupCuratorTestDB(t)
	defer db.Close()

	now := time.Unix(1700000000, 0)
	lastAcc := now.Add(-30 * 24 * time.Hour).Unix()

	insertCuratorPoolAndCollections(t, db,
		map[string]string{"pool-1": "warm"},
		map[string]struct {
			poolID   string
			tier     string
			lastAcc  int64
			size     int64
		}{
			"coll-1": {poolID: "pool-1", tier: "warm", lastAcc: lastAcc, size: 1000},
		})

	// Pin the collection
	if err := PinCollection(db, "coll-1", now, "test pin"); err != nil {
		t.Fatalf("PinCollection: %v", err)
	}

	created, err := GenerateRecommendations(db, now)
	if err != nil {
		t.Fatalf("GenerateRecommendations: %v", err)
	}
	if created != 0 {
		t.Errorf("created: got %d, want 0 (pinned excluded)", created)
	}

	recs, _, err := ListRecommendations(db, "pending", "")
	if err != nil {
		t.Fatalf("ListRecommendations: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("expected no pending recommendations for pinned collection, got %d", len(recs))
	}
}

// TestCuratorSecondGenerateDedup tests that a second generate creates 0
// while pendings exist.
func TestCuratorSecondGenerateDedup(t *testing.T) {
	db := setupCuratorTestDB(t)
	defer db.Close()

	now := time.Unix(1700000000, 0)
	lastAcc := now.Add(-30 * 24 * time.Hour).Unix()

	insertCuratorPoolAndCollections(t, db,
		map[string]string{"pool-1": "warm"},
		map[string]struct {
			poolID   string
			tier     string
			lastAcc  int64
			size     int64
		}{
			"coll-1": {poolID: "pool-1", tier: "warm", lastAcc: lastAcc, size: 1000},
		})

	created1, err := GenerateRecommendations(db, now)
	if err != nil {
		t.Fatalf("first GenerateRecommendations: %v", err)
	}
	if created1 != 1 {
		t.Errorf("first created: got %d, want 1", created1)
	}

	created2, err := GenerateRecommendations(db, now)
	if err != nil {
		t.Fatalf("second GenerateRecommendations: %v", err)
	}
	if created2 != 0 {
		t.Errorf("second created: got %d, want 0 (dedup)", created2)
	}

	recs, total, err := ListRecommendations(db, "pending", "")
	if err != nil {
		t.Fatalf("ListRecommendations: %v", err)
	}
	if total != 1 || len(recs) != 1 {
		t.Errorf("expected exactly 1 pending recommendation, got total=%d len=%d", total, len(recs))
	}
}

// TestCuratorApproveReject tests approve/reject from pending, and error from decided.
func TestCuratorApproveReject(t *testing.T) {
	db := setupCuratorTestDB(t)
	defer db.Close()

	now := time.Unix(1700000000, 0)
	lastAcc := now.Add(-30 * 24 * time.Hour).Unix()

	insertCuratorPoolAndCollections(t, db,
		map[string]string{"pool-1": "warm"},
		map[string]struct {
			poolID   string
			tier     string
			lastAcc  int64
			size     int64
		}{
			"coll-1": {poolID: "pool-1", tier: "warm", lastAcc: lastAcc, size: 1000},
		})

	if _, err := GenerateRecommendations(db, now); err != nil {
		t.Fatalf("GenerateRecommendations: %v", err)
	}

	recs, _, err := ListRecommendations(db, "pending", "")
	if err != nil {
		t.Fatalf("ListRecommendations: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 pending recommendation, got %d", len(recs))
	}
	recID := recs[0].ID

	// Approve from pending -> ok
	if err := DecideRecommendation(db, recID, "approved", now); err != nil {
		t.Fatalf("DecideRecommendation approve: %v", err)
	}

	// Verify status changed
	recs, _, err = ListRecommendations(db, "approved", "")
	if err != nil {
		t.Fatalf("ListRecommendations approved: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != recID {
		t.Errorf("expected 1 approved recommendation with id %s", recID)
	}

	// Create a new recommendation for reject test (reuse existing pool)
	_, err = db.Exec(`INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, last_accessed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"coll-2", "coll-2", "pool-1", "/coll-2", "sda", 1000, lastAcc, now, now)
	if err != nil {
		t.Fatalf("insert coll-2: %v", err)
	}
	_, err = db.Exec(`INSERT INTO collection_heat (collection_id, last_access, score, event_count, size_bytes, computed_at) VALUES (?, ?, 0, 0, ?, 0)`,
		"coll-2", lastAcc, 1000)
	if err != nil {
		t.Fatalf("insert collection_heat coll-2: %v", err)
	}
	if _, err := GenerateRecommendations(db, now); err != nil {
		t.Fatalf("GenerateRecommendations second: %v", err)
	}
	recs, _, err = ListRecommendations(db, "pending", "")
	if err != nil {
		t.Fatalf("ListRecommendations pending: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 pending recommendation, got %d", len(recs))
	}
	recID2 := recs[0].ID

	// Reject from pending -> ok
	if err := DecideRecommendation(db, recID2, "rejected", now); err != nil {
		t.Fatalf("DecideRecommendation reject: %v", err)
	}

	// Approve from decided (already approved) -> error
	if err := DecideRecommendation(db, recID, "approved", now); err == nil {
		t.Error("expected error approving already-approved recommendation")
	}

	// Reject from decided (already rejected) -> error
	if err := DecideRecommendation(db, recID2, "rejected", now); err == nil {
		t.Error("expected error rejecting already-rejected recommendation")
	}

	// Unknown id -> sql.ErrNoRows
	if err := DecideRecommendation(db, "nonexistent-id", "approved", now); err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows for unknown id, got %v", err)
	}
}

// TestCuratorPinSupersedesPending tests that pinning a collection with a
// pending recommendation supersedes it.
func TestCuratorPinSupersedesPending(t *testing.T) {
	db := setupCuratorTestDB(t)
	defer db.Close()

	now := time.Unix(1700000000, 0)
	lastAcc := now.Add(-30 * 24 * time.Hour).Unix()

	insertCuratorPoolAndCollections(t, db,
		map[string]string{"pool-1": "warm"},
		map[string]struct {
			poolID   string
			tier     string
			lastAcc  int64
			size     int64
		}{
			"coll-1": {poolID: "pool-1", tier: "warm", lastAcc: lastAcc, size: 1000},
		})

	if _, err := GenerateRecommendations(db, now); err != nil {
		t.Fatalf("GenerateRecommendations: %v", err)
	}

	recs, _, err := ListRecommendations(db, "pending", "")
	if err != nil {
		t.Fatalf("ListRecommendations: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 pending recommendation, got %d", len(recs))
	}

	// Pin the collection - should supersede the pending recommendation
	if err := PinCollection(db, "coll-1", now, "supersede test"); err != nil {
		t.Fatalf("PinCollection: %v", err)
	}

	// Pending should now be superseded
	recs, _, err = ListRecommendations(db, "pending", "")
	if err != nil {
		t.Fatalf("ListRecommendations pending: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("expected 0 pending after pin, got %d", len(recs))
	}

	recs, _, err = ListRecommendations(db, "superseded", "")
	if err != nil {
		t.Fatalf("ListRecommendations superseded: %v", err)
	}
	if len(recs) != 1 {
		t.Errorf("expected 1 superseded recommendation, got %d", len(recs))
	}
}

// TestCuratorPolicyDefaults tests that GetTierPolicy returns defaults when no row exists.
func TestCuratorPolicyDefaults(t *testing.T) {
	db := setupCuratorTestDB(t)
	defer db.Close()

	policy, err := GetTierPolicy(db, "nonexistent-pool")
	if err != nil {
		t.Fatalf("GetTierPolicy: %v", err)
	}
	if policy.Mode != "recommend_only" {
		t.Errorf("mode: got %s, want recommend_only", policy.Mode)
	}
	if policy.ColdThresholdDays != 21 {
		t.Errorf("cold_threshold_days: got %d, want 21", policy.ColdThresholdDays)
	}
	if policy.AutoMoveWaitDays != 7 {
		t.Errorf("auto_move_wait_days: got %d, want 7", policy.AutoMoveWaitDays)
	}
}

// TestCuratorSetGetTierPolicy tests setting and getting a tier policy.
func TestCuratorSetGetTierPolicy(t *testing.T) {
	db := setupCuratorTestDB(t)
	defer db.Close()

	// Insert a pool first
	now := time.Now().Unix()
	_, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"pool-1", "media", "local", "warm", "single", "single", "/mnt/media", 4000787030016, 0, 0, "", "active", now, now)
	if err != nil {
		t.Fatalf("insert pool: %v", err)
	}

	// Set policy
	if err := SetTierPolicy(db, "pool-1", "auto_if_no_response", 14, 3); err != nil {
		t.Fatalf("SetTierPolicy: %v", err)
	}

	// Get policy
	policy, err := GetTierPolicy(db, "pool-1")
	if err != nil {
		t.Fatalf("GetTierPolicy: %v", err)
	}
	if policy.PoolID != "pool-1" {
		t.Errorf("pool_id: got %s, want pool-1", policy.PoolID)
	}
	if policy.Mode != "auto_if_no_response" {
		t.Errorf("mode: got %s, want auto_if_no_response", policy.Mode)
	}
	if policy.ColdThresholdDays != 14 {
		t.Errorf("cold_threshold_days: got %d, want 14", policy.ColdThresholdDays)
	}
	if policy.AutoMoveWaitDays != 3 {
		t.Errorf("auto_move_wait_days: got %d, want 3", policy.AutoMoveWaitDays)
	}
}

// TestCuratorInvalidTierPolicy tests invalid mode and days are rejected.
func TestCuratorInvalidTierPolicy(t *testing.T) {
	db := setupCuratorTestDB(t)
	defer db.Close()

	now := time.Now().Unix()
	_, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"pool-1", "media", "local", "warm", "single", "single", "/mnt/media", 4000787030016, 0, 0, "", "active", now, now)
	if err != nil {
		t.Fatalf("insert pool: %v", err)
	}

	// Invalid mode
	if err := SetTierPolicy(db, "pool-1", "invalid_mode", 21, 7); err != ErrInvalidTierPolicyMode {
		t.Errorf("expected ErrInvalidTierPolicyMode, got %v", err)
	}

	// Invalid cold days (0)
	if err := SetTierPolicy(db, "pool-1", "recommend_only", 0, 7); err != ErrInvalidTierPolicyDays {
		t.Errorf("expected ErrInvalidTierPolicyDays for cold_days=0, got %v", err)
	}

	// Invalid wait days (negative)
	if err := SetTierPolicy(db, "pool-1", "recommend_only", 21, -1); err != ErrInvalidTierPolicyDays {
		t.Errorf("expected ErrInvalidTierPolicyDays for wait_days=-1, got %v", err)
	}
}

// TestCuratorUnpinCollection tests unpin behavior.
func TestCuratorUnpinCollection(t *testing.T) {
	db := setupCuratorTestDB(t)
	defer db.Close()

	now := time.Unix(1700000000, 0)

	// Unpin non-existent -> sql.ErrNoRows
	if err := UnpinCollection(db, "nonexistent"); err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows for unpin non-existent, got %v", err)
	}

	// Insert a pool first (FK for collection)
	_, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"pool-1", "pool-1", "local", "warm", "single", "single", "/mnt/pool-1", 4000787030016, 0, 0, "", "active", now, now)
	if err != nil {
		t.Fatalf("insert pool: %v", err)
	}

	// Insert a collection first
	_, err = db.Exec(`INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, last_accessed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"coll-1", "coll-1", "pool-1", "/coll-1", "sda", 1000, 0, now, now)
	if err != nil {
		t.Fatalf("insert collection: %v", err)
	}

	// Pin then unpin
	if err := PinCollection(db, "coll-1", now, "test"); err != nil {
		t.Fatalf("PinCollection: %v", err)
	}
	if err := UnpinCollection(db, "coll-1"); err != nil {
		t.Fatalf("UnpinCollection: %v", err)
	}
	// Unpin again -> sql.ErrNoRows
	if err := UnpinCollection(db, "coll-1"); err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows for unpin again, got %v", err)
	}
}
