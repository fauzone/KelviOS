package httpserver

// WS27 (FR-1.3) heat tests — Chief takeover: Saka's session ended at the
// call ceiling after the catalog tests, leaving the HTTP layer (endpoints +
// recording hooks) untested. These tests cover exactly that gap: recording
// via real HTTP handlers, heat/heated endpoints, and share recording with
// the token-prefix actor rule.

import (
	"bytes"
	"database/sql"
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

func setupHeatServer(t *testing.T, db *sql.DB) (*httptest.Server, string, string) {
	t.Helper()
	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{Path: ":memory:"},
	}
	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-HEAT", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	s := New(cfg, db, "test-version")
	s.SetDriver(storage.NewSingleDriver(disks))
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	ts := httptest.NewServer(s.srv.Handler)

	doReq := func(method, path string, payload any, token string) *http.Response {
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

	// Auth: setup + login (setup may 409 on repeat in shared servers; fresh DB here).
	resp := doReq("POST", "/api/auth/setup", map[string]any{"username": "admin", "password": "adminpassword123"}, "")
	resp.Body.Close()
	resp = doReq("POST", "/api/auth/login", map[string]any{"username": "admin", "password": "adminpassword123"}, "")
	var login struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&login); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	resp.Body.Close()
	token := login.Token

	resp = doReq("POST", "/api/pools", map[string]any{"name": "media", "tier": "warm", "mechanism": "single", "disks": []string{"sda"}, "mountpoint": "/mnt/media"}, token)
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

	resp = doReq("POST", "/api/pools/"+poolResp.Pool.ID+"/collections", map[string]any{"name": "photos", "path": "photos", "disk_name": "sda"}, token)
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

	// Fixture tree + mountpoint rewrite (same pattern as files_test.go).
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "mnt", "media", "photos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	mountpoint := filepath.Join(tmpDir, "mnt", "media")
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, mountpoint, poolResp.Pool.ID); err != nil {
		t.Fatalf("update pool mountpoint: %v", err)
	}

	// Scan to index a.txt (heat endpoint requires an indexed file).
	resp = doReq("POST", "/api/collections/"+collID+"/scan", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST scan: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()
	return ts, token, collID
}

func heatDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) *http.Response {
	t.Helper()
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

func TestHeatRecordingViaHTTP(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID := setupHeatServer(t, db)
	defer ts.Close()

	// 1. Successful content + preview accesses record events.
	resp := heatDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET content: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()
	resp = heatDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/preview?path=a.txt", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET preview: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 2. Failed access records nothing.
	resp = heatDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path=missing.txt", nil, token)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("GET missing content: expected non-200, got 200")
	}
	resp.Body.Close()

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM access_events WHERE collection_id = ?`, collID).Scan(&count); err != nil {
		t.Fatalf("count access_events: %v", err)
	}
	if count != 2 {
		t.Errorf("access_events: got %d, want 2 (failed access must not record)", count)
	}

	// 3. Heat endpoint reflects the two accesses.
	resp = heatDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/heat?path=a.txt", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET heat: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var heat struct {
		Count      int   `json:"count"`
		LastAccess int64 `json:"last_access"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&heat); err != nil {
		t.Fatalf("decode heat: %v", err)
	}
	resp.Body.Close()
	if heat.Count != 2 {
		t.Errorf("heat count: got %d, want 2", heat.Count)
	}
	if heat.LastAccess <= 0 {
		t.Errorf("heat last_access: got %d, want > 0", heat.LastAccess)
	}

	// 4. Heated endpoint lists a.txt on top.
	resp = heatDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/heated", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET heated: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var heated struct {
		Files []struct {
			RelPath string `json:"rel_path"`
			Count   int    `json:"count"`
		} `json:"files"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&heated); err != nil {
		t.Fatalf("decode heated: %v", err)
	}
	resp.Body.Close()
	if heated.Total != 1 || len(heated.Files) != 1 || heated.Files[0].RelPath != "a.txt" || heated.Files[0].Count != 2 {
		t.Errorf("heated: got %+v, want [{a.txt 2}] total 1", heated)
	}
}

func TestHeatEndpointsValidation(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID := setupHeatServer(t, db)
	defer ts.Close()

	// Unauthenticated -> 401.
	resp := heatDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/heat?path=a.txt", nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauth heat: got %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Missing path -> 400.
	resp = heatDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/heat", nil, token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("heat without path: got %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Unindexed file -> 404.
	resp = heatDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/heat?path=missing.txt", nil, token)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("heat unindexed file: got %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown collection -> 404.
	resp = heatDoReq(t, ts, "GET", "/api/collections/coll-nope/files/heat?path=a.txt", nil, token)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("heat unknown collection: got %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHeatShareRecording(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID := setupHeatServer(t, db)
	defer ts.Close()

	resp := heatDoReq(t, ts, "POST", "/api/collections/"+collID+"/share-links", map[string]any{"rel_path": "a.txt"}, token)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST share-link: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var linkResp struct {
		ShareLink struct {
			Token string `json:"token"`
		} `json:"share_link"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&linkResp); err != nil {
		t.Fatalf("decode share-link: %v", err)
	}
	resp.Body.Close()
	fullToken := linkResp.ShareLink.Token
	if len(fullToken) != 32 {
		t.Fatalf("share token length: got %d, want 32", len(fullToken))
	}

	// Public access (no auth) must succeed and record a share event.
	resp = heatDoReq(t, ts, "GET", "/api/public/share/"+fullToken, nil, "")
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET public share: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	var actor, kind string
	err = db.QueryRow(`SELECT actor, kind FROM access_events WHERE collection_id = ? AND kind = 'share'`, collID).Scan(&actor, &kind)
	if err != nil {
		t.Fatalf("query share event: %v", err)
	}
	if !strings.HasPrefix(actor, "share:") || len(actor) != len("share:")+8 {
		t.Errorf("share actor: got %q, want share:<8-char prefix>", actor)
	}
	if actor == "share:"+fullToken {
		t.Errorf("share actor leaks the full token")
	}
}

