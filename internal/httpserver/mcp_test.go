package httpserver

// KelviOS Fase 1 WS45-B: MCP endpoint tests (FR-3.4 slice 2).
// Tests the JSON-RPC 2.0 over HTTP endpoint at POST /mcp behind bearer auth.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/agentmodes"
	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

const (
	mcpTestUser = "admin"
	mcpTestPass = "adminpassword123"
)

// newMCPTestServer creates a test server with MCP enabled and returns the
// httptest.Server and a valid bearer token for the admin user.
func newMCPTestServer(t *testing.T, dbPath string) (*httptest.Server, string, *sql.DB) {
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

	// Enable MCP (ExternalMCP = true) for MCP tests
	if err := agentmodes.SetModes(db, &agentmodes.Modes{
		ResidentHarness: true,
		ExternalMCP:     true,
	}); err != nil {
		t.Fatalf("enable MCP: %v", err)
	}

	// Setup admin user
	st, _ := postJSON(t, ts.URL+"/api/auth/setup", map[string]string{"username": mcpTestUser, "password": mcpTestPass}, "")
	if st != http.StatusCreated {
		t.Fatalf("setup: got %d, want 201", st)
	}
	// Login to get token
	st, body := postJSON(t, ts.URL+"/api/auth/login", map[string]string{"username": mcpTestUser, "password": mcpTestPass}, "")
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

// mcpRequest sends a JSON-RPC request to /mcp and returns the status code and parsed response.
func mcpRequest(t *testing.T, ts *httptest.Server, token string, payload any) (int, map[string]any) {
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

// TestMCPAuthGate tests that /mcp requires a valid Bearer token (REGRESSION for WS45-B live pass finding).
func TestMCPAuthGate(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")

	// Without token → 401
	st, body := mcpRequest(t, ts, "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	})
	if st != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401; body: %v", st, body)
	}
	if body["error"] != "unauthorized" {
		t.Errorf("unauthorized body: got %v, want error=unauthorized", body)
	}

	// With valid token → 200
	st, body = mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	})
	if st != http.StatusOK {
		t.Fatalf("with token: got %d, want 200; body: %v", st, body)
	}
	if body["result"] == nil {
		t.Errorf("initialize with token: missing result: %v", body)
	}
}

// TestMCPInitialize tests the initialize method returns correct protocol version and server info.
func TestMCPInitialize(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")

	st, body := mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	})
	if st != http.StatusOK {
		t.Fatalf("initialize: got %d, want 200; body: %v", st, body)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize: missing result object: %v", body)
	}
	if result["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion: got %v, want 2025-03-26", result["protocolVersion"])
	}
	serverInfo, ok := result["serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("serverInfo missing: %v", result)
	}
	if serverInfo["name"] != "kelvios" {
		t.Errorf("serverInfo.name: got %v, want kelvios", serverInfo["name"])
	}
	if body["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc field: got %v, want 2.0", body["jsonrpc"])
	}
	if body["id"] != float64(1) {
		t.Errorf("id field: got %v, want 1", body["id"])
	}
}

// TestMCPNotificationsInitialized tests that notifications/initialized returns 202 with empty body.
func TestMCPNotificationsInitialized(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")

	st, body := mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	if st != http.StatusAccepted {
		t.Fatalf("notifications/initialized: got %d, want 202; body: %v", st, body)
	}
	// No response body beyond 202 (JSON-RPC notification has no id, no response)
	if len(body) > 0 {
		t.Errorf("notifications/initialized: expected empty body, got %v", body)
	}
}

// TestMCPToolsList tests that tools/list returns all 12 v1 tools by exact name.
func TestMCPToolsList(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")

	st, body := mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})
	if st != http.StatusOK {
		t.Fatalf("tools/list: got %d, want 200; body: %v", st, body)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list: missing result object: %v", body)
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list: missing tools array: %v", result)
	}
	if len(tools) != 12 {
		t.Fatalf("tools/list: expected 12 tools, got %d", len(tools))
	}
	expected := []string{
		"list_pools",
		"list_disks",
		"list_collections",
		"get_maintenance_status",
		"list_backup_repos",
		"list_restore_test_alerts",
		"wake_disk",
		"sleep_disk",
		"delete_pool",
		"compose_propose",
		"compose_list",
		"compose_get",
	}
	seen := make(map[string]bool)
	for _, tAny := range tools {
		tool, ok := tAny.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tool["name"].(string)
		seen[name] = true
	}
	for _, exp := range expected {
		if !seen[exp] {
			t.Errorf("tools/list: missing expected tool %q", exp)
		}
	}
}

// TestMCPToolsCallRead tests that a read tool (list_pools) executes and returns pools JSON.
func TestMCPToolsCallRead(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")

	st, body := mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "list_pools",
			"arguments": map[string]any{},
		},
	})
	if st != http.StatusOK {
		t.Fatalf("tools/call list_pools: got %d, want 200; body: %v", st, body)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call list_pools: missing result object: %v", body)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("tools/call list_pools: missing content array: %v", result)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("tools/call list_pools: content[0] not object: %v", content[0])
	}
	if first["type"] != "text" {
		t.Errorf("content type: got %v, want text", first["type"])
	}
	// The text field should contain JSON with pools array
	text, _ := first["text"].(string)
	if !strings.Contains(text, "pools") {
		t.Errorf("content text missing pools: %s", text)
	}
}

