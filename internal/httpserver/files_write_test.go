package httpserver

// KelviOS Fase 1 WS15: File Manager write operations tests (FR-1.13) —
// upload / mkdir / move / copy / delete on top of the WS13 browse and
// WS14 download seams. Written by Chief under takeover (Kapten 2026-10-29
// 09:15, auto-takeover bila worth it) after three Saka attempts failed to
// land a single test edit. Production code is the merged takeover code:
// upload checks disk existence, move/copy require an indexed (catalog)
// source row, delete removes disk first then the catalog row.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

type ws15Fixture struct {
	s      *Server
	ts     *httptest.Server
	db     *sql.DB
	token  string
	tmpDir string
}

func newWS15Fixture(t *testing.T, dbPath string) *ws15Fixture {
	t.Helper()
	db, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		db.Close()
		t.Fatalf("migrate: %v", err)
	}
	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{Path: dbPath},
	}
	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	s := New(cfg, db, "test-version")
	s.SetDriver(storage.NewSingleDriver(disks))
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	ts := httptest.NewServer(s.srv.Handler)
	f := &ws15Fixture{s: s, ts: ts, db: db, tmpDir: t.TempDir()}
	f.token = mustToken(t, ts)
	return f
}

func (f *ws15Fixture) close() {
	f.ts.Close()
	_ = f.s.Shutdown(context.Background())
	_ = f.db.Close()
}

// setupCollection creates a pool+collection via the API and points the
// pool mountpoint at a fresh temp dir. Returns (collectionID, root).
func (f *ws15Fixture) setupCollection(t *testing.T, tier, name, collPath string) (string, string) {
	t.Helper()
	st, body := postJSON(t, f.ts.URL+"/api/pools", map[string]any{
		"name":       "pool-" + name,
		"tier":       tier,
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": "/mnt/placeholder-" + name,
	}, f.token)
	if st != http.StatusCreated {
		t.Fatalf("POST pool %s: got %d, want 201: %s", name, st, body)
	}
	var poolResp struct {
		Pool struct {
			ID string `json:"id"`
		} `json:"pool"`
	}
	if err := json.Unmarshal(body, &poolResp); err != nil {
		t.Fatalf("decode pool: %v", err)
	}
	poolID := poolResp.Pool.ID

	st, body = postJSON(t, f.ts.URL+"/api/pools/"+poolID+"/collections", map[string]any{
		"name":      name,
		"path":      collPath,
		"disk_name": "sda",
	}, f.token)
	if st != http.StatusCreated {
		t.Fatalf("POST collection %s: got %d, want 201: %s", name, st, body)
	}
	var collResp struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	if err := json.Unmarshal(body, &collResp); err != nil {
		t.Fatalf("decode collection: %v", err)
	}

	mountpoint := filepath.Join(f.tmpDir, "mnt", name)
	root := filepath.Join(mountpoint, collPath)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, mountpoint, poolID); err != nil {
		t.Fatalf("update pool mountpoint: %v", err)
	}
	return collResp.Collection.ID, root
}

func ws15Raw(t *testing.T, ts *httptest.Server, token, method, rawURL string, body []byte, contentType string) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rdr)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, rawURL, err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, rawURL, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

func (f *ws15Fixture) upload(t *testing.T, collID, rel, content string, overwrite bool) (int, []byte) {
	t.Helper()
	u := f.ts.URL + "/api/collections/" + collID + "/files/content?path=" + url.QueryEscape(rel)
	if overwrite {
		u += "&overwrite=true"
	}
	return ws15Raw(t, f.ts, f.token, http.MethodPost, u, []byte(content), "application/octet-stream")
}

func (f *ws15Fixture) download(t *testing.T, collID, rel string) (int, []byte) {
	t.Helper()
	u := f.ts.URL + "/api/collections/" + collID + "/files/content?path=" + url.QueryEscape(rel)
	return ws15Raw(t, f.ts, f.token, http.MethodGet, u, nil, "")
}

