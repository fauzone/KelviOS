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

// setupArrangeServer creates a test server with one warm pool and one
// collection ("photos") whose root is tmpDir/mnt/warm/photos.
// Returns ts, token, collID, collRoot.
func setupArrangeServer(t *testing.T, db *sql.DB) (*httptest.Server, string, string, string) {
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
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-ARRANGE-WARM", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
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
	collRoot := filepath.Join(warmMount, "photos")
	if err := os.MkdirAll(collRoot, 0o755); err != nil {
		t.Fatalf("mkdir coll root: %v", err)
	}

	resp = doReq("POST", "/api/pools", map[string]any{"name": "warm", "tier": "warm", "mechanism": "single", "disks": []string{"sda"}, "mountpoint": warmMount}, token)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST pool: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var pr struct {
		Pool struct {
			ID string `json:"id"`
		} `json:"pool"`
	}
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()

	resp = doReq("POST", "/api/pools/"+pr.Pool.ID+"/collections", map[string]any{"name": "photos", "path": "photos", "disk_name": "sda"}, token)
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

	return ts, token, collResp.Collection.ID, collRoot
}

func arrangeDo(t *testing.T, ts *httptest.Server, method, path string, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if b, _ := io.ReadAll(resp.Body); len(b) > 0 {
		_ = json.Unmarshal(b, &out)
	}
	return resp.StatusCode, out
}

func writeArrangeHTTPFile(t *testing.T, collRoot, rel, content string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(collRoot, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatalf("chtime %s: %v", rel, err)
		}
	}
}

func TestArrangeGenerate201(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID, collRoot := setupArrangeServer(t, db)
	defer ts.Close()
	mtime := time.Date(2023, 3, 4, 12, 0, 0, 0, time.UTC)
	writeArrangeHTTPFile(t, collRoot, "a.png", "png-a", mtime)
	writeArrangeHTTPFile(t, collRoot, "b.png", "png-b", mtime)

	code, out := arrangeDo(t, ts, "POST", "/api/collections/"+collID+"/arrange/plans", token)
	if code != http.StatusCreated {
		t.Fatalf("generate: expected 201, got %d: %v", code, out)
	}
	plan, _ := out["plan"].(map[string]any)
	if plan == nil {
		t.Fatalf("generate response has no plan: %v", out)
	}
	if plan["status"] != "pending" {
		t.Errorf("plan status: got %v, want pending", plan["status"])
	}
	if plan["move_count"] != float64(2) {
		t.Errorf("move_count: got %v, want 2", plan["move_count"])
	}
	if out["conflict_count"] != float64(0) {
		t.Errorf("conflict_count: got %v, want 0", out["conflict_count"])
	}
}

func TestArrangeListAndGet(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID, collRoot := setupArrangeServer(t, db)
	defer ts.Close()
	writeArrangeHTTPFile(t, collRoot, "a.png", "png-a", time.Time{})

	code, gen := arrangeDo(t, ts, "POST", "/api/collections/"+collID+"/arrange/plans", token)
	if code != http.StatusCreated {
		t.Fatalf("generate: expected 201, got %d", code)
	}
	planID := gen["plan"].(map[string]any)["id"].(string)

	code, list := arrangeDo(t, ts, "GET", "/api/collections/"+collID+"/arrange/plans", token)
	if code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", code)
	}
	if list["total"] != float64(1) {
		t.Errorf("list total: got %v, want 1", list["total"])
	}

	code, got := arrangeDo(t, ts, "GET", "/api/collections/"+collID+"/arrange/plans/"+planID, token)
	if code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", code)
	}
	if got["plan"].(map[string]any)["id"] != planID {
		t.Errorf("get plan id mismatch: %v", got)
	}
	if _, ok := got["moves"]; !ok {
		t.Errorf("get response has no moves: %v", got)
	}
}

