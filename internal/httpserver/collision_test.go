package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

// TestCollectionIDCollision tests that creating two collections within the same second
// doesn't cause primary key conflicts. Collection IDs are generated as
// "coll-<unix-nano>-<8 hex chars from crypto/rand>" - they must be unique even
// when created in rapid succession within the same second.
func TestCollectionIDCollision(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "catalog.db")

	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}

	db, err := catalog.Open(dbPath)
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
			Path: dbPath,
		},
	}

	driver := storage.NewSingleDriver(disks)
	s := New(cfg, db, "test-version")
	s.SetDriver(driver)
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	if err := s.RehydratePools(); err != nil {
		t.Fatalf("rehydrate pools: %v", err)
	}

	ts := httptest.NewServer(s.srv.Handler)
	defer ts.Close()
	token := mustToken(t, ts)

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
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s failed: %v", method, path, err)
		}
		return resp
	}

	readBody := func(resp *http.Response) string {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
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

	// Create first collection
	resp = doReq("POST", "/api/pools/"+poolID+"/collections", map[string]any{
		"name":      "photos1",
		"path":      "/photos1",
		"disk_name": "sda",
	})
	if resp.StatusCode != http.StatusCreated {
		body1 := readBody(resp)
		t.Fatalf("First collection create: expected 201, got %d: %s", resp.StatusCode, body1)
	}
	body1 := readBody(resp)
	var collResp1 struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	json.Unmarshal([]byte(body1), &collResp1)
	collID1 := collResp1.Collection.ID

	// Create second collection immediately (same second)
	resp = doReq("POST", "/api/pools/"+poolID+"/collections", map[string]any{
		"name":      "photos2",
		"path":      "/photos2",
		"disk_name": "sda",
	})
	if resp.StatusCode != http.StatusCreated {
		body2 := readBody(resp)
		t.Fatalf("Second collection create: expected 201, got %d: %s", resp.StatusCode, body2)
	}
	body2 := readBody(resp)
	var collResp2 struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	json.Unmarshal([]byte(body2), &collResp2)
	collID2 := collResp2.Collection.ID

	// ASSERT: Both creates must return 201 with DISTINCT IDs
	if collID1 == collID2 {
		t.Fatalf("COLLISION: Both collections have SAME ID: %s (expected distinct IDs)", collID1)
	}

	// ASSERT: Both collections must be retrievable via GET
	resp = doReq("GET", "/api/collections/"+collID1, nil)
	if resp.StatusCode != http.StatusOK {
		body := readBody(resp)
		t.Fatalf("GET collection 1: expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	resp = doReq("GET", "/api/collections/"+collID2, nil)
	if resp.StatusCode != http.StatusOK {
		body := readBody(resp)
		t.Fatalf("GET collection 2: expected 200, got %d: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Verify ID format: coll-<unix-nano>-<8 hex chars>
	if len(collID1) < 15 || collID1[:5] != "coll-" {
		t.Errorf("Collection 1 ID format unexpected: %s", collID1)
	}
	if len(collID2) < 15 || collID2[:5] != "coll-" {
		t.Errorf("Collection 2 ID format unexpected: %s", collID2)
	}

	t.Logf("SUCCESS: Both collections created with distinct IDs")
	t.Logf("Collection 1 ID: %s", collID1)
	t.Logf("Collection 2 ID: %s", collID2)
}