func (f *ws15Fixture) delete(t *testing.T, collID, rawQuery string) (int, []byte) {
	t.Helper()
	u := f.ts.URL + "/api/collections/" + collID + "/files/content?" + rawQuery
	return ws15Raw(t, f.ts, f.token, http.MethodDelete, u, nil, "")
}

type ws15Entry struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	RelPath   string `json:"rel_path"`
	SizeBytes int64  `json:"size_bytes"`
	Offline   bool   `json:"offline"`
}

func (f *ws15Fixture) browse(t *testing.T, collID, prefix string) []ws15Entry {
	t.Helper()
	u := f.ts.URL + "/api/collections/" + collID + "/files"
	if prefix != "" {
		u += "?path=" + url.QueryEscape(prefix)
	}
	st, body := getWithToken(t, u, f.token)
	if st != http.StatusOK {
		t.Fatalf("GET browse prefix %q: got %d, want 200: %s", prefix, st, body)
	}
	var br struct {
		Entries []ws15Entry `json:"entries"`
		Tier    string      `json:"tier"`
	}
	if err := json.Unmarshal(body, &br); err != nil {
		t.Fatalf("decode browse: %v", err)
	}
	if br.Entries == nil {
		return []ws15Entry{}
	}
	return br.Entries
}

func ws15Find(entries []ws15Entry, name string) *ws15Entry {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}

func ws15WakeRequired(t *testing.T, body []byte) bool {
	t.Helper()
	var er struct {
		WakeRequired bool `json:"wake_required"`
	}
	if err := json.Unmarshal(body, &er); err != nil {
		t.Fatalf("decode wake_required body: %v (%s)", err, body)
	}
	return er.WakeRequired
}

func TestWriteUploadAndOverwrite(t *testing.T) {
	f := newWS15Fixture(t, ":memory:")
	defer f.close()
	collID, _ := f.setupCollection(t, "warm", "photos", "photos")

	// Upload new file -> 201.
	st, body := f.upload(t, collID, "hello.txt", "hello-ws15", false)
	if st != http.StatusCreated {
		t.Fatalf("upload hello.txt: got %d, want 201: %s", st, body)
	}

	// Browse lists it as a file.
	entries := f.browse(t, collID, "")
	e := ws15Find(entries, "hello.txt")
	if e == nil {
		t.Fatalf("browse root after upload: hello.txt missing in %+v", entries)
	}
	if e.Kind != "file" {
		t.Errorf("hello.txt kind: got %q, want file", e.Kind)
	}

	// Download byte-identical.
	st, body = f.download(t, collID, "hello.txt")
	if st != http.StatusOK || string(body) != "hello-ws15" {
		t.Fatalf("download hello.txt: got %d %q, want 200 %q", st, body, "hello-ws15")
	}

	// Re-upload without overwrite -> 409, content unchanged.
	st, body = f.upload(t, collID, "hello.txt", "second-version", false)
	if st != http.StatusConflict {
		t.Fatalf("re-upload without overwrite: got %d, want 409: %s", st, body)
	}
	_, body = f.download(t, collID, "hello.txt")
	if string(body) != "hello-ws15" {
		t.Fatalf("download after 409: got %q, want original %q", body, "hello-ws15")
	}

	// Overwrite=true -> 200, new content downloads.
	st, body = f.upload(t, collID, "hello.txt", "second-version", true)
	if st != http.StatusOK {
		t.Fatalf("overwrite upload: got %d, want 200: %s", st, body)
	}
	_, body = f.download(t, collID, "hello.txt")
	if string(body) != "second-version" {
		t.Fatalf("download after overwrite: got %q, want %q", body, "second-version")
	}

	// Catalog row reflects the new size.
	row, err := catalog.GetFile(f.db, collID, "hello.txt")
	if err != nil {
		t.Fatalf("GetFile hello.txt: %v", err)
	}
	if row == nil {
		t.Fatalf("GetFile hello.txt: row missing")
	}
	if row.SizeBytes != int64(len("second-version")) {
		t.Errorf("catalog size: got %d, want %d", row.SizeBytes, len("second-version"))
	}

	// Audit: file.upload ok rows exist and never leak file content.
	var okCount int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'file.upload' AND result = 'ok'`).Scan(&okCount); err != nil {
		t.Fatalf("count audit file.upload: %v", err)
	}
	if okCount < 2 {
		t.Errorf("audit file.upload ok rows: got %d, want >= 2", okCount)
	}
	var leakCount int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = 'file.upload' AND (detail LIKE '%hello-ws15%' OR detail LIKE '%second-version%')`).Scan(&leakCount); err != nil {
		t.Fatalf("count audit leak: %v", err)
	}
	if leakCount != 0 {
		t.Errorf("audit detail leaks file content in %d rows", leakCount)
	}
}

