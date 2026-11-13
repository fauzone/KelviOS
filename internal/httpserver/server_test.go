package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

func TestHealthz(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: ":memory:",
		},
	}

	s := New(cfg, db, "test-version")
	ts := httptest.NewServer(s.srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("expected JSON content type, got %s", resp.Header.Get("Content-Type"))
	}
}

func TestUIRoot(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: ":memory:",
		},
	}

	s := New(cfg, db, "test-version")
	ts := httptest.NewServer(s.srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	if resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("expected HTML content type, got %s", resp.Header.Get("Content-Type"))
	}
}

func TestDisksEndpoint(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: ":memory:",
		},
	}

	s := New(cfg, db, "test-version")
	// Stub the inventory so the test never depends on real hardware.
	s.disksFunc = func() ([]storage.Disk, error) {
		return []storage.Disk{
			{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
		}, nil
	}
	ts := httptest.NewServer(s.srv.Handler)
	token := mustToken(t, ts)
	defer ts.Close()

	dreq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/disks", nil)
	dreq.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(dreq)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("expected JSON content type, got %s", resp.Header.Get("Content-Type"))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var payload struct {
		Disks []storage.Disk `json:"disks"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v (body: %s)", err, body)
	}
	if len(payload.Disks) != 1 || payload.Disks[0].Name != "sda" || payload.Disks[0].SizeBytes != 4000787030016 {
		t.Errorf("unexpected disks payload: %+v", payload.Disks)
	}
}

// TestPoolsEndpoints tests the pool CRUD endpoints with a stub driver.
func TestPoolsEndpoints(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: ":memory:",
		},
	}

	// Stub inventory with one disk
	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	driver := storage.NewSingleDriver(disks)

	s := New(cfg, db, "test-version")
	s.SetDriver(driver)
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }

	ts := httptest.NewServer(s.srv.Handler)
	token := mustToken(t, ts)
	defer ts.Close()

	// Helper to make JSON requests
	doReq := func(method, path string, payload any) *http.Response {
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
		return resp
	}

	readError := func(resp *http.Response) string {
		defer resp.Body.Close()
		var e struct{ Error string `json:"error"` }
		json.NewDecoder(resp.Body).Decode(&e)
		return e.Error
	}

	// 1. POST /api/pools with missing tier -> 400
	resp := doReq("POST", "/api/pools", map[string]any{
		"name":       "media",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST without tier: expected 400, got %d", resp.StatusCode)
	}
	errMsg := readError(resp)
	if !strings.Contains(errMsg, "tier") {
		t.Errorf("expected tier error, got: %s", errMsg)
	}

	// 2. POST /api/pools with invalid tier -> 400
	resp = doReq("POST", "/api/pools", map[string]any{
		"name":       "media",
		"tier":       "invalid",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST with invalid tier: expected 400, got %d", resp.StatusCode)
	}
	errMsg = readError(resp)
	if !strings.Contains(errMsg, "invalid tier") {
		t.Errorf("expected invalid tier error, got: %s", errMsg)
	}

	// 3. POST /api/pools with unknown mechanism -> 400
	resp = doReq("POST", "/api/pools", map[string]any{
		"name":       "media",
		"tier":       "warm",
		"mechanism":  "unknown",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST with unknown mechanism: expected 400, got %d", resp.StatusCode)
	}
	errMsg = readError(resp)
	if !strings.Contains(errMsg, "invalid mechanism") {
		t.Errorf("expected invalid mechanism error, got: %s", errMsg)
	}

	// 4. POST /api/pools with unknown disk -> 400
	// (WS25: mechanism is mandatory now — pass it explicitly so the
	// request reaches the disk validation this subtest targets.)
	resp = doReq("POST", "/api/pools", map[string]any{
		"name":       "media",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sdz"},
		"mountpoint": "/mnt/media",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST with unknown disk: expected 400, got %d", resp.StatusCode)
	}
	// Debug: read raw body
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Logf("unknown disk response: status=%d body=%s", resp.StatusCode, string(body))
	var e struct{ Error string `json:"error"` }
	json.Unmarshal(body, &e)
	errMsg = e.Error
	if !strings.Contains(errMsg, "not in inventory") {
		t.Errorf("expected unknown disk error, got: %s", errMsg)
	}

	// 4. POST /api/pools valid -> 201
	resp = doReq("POST", "/api/pools", map[string]any{
		"name":       "media",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
		"encrypted":  false,
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("POST valid pool: expected 201, got %d: %s", resp.StatusCode, string(b))
		resp.Body.Close()
	}
	var createResp struct {
		Pool struct {
			ID         string   `json:"id"`
			Name       string   `json:"name"`
			Tier       string   `json:"tier"`
			Mechanism  string   `json:"mechanism"`
			Driver     string   `json:"driver"`
			DiskNames  []string `json:"disk_names"`
			Mountpoint string   `json:"mountpoint"`
			SizeBytes  int64    `json:"size_bytes"`
			Encrypted  bool     `json:"encrypted"`
		} `json:"pool"`
	}
	json.NewDecoder(resp.Body).Decode(&createResp)
	resp.Body.Close()
	if createResp.Pool.ID == "" || createResp.Pool.Name != "media" || createResp.Pool.Tier != "warm" {
		t.Errorf("unexpected pool response: %+v", createResp.Pool)
	}
	if len(createResp.Pool.DiskNames) != 1 || createResp.Pool.DiskNames[0] != "sda" {
		t.Errorf("disk_names mismatch: %v", createResp.Pool.DiskNames)
	}
	poolID := createResp.Pool.ID

	// 5. GET /api/pools -> list with the created pool
	resp = doReq("GET", "/api/pools", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/pools: expected 200, got %d", resp.StatusCode)
	}
	var listResp struct {
		Pools []struct {
			ID         string   `json:"id"`
			Name       string   `json:"name"`
			Tier       string   `json:"tier"`
			DiskNames  []string `json:"disk_names"`
		} `json:"pools"`
	}
	json.NewDecoder(resp.Body).Decode(&listResp)
	resp.Body.Close()
	if len(listResp.Pools) != 1 || listResp.Pools[0].ID != poolID {
		t.Errorf("list pools mismatch: %+v", listResp.Pools)
	}

	// 6. GET /api/pools/{id} -> get single pool
	resp = doReq("GET", "/api/pools/"+poolID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /api/pools/{id}: expected 200, got %d", resp.StatusCode)
	}
	var getResp struct {
		Pool struct {
			ID         string   `json:"id"`
			Name       string   `json:"name"`
			Tier       string   `json:"tier"`
			DiskNames  []string `json:"disk_names"`
		} `json:"pool"`
	}
	json.NewDecoder(resp.Body).Decode(&getResp)
	resp.Body.Close()
	if getResp.Pool.ID != poolID || getResp.Pool.Name != "media" {
		t.Errorf("get pool mismatch: %+v", getResp.Pool)
	}

	// 7. GET /api/pools/{id} not found -> 404
	resp = doReq("GET", "/api/pools/pool-nonexistent", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET nonexistent pool: expected 404, got %d", resp.StatusCode)
	}
	errMsg = readError(resp)
	if !strings.Contains(errMsg, "not found") {
		t.Errorf("expected not found error, got: %s", errMsg)
	}

	// 8. DELETE /api/pools/{id} -> 204
	resp = doReq("DELETE", "/api/pools/"+poolID, nil)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("DELETE pool: expected 204, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 9. GET /api/pools/{id} after delete -> 404
	resp = doReq("GET", "/api/pools/"+poolID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after delete: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 10. DELETE /api/pools/{id} not found -> 404
	resp = doReq("DELETE", "/api/pools/pool-nonexistent", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE nonexistent: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestPoolRestartRehydration tests that pools survive a REAL server restart
// across a file-DB/process boundary.
// It uses a temp FILE database (t.TempDir()), creates a pool on server instance A,
// closes A completely (including its sql.DB handle), then constructs server instance B
// on the SAME file path. B must rehydrate from the file alone.
// Assertions: GET pool = 200 on B, DELETE = 2xx, GET again = 404.
// Also asserts the catalog survives a second close/reopen cycle.
func TestPoolRestartRehydration(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "catalog.db")

	// Stub inventory with one disk
	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}

	// --- First server instance (simulates initial boot) ---
	db1, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db1); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: dbPath,
		},
	}

	driver1 := storage.NewSingleDriver(disks)
	s1 := New(cfg, db1, "test-version")
	s1.SetDriver(driver1)
	s1.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s1.RehydratePools(); err != nil {
		t.Fatalf("rehydrate pools: %v", err)
	}

	ts1 := httptest.NewServer(s1.srv.Handler)
	token := mustToken(t, ts1)
	defer ts1.Close()

	doReq := func(method, path string, payload any) *http.Response {
		var body io.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts1.URL+path, body)
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
		return resp
	}

	// Create a pool
	resp := doReq("POST", "/api/pools", map[string]any{
		"name":       "media",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
		"encrypted":  false,
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("POST valid pool: expected 201, got %d: %s", resp.StatusCode, string(b))
		resp.Body.Close()
	}
	var createResp struct {
		Pool struct {
			ID string `json:"id"`
		} `json:"pool"`
	}
	json.NewDecoder(resp.Body).Decode(&createResp)
	resp.Body.Close()
	poolID := createResp.Pool.ID
	if poolID == "" {
		t.Fatal("pool ID empty")
	}

	// Verify pool exists
	resp = doReq("GET", "/api/pools/"+poolID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET pool: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Shutdown first server AND close its DB handle (crosses process/file-DB boundary)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s1.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown server 1: %v", err)
	}
	cancel()
	if err := db1.Close(); err != nil {
		t.Fatalf("close db1: %v", err)
	}

	// --- Second server instance (simulates restart on same FILE DB) ---
	db2, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog (restart): %v", err)
	}
	if err := catalog.Migrate(db2); err != nil {
		t.Fatalf("migrate (restart): %v", err)
	}

	driver2 := storage.NewSingleDriver(disks)
	s2 := New(cfg, db2, "test-version")
	s2.SetDriver(driver2)
	s2.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s2.RehydratePools(); err != nil {
		t.Fatalf("rehydrate pools on restart: %v", err)
	}

	ts2 := httptest.NewServer(s2.srv.Handler)
	defer ts2.Close()

	doReq2 := func(method, path string, payload any) *http.Response {
		var body io.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts2.URL+path, body)
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
		return resp
	}

	// GET the pool after restart -> should return 200 (rehydrated from file)
	resp = doReq2("GET", "/api/pools/"+poolID, nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET after restart: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// DELETE the pool after restart -> should return 2xx (204)
	resp = doReq2("DELETE", "/api/pools/"+poolID, nil)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("DELETE after restart: expected 204, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// GET the pool after delete -> should return 404
	resp = doReq2("GET", "/api/pools/"+poolID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after delete: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Shutdown second server and close its DB handle
	shutdownCtx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s2.Shutdown(shutdownCtx2); err != nil {
		t.Fatalf("shutdown server 2: %v", err)
	}
	cancel2()
	if err := db2.Close(); err != nil {
		t.Fatalf("close db2: %v", err)
	}

	// --- Third server instance (second restart cycle) ---
	db3, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog (2nd restart): %v", err)
	}
	if err := catalog.Migrate(db3); err != nil {
		t.Fatalf("migrate (2nd restart): %v", err)
	}

	driver3 := storage.NewSingleDriver(disks)
	s3 := New(cfg, db3, "test-version")
	s3.SetDriver(driver3)
	s3.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s3.RehydratePools(); err != nil {
		t.Fatalf("rehydrate pools on 2nd restart: %v", err)
	}

	ts3 := httptest.NewServer(s3.srv.Handler)
	defer ts3.Close()

	doReq3 := func(method, path string, payload any) *http.Response {
		var body io.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts3.URL+path, body)
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
		return resp
	}

	// Pool should be gone (deleted in second instance)
	resp = doReq3("GET", "/api/pools/"+poolID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after 2nd restart: expected 404 (pool deleted), got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Verify catalog file still exists and is valid
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("catalog file missing after 2nd restart cycle: %v", err)
	}

	_ = db3.Close()
	_ = s3.Shutdown(context.Background())
}

// TestListPoolsEmptyCatalog tests that GET /api/pools returns empty array, not null.
func TestListPoolsEmptyCatalog(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: ":memory:",
		},
	}

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	driver := storage.NewSingleDriver(disks)

	s := New(cfg, db, "test-version")
	s.SetDriver(driver)
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s.RehydratePools(); err != nil {
		t.Fatalf("rehydrate pools: %v", err)
	}

	ts := httptest.NewServer(s.srv.Handler)
	token := mustToken(t, ts)
	defer ts.Close()

	plreq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pools", nil)
	plreq.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(plreq)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var payload struct {
		Pools []struct{
			ID string `json:"id"`
		} `json:"pools"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v (body: %s)", err, string(body))
	}

	// The key assertion: pools must be an empty array, not null
	if payload.Pools == nil {
		t.Errorf("pools field is null, expected empty array; body: %s", string(body))
	}
	if len(payload.Pools) != 0 {
		t.Errorf("expected 0 pools, got %d", len(payload.Pools))
	}
}

