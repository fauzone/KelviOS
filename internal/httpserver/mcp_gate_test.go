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
	mcpGateTestUser = "admin"
	mcpGateTestPass = "adminpassword123"
)

// newMCPGateTestServer creates a test server with MCP DISABLED by default
// (does NOT use newMCPTestServer which enables MCP).
// Returns the httptest.Server, a valid bearer token, and the database.
func newMCPGateTestServer(t *testing.T, dbPath string) (*httptest.Server, string, *sql.DB) {
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
	st, _ := postJSON(t, ts.URL+"/api/auth/setup", map[string]string{"username": mcpGateTestUser, "password": mcpGateTestPass}, "")
	if st != http.StatusCreated {
		t.Fatalf("setup: got %d, want 201", st)
	}
	// Login to get token
	st, body := postJSON(t, ts.URL+"/api/auth/login", map[string]string{"username": mcpGateTestUser, "password": mcpGateTestPass}, "")
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

// mgMcpRequest sends a JSON-RPC request to /mcp and returns the status code and parsed response.
func mgMcpRequest(t *testing.T, ts *httptest.Server, token string, payload any) (int, map[string]any) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", body)
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
	var out map[string]any
	if len(b) > 0 {
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("decode response: %v (body: %s)", err, string(b))
		}
	}
	return resp.StatusCode, out
}

// TestMCPGateDisabledByDefault tests that MCP returns 503 mcp_disabled when external_mcp is OFF.
func TestMCPGateDisabledByDefault(t *testing.T) {
	ts, token, _ := newMCPGateTestServer(t, ":memory:")

	// POST /mcp initialize with token → 503 and body {"error":"mcp_disabled"}
	st, body := mgMcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	})
	if st != http.StatusServiceUnavailable {
		t.Fatalf("MCP with external_mcp=OFF: got %d, want 503; body: %v", st, body)
	}
	errMsg, _ := body["error"].(string)
	if errMsg != "mcp_disabled" {
		t.Errorf("error = %q, want 'mcp_disabled'", errMsg)
	}
}

// TestMCPGateEnabledAfterModesUpdate tests that MCP works after enabling external_mcp via PUT /api/agent-modes.
func TestMCPGateEnabledAfterModesUpdate(t *testing.T) {
	ts, token, db := newMCPGateTestServer(t, ":memory:")

	// First verify MCP is disabled
	st, body := mgMcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	})
	if st != http.StatusServiceUnavailable {
		t.Fatalf("initial MCP: got %d, want 503; body: %v", st, body)
	}
	if body["error"] != "mcp_disabled" {
		t.Errorf("initial error = %v, want mcp_disabled", body["error"])
	}

	// Enable MCP via PUT /api/agent-modes (resident_harness=true, external_mcp=true)
	st, bodyBytes := amDoReq(t, ts, http.MethodPut, "/api/agent-modes", map[string]any{
		"resident_harness": true,
		"external_mcp":     true,
	}, token)
	if st != http.StatusOK {
		t.Fatalf("PUT agent-modes: got %d, want 200; body: %s", st, bodyBytes)
	}
	var modes agentmodes.Modes
	if err := json.Unmarshal(bodyBytes, &modes); err != nil {
		t.Fatalf("decode modes response: %v", err)
	}
	if modes.Preset != agentmodes.PresetBoth {
		t.Errorf("preset after enable = %q, want %q", modes.Preset, agentmodes.PresetBoth)
	}

	// Verify DB state
	dbModes, err := agentmodes.GetModes(db)
	if err != nil {
		t.Fatalf("GetModes from DB: %v", err)
	}
	if !dbModes.ExternalMCP {
		t.Errorf("DB ExternalMCP = false, want true")
	}

	// Now MCP should work → 200 with protocolVersion "2025-03-26"
	st, body = mgMcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "initialize",
	})
	if st != http.StatusOK {
		t.Fatalf("MCP after enable: got %d, want 200; body: %v", st, body)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("MCP response missing result object: %v", body)
	}
	protoVer, _ := result["protocolVersion"].(string)
	if protoVer != "2025-03-26" {
		t.Errorf("protocolVersion = %q, want '2025-03-26'", protoVer)
	}
}

// TestMCPGateWithoutToken tests that /mcp requires a valid Bearer token even when enabled.
func TestMCPGateWithoutToken(t *testing.T) {
	ts, token, db := newMCPGateTestServer(t, ":memory:")
	_ = db // used in other tests

	// Enable MCP first
	st, _ := amDoReq(t, ts, http.MethodPut, "/api/agent-modes", map[string]any{
		"resident_harness": true,
		"external_mcp":     true,
	}, token)
	if st != http.StatusOK {
		t.Fatalf("PUT agent-modes: got %d, want 200", st)
	}

	// Without token → 401
	st, body := mgMcpRequest(t, ts, "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	})
	if st != http.StatusUnauthorized {
		t.Fatalf("MCP no token: got %d, want 401; body: %v", st, body)
	}
	if body["error"] != "unauthorized" {
		t.Errorf("unauthorized error = %v, want 'unauthorized'", body["error"])
	}

	// With valid token → 200
	st, body = mgMcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "initialize",
	})
	if st != http.StatusOK {
		t.Fatalf("MCP with token: got %d, want 200; body: %v", st, body)
	}
	result, _ := body["result"].(map[string]any)
	if result["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want '2025-03-26'", result["protocolVersion"])
	}
}

// TestMCPGateResidentHarnessIndependent tests that resident_harness switch
// does not affect MCP gate (only external_mcp matters).
func TestMCPGateResidentHarnessIndependent(t *testing.T) {
	ts, token, _ := newMCPGateTestServer(t, ":memory:")

	// Enable MCP but keep resident_harness OFF
	st, _ := amDoReq(t, ts, http.MethodPut, "/api/agent-modes", map[string]any{
		"resident_harness": false,
		"external_mcp":     true,
	}, token)
	if st != http.StatusOK {
		t.Fatalf("PUT agent-modes: got %d, want 200", st)
	}

	// MCP should work (external_mcp=true)
	st, body := mgMcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	})
	if st != http.StatusOK {
		t.Fatalf("MCP with external_mcp=true, resident_harness=false: got %d, want 200; body: %v", st, body)
	}
	result, _ := body["result"].(map[string]any)
	if result["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want '2025-03-26'", result["protocolVersion"])
	}

	// Now disable MCP but keep resident_harness ON
	st, _ = amDoReq(t, ts, http.MethodPut, "/api/agent-modes", map[string]any{
		"resident_harness": true,
		"external_mcp":     false,
	}, token)
	if st != http.StatusOK {
		t.Fatalf("PUT agent-modes: got %d, want 200", st)
	}

	// MCP should be disabled (external_mcp=false)
	st, body = mgMcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "initialize",
	})
	if st != http.StatusServiceUnavailable {
		t.Fatalf("MCP with external_mcp=false, resident_harness=true: got %d, want 503; body: %v", st, body)
	}
	if body["error"] != "mcp_disabled" {
		t.Errorf("error = %v, want 'mcp_disabled'", body["error"])
	}
}