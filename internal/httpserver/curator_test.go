package httpserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

func setupCuratorServer(t *testing.T, db *sql.DB) (*httptest.Server, string, string, string) {
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
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-CURATOR", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
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

	// Auth: setup + login
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

	// Create pool
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
	poolID := poolResp.Pool.ID

	// Create collection
	resp = doReq("POST", "/api/pools/"+poolID+"/collections", map[string]any{"name": "photos", "path": "photos", "disk_name": "sda"}, token)
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

	// Fixture tree + mountpoint rewrite
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "mnt", "media", "photos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	mountpoint := filepath.Join(tmpDir, "mnt", "media")
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, mountpoint, poolID); err != nil {
		t.Fatalf("update pool mountpoint: %v", err)
	}

	// Scan to index a.txt
	resp = doReq("POST", "/api/collections/"+collID+"/scan", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST scan: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	return ts, token, poolID, collID
}

func curatorDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) *http.Response {
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

// TestCuratorGenerateListRoundtrip tests generate + list roundtrip via HTTP.
func TestCuratorGenerateListRoundtrip(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, poolID, collID := setupCuratorServer(t, db)
	defer ts.Close()

	// Set collection tier to warm and last_access to 30 days ago via DB
	now := time.Unix(1700000000, 0)
	lastAcc := now.Add(-30 * 24 * time.Hour).Unix()
	if _, err := db.Exec(`UPDATE collections SET last_accessed_at = ? WHERE id = ?`, lastAcc, collID); err != nil {
		t.Fatalf("update collection last_access: %v", err)
	}

	// Generate recommendations
	resp := curatorDoReq(t, ts, "POST", "/api/curator/recommendations/generate", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST generate: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var genResp struct {
		Created int `json:"created"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&genResp); err != nil {
		t.Fatalf("decode generate: %v", err)
	}
	resp.Body.Close()
	if genResp.Created != 1 {
		t.Errorf("created: got %d, want 1", genResp.Created)
	}

	// List pending recommendations
	resp = curatorDoReq(t, ts, "GET", "/api/curator/recommendations?status=pending", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET list pending: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var listResp struct {
		Recommendations []map[string]any `json:"recommendations"`
		Total           int              `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	if listResp.Total != 1 || len(listResp.Recommendations) != 1 {
		t.Errorf("list: got total=%d len=%d, want 1", listResp.Total, len(listResp.Recommendations))
	}
	if listResp.Recommendations[0]["collection_id"] != collID {
		t.Errorf("collection_id: got %v, want %s", listResp.Recommendations[0]["collection_id"], collID)
	}
	if listResp.Recommendations[0]["direction"] != "warm_to_cold" {
		t.Errorf("direction: got %v, want warm_to_cold", listResp.Recommendations[0]["direction"])
	}
	if listResp.Recommendations[0]["status"] != "pending" {
		t.Errorf("status: got %v, want pending", listResp.Recommendations[0]["status"])
	}
	_ = poolID
}

// TestCuratorGenerateDedup tests that second generate creates 0 via HTTP.
func TestCuratorGenerateDedup(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, collID := setupCuratorServer(t, db)
	defer ts.Close()

	now := time.Unix(1700000000, 0)
	lastAcc := now.Add(-30 * 24 * time.Hour).Unix()
	if _, err := db.Exec(`UPDATE collections SET last_accessed_at = ? WHERE id = ?`, lastAcc, collID); err != nil {
		t.Fatalf("update collection last_access: %v", err)
	}

	// First generate
	resp := curatorDoReq(t, ts, "POST", "/api/curator/recommendations/generate", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST generate 1: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var genResp struct {
		Created int `json:"created"`
	}
	json.NewDecoder(resp.Body).Decode(&genResp)
	resp.Body.Close()
	if genResp.Created != 1 {
		t.Errorf("first created: got %d, want 1", genResp.Created)
	}

	// Second generate -> 0 (dedup)
	resp = curatorDoReq(t, ts, "POST", "/api/curator/recommendations/generate", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST generate 2: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	json.NewDecoder(resp.Body).Decode(&genResp)
	resp.Body.Close()
	if genResp.Created != 0 {
		t.Errorf("second created: got %d, want 0", genResp.Created)
	}
}

// TestCuratorApproveRejectViaHTTP tests approve/reject via HTTP endpoints.
func TestCuratorApproveRejectViaHTTP(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, collID := setupCuratorServer(t, db)
	defer ts.Close()

	now := time.Unix(1700000000, 0)
	lastAcc := now.Add(-30 * 24 * time.Hour).Unix()
	if _, err := db.Exec(`UPDATE collections SET last_accessed_at = ? WHERE id = ?`, lastAcc, collID); err != nil {
		t.Fatalf("update collection last_access: %v", err)
	}

	// Generate
	resp := curatorDoReq(t, ts, "POST", "/api/curator/recommendations/generate", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST generate: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// List pending to get ID
	resp = curatorDoReq(t, ts, "GET", "/api/curator/recommendations?status=pending", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET list pending: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var listResp struct {
		Recommendations []map[string]any `json:"recommendations"`
		Total           int              `json:"total"`
	}
	json.NewDecoder(resp.Body).Decode(&listResp)
	resp.Body.Close()
	if listResp.Total != 1 {
		t.Fatalf("expected 1 pending, got %d", listResp.Total)
	}
	recID := listResp.Recommendations[0]["id"].(string)

	// Approve
	resp = curatorDoReq(t, ts, "POST", "/api/curator/recommendations/"+recID+"/approve", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST approve: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var decideResp struct {
		Status string `json:"status"`
	}
	json.NewDecoder(resp.Body).Decode(&decideResp)
	resp.Body.Close()
	if decideResp.Status != "approved" {
		t.Errorf("status: got %s, want approved", decideResp.Status)
	}

	// Approve again -> 409 (not pending)
	resp = curatorDoReq(t, ts, "POST", "/api/curator/recommendations/"+recID+"/approve", nil, token)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("approve non-pending: got %d, want 409", resp.StatusCode)
	}
	resp.Body.Close()

	// Generate another for reject test
	if _, err := db.Exec(`INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, last_accessed_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"coll-2", "photos2", "pool-1", "/photos2", "sda", 0, lastAcc, now.Unix(), now.Unix()); err != nil {
		t.Fatalf("insert coll-2: %v", err)
	}
	resp = curatorDoReq(t, ts, "POST", "/api/curator/recommendations/generate", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST generate 2: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	resp = curatorDoReq(t, ts, "GET", "/api/curator/recommendations?status=pending", nil, token)
	json.NewDecoder(resp.Body).Decode(&listResp)
	resp.Body.Close()
	if listResp.Total != 1 {
		t.Fatalf("expected 1 pending after second generate, got %d", listResp.Total)
	}
	recID2 := listResp.Recommendations[0]["id"].(string)

	// Reject
	resp = curatorDoReq(t, ts, "POST", "/api/curator/recommendations/"+recID2+"/reject", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST reject: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	json.NewDecoder(resp.Body).Decode(&decideResp)
	resp.Body.Close()
	if decideResp.Status != "rejected" {
		t.Errorf("status: got %s, want rejected", decideResp.Status)
	}

	// Unknown id -> 404
	resp = curatorDoReq(t, ts, "POST", "/api/curator/recommendations/nonexistent-id/approve", nil, token)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("approve unknown id: got %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	_ = collID
}

// TestCuratorPinUnpinViaHTTP tests pin and unpin via HTTP.
func TestCuratorPinUnpinViaHTTP(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, collID := setupCuratorServer(t, db)
	defer ts.Close()

	// Pin
	resp := curatorDoReq(t, ts, "POST", "/api/collections/"+collID+"/pin", map[string]any{"note": "test pin"}, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST pin: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Unpin
	resp = curatorDoReq(t, ts, "DELETE", "/api/collections/"+collID+"/pin", nil, token)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("DELETE unpin: got %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()

	// Unpin again -> 404 (not pinned)
	resp = curatorDoReq(t, ts, "DELETE", "/api/collections/"+collID+"/pin", nil, token)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("DELETE unpin again: got %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestCuratorTierPolicyViaHTTP tests get/set tier policy via HTTP.
func TestCuratorTierPolicyViaHTTP(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, poolID, _ := setupCuratorServer(t, db)
	defer ts.Close()

	// Get default policy (no row set)
	resp := curatorDoReq(t, ts, "GET", "/api/pools/"+poolID+"/tier-policy", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET tier-policy: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var policyResp struct {
		Mode               string `json:"mode"`
		ColdThresholdDays  int    `json:"cold_threshold_days"`
		AutoMoveWaitDays   int    `json:"auto_move_wait_days"`
	}
	json.NewDecoder(resp.Body).Decode(&policyResp)
	resp.Body.Close()
	if policyResp.Mode != "recommend_only" {
		t.Errorf("default mode: got %s, want recommend_only", policyResp.Mode)
	}
	if policyResp.ColdThresholdDays != 21 {
		t.Errorf("default cold_threshold_days: got %d, want 21", policyResp.ColdThresholdDays)
	}
	if policyResp.AutoMoveWaitDays != 7 {
		t.Errorf("default auto_move_wait_days: got %d, want 7", policyResp.AutoMoveWaitDays)
	}

	// Set policy
	resp = curatorDoReq(t, ts, "PUT", "/api/pools/"+poolID+"/tier-policy", map[string]any{
		"mode":                "auto_if_no_response",
		"cold_threshold_days": 14,
		"auto_move_wait_days": 3,
	}, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT tier-policy: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	json.NewDecoder(resp.Body).Decode(&policyResp)
	resp.Body.Close()
	if policyResp.Mode != "auto_if_no_response" {
		t.Errorf("set mode: got %s, want auto_if_no_response", policyResp.Mode)
	}
	if policyResp.ColdThresholdDays != 14 {
		t.Errorf("set cold_threshold_days: got %d, want 14", policyResp.ColdThresholdDays)
	}
	if policyResp.AutoMoveWaitDays != 3 {
		t.Errorf("set auto_move_wait_days: got %d, want 3", policyResp.AutoMoveWaitDays)
	}

	// Invalid mode -> 400
	resp = curatorDoReq(t, ts, "PUT", "/api/pools/"+poolID+"/tier-policy", map[string]any{
		"mode":                "invalid_mode",
		"cold_threshold_days": 21,
		"auto_move_wait_days": 7,
	}, token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT invalid mode: got %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// Invalid days -> 400
	resp = curatorDoReq(t, ts, "PUT", "/api/pools/"+poolID+"/tier-policy", map[string]any{
		"mode":                "recommend_only",
		"cold_threshold_days": 0,
		"auto_move_wait_days": 7,
	}, token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT invalid days: got %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestCuratorAuthAndNotFound tests 401/404 paths.
func TestCuratorAuthAndNotFound(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, collID := setupCuratorServer(t, db)
	defer ts.Close()

	// No token -> 401
	resp := curatorDoReq(t, ts, "POST", "/api/curator/recommendations/generate", nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("generate no token: got %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown recommendation id -> 404
	resp = curatorDoReq(t, ts, "POST", "/api/curator/recommendations/nonexistent-id/approve", nil, token)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("approve unknown id: got %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// Pin unknown collection -> 404
	resp = curatorDoReq(t, ts, "POST", "/api/collections/nonexistent-id/pin", nil, token)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("pin unknown collection: got %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// Tier policy for unknown pool -> 404
	resp = curatorDoReq(t, ts, "GET", "/api/pools/nonexistent-id/tier-policy", nil, token)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET tier-policy unknown pool: got %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	_ = collID
}