// TestCollectionsEndpoints tests the collections CRUD endpoints.
func TestCollectionsEndpoints(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: ":memory:",
		},
	}

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	driver := storage.NewSingleDriver(disks)

	s := New(cfg, db, "test-version")
	s.SetDriver(driver)
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }

	ts := httptest.NewServer(s.srv.Handler)
	token := mustToken(t, ts)
	defer ts.Close()

	doReq := func(method, path string, payload any) *http.Response {
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
		return resp
	}

	readError := func(resp *http.Response) string {
		defer resp.Body.Close()
		var e struct{ Error string `json:"error"` }
		json.NewDecoder(resp.Body).Decode(&e)
		return e.Error
	}

	// Create a pool first
	resp := doReq("POST", "/api/pools", map[string]any{
		"name":       "media",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST pool: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var poolResp struct {
		Pool struct {
			ID string `json:"id"`
		} `json:"pool"`
	}
	json.NewDecoder(resp.Body).Decode(&poolResp)
	resp.Body.Close()
	poolID := poolResp.Pool.ID

	// 1. POST /api/pools/:poolID/collections with empty name -> 400
	resp = doReq("POST", "/api/pools/"+poolID+"/collections", map[string]any{
		"name": "",
		"path": "/photos",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST with empty name: expected 400, got %d", resp.StatusCode)
	}
	errMsg := readError(resp)
	if !strings.Contains(errMsg, "name") {
		t.Errorf("expected name error, got: %s", errMsg)
	}

	// 2. POST /api/pools/:poolID/collections with empty path -> 400
	resp = doReq("POST", "/api/pools/"+poolID+"/collections", map[string]any{
		"name": "photos",
		"path": "",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST with empty path: expected 400, got %d", resp.StatusCode)
	}
	errMsg = readError(resp)
	if !strings.Contains(errMsg, "path") {
		t.Errorf("expected path error, got: %s", errMsg)
	}

	// 3. POST /api/pools/:poolID/collections with invalid disk_name -> 400
	resp = doReq("POST", "/api/pools/"+poolID+"/collections", map[string]any{
		"name":      "photos",
		"path":      "/photos",
		"disk_name": "sdz",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST with invalid disk_name: expected 400, got %d", resp.StatusCode)
	}
	errMsg = readError(resp)
	if !strings.Contains(errMsg, "disk_name") {
		t.Errorf("expected disk_name error, got: %s", errMsg)
	}

	// 4. POST /api/pools/:poolID/collections valid -> 201
	resp = doReq("POST", "/api/pools/"+poolID+"/collections", map[string]any{
		"name":      "photos",
		"path":      "/photos",
		"disk_name": "sda",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("POST valid collection: expected 201, got %d: %s", resp.StatusCode, string(b))
		resp.Body.Close()
	}
	var createResp struct {
		Collection struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			PoolID   string `json:"pool_id"`
			Path     string `json:"path"`
			DiskName string `json:"disk_name"`
		} `json:"collection"`
	}
	json.NewDecoder(resp.Body).Decode(&createResp)
	resp.Body.Close()
	if createResp.Collection.ID == "" || createResp.Collection.Name != "photos" {
		t.Errorf("unexpected collection response: %+v", createResp.Collection)
	}
	collID := createResp.Collection.ID

	// 5. GET /api/pools/:poolID/collections -> list with the created collection
	resp = doReq("GET", "/api/pools/"+poolID+"/collections", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET collections: expected 200, got %d", resp.StatusCode)
	}
	var listResp struct {
		Collections []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"collections"`
	}
	json.NewDecoder(resp.Body).Decode(&listResp)
	resp.Body.Close()
	if len(listResp.Collections) != 1 || listResp.Collections[0].ID != collID {
		t.Errorf("list collections mismatch: %+v", listResp.Collections)
	}

	// 6. GET /api/collections/:id -> 200
	resp = doReq("GET", "/api/collections/"+collID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET collection: expected 200, got %d", resp.StatusCode)
	}
	var getResp struct {
		Collection struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"collection"`
	}
	json.NewDecoder(resp.Body).Decode(&getResp)
	resp.Body.Close()
	if getResp.Collection.ID != collID || getResp.Collection.Name != "photos" {
		t.Errorf("get collection mismatch: %+v", getResp.Collection)
	}

	// 7. GET /api/collections/:id not found -> 404
	resp = doReq("GET", "/api/collections/coll-nonexistent", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET nonexistent collection: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 8. GET /api/pools/:poolID/collections unknown pool -> 404
	resp = doReq("GET", "/api/pools/pool-nonexistent/collections", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET collections unknown pool: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 9. POST /api/pools/:poolID/collections unknown pool -> 404
	resp = doReq("POST", "/api/pools/pool-nonexistent/collections", map[string]any{
		"name": "test",
		"path": "/test",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST collection unknown pool: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 10. POST /api/collections/:id/shares with missing protocol -> 400
	resp = doReq("POST", "/api/collections/"+collID+"/shares", map[string]any{
		"name": "share1",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST share missing protocol: expected 400, got %d", resp.StatusCode)
	}
	errMsg = readError(resp)
	if !strings.Contains(errMsg, "protocol") {
		t.Errorf("expected protocol error, got: %s", errMsg)
	}

	// 11. POST /api/collections/:id/shares with invalid protocol -> 400
	resp = doReq("POST", "/api/collections/"+collID+"/shares", map[string]any{
		"name":     "share1",
		"protocol": "invalid",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST share invalid protocol: expected 400, got %d", resp.StatusCode)
	}
	errMsg = readError(resp)
	if !strings.Contains(errMsg, "invalid protocol") {
		t.Errorf("expected invalid protocol error, got: %s", errMsg)
	}

	// 12. POST /api/collections/:id/shares valid -> 201
	resp = doReq("POST", "/api/collections/"+collID+"/shares", map[string]any{
		"name":      "share1",
		"protocol":  "smb",
		"read_only": false,
		"metadata":  "allowed_ips=192.168.1.0/24",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("POST valid share: expected 201, got %d: %s", resp.StatusCode, string(b))
		resp.Body.Close()
	}
	var shareResp struct {
		Share struct {
			ID           string `json:"id"`
			CollectionID string `json:"collection_id"`
			Name         string `json:"name"`
			Protocol     string `json:"protocol"`
			ReadOnly     bool   `json:"read_only"`
			Metadata     string `json:"metadata"`
		} `json:"share"`
	}
	json.NewDecoder(resp.Body).Decode(&shareResp)
	resp.Body.Close()
	if shareResp.Share.ID == "" || shareResp.Share.Name != "share1" || shareResp.Share.Protocol != "smb" {
		t.Errorf("unexpected share response: %+v", shareResp.Share)
	}
	if !strings.HasPrefix(shareResp.Share.ID, "share-") {
		t.Errorf("share ID should have 'share-' prefix: %s", shareResp.Share.ID)
	}
	shareID := shareResp.Share.ID

	// 13. POST /api/collections/:id/shares duplicate name -> 409
	resp = doReq("POST", "/api/collections/"+collID+"/shares", map[string]any{
		"name":     "share1",
		"protocol": "nfs",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("POST duplicate share name: expected 409, got %d", resp.StatusCode)
	}
	errMsg = readError(resp)
	if !strings.Contains(errMsg, "already exists") {
		t.Errorf("expected duplicate name error, got: %s", errMsg)
	}

	// 14. GET /api/collections/:id/shares -> list shares
	resp = doReq("GET", "/api/collections/"+collID+"/shares", nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET shares list: expected 200, got %d", resp.StatusCode)
	}
	var shareListResp struct {
		Shares []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Protocol string `json:"protocol"`
		} `json:"shares"`
	}
	json.NewDecoder(resp.Body).Decode(&shareListResp)
	resp.Body.Close()
	if len(shareListResp.Shares) != 1 || shareListResp.Shares[0].ID != shareID {
		t.Errorf("list shares mismatch: %+v", shareListResp.Shares)
	}

	// 15. GET /api/shares/:id -> 200
	resp = doReq("GET", "/api/shares/"+shareID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET share: expected 200, got %d", resp.StatusCode)
	}
	var shareGetResp struct {
		Share struct {
			ID           string `json:"id"`
			CollectionID string `json:"collection_id"`
			Name         string `json:"name"`
			Protocol     string `json:"protocol"`
			ReadOnly     bool   `json:"read_only"`
			Metadata     string `json:"metadata"`
		} `json:"share"`
	}
	json.NewDecoder(resp.Body).Decode(&shareGetResp)
	resp.Body.Close()
	if shareGetResp.Share.ID != shareID || shareGetResp.Share.Name != "share1" || shareGetResp.Share.Protocol != "smb" {
		t.Errorf("get share mismatch: %+v", shareGetResp.Share)
	}

	// 16. GET /api/shares/:id not found -> 404
	resp = doReq("GET", "/api/shares/share-nonexistent", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET nonexistent share: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 17. DELETE /api/shares/:id -> 204
	resp = doReq("DELETE", "/api/shares/"+shareID, nil)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("DELETE share: expected 204, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 18. GET /api/shares/:id after delete -> 404
	resp = doReq("GET", "/api/shares/"+shareID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET share after delete: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 19. DELETE /api/shares/:id not found -> 404
	resp = doReq("DELETE", "/api/shares/share-nonexistent", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE nonexistent share: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 20. POST /api/collections/:id/shares unknown collection -> 404
	resp = doReq("POST", "/api/collections/coll-nonexistent/shares", map[string]any{
		"name":     "share1",
		"protocol": "smb",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST share unknown collection: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 21. GET /api/collections/:id/shares unknown collection -> 404
	resp = doReq("GET", "/api/collections/coll-nonexistent/shares", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET shares unknown collection: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 22. DELETE /api/collections/:id -> 204
	resp = doReq("DELETE", "/api/collections/"+collID, nil)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("DELETE collection: expected 204, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 23. GET /api/collections/:id after delete -> 404
	resp = doReq("GET", "/api/collections/"+collID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET after delete: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 24. DELETE /api/collections/:id not found -> 404
	resp = doReq("DELETE", "/api/collections/coll-nonexistent", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE nonexistent: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestListCollectionsEmpty tests that GET /api/pools/:poolID/collections returns empty array, not null.
func TestListCollectionsEmpty(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: ":memory:",
		},
	}

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	driver := storage.NewSingleDriver(disks)

	s := New(cfg, db, "test-version")
	s.SetDriver(driver)
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }

	ts := httptest.NewServer(s.srv.Handler)
	token := mustToken(t, ts)
	defer ts.Close()

	// Create a pool
	payload := map[string]any{
		"name":       "media",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
	}
	b, _ := json.Marshal(payload)
	cpreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/pools", bytes.NewReader(b))
	cpreq.Header.Set("Content-Type", "application/json")
	cpreq.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(cpreq)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	var poolResp struct {
		Pool struct {
			ID string `json:"id"`
		} `json:"pool"`
	}
	json.NewDecoder(resp.Body).Decode(&poolResp)
	resp.Body.Close()
	poolID := poolResp.Pool.ID

	// GET collections for empty pool
	clreq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pools/"+poolID+"/collections", nil)
	clreq.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(clreq)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	var listResp struct {
		Collections []struct {
			ID string `json:"id"`
		} `json:"collections"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode body: %v (body: %s)", err, string(body))
	}

	// The key assertion: collections must be an empty array, not null
	if listResp.Collections == nil {
		t.Errorf("collections field is null, expected empty array; body: %s", string(body))
	}
	if len(listResp.Collections) != 0 {
		t.Errorf("expected 0 collections, got %d", len(listResp.Collections))
	}
}

// TestCollectionRestartPersistence tests that collections survive a server restart
// across a file-DB boundary (WS4 pattern).
func TestCollectionRestartPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "catalog.db")

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}

	// --- First server instance ---
	db1, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db1); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: dbPath,
		},
	}

	driver1 := storage.NewSingleDriver(disks)
	s1 := New(cfg, db1, "test-version")
	s1.SetDriver(driver1)
	s1.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s1.RehydratePools(); err != nil {
		t.Fatalf("rehydrate pools: %v", err)
	}

	ts1 := httptest.NewServer(s1.srv.Handler)
	token := mustToken(t, ts1)
	defer ts1.Close()

	doReq := func(method, path string, payload any) *http.Response {
		var body io.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts1.URL+path, body)
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
		return resp
	}

	// Create a pool
	resp := doReq("POST", "/api/pools", map[string]any{
		"name":       "media",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST pool: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var poolResp struct {
		Pool struct {
			ID string `json:"id"`
		} `json:"pool"`
	}
	json.NewDecoder(resp.Body).Decode(&poolResp)
	resp.Body.Close()
	poolID := poolResp.Pool.ID

	// Create a collection
	resp = doReq("POST", "/api/pools/"+poolID+"/collections", map[string]any{
		"name":      "photos",
		"path":      "/photos",
		"disk_name": "sda",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST collection: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var collResp struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	json.NewDecoder(resp.Body).Decode(&collResp)
	resp.Body.Close()
	collID := collResp.Collection.ID

	// Verify collection exists
	resp = doReq("GET", "/api/collections/"+collID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET collection: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Shutdown first server AND close its DB handle
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s1.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown server 1: %v", err)
	}
	cancel()
	if err := db1.Close(); err != nil {
		t.Fatalf("close db1: %v", err)
	}

	// --- Second server instance (restart on same FILE DB) ---
	db2, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog (restart): %v", err)
	}
	if err := catalog.Migrate(db2); err != nil {
		t.Fatalf("migrate (restart): %v", err)
	}

	driver2 := storage.NewSingleDriver(disks)
	s2 := New(cfg, db2, "test-version")
	s2.SetDriver(driver2)
	s2.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s2.RehydratePools(); err != nil {
		t.Fatalf("rehydrate pools on restart: %v", err)
	}

	ts2 := httptest.NewServer(s2.srv.Handler)
	defer ts2.Close()

	doReq2 := func(method, path string, payload any) *http.Response {
		var body io.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts2.URL+path, body)
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
		return resp
	}

	// GET the collection after restart -> should return 200
	resp = doReq2("GET", "/api/collections/"+collID, nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET after restart: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	_ = db2.Close()
	_ = s2.Shutdown(context.Background())
}

// TestPoolDeleteCascadesCollections tests that deleting a pool removes its collections
// via the FK cascade (foreign_keys pragma).
func TestPoolDeleteCascadesCollections(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: ":memory:",
		},
	}

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	driver := storage.NewSingleDriver(disks)

	s := New(cfg, db, "test-version")
	s.SetDriver(driver)
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }

	ts := httptest.NewServer(s.srv.Handler)
	token := mustToken(t, ts)
	defer ts.Close()

	doReq := func(method, path string, payload any) *http.Response {
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
		return resp
	}

	// Create a pool
	resp := doReq("POST", "/api/pools", map[string]any{
		"name":       "media",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST pool: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var poolResp struct {
		Pool struct {
			ID string `json:"id"`
		} `json:"pool"`
	}
	json.NewDecoder(resp.Body).Decode(&poolResp)
	resp.Body.Close()
	poolID := poolResp.Pool.ID

	// Create a collection in the pool
	resp = doReq("POST", "/api/pools/"+poolID+"/collections", map[string]any{
		"name": "photos",
		"path": "/photos",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST collection: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var collResp struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	json.NewDecoder(resp.Body).Decode(&collResp)
	resp.Body.Close()
	collID := collResp.Collection.ID

	// Delete the pool
	resp = doReq("DELETE", "/api/pools/"+poolID, nil)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("DELETE pool: expected 204, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// GET the collection should now return 404 (cascaded)
	resp = doReq("GET", "/api/collections/"+collID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET collection after pool delete: expected 404 (cascaded), got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestCollectionDeleteCascadesShares tests that deleting a collection removes its shares
// via the FK cascade (foreign_keys pragma).
func TestCollectionDeleteCascadesShares(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: ":memory:",
		},
	}

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	driver := storage.NewSingleDriver(disks)

	s := New(cfg, db, "test-version")
	s.SetDriver(driver)
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }

	ts := httptest.NewServer(s.srv.Handler)
	token := mustToken(t, ts)
	defer ts.Close()

	doReq := func(method, path string, payload any) *http.Response {
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
		return resp
	}

	// Create a pool
	resp := doReq("POST", "/api/pools", map[string]any{
		"name":       "media",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST pool: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var poolResp struct {
		Pool struct {
			ID string `json:"id"`
		} `json:"pool"`
	}
	json.NewDecoder(resp.Body).Decode(&poolResp)
	resp.Body.Close()
	poolID := poolResp.Pool.ID

	// Create a collection in the pool
	resp = doReq("POST", "/api/pools/"+poolID+"/collections", map[string]any{
		"name":      "photos",
		"path":      "/photos",
		"disk_name": "sda",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST collection: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var collResp struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	json.NewDecoder(resp.Body).Decode(&collResp)
	resp.Body.Close()
	collID := collResp.Collection.ID

	// Create a share for the collection
	resp = doReq("POST", "/api/collections/"+collID+"/shares", map[string]any{
		"name":     "share1",
		"protocol": "smb",
		"read_only": false,
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST share: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var shareResp struct {
		Share struct {
			ID string `json:"id"`
		} `json:"share"`
	}
	json.NewDecoder(resp.Body).Decode(&shareResp)
	resp.Body.Close()
	shareID := shareResp.Share.ID

	// Verify share exists
	resp = doReq("GET", "/api/shares/"+shareID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET share: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Delete the collection
	resp = doReq("DELETE", "/api/collections/"+collID, nil)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("DELETE collection: expected 204, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// GET the share should now return 404 (cascaded)
	resp = doReq("GET", "/api/shares/"+shareID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET share after collection delete: expected 404 (cascaded), got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Also verify list shares returns empty
	resp = doReq("GET", "/api/collections/"+collID+"/shares", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET shares list after collection delete: expected 404 (collection gone), got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestShareRestartPersistence tests that shares survive a server restart
// across a file-DB boundary (WS5 pattern).
func TestShareRestartPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "catalog.db")

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}

	// --- First server instance ---
	db1, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db1); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: dbPath,
		},
	}

	driver1 := storage.NewSingleDriver(disks)
	s1 := New(cfg, db1, "test-version")
	s1.SetDriver(driver1)
	s1.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s1.RehydratePools(); err != nil {
		t.Fatalf("rehydrate pools: %v", err)
	}

	ts1 := httptest.NewServer(s1.srv.Handler)
	token := mustToken(t, ts1)
	defer ts1.Close()

	doReq := func(method, path string, payload any) *http.Response {
		var body io.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts1.URL+path, body)
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
		return resp
	}

	// Create a pool
	resp := doReq("POST", "/api/pools", map[string]any{
		"name":       "media",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST pool: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var poolResp struct {
		Pool struct {
			ID string `json:"id"`
		} `json:"pool"`
	}
	json.NewDecoder(resp.Body).Decode(&poolResp)
	resp.Body.Close()
	poolID := poolResp.Pool.ID

	// Create a collection
	resp = doReq("POST", "/api/pools/"+poolID+"/collections", map[string]any{
		"name":      "photos",
		"path":      "/photos",
		"disk_name": "sda",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST collection: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var collResp struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	json.NewDecoder(resp.Body).Decode(&collResp)
	resp.Body.Close()
	collID := collResp.Collection.ID

	// Create a share
	resp = doReq("POST", "/api/collections/"+collID+"/shares", map[string]any{
		"name":     "share1",
		"protocol": "smb",
		"read_only": false,
		"metadata": "allowed_ips=192.168.1.0/24",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST share: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var shareResp struct {
		Share struct {
			ID string `json:"id"`
		} `json:"share"`
	}
	json.NewDecoder(resp.Body).Decode(&shareResp)
	resp.Body.Close()
	shareID := shareResp.Share.ID

	// Verify share exists
	resp = doReq("GET", "/api/shares/"+shareID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET share: expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Shutdown first server AND close its DB handle
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s1.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown server 1: %v", err)
	}
	cancel()
	if err := db1.Close(); err != nil {
		t.Fatalf("close db1: %v", err)
	}

	// --- Second server instance (restart on same FILE DB) ---
	db2, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog (restart): %v", err)
	}
	if err := catalog.Migrate(db2); err != nil {
		t.Fatalf("migrate (restart): %v", err)
	}

	driver2 := storage.NewSingleDriver(disks)
	s2 := New(cfg, db2, "test-version")
	s2.SetDriver(driver2)
	s2.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s2.RehydratePools(); err != nil {
		t.Fatalf("rehydrate pools on restart: %v", err)
	}

	ts2 := httptest.NewServer(s2.srv.Handler)
	defer ts2.Close()

	doReq2 := func(method, path string, payload any) *http.Response {
		var body io.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts2.URL+path, body)
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
		return resp
	}

	// GET the share after restart -> should return 200
	resp = doReq2("GET", "/api/shares/"+shareID, nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET share after restart: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// GET the collection's shares list after restart
	resp = doReq2("GET", "/api/collections/"+collID+"/shares", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET shares list after restart: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// DELETE the share after restart -> should return 204
	resp = doReq2("DELETE", "/api/shares/"+shareID, nil)
	if resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("DELETE share after restart: expected 204, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// GET the share after delete -> should return 404
	resp = doReq2("GET", "/api/shares/"+shareID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET share after delete: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Shutdown second server and close its DB handle
	shutdownCtx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	if err := s2.Shutdown(shutdownCtx2); err != nil {
		t.Fatalf("shutdown server 2: %v", err)
	}
	cancel2()
	if err := db2.Close(); err != nil {
		t.Fatalf("close db2: %v", err)
	}

	// --- Third server instance (second restart cycle) ---
	db3, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog (2nd restart): %v", err)
	}
	if err := catalog.Migrate(db3); err != nil {
		t.Fatalf("migrate (2nd restart): %v", err)
	}

	driver3 := storage.NewSingleDriver(disks)
	s3 := New(cfg, db3, "test-version")
	s3.SetDriver(driver3)
	s3.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s3.RehydratePools(); err != nil {
		t.Fatalf("rehydrate pools on 2nd restart: %v", err)
	}

	ts3 := httptest.NewServer(s3.srv.Handler)
	defer ts3.Close()

	doReq3 := func(method, path string, payload any) *http.Response {
		var body io.Reader
		if payload != nil {
			b, _ := json.Marshal(payload)
			body = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts3.URL+path, body)
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
		return resp
	}

	// Share should be gone (deleted in second instance)
	resp = doReq3("GET", "/api/shares/"+shareID, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET share after 2nd restart: expected 404 (share deleted), got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Verify catalog file still exists and is valid
	if _, err := os.Stat(dbPath); err != nil {
		t.Errorf("catalog file missing after 2nd restart cycle: %v", err)
	}

	_ = db3.Close()
	_ = s3.Shutdown(context.Background())
}

// TestDaemonConfigShareEndpoint tests GET /api/shares/{id}/daemon-config
func TestDaemonConfigShareEndpoint(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: ":memory:",
		},
	}

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	driver := storage.NewSingleDriver(disks)

	s := New(cfg, db, "test-version")
	s.SetDriver(driver)
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }

	ts := httptest.NewServer(s.srv.Handler)
	token := mustToken(t, ts)
	defer ts.Close()

	doReq := func(method, path string, payload any) *http.Response {
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
		return resp
	}

	// Create a pool
	resp := doReq("POST", "/api/pools", map[string]any{
		"name":       "media",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST pool: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var poolResp struct {
		Pool struct {
			ID string `json:"id"`
		} `json:"pool"`
	}
	json.NewDecoder(resp.Body).Decode(&poolResp)
	resp.Body.Close()
	poolID := poolResp.Pool.ID

	// Create a collection
	resp = doReq("POST", "/api/pools/"+poolID+"/collections", map[string]any{
		"name":      "photos",
		"path":      "/photos",
		"disk_name": "sda",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST collection: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var collResp struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	json.NewDecoder(resp.Body).Decode(&collResp)
	resp.Body.Close()
	collID := collResp.Collection.ID

	// Test 1: SMB share daemon-config
	resp = doReq("POST", "/api/collections/"+collID+"/shares", map[string]any{
		"name":      "smb-share",
		"protocol":  "smb",
		"read_only": false,
		"metadata":  `{"allowed_ips": "192.168.1.0/24"}`,
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST SMB share: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var shareResp struct {
		Share struct {
			ID string `json:"id"`
		} `json:"share"`
	}
	json.NewDecoder(resp.Body).Decode(&shareResp)
	resp.Body.Close()
	smbShareID := shareResp.Share.ID

	// GET /api/shares/{id}/daemon-config -> 200
	resp = doReq("GET", "/api/shares/"+smbShareID+"/daemon-config", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET SMB daemon-config: expected 200, got %d: %s", resp.StatusCode, string(b))
		resp.Body.Close()
	} else {
		var dcResp struct {
			Protocol   string `json:"protocol"`
			ConfigText string `json:"config_text"`
		}
		json.NewDecoder(resp.Body).Decode(&dcResp)
		resp.Body.Close()
		if dcResp.Protocol != "smb" {
			t.Errorf("expected protocol smb, got %s", dcResp.Protocol)
		}
		// Verify SMB config contains required elements
		checks := []string{
			"guest ok = no",
			"read only = no",
			"server min protocol = SMB2",
			"smb encrypt = desired",
			"client signing = required",
			"server signing = required",
			"path = /mnt/media/photos",
			"[smb-share]",
		}
		for _, check := range checks {
			if !strings.Contains(dcResp.ConfigText, check) {
				t.Errorf("SMB config missing '%s':\n%s", check, dcResp.ConfigText)
			}
		}
	}

	// Test 2: NFS share daemon-config (read-only default)
	resp = doReq("POST", "/api/collections/"+collID+"/shares", map[string]any{
		"name":      "nfs-share",
		"protocol":  "nfs",
		"read_only": false, // Even false, NFS defaults to ro
		"metadata":  `{"allowed_ips": "10.0.0.0/8"}`,
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST NFS share: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	json.NewDecoder(resp.Body).Decode(&shareResp)
	resp.Body.Close()
	nfsShareID := shareResp.Share.ID

	resp = doReq("GET", "/api/shares/"+nfsShareID+"/daemon-config", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET NFS daemon-config: expected 200, got %d: %s", resp.StatusCode, string(b))
		resp.Body.Close()
	} else {
		var dcResp struct {
			Protocol   string `json:"protocol"`
			ConfigText string `json:"config_text"`
		}
		json.NewDecoder(resp.Body).Decode(&dcResp)
		resp.Body.Close()
		if dcResp.Protocol != "nfs" {
			t.Errorf("expected protocol nfs, got %s", dcResp.Protocol)
		}
		// Verify NFS config: ro default, allowed IPs, no wildcard
		if !strings.Contains(dcResp.ConfigText, "ro") {
			t.Errorf("NFS config should default to ro:\n%s", dcResp.ConfigText)
		}
		if !strings.Contains(dcResp.ConfigText, "10.0.0.0/8") {
			t.Errorf("NFS config missing allowed IPs:\n%s", dcResp.ConfigText)
		}
		if strings.Contains(dcResp.ConfigText, "*(") {
			t.Errorf("NFS config must not contain wildcard:\n%s", dcResp.ConfigText)
		}
		if !strings.Contains(dcResp.ConfigText, "/mnt/media/photos") {
			t.Errorf("NFS config missing path:\n%s", dcResp.ConfigText)
		}
	}

	// Test 3: SFTP share daemon-config (keys-only)
	resp = doReq("POST", "/api/collections/"+collID+"/shares", map[string]any{
		"name":      "sftp-share",
		"protocol":  "sftp",
		"read_only": false,
		"metadata":  "",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST SFTP share: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	json.NewDecoder(resp.Body).Decode(&shareResp)
	resp.Body.Close()
	sftpShareID := shareResp.Share.ID

	resp = doReq("GET", "/api/shares/"+sftpShareID+"/daemon-config", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET SFTP daemon-config: expected 200, got %d: %s", resp.StatusCode, string(b))
		resp.Body.Close()
	} else {
		var dcResp struct {
			Protocol   string `json:"protocol"`
			ConfigText string `json:"config_text"`
		}
		json.NewDecoder(resp.Body).Decode(&dcResp)
		resp.Body.Close()
		if dcResp.Protocol != "sftp" {
			t.Errorf("expected protocol sftp, got %s", dcResp.Protocol)
		}
		// Verify SFTP config: keys-only, chroot, no password auth
		checks := []string{
			"PasswordAuthentication no",
			"PubkeyAuthentication yes",
			"ForceCommand internal-sftp",
			"ChrootDirectory /mnt/media/photos",
			"Match Group sftp-sftp-share",
		}
		for _, check := range checks {
			if !strings.Contains(dcResp.ConfigText, check) {
				t.Errorf("SFTP config missing '%s':\n%s", check, dcResp.ConfigText)
			}
		}
	}

	// Test 4: GET /api/shares/{id}/daemon-config not found -> 404
	resp = doReq("GET", "/api/shares/share-nonexistent/daemon-config", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET nonexistent share daemon-config: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Test 5: GET /api/daemon-config (all shares)
	resp = doReq("GET", "/api/daemon-config", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET /api/daemon-config: expected 200, got %d: %s", resp.StatusCode, string(b))
		resp.Body.Close()
	} else {
		var allResp struct {
			Configs map[string]string `json:"configs"`
		}
		json.NewDecoder(resp.Body).Decode(&allResp)
		resp.Body.Close()

		// Should have all three protocols
		for _, proto := range []string{"smb", "nfs", "sftp"} {
			if _, ok := allResp.Configs[proto]; !ok {
				t.Errorf("missing protocol %s in combined config", proto)
			}
			if allResp.Configs[proto] == "" {
				t.Errorf("protocol %s has empty config", proto)
			}
		}

		// SMB should have global section once and both share sections
		if strings.Count(allResp.Configs["smb"], "[global]") != 1 {
			t.Errorf("SMB global section should appear exactly once")
		}
		if !strings.Contains(allResp.Configs["smb"], "[smb-share]") {
			t.Errorf("combined SMB missing [smb-share]")
		}
		if !strings.Contains(allResp.Configs["nfs"], "10.0.0.0/8") {
			t.Errorf("combined NFS missing allowed IPs")
		}
		if !strings.Contains(allResp.Configs["sftp"], "sftp-share") {
			t.Errorf("combined SFTP missing sftp-share")
		}
	}
}

// TestDaemonConfigRenderStability tests that rendering the same share twice produces byte-identical output.
func TestDaemonConfigRenderStability(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: ":memory:",
		},
	}

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	driver := storage.NewSingleDriver(disks)

	s := New(cfg, db, "test-version")
	s.SetDriver(driver)
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }

	ts := httptest.NewServer(s.srv.Handler)
	token := mustToken(t, ts)
	defer ts.Close()

	doReq := func(method, path string, payload any) *http.Response {
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
		return resp
	}

	// Create pool, collection, share
	resp := doReq("POST", "/api/pools", map[string]any{
		"name":       "media",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
	})
	var poolResp struct{ Pool struct{ ID string `json:"id"` } }
	json.NewDecoder(resp.Body).Decode(&poolResp)
	resp.Body.Close()
	poolID := poolResp.Pool.ID

	resp = doReq("POST", "/api/pools/"+poolID+"/collections", map[string]any{
		"name":      "data",
		"path":      "/data",
		"disk_name": "sda",
	})
	var collResp struct{ Collection struct{ ID string `json:"id"` } }
	json.NewDecoder(resp.Body).Decode(&collResp)
	resp.Body.Close()
	collID := collResp.Collection.ID

	resp = doReq("POST", "/api/collections/"+collID+"/shares", map[string]any{
		"name":      "stable-share",
		"protocol":  "smb",
		"read_only": true,
		"metadata":  `{"allowed_ips": "192.168.1.0/24"}`,
	})
	var shareResp struct{ Share struct{ ID string `json:"id"` } }
	json.NewDecoder(resp.Body).Decode(&shareResp)
	resp.Body.Close()
	shareID := shareResp.Share.ID

	// First render
	resp = doReq("GET", "/api/shares/"+shareID+"/daemon-config", nil)
	var dcResp1 struct {
		Protocol   string `json:"protocol"`
		ConfigText string `json:"config_text"`
	}
	json.NewDecoder(resp.Body).Decode(&dcResp1)
	resp.Body.Close()

	// Second render
	resp = doReq("GET", "/api/shares/"+shareID+"/daemon-config", nil)
	var dcResp2 struct {
		Protocol   string `json:"protocol"`
		ConfigText string `json:"config_text"`
	}
	json.NewDecoder(resp.Body).Decode(&dcResp2)
	resp.Body.Close()

	if dcResp1.ConfigText != dcResp2.ConfigText {
		t.Errorf("render not stable: outputs differ\n--- first ---\n%s\n--- second ---\n%s", dcResp1.ConfigText, dcResp2.ConfigText)
	}
	if dcResp1.Protocol != dcResp2.Protocol {
		t.Errorf("protocol not stable: %s vs %s", dcResp1.Protocol, dcResp2.Protocol)
	}
}

// TestAuditLogEndpoints tests recording and querying audit log entries.
func TestAuditLogEndpoints(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: ":memory:",
		},
	}

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	driver := storage.NewSingleDriver(disks)

	s := New(cfg, db, "test-version")
	s.SetDriver(driver)
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }

	ts := httptest.NewServer(s.srv.Handler)
	token := mustToken(t, ts)
	defer ts.Close()

	doReq := func(method, path string, payload any) *http.Response {
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
		return resp
	}

	type AuditEntry struct {
		ID         string `json:"id"`
		CreatedAt  int64  `json:"created_at"`
		Actor      string `json:"actor"`
		Action     string `json:"action"`
		TargetType string `json:"target_type"`
		TargetID   string `json:"target_id"`
		Result     string `json:"result"`
		Detail     string `json:"detail,omitempty"`
	}

	type AuditLogResp struct {
		Entries []AuditEntry `json:"entries"`
		Total   int          `json:"total"`
	}

	getAuditLog := func(query string) AuditLogResp {
		resp := doReq("GET", "/api/audit-log"+query, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/audit-log failed: %d", resp.StatusCode)
		}
		defer resp.Body.Close()
		var r AuditLogResp
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatalf("decode audit log response: %v", err)
		}
		return r
	}

	// 1. Initial audit log is empty
	initial := getAuditLog("")
	if initial.Total != 2 || len(initial.Entries) != 2 {
		t.Errorf("expected 2 entries initially (auth.setup+auth.login), got %d (entries: %v)", initial.Total, initial.Entries)
	}
	if initial.Entries == nil {
		t.Errorf("entries must not be nil (empty slice required)")
	}

	// 2. Failed pool create (unknown disk 400)
	resp := doReq("POST", "/api/pools", map[string]any{
		"name":       "invalid-pool",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"nonexistent-disk"},
		"mountpoint": "/mnt/test",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown disk, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	afterFailedPool := getAuditLog("")
	if afterFailedPool.Total != 3 {
		t.Fatalf("expected 3 audit entries after failed pool create, got %d", afterFailedPool.Total)
	}
	e := afterFailedPool.Entries[0]
	if e.Action != "pool.create" || e.TargetType != "pool" || e.Result != "error" || !strings.Contains(e.Detail, "not in inventory") {
		t.Errorf("unexpected audit entry for failed pool: %+v", e)
	}
	if e.Actor != testAuthUser {
		t.Errorf("expected actor %q, got %s", testAuthUser, e.Actor)
	}
	if !strings.HasPrefix(e.ID, "audit-") {
		t.Errorf("expected audit ID format audit-..., got %s", e.ID)
	}

	// 3. Successful pool create (201)
	resp = doReq("POST", "/api/pools", map[string]any{
		"name":       "media",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for pool create, got %d", resp.StatusCode)
	}
	var poolResp struct{ Pool struct{ ID string `json:"id"` } }
	json.NewDecoder(resp.Body).Decode(&poolResp)
	resp.Body.Close()
	poolID := poolResp.Pool.ID

	afterPoolCreate := getAuditLog("")
	if afterPoolCreate.Total != 4 {
		t.Fatalf("expected 4 audit entries, got %d", afterPoolCreate.Total)
	}
	// Newest first!
	if afterPoolCreate.Entries[0].Action != "pool.create" || afterPoolCreate.Entries[0].Result != "ok" || afterPoolCreate.Entries[0].TargetID != poolID {
		t.Errorf("unexpected latest audit entry: %+v", afterPoolCreate.Entries[0])
	}

	// 4. Successful collection create (201)
	resp = doReq("POST", "/api/pools/"+poolID+"/collections", map[string]any{
		"name":      "photos",
		"path":      "/photos",
		"disk_name": "sda",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for collection create, got %d", resp.StatusCode)
	}
	var collResp struct{ Collection struct{ ID string `json:"id"` } }
	json.NewDecoder(resp.Body).Decode(&collResp)
	resp.Body.Close()
	collID := collResp.Collection.ID

	afterCollCreate := getAuditLog("")
	if afterCollCreate.Total != 5 {
		t.Fatalf("expected 5 audit entries, got %d", afterCollCreate.Total)
	}
	if afterCollCreate.Entries[0].Action != "collection.create" || afterCollCreate.Entries[0].Result != "ok" || afterCollCreate.Entries[0].TargetID != collID {
		t.Errorf("unexpected latest audit entry: %+v", afterCollCreate.Entries[0])
	}

	// 5. Successful share create (201)
	resp = doReq("POST", "/api/collections/"+collID+"/shares", map[string]any{
		"name":     "family-photos",
		"protocol": "smb",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for share create, got %d", resp.StatusCode)
	}
	var shareResp struct{ Share struct{ ID string `json:"id"` } }
	json.NewDecoder(resp.Body).Decode(&shareResp)
	resp.Body.Close()
	shareID := shareResp.Share.ID

	afterShareCreate := getAuditLog("")
	if afterShareCreate.Total != 6 {
		t.Fatalf("expected 6 audit entries, got %d", afterShareCreate.Total)
	}
	if afterShareCreate.Entries[0].Action != "share.create" || afterShareCreate.Entries[0].Result != "ok" || afterShareCreate.Entries[0].TargetID != shareID {
		t.Errorf("unexpected latest audit entry: %+v", afterShareCreate.Entries[0])
	}

	// 6. Duplicate share create (409) -> audit row with result=error
	resp = doReq("POST", "/api/collections/"+collID+"/shares", map[string]any{
		"name":     "family-photos",
		"protocol": "smb",
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for duplicate share, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	afterDupShare := getAuditLog("")
	if afterDupShare.Total != 7 {
		t.Fatalf("expected 7 audit entries, got %d", afterDupShare.Total)
	}
	if afterDupShare.Entries[0].Action != "share.create" || afterDupShare.Entries[0].Result != "error" || !strings.Contains(afterDupShare.Entries[0].Detail, "already exists") {
		t.Errorf("unexpected audit entry for duplicate share: %+v", afterDupShare.Entries[0])
	}

	// 7. Delete share (204) -> audit row
	resp = doReq("DELETE", "/api/shares/"+shareID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 for delete share, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	afterDelShare := getAuditLog("")
	if afterDelShare.Total != 8 {
		t.Fatalf("expected 8 audit entries, got %d", afterDelShare.Total)
	}
	if afterDelShare.Entries[0].Action != "share.delete" || afterDelShare.Entries[0].Result != "ok" || afterDelShare.Entries[0].TargetID != shareID {
		t.Errorf("unexpected audit entry for share delete: %+v", afterDelShare.Entries[0])
	}

	// 8. Delete collection (204) -> audit row
	resp = doReq("DELETE", "/api/collections/"+collID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 for delete collection, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	afterDelColl := getAuditLog("")
	if afterDelColl.Total != 9 {
		t.Fatalf("expected 9 audit entries, got %d", afterDelColl.Total)
	}
	if afterDelColl.Entries[0].Action != "collection.delete" || afterDelColl.Entries[0].Result != "ok" || afterDelColl.Entries[0].TargetID != collID {
		t.Errorf("unexpected audit entry for collection delete: %+v", afterDelColl.Entries[0])
	}

	// 9. Delete pool (204) -> audit row
	resp = doReq("DELETE", "/api/pools/"+poolID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 for delete pool, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	afterDelPool := getAuditLog("")
	if afterDelPool.Total != 10 {
		t.Fatalf("expected 10 audit entries, got %d", afterDelPool.Total)
	}
	if afterDelPool.Entries[0].Action != "pool.delete" || afterDelPool.Entries[0].Result != "ok" || afterDelPool.Entries[0].TargetID != poolID {
		t.Errorf("unexpected audit entry for pool delete: %+v", afterDelPool.Entries[0])
	}

	// 10. Filter checks
	// Filter by action: pool.create
	filteredAction := getAuditLog("?action=pool.create")
	if filteredAction.Total != 2 { // 1 failed + 1 successful
		t.Errorf("expected 2 pool.create entries, got %d", filteredAction.Total)
	}
	for _, entry := range filteredAction.Entries {
		if entry.Action != "pool.create" {
			t.Errorf("entry action does not match filter: %+v", entry)
		}
	}

	// Filter by target_type: share
	filteredTarget := getAuditLog("?target_type=share")
	if filteredTarget.Total != 3 { // 1 success create, 1 failed create, 1 delete
		t.Errorf("expected 3 share target entries, got %d", filteredTarget.Total)
	}
	for _, entry := range filteredTarget.Entries {
		if entry.TargetType != "share" {
			t.Errorf("entry target_type does not match filter: %+v", entry)
		}
	}

	// Filter by result: error
	filteredResult := getAuditLog("?result=error")
	if filteredResult.Total != 2 { // 1 failed pool create, 1 failed share create
		t.Errorf("expected 2 error entries, got %d", filteredResult.Total)
	}
	for _, entry := range filteredResult.Entries {
		if entry.Result != "error" {
			t.Errorf("entry result does not match filter: %+v", entry)
		}
	}

	// Combined filters: action=share.create AND result=error
	combined := getAuditLog("?action=share.create&result=error")
	if combined.Total != 1 {
		t.Errorf("expected 1 combined filter entry, got %d", combined.Total)
	}

	// Limit and offset pagination
	page1 := getAuditLog("?limit=2&offset=0")
	if len(page1.Entries) != 2 || page1.Total != 10 {
		t.Errorf("page1 limit/offset failed: len=%d, total=%d", len(page1.Entries), page1.Total)
	}
	page2 := getAuditLog("?limit=2&offset=2")
	if len(page2.Entries) != 2 || page2.Total != 10 {
		t.Errorf("page2 limit/offset failed: len=%d, total=%d", len(page2.Entries), page2.Total)
	}
	if page1.Entries[0].ID == page2.Entries[0].ID {
		t.Errorf("pagination returned duplicate entries across pages")
	}
}

// TestAuditLogRestartPersistence verifies that audit log rows survive server restart
// across file-DB boundary.
func TestAuditLogRestartPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "catalog.db")

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}

	// --- Instance 1 ---
	db1, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db1); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: dbPath,
		},
	}

	driver1 := storage.NewSingleDriver(disks)
	s1 := New(cfg, db1, "test-version")
	s1.SetDriver(driver1)
	s1.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s1.RehydratePools(); err != nil {
		t.Fatalf("rehydrate: %v", err)
	}

	ts1 := httptest.NewServer(s1.srv.Handler)
	token := mustToken(t, ts1)
	defer ts1.Close()

	// Create pool
	b, _ := json.Marshal(map[string]any{
		"name":       "media",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/media",
	})
	areq, _ := http.NewRequest(http.MethodPost, ts1.URL+"/api/pools", bytes.NewReader(b))
	areq.Header.Set("Content-Type", "application/json")
	areq.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(areq)
	if err != nil {
		t.Fatalf("post pool: %v", err)
	}
	resp.Body.Close()

	// Shutdown s1 & db1
	s1.Shutdown(context.Background())
	db1.Close()

	// --- Instance 2 (restart) ---
	db2, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog on restart: %v", err)
	}
	if err := catalog.Migrate(db2); err != nil {
		t.Fatalf("migrate on restart: %v", err)
	}

	driver2 := storage.NewSingleDriver(disks)
	s2 := New(cfg, db2, "test-version")
	s2.SetDriver(driver2)
	s2.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s2.RehydratePools(); err != nil {
		t.Fatalf("rehydrate on restart: %v", err)
	}

	ts2 := httptest.NewServer(s2.srv.Handler)
	defer ts2.Close()

	areq2, _ := http.NewRequest(http.MethodGet, ts2.URL+"/api/audit-log", nil)
	areq2.Header.Set("Authorization", "Bearer "+token)
	resp2, err := http.DefaultClient.Do(areq2)
	if err != nil {
		t.Fatalf("get audit log: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}

	var payload struct {
		Entries []struct {
			Action string `json:"action"`
			Result string `json:"result"`
		} `json:"entries"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if payload.Total != 3 || len(payload.Entries) != 3 {
		t.Fatalf("expected 3 persisted audit entries (setup+login+pool.create), got %d", payload.Total)
	}
	if payload.Entries[0].Action != "pool.create" || payload.Entries[0].Result != "ok" {
		t.Errorf("unexpected entry content: %+v", payload.Entries[0])
	}

	s2.Shutdown(context.Background())
	db2.Close()
}
