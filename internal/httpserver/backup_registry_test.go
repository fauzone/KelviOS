package httpserver

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

// setupRegistryServer creates a test server with one warm pool and one
// collection ("photos") whose root is tmpDir/mnt/warm/photos.
// Returns ts, token, collID, collRoot, tmpDir.
func setupRegistryServer(t *testing.T, db *sql.DB) (*httptest.Server, string, string, string, string) {
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
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-REGISTRY-WARM", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
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
		resp.Body.Close()
		t.Fatalf("POST pool: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var pr struct {
		Pool struct {
			ID string `json:"id"`
		} `json:"pool"`
	}
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if pr.Pool.ID == "" {
		t.Fatalf("pool ID empty")
	}

	resp = doReq("POST", "/api/pools/"+pr.Pool.ID+"/collections", map[string]any{"name": "photos", "path": "photos", "disk_name": "sda"}, token)
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("POST collection: expected 201, got %d: %s", resp.StatusCode, string(b))
	}
	var collResp struct {
		Collection struct {
			ID string `json:"id"`
		} `json:"collection"`
	}
	json.NewDecoder(resp.Body).Decode(&collResp)
	resp.Body.Close()
	if collResp.Collection.ID == "" {
		t.Fatalf("collection ID empty")
	}

	return ts, token, collResp.Collection.ID, collRoot, tmpDir
}

// registryDoReq performs one HTTP call against ts and decodes the JSON body.
func registryDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) (int, map[string]any) {
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

// TestBackupRegistry_CreateRepoPerType tests creating repos of each provider
// type via the registry path: 201 + stored fields.
func TestBackupRegistry_CreateRepoPerType(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, _, _, tmpDir := setupRegistryServer(t, db)
	defer ts.Close()

	tests := []struct {
		name      string
		payload   map[string]any
		wantCode  int
		wantType  string
		wantPath  string
		wantErr   bool
	}{
		{
			name:     "local",
			payload:  map[string]any{"name": "repo-local", "target_type": "local", "target_path": filepath.Join(tmpDir, "repo-local")},
			wantCode: http.StatusCreated,
			wantType: "local",
		},
		{
			name:     "s3",
			payload:  map[string]any{"name": "repo-s3", "target_type": "s3", "config": map[string]string{"endpoint": "s3.amazonaws.com", "bucket": "my-bucket"}, "secret_name": "s3-secret"},
			wantCode: http.StatusCreated,
			wantType: "s3",
		},
		{
			name:     "b2",
			payload:  map[string]any{"name": "repo-b2", "target_type": "b2", "config": map[string]string{"bucket": "my-bucket"}, "secret_name": "b2-secret"},
			wantCode: http.StatusCreated,
			wantType: "b2",
		},
		{
			name:     "sftp",
			payload:  map[string]any{"name": "repo-sftp", "target_type": "sftp", "config": map[string]string{"user": "u", "host": "h", "path": "/p"}},
			wantCode: http.StatusCreated,
			wantType: "sftp",
		},
		{
			name:     "gdrive",
			payload:  map[string]any{"name": "repo-gdrive", "target_type": "gdrive", "config": map[string]string{"remote": "r", "path": "p"}},
			wantCode: http.StatusCreated,
			wantType: "gdrive",
		},
		{
			name:     "unknown target_type",
			payload:  map[string]any{"name": "repo-unknown", "target_type": "azure"},
			wantCode: http.StatusBadRequest,
			wantErr:  true,
		},
		{
			name:     "s3 missing required config",
			payload:  map[string]any{"name": "repo-s3-bad", "target_type": "s3", "config": map[string]string{"bucket": "my-bucket"}, "secret_name": "s3-secret"},
			wantCode: http.StatusBadRequest,
			wantErr:  true,
		},
		{
			name:     "config contains access_key",
			payload:  map[string]any{"name": "repo-secret-key", "target_type": "s3", "config": map[string]string{"endpoint": "s3.amazonaws.com", "bucket": "my-bucket", "access_key": "AKIA..."}, "secret_name": "s3-secret"},
			wantCode: http.StatusBadRequest,
			wantErr:  true,
		},
		{
			name:     "config contains secret_key",
			payload:  map[string]any{"name": "repo-secret-key2", "target_type": "s3", "config": map[string]string{"endpoint": "s3.amazonaws.com", "bucket": "my-bucket", "secret_key": "secret..."}, "secret_name": "s3-secret"},
			wantCode: http.StatusBadRequest,
			wantErr:  true,
		},
		{
			name:     "config contains app_key",
			payload:  map[string]any{"name": "repo-secret-key3", "target_type": "b2", "config": map[string]string{"bucket": "my-bucket", "app_key": "key..."}, "secret_name": "b2-secret"},
			wantCode: http.StatusBadRequest,
			wantErr:  true,
		},
		{
			name:     "config contains password",
			payload:  map[string]any{"name": "repo-secret-key4", "target_type": "sftp", "config": map[string]string{"user": "u", "host": "h", "path": "/p", "password": "pw"}},
			wantCode: http.StatusBadRequest,
			wantErr:  true,
		},
		{
			name:     "config contains token",
			payload:  map[string]any{"name": "repo-secret-key5", "target_type": "gdrive", "config": map[string]string{"remote": "r", "path": "p", "token": "tok"}},
			wantCode: http.StatusBadRequest,
			wantErr:  true,
		},
		{
			name:     "config contains account_id",
			payload:  map[string]any{"name": "repo-secret-key6", "target_type": "b2", "config": map[string]string{"bucket": "my-bucket", "account_id": "acc"}, "secret_name": "b2-secret"},
			wantCode: http.StatusBadRequest,
			wantErr:  true,
		},
		{
			name:     "config contains account_key",
			payload:  map[string]any{"name": "repo-secret-key7", "target_type": "b2", "config": map[string]string{"bucket": "my-bucket", "account_key": "key"}, "secret_name": "b2-secret"},
			wantCode: http.StatusBadRequest,
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out := registryDoReq(t, ts, "POST", "/api/backups/repos", tc.payload, token)
			if code != tc.wantCode {
				t.Fatalf("create repo %s: expected %d, got %d: %v", tc.name, tc.wantCode, code, out)
			}
			if tc.wantErr {
				return
			}
			repo, ok := out["repo"].(map[string]any)
			if !ok {
				t.Fatalf("create repo %s: no 'repo' in response: %v", tc.name, out)
			}
			if repo["target_type"] != tc.wantType {
				t.Errorf("create repo %s: target_type = %v, want %q", tc.name, repo["target_type"], tc.wantType)
			}
			if repo["name"] != tc.payload["name"] {
				t.Errorf("create repo %s: name = %v, want %q", tc.name, repo["name"], tc.payload["name"])
			}
			if tc.wantPath != "" {
				if repo["target_path"] != tc.wantPath {
					t.Errorf("create repo %s: target_path = %v, want %q", tc.name, repo["target_path"], tc.wantPath)
				}
			}
			if repo["id"] == "" {
				t.Errorf("create repo %s: id is empty", tc.name)
			}
		})
	}
}