// TestMCPToolsCallDestructiveRefused tests that a destructive tool (delete_pool) is refused
// with JSON-RPC error -32001 and an audit row is written.
func TestMCPToolsCallDestructiveRefused(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")

	st, body := mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "delete_pool",
			"arguments": map[string]any{"pool_id": "pool-nonexistent"},
		},
	})
	if st != http.StatusOK {
		t.Fatalf("tools/call delete_pool: got %d, want 200 (JSON-RPC error in body); body: %v", st, body)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call delete_pool: missing error object: %v", body)
	}
	if errObj["code"] != float64(-32001) {
		t.Errorf("error code: got %v, want -32001", errObj["code"])
	}
	msg, _ := errObj["message"].(string)
	if msg != "approval required" {
		t.Errorf("error message: got %q, want 'approval required'", msg)
	}
	data, _ := errObj["data"].(map[string]any)
	// FR-3.7 contract: the refusal carries a pending approval request.
	if data == nil || data["status"] != "pending" {
		t.Errorf("error data: got %v, want status=pending", data)
	}
	approvalID, _ := data["approval_id"].(string)
	if approvalID == "" {
		t.Fatalf("error data: missing approval_id: %v", data)
	}

	// The handler must NOT have run: a pending approval request exists for
	// delete_pool and no pool was deleted (the id never existed).
	var pending int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM mcp_approvals
		WHERE id = ? AND tool = ? AND status = ?
	`, approvalID, "delete_pool", "pending").Scan(&pending)
	if err != nil {
		t.Fatalf("query mcp_approvals: %v", err)
	}
	if pending != 1 {
		t.Errorf("pending approval request: expected 1, got %d", pending)
	}

	// Verify audit row exists for the approval request creation.
	var count int
	err = db.QueryRow(`
		SELECT COUNT(*) FROM audit_log
		WHERE target_type = ? AND target_id = ? AND result = ?
	`, "mcp_approval", approvalID, "pending").Scan(&count)
	if err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	if count != 1 {
		t.Errorf("audit row for approval request: expected 1, got %d", count)
	}
}

// TestMCPUnknownMethod tests that an unknown method returns JSON-RPC error -32601.
func TestMCPUnknownMethod(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")

	st, body := mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "unknown/method",
	})
	if st != http.StatusOK {
		t.Fatalf("unknown method: got %d, want 200 (JSON-RPC error in body); body: %v", st, body)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("unknown method: missing error object: %v", body)
	}
	if errObj["code"] != float64(-32601) {
		t.Errorf("error code: got %v, want -32601", errObj["code"])
	}
}

// TestMCPMalformedJSON tests that malformed JSON body returns JSON-RPC error -32700.
func TestMCPMalformedJSON(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")

	// Send malformed JSON
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader([]byte("{not valid json")))
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
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, string(b))
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("malformed JSON: got %d, want 200 (JSON-RPC error in body)", resp.StatusCode)
	}
	errObj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("malformed JSON: missing error object: %v", out)
	}
	if errObj["code"] != float64(-32700) {
		t.Errorf("error code: got %v, want -32700", errObj["code"])
	}
}

// TestMCPInvalidJSONRPCVersion tests that jsonrpc != "2.0" returns invalid-request error.
func TestMCPInvalidJSONRPCVersion(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")

	st, body := mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "1.0",
		"id":      1,
		"method":  "initialize",
	})
	if st != http.StatusOK {
		t.Fatalf("invalid jsonrpc: got %d, want 200 (JSON-RPC error in body); body: %v", st, body)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("invalid jsonrpc: missing error object: %v", body)
	}
	if errObj["code"] != float64(-32600) {
		t.Errorf("error code: got %v, want -32600 (invalid request)", errObj["code"])
	}
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "jsonrpc must be 2.0") {
		t.Errorf("error message: got %q, want contains 'jsonrpc must be 2.0'", msg)
	}
}

// TestMCPDroppedToolsAbsent tests that run_backup and run_restore_test are
// ABSENT from the v1 tool list (dropped per the production brief drop-rule:
// a tool whose service call is not cleanly reachable must be dropped, not
// stubbed — Chief decision 5 Nov, replacing the session's not-implemented
// stubs). tools/call on either name returns "tool not found".
func TestMCPDroppedToolsAbsent(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")

	st, body := mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})
	if st != http.StatusOK {
		t.Fatalf("tools/list: got %d, want 200; body: %v", st, body)
	}
	result, _ := body["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != 12 {
		t.Fatalf("tools/list: expected 12 tools, got %d", len(tools))
	}
	for _, tAny := range tools {
		tool, _ := tAny.(map[string]any)
		name, _ := tool["name"].(string)
		if name == "run_backup" || name == "run_restore_test" {
			t.Errorf("tools/list: dropped tool %q still present", name)
		}
	}
	for _, name := range []string{"run_backup", "run_restore_test"} {
		st, body := mcpRequest(t, ts, token, map[string]any{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "tools/call",
			"params": map[string]any{
				"name":      name,
				"arguments": map[string]any{},
			},
		})
		if st != http.StatusOK {
			t.Fatalf("tools/call %s: got %d, want 200 (JSON-RPC error in body)", name, st)
		}
		errObj, _ := body["error"].(map[string]any)
		if errObj == nil {
			t.Fatalf("tools/call %s: missing error object: %v", name, body)
		}
		msg, _ := errObj["message"].(string)
		if errObj["code"] != float64(-32601) || msg != "tool not found: "+name {
			t.Errorf("tools/call %s: got code %v msg %q, want -32601 'tool not found: %s'", name, errObj["code"], msg, name)
		}
	}
}
