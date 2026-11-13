package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

// TestConcurrentCreatePoolHTTP tests the Chief's probe scenario:
// 5x POST /api/pools concurrent with same name -> ZERO 500, ZERO spurious 401.
// Reports pool count and status codes for Kapten's decision on duplicate-name policy.
func TestConcurrentCreatePoolHTTP(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	driver := storage.NewSingleDriver(disks)

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{Path: ":memory:"},
	}

	s := New(cfg, db, "test-version")
	s.SetDriver(driver)
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s.RehydratePools(); err != nil {
		t.Fatalf("rehydrate pools: %v", err)
	}

	ts := httptest.NewServer(s.srv.Handler)
	defer ts.Close()

	token := mustToken(t, ts)

	const numRequests = 5
	var wg sync.WaitGroup
	var statusCodes [numRequests]int
	var responses [numRequests]string
	var errors [numRequests]error

	payload := map[string]any{
		"name":       "concurrent-http-pool",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/concurrent",
		"encrypted":  false,
	}

	wg.Add(numRequests)
	for i := 0; i < numRequests; i++ {
		go func(idx int) {
			defer wg.Done()
			body, _ := json.Marshal(payload)
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/pools", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errors[idx] = err
				return
			}
			defer resp.Body.Close()
			statusCodes[idx] = resp.StatusCode
			b, _ := io.ReadAll(resp.Body)
			responses[idx] = string(b)
		}(i)
	}
	wg.Wait()

	// Report results
	t.Logf("=== Concurrent HTTP CreatePool Results (5 requests, same name) ===")
	successCount := 0
	errorCount := 0
	unauthorizedCount := 0
	serverErrorCount := 0
	for i := 0; i < numRequests; i++ {
		if errors[i] != nil {
			t.Logf("  Request %d: ERROR - %v", i, errors[i])
			errorCount++
			continue
		}
		t.Logf("  Request %d: HTTP %d - %s", i, statusCodes[i], responses[i])
		switch statusCodes[i] {
		case http.StatusCreated:
			successCount++
		case http.StatusUnauthorized:
			unauthorizedCount++
		case http.StatusInternalServerError:
			serverErrorCount++
		default:
			errorCount++
		}
	}
	t.Logf("Summary: 201=%d, 401=%d, 500=%d, other errors=%d", successCount, unauthorizedCount, serverErrorCount, errorCount)

	// Assertions per brief
	if serverErrorCount > 0 {
		t.Errorf("FAIL: %d requests returned 500 (catalog persist failed / SQLITE_BUSY)", serverErrorCount)
	}
	if unauthorizedCount > 0 {
		t.Errorf("FAIL: %d requests returned 401 (spurious unauthorized under load)", unauthorizedCount)
	}
	if successCount == 0 {
		t.Errorf("FAIL: No successful pool creations (all failed)")
	}

	// Verify catalog consistency
	result, err := catalog.IntegrityCheck(db)
	if err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
	if result != "ok" {
		t.Errorf("IntegrityCheck failed: %s", result)
	}

	// Report pool count for Kapten's decision
	var poolCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM pools WHERE name = ?`, "concurrent-http-pool").Scan(&poolCount)
	if err != nil {
		t.Fatalf("Count pools: %v", err)
	}
	t.Logf("Pools created in catalog with name 'concurrent-http-pool': %d", poolCount)

	// List all pools for visibility
	rows, err := db.Query(`SELECT id, name, created_at FROM pools WHERE name = ? ORDER BY created_at`, "concurrent-http-pool")
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

	// Control: sequential request with same token should succeed (201)
	controlPayload := map[string]any{
		"name":       "control-pool",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/control",
		"encrypted":  false,
	}
	ctrlBody, _ := json.Marshal(controlPayload)
	ctrlReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/pools", bytes.NewReader(ctrlBody))
	ctrlReq.Header.Set("Content-Type", "application/json")
	ctrlReq.Header.Set("Authorization", "Bearer "+token)
	ctrlResp, err := http.DefaultClient.Do(ctrlReq)
	if err != nil {
		t.Fatalf("Control request failed: %v", err)
	}
	defer ctrlResp.Body.Close()
	t.Logf("Control (sequential) request: HTTP %d", ctrlResp.StatusCode)
	if ctrlResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(ctrlResp.Body)
		t.Errorf("Control request should be 201, got %d: %s", ctrlResp.StatusCode, string(b))
	}
}

// TestConcurrentCreatePoolHTTPFileDB tests concurrent pool creation with a file DB
// to verify WAL mode works under HTTP load.
func TestConcurrentCreatePoolHTTPFileDB(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/concurrent_http.db"

	db, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	driver := storage.NewSingleDriver(disks)

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{Path: dbPath},
	}

	s := New(cfg, db, "test-version")
	s.SetDriver(driver)
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s.RehydratePools(); err != nil {
		t.Fatalf("rehydrate pools: %v", err)
	}

	ts := httptest.NewServer(s.srv.Handler)
	defer ts.Close()

	token := mustToken(t, ts)

	const numRequests = 5
	var wg sync.WaitGroup
	var statusCodes [numRequests]int
	var responses [numRequests]string
	var errors [numRequests]error

	payload := map[string]any{
		"name":       "file-db-pool",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/file-db",
		"encrypted":  false,
	}

	wg.Add(numRequests)
	for i := 0; i < numRequests; i++ {
		go func(idx int) {
			defer wg.Done()
			body, _ := json.Marshal(payload)
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/pools", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				errors[idx] = err
				return
			}
			defer resp.Body.Close()
			statusCodes[idx] = resp.StatusCode
			b, _ := io.ReadAll(resp.Body)
			responses[idx] = string(b)
		}(i)
	}
	wg.Wait()

	// Report results
	t.Logf("=== Concurrent HTTP CreatePool (File DB) Results ===")
	successCount := 0
	serverErrorCount := 0
	unauthorizedCount := 0
	for i := 0; i < numRequests; i++ {
		if errors[i] != nil {
			t.Logf("  Request %d: ERROR - %v", i, errors[i])
			continue
		}
		t.Logf("  Request %d: HTTP %d - %s", i, statusCodes[i], responses[i])
		switch statusCodes[i] {
		case http.StatusCreated:
			successCount++
		case http.StatusUnauthorized:
			unauthorizedCount++
		case http.StatusInternalServerError:
			serverErrorCount++
		}
	}
	t.Logf("Summary: 201=%d, 401=%d, 500=%d", successCount, unauthorizedCount, serverErrorCount)

	// Assertions
	if serverErrorCount > 0 {
		t.Errorf("FAIL: %d requests returned 500", serverErrorCount)
	}
	if unauthorizedCount > 0 {
		t.Errorf("FAIL: %d requests returned 401 (spurious)", unauthorizedCount)
	}

	// Verify catalog integrity
	result, err := catalog.IntegrityCheck(db)
	if err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
	if result != "ok" {
		t.Errorf("IntegrityCheck failed: %s", result)
	}

	// Verify WAL mode persisted
	var jm string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&jm); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if jm != "wal" && jm != "WAL" {
		t.Errorf("journal_mode not WAL after concurrent HTTP: got %s", jm)
	}
	t.Logf("journal_mode after concurrent HTTP: %s", jm)

	// Report pool count
	var poolCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM pools WHERE name = ?`, "file-db-pool").Scan(&poolCount)
	if err != nil {
		t.Fatalf("Count pools: %v", err)
	}
	t.Logf("Pools created: %d", poolCount)

	// Shutdown and reopen to verify persistence
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	cancel()
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// Reopen
	db2, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen catalog: %v", err)
	}
	defer db2.Close()
	if err := catalog.Migrate(db2); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}

	// Verify journal_mode still WAL
	if err := db2.QueryRow(`PRAGMA journal_mode`).Scan(&jm); err != nil {
		t.Fatalf("PRAGMA journal_mode (reopen): %v", err)
	}
	if jm != "wal" && jm != "WAL" {
		t.Errorf("journal_mode not WAL after reopen: got %s", jm)
	}
	t.Logf("journal_mode after reopen: %s", jm)

	// Verify data persisted
	err = db2.QueryRow(`SELECT COUNT(*) FROM pools WHERE name = ?`, "file-db-pool").Scan(&poolCount)
	if err != nil {
		t.Fatalf("Count pools (reopen): %v", err)
	}
	t.Logf("Pools after reopen: %d", poolCount)

	result, err = catalog.IntegrityCheck(db2)
	if err != nil {
		t.Fatalf("IntegrityCheck (reopen): %v", err)
	}
	if result != "ok" {
		t.Errorf("IntegrityCheck after reopen: %s", result)
	}
}