func TestWriteMkdir(t *testing.T) {
	f := newWS15Fixture(t, ":memory:")
	defer f.close()
	collID, root := f.setupCollection(t, "warm", "photos", "photos")

	// Mkdir -> 201, directory exists on disk.
	st, body := postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/mkdir?path=newdir", nil, f.token)
	if st != http.StatusCreated {
		t.Fatalf("mkdir newdir: got %d, want 201: %s", st, body)
	}
	if fi, err := os.Stat(filepath.Join(root, "newdir")); err != nil || !fi.IsDir() {
		t.Fatalf("newdir on disk: err=%v", err)
	}

	// Browse shows the directory.
	entries := f.browse(t, collID, "")
	e := ws15Find(entries, "newdir")
	if e == nil {
		t.Fatalf("browse root after mkdir: newdir missing in %+v", entries)
	}
	if e.Kind != "dir" {
		t.Errorf("newdir kind: got %q, want dir", e.Kind)
	}

	// Mkdir again -> 409.
	st, body = postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/mkdir?path=newdir", nil, f.token)
	if st != http.StatusConflict {
		t.Fatalf("mkdir existing: got %d, want 409: %s", st, body)
	}

	// Mkdir with missing parent -> 400.
	st, body = postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/mkdir?path=missing/child", nil, f.token)
	if st != http.StatusBadRequest {
		t.Fatalf("mkdir missing parent: got %d, want 400: %s", st, body)
	}
	if _, err := os.Stat(filepath.Join(root, "missing")); !os.IsNotExist(err) {
		t.Errorf("missing parent dir should not exist on disk, stat err=%v", err)
	}

	// Mkdir inside the new directory works (parent exists now).
	st, body = postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/mkdir?path=newdir/inner", nil, f.token)
	if st != http.StatusCreated {
		t.Fatalf("mkdir newdir/inner: got %d, want 201: %s", st, body)
	}
	if !strings.Contains(string(body), "newdir") {
		t.Errorf("mkdir response should echo path, got %s", body)
	}

	// Browse inside newdir shows the nested empty directory.
	inner := f.browse(t, collID, "newdir")
	ie := ws15Find(inner, "inner")
	if ie == nil {
		t.Fatalf("browse newdir: inner missing in %+v", inner)
	}
	if ie.Kind != "dir" {
		t.Errorf("inner kind: got %q, want dir", ie.Kind)
	}
}

