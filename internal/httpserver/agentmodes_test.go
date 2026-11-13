package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/agentmodes"
	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

const (
	agentModesTestUser = "admin"
	agentModesTestPass = "adminpassword123"
)

// newAgentModesTestServer creates a test server with admin user setup and returns
// the httptest.Server, a valid bearer token, and the database.
func newAgentModesTestServer(t *testing.T, dbPath string) (*httptest.Server, string, *sql.DB) {
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
	t.Cleanup(func() {
		ts.Close()
		s.Shutdown(context.Background())
		db.Close()
	})

	// Setup admin user
	st, _ := postJSON(t, ts.URL+"/api/auth/setup", map[string]string{"username": agentModesTestUser, "password": agentModesTestPass}, "")
	if st != http.StatusCreated {
		t.Fatalf("setup: got %d, want 201", st)
	}
	// Login to get token
	st, body := postJSON(t, ts.URL+"/api/auth/login", map[string]string{"username": agentModesTestUser, "password": agentModesTestPass}, "")
	if st != http.StatusOK {
		t.Fatalf("login: got %d, want 200", st)
	}
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &lr); err != nil || lr.Token == "" {
		t.Fatalf("login response missing token: %v %s", err, body)
	}
	return ts, lr.Token, db
}

// amDoReq makes an HTTP request with the given method, path, payload, and token.
func amDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

// TestAgentModesAuthGate tests that /api/agent-modes requires a valid Bearer token.
func TestAgentModesAuthGate(t *testing.T) {
	ts, token, _ := newAgentModesTestServer(t, ":memory:")

	// GET without token → 401
	st, _ := amDoReq(t, ts, http.MethodGet, "/api/agent-modes", nil, "")
	if st != http.StatusUnauthorized {
		t.Fatalf("GET no token: got %d, want 401", st)
	}

	// PUT without token → 401
	st, _ = amDoReq(t, ts, http.MethodPut, "/api/agent-modes", map[string]any{"resident_harness": true, "external_mcp": false}, "")
	if st != http.StatusUnauthorized {
		t.Fatalf("PUT no token: got %d, want 401", st)
	}

	// GET with valid token → 200
	st, body := amDoReq(t, ts, http.MethodGet, "/api/agent-modes", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET with token: got %d, want 200; body: %s", st, body)
	}
	var modes agentmodes.Modes
	if err := json.Unmarshal(body, &modes); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	// Default from migration v36: harness=true, mcp=false, preset=agent_inside
	if !modes.ResidentHarness {
		t.Errorf("default ResidentHarness = false, want true")
	}
	if modes.ExternalMCP {
		t.Errorf("default ExternalMCP = true, want false")
	}
	if modes.Preset != agentmodes.PresetAgentInside {
		t.Errorf("default Preset = %q, want %q", modes.Preset, agentmodes.PresetAgentInside)
	}
}

// TestAgentModesPUT tests PUT /api/agent-modes with the 4 switch combinations.
func TestAgentModesPUT(t *testing.T) {
	ts, token, db := newAgentModesTestServer(t, ":memory:")

	tests := []struct {
		name           string
		harness        bool
		mcp            bool
		wantPreset     string
	}{
		{"agent_inside", true, false, agentmodes.PresetAgentInside},
		{"managed_outside", false, true, agentmodes.PresetManagedOutside},
		{"both", true, true, agentmodes.PresetBoth},
		{"no_agent", false, false, agentmodes.PresetNoAgent},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, body := amDoReq(t, ts, http.MethodPut, "/api/agent-modes", map[string]any{
				"resident_harness": tc.harness,
				"external_mcp":     tc.mcp,
			}, token)
			if st != http.StatusOK {
				t.Fatalf("PUT %s: got %d, want 200; body: %s", tc.name, st, body)
			}
			var modes agentmodes.Modes
			if err := json.Unmarshal(body, &modes); err != nil {
				t.Fatalf("decode PUT response: %v", err)
			}
			if modes.ResidentHarness != tc.harness {
				t.Errorf("ResidentHarness = %v, want %v", modes.ResidentHarness, tc.harness)
			}
			if modes.ExternalMCP != tc.mcp {
				t.Errorf("ExternalMCP = %v, want %v", modes.ExternalMCP, tc.mcp)
			}
			if modes.Preset != tc.wantPreset {
				t.Errorf("Preset = %q, want %q", modes.Preset, tc.wantPreset)
			}

			// Verify DB state
			dbModes, err := agentmodes.GetModes(db)
			if err != nil {
				t.Fatalf("GetModes from DB: %v", err)
			}
			if dbModes.ResidentHarness != tc.harness || dbModes.ExternalMCP != tc.mcp || dbModes.Preset != tc.wantPreset {
				t.Errorf("DB state mismatch: got %+v, want harness=%v mcp=%v preset=%q", dbModes, tc.harness, tc.mcp, tc.wantPreset)
			}
		})
	}
}

