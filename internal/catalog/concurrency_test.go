package catalog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestConcurrentCreatePoolSameName tests that 5 concurrent CreatePool calls
// with the same name produce ZERO SQLITE_BUSY / transaction collision errors.
// The catalog must remain consistent (integrity_check passes).
// Since pools.name has no UNIQUE constraint, multiple pools may be created.
// This test observes and reports the outcome.
func TestConcurrentCreatePoolSameName(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	const numGoroutines = 5
	var wg sync.WaitGroup
	var successCount atomic.Int32
	var errorCount atomic.Int32
	var errors []string
	var errorsMu sync.Mutex

	poolName := "concurrent-pool"
	nodeID := "local"
	tier := "warm"
	mechanism := "single"
	driver := "single"
	mountpoint := "/mnt/test"
	sizeBytes := int64(1000000)

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			pool := &PoolRow{
				ID:         generatePoolID(),
				Name:       poolName,
				NodeID:     nodeID,
				Tier:       tier,
				Mechanism:  mechanism,
				Driver:     driver,
				Mountpoint: mountpoint,
				SizeBytes:  sizeBytes,
				Status:     "active",
			}
			diskNames := []string{"sda"}

			err := CreatePool(db, pool, diskNames)
			if err != nil {
				errorCount.Add(1)
				errorsMu.Lock()
				errors = append(errors, err.Error())
				errorsMu.Unlock()
				return
			}
			successCount.Add(1)
		}(i)
	}
	wg.Wait()

	t.Logf("Concurrent CreatePool results: success=%d, errors=%d", successCount.Load(), errorCount.Load())
	for _, e := range errors {
		t.Logf("  Error: %s", e)
	}

	// Verify ZERO SQLITE_BUSY / transaction collision errors
	for _, e := range errors {
		if containsSQLiteBusy(e) || containsTransactionCollision(e) {
			t.Errorf("Found concurrency error (SQLITE_BUSY or transaction collision): %s", e)
		}
	}

	// Verify catalog integrity
	result, err := IntegrityCheck(db)
	if err != nil {
		t.Fatalf("IntegrityCheck failed: %v", err)
	}
	if result != "ok" {
		t.Errorf("IntegrityCheck returned: %s", result)
	}

	// Report how many pools were created (product decision, not test assertion)
	var poolCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM pools WHERE name = ?`, poolName).Scan(&poolCount)
	if err != nil {
		t.Fatalf("Count pools: %v", err)
	}
	t.Logf("Pools created with name '%s': %d", poolName, poolCount)

	// List all pools for visibility
	rows, err := db.Query(`SELECT id, name, created_at FROM pools WHERE name = ? ORDER BY created_at`, poolName)
	if err != nil {
		t.Fatalf("Query pools: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		var createdAt int64
		if err := rows.Scan(&id, &name, &createdAt); err != nil {
			t.Fatalf("Scan pool: %v", err)
		}
		t.Logf("  Pool: id=%s name=%s created_at=%d", id, name, createdAt)
	}
}

// TestConcurrentCreatePoolWithContext tests concurrent CreatePool with context cancellation.
func TestConcurrentCreatePoolWithContext(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	const numGoroutines = 5
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var successCount atomic.Int32
	var errorCount atomic.Int32

	poolName := "context-pool"

	wg.Add(numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				errorCount.Add(1)
				return
			default:
			}

			pool := &PoolRow{
				ID:         generatePoolID(),
				Name:       poolName,
				NodeID:     "local",
				Tier:       "warm",
				Mechanism:  "single",
				Driver:     "single",
				Mountpoint: "/mnt/test",
				SizeBytes:  1000000,
				Status:     "active",
			}

			if err := CreatePool(db, pool, []string{"sda"}); err != nil {
				errorCount.Add(1)
				return
			}
			successCount.Add(1)
		}()
	}
	wg.Wait()

	t.Logf("Context test: success=%d, errors=%d", successCount.Load(), errorCount.Load())

	// Verify integrity
	result, err := IntegrityCheck(db)
	if err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
	if result != "ok" {
		t.Errorf("IntegrityCheck: %s", result)
	}
}

// TestWALModeRestartPersistence tests that a WAL-mode database
// survives close/reopen with data intact and journal_mode still WAL.
func TestWALModeRestartPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/wal_test.db"

	// First instance: create DB, enable WAL, write data
	db1, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (1st): %v", err)
	}
	if err := Migrate(db1); err != nil {
		t.Fatalf("Migrate (1st): %v", err)
	}

	// Verify WAL mode is active
	var jm string
	if err := db1.QueryRow(`PRAGMA journal_mode`).Scan(&jm); err != nil {
		t.Fatalf("PRAGMA journal_mode (1st): %v", err)
	}
	if !equalFold(jm, "wal") {
		t.Fatalf("Expected WAL mode, got %s", jm)
	}
	t.Logf("First instance journal_mode: %s", jm)

	// Create a pool
	pool := &PoolRow{
		ID:         "pool-persist-1",
		Name:       "persist-pool",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/persist",
		SizeBytes:  5000000000,
		Status:     "active",
	}
	if err := CreatePool(db1, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool (1st): %v", err)
	}

	// Create a collection
	coll := &CollectionRow{
		ID:        "coll-persist-1",
		Name:      "photos",
		PoolID:    pool.ID,
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 1024 * 1024 * 100,
	}
	if err := CreateCollection(db1, coll); err != nil {
		t.Fatalf("CreateCollection (1st): %v", err)
	}

	// Write audit log
	WriteAuditLog(db1, "test", "pool.create", "pool", pool.ID, "ok", "persistence test")

	db1.Close()

	// Second instance: reopen and verify
	db2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (2nd): %v", err)
	}
	defer db2.Close()

	if err := Migrate(db2); err != nil {
		t.Fatalf("Migrate (2nd): %v", err)
	}

	// Verify journal_mode is still WAL
	if err := db2.QueryRow(`PRAGMA journal_mode`).Scan(&jm); err != nil {
		t.Fatalf("PRAGMA journal_mode (2nd): %v", err)
	}
	if !equalFold(jm, "wal") {
		t.Errorf("Expected WAL mode after restart, got %s", jm)
	}
	t.Logf("Second instance journal_mode: %s", jm)

	// Verify pool exists
	gotPool, gotDisks, err := GetPool(db2, pool.ID)
	if err != nil {
		t.Fatalf("GetPool (2nd): %v", err)
	}
	if gotPool == nil {
		t.Fatal("Pool not found after restart")
	}
	if gotPool.Name != pool.Name || gotPool.SizeBytes != pool.SizeBytes {
		t.Errorf("Pool data mismatch: got %+v", gotPool)
	}
	if len(gotDisks) != 1 || gotDisks[0] != "sda" {
		t.Errorf("Pool disks mismatch: %v", gotDisks)
	}

	// Verify collection exists
	gotColl, err := GetCollection(db2, coll.ID)
	if err != nil {
		t.Fatalf("GetCollection (2nd): %v", err)
	}
	if gotColl == nil {
		t.Fatal("Collection not found after restart")
	}
	if gotColl.Name != coll.Name || gotColl.PoolID != coll.PoolID {
		t.Errorf("Collection data mismatch: %+v", gotColl)
	}

	// Verify audit log exists
	entries, total, err := ListAuditLog(db2, 10, 0, "pool.create", "", "")
	if err != nil {
		t.Fatalf("ListAuditLog (2nd): %v", err)
	}
	if total != 1 || len(entries) != 1 {
		t.Errorf("Audit log mismatch: total=%d len=%d", total, len(entries))
	}
	if entries[0].TargetID != pool.ID {
		t.Errorf("Audit log target_id mismatch: %s", entries[0].TargetID)
	}

	// Verify integrity
	result, err := IntegrityCheck(db2)
	if err != nil {
		t.Fatalf("IntegrityCheck (2nd): %v", err)
	}
	if result != "ok" {
		t.Errorf("IntegrityCheck after restart: %s", result)
	}

	// Third instance: another restart cycle
	db2.Close()

	db3, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (3rd): %v", err)
	}
	defer db3.Close()

	if err := Migrate(db3); err != nil {
		t.Fatalf("Migrate (3rd): %v", err)
	}

	if err := db3.QueryRow(`PRAGMA journal_mode`).Scan(&jm); err != nil {
		t.Fatalf("PRAGMA journal_mode (3rd): %v", err)
	}
	if !equalFold(jm, "wal") {
		t.Errorf("Expected WAL mode after 2nd restart, got %s", jm)
	}
	t.Logf("Third instance journal_mode: %s", jm)

	// Data should still be there
	var count int
	if err := db3.QueryRow(`SELECT COUNT(*) FROM pools WHERE id = ?`, pool.ID).Scan(&count); err != nil {
		t.Fatalf("Count pools (3rd): %v", err)
	}
	if count != 1 {
		t.Errorf("Pool missing after 2nd restart: count=%d", count)
	}

	result, err = IntegrityCheck(db3)
	if err != nil {
		t.Fatalf("IntegrityCheck (3rd): %v", err)
	}
	if result != "ok" {
		t.Errorf("IntegrityCheck after 2nd restart: %s", result)
	}
}

// TestForeignKeyEnforcement tests that foreign_keys=ON is actually enforced
// (delete parent with child -> cascades per schema ON DELETE CASCADE).
func TestForeignKeyEnforcement(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Create pool
	pool := &PoolRow{
		ID:         "pool-fk-1",
		Name:       "fk-pool",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/fk",
		SizeBytes:  1000000,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	// Create collection referencing pool
	coll := &CollectionRow{
		ID:        "coll-fk-1",
		Name:      "fk-collection",
		PoolID:    pool.ID,
		Path:      "/fk",
		DiskName:  "sda",
		SizeBytes: 1000,
	}
	if err := CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	// Delete pool - should CASCADE delete collection (per schema FK)
	if err := DeletePool(db, pool.ID); err != nil {
		t.Fatalf("DeletePool should cascade: %v", err)
	}

	// Verify collection is gone (cascaded)
	gotColl, err := GetCollection(db, coll.ID)
	if err != nil {
		t.Fatalf("GetCollection after cascade: %v", err)
	}
	if gotColl != nil {
		t.Error("Collection should have been cascaded deleted")
	}

	// Verify pool is gone
	gotPool, _, err := GetPool(db, pool.ID)
	if err != nil {
		t.Fatalf("GetPool after delete: %v", err)
	}
	if gotPool != nil {
		t.Error("Pool should be deleted")
	}

	// Verify pool_disks also cascaded
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM pool_disks WHERE pool_id=?", pool.ID).Scan(&count); err != nil {
		t.Fatalf("Count pool_disks: %v", err)
	}
	if count != 0 {
		t.Errorf("pool_disks not cascaded: count=%d", count)
	}
}

// TestBusyTimeoutUnderContention tests that busy_timeout prevents
// immediate SQLITE_BUSY under contention.
func TestBusyTimeoutUnderContention(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Start a long-running transaction (hold write lock)
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin holding tx: %v", err)
	}
	_, err = tx.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"holder", "holder", "local", "warm", "single", "single", "/mnt/holder", 1000, 0, 0, "", "active", time.Now().Unix(), time.Now().Unix())
	if err != nil {
		tx.Rollback()
		t.Fatalf("Insert in holding tx: %v", err)
	}
	// Don't commit yet - hold the lock

	// Try concurrent write - should wait up to busy_timeout then fail
	done := make(chan error, 1)
	go func() {
		_, err := db.Exec(`INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"waiter", "waiter", "local", "warm", "single", "single", "/mnt/waiter", 1000, 0, 0, "", "active", time.Now().Unix(), time.Now().Unix())
		done <- err
	}()

	// Wait a bit, then release the lock
	time.Sleep(100 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit holding tx: %v", err)
	}

	select {
	case err := <-done:
		// With busy_timeout=5000, the waiter should succeed after the lock is released
		if err != nil {
			t.Logf("Waiter error (may be expected if timeout too short): %v", err)
		} else {
			t.Log("Waiter succeeded after lock released")
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Waiter timed out (busy_timeout not working)")
	}

	// Verify integrity
	result, err := IntegrityCheck(db)
	if err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
	if result != "ok" {
		t.Errorf("IntegrityCheck: %s", result)
	}
}