func TestWriteMove(t *testing.T) {
	f := newWS15Fixture(t, ":memory:")
	defer f.close()
	collID, _ := f.setupCollection(t, "warm", "photos", "photos")

	if st, body := f.upload(t, collID, "a.txt", "move-me", false); st != http.StatusCreated {
		t.Fatalf("upload a.txt: got %d: %s", st, body)
	}
	if st, body := f.upload(t, collID, "exists.txt", "stay-here", false); st != http.StatusCreated {
		t.Fatalf("upload exists.txt: got %d: %s", st, body)
	}

	move := func(from, to string) (int, []byte) {
		return postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/move",
			map[string]string{"from": from, "to": to}, f.token)
	}

	// Move onto an existing target -> 409.
	if st, body := move("a.txt", "exists.txt"); st != http.StatusConflict {
		t.Fatalf("move onto existing: got %d, want 409: %s", st, body)
	}
	// Move a missing source -> 404.
	if st, body := move("missing.txt", "x.txt"); st != http.StatusNotFound {
		t.Fatalf("move missing source: got %d, want 404: %s", st, body)
	}
	// Move -> 200.
	if st, body := move("a.txt", "moved.txt"); st != http.StatusOK {
		t.Fatalf("move a.txt->moved.txt: got %d, want 200: %s", st, body)
	}

	// Source downloads 404, destination byte-identical.
	if st, _ := f.download(t, collID, "a.txt"); st != http.StatusNotFound {
		t.Errorf("download a.txt after move: got %d, want 404", st)
	}
	st, body := f.download(t, collID, "moved.txt")
	if st != http.StatusOK || string(body) != "move-me" {
		t.Errorf("download moved.txt: got %d %q, want 200 %q", st, body, "move-me")
	}

	// Catalog row moved.
	if row, err := catalog.GetFile(f.db, collID, "a.txt"); err != nil || row != nil {
		t.Errorf("catalog a.txt after move: row=%v err=%v, want nil row", row, err)
	}
	if row, err := catalog.GetFile(f.db, collID, "moved.txt"); err != nil || row == nil {
		t.Errorf("catalog moved.txt after move: row=%v err=%v, want a row", row, err)
	}

	// Browse agrees.
	entries := f.browse(t, collID, "")
	if ws15Find(entries, "a.txt") != nil {
		t.Errorf("browse still lists a.txt after move: %+v", entries)
	}
	if ws15Find(entries, "moved.txt") == nil {
		t.Errorf("browse missing moved.txt after move: %+v", entries)
	}
}

func TestWriteCopy(t *testing.T) {
	f := newWS15Fixture(t, ":memory:")
	defer f.close()
	collID, _ := f.setupCollection(t, "warm", "photos", "photos")

	if st, body := f.upload(t, collID, "src.txt", "copy-me", false); st != http.StatusCreated {
		t.Fatalf("upload src.txt: got %d: %s", st, body)
	}
	if st, body := postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/mkdir?path=somedir", nil, f.token); st != http.StatusCreated {
		t.Fatalf("mkdir somedir: got %d: %s", st, body)
	}

	copyOp := func(from, to string) (int, []byte) {
		return postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/copy",
			map[string]string{"from": from, "to": to}, f.token)
	}

	// Copy a directory -> 400 (files only).
	if st, body := copyOp("somedir", "dircopy"); st != http.StatusBadRequest {
		t.Fatalf("copy directory: got %d, want 400: %s", st, body)
	}
	// Copy a missing source -> 404.
	if st, body := copyOp("missing.txt", "x.txt"); st != http.StatusNotFound {
		t.Fatalf("copy missing source: got %d, want 404: %s", st, body)
	}
	// Copy -> 201.
	if st, body := copyOp("src.txt", "dst.txt"); st != http.StatusCreated {
		t.Fatalf("copy src.txt->dst.txt: got %d, want 201: %s", st, body)
	}
	// Copy onto existing -> 409.
	if st, body := copyOp("src.txt", "dst.txt"); st != http.StatusConflict {
		t.Fatalf("copy onto existing: got %d, want 409: %s", st, body)
	}

	// Source intact + copy byte-identical.
	st, body := f.download(t, collID, "src.txt")
	if st != http.StatusOK || string(body) != "copy-me" {
		t.Errorf("download src.txt: got %d %q, want 200 %q", st, body, "copy-me")
	}
	st, body = f.download(t, collID, "dst.txt")
	if st != http.StatusOK || string(body) != "copy-me" {
		t.Errorf("download dst.txt: got %d %q, want 200 %q", st, body, "copy-me")
	}

	// Two catalog rows.
	for _, rel := range []string{"src.txt", "dst.txt"} {
		row, err := catalog.GetFile(f.db, collID, rel)
		if err != nil || row == nil {
			t.Errorf("catalog row %s: row=%v err=%v, want a row", rel, row, err)
		}
	}
}

