package httpserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
)

// tagDoReq performs one HTTP call against ts and decodes the JSON body
// into a generic map (empty map when the body is empty, e.g. 204).
func tagDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) (int, map[string]any) {
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
	defer resp.Body.Close()
	out := map[string]any{}
	if b, _ := io.ReadAll(resp.Body); len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return resp.StatusCode, out
}

// setupTaggingFixture builds a warm pool + photos collection (via the
// shared arrange harness), writes a.jpg/b.jpg with the same mtime, and
// scans. Returns ts, token, collID, collRoot.
func setupTaggingFixture(t *testing.T, db *sql.DB) (*httptest.Server, string, string, string) {
	t.Helper()
	ts, token, collID, collRoot := setupArrangeServer(t, db)
	mtime := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	writeArrangeHTTPFile(t, collRoot, "a.jpg", "jpeg-a", mtime)
	writeArrangeHTTPFile(t, collRoot, "b.jpg", "jpeg-b", mtime)
	code, out := tagDoReq(t, ts, "POST", "/api/collections/"+collID+"/scan", nil, token)
	if code != http.StatusOK {
		t.Fatalf("scan: expected 200, got %d: %v", code, out)
	}
	if out["added"] != float64(2) {
		t.Fatalf("scan added: got %v, want 2", out["added"])
	}
	return ts, token, collID, collRoot
}

// tagsFor fetches GET .../tags?rel_path= and returns the tag rows.
func tagsFor(t *testing.T, ts *httptest.Server, collID, rel, token string) []any {
	t.Helper()
	code, out := tagDoReq(t, ts, "GET", "/api/collections/"+collID+"/tags?rel_path="+rel, nil, token)
	if code != http.StatusOK {
		t.Fatalf("tags %s: expected 200, got %d: %v", rel, code, out)
	}
	rows, _ := out["tags"].([]any)
	return rows
}

func findTag(rows []any, want string) map[string]any {
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if m != nil && m["tag"] == want {
			return m
		}
	}
	return nil
}

func TestTagRun200AndPropagation(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID, _ := setupTaggingFixture(t, db)
	defer ts.Close()

	code, out := tagDoReq(t, ts, "POST", "/api/collections/"+collID+"/tags/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("run: expected 200, got %d: %v", code, out)
	}
	if out["clusters"] != float64(1) {
		t.Errorf("clusters: got %v, want 1", out["clusters"])
	}
	if out["tags_created"] != float64(2) {
		t.Errorf("tags_created: got %v, want 2 (sample month + propagated month)", out["tags_created"])
	}
	if out["propagated"] != float64(1) {
		t.Errorf("propagated: got %v, want 1", out["propagated"])
	}
	if out["sidecars_written"] != float64(2) {
		t.Errorf("sidecars_written: got %v, want 2", out["sidecars_written"])
	}

	// Propagated tag on the non-sample file: confidence 0.9 * 0.8 = 0.72.
	bTags := tagsFor(t, ts, collID, "b.jpg", token)
	prop := findTag(bTags, "month:2024-06")
	if prop == nil {
		t.Fatalf("b.jpg has no month:2024-06 tag: %v", bTags)
	}
	if conf, _ := prop["confidence"].(float64); math.Abs(conf-0.72) > 1e-9 {
		t.Errorf("b.jpg confidence: got %v, want 0.72", conf)
	}
	if prop["origin"] != "rule" || prop["source"] != "cluster" {
		t.Errorf("b.jpg tag origin/source: got %v/%v, want rule/cluster", prop["origin"], prop["source"])
	}

	// Sample tag: origin rule, source sample, confidence 0.9.
	aTags := tagsFor(t, ts, collID, "a.jpg", token)
	sample := findTag(aTags, "month:2024-06")
	if sample == nil {
		t.Fatalf("a.jpg has no month:2024-06 tag: %v", aTags)
	}
	if conf, _ := sample["confidence"].(float64); math.Abs(conf-0.9) > 1e-9 {
		t.Errorf("a.jpg confidence: got %v, want 0.9", conf)
	}
	if sample["origin"] != "rule" || sample["source"] != "sample" {
		t.Errorf("a.jpg tag origin/source: got %v/%v, want rule/sample", sample["origin"], sample["source"])
	}
}

func TestTagRunSecondIdempotent(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID, _ := setupTaggingFixture(t, db)
	defer ts.Close()

	code, _ := tagDoReq(t, ts, "POST", "/api/collections/"+collID+"/tags/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("run 1: expected 200, got %d", code)
	}
	code, out := tagDoReq(t, ts, "POST", "/api/collections/"+collID+"/tags/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("run 2: expected 200, got %d: %v", code, out)
	}
	if out["tags_created"] != float64(0) {
		t.Errorf("run 2 tags_created: got %v, want 0", out["tags_created"])
	}
	if out["propagated"] != float64(0) {
		t.Errorf("run 2 propagated: got %v, want 0", out["propagated"])
	}
}