// TestCollectionHeatEndpoint tests GET /api/collections/{id}/heat
func TestCollectionHeatEndpoint(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID := setupHeatServer(t, db)
	defer ts.Close()

	// Seed a heat row via ComputeHeatScores
	now := time.Now()
	if _, err := catalog.ComputeHeatScores(db, now); err != nil {
		t.Fatalf("ComputeHeatScores: %v", err)
	}

	// GET heat -> 200 with stored row
	resp := heatDoReq(t, ts, "GET", "/api/collections/"+collID+"/heat", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET heat: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var heat struct {
		CollectionID string  `json:"collection_id"`
		Score        float64 `json:"score"`
		EventCount   int     `json:"event_count"`
		LastAccess   int64   `json:"last_access"`
		SizeBytes    int64   `json:"size_bytes"`
		ComputedAt   int64   `json:"computed_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&heat); err != nil {
		t.Fatalf("decode heat: %v", err)
	}
	resp.Body.Close()
	if heat.CollectionID != collID {
		t.Errorf("collection_id: got %s, want %s", heat.CollectionID, collID)
	}
	if heat.ComputedAt == 0 {
		t.Errorf("computed_at should be > 0, got 0")
	}

	// Unknown collection -> 404
	resp = heatDoReq(t, ts, "GET", "/api/collections/coll-nope/heat", nil, token)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("heat unknown collection: got %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// Unauthenticated -> 401
	resp = heatDoReq(t, ts, "GET", "/api/collections/"+collID+"/heat", nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauth heat: got %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestCollectionHeatZeroRow tests GET heat for collection with no heat row yet.
func TestCollectionHeatZeroRow(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID := setupHeatServer(t, db)
	defer ts.Close()

	// Do NOT run ComputeHeatScores - collection exists but no heat row

	resp := heatDoReq(t, ts, "GET", "/api/collections/"+collID+"/heat", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET heat: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var heat struct {
		CollectionID string  `json:"collection_id"`
		Score        float64 `json:"score"`
		EventCount   int     `json:"event_count"`
		LastAccess   int64   `json:"last_access"`
		SizeBytes    int64   `json:"size_bytes"`
		ComputedAt   int64   `json:"computed_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&heat); err != nil {
		t.Fatalf("decode heat: %v", err)
	}
	resp.Body.Close()
	if heat.CollectionID != collID {
		t.Errorf("collection_id: got %s, want %s", heat.CollectionID, collID)
	}
	if heat.Score != 0 {
		t.Errorf("score: got %v, want 0", heat.Score)
	}
	if heat.EventCount != 0 {
		t.Errorf("event_count: got %d, want 0", heat.EventCount)
	}
	if heat.LastAccess != 0 {
		t.Errorf("last_access: got %d, want 0", heat.LastAccess)
	}
	if heat.SizeBytes != 0 {
		t.Errorf("size_bytes: got %d, want 0", heat.SizeBytes)
	}
	if heat.ComputedAt != 0 {
		t.Errorf("computed_at: got %d, want 0", heat.ComputedAt)
	}
}

// TestHeatRecomputeEndpoint tests POST /api/heat/recompute
func TestHeatRecomputeEndpoint(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _ := setupHeatServer(t, db)
	defer ts.Close()

	// POST recompute -> 200 {computed: n}
	resp := heatDoReq(t, ts, "POST", "/api/heat/recompute", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST recompute: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var recomputeResp struct {
		Computed int `json:"computed"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&recomputeResp); err != nil {
		t.Fatalf("decode recompute: %v", err)
	}
	resp.Body.Close()
	if recomputeResp.Computed < 1 {
		t.Errorf("computed: got %d, want >= 1", recomputeResp.Computed)
	}

	// Verify audit row action heat.recompute result ok
	var action, result string
	err = db.QueryRow(`SELECT action, result FROM audit_log WHERE action = 'heat.recompute' ORDER BY created_at DESC LIMIT 1`).Scan(&action, &result)
	if err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	if action != "heat.recompute" || result != "ok" {
		t.Errorf("audit log: got action=%s result=%s, want heat.recompute ok", action, result)
	}

	// Unauthenticated -> 401
	resp = heatDoReq(t, ts, "POST", "/api/heat/recompute", nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauth recompute: got %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Wrong method -> 405
	resp = heatDoReq(t, ts, "GET", "/api/heat/recompute", nil, token)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET recompute: got %d, want 405", resp.StatusCode)
	}
	resp.Body.Close()
}