func TestWriteDelete(t *testing.T) {
	f := newWS15Fixture(t, ":memory:")
	defer f.close()
	collID, root := f.setupCollection(t, "warm", "photos", "photos")

	if st, body := f.upload(t, collID, "del.txt", "delete-me", false); st != http.StatusCreated {
		t.Fatalf("upload del.txt: got %d: %s", st, body)
	}

	// Delete -> 204.
	st, body := f.delete(t, collID, "path=del.txt")
	if st != http.StatusNoContent {
		t.Fatalf("delete del.txt: got %d, want 204: %s", st, body)
	}
	if _, err := os.Stat(filepath.Join(root, "del.txt")); !os.IsNotExist(err) {
		t.Errorf("del.txt should be gone from disk, stat err=%v", err)
	}

	// Download 404 + browse no longer lists it + catalog row gone.
	if st, _ := f.download(t, collID, "del.txt"); st != http.StatusNotFound {
		t.Errorf("download del.txt after delete: got %d, want 404", st)
	}
	if ws15Find(f.browse(t, collID, ""), "del.txt") != nil {
		t.Errorf("browse still lists del.txt after delete")
	}
	if row, err := catalog.GetFile(f.db, collID, "del.txt"); err != nil || row != nil {
		t.Errorf("catalog del.txt after delete: row=%v err=%v, want nil row", row, err)
	}

	// Delete again -> 404.
	if st, body := f.delete(t, collID, "path=del.txt"); st != http.StatusNotFound {
		t.Fatalf("delete again: got %d, want 404: %s", st, body)
	}

	// Directory: delete without dir=true -> 400; empty dir with dir=true -> 204.
	if st, body := postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/mkdir?path=emptydir", nil, f.token); st != http.StatusCreated {
		t.Fatalf("mkdir emptydir: got %d: %s", st, body)
	}
	if st, body := f.delete(t, collID, "path=emptydir"); st != http.StatusBadRequest {
		t.Fatalf("delete dir without dir=true: got %d, want 400: %s", st, body)
	}
	if st, body := f.delete(t, collID, "path=emptydir&dir=true"); st != http.StatusNoContent {
		t.Fatalf("delete empty dir with dir=true: got %d, want 204: %s", st, body)
	}
	if _, err := os.Stat(filepath.Join(root, "emptydir")); !os.IsNotExist(err) {
		t.Errorf("emptydir should be gone from disk, stat err=%v", err)
	}
}