// TestBackupRegistry_LocalRunRestore tests local repo run + restore through
// the registry path, byte-identical to WS40 behavior.
func TestBackupRegistry_LocalRunRestore(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, collID, collRoot, tmpDir := setupRegistryServer(t, db)
	defer ts.Close()

	mtime := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	writeArrangeHTTPFile(t, collRoot, "a.jpg", "jpeg-a", mtime)
	writeArrangeHTTPFile(t, collRoot, "sub/b.txt", "hello-b", mtime)

	code, out := tagDoReq(t, ts, "POST", "/api/collections/"+collID+"/scan", nil, token)
	if code != http.StatusOK {
		t.Fatalf("scan: expected 200, got %d: %v", code, out)
	}

	repoPath := filepath.Join(tmpDir, "restic-repo-reg")
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": "reg-repo", "target_type": "local", "target_path": repoPath}, token)
	if code != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d: %v", code, out)
	}
	repoMap, _ := out["repo"].(map[string]any)
	repoID, _ := repoMap["id"].(string)
	if repoID == "" {
		t.Fatalf("create repo: no id in %v", out)
	}

	// Run -> 200 with a snapshot_id.
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos/"+repoID+"/run", map[string]any{"collection_id": collID, "password": "pw-1234567890"}, token)
	if code != http.StatusOK {
		t.Fatalf("run: expected 200, got %d: %v", code, out)
	}
	snapID, _ := out["snapshot_id"].(string)
	if snapID == "" {
		t.Fatalf("run: no snapshot_id in %v", out)
	}

	// Restore without confirm -> 409 (P6).
	dest := filepath.Join(tmpDir, "restore-out")
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

// TestBackupRegistry_ColdWakeRequired tests 409 wake_required on Cold+asleep
// through the registry path.
func TestBackupRegistry_ColdWakeRequired(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, _, _, tmpDir := setupRegistryServer(t, db)
	defer ts.Close()

	repoPath := filepath.Join(tmpDir, "restic-repo-cold")
	code, out := tagDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": "cold-repo-reg", "target_type": "local", "target_path": repoPath}, token)
	if code != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d: %v", code, out)
	}
	repoMap, _ := out["repo"].(map[string]any)
	repoID, _ := repoMap["id"].(string)

	// Cold-tier pool + collection.
	coldMount := filepath.Join(tmpDir, "mnt", "cold")
	if err := os.MkdirAll(filepath.Join(coldMount, "photos"), 0o755); err != nil {
		t.Fatalf("mkdir cold root: %v", err)
	}
	code, out = tagDoReq(t, ts, "POST", "/api/pools", map[string]any{"name": "coldpool-reg", "tier": "cold", "mechanism": "single", "disks": []string{"sda"}, "mountpoint": coldMount}, token)
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
}

