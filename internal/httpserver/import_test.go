package httpserver

import (
	"bytes"
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

func TestImportEndpoints(t *testing.T) {
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

	// Create a pool (warm tier)
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

	// Build fixture source tree under a temp dir
	tmpDir := t.TempDir()

	// Validation (Opsi R, keputusan Kapten 2026-10-30): import sources must
	// resolve under the allowed root. Production root is /mnt; tests override
	// it to this test's temp dir (restored on return).
	oldRoot := importAllowedRoot
	importAllowedRoot = tmpDir
	defer func() { importAllowedRoot = oldRoot }()
	sourceRoot := filepath.Join(tmpDir, "source")
	if err := os.MkdirAll(filepath.Join(sourceRoot, "sub", "deep"), 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "sub", "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "sub", "deep", "c.bin"), []byte("binary\x00\x01\x02"), 0o644); err != nil {
		t.Fatalf("write c.bin: %v", err)
	}

	// Update pool mountpoint in DB to point at temp dir (for destination)
	mountpoint := filepath.Join(tmpDir, "mnt", "media")
	if _, err := db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, mountpoint, poolID); err != nil {
		t.Fatalf("update pool mountpoint: %v", err)
	}

	// 1. Unauthenticated POST /import -> 401
	resp, err = http.Post(ts.URL+"/api/collections/"+collID+"/import", "application/json", nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST import unauthenticated: expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 2. Unauthenticated GET /imports -> 401
	resp, err = http.Get(ts.URL + "/api/collections/" + collID + "/imports")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET imports unauthenticated: expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 3. GET on /import -> 405
	resp = doReq("GET", "/api/collections/"+collID+"/import", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /import: expected 405, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 4. POST on /imports -> 405
	resp = doReq("POST", "/api/collections/"+collID+"/imports", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /imports: expected 405, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 5. Unknown collection POST -> 404
	resp = doReq("POST", "/api/collections/coll-nonexistent/import", map[string]string{"source_path": "/tmp"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("POST import unknown collection: expected 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 6. Malformed JSON -> 400
	resp = doReq("POST", "/api/collections/"+collID+"/import", "not json")
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("malformed JSON: expected 400, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 7. Empty source_path -> 400
	resp = doReq("POST", "/api/collections/"+collID+"/import", map[string]string{"source_path": ""})
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("empty source_path: expected 400, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 8. Nonexistent source dir -> 400
	resp = doReq("POST", "/api/collections/"+collID+"/import", map[string]string{"source_path": "/nonexistent/path"})
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("nonexistent source: expected 400, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 9. Happy path: POST /import with valid source
	resp = doReq("POST", "/api/collections/"+collID+"/import", map[string]string{
		"source_path": sourceRoot,
		"fs_hint":     "NTFS",
	})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST import happy path: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var importResp struct {
		Import struct {
			ID           string `json:"id"`
			CollectionID string `json:"collection_id"`
			SourcePath   string `json:"source_path"`
			FSType       string `json:"fs_type"`
			TotalFiles   int    `json:"total_files"`
			TotalBytes   int64  `json:"total_bytes"`
			Copied       int    `json:"copied"`
			Failed       int    `json:"failed"`
			BytesCopied  int64  `json:"bytes_copied"`
			TS           int64  `json:"ts"`
		} `json:"import"`
		Plan struct {
			TotalFiles int   `json:"total_files"`
			TotalBytes int64 `json:"total_bytes"`
		} `json:"plan"`
		Report struct {
			Files []struct {
				Path   string `json:"path"`
				Size   int64  `json:"size"`
				SHA256 string `json:"sha256"`
				Status string `json:"status"`
				Err    string `json:"err"`
			} `json:"files"`
			Copied      int   `json:"copied"`
			Failed      int   `json:"failed"`
			BytesCopied int64 `json:"bytes_copied"`
		} `json:"report"`
	}
	json.NewDecoder(resp.Body).Decode(&importResp)
	resp.Body.Close()

	// Verify import record
	if importResp.Import.CollectionID != collID {
		t.Errorf("import collection_id: got %s want %s", importResp.Import.CollectionID, collID)
	}
	if importResp.Import.SourcePath != sourceRoot {
		t.Errorf("import source_path: got %s want %s", importResp.Import.SourcePath, sourceRoot)
	}
	if importResp.Import.FSType != "ntfs" {
		t.Errorf("import fs_type: got %s want ntfs", importResp.Import.FSType)
	}
	if importResp.Import.TotalFiles != 5 { // 3 files + 2 dirs (sub, sub/deep)
		t.Errorf("import total_files: got %d want 5", importResp.Import.TotalFiles)
	}
	if importResp.Import.Copied != 3 {
		t.Errorf("import copied: got %d want 3", importResp.Import.Copied)
	}
	if importResp.Import.Failed != 0 {
		t.Errorf("import failed: got %d want 0", importResp.Import.Failed)
	}
	if importResp.Import.BytesCopied != 19 { // 5 + 5 + 9
		t.Errorf("import bytes_copied: got %d want 19", importResp.Import.BytesCopied)
	}
	if !strings.HasPrefix(importResp.Import.ID, "imp-") {
		t.Errorf("import ID prefix: got %s", importResp.Import.ID)
	}

	// Verify plan
	if importResp.Plan.TotalFiles != 5 {
		t.Errorf("plan total_files: got %d want 5", importResp.Plan.TotalFiles)
	}
	if importResp.Plan.TotalBytes != 19 {
		t.Errorf("plan total_bytes: got %d want 19", importResp.Plan.TotalBytes)
	}

	// Verify report
	if importResp.Report.Copied != 3 {
		t.Errorf("report copied: got %d want 3", importResp.Report.Copied)
	}
	if importResp.Report.Failed != 0 {
		t.Errorf("report failed: got %d want 0", importResp.Report.Failed)
	}
	if importResp.Report.BytesCopied != 19 {
		t.Errorf("report bytes_copied: got %d want 19", importResp.Report.BytesCopied)
	}
	if len(importResp.Report.Files) != 3 {
		t.Errorf("report files len: got %d want 3", len(importResp.Report.Files))
	}
	for _, fr := range importResp.Report.Files {
		if fr.Status != "ok" {
			t.Errorf("report file status: got %s want ok", fr.Status)
		}
		if fr.Err != "" {
			t.Errorf("report file err should be empty: got %s", fr.Err)
		}
	}

	// 10. Verify 3 files exist under collection root with byte-identical content
	destRoot := filepath.Join(mountpoint, "photos")
	expectedFiles := map[string]string{
		"a.txt":          "hello",
		"sub/b.txt":      "world",
		"sub/deep/c.bin": "binary\x00\x01\x02",
	}
	for relPath, expectedContent := range expectedFiles {
		fullPath := filepath.Join(destRoot, relPath)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			t.Errorf("dest file %s: read error: %v", relPath, err)
			continue
		}
		if string(data) != expectedContent {
			t.Errorf("dest file %s: content mismatch", relPath)
		}
	}

	// 11. GET /imports -> total=1 with matching row
	resp = doReq("GET", "/api/collections/"+collID+"/imports", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET imports: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var listResp struct {
		Imports []struct {
			ID           string `json:"id"`
			CollectionID string `json:"collection_id"`
			SourcePath   string `json:"source_path"`
			FSType       string `json:"fs_type"`
			TotalFiles   int    `json:"total_files"`
			TotalBytes   int64  `json:"total_bytes"`
			Copied       int    `json:"copied"`
			Failed       int    `json:"failed"`
			BytesCopied  int64  `json:"bytes_copied"`
			TS           int64  `json:"ts"`
		} `json:"imports"`
		Total int `json:"total"`
	}
	json.NewDecoder(resp.Body).Decode(&listResp)
	resp.Body.Close()
	if listResp.Total != 1 {
		t.Errorf("imports total: got %d want 1", listResp.Total)
	}
	if len(listResp.Imports) != 1 {
		t.Errorf("imports len: got %d want 1", len(listResp.Imports))
	}
	if listResp.Imports[0].ID != importResp.Import.ID {
		t.Errorf("imports[0].ID mismatch: got %s want %s", listResp.Imports[0].ID, importResp.Import.ID)
	}

	// 12. Audit log contains import.execute with result ok and actor = session username
	resp = doReq("GET", "/api/audit-log?action=import.execute&result=ok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET audit-log: expected 200, got %d", resp.StatusCode)
	}
	var auditResp struct {
		Entries []struct {
			Action string `json:"action"`
			Detail string `json:"detail"`
			Result string `json:"result"`
			Actor  string `json:"actor"`
		} `json:"entries"`
		Total int `json:"total"`
	}
	json.NewDecoder(resp.Body).Decode(&auditResp)
	resp.Body.Close()
	if auditResp.Total < 1 {
		t.Errorf("expected at least 1 import.execute ok entry, got %d", auditResp.Total)
	}
	found := false
	for _, e := range auditResp.Entries {
		if e.Action == "import.execute" && e.Result == "ok" {
			found = true
			if !strings.Contains(e.Detail, "source="+sourceRoot) {
				t.Errorf("audit detail missing source: %s", e.Detail)
			}
			if !strings.Contains(e.Detail, "fs=ntfs") {
				t.Errorf("audit detail missing fs: %s", e.Detail)
			}
			if !strings.Contains(e.Detail, "copied=3") {
				t.Errorf("audit detail missing copied: %s", e.Detail)
			}
			if e.Actor != "admin" {
				t.Errorf("audit actor: got %s want admin", e.Actor)
			}
		}
	}
	if !found {
		t.Error("import.execute ok audit entry not found")
	}

	// 13. Cold-tier pool -> POST 409 with wake_required: true
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

	resp = doReq("POST", "/api/collections/"+coldCollID+"/import", map[string]string{
		"source_path": sourceRoot,
	})
	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("POST import cold tier: expected 409, got %d: %s", resp.StatusCode, string(b))
	} else {
		var coldResp struct {
			Error        string `json:"error"`
			WakeRequired bool   `json:"wake_required"`
		}
		json.NewDecoder(resp.Body).Decode(&coldResp)
		if !coldResp.WakeRequired {
			t.Errorf("expected wake_required=true, got false")
		}
	}
	resp.Body.Close()

	// 14. Repeat the same import -> copied=3 again; GET /imports total=2, newest first
	resp = doReq("POST", "/api/collections/"+collID+"/import", map[string]string{
		"source_path": sourceRoot,
		"fs_hint":     "NTFS",
	})
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST import repeat: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var importResp2 struct {
		Import struct {
			ID          string `json:"id"`
			Copied      int    `json:"copied"`
			Failed      int    `json:"failed"`
			BytesCopied int64  `json:"bytes_copied"`
		} `json:"import"`
	}
	json.NewDecoder(resp.Body).Decode(&importResp2)
	resp.Body.Close()
	if importResp2.Import.Copied != 3 {
		t.Errorf("repeat import copied: got %d want 3", importResp2.Import.Copied)
	}
	if importResp2.Import.Failed != 0 {
		t.Errorf("repeat import failed: got %d want 0", importResp2.Import.Failed)
	}
	if importResp2.Import.BytesCopied != 19 {
		t.Errorf("repeat import bytes_copied: got %d want 19", importResp2.Import.BytesCopied)
	}

	resp = doReq("GET", "/api/collections/"+collID+"/imports", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET imports after repeat: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	json.NewDecoder(resp.Body).Decode(&listResp)
	resp.Body.Close()
	if listResp.Total != 2 {
		t.Errorf("imports total after repeat: got %d want 2", listResp.Total)
	}
	if len(listResp.Imports) != 2 {
		t.Errorf("imports len after repeat: got %d want 2", len(listResp.Imports))
	}
	// Newest first: second import should be first
	if listResp.Imports[0].ID != importResp2.Import.ID {
		t.Errorf("newest first: first ID %s want %s", listResp.Imports[0].ID, importResp2.Import.ID)
	}
	if listResp.Imports[1].ID != importResp.Import.ID {
		t.Errorf("newest first: second ID %s want %s", listResp.Imports[1].ID, importResp.Import.ID)
	}

	// 15. Absolute path outside allowed root -> 400, nothing copied.
	// (Sentinel temp dir stands in for any system path: the check is purely
	// path-based, so no system directory is ever used as a test source.)
	outsideDir := t.TempDir()
	resp = doReq("POST", "/api/collections/"+collID+"/import", map[string]string{
		"source_path": outsideDir,
	})
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("POST import outside root: expected 400, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 16. Dot-dot escape resolving outside allowed root -> 400.
	escapePath := filepath.Join(sourceRoot, "..", "..", "escape")
	resp = doReq("POST", "/api/collections/"+collID+"/import", map[string]string{
		"source_path": escapePath,
	})
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("POST import dot-dot escape: expected 400, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 17. Symlink whose target resolves outside allowed root -> 400.
	linkPath := filepath.Join(tmpDir, "escape-link")
	if err := os.Symlink(outsideDir, linkPath); err != nil {
		t.Logf("symlink creation unsupported, skipping escape-link case: %v", err)
	} else {
		resp = doReq("POST", "/api/collections/"+collID+"/import", map[string]string{
			"source_path": linkPath,
		})
		if resp.StatusCode != http.StatusBadRequest {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("POST import symlink escape: expected 400, got %d: %s", resp.StatusCode, string(b))
		}
		resp.Body.Close()
	}

	// 18. Self-import (source == collection root) -> 400.
	resp = doReq("POST", "/api/collections/"+collID+"/import", map[string]string{
		"source_path": destRoot,
	})
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("POST import self-overlap: expected 400, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 19. Rejections left imports total unchanged (still 2) and an audit error row.
	resp = doReq("GET", "/api/collections/"+collID+"/imports", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET imports after rejections: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	json.NewDecoder(resp.Body).Decode(&listResp)
	resp.Body.Close()
	if listResp.Total != 2 {
		t.Errorf("imports total after rejections: got %d want 2", listResp.Total)
	}

	resp = doReq("GET", "/api/audit-log?action=import.execute&result=error", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET audit-log error: expected 200, got %d", resp.StatusCode)
	}
	var auditErrResp struct {
		Total int `json:"total"`
	}
	json.NewDecoder(resp.Body).Decode(&auditErrResp)
	resp.Body.Close()
	if auditErrResp.Total < 1 {
		t.Errorf("expected at least 1 import.execute error entry, got %d", auditErrResp.Total)
	}
}