func TestWriteNegativePaths(t *testing.T) {
	f := newWS15Fixture(t, ":memory:")
	defer f.close()
	collID, root := f.setupCollection(t, "warm", "photos", "photos")

	badQueries := map[string]string{
		"traversal": "path=../evil.txt",
		"encoded":   "path=%2e%2e%2fevil.txt",
		"backslash": "path=sub%5Cevil.txt",
		"absolute":  "path=%2Fetc%2Fpasswd",
		"empty":     "path=",
	}
	for label, qs := range badQueries {
		// Upload.
		st, body := ws15Raw(t, f.ts, f.token, http.MethodPost,
			f.ts.URL+"/api/collections/"+collID+"/files/content?"+qs, []byte("x"), "application/octet-stream")
		if st != http.StatusBadRequest {
			t.Errorf("upload %s: got %d, want 400: %s", label, st, body)
		}
		// Mkdir.
		st, body = postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/mkdir?"+qs, nil, f.token)
		if st != http.StatusBadRequest {
			t.Errorf("mkdir %s: got %d, want 400: %s", label, st, body)
		}
		// Delete.
		st, body = f.delete(t, collID, qs)
		if st != http.StatusBadRequest {
			t.Errorf("delete %s: got %d, want 400: %s", label, st, body)
		}
	}
	// Nothing escaped onto disk outside the collection root.
	if _, err := os.Stat(filepath.Join(root, "..", "evil.txt")); !os.IsNotExist(err) {
		t.Errorf("evil.txt escaped the collection root, stat err=%v", err)
	}

	// Move/copy path validation (JSON body).
	if st, body := postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/move",
		map[string]string{"from": "../evil.txt", "to": "x.txt"}, f.token); st != http.StatusBadRequest {
		t.Errorf("move from traversal: got %d, want 400: %s", st, body)
	}
	if st, body := postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/move",
		map[string]string{"from": "ok.txt", "to": "../evil.txt"}, f.token); st != http.StatusBadRequest {
		t.Errorf("move to traversal: got %d, want 400: %s", st, body)
	}
	if st, body := postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/copy",
		map[string]string{"from": "/etc/passwd", "to": "x.txt"}, f.token); st != http.StatusBadRequest {
		t.Errorf("copy from absolute: got %d, want 400: %s", st, body)
	}
	if st, body := postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/copy",
		map[string]string{"from": "", "to": "x.txt"}, f.token); st != http.StatusBadRequest {
		t.Errorf("copy from empty: got %d, want 400: %s", st, body)
	}

	// Unauthenticated -> 401 on the write routes.
	if st, _ := ws15Raw(t, f.ts, "", http.MethodPost,
		f.ts.URL+"/api/collections/"+collID+"/files/content?path=x.txt", []byte("x"), "application/octet-stream"); st != http.StatusUnauthorized {
		t.Errorf("upload unauth: got %d, want 401", st)
	}
	if st, _ := postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/mkdir?path=xdir", nil, ""); st != http.StatusUnauthorized {
		t.Errorf("mkdir unauth: got %d, want 401", st)
	}
	if st, _ := ws15Raw(t, f.ts, "", http.MethodDelete,
		f.ts.URL+"/api/collections/"+collID+"/files/content?path=x.txt", nil, ""); st != http.StatusUnauthorized {
		t.Errorf("delete unauth: got %d, want 401", st)
	}

	// Foreign / unknown collection -> 404 on the write routes.
	const ghost = "coll-nonexistent"
	if st, body := f.upload(t, ghost, "x.txt", "x", false); st != http.StatusNotFound {
		t.Errorf("upload foreign collection: got %d, want 404: %s", st, body)
	}
	if st, body := postJSON(t, f.ts.URL+"/api/collections/"+ghost+"/files/mkdir?path=xdir", nil, f.token); st != http.StatusNotFound {
		t.Errorf("mkdir foreign collection: got %d, want 404: %s", st, body)
	}
	if st, body := postJSON(t, f.ts.URL+"/api/collections/"+ghost+"/files/move",
		map[string]string{"from": "a.txt", "to": "b.txt"}, f.token); st != http.StatusNotFound {
		t.Errorf("move foreign collection: got %d, want 404: %s", st, body)
	}
	if st, body := postJSON(t, f.ts.URL+"/api/collections/"+ghost+"/files/copy",
		map[string]string{"from": "a.txt", "to": "b.txt"}, f.token); st != http.StatusNotFound {
		t.Errorf("copy foreign collection: got %d, want 404: %s", st, body)
	}
	if st, body := f.delete(t, ghost, "path=x.txt"); st != http.StatusNotFound {
		t.Errorf("delete foreign collection: got %d, want 404: %s", st, body)
	}
}

