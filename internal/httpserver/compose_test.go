package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/compose"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"

	_ "modernc.org/sqlite"
)

type composeFixture struct {
	s      *Server
	ts     *httptest.Server
	db     *sql.DB
	token  string
	tmpDir string
}

func newComposeFixture(t *testing.T, dbPath string) *composeFixture {
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
	f := &composeFixture{s: s, ts: ts, db: db, tmpDir: t.TempDir()}
	f.token = mustToken(t, ts)
	return f
}

func (f *composeFixture) close() {
	f.ts.Close()
	_ = f.s.Shutdown(context.Background())
	_ = f.db.Close()
}

func (f *composeFixture) postJSON(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, f.ts.URL+path, bytes.NewReader(data))
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

func (f *composeFixture) getJSON(t *testing.T, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.ts.URL+path, nil)
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

func (f *composeFixture) putJSON(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPut, f.ts.URL+path, bytes.NewReader(data))
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

func (f *composeFixture) deleteReq(t *testing.T, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, f.ts.URL+path, nil)
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

func decodeArtifact(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var resp struct {
		Artifact map[string]any `json:"artifact"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode artifact: %v", err)
	}
	return resp.Artifact
}

func decodeArtifactList(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var resp struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode artifact list: %v", err)
	}
	return resp.Artifacts
}

func TestComposeCreateListGet(t *testing.T) {
	f := newComposeFixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	// Create report_schedule
	spec := map[string]any{
		"transport":      "telegram",
		"chat_id":        "123456",
		"interval_hours": 24,
	}
	st, body := f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "daily-report",
		"kind": compose.KindReportSchedule,
		"spec": spec,
	})
	if st != http.StatusCreated {
		t.Fatalf("create: got %d, want 201: %s", st, body)
	}
	artifact := decodeArtifact(t, body)
	if artifact["name"] != "daily-report" {
		t.Errorf("name: got %v, want daily-report", artifact["name"])
	}
	if artifact["kind"] != compose.KindReportSchedule {
		t.Errorf("kind: got %v, want %s", artifact["kind"], compose.KindReportSchedule)
	}
	if artifact["status"] != compose.StatusPending {
		t.Errorf("status: got %v, want %s", artifact["status"], compose.StatusPending)
	}
	if artifact["current_version"] != float64(1) {
		t.Errorf("current_version: got %v, want 1", artifact["current_version"])
	}
	artifactID := artifact["id"].(string)

	// Create automation_rule
	spec2 := map[string]any{
		"trigger": map[string]any{"type": "schedule", "interval_hours": 1},
		"action":  map[string]any{"type": "send_report", "transport": "telegram", "chat_id": "123456"},
	}
	st, body = f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "hourly-rule",
		"kind": compose.KindAutomationRule,
		"spec": spec2,
	})
	if st != http.StatusCreated {
		t.Fatalf("create rule: got %d, want 201: %s", st, body)
	}

	// List all
	st, body = f.getJSON(t, "/api/compose/artifacts")
	if st != http.StatusOK {
		t.Fatalf("list: got %d, want 200: %s", st, body)
	}
	artifacts := decodeArtifactList(t, body)
	if len(artifacts) != 2 {
		t.Errorf("expected 2 artifacts, got %d", len(artifacts))
	}

	// Filter by kind
	st, body = f.getJSON(t, "/api/compose/artifacts?kind="+compose.KindReportSchedule)
	if st != http.StatusOK {
		t.Fatalf("list by kind: got %d, want 200: %s", st, body)
	}
	artifacts = decodeArtifactList(t, body)
	if len(artifacts) != 1 || artifacts[0]["kind"] != compose.KindReportSchedule {
		t.Errorf("kind filter failed: got %d artifacts", len(artifacts))
	}

	// Filter by status
	st, body = f.getJSON(t, "/api/compose/artifacts?status="+compose.StatusPending)
	if st != http.StatusOK {
		t.Fatalf("list by status: got %d, want 200: %s", st, body)
	}
	artifacts = decodeArtifactList(t, body)
	if len(artifacts) != 2 {
		t.Errorf("status filter failed: got %d artifacts", len(artifacts))
	}

	// Get by ID
	st, body = f.getJSON(t, "/api/compose/artifacts/"+artifactID)
	if st != http.StatusOK {
		t.Fatalf("get by id: got %d, want 200: %s", st, body)
	}
	artifact = decodeArtifact(t, body)
	if artifact["id"] != artifactID {
		t.Errorf("get by id: ID mismatch")
	}
	if artifact["versions"] == nil {
		t.Error("versions should be present in get by id")
	}
}

func TestComposeCreateValidation(t *testing.T) {
	f := newComposeFixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	// Missing name
	st, body := f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"kind": compose.KindReportSchedule,
		"spec": map[string]any{"transport": "telegram", "chat_id": "123", "interval_hours": 24},
	})
	if st != http.StatusBadRequest {
		t.Errorf("missing name: got %d, want 400: %s", st, body)
	}

	// Invalid spec for report_schedule (missing transport)
	st, body = f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "bad",
		"kind": compose.KindReportSchedule,
		"spec": map[string]any{"chat_id": "123", "interval_hours": 24},
	})
	if st != http.StatusBadRequest {
		t.Errorf("invalid spec: got %d, want 400: %s", st, body)
	}

	// Invalid transport
	st, body = f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "bad",
		"kind": compose.KindReportSchedule,
		"spec": map[string]any{"transport": "email", "chat_id": "123", "interval_hours": 24},
	})
	if st != http.StatusBadRequest {
		t.Errorf("invalid transport: got %d, want 400: %s", st, body)
	}

	// Invalid interval_hours
	st, body = f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "bad",
		"kind": compose.KindReportSchedule,
		"spec": map[string]any{"transport": "telegram", "chat_id": "123", "interval_hours": 200},
	})
	if st != http.StatusBadRequest {
		t.Errorf("invalid interval: got %d, want 400: %s", st, body)
	}
}

func TestComposeUpdate(t *testing.T) {
	f := newComposeFixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	// Create artifact
	spec := map[string]any{
		"transport":      "telegram",
		"chat_id":        "123456",
		"interval_hours": 24,
	}
	st, body := f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "test-report",
		"kind": compose.KindReportSchedule,
		"spec": spec,
	})
	if st != http.StatusCreated {
		t.Fatalf("create: got %d, want 201: %s", st, body)
	}
	artifact := decodeArtifact(t, body)
	artifactID := artifact["id"].(string)

	// Update spec (pending -> allowed)
	newSpec := map[string]any{
		"transport":      "telegram",
		"chat_id":        "123456",
		"interval_hours": 12,
	}
	st, body = f.putJSON(t, "/api/compose/artifacts/"+artifactID, map[string]any{"spec": newSpec})
	if st != http.StatusOK {
		t.Fatalf("update: got %d, want 200: %s", st, body)
	}
	artifact = decodeArtifact(t, body)
	if artifact["current_version"] != float64(2) {
		t.Errorf("version after update: got %v, want 2", artifact["current_version"])
	}
	if artifact["spec"].(map[string]any)["interval_hours"] != float64(12) {
		t.Errorf("spec not updated")
	}

	// Activate
	st, body = f.postJSON(t, "/api/compose/artifacts/"+artifactID+"/activate", nil)
	if st != http.StatusOK {
		t.Fatalf("activate: got %d, want 200: %s", st, body)
	}
	artifact = decodeArtifact(t, body)
	if artifact["status"] != compose.StatusActive {
		t.Errorf("status after activate: got %v, want %s", artifact["status"], compose.StatusActive)
	}

	// Update spec while active -> should fail
	st, body = f.putJSON(t, "/api/compose/artifacts/"+artifactID, map[string]any{"spec": newSpec})
	if st != http.StatusConflict {
		t.Errorf("update active: got %d, want 409: %s", st, body)
	}

	// Disable
	st, body = f.postJSON(t, "/api/compose/artifacts/"+artifactID+"/disable", nil)
	if st != http.StatusOK {
		t.Fatalf("disable: got %d, want 200: %s", st, body)
	}
	artifact = decodeArtifact(t, body)
	if artifact["status"] != compose.StatusDisabled {
		t.Errorf("status after disable: got %v, want %s", artifact["status"], compose.StatusDisabled)
	}

	// Update spec while disabled -> allowed
	st, body = f.putJSON(t, "/api/compose/artifacts/"+artifactID, map[string]any{"spec": newSpec})
	if st != http.StatusOK {
		t.Fatalf("update disabled: got %d, want 200: %s", st, body)
	}
	artifact = decodeArtifact(t, body)
	if artifact["current_version"] != float64(3) {
		t.Errorf("version after update disabled: got %v, want 3", artifact["current_version"])
	}
}

func TestComposeActivateDisableGuards(t *testing.T) {
	f := newComposeFixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	spec := map[string]any{
		"transport":      "telegram",
		"chat_id":        "123456",
		"interval_hours": 24,
	}
	st, body := f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "test",
		"kind": compose.KindReportSchedule,
		"spec": spec,
	})
	if st != http.StatusCreated {
		t.Fatalf("create: got %d, want 201: %s", st, body)
	}
	artifact := decodeArtifact(t, body)
	artifactID := artifact["id"].(string)

	// Activate already active -> should fail
	st, body = f.postJSON(t, "/api/compose/artifacts/"+artifactID+"/activate", nil)
	if st != http.StatusOK {
		t.Fatalf("first activate: got %d, want 200: %s", st, body)
	}
	st, body = f.postJSON(t, "/api/compose/artifacts/"+artifactID+"/activate", nil)
	if st != http.StatusConflict {
		t.Errorf("activate active: got %d, want 409: %s", st, body)
	}

	// Disable non-active -> should fail
	st, body = f.postJSON(t, "/api/compose/artifacts/"+artifactID+"/disable", nil)
	if st != http.StatusOK {
		t.Fatalf("first disable: got %d, want 200: %s", st, body)
	}
	st, body = f.postJSON(t, "/api/compose/artifacts/"+artifactID+"/disable", nil)
	if st != http.StatusConflict {
		t.Errorf("disable disabled: got %d, want 409: %s", st, body)
	}
}

func TestComposeRollback(t *testing.T) {
	f := newComposeFixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	spec1 := map[string]any{"transport": "telegram", "chat_id": "123", "interval_hours": 24}
	spec2 := map[string]any{"transport": "telegram", "chat_id": "123", "interval_hours": 12}
	spec3 := map[string]any{"transport": "telegram", "chat_id": "123", "interval_hours": 6}

	st, body := f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "test",
		"kind": compose.KindReportSchedule,
		"spec": spec1,
	})
	if st != http.StatusCreated {
		t.Fatalf("create: got %d, want 201: %s", st, body)
	}
	artifact := decodeArtifact(t, body)
	artifactID := artifact["id"].(string)

	// Update to v2
	st, body = f.putJSON(t, "/api/compose/artifacts/"+artifactID, map[string]any{"spec": spec2})
	if st != http.StatusOK {
		t.Fatalf("update v2: got %d, want 200: %s", st, body)
	}

	// Update to v3
	st, body = f.putJSON(t, "/api/compose/artifacts/"+artifactID, map[string]any{"spec": spec3})
	if st != http.StatusOK {
		t.Fatalf("update v3: got %d, want 200: %s", st, body)
	}

	// Rollback to v1 (artifact must be pending or disabled)
	st, body = f.postJSON(t, "/api/compose/artifacts/"+artifactID+"/rollback", map[string]any{"version": 1})
	if st != http.StatusOK {
		t.Fatalf("rollback: got %d, want 200: %s", st, body)
	}
	artifact = decodeArtifact(t, body)
	if artifact["current_version"] != float64(4) {
		t.Errorf("version after rollback: got %v, want 4", artifact["current_version"])
	}
	if artifact["spec"].(map[string]any)["interval_hours"] != float64(24) {
		t.Errorf("spec after rollback: got %v, want 24", artifact["spec"].(map[string]any)["interval_hours"])
	}

	// Rollback to non-existent version
	st, body = f.postJSON(t, "/api/compose/artifacts/"+artifactID+"/rollback", map[string]any{"version": 999})
	if st != http.StatusNotFound {
		t.Errorf("rollback missing version: got %d, want 404: %s", st, body)
	}

	// Rollback while active -> should fail
	st, body = f.postJSON(t, "/api/compose/artifacts/"+artifactID+"/activate", nil)
	if st != http.StatusOK {
		t.Fatalf("activate: got %d, want 200: %s", st, body)
	}
	st, body = f.postJSON(t, "/api/compose/artifacts/"+artifactID+"/rollback", map[string]any{"version": 1})
	if st != http.StatusConflict {
		t.Errorf("rollback active: got %d, want 409: %s", st, body)
	}
}

func TestComposeDelete(t *testing.T) {
	f := newComposeFixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	spec := map[string]any{
		"transport":      "telegram",
		"chat_id":        "123456",
		"interval_hours": 24,
	}
	st, body := f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "test",
		"kind": compose.KindReportSchedule,
		"spec": spec,
	})
	if st != http.StatusCreated {
		t.Fatalf("create: got %d, want 201: %s", st, body)
	}
	artifact := decodeArtifact(t, body)
	artifactID := artifact["id"].(string)

	// Delete while pending -> allowed
	st, body = f.deleteReq(t, "/api/compose/artifacts/"+artifactID)
	if st != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204: %s", st, body)
	}

	// Verify gone
	st, body = f.getJSON(t, "/api/compose/artifacts/"+artifactID)
	if st != http.StatusNotFound {
		t.Errorf("get after delete: got %d, want 404: %s", st, body)
	}

	// Delete non-existent
	st, body = f.deleteReq(t, "/api/compose/artifacts/"+artifactID)
	if st != http.StatusNotFound {
		t.Errorf("delete missing: got %d, want 404: %s", st, body)
	}

	// Create and activate, then delete -> should fail
	st, body = f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "test2",
		"kind": compose.KindReportSchedule,
		"spec": spec,
	})
	if st != http.StatusCreated {
		t.Fatalf("create2: got %d, want 201: %s", st, body)
	}
	artifact = decodeArtifact(t, body)
	artifactID = artifact["id"].(string)

	st, body = f.postJSON(t, "/api/compose/artifacts/"+artifactID+"/activate", nil)
	if st != http.StatusOK {
		t.Fatalf("activate: got %d, want 200: %s", st, body)
	}

	st, body = f.deleteReq(t, "/api/compose/artifacts/"+artifactID)
	if st != http.StatusConflict {
		t.Errorf("delete active: got %d, want 409: %s", st, body)
	}
}

func TestComposeNotFound(t *testing.T) {
	f := newComposeFixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	// Get non-existent
	st, body := f.getJSON(t, "/api/compose/artifacts/nonexistent")
	if st != http.StatusNotFound {
		t.Errorf("get missing: got %d, want 404: %s", st, body)
	}

	// Update non-existent
	st, body = f.putJSON(t, "/api/compose/artifacts/nonexistent", map[string]any{"spec": map[string]any{}})
	if st != http.StatusNotFound {
		t.Errorf("update missing: got %d, want 404: %s", st, body)
	}

	// Delete non-existent
	st, body = f.deleteReq(t, "/api/compose/artifacts/nonexistent")
	if st != http.StatusNotFound {
		t.Errorf("delete missing: got %d, want 404: %s", st, body)
	}

	// Activate non-existent
	st, body = f.postJSON(t, "/api/compose/artifacts/nonexistent/activate", nil)
	if st != http.StatusNotFound {
		t.Errorf("activate missing: got %d, want 404: %s", st, body)
	}

	// Disable non-existent
	st, body = f.postJSON(t, "/api/compose/artifacts/nonexistent/disable", nil)
	if st != http.StatusNotFound {
		t.Errorf("disable missing: got %d, want 404: %s", st, body)
	}

	// Rollback non-existent
	st, body = f.postJSON(t, "/api/compose/artifacts/nonexistent/rollback", map[string]any{"version": 1})
	if st != http.StatusNotFound {
		t.Errorf("rollback missing: got %d, want 404: %s", st, body)
	}
}

func TestComposeMethodNotAllowed(t *testing.T) {
	f := newComposeFixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	// POST to /api/compose/artifacts/{id} (no sub-route)
	spec := map[string]any{"transport": "telegram", "chat_id": "123", "interval_hours": 24}
	st, body := f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "test", "kind": compose.KindReportSchedule, "spec": spec,
	})
	if st != http.StatusCreated {
		t.Fatalf("create: got %d, want 201: %s", st, body)
	}
	var artifact map[string]any
	json.Unmarshal(body, &artifact)
	artifactID := artifact["artifact"].(map[string]any)["id"].(string)

	// POST to artifact by ID (not a sub-route)
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/api/compose/artifacts/"+artifactID, nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST to artifact ID: got %d, want 405", resp.StatusCode)
	}
	resp.Body.Close()

	// GET to /activate
	req, _ = http.NewRequest(http.MethodGet, f.ts.URL+"/api/compose/artifacts/"+artifactID+"/activate", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET to activate: got %d, want 405", resp.StatusCode)
	}
	resp.Body.Close()

	// PUT to /disable
	req, _ = http.NewRequest(http.MethodPut, f.ts.URL+"/api/compose/artifacts/"+artifactID+"/disable", nil)
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("PUT to disable: got %d, want 405", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestComposeUnauthorized(t *testing.T) {
	f := newComposeFixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	// No token
	req, _ := http.NewRequest(http.MethodGet, f.ts.URL+"/api/compose/artifacts", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Invalid token
	req, _ = http.NewRequest(http.MethodGet, f.ts.URL+"/api/compose/artifacts", nil)
	req.Header.Set("Authorization", "Bearer invalid")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("invalid token: got %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestComposeRecipeAndToolChain(t *testing.T) {
	f := newComposeFixture(t, filepath.Join(t.TempDir(), "test.db"))
	defer f.close()

	// Create recipe
	recipeSpec := map[string]any{
		"steps": []map[string]any{
			{"tool": "tool1", "args": map[string]any{"param": "value"}},
			{"tool": "tool2", "args": map[string]any{"flag": true}},
		},
	}
	st, body := f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "my-recipe",
		"kind": compose.KindRecipe,
		"spec": recipeSpec,
	})
	if st != http.StatusCreated {
		t.Fatalf("create recipe: got %d, want 201: %s", st, body)
	}
	artifact := decodeArtifact(t, body)
	if artifact["kind"] != compose.KindRecipe {
		t.Errorf("kind: got %v, want %s", artifact["kind"], compose.KindRecipe)
	}

	// Create tool_chain
	chainSpec := map[string]any{
		"chain": []string{"tool1", "tool2", "tool3"},
	}
	st, body = f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "my-chain",
		"kind": compose.KindToolChain,
		"spec": chainSpec,
	})
	if st != http.StatusCreated {
		t.Fatalf("create tool_chain: got %d, want 201: %s", st, body)
	}
	artifact = decodeArtifact(t, body)
	if artifact["kind"] != compose.KindToolChain {
		t.Errorf("kind: got %v, want %s", artifact["kind"], compose.KindToolChain)
	}

	// Invalid recipe (missing steps)
	st, body = f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "bad-recipe",
		"kind": compose.KindRecipe,
		"spec": map[string]any{},
	})
	if st != http.StatusBadRequest {
		t.Errorf("invalid recipe: got %d, want 400: %s", st, body)
	}

	// Invalid tool_chain (empty chain)
	st, body = f.postJSON(t, "/api/compose/artifacts", map[string]any{
		"name": "bad-chain",
		"kind": compose.KindToolChain,
		"spec": map[string]any{"chain": []string{}},
	})
	if st != http.StatusBadRequest {
		t.Errorf("invalid tool_chain: got %d, want 400: %s", st, body)
	}
}