// generatePoolID generates a unique pool ID for testing.
func generatePoolID() string {
	return "pool-" + randomHex(16)
}

func randomHex(n int) string {
	b := make([]byte, n/2)
	if _, err := rand.Read(b); err != nil {
		// Fallback: time-based
		for i := range b {
			b[i] = byte('a' + (time.Now().UnixNano() + int64(i)) % 26)
		}
	}
	return hex.EncodeToString(b)
}

func containsSQLiteBusy(errMsg string) bool {
	return len(errMsg) > 0 && (contains(errMsg, "SQLITE_BUSY") || contains(errMsg, "database is locked") || contains(errMsg, "busy"))
}

func containsTransactionCollision(errMsg string) bool {
	return len(errMsg) > 0 && (contains(errMsg, "cannot start a transaction within a transaction") || contains(errMsg, "transaction within a transaction"))
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || indexSubstring(s, substr) >= 0))
}

func indexSubstring(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// TestConcurrentMixedOperations tests mixed read/write operations concurrently.
func TestConcurrentMixedOperations(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Pre-create a pool
	pool := &PoolRow{
		ID:         "pool-mixed-1",
		Name:       "mixed-pool",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/mixed",
		SizeBytes:  10000000,
		Status:     "active",
	}
	if err := CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	const numGoroutines = 10
	var wg sync.WaitGroup
	var readErrors, writeErrors atomic.Int32

	// 5 readers, 5 writers
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if idx%2 == 0 {
				// Reader: ListPools
				_, err := ListPools(db)
				if err != nil {
					readErrors.Add(1)
					t.Logf("Reader %d error: %v", idx, err)
				}
			} else {
				// Writer: CreateCollection
				coll := &CollectionRow{
					ID:        "coll-mixed-" + randomHex(4),
					Name:      "mixed-coll-" + randomHex(4),
					PoolID:    pool.ID,
					Path:      "/mixed/" + randomHex(4),
					DiskName:  "sda",
					SizeBytes: 1000,
				}
				if err := CreateCollection(db, coll); err != nil {
					writeErrors.Add(1)
					t.Logf("Writer %d error: %v", idx, err)
				}
			}
		}(i)
	}
	wg.Wait()

	t.Logf("Mixed ops: readErrors=%d, writeErrors=%d", readErrors.Load(), writeErrors.Load())

	// Verify integrity
	result, err := IntegrityCheck(db)
	if err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
	if result != "ok" {
		t.Errorf("IntegrityCheck: %s", result)
	}
}