func TestWriteColdTier(t *testing.T) {
	f := newWS15Fixture(t, ":memory:")
	defer f.close()
	collID, root := f.setupCollection(t, "cold", "coldphotos", "coldphotos")

	// A real file on disk inside the cold root (unindexed is fine: the
	// Cold guard must fire before any disk or catalog work).
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("keep-me"), 0o644); err != nil {
		t.Fatalf("write keep.txt: %v", err)
	}

	type op struct {
		label string
		run   func() (int, []byte)
	}
	ops := []op{
		{"upload", func() (int, []byte) { return f.upload(t, collID, "ghost.txt", "ghost", false) }},
		{"mkdir", func() (int, []byte) {
			return postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/mkdir?path=newdir", nil, f.token)
		}},
		{"move", func() (int, []byte) {
			return postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/move",
				map[string]string{"from": "keep.txt", "to": "moved.txt"}, f.token)
		}},
		{"copy", func() (int, []byte) {
			return postJSON(t, f.ts.URL+"/api/collections/"+collID+"/files/copy",
				map[string]string{"from": "keep.txt", "to": "copy.txt"}, f.token)
		}},
		{"delete", func() (int, []byte) { return f.delete(t, collID, "path=keep.txt") }},
	}
	for _, o := range ops {
		st, body := o.run()
		if st != http.StatusConflict {
			t.Errorf("cold %s: got %d, want 409: %s", o.label, st, body)
			continue
		}
		if !ws15WakeRequired(t, body) {
			t.Errorf("cold %s: wake_required missing in %s", o.label, body)
		}
	}

	// No file changed on disk.
	if b, err := os.ReadFile(filepath.Join(root, "keep.txt")); err != nil || string(b) != "keep-me" {
		t.Errorf("cold keep.txt changed: %q err=%v", b, err)
	}
	for _, rel := range []string{"ghost.txt", "newdir", "moved.txt", "copy.txt"} {
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Errorf("cold %s should not exist on disk, stat err=%v", rel, err)
		}
	}
	// And no catalog rows were created.
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM files WHERE collection_id = ?`, collID).Scan(&n); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if n != 0 {
		t.Errorf("cold tier created %d catalog rows, want 0", n)
	}
}

func TestWriteRestartPersistence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kelvios-ws15.db")

	f1 := newWS15Fixture(t, dbPath)
	collID, _ := f1.setupCollection(t, "warm", "photos", "photos")

	if st, body := f1.upload(t, collID, "keep.txt", "keep-content", false); st != http.StatusCreated {
		t.Fatalf("upload keep.txt: got %d: %s", st, body)
	}
	if st, body := f1.upload(t, collID, "mv.txt", "mv-content", false); st != http.StatusCreated {
		t.Fatalf("upload mv.txt: got %d: %s", st, body)
	}
	if st, body := postJSON(t, f1.ts.URL+"/api/collections/"+collID+"/files/move",
		map[string]string{"from": "mv.txt", "to": "moved.txt"}, f1.token); st != http.StatusOK {
		t.Fatalf("move mv.txt: got %d: %s", st, body)
	}
	if st, body := f1.upload(t, collID, "del.txt", "del-content", false); st != http.StatusCreated {
		t.Fatalf("upload del.txt: got %d: %s", st, body)
	}
	if st, body := f1.delete(t, collID, "path=del.txt"); st != http.StatusNoContent {
		t.Fatalf("delete del.txt: got %d: %s", st, body)
	}
	f1.close()

	// Restart against the same DB file.
	f2 := newWS15Fixture(t, dbPath)
	defer f2.close()
	// The cold/warm fixtures point the pool at each server instance's own
	// temp mountpoint only in the DB — but the DB is shared, so f2's
	// setupCollection is NOT called: pool/collection rows and the
	// mountpoint update from f1 persist in the file DB. The collection
	// root still points at f1's temp dir, which lives for the whole test.

	entries := f2.browse(t, collID, "")
	if ws15Find(entries, "keep.txt") == nil {
		t.Errorf("browse after restart: keep.txt missing in %+v", entries)
	}
	if ws15Find(entries, "moved.txt") == nil {
		t.Errorf("browse after restart: moved.txt missing in %+v", entries)
	}
	if ws15Find(entries, "mv.txt") != nil {
		t.Errorf("browse after restart: mv.txt should be gone in %+v", entries)
	}
	if ws15Find(entries, "del.txt") != nil {
		t.Errorf("browse after restart: del.txt should be gone in %+v", entries)
	}

	st, body := f2.download(t, collID, "keep.txt")
	if st != http.StatusOK || string(body) != "keep-content" {
		t.Errorf("download keep.txt after restart: got %d %q", st, body)
	}
	st, body = f2.download(t, collID, "moved.txt")
	if st != http.StatusOK || string(body) != "mv-content" {
		t.Errorf("download moved.txt after restart: got %d %q", st, body)
	}
	if st, _ := f2.download(t, collID, "del.txt"); st != http.StatusNotFound {
		t.Errorf("download del.txt after restart: got %d, want 404", st)
	}

	// Audit history intact across the restart.
	// Note: file.delete is now file.trash (WS22 Step A behavior change).
	for action, want := range map[string]int{"file.upload": 3, "file.move": 1, "file.trash": 1} {
		var n int
		if err := f2.db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ? AND result = 'ok'`, action).Scan(&n); err != nil {
			t.Fatalf("count audit %s: %v", action, err)
		}
		if n < want {
			t.Errorf("audit %s ok rows after restart: got %d, want >= %d", action, n, want)
		}
	}
}