// TestAgentModesPUTIgnoresPresetField tests that the "preset" field in PUT body is ignored.
func TestAgentModesPUTIgnoresPresetField(t *testing.T) {
	ts, token, db := newAgentModesTestServer(t, ":memory:")

	// Send PUT with preset field that contradicts the switches
	// switches: harness=true, mcp=false → should be agent_inside
	// but preset field says "both"
	st, body := amDoReq(t, ts, http.MethodPut, "/api/agent-modes", map[string]any{
		"resident_harness": true,
		"external_mcp":     false,
		"preset":           "both", // should be ignored
	}, token)
	if st != http.StatusOK {
		t.Fatalf("PUT with preset field: got %d, want 200; body: %s", st, body)
	}
	var modes agentmodes.Modes
	if err := json.Unmarshal(body, &modes); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if modes.Preset != agentmodes.PresetAgentInside {
		t.Errorf("Preset = %q, want %q (preset field must be ignored)", modes.Preset, agentmodes.PresetAgentInside)
	}

	// Verify DB state
	dbModes, err := agentmodes.GetModes(db)
	if err != nil {
		t.Fatalf("GetModes from DB: %v", err)
	}
	if dbModes.Preset != agentmodes.PresetAgentInside {
		t.Errorf("DB Preset = %q, want %q", dbModes.Preset, agentmodes.PresetAgentInside)
	}
}

// TestAgentModesPUTMalformedJSON tests that malformed JSON body returns 400.
func TestAgentModesPUTMalformedJSON(t *testing.T) {
	ts, token, _ := newAgentModesTestServer(t, ":memory:")

	// Send malformed JSON
	req, err := http.NewRequest(http.MethodPut, ts.URL+"/api/agent-modes", bytes.NewReader([]byte("{not valid json")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed JSON: got %d, want 400; body: %s", resp.StatusCode, b)
	}
	var errResp map[string]string
	if err := json.Unmarshal(b, &errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp["error"] != "invalid JSON" {
		t.Errorf("error message = %q, want 'invalid JSON'", errResp["error"])
	}
}

// TestAgentModesAuditLog tests that a successful PUT writes an audit log entry.
func TestAgentModesAuditLog(t *testing.T) {
	ts, token, db := newAgentModesTestServer(t, ":memory:")

	// Perform a PUT
	st, _ := amDoReq(t, ts, http.MethodPut, "/api/agent-modes", map[string]any{
		"resident_harness": true,
		"external_mcp":     true,
	}, token)
	if st != http.StatusOK {
		t.Fatalf("PUT: got %d, want 200", st)
	}

	// Check audit log for agent_modes.update action
	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM audit_log
		WHERE action = ? AND result = ?
	`, "agent_modes.update", "ok").Scan(&count)
	if err != nil {
		t.Fatalf("query audit_log: %v", err)
	}
	if count != 1 {
		t.Errorf("audit_log count for agent_modes.update = %d, want 1", count)
	}

	// Verify detail contains the modes
	var detail string
	err = db.QueryRow(`
		SELECT detail FROM audit_log
		WHERE action = ? AND result = ?
		ORDER BY created_at DESC LIMIT 1
	`, "agent_modes.update", "ok").Scan(&detail)
	if err != nil {
		t.Fatalf("query audit_log detail: %v", err)
	}
	if detail == "" {
		t.Error("audit_log detail is empty")
	}
	// Detail should contain harness=true,mcp=true,preset=both
	expected := "harness=true,mcp=true,preset=both"
	if detail != expected {
		t.Errorf("audit_log detail = %q, want %q", detail, expected)
	}
}

// TestHarnessWrapperGate tests the harnessWrapper gate (WS51-A, FR-3.9).
// When resident_harness is OFF, Run returns fixed text without calling inner harness.
// When resident_harness is ON, inner harness is called and result is passed through.
func TestHarnessWrapperGate(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Delete migration default row to control modes precisely
	if _, err := db.Exec(`DELETE FROM agent_modes`); err != nil {
		t.Fatalf("delete agent_modes: %v", err)
	}

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
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	s := New(cfg, db, "test-version")
	s.SetDriver(storage.NewSingleDriver(disks))
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }

	// Replace the harness with a stub that records calls
	stub := &testStubHarness{
		result: "stub-result",
		err:    nil,
	}
	s.harness = &harnessWrapper{server: s, inner: stub}

	// Test 1: resident_harness = false → Run returns fixed text, stub NOT called
	if err := agentmodes.SetModes(db, &agentmodes.Modes{ResidentHarness: false, ExternalMCP: false}); err != nil {
		t.Fatalf("SetModes OFF: %v", err)
	}

	ctx := context.Background()
	result, err := s.harness.Run(ctx, "test prompt", nil)
	if err != nil {
		t.Fatalf("Run with harness OFF returned error: %v", err)
	}
	expected := "Resident harness is disabled in agent setup modes."
	if result != expected {
		t.Errorf("Run with harness OFF = %q, want %q", result, expected)
	}
	if stub.called {
		t.Error("stub harness was called when resident_harness=false (should not be called)")
	}

	// Test 2: resident_harness = true → Run calls stub and returns its result
	if err := agentmodes.SetModes(db, &agentmodes.Modes{ResidentHarness: true, ExternalMCP: false}); err != nil {
		t.Fatalf("SetModes ON: %v", err)
	}

	stub.called = false
	result, err = s.harness.Run(ctx, "test prompt", nil)
	if err != nil {
		t.Fatalf("Run with harness ON returned error: %v", err)
	}
	if result != "stub-result" {
		t.Errorf("Run with harness ON = %q, want %q", result, "stub-result")
	}
	if !stub.called {
		t.Error("stub harness was NOT called when resident_harness=true (should be called)")
	}
}

// testStubHarness implements harness.Harness for testing.
type testStubHarness struct {
	called bool
	result string
	err    error
}

func (s *testStubHarness) Run(ctx context.Context, prompt string, tools []string) (string, error) {
	s.called = true
	return s.result, s.err
}
