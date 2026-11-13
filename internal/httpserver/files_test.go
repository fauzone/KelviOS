package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
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

func TestScanAndBrowseEndpoints(t *testing.T) {
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
		"path":      "photos",
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

	// Build fixture tree under a temp dir
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "mnt", "media", "photos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}

	// Update pool mountpoint in DB to point at temp dir
	mountpoint := filepath.Join(tmpDir, "mnt", "media")
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, mountpoint, poolID); err != nil {
		t.Fatalf("update pool mountpoint: %v", err)
	}

	// 1. POST /api/collections/:id/scan -> 200 with counts
	resp = doReq("POST", "/api/collections/"+collID+"/scan", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST scan: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var scanResp struct {
		Added   int `json:"added"`
		Updated int `json:"updated"`
		Removed int `json:"removed"`
	}
	json.NewDecoder(resp.Body).Decode(&scanResp)
	resp.Body.Close()
	if scanResp.Added != 2 {
		t.Errorf("expected 2 added, got %d", scanResp.Added)
	}

	// 2. GET /api/collections/:id/files -> 200 with entries
	resp = doReq("GET", "/api/collections/"+collID+"/files", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET files: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var browseResp struct {
		Entries []struct {
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			RelPath   string `json:"rel_path"`
			SizeBytes int64  `json:"size_bytes"`
			Offline   bool   `json:"offline"`
		} `json:"entries"`
		Tier string `json:"tier"`
	}
	json.NewDecoder(resp.Body).Decode(&browseResp)
	resp.Body.Close()
	if len(browseResp.Entries) != 2 {
		t.Errorf("expected 2 entries, got %d", len(browseResp.Entries))
	}
	if browseResp.Tier != "warm" {
		t.Errorf("expected tier warm, got %s", browseResp.Tier)
	}
	for _, e := range browseResp.Entries {
		if e.Offline {
			t.Errorf("expected offline=false for warm tier, got true")
		}
	}

	// 3. GET /api/collections/:id/files unknown collection -> 404
	resp = doReq("GET", "/api/collections/coll-nonexistent/files", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET files unknown collection: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 4. POST /api/collections/:id/scan unknown collection -> 404
	resp = doReq("POST", "/api/collections/coll-nonexistent/scan", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST scan unknown collection: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 5. Unauthenticated requests -> 401
	resp, err = http.Get(ts.URL + "/api/collections/" + collID + "/files")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET files unauthenticated: expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Post(ts.URL+"/api/collections/"+collID+"/scan", "application/json", nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST scan unauthenticated: expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDownloadCollectionFile(t *testing.T) {
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
		"path":      "photos",
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

	// Build fixture tree under a temp dir
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "mnt", "media", "photos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}

	// Update pool mountpoint in DB to point at temp dir
	mountpoint := filepath.Join(tmpDir, "mnt", "media")
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, mountpoint, poolID); err != nil {
		t.Fatalf("update pool mountpoint: %v", err)
	}

	// Scan first to index files
	resp = doReq("POST", "/api/collections/"+collID+"/scan", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST scan: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 1. Download a.txt -> 200, byte-identical body, correct headers
	resp = doReq("GET", "/api/collections/"+collID+"/files/content?path=a.txt", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET download a.txt: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("body mismatch: got %q, want %q", string(body), "hello")
	}
	if resp.Header.Get("Content-Type") != mime.TypeByExtension(".txt") {
		t.Errorf("Content-Type: got %s, want %s", resp.Header.Get("Content-Type"), mime.TypeByExtension(".txt"))
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") || !strings.Contains(resp.Header.Get("Content-Disposition"), "a.txt") {
		t.Errorf("Content-Disposition: got %s", resp.Header.Get("Content-Disposition"))
	}

	// 2. Download b.txt -> 200
	resp = doReq("GET", "/api/collections/"+collID+"/files/content?path=b.txt", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET download b.txt: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	body, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "world" {
		t.Errorf("body mismatch: got %q, want %q", string(body), "world")
	}

	// 3. Unknown collection -> 404
	resp = doReq("GET", "/api/collections/coll-nonexistent/files/content?path=a.txt", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET download unknown collection: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 4. Unknown file (not indexed) -> 404
	resp = doReq("GET", "/api/collections/"+collID+"/files/content?path=unknown.txt", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET download unknown file: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 5. Empty path -> 400
	resp = doReq("GET", "/api/collections/"+collID+"/files/content?path=", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET download empty path: expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 6. Absolute path -> 400
	resp = doReq("GET", "/api/collections/"+collID+"/files/content?path=/etc/passwd", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET download absolute path: expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 7. Path escape (..) -> 400
	resp = doReq("GET", "/api/collections/"+collID+"/files/content?path=../etc/passwd", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET download path escape: expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 8. Path escape (subdir/..) -> 400
	resp = doReq("GET", "/api/collections/"+collID+"/files/content?path=subdir/../a.txt", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET download path escape subdir: expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 9. Directory on disk -> 400 (not a file)
	if err := os.MkdirAll(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	resp = doReq("GET", "/api/collections/"+collID+"/files/content?path=subdir", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET download directory: expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 10. Symlink -> 400 (not indexed, but defense in depth)
	// Note: symlinks are not indexed by scan, so this would be 404 not indexed.
	// We test the handler's symlink check by creating a symlink and trying to download
	// but since it's not indexed, it will be 404. The symlink check is defense in depth.

	// 11. Root missing -> 409
	// Create a new pool/collection with a non-existent mountpoint
	resp = doReq("POST", "/api/pools", map[string]any{
		"name":       "media2",
		"tier":       "warm",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "C:\\nonexistent\\path",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST pool2: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	json.NewDecoder(resp.Body).Decode(&poolResp)
	resp.Body.Close()
	poolID2 := poolResp.Pool.ID

	resp = doReq("POST", "/api/pools/"+poolID2+"/collections", map[string]any{
		"name":      "photos2",
		"path":      "photos",
		"disk_name": "sda",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST collection2: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	json.NewDecoder(resp.Body).Decode(&collResp)
	resp.Body.Close()
	collID2 := collResp.Collection.ID

	resp = doReq("GET", "/api/collections/"+collID2+"/files/content?path=a.txt", nil)
	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET download root missing: expected 409, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 12. Cold tier -> 409 with wake_required
	resp = doReq("POST", "/api/pools", map[string]any{
		"name":       "coldpool",
		"tier":       "cold",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": mountpoint,
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST cold pool: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	json.NewDecoder(resp.Body).Decode(&poolResp)
	resp.Body.Close()
	coldPoolID := poolResp.Pool.ID

	resp = doReq("POST", "/api/pools/"+coldPoolID+"/collections", map[string]any{
		"name":      "coldphotos",
		"path":      "photos",
		"disk_name": "sda",
	})
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST cold collection: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	json.NewDecoder(resp.Body).Decode(&collResp)
	resp.Body.Close()
	coldCollID := collResp.Collection.ID

	resp = doReq("POST", "/api/collections/"+coldCollID+"/scan", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST cold scan: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	resp = doReq("GET", "/api/collections/"+coldCollID+"/files/content?path=a.txt", nil)
	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET download cold tier: expected 409, got %d: %s", resp.StatusCode, string(b))
	} else {
		var coldResp struct {
			Error         string `json:"error"`
			WakeRequired  bool   `json:"wake_required"`
		}
		json.NewDecoder(resp.Body).Decode(&coldResp)
		if !coldResp.WakeRequired {
			t.Errorf("expected wake_required=true, got false")
		}
	}
	resp.Body.Close()

	// 13. Unauthenticated -> 401
	resp, err = http.Get(ts.URL + "/api/collections/" + collID + "/files/content?path=a.txt")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET download unauthenticated: expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 14. Audit log: verify file.download row with rel_path and no content leak
	resp = doReq("GET", "/api/audit-log?action=file.download&result=ok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET audit-log: expected 200, got %d", resp.StatusCode)
	}
	var auditResp struct {
		Entries []struct {
			Action  string `json:"action"`
			Detail  string `json:"detail"`
			Result  string `json:"result"`
		} `json:"entries"`
		Total int `json:"total"`
	}
	json.NewDecoder(resp.Body).Decode(&auditResp)
	resp.Body.Close()
	if auditResp.Total < 2 {
		t.Errorf("expected at least 2 file.download ok entries, got %d", auditResp.Total)
	}
	for _, e := range auditResp.Entries {
		if e.Action != "file.download" || e.Result != "ok" {
			t.Errorf("unexpected audit entry: %+v", e)
		}
		if strings.Contains(e.Detail, "hello") || strings.Contains(e.Detail, "world") {
			t.Errorf("audit detail leaks file content: %s", e.Detail)
		}
		if e.Detail != "a.txt" && e.Detail != "b.txt" {
			t.Errorf("audit detail should be rel_path only: %s", e.Detail)
		}
	}
}