// TestConcurrentMixedHTTPOperations tests mixed read/write HTTP operations concurrently.
func TestConcurrentMixedHTTPOperations(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	driver := storage.NewSingleDriver(disks)

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{Path: ":memory:"},
	}

	s := New(cfg, db, "test-version")
	s.SetDriver(driver)
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s.RehydratePools(); err != nil {
		t.Fatalf("rehydrate pools: %v", err)
	}

	ts := httptest.NewServer(s.srv.Handler)
	defer ts.Close()

	token := mustToken(t, ts)

	// Pre-create a pool
	poolPayload := map[string]any{
		"name":       "mixed-pool",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/mixed",
		"encrypted":  false,
	}
	poolBody, _ := json.Marshal(poolPayload)
	poolReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/pools", bytes.NewReader(poolBody))
	poolReq.Header.Set("Content-Type", "application/json")
	poolReq.Header.Set("Authorization", "Bearer "+token)
	poolResp, err := http.DefaultClient.Do(poolReq)
	if err != nil {
		t.Fatalf("Pre-create pool: %v", err)
	}
	defer poolResp.Body.Close()
	var poolRespBody struct {
		Pool struct {
			ID string `json:"id"`
		} `json:"pool"`
	}
	json.NewDecoder(poolResp.Body).Decode(&poolRespBody)
	poolID := poolRespBody.Pool.ID

	const numRequests = 10
	var wg sync.WaitGroup
	var readErrors, writeErrors atomic.Int32

	wg.Add(numRequests)
	for i := 0; i < numRequests; i++ {
		go func(idx int) {
			defer wg.Done()
			if idx%2 == 0 {
				// Reader: GET /api/pools
				req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pools", nil)
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					readErrors.Add(1)
					return
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					readErrors.Add(1)
				}
			} else {
				// Writer: POST /api/pools/:poolID/collections
				collPayload := map[string]any{
					"name":      "mixed-coll-" + randomHex(4),
					"path":      "/mixed/" + randomHex(4),
					"disk_name": "sda",
				}
				body, _ := json.Marshal(collPayload)
				req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/pools/"+poolID+"/collections", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					writeErrors.Add(1)
					return
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusCreated {
					writeErrors.Add(1)
				}
			}
		}(i)
	}
	wg.Wait()

	t.Logf("Mixed HTTP ops: readErrors=%d, writeErrors=%d", readErrors.Load(), writeErrors.Load())

	// Verify catalog integrity
	result, err := catalog.IntegrityCheck(db)
	if err != nil {
		t.Fatalf("IntegrityCheck: %v", err)
	}
	if result != "ok" {
		t.Errorf("IntegrityCheck: %s", result)
	}
}

func randomHex(n int) string {
	b := make([]byte, n/2)
	// Simple time-based for test
	for i := range b {
		b[i] = byte('a' + (time.Now().UnixNano() + int64(i)) % 26)
	}
	return string(b)
}
