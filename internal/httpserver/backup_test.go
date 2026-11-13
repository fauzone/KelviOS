package httpserver

import (
	"database/sql"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
)

// buildResticStub compiles internal/backup/teststub once per test and
// points KELVIOS_RESTIC_BIN at it, so backup HTTP tests need no real
// restic binary.
func buildResticStub(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "restic-stub")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		goBin += ".exe"
	}
	cmd := exec.Command(goBin, "build", "-o", bin, "github.com/fauzone/kelvios/internal/backup/teststub")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build restic stub: %v\n%s", err, out)
	}
	t.Setenv("KELVIOS_RESTIC_BIN", bin)
	return bin
}

func openTagDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestBackupRepoCreateListAndRunRestore(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	ts, token, collID, collRoot := setupArrangeServer(t, db)
	defer ts.Close()
	mtime := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	writeArrangeHTTPFile(t, collRoot, "a.jpg", "jpeg-a", mtime)
	code, out := tagDoReq(t, ts, "POST", "/api/collections/"+collID+"/scan", nil, token)
	if code != http.StatusOK {
		t.Fatalf("scan: expected 200, got %d: %v", code, out)
	}

	repoPath := filepath.Join(t.TempDir(), "restic-repo")
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": "main", "target_path": repoPath}, token)
	if code != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d: %v", code, out)
	}
	repoMap, _ := out["repo"].(map[string]any)
	repoID, _ := repoMap["id"].(string)
	if repoID == "" {
		t.Fatalf("create repo: no id in %v", out)
	}
	// Duplicate name -> 409.
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": "main", "target_path": repoPath}, token)
	if code != http.StatusConflict {
		t.Errorf("duplicate repo: expected 409, got %d: %v", code, out)
	}
	// List -> 200 with the repo.
	code, out = tagDoReq(t, ts, "GET", "/api/backups/repos", nil, token)
	if code != http.StatusOK || out["total"] != float64(1) {
		t.Fatalf("list repos: expected 200/total 1, got %d: %v", code, out)
	}

	// Run -> 200 with a snapshot_id; snapshot recorded and listed.
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos/"+repoID+"/run", map[string]any{"collection_id": collID, "password": "pw-1234567890"}, token)
	if code != http.StatusOK {
		t.Fatalf("run: expected 200, got %d: %v", code, out)
	}
	snapID, _ := out["snapshot_id"].(string)
	if snapID == "" {
		t.Fatalf("run: no snapshot_id in %v", out)
	}
	code, out = tagDoReq(t, ts, "GET", "/api/backups/repos/"+repoID+"/snapshots", nil, token)
	if code != http.StatusOK || out["total"] != float64(1) {
		t.Fatalf("list snapshots: expected 200/total 1, got %d: %v", code, out)
	}

	// Restore without confirm -> 409 (P6).
	dest := filepath.Join(t.TempDir(), "restore-out")
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos/"+repoID+"/restore", map[string]any{"snapshot_id": snapID, "rel_path": "a.jpg", "dest": dest, "password": "pw-1234567890"}, token)
	if code != http.StatusConflict {
		t.Errorf("restore no confirm: expected 409, got %d: %v", code, out)
	}
	// Restore with confirm -> 200 and the file lands at dest/a.jpg.
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos/"+repoID+"/restore", map[string]any{"snapshot_id": snapID, "rel_path": "a.jpg", "dest": dest, "password": "pw-1234567890", "confirm": true}, token)
	if code != http.StatusOK {
		t.Fatalf("restore: expected 200, got %d: %v", code, out)
	}
	if _, err := os.Stat(filepath.Join(dest, "a.jpg")); err != nil {
		t.Errorf("restored file missing: %v", err)
	}
}

func TestBackupRunCold409AndUnknowns(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	ts, token, _, _ := setupArrangeServer(t, db)
	defer ts.Close()

	repoPath := filepath.Join(t.TempDir(), "restic-repo")
	code, out := tagDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": "cold-repo", "target_path": repoPath}, token)
	if code != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d: %v", code, out)
	}
	repoMap, _ := out["repo"].(map[string]any)
	repoID, _ := repoMap["id"].(string)

	// Cold-tier pool + collection (same pattern as the tags cold test).
	tmpDir := t.TempDir()
	coldMount := filepath.Join(tmpDir, "mnt", "cold")
	if err := os.MkdirAll(filepath.Join(coldMount, "photos"), 0o755); err != nil {
		t.Fatalf("mkdir cold root: %v", err)
	}
	code, out = tagDoReq(t, ts, "POST", "/api/pools", map[string]any{"name": "coldpool", "tier": "cold", "mechanism": "single", "disks": []string{"sda"}, "mountpoint": coldMount}, token)
	if code != http.StatusCreated {
		t.Fatalf("cold pool: expected 201, got %d: %v", code, out)
	}
	pool, _ := out["pool"].(map[string]any)
	code, out = tagDoReq(t, ts, "POST", "/api/pools/"+pool["id"].(string)+"/collections", map[string]any{"name": "coldphotos", "path": "photos", "disk_name": "sda"}, token)
	if code != http.StatusCreated {
		t.Fatalf("cold collection: expected 201, got %d: %v", code, out)
	}
	coll, _ := out["collection"].(map[string]any)
	coldCollID, _ := coll["id"].(string)

	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos/"+repoID+"/run", map[string]any{"collection_id": coldCollID, "password": "pw-1234567890"}, token)
	if code != http.StatusConflict {
		t.Fatalf("cold run: expected 409, got %d: %v", code, out)
	}
	if out["wake_required"] != true {
		t.Errorf("cold run wake_required: got %v, want true", out["wake_required"])
	}

	// Unknown repo -> 404; unauth -> 401.
	code, out = tagDoReq(t, ts, "GET", "/api/backups/repos/nope/snapshots", nil, token)
	if code != http.StatusNotFound {
		t.Errorf("unknown repo: expected 404, got %d: %v", code, out)
	}
	code, out = tagDoReq(t, ts, "GET", "/api/backups/repos", nil, "")
	if code != http.StatusUnauthorized {
		t.Errorf("unauth repos: expected 401, got %d: %v", code, out)
	}
	// Unknown collection in run -> 404.
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos/"+repoID+"/run", map[string]any{"collection_id": "coll-unknown", "password": "pw-1234567890"}, token)
	if code != http.StatusNotFound {
		t.Errorf("unknown collection run: expected 404, got %d: %v", code, out)
	}
}