func TestTagRunCold409(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, _, _ := setupArrangeServer(t, db)
	defer ts.Close()

	// Cold-tier pool + collection (same pattern as files_test.go §12).
	tmpDir := t.TempDir()
	coldMount := filepath.Join(tmpDir, "mnt", "cold")
	if err := os.MkdirAll(filepath.Join(coldMount, "photos"), 0o755); err != nil {
		t.Fatalf("mkdir cold root: %v", err)
	}
	code, out := tagDoReq(t, ts, "POST", "/api/pools", map[string]any{"name": "coldpool", "tier": "cold", "mechanism": "single", "disks": []string{"sda"}, "mountpoint": coldMount}, token)
	if code != http.StatusCreated {
		t.Fatalf("cold pool: expected 201, got %d: %v", code, out)
	}
	pool, _ := out["pool"].(map[string]any)
	poolID, _ := pool["id"].(string)
	code, out = tagDoReq(t, ts, "POST", "/api/pools/"+poolID+"/collections", map[string]any{"name": "coldphotos", "path": "photos", "disk_name": "sda"}, token)
	if code != http.StatusCreated {
		t.Fatalf("cold collection: expected 201, got %d: %v", code, out)
	}
	coll, _ := out["collection"].(map[string]any)
	collID, _ := coll["id"].(string)

	code, out = tagDoReq(t, ts, "POST", "/api/collections/"+collID+"/tags/run", nil, token)
	if code != http.StatusConflict {
		t.Fatalf("cold run: expected 409, got %d: %v", code, out)
	}
	if out["wake_required"] != true {
		t.Errorf("cold run wake_required: got %v, want true", out["wake_required"])
	}
}

func TestTagSearchAndManualCRUD(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID, _ := setupTaggingFixture(t, db)
	defer ts.Close()

	code, _ := tagDoReq(t, ts, "POST", "/api/collections/"+collID+"/tags/run", nil, token)
	if code != http.StatusOK {
		t.Fatalf("run: expected 200, got %d", code)
	}

	// Search by tag returns both files.
	code, out := tagDoReq(t, ts, "GET", "/api/collections/"+collID+"/tags/search?tag=month:2024-06", nil, token)
	if code != http.StatusOK {
		t.Fatalf("search: expected 200, got %d: %v", code, out)
	}
	files, _ := out["files"].([]any)
	if len(files) != 2 || out["total"] != float64(2) {
		t.Errorf("search files/total: got %v/%v, want [a.jpg b.jpg]/2", files, out["total"])
	}

	// Manual create: 201, origin manual, confidence 1.0.
	code, out = tagDoReq(t, ts, "POST", "/api/collections/"+collID+"/tags", map[string]any{"rel_path": "a.jpg", "tag": "manual:hero"}, token)
	if code != http.StatusCreated {
		t.Fatalf("manual create: expected 201, got %d: %v", code, out)
	}
	// Duplicate manual tag -> 409.
	code, out = tagDoReq(t, ts, "POST", "/api/collections/"+collID+"/tags", map[string]any{"rel_path": "a.jpg", "tag": "manual:hero"}, token)
	if code != http.StatusConflict {
		t.Errorf("manual duplicate: expected 409, got %d: %v", code, out)
	}
	// Manual tag on missing file -> 404.
	code, out = tagDoReq(t, ts, "POST", "/api/collections/"+collID+"/tags", map[string]any{"rel_path": "nope.jpg", "tag": "manual:x"}, token)
	if code != http.StatusNotFound {
		t.Errorf("manual missing file: expected 404, got %d: %v", code, out)
	}
	// Manual tag visible via list with the right origin/confidence.
	aTags := tagsFor(t, ts, collID, "a.jpg", token)
	manual := findTag(aTags, "manual:hero")
	if manual == nil {
		t.Fatalf("a.jpg has no manual:hero tag: %v", aTags)
	}
	if manual["origin"] != "manual" {
		t.Errorf("manual origin: got %v, want manual", manual["origin"])
	}
	if conf, _ := manual["confidence"].(float64); math.Abs(conf-1.0) > 1e-9 {
		t.Errorf("manual confidence: got %v, want 1.0", conf)
	}

	// Delete: 204, then missing -> 404.
	code, _ = tagDoReq(t, ts, "DELETE", "/api/collections/"+collID+"/tags?rel_path=a.jpg&tag=manual:hero", nil, token)
	if code != http.StatusNoContent {
		t.Errorf("manual delete: expected 204, got %d", code)
	}
	code, out = tagDoReq(t, ts, "DELETE", "/api/collections/"+collID+"/tags?rel_path=a.jpg&tag=manual:hero", nil, token)
	if code != http.StatusNotFound {
		t.Errorf("manual delete missing: expected 404, got %d: %v", code, out)
	}
}

func TestTagUnauth401AndUnknown404(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID, _ := setupTaggingFixture(t, db)
	defer ts.Close()

	code, out := tagDoReq(t, ts, "GET", "/api/collections/"+collID+"/tags?rel_path=a.jpg", nil, "")
	if code != http.StatusUnauthorized {
		t.Errorf("unauth tags: expected 401, got %d: %v", code, out)
	}
	code, out = tagDoReq(t, ts, "POST", "/api/collections/coll-unknown/tags/run", nil, token)
	if code != http.StatusNotFound {
		t.Errorf("unknown run: expected 404, got %d: %v", code, out)
	}
	code, out = tagDoReq(t, ts, "GET", "/api/collections/coll-unknown/tags?rel_path=a.jpg", nil, token)
	if code != http.StatusNotFound {
		t.Errorf("unknown tags: expected 404, got %d: %v", code, out)
	}
}