// TestBackupRegistry_RoundTripSHA256 is the centerpiece: real round-trip
// — create collection + files, scan, run backup via the registry path,
// restore one file to dest, SHA-256 compare restored bytes with source.
func TestBackupRegistry_RoundTripSHA256(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, collID, collRoot, tmpDir := setupRegistryServer(t, db)
	defer ts.Close()

	mtime := time.Date(2024, 6, 15, 10, 0, 0, 0, time.UTC)
	sourceContent := "round-trip test content for SHA-256 verification"
	writeArrangeHTTPFile(t, collRoot, "data.bin", sourceContent, mtime)

	code, out := tagDoReq(t, ts, "POST", "/api/collections/"+collID+"/scan", nil, token)
	if code != http.StatusOK {
		t.Fatalf("scan: expected 200, got %d: %v", code, out)
	}

	repoPath := filepath.Join(tmpDir, "restic-repo-roundtrip")
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": "roundtrip-repo", "target_type": "local", "target_path": repoPath}, token)
	if code != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d: %v", code, out)
	}
	repoMap, _ := out["repo"].(map[string]any)
	repoID, _ := repoMap["id"].(string)

	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos/"+repoID+"/run", map[string]any{"collection_id": collID, "password": "pw-1234567890"}, token)
	if code != http.StatusOK {
		t.Fatalf("run: expected 200, got %d: %v", code, out)
	}
	snapID, _ := out["snapshot_id"].(string)
	if snapID == "" {
		t.Fatalf("run: no snapshot_id in %v", out)
	}

	dest := filepath.Join(tmpDir, "restore-roundtrip")
	code, out = tagDoReq(t, ts, "POST", "/api/backups/repos/"+repoID+"/restore", map[string]any{"snapshot_id": snapID, "rel_path": "data.bin", "dest": dest, "password": "pw-1234567890", "confirm": true}, token)
	if code != http.StatusOK {
		t.Fatalf("restore: expected 200, got %d: %v", code, out)
	}

	restoredPath := filepath.Join(dest, "data.bin")
	restoredBytes, err := os.ReadFile(restoredPath)
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}

	sourceHash := sha256.Sum256([]byte(sourceContent))
	restoredHash := sha256.Sum256(restoredBytes)
	if hex.EncodeToString(sourceHash[:]) != hex.EncodeToString(restoredHash[:]) {
		t.Fatalf("SHA-256 mismatch: source=%x restored=%x", sourceHash, restoredHash)
	}
	if string(restoredBytes) != sourceContent {
		t.Errorf("content mismatch: got %q, want %q", string(restoredBytes), sourceContent)
	}
}

// TestBackupRegistry_StoredFields verifies that created repo rows store the
// correct fields (target_type, target_path, config_json, secret_name).
func TestBackupRegistry_StoredFields(t *testing.T) {
	buildResticStub(t)
	db := openTagDB(t)
	defer db.Close()

	ts, token, _, _, tmpDir := setupRegistryServer(t, db)
	defer ts.Close()

	repoPath := filepath.Join(tmpDir, "stored-fields-repo")
	code, out := tagDoReq(t, ts, "POST", "/api/backups/repos", map[string]any{"name": "stored-fields", "target_type": "local", "target_path": repoPath}, token)
	if code != http.StatusCreated {
		t.Fatalf("create repo: expected 201, got %d: %v", code, out)
	}
	repoMap, _ := out["repo"].(map[string]any)
	repoID, _ := repoMap["id"].(string)

	// List repos and find ours to verify stored fields.
	code, out = tagDoReq(t, ts, "GET", "/api/backups/repos", nil, token)
	if code != http.StatusOK {
		t.Fatalf("list repos: expected 200, got %d: %v", code, out)
	}
	repos, _ := out["repos"].([]any)
	found := false
	for _, r := range repos {
		row, _ := r.(map[string]any)
		if row["id"] == repoID {
			found = true
			if row["target_type"] != "local" {
				t.Errorf("stored target_type = %v, want local", row["target_type"])
			}
			if row["target_path"] != repoPath {
				t.Errorf("stored target_path = %v, want %q", row["target_path"], repoPath)
			}
			if row["name"] != "stored-fields" {
				t.Errorf("stored name = %v, want stored-fields", row["name"])
			}
			break
		}
	}
	if !found {
		t.Errorf("repo %s not found in list: %v", repoID, repos)
	}
}