func TestArrangeApproveApplyUndo(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID, collRoot := setupArrangeServer(t, db)
	defer ts.Close()
	mtime := time.Date(2023, 3, 4, 12, 0, 0, 0, time.UTC)
	writeArrangeHTTPFile(t, collRoot, "a.png", "png-a", mtime)

	_, gen := arrangeDo(t, ts, "POST", "/api/collections/"+collID+"/arrange/plans", token)
	planID := gen["plan"].(map[string]any)["id"].(string)

	code, _ := arrangeDo(t, ts, "POST", "/api/collections/"+collID+"/arrange/plans/"+planID+"/approve", token)
	if code != http.StatusOK {
		t.Fatalf("approve: expected 200, got %d", code)
	}
	code, _ = arrangeDo(t, ts, "POST", "/api/collections/"+collID+"/arrange/plans/"+planID+"/apply", token)
	if code != http.StatusOK {
		t.Fatalf("apply: expected 200, got %d", code)
	}
	if _, err := os.Stat(filepath.Join(collRoot, "2023/2023-03-04/a.png")); err != nil {
		t.Errorf("a.png not at destination after apply: %v", err)
	}
	if _, err := os.Stat(filepath.Join(collRoot, "a.png")); !os.IsNotExist(err) {
		t.Errorf("a.png still at source after apply")
	}
	code, _ = arrangeDo(t, ts, "POST", "/api/collections/"+collID+"/arrange/plans/"+planID+"/undo", token)
	if code != http.StatusOK {
		t.Fatalf("undo: expected 200, got %d", code)
	}
	if _, err := os.Stat(filepath.Join(collRoot, "a.png")); err != nil {
		t.Errorf("a.png not back at source after undo: %v", err)
	}
}

func TestArrangeApplyPending409(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID, collRoot := setupArrangeServer(t, db)
	defer ts.Close()
	writeArrangeHTTPFile(t, collRoot, "a.png", "png-a", time.Time{})

	_, gen := arrangeDo(t, ts, "POST", "/api/collections/"+collID+"/arrange/plans", token)
	planID := gen["plan"].(map[string]any)["id"].(string)

	code, _ := arrangeDo(t, ts, "POST", "/api/collections/"+collID+"/arrange/plans/"+planID+"/apply", token)
	if code != http.StatusConflict {
		t.Fatalf("apply on pending: expected 409, got %d", code)
	}
}

func TestArrangeUnknown404(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID, collRoot := setupArrangeServer(t, db)
	defer ts.Close()
	writeArrangeHTTPFile(t, collRoot, "a.png", "png-a", time.Time{})

	code, _ := arrangeDo(t, ts, "POST", "/api/collections/"+collID+"/arrange/plans/no-such-plan/approve", token)
	if code != http.StatusNotFound {
		t.Fatalf("approve unknown: expected 404, got %d", code)
	}
	code, _ = arrangeDo(t, ts, "GET", "/api/collections/"+collID+"/arrange/plans/no-such-plan", token)
	if code != http.StatusNotFound {
		t.Fatalf("get unknown: expected 404, got %d", code)
	}
}

func TestArrangeUnauth401(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID, collRoot := setupArrangeServer(t, db)
	defer ts.Close()
	_ = token
	writeArrangeHTTPFile(t, collRoot, "a.png", "png-a", time.Time{})

	code, _ := arrangeDo(t, ts, "POST", "/api/collections/"+collID+"/arrange/plans", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("generate unauth: expected 401, got %d", code)
	}
}

func TestArrangeConflictBlocksApply(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ts, token, collID, collRoot := setupArrangeServer(t, db)
	defer ts.Close()
	mtime := time.Date(2023, 3, 4, 12, 0, 0, 0, time.UTC)
	// Pre-create the destination so the planned move conflicts (dest_exists).
	writeArrangeHTTPFile(t, collRoot, "2023/2023-03-04/exists.png", "already there", time.Time{})
	writeArrangeHTTPFile(t, collRoot, "exists.png", "source", mtime)

	code, gen := arrangeDo(t, ts, "POST", "/api/collections/"+collID+"/arrange/plans", token)
	if code != http.StatusCreated {
		t.Fatalf("generate: expected 201, got %d", code)
	}
	if gen["conflict_count"] != float64(1) {
		t.Fatalf("conflict_count: got %v, want 1", gen["conflict_count"])
	}
	planID := gen["plan"].(map[string]any)["id"].(string)

	code, _ = arrangeDo(t, ts, "POST", "/api/collections/"+collID+"/arrange/plans/"+planID+"/approve", token)
	if code != http.StatusOK {
		t.Fatalf("approve: expected 200, got %d", code)
	}
	code, _ = arrangeDo(t, ts, "POST", "/api/collections/"+collID+"/arrange/plans/"+planID+"/apply", token)
	if code != http.StatusConflict {
		t.Fatalf("apply on conflict plan: expected 409, got %d", code)
	}
	if _, err := os.Stat(filepath.Join(collRoot, "exists.png")); err != nil {
		t.Errorf("exists.png moved despite failed apply: %v", err)
	}
}
