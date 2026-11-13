package httpserver

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
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

func TestPreviewCollectionFile(t *testing.T) {
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

	// Build fixture tree under a temp dir
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "mnt", "media", "photos")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	// Create a 1x1 PNG file
	pngPath := filepath.Join(root, "test.png")
	pngFile, err := os.Create(pngPath)
	if err != nil {
		t.Fatalf("create png: %v", err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	if err := png.Encode(pngFile, img); err != nil {
		pngFile.Close()
		t.Fatalf("encode png: %v", err)
	}
	pngFile.Close()

	// Create a dummy mp3 file (just bytes, not a real mp3)
	mp3Path := filepath.Join(root, "audio.mp3")
	if err := os.WriteFile(mp3Path, []byte("dummy mp3 content"), 0o644); err != nil {
		t.Fatalf("write mp3: %v", err)
	}

	// Create a file with unknown extension
	unknownPath := filepath.Join(root, "data.xyz")
	if err := os.WriteFile(unknownPath, []byte("unknown"), 0o644); err != nil {
		t.Fatalf("write unknown: %v", err)
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

	// 1. Preview PNG image -> 200, kind image, width 1, height 1
	resp = doReq("GET", "/api/collections/"+collID+"/files/preview?path=test.png", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET preview test.png: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	var previewResp struct {
		CollectionID     string `json:"collection_id"`
		RelPath          string `json:"rel_path"`
		Kind             string `json:"kind"`
		Mime             string `json:"mime"`
		SizeBytes        int64  `json:"size_bytes"`
		Width            any    `json:"width"`
		Height           any    `json:"height"`
		DurationSeconds  any    `json:"duration_seconds"`
		PreviewAvailable bool   `json:"preview_available"`
	}
	json.NewDecoder(resp.Body).Decode(&previewResp)
	resp.Body.Close()
	if previewResp.Kind != "image" {
		t.Errorf("kind: got %s, want image", previewResp.Kind)
	}
	if previewResp.Mime != "image/png" {
		t.Errorf("mime: got %s, want image/png", previewResp.Mime)
	}
	if previewResp.Width != float64(1) {
		t.Errorf("width: got %v, want 1", previewResp.Width)
	}
	if previewResp.Height != float64(1) {
		t.Errorf("height: got %v, want 1", previewResp.Height)
	}
	if previewResp.DurationSeconds != nil {
		t.Errorf("duration_seconds: got %v, want nil", previewResp.DurationSeconds)
	}
	if !previewResp.PreviewAvailable {
		t.Error("preview_available should be true for image")
	}
	if previewResp.SizeBytes <= 0 {
		t.Errorf("size_bytes should be > 0, got %d", previewResp.SizeBytes)
	}

	// 2. Preview MP3 audio -> 200, kind audio, duration_seconds null, preview_available true
	resp = doReq("GET", "/api/collections/"+collID+"/files/preview?path=audio.mp3", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET preview audio.mp3: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	json.NewDecoder(resp.Body).Decode(&previewResp)
	resp.Body.Close()
	if previewResp.Kind != "audio" {
		t.Errorf("kind: got %s, want audio", previewResp.Kind)
	}
	if previewResp.Mime != "audio/mpeg" {
		t.Errorf("mime: got %s, want audio/mpeg", previewResp.Mime)
	}
	if previewResp.Width != nil {
		t.Errorf("width: got %v, want nil for audio", previewResp.Width)
	}
	if previewResp.Height != nil {
		t.Errorf("height: got %v, want nil for audio", previewResp.Height)
	}
	if previewResp.DurationSeconds != nil {
		t.Errorf("duration_seconds: got %v, want nil", previewResp.DurationSeconds)
	}
	if !previewResp.PreviewAvailable {
		t.Error("preview_available should be true for audio")
	}

	// 3. Preview unknown extension -> 200, kind other, preview_available false
	resp = doReq("GET", "/api/collections/"+collID+"/files/preview?path=data.xyz", nil)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET preview data.xyz: expected 200, got %d: %s", resp.StatusCode, string(b))
	}
	json.NewDecoder(resp.Body).Decode(&previewResp)
	resp.Body.Close()
	if previewResp.Kind != "other" {
		t.Errorf("kind: got %s, want other", previewResp.Kind)
	}
	if previewResp.Mime != "application/octet-stream" {
		t.Errorf("mime: got %s, want application/octet-stream", previewResp.Mime)
	}
	if previewResp.PreviewAvailable {
		t.Error("preview_available should be false for other")
	}

	// 4. Traversal (../x) -> 400
	resp = doReq("GET", "/api/collections/"+collID+"/files/preview?path=../etc/passwd", nil)
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET preview traversal: expected 400, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 5. Unauthenticated -> 401
	resp, err = http.Get(ts.URL + "/api/collections/" + collID + "/files/preview?path=test.png")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET preview unauthenticated: expected 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 6. Unindexed file -> 404
	resp = doReq("GET", "/api/collections/"+collID+"/files/preview?path=notindexed.png", nil)
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET preview unindexed: expected 404, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 7. Directory path -> 400
	if err := os.MkdirAll(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	resp = doReq("GET", "/api/collections/"+collID+"/files/preview?path=subdir", nil)
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET preview directory: expected 400, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 8. Cold pool -> 409 with wake_required
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

	resp = doReq("GET", "/api/collections/"+coldCollID+"/files/preview?path=test.png", nil)
	if resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET preview cold tier: expected 409, got %d: %s", resp.StatusCode, string(b))
	} else {
		var coldResp struct {
			Error        string `json:"error"`
			WakeRequired bool   `json:"wake_required"`
		}
		json.NewDecoder(resp.Body).Decode(&coldResp)
		if !coldResp.WakeRequired {
			t.Error("wake_required should be true for cold tier")
		}
		if !strings.Contains(coldResp.Error, "cold") {
			t.Errorf("error should mention cold tier, got: %s", coldResp.Error)
		}
	}
	resp.Body.Close()

	// 9. Empty path -> 400
	resp = doReq("GET", "/api/collections/"+collID+"/files/preview?path=", nil)
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET preview empty path: expected 400, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 10. Absolute path -> 400
	resp = doReq("GET", "/api/collections/"+collID+"/files/preview?path=/etc/passwd", nil)
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET preview absolute path: expected 400, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 11. Unknown collection -> 404
	resp = doReq("GET", "/api/collections/coll-nonexistent/files/preview?path=test.png", nil)
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("GET preview unknown collection: expected 404, got %d: %s", resp.StatusCode, string(b))
	}
	resp.Body.Close()

	// 12. Audit log: verify file.preview row with rel_path and no content leak
	resp = doReq("GET", "/api/audit-log?action=file.preview&result=ok", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET audit-log: expected 200, got %d", resp.StatusCode)
	}
	var auditResp struct {
		Entries []struct {
			Action string `json:"action"`
			Detail string `json:"detail"`
			Result string `json:"result"`
		} `json:"entries"`
		Total int `json:"total"`
	}
	json.NewDecoder(resp.Body).Decode(&auditResp)
	resp.Body.Close()
	if auditResp.Total < 3 {
		t.Errorf("expected at least 3 file.preview ok entries (png, mp3, xyz), got %d", auditResp.Total)
	}
	for _, e := range auditResp.Entries {
		if e.Action != "file.preview" || e.Result != "ok" {
			t.Errorf("unexpected audit entry: %+v", e)
		}
		// Detail should be rel_path only, no file content
		if strings.Contains(e.Detail, "dummy") || strings.Contains(e.Detail, "unknown") {
			t.Errorf("audit detail leaks file content: %s", e.Detail)
		}
	}
}
