package httpserver

// KelviOS WS22 STEP B: Recycle bin (FR-2.10) HTTP handler tests.
// Covers: migration v15, catalog helpers (RecordTrash/ListTrash/GetTrash/DeleteTrashRow),
// handler contracts (list/restore/purge/empty), DELETE file -> trash, 401/404/405,
// DELETE directory stays 400, scan skips .kelvios-trash, restart persistence,
// audit file.trash/file.restore/file.purge/file.empty_trash.

import (
	"context"
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

type ws22Fixture struct {
	s      *Server
	ts     *httptest.Server
	db     *sql.DB
	token  string
	tmpDir string
}

func newWS22Fixture(t *testing.T, dbPath string) *ws22Fixture {
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
	f := &ws22Fixture{s: s, ts: ts, db: db, tmpDir: t.TempDir()}
	f.token = mustToken(t, ts)
	return f
}

func (f *ws22Fixture) close() {
	f.ts.Close()
	_ = f.s.Shutdown(context.Background())
	_ = f.db.Close()
}

// setupCollection creates a pool+collection via the API and points the
// pool mountpoint at a fresh temp dir. Returns (collectionID, root).
func (f *ws22Fixture) setupCollection(t *testing.T, tier, name, collPath string) (string, string) {
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
		"name":     name,
		"path":     collPath,
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
	collID := collResp.Collection.ID

	// Mountpoint must be the PARENT dir: ResolveCollectionRoot joins mountpoint +
	// collection path, so the resolved root is root below (not root/collPath).
	root := filepath.Join(f.tmpDir, collPath)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	_, err := f.db.Exec(`UPDATE pools SET mountpoint = ? WHERE id = ?`, f.tmpDir, poolID)
	if err != nil {
		t.Fatalf("update pool mountpoint: %v", err)
	}
	return collID, root
}

func (f *ws22Fixture) upload(t *testing.T, collID, relPath, content string, overwrite bool) (int, []byte) {
	t.Helper()
	url := f.ts.URL + "/api/collections/" + collID + "/files/content?path=" + relPath
	if overwrite {
		url += "&overwrite=true"
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(content))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func (f *ws22Fixture) delete(t *testing.T, collID, query string) (int, []byte) {
	t.Helper()
	url := f.ts.URL + "/api/collections/" + collID + "/files/content?" + query
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func (f *ws22Fixture) browse(t *testing.T, collID, prefix string) []map[string]any {
	t.Helper()
	url := f.ts.URL + "/api/collections/" + collID + "/files"
	if prefix != "" {
		url += "?path=" + prefix
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("browse: got %d: %s", resp.StatusCode, body)
	}
	var respBody struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(body, &respBody); err != nil {
		t.Fatalf("decode browse: %v", err)
	}
	return respBody.Entries
}

func (f *ws22Fixture) listTrash(t *testing.T, collID string, limit, offset int) (int, []byte) {
	t.Helper()
	url := f.ts.URL + "/api/collections/" + collID + "/trash"
	if limit > 0 {
		url += "?limit=" + string(rune(limit))
	}
	if offset > 0 {
		if strings.Contains(url, "?") {
			url += "&offset=" + string(rune(offset))
		} else {
			url += "?offset=" + string(rune(offset))
		}
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func (f *ws22Fixture) restoreTrash(t *testing.T, collID, trashID, targetRelPath string) (int, []byte) {
	t.Helper()
	url := f.ts.URL + "/api/collections/" + collID + "/trash/" + trashID + "/restore"
	var body []byte
	if targetRelPath != "" {
		body, _ = json.Marshal(map[string]string{"target_rel_path": targetRelPath})
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody
}

func (f *ws22Fixture) purgeTrash(t *testing.T, collID, trashID string) (int, []byte) {
	t.Helper()
	url := f.ts.URL + "/api/collections/" + collID + "/trash/" + trashID
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func (f *ws22Fixture) emptyTrash(t *testing.T, collID string) (int, []byte) {
	t.Helper()
	url := f.ts.URL + "/api/collections/" + collID + "/trash"
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func (f *ws22Fixture) download(t *testing.T, collID, relPath string) (int, []byte) {
	t.Helper()
	url := f.ts.URL + "/api/collections/" + collID + "/files/content?path=" + relPath
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func ws22Find(entries []map[string]any, name string) *map[string]any {
	for _, e := range entries {
		if e["name"] == name {
			return &e
		}
	}
	return nil
}

// TestTrashMigration tests that migration to v15 creates the trash_items table and index.
func TestTrashMigration(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Verify trash_items table exists
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", "trash_items").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("table trash_items not created")
	}

	// Verify schema version (15 after WS22; WS25 raised it to 16; WS27 raised it to 17; WS29 raised it to 18; WS30 raised it to 19; WS32 raised it to 20; WS33 raised it to 21: tier_recommendations/tier_pins/tier_policies; WS34-A raised it to 22: tier_migrations; WS35 raised it to 23: file_shadows; WS36 raised it to 24: dirty write-back shadows; WS37 raised it to 25: shadow eviction + promotion recommendations; WS38 raised it to 26: arrange tables; WS39 raised it to 27: tagging tables; WS40 raised it to 28: backup tables; WS42-A raised it to 29: wake_checklist_runs; WS42-B raised it to 30: checksum columns + scrub_state; WS43 raised it to 31: backup provider registry; WS44 raised it to 32: scheduled restore tests; WS51-A raised it to 36: agent_modes; WS52-A raised it to 37: compose backend core; WS53-B raised it to 38: app_installs; WS55 raised it to 39: app_installs columns + v39; WS59 raised it to 40: ota_updates; WS62 raised it to 41: event_rules)
	var version int
	err = db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&version)
	if err != nil || version != 41 {
		t.Errorf("schema_version: got %d, want 41", version)
	}

	// Verify index exists
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", "idx_trash_collection_deleted").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("index idx_trash_collection_deleted not created")
	}
}

// TestTrashMigrationFromV14 tests that migrating from v14 preserves existing data.
func TestTrashMigrationFromV14(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Manually create v14 schema (without trash_items)
	_, err = db.Exec(`
		CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO meta (key, value) VALUES ('schema_version', '14');
		CREATE TABLE collections (id TEXT PRIMARY KEY, name TEXT NOT NULL, pool_id TEXT NOT NULL, path TEXT NOT NULL, disk_name TEXT, size_bytes INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL);
		CREATE TABLE audit_log (id TEXT PRIMARY KEY, created_at INTEGER NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, target_type TEXT NOT NULL, target_id TEXT NOT NULL, result TEXT NOT NULL, detail TEXT);
	`)
	if err != nil {
		t.Fatalf("create v14 schema: %v", err)
	}

	// Insert some pre-existing data
	_, err = db.Exec(`INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, created_at, updated_at) VALUES ('coll-1', 'photos', 'pool-1', '/photos', 'sda', 0, 1700000000, 1700000000)`)
	if err != nil {
		t.Fatalf("insert collection: %v", err)
	}
	_, err = db.Exec(`INSERT INTO audit_log (id, created_at, actor, action, target_type, target_id, result, detail) VALUES ('audit-1', 1700000000, 'test', 'file.upload', 'collection', 'coll-1', 'ok', 'test.txt')`)
	if err != nil {
		t.Fatalf("insert audit: %v", err)
	}

	// Run migration (v15 via WS22, then v16 via WS25)
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("Migrate from v14: %v", err)
	}

	// Verify schema version is now 41 (v15 trash, v16 encryption, v17 access_events, v18 pool name uniqueness, v19 power schedules, v20 collection_heat, v21 tier_recommendations/tier_pins/tier_policies, v22 tier_migrations, v23 file_shadows, v24 dirty write-back shadows, v25 shadow eviction + promotion recommendations, v26 arrange tables, v27 tagging tables, v28 backup tables, v29 wake_checklist_runs, v30 checksum columns + scrub_state, v31 backup provider registry, v32 scheduled restore tests, v33 chat bridge, v34 wake_checklist_runs, v35 checksum columns + scrub_state, v36 agent_modes, v37 compose backend core, v38 app_installs, v39 app_installs columns, v40 ota_updates, v41 event_rules)
	var version int
	err = db.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&version)
	if err != nil || version != 41 {
		t.Errorf("schema_version after migration: got %d, want 41", version)
	}

	// Verify trash_items table and index exist
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", "trash_items").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("trash_items table not created after migration")
	}
	err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", "idx_trash_collection_deleted").Scan(&count)
	if err != nil || count == 0 {
		t.Errorf("idx_trash_collection_deleted not created after migration")
	}

	// Verify pre-existing data preserved
	var collName string
	err = db.QueryRow("SELECT name FROM collections WHERE id = 'coll-1'").Scan(&collName)
	if err != nil || collName != "photos" {
		t.Errorf("pre-existing collection not preserved: %v", err)
	}
	var auditAction string
	err = db.QueryRow("SELECT action FROM audit_log WHERE id = 'audit-1'").Scan(&auditAction)
	if err != nil || auditAction != "file.upload" {
		t.Errorf("pre-existing audit not preserved: %v", err)
	}
}

// TestRecordTrash tests RecordTrash insert and ID generation.
func TestRecordTrash(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Create pool and collection first (FK constraint)
	pool := &catalog.PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := catalog.CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	coll := &catalog.CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := catalog.CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	// Insert with empty ID (auto-generate)
	id1, err := catalog.RecordTrash(db, "", "coll-1", "test.txt", ".kelvios-trash/trash-1/test.txt", "file", "sda", 1024, 30, "test-user")
	if err != nil {
		t.Fatalf("RecordTrash auto ID: %v", err)
	}
	if id1 == "" || !strings.HasPrefix(id1, "trash-") {
		t.Errorf("unexpected auto-generated ID: %s", id1)
	}

	// Insert with explicit ID
	id2, err := catalog.RecordTrash(db, "trash-explicit-1", "coll-1", "test2.txt", ".kelvios-trash/trash-explicit-1/test2.txt", "file", "sda", 2048, 60, "test-user")
	if err != nil {
		t.Fatalf("RecordTrash explicit ID: %v", err)
	}
	if id2 != "trash-explicit-1" {
		t.Errorf("explicit ID not preserved: got %s", id2)
	}

	// Verify both rows exist
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM trash_items WHERE collection_id = 'coll-1'").Scan(&count)
	if err != nil || count != 2 {
		t.Errorf("expected 2 trash items, got %d: %v", count, err)
	}

	// Verify retention_days default when <= 0
	id3, err := catalog.RecordTrash(db, "", "coll-1", "test3.txt", ".kelvios-trash/trash-3/test3.txt", "file", "sda", 512, 0, "test-user")
	if err != nil {
		t.Fatalf("RecordTrash zero retention: %v", err)
	}
	var retention int
	err = db.QueryRow("SELECT retention_days FROM trash_items WHERE id = ?", id3).Scan(&retention)
	if err != nil || retention != 30 {
		t.Errorf("retention_days default: got %d, want 30", retention)
	}
}

// TestListTrash tests ListTrash newest-first, limit/offset, and total count.
func TestListTrash(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Create pool and collection first (FK constraint)
	pool := &catalog.PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := catalog.CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	coll := &catalog.CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := catalog.CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	// Insert 3 trash items with slight time gaps to ensure ordering
	baseTime := time.Unix(1700000000, 0)
	items := []struct {
		id        string
		relPath   string
		deletedAt time.Time
	}{
		{"trash-1", "a.txt", baseTime},
		{"trash-2", "b.txt", baseTime.Add(time.Second)},
		{"trash-3", "c.txt", baseTime.Add(2 * time.Second)},
	}
	for _, item := range items {
		_, err := catalog.RecordTrash(db, item.id, "coll-1", item.relPath, ".kelvios-trash/"+item.id+"/"+item.relPath, "file", "sda", 100, 30, "test-user")
		if err != nil {
			t.Fatalf("RecordTrash %s: %v", item.id, err)
		}
		// Manually update deleted_at for ordering test
		_, err = db.Exec("UPDATE trash_items SET deleted_at = ? WHERE id = ?", item.deletedAt.Unix(), item.id)
		if err != nil {
			t.Fatalf("update deleted_at %s: %v", item.id, err)
		}
	}

	// List all - newest first (trash-3, trash-2, trash-1)
	entries, total, err := catalog.ListTrash(db, "coll-1", 0, 0)
	if err != nil {
		t.Fatalf("ListTrash: %v", err)
	}
	if total != 3 || len(entries) != 3 {
		t.Errorf("expected 3 items, got total=%d len=%d", total, len(entries))
	}
	if entries[0].ID != "trash-3" || entries[1].ID != "trash-2" || entries[2].ID != "trash-1" {
		t.Errorf("unexpected order: %s, %s, %s", entries[0].ID, entries[1].ID, entries[2].ID)
	}

	// Pagination: limit=1, offset=0
	page1, total1, err := catalog.ListTrash(db, "coll-1", 1, 0)
	if err != nil {
		t.Fatalf("ListTrash page1: %v", err)
	}
	if total1 != 3 || len(page1) != 1 || page1[0].ID != "trash-3" {
		t.Errorf("page1: total=%d len=%d first=%s", total1, len(page1), page1[0].ID)
	}

	// Pagination: limit=1, offset=1
	page2, total2, err := catalog.ListTrash(db, "coll-1", 1, 1)
	if err != nil {
		t.Fatalf("ListTrash page2: %v", err)
	}
	if total2 != 3 || len(page2) != 1 || page2[0].ID != "trash-2" {
		t.Errorf("page2: total=%d len=%d first=%s", total2, len(page2), page2[0].ID)
	}

	// Limit clamp (max 1000)
	pageLarge, totalLarge, err := catalog.ListTrash(db, "coll-1", 2000, 0)
	if err != nil {
		t.Fatalf("ListTrash large limit: %v", err)
	}
	if len(pageLarge) != 3 {
		t.Errorf("limit clamp: expected 3 (max 1000), got %d", len(pageLarge))
	}
	if totalLarge != 3 {
		t.Errorf("total unchanged by limit: got %d", totalLarge)
	}

	// Empty list for non-existent collection
	emptyList, emptyTotal, err := catalog.ListTrash(db, "coll-nonexistent", 0, 0)
	if err != nil {
		t.Fatalf("ListTrash empty: %v", err)
	}
	if emptyTotal != 0 || len(emptyList) != 0 {
		t.Errorf("expected empty list, got total=%d len=%d", emptyTotal, len(emptyList))
	}
	if emptyList == nil {
		t.Errorf("empty list should be empty slice, not nil")
	}
}

// TestGetTrash tests GetTrash by ID and not found.
func TestGetTrash(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Create pool and collection first (FK constraint)
	pool := &catalog.PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := catalog.CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	coll := &catalog.CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := catalog.CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	_, err = catalog.RecordTrash(db, "trash-get-1", "coll-1", "test.txt", ".kelvios-trash/trash-get-1/test.txt", "file", "sda", 1024, 30, "test-user")
	if err != nil {
		t.Fatalf("RecordTrash: %v", err)
	}

	// Get existing
	got, err := catalog.GetTrash(db, "trash-get-1")
	if err != nil {
		t.Fatalf("GetTrash: %v", err)
	}
	if got == nil {
		t.Fatal("GetTrash returned nil for existing item")
	}
	if got.ID != "trash-get-1" || got.OriginalRelPath != "test.txt" || got.CollectionID != "coll-1" {
		t.Errorf("GetTrash mismatch: %+v", got)
	}

	// Get non-existent
	got, err = catalog.GetTrash(db, "trash-nonexistent")
	if err != nil {
		t.Fatalf("GetTrash nonexistent: %v", err)
	}
	if got != nil {
		t.Error("GetTrash should return nil for non-existent item")
	}
}

// TestDeleteTrashRow tests DeleteTrashRow and not found error.
func TestDeleteTrashRow(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Create pool and collection first (FK constraint)
	pool := &catalog.PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := catalog.CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	coll := &catalog.CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := catalog.CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	_, err = catalog.RecordTrash(db, "trash-del-1", "coll-1", "test.txt", ".kelvios-trash/trash-del-1/test.txt", "file", "sda", 1024, 30, "test-user")
	if err != nil {
		t.Fatalf("RecordTrash: %v", err)
	}

	// Delete existing
	if err := catalog.DeleteTrashRow(db, "trash-del-1"); err != nil {
		t.Fatalf("DeleteTrashRow: %v", err)
	}

	// Verify deleted
	got, err := catalog.GetTrash(db, "trash-del-1")
	if err != nil {
		t.Fatalf("GetTrash after delete: %v", err)
	}
	if got != nil {
		t.Error("trash item should be deleted")
	}

	// Delete missing should return error
	err = catalog.DeleteTrashRow(db, "trash-del-1")
	if err == nil {
		t.Error("DeleteTrashRow should fail for missing item")
	}
}

// TestTrashCatalogRestartPersistence tests that trash rows survive DB close/reopen.
func TestTrashCatalogRestartPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	db, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Create pool and collection first (FK constraint)
	pool := &catalog.PoolRow{
		ID:         "pool-1",
		Name:       "media",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/media",
		SizeBytes:  4000787030016,
		Status:     "active",
	}
	if err := catalog.CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	coll := &catalog.CollectionRow{
		ID:        "coll-1",
		Name:      "photos",
		PoolID:    "pool-1",
		Path:      "/photos",
		DiskName:  "sda",
		SizeBytes: 0,
	}
	if err := catalog.CreateCollection(db, coll); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	_, err = catalog.RecordTrash(db, "trash-persist-1", "coll-1", "test.txt", ".kelvios-trash/trash-persist-1/test.txt", "file", "sda", 1024, 30, "test-user")
	if err != nil {
		t.Fatalf("RecordTrash: %v", err)
	}

	db.Close()

	// Reopen and verify persistence
	db2, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()

	got, err := catalog.GetTrash(db2, "trash-persist-1")
	if err != nil {
		t.Fatalf("GetTrash: %v", err)
	}
	if got == nil {
		t.Fatal("trash item not persisted across restart")
	}
	if got.OriginalRelPath != "test.txt" || got.CollectionID != "coll-1" {
		t.Errorf("trash data mismatch: %+v", got)
	}
}

// --- Chief takeover (2026-10-31): HTTP handler tests for WS22 Step B ---
// The worker sessions looped twice on this section; the catalog-level tests
// above landed, the handler contracts below did not. Written by Chief.

func ws22AuditOK(t *testing.T, db *sql.DB, action string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ? AND result = 'ok'`, action).Scan(&n); err != nil {
		t.Fatalf("audit count %s: %v", action, err)
	}
	return n
}

func ws22DecodeTrashList(t *testing.T, body []byte) (int, []map[string]any) {
	t.Helper()
	var resp struct {
		Entries []map[string]any `json:"entries"`
		Total   int              `json:"total"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode trash list: %v", err)
	}
	return resp.Total, resp.Entries
}

func ws22Raw(t *testing.T, method, url, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// TestTrashHTTPDeleteListRestoreFlow covers the core user journey:
// upload -> DELETE (moves to trash, 204) -> browse hides trash -> list shows
// the item -> restore (200) -> file back with original content -> trash empty.
func TestTrashHTTPDeleteListRestoreFlow(t *testing.T) {
	f := newWS22Fixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	collID, root := f.setupCollection(t, "warm", "flow", "flow")

	st, _ := f.upload(t, collID, "hello.txt", "hello-trash", false)
	if st != http.StatusCreated {
		t.Fatalf("upload: got %d, want 201", st)
	}

	st, _ = f.delete(t, collID, "path=hello.txt")
	if st != http.StatusNoContent {
		t.Fatalf("DELETE file: got %d, want 204", st)
	}

	// Browse must not surface the deleted file nor the trash dir.
	for _, e := range f.browse(t, collID, "") {
		if e["name"] == "hello.txt" || e["name"] == ".kelvios-trash" {
			t.Errorf("browse after delete still shows %v", e["name"])
		}
	}
	// The trashed file must exist on disk under .kelvios-trash.
	matches, err := filepath.Glob(filepath.Join(root, ".kelvios-trash", "*", "hello.txt"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("trashed file on disk: matches=%v err=%v", matches, err)
	}

	st, body := f.listTrash(t, collID, 0, 0)
	if st != http.StatusOK {
		t.Fatalf("list trash: got %d, want 200: %s", st, body)
	}
	total, entries := ws22DecodeTrashList(t, body)
	if total != 1 || len(entries) != 1 {
		t.Fatalf("list trash: total=%d len=%d, want 1", total, len(entries))
	}
	if entries[0]["original_rel_path"] != "hello.txt" {
		t.Errorf("trash entry original_rel_path: got %v, want hello.txt", entries[0]["original_rel_path"])
	}
	trashID, _ := entries[0]["id"].(string)
	if trashID == "" {
		t.Fatal("trash entry has no id")
	}

	st, body = f.restoreTrash(t, collID, trashID, "")
	if st != http.StatusOK {
		t.Fatalf("restore: got %d, want 200: %s", st, body)
	}
	var restored struct {
		RestoredTo string `json:"restored_to"`
	}
	if err := json.Unmarshal(body, &restored); err != nil || restored.RestoredTo != "hello.txt" {
		t.Fatalf("restore body: %s err=%v", body, err)
	}

	st, body = f.download(t, collID, "hello.txt")
	if st != http.StatusOK || string(body) != "hello-trash" {
		t.Fatalf("download after restore: got %d %q, want 200 hello-trash", st, body)
	}

	st, body = f.listTrash(t, collID, 0, 0)
	total, _ = ws22DecodeTrashList(t, body)
	if st != http.StatusOK || total != 0 {
		t.Errorf("list trash after restore: got %d total=%d, want 200/0", st, total)
	}
	if n := ws22AuditOK(t, f.db, "file.trash"); n < 1 {
		t.Errorf("audit file.trash ok: got %d, want >= 1", n)
	}
	if n := ws22AuditOK(t, f.db, "file.restore"); n < 1 {
		t.Errorf("audit file.restore ok: got %d, want >= 1", n)
	}
}

// TestTrashHTTPRestoreConflictAndNotFound covers restore 409 (target exists),
// 404 (trash id missing), and 404 (trash id from another collection).
func TestTrashHTTPRestoreConflictAndNotFound(t *testing.T) {
	f := newWS22Fixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	collID, _ := f.setupCollection(t, "warm", "conflict", "conflict")
	otherID, _ := f.setupCollection(t, "warm", "other", "other")

	if st, _ := f.upload(t, collID, "a.txt", "v1", false); st != http.StatusCreated {
		t.Fatalf("upload a.txt: got %d, want 201", st)
	}
	if st, _ := f.delete(t, collID, "path=a.txt"); st != http.StatusNoContent {
		t.Fatalf("DELETE a.txt: got %d, want 204", st)
	}
	st, body := f.listTrash(t, collID, 0, 0)
	if st != http.StatusOK {
		t.Fatalf("list trash: got %d, want 200: %s", st, body)
	}
	_, entries := ws22DecodeTrashList(t, body)
	if len(entries) != 1 {
		t.Fatalf("list trash: got %d entries, want 1", len(entries))
	}
	trashID, _ := entries[0]["id"].(string)

	// Re-upload the same name, then restore must conflict.
	if st, _ := f.upload(t, collID, "a.txt", "v2", false); st != http.StatusCreated {
		t.Fatalf("re-upload a.txt: got %d, want 201", st)
	}
	if st, body := f.restoreTrash(t, collID, trashID, ""); st != http.StatusConflict {
		t.Errorf("restore over existing target: got %d, want 409: %s", st, body)
	}

	// Missing trash id.
	if st, body := f.restoreTrash(t, collID, "trash-nope", ""); st != http.StatusNotFound {
		t.Errorf("restore missing trash: got %d, want 404: %s", st, body)
	}

	// Trash id belonging to another collection.
	if st, body := f.restoreTrash(t, otherID, trashID, ""); st != http.StatusNotFound {
		t.Errorf("restore cross-collection trash: got %d, want 404: %s", st, body)
	}
}

// TestTrashHTTPPurgeAndEmpty covers purge (204, 404 when missing) and
// empty-all (204), plus their audit actions.
func TestTrashHTTPPurgeAndEmpty(t *testing.T) {
	f := newWS22Fixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	collID, _ := f.setupCollection(t, "warm", "purge", "purge")
	for _, name := range []string{"x.txt", "y.txt"} {
		if st, _ := f.upload(t, collID, name, "data-"+name, false); st != http.StatusCreated {
			t.Fatalf("upload %s: got %d, want 201", name, st)
		}
		if st, _ := f.delete(t, collID, "path="+name); st != http.StatusNoContent {
			t.Fatalf("DELETE %s: got %d, want 204", name, st)
		}
	}
	st, body := f.listTrash(t, collID, 0, 0)
	total, entries := ws22DecodeTrashList(t, body)
	if st != http.StatusOK || total != 2 {
		t.Fatalf("list trash: got %d total=%d, want 200/2", st, total)
	}
	firstID, _ := entries[0]["id"].(string)

	if st, body := f.purgeTrash(t, collID, firstID); st != http.StatusNoContent {
		t.Fatalf("purge: got %d, want 204: %s", st, body)
	}
	st, body = f.listTrash(t, collID, 0, 0)
	total, _ = ws22DecodeTrashList(t, body)
	if total != 1 {
		t.Errorf("list trash after purge: total=%d, want 1", total)
	}
	if st, body := f.purgeTrash(t, collID, firstID); st != http.StatusNotFound {
		t.Errorf("purge missing: got %d, want 404: %s", st, body)
	}
	if n := ws22AuditOK(t, f.db, "file.purge"); n < 1 {
		t.Errorf("audit file.purge ok: got %d, want >= 1", n)
	}

	if st, body := f.emptyTrash(t, collID); st != http.StatusNoContent {
		t.Fatalf("empty trash: got %d, want 204: %s", st, body)
	}
	st, body = f.listTrash(t, collID, 0, 0)
	total, _ = ws22DecodeTrashList(t, body)
	if total != 0 {
		t.Errorf("list trash after empty: total=%d, want 0", total)
	}
	if n := ws22AuditOK(t, f.db, "file.empty_trash"); n < 1 {
		t.Errorf("audit file.empty_trash ok: got %d, want >= 1", n)
	}
}

// TestTrashHTTPGuards covers 401 (no token), 404 (unknown collection),
// 405 (wrong method on trash routes), and directory delete staying 400.
func TestTrashHTTPGuards(t *testing.T) {
	f := newWS22Fixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	collID, root := f.setupCollection(t, "warm", "guards", "guards")

	if st, _ := ws22Raw(t, http.MethodGet, f.ts.URL+"/api/collections/"+collID+"/trash", ""); st != http.StatusUnauthorized {
		t.Errorf("GET trash without token: got %d, want 401", st)
	}
	if st, body := ws22Raw(t, http.MethodGet, f.ts.URL+"/api/collections/coll-nope/trash", f.token); st != http.StatusNotFound {
		t.Errorf("GET trash unknown collection: got %d, want 404: %s", st, body)
	}
	if st, body := ws22Raw(t, http.MethodPost, f.ts.URL+"/api/collections/"+collID+"/trash", f.token); st != http.StatusMethodNotAllowed {
		t.Errorf("POST collection trash: got %d, want 405: %s", st, body)
	}
	if st, body := ws22Raw(t, http.MethodGet, f.ts.URL+"/api/collections/"+collID+"/trash/trash-x", f.token); st != http.StatusMethodNotAllowed {
		t.Errorf("GET trash item: got %d, want 405: %s", st, body)
	}

	// On-disk directory without dir=true must stay 400 (dir-trash out of scope).
	if err := os.MkdirAll(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	if st, body := f.delete(t, collID, "path=subdir"); st != http.StatusBadRequest {
		t.Errorf("DELETE directory: got %d, want 400: %s", st, body)
	}
	if _, err := os.Stat(filepath.Join(root, "subdir")); err != nil {
		t.Errorf("directory must survive failed delete: %v", err)
	}
}

// TestTrashHTTPColdTier covers Cold 409 wake_required on DELETE and on
// restore (tier flipped via DB after warm setup, since Cold upload is out
// of this test's scope).
func TestTrashHTTPColdTier(t *testing.T) {
	f := newWS22Fixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	collID, _ := f.setupCollection(t, "warm", "cold", "cold")
	if st, _ := f.upload(t, collID, "f1.txt", "one", false); st != http.StatusCreated {
		t.Fatalf("upload f1: got %d, want 201", st)
	}
	if st, _ := f.upload(t, collID, "f2.txt", "two", false); st != http.StatusCreated {
		t.Fatalf("upload f2: got %d, want 201", st)
	}
	if st, _ := f.delete(t, collID, "path=f1.txt"); st != http.StatusNoContent {
		t.Fatalf("DELETE f1: got %d, want 204", st)
	}
	st, body := f.listTrash(t, collID, 0, 0)
	if st != http.StatusOK {
		t.Fatalf("list trash: got %d, want 200: %s", st, body)
	}
	_, entries := ws22DecodeTrashList(t, body)
	if len(entries) != 1 {
		t.Fatalf("list trash: got %d entries, want 1", len(entries))
	}
	trashID, _ := entries[0]["id"].(string)

	var poolID string
	if err := f.db.QueryRow(`SELECT pool_id FROM collections WHERE id = ?`, collID).Scan(&poolID); err != nil {
		t.Fatalf("pool_id lookup: %v", err)
	}
	if _, err := f.db.Exec(`UPDATE pools SET tier = 'cold' WHERE id = ?`, poolID); err != nil {
		t.Fatalf("flip tier to cold: %v", err)
	}

	if st, body := f.delete(t, collID, "path=f2.txt"); st != http.StatusConflict {
		t.Errorf("DELETE on Cold: got %d, want 409: %s", st, body)
	} else if !strings.Contains(string(body), "wake_required") {
		t.Errorf("DELETE on Cold body lacks wake_required: %s", body)
	}
	if st, body := f.restoreTrash(t, collID, trashID, ""); st != http.StatusConflict {
		t.Errorf("restore on Cold: got %d, want 409: %s", st, body)
	} else if !strings.Contains(string(body), "wake_required") {
		t.Errorf("restore on Cold body lacks wake_required: %s", body)
	}
}

// TestTrashHTTPRestartPersistence covers trash rows + audit rows surviving
// a full server restart (new fixture over the same DB file).
func TestTrashHTTPRestartPersistence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	f1 := newWS22Fixture(t, dbPath)
	collID, _ := f1.setupCollection(t, "warm", "restart", "restart")
	if st, _ := f1.upload(t, collID, "keep.txt", "keep-me", false); st != http.StatusCreated {
		t.Fatalf("upload: got %d, want 201", st)
	}
	if st, _ := f1.delete(t, collID, "path=keep.txt"); st != http.StatusNoContent {
		t.Fatalf("DELETE: got %d, want 204", st)
	}
	f1.close()

	f2 := newWS22Fixture(t, dbPath)
	defer f2.close()
	st, body := f2.listTrash(t, collID, 0, 0)
	total, entries := ws22DecodeTrashList(t, body)
	if st != http.StatusOK || total != 1 {
		t.Fatalf("list trash after restart: got %d total=%d, want 200/1: %s", st, total, body)
	}
	if entries[0]["original_rel_path"] != "keep.txt" {
		t.Errorf("trash entry after restart: got %v, want keep.txt", entries[0]["original_rel_path"])
	}
	if n := ws22AuditOK(t, f2.db, "file.trash"); n < 1 {
		t.Errorf("audit file.trash ok after restart: got %d, want >= 1", n)
	}
}
