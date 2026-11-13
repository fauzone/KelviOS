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

// setupPromotionServer creates a test server with:
// - warm pool (target for shadows) with temp mountpoint
// - cold pool (source) with temp mountpoint
// - one collection on cold pool with a fixture file
// Returns ts, token, warmPoolID, coldPoolID, collID, tmpDir, coldDiskName
func setupPromotionServer(t *testing.T, db *sql.DB) (*httptest.Server, string, string, string, string, string, string) {
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
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-SHADOW-WARM", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
		{Name: "sdb", Path: "/dev/sdb", Model: "WDC WD40EFRX", Serial: "WD-SHADOW-COLD", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
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

	tmpDir := t.TempDir()
	warmMount := filepath.Join(tmpDir, "mnt", "warm")
	coldMount := filepath.Join(tmpDir, "mnt", "cold")
	if err := os.MkdirAll(filepath.Join(coldMount, "photos"), 0o755); err != nil {
		t.Fatalf("mkdir cold tree: %v", err)
	}
	if err := os.MkdirAll(warmMount, 0o755); err != nil {
		t.Fatalf("mkdir warm mount: %v", err)
	}
	// Fixture file on cold pool
	if err := os.WriteFile(filepath.Join(coldMount, "photos", "a.txt"), []byte("hello from cold"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	createPool := func(name, tier, disk, mount string) string {
		resp := doReq("POST", "/api/pools", map[string]any{"name": name, "tier": tier, "mechanism": "single", "disks": []string{disk}, "mountpoint": mount}, token)
		if resp.StatusCode != http.StatusCreated {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST pool %s: expected 201, got %d: %s", name, resp.StatusCode, string(b))
		}
		var pr struct {
			Pool struct {
				ID string `json:"id"`
			} `json:"pool"`
		}
		json.NewDecoder(resp.Body).Decode(&pr)
		resp.Body.Close()
		return pr.Pool.ID
	}
	warmPoolID := createPool("warm", "warm", "sda", warmMount)
	coldPoolID := createPool("cold", "cold", "sdb", coldMount)

	resp = doReq("POST", "/api/pools/"+coldPoolID+"/collections", map[string]any{"name": "photos", "path": "photos", "disk_name": "sdb"}, token)
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

	// Scan the collection so the fixture file is indexed (download looks up the file row).
	resp = doReq("POST", "/api/collections/"+collResp.Collection.ID+"/scan", nil, token)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST scan: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	return ts, token, warmPoolID, coldPoolID, collResp.Collection.ID, tmpDir, "sdb"
}

func promoDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) *http.Response {
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

func promoExpect(t *testing.T, resp *http.Response, want int, what string) {
	t.Helper()
	if resp.StatusCode != want {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("%s: expected %d, got %d: %s", what, want, resp.StatusCode, string(b))
	}
}

func promoDecode(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// seedAccessEvents inserts access events directly into the DB for a given
// collection and rel_path. Used to quickly create >10 access events for
// promotion eligibility without making 12+ HTTP downloads.
func seedAccessEvents(t *testing.T, db *sql.DB, collectionID, relPath string, count int) {
	t.Helper()
	now := time.Now().Unix()
	for i := 0; i < count; i++ {
		// Spread timestamps slightly to simulate real access pattern
		// actor="test", kind="content" to satisfy NOT NULL constraints
		_, err := db.Exec(`
			INSERT INTO access_events (id, collection_id, rel_path, actor, kind, accessed_at)
			VALUES (?, ?, ?, 'test', 'content', ?)
		`, "ae-"+collectionID+"-"+relPath+"-"+string(rune(i)), collectionID, relPath, now-int64(i))
		if err != nil {
			t.Fatalf("seed access event %d: %v", i, err)
		}
	}
}

// createShadowViaDownloads creates a shadow for the given file by:
// 1. Waking the cold disk
// 2. Downloading the file twice (2nd access creates shadow)
func createShadowViaDownloads(t *testing.T, ts *httptest.Server, token, coldDisk, collID, relPath string) {
	t.Helper()
	// Wake cold disk
	resp := promoDoReq(t, ts, "POST", "/api/power/"+coldDisk+"/wake", nil, token)
	promoExpect(t, resp, http.StatusOK, "POST wake cold disk")
	resp.Body.Close()

	// First download (1st access -> no shadow yet)
	resp = promoDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path="+relPath, nil, token)
	promoExpect(t, resp, http.StatusOK, "GET content 1st access")
	resp.Body.Close()

	// Second download (2nd access within window -> shadow created)
	resp = promoDoReq(t, ts, "GET", "/api/collections/"+collID+"/files/content?path="+relPath, nil, token)
	promoExpect(t, resp, http.StatusOK, "GET content 2nd access")
	resp.Body.Close()
}

// TestShadowPromotionGenerate tests the generate endpoint:
// - One shadow + >10 access events -> POST /api/shadows/promotions/generate -> 200 {created: 1}
// - Second generate -> {created: 0} (dedup: non-superseded row blocks)
func TestShadowPromotionGenerate(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _, collID, _, coldDisk := setupPromotionServer(t, db)
	defer ts.Close()

	// Create shadow via two downloads
	createShadowViaDownloads(t, ts, token, coldDisk, collID, "a.txt")

	// Seed >10 access events (threshold is 10, need >10 so 11)
	seedAccessEvents(t, db, collID, "a.txt", 11)

	// First generate -> should create 1 promotion
	resp := promoDoReq(t, ts, "POST", "/api/shadows/promotions/generate", nil, token)
	promoExpect(t, resp, http.StatusOK, "POST generate 1st")
	var genResp1 struct {
		Created int `json:"created"`
	}
	promoDecode(t, resp, &genResp1)
	if genResp1.Created != 1 {
		t.Errorf("generate 1st: created=%d, want 1", genResp1.Created)
	}

	// Second generate -> should create 0 (dedup: non-superseded row exists)
	resp = promoDoReq(t, ts, "POST", "/api/shadows/promotions/generate", nil, token)
	promoExpect(t, resp, http.StatusOK, "POST generate 2nd")
	var genResp2 struct {
		Created int `json:"created"`
	}
	promoDecode(t, resp, &genResp2)
	if genResp2.Created != 0 {
		t.Errorf("generate 2nd: created=%d, want 0 (dedup)", genResp2.Created)
	}
}

// TestShadowPromotionList tests the list endpoint:
// - GET /api/shadows/promotions?status=pending -> 200 with promotion from generate
// - ?status=approved -> empty before any approval
func TestShadowPromotionList(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _, collID, _, coldDisk := setupPromotionServer(t, db)
	defer ts.Close()

	// Create shadow and seed access events
	createShadowViaDownloads(t, ts, token, coldDisk, collID, "a.txt")
	seedAccessEvents(t, db, collID, "a.txt", 11)

	// Generate promotion
	resp := promoDoReq(t, ts, "POST", "/api/shadows/promotions/generate", nil, token)
	promoExpect(t, resp, http.StatusOK, "POST generate")
	resp.Body.Close()

	// List pending -> should contain the promotion
	resp = promoDoReq(t, ts, "GET", "/api/shadows/promotions?status=pending", nil, token)
	promoExpect(t, resp, http.StatusOK, "GET list pending")
	var listResp struct {
		Promotions []map[string]any `json:"promotions"`
		Total      int              `json:"total"`
	}
	promoDecode(t, resp, &listResp)
	if listResp.Total != 1 {
		t.Errorf("list pending: total=%d, want 1", listResp.Total)
	}
	if len(listResp.Promotions) != 1 {
		t.Errorf("list pending: promotions count=%d, want 1", len(listResp.Promotions))
	}
	promo := listResp.Promotions[0]
	if promo["collection_id"] != collID {
		t.Errorf("promotion collection_id: got %v, want %s", promo["collection_id"], collID)
	}
	if promo["rel_path"] != "a.txt" {
		t.Errorf("promotion rel_path: got %v, want a.txt", promo["rel_path"])
	}
	if promo["status"] != "pending" {
		t.Errorf("promotion status: got %v, want pending", promo["status"])
	}
	// access_count_30d = 11 seeded + 2 from downloads = 13
	if promo["access_count_30d"].(float64) != 13 {
		t.Errorf("promotion access_count_30d: got %v, want 13 (11 seeded + 2 downloads)", promo["access_count_30d"])
	}

	// List approved -> should be empty before any approval
	resp = promoDoReq(t, ts, "GET", "/api/shadows/promotions?status=approved", nil, token)
	promoExpect(t, resp, http.StatusOK, "GET list approved")
	var approvedResp struct {
		Promotions []map[string]any `json:"promotions"`
		Total      int              `json:"total"`
	}
	promoDecode(t, resp, &approvedResp)
	if approvedResp.Total != 0 {
		t.Errorf("list approved before approval: total=%d, want 0", approvedResp.Total)
	}
}

// TestShadowPromotionApproveReject tests approve/reject endpoints:
// - POST /api/shadows/promotions/{id}/approve -> 200, status becomes "approve" (verbatim, NOT "approved")
// - Second promotion (different rel_path) rejected -> status "reject"
// - Double-decide -> error (409 Conflict)
func TestShadowPromotionApproveReject(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, coldPoolID, collID, tmpDir, coldDisk := setupPromotionServer(t, db)
	defer ts.Close()

	// Create first shadow (a.txt) and seed access events
	createShadowViaDownloads(t, ts, token, coldDisk, collID, "a.txt")
	seedAccessEvents(t, db, collID, "a.txt", 11)

	// Generate first promotion
	resp := promoDoReq(t, ts, "POST", "/api/shadows/promotions/generate", nil, token)
	promoExpect(t, resp, http.StatusOK, "POST generate 1st")
	var genResp1 struct {
		Created int `json:"created"`
	}
	promoDecode(t, resp, &genResp1)
	if genResp1.Created != 1 {
		t.Errorf("generate 1st: created=%d, want 1", genResp1.Created)
	}

	// Get the promotion ID
	resp = promoDoReq(t, ts, "GET", "/api/shadows/promotions?status=pending", nil, token)
	promoExpect(t, resp, http.StatusOK, "GET list pending")
	var listResp1 struct {
		Promotions []map[string]any `json:"promotions"`
	}
	promoDecode(t, resp, &listResp1)
	promo1ID := listResp1.Promotions[0]["id"].(string)

	// Approve first promotion -> status should become "approve" (verbatim)
	resp = promoDoReq(t, ts, "POST", "/api/shadows/promotions/"+promo1ID+"/approve", nil, token)
	promoExpect(t, resp, http.StatusOK, "POST approve 1st")
	var approveResp struct {
		Status string `json:"status"`
	}
	promoDecode(t, resp, &approveResp)
	if approveResp.Status != "approve" {
		t.Errorf("approve response status: got %q, want %q (verbatim, NOT approved)", approveResp.Status, "approve")
	}

	// Verify status in DB is "approve"
	var status string
	err = db.QueryRow(`SELECT status FROM shadow_promotions WHERE id = ?`, promo1ID).Scan(&status)
	if err != nil {
		t.Fatalf("query promotion status: %v", err)
	}
	if status != "approve" {
		t.Errorf("DB promotion status: got %q, want %q", status, "approve")
	}

	// Create second collection on same cold pool with different file (b.txt)
	// to test partial unique index (collection+rel_path must be unique for non-superseded)
	coldMount := filepath.Join(tmpDir, "mnt", "cold")
	if err := os.WriteFile(filepath.Join(coldMount, "photos", "b.txt"), []byte("second file"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}

	// Create second collection using coldPoolID
	resp = promoDoReq(t, ts, "POST", "/api/pools/"+coldPoolID+"/collections", map[string]any{"name": "photos2", "path": "photos", "disk_name": "sdb"}, token)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST collection 2: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var collResp2 struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	json.NewDecoder(resp.Body).Decode(&collResp2)
	resp.Body.Close()
	collID2 := collResp2.Collection.ID

	// Scan second collection
	resp = promoDoReq(t, ts, "POST", "/api/collections/"+collID2+"/scan", nil, token)
	promoExpect(t, resp, http.StatusOK, "POST scan 2nd collection")
	resp.Body.Close()

	// Create shadow for b.txt
	createShadowViaDownloads(t, ts, token, coldDisk, collID2, "b.txt")
	seedAccessEvents(t, db, collID2, "b.txt", 11)

	// Generate second promotion
	resp = promoDoReq(t, ts, "POST", "/api/shadows/promotions/generate", nil, token)
	promoExpect(t, resp, http.StatusOK, "POST generate 2nd")
	var genResp2 struct {
		Created int `json:"created"`
	}
	promoDecode(t, resp, &genResp2)
	if genResp2.Created != 1 {
		t.Errorf("generate 2nd: created=%d, want 1", genResp2.Created)
	}

	// Get second promotion ID
	resp = promoDoReq(t, ts, "GET", "/api/shadows/promotions?status=pending", nil, token)
	promoExpect(t, resp, http.StatusOK, "GET list pending 2nd")
	var listResp2 struct {
		Promotions []map[string]any `json:"promotions"`
	}
	promoDecode(t, resp, &listResp2)
	promo2ID := listResp2.Promotions[0]["id"].(string)

	// Reject second promotion -> status should become "reject"
	resp = promoDoReq(t, ts, "POST", "/api/shadows/promotions/"+promo2ID+"/reject", nil, token)
	promoExpect(t, resp, http.StatusOK, "POST reject 2nd")
	var rejectResp struct {
		Status string `json:"status"`
	}
	promoDecode(t, resp, &rejectResp)
	if rejectResp.Status != "reject" {
		t.Errorf("reject response status: got %q, want %q", rejectResp.Status, "reject")
	}

	// Verify status in DB is "reject"
	err = db.QueryRow(`SELECT status FROM shadow_promotions WHERE id = ?`, promo2ID).Scan(&status)
	if err != nil {
		t.Fatalf("query promotion 2 status: %v", err)
	}
	if status != "reject" {
		t.Errorf("DB promotion 2 status: got %q, want %q", status, "reject")
	}

	// Double-decide on first promotion (already approved) -> should error (409 Conflict)
	resp = promoDoReq(t, ts, "POST", "/api/shadows/promotions/"+promo1ID+"/approve", nil, token)
	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("double approve: expected 409, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Double-decide on second promotion (already rejected) -> should error (409 Conflict)
	resp = promoDoReq(t, ts, "POST", "/api/shadows/promotions/"+promo2ID+"/reject", nil, token)
	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("double reject: expected 409, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()
}

// TestShadowPromotionUnknownID tests approve/reject with unknown promotion ID -> 404
func TestShadowPromotionUnknownID(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _, _, _, _ := setupPromotionServer(t, db)
	defer ts.Close()

	unknownID := "shp-unknown-1234567890-abcdef12"

	// Approve unknown ID -> 404
	resp := promoDoReq(t, ts, "POST", "/api/shadows/promotions/"+unknownID+"/approve", nil, token)
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("approve unknown: expected 404, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// Reject unknown ID -> 404
	resp = promoDoReq(t, ts, "POST", "/api/shadows/promotions/"+unknownID+"/reject", nil, token)
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("reject unknown: expected 404, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()
}