package httpserver

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/fauzone/kelvios/internal/compose"
)

// TestMCPComposeProposeCreatesPending tests that compose_propose creates a pending v1 artifact
// and compose_get returns it with correct created_by and versions.
func TestMCPComposeProposeCreatesPending(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")
	defer func() {
		ts.Close()
		db.Close()
	}()

	// Propose a valid report_schedule artifact
	spec := map[string]any{
		"transport":      "telegram",
		"chat_id":        "123",
		"interval_hours": 24,
	}
	st, body := mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "compose_propose",
			"arguments": map[string]any{
				"name": "daily-report",
				"kind": compose.KindReportSchedule,
				"spec": spec,
			},
		},
	})
	if st != http.StatusOK {
		t.Fatalf("compose_propose: got %d, want 200; body: %v", st, body)
	}

	// Parse MCP result: result.content[0].text contains JSON from handler
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("compose_propose: missing result object: %v", body)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("compose_propose: missing content array: %v", result)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("compose_propose: content[0] not object: %v", content[0])
	}
	text, _ := first["text"].(string)
	var proposeResult map[string]any
	if err := json.Unmarshal([]byte(text), &proposeResult); err != nil {
		t.Fatalf("compose_propose: unmarshal handler result: %v (text: %s)", err, text)
	}

	// Verify propose result
	if proposeResult["status"] != "pending" {
		t.Errorf("propose status: got %v, want pending", proposeResult["status"])
	}
	if proposeResult["current_version"] != float64(1) {
		t.Errorf("propose current_version: got %v, want 1", proposeResult["current_version"])
	}
	id, _ := proposeResult["id"].(string)
	if id == "" || len(id) < 4 || id[:4] != "cmp-" {
		t.Errorf("propose id: got %q, want prefix cmp-", id)
	}

	// Now call compose_get to verify full artifact
	st, body = mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "compose_get",
			"arguments": map[string]any{
				"id": id,
			},
		},
	})
	if st != http.StatusOK {
		t.Fatalf("compose_get: got %d, want 200; body: %v", st, body)
	}

	result, ok = body["result"].(map[string]any)
	if !ok {
		t.Fatalf("compose_get: missing result object: %v", body)
	}
	content, ok = result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("compose_get: missing content array: %v", result)
	}
	first, ok = content[0].(map[string]any)
	if !ok {
		t.Fatalf("compose_get: content[0] not object: %v", content[0])
	}
	text, _ = first["text"].(string)
	var getResult map[string]any
	if err := json.Unmarshal([]byte(text), &getResult); err != nil {
		t.Fatalf("compose_get: unmarshal handler result: %v (text: %s)", err, text)
	}

	artifact, ok := getResult["artifact"].(map[string]any)
	if !ok {
		t.Fatalf("compose_get: missing artifact object: %v", getResult)
	}

	// Verify created_by == "admin" (mcpTestUser)
	if artifact["created_by"] != "admin" {
		t.Errorf("artifact created_by: got %v, want admin", artifact["created_by"])
	}
	if artifact["status"] != "pending" {
		t.Errorf("artifact status: got %v, want pending", artifact["status"])
	}

	versions, ok := artifact["versions"].([]any)
	if !ok || len(versions) != 1 {
		t.Errorf("artifact versions: expected 1, got %d", len(versions))
	}
	if len(versions) == 1 {
		v, _ := versions[0].(map[string]any)
		if v["version"] != float64(1) {
			t.Errorf("version[0].version: got %v, want 1", v["version"])
		}
	}
}

// TestMCPComposeProposeIgnoresStatus tests that compose_propose ignores status field
// in arguments and always creates pending artifact.
func TestMCPComposeProposeIgnoresStatus(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")
	defer func() {
		ts.Close()
		db.Close()
	}()

	// Propose with status: "active" in arguments (should be ignored)
	spec := map[string]any{
		"transport":      "telegram",
		"chat_id":        "123",
		"interval_hours": 24,
	}
	st, body := mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "compose_propose",
			"arguments": map[string]any{
				"name":   "test-ignore-status",
				"kind":   compose.KindReportSchedule,
				"spec":   spec,
				"status": "active", // This should be ignored
			},
		},
	})
	if st != http.StatusOK {
		t.Fatalf("compose_propose: got %d, want 200; body: %v", st, body)
	}

	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("compose_propose: missing result object: %v", body)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("compose_propose: missing content array: %v", result)
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("compose_propose: content[0] not object: %v", content[0])
	}
	text, _ := first["text"].(string)
	var proposeResult map[string]any
	if err := json.Unmarshal([]byte(text), &proposeResult); err != nil {
		t.Fatalf("compose_propose: unmarshal handler result: %v (text: %s)", err, text)
	}

	// Verify status is pending despite "active" in request
	if proposeResult["status"] != "pending" {
		t.Errorf("propose status: got %v, want pending (status from args must be ignored)", proposeResult["status"])
	}

	// Also verify via compose_get
	id, _ := proposeResult["id"].(string)
	st, body = mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "compose_get",
			"arguments": map[string]any{
				"id": id,
			},
		},
	})
	if st != http.StatusOK {
		t.Fatalf("compose_get: got %d, want 200; body: %v", st, body)
	}

	result, ok = body["result"].(map[string]any)
	if !ok {
		t.Fatalf("compose_get: missing result object: %v", body)
	}
	content, ok = result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("compose_get: missing content array: %v", result)
	}
	first, ok = content[0].(map[string]any)
	if !ok {
		t.Fatalf("compose_get: content[0] not object: %v", content[0])
	}
	text, _ = first["text"].(string)
	var getResult map[string]any
	if err := json.Unmarshal([]byte(text), &getResult); err != nil {
		t.Fatalf("compose_get: unmarshal handler result: %v (text: %s)", err, text)
	}

	artifact, ok := getResult["artifact"].(map[string]any)
	if !ok {
		t.Fatalf("compose_get: missing artifact object: %v", getResult)
	}
	if artifact["status"] != "pending" {
		t.Errorf("artifact status via get: got %v, want pending", artifact["status"])
	}
}

// TestMCPComposeProposeInvalidSpec tests that compose_propose returns JSON-RPC error
// for invalid spec (missing transport, interval 0, etc.).
func TestMCPComposeProposeInvalidSpec(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")
	defer func() {
		ts.Close()
		db.Close()
	}()

	testCases := []struct {
		name string
		spec map[string]any
	}{
		{
			name: "missing transport",
			spec: map[string]any{
				"chat_id":        "123",
				"interval_hours": 24,
			},
		},
		{
			name: "interval_hours zero",
			spec: map[string]any{
				"transport":      "telegram",
				"chat_id":        "123",
				"interval_hours": 0,
			},
		},
		{
			name: "invalid transport",
			spec: map[string]any{
				"transport":      "email",
				"chat_id":        "123",
				"interval_hours": 24,
			},
		},
		{
			name: "empty chat_id",
			spec: map[string]any{
				"transport":      "telegram",
				"chat_id":        "",
				"interval_hours": 24,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			st, body := mcpRequest(t, ts, token, map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"method":  "tools/call",
				"params": map[string]any{
					"name": "compose_propose",
					"arguments": map[string]any{
						"name": "test-invalid-" + tc.name,
						"kind": compose.KindReportSchedule,
						"spec": tc.spec,
					},
				},
			})
			if st != http.StatusOK {
				t.Fatalf("compose_propose: got %d, want 200 (JSON-RPC error in body); body: %v", st, body)
			}
			errObj, ok := body["error"].(map[string]any)
			if !ok {
				t.Fatalf("compose_propose %s: missing error object: %v", tc.name, body)
			}
			// Should be internal error (-32603) from handler validation failure
			if errObj["code"] != float64(-32603) {
				t.Errorf("compose_propose %s: error code got %v, want -32603", tc.name, errObj["code"])
			}
		})
	}
}

// TestMCPComposeListFilters tests compose_list with various filters.
func TestMCPComposeListFilters(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")
	defer func() {
		ts.Close()
		db.Close()
	}()

	// Create a report_schedule artifact
	spec1 := map[string]any{
		"transport":      "telegram",
		"chat_id":        "123",
		"interval_hours": 24,
	}
	st, body := mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "compose_propose",
			"arguments": map[string]any{
				"name": "report-1",
				"kind": compose.KindReportSchedule,
				"spec": spec1,
			},
		},
	})
	if st != http.StatusOK {
		t.Fatalf("compose_propose report: got %d, want 200; body: %v", st, body)
	}
	result, _ := body["result"].(map[string]any)
	content, _ := result["content"].([]any)
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	var r1 map[string]any
	json.Unmarshal([]byte(text), &r1)
	id1, _ := r1["id"].(string)

	// Create an automation_rule artifact
	spec2 := map[string]any{
		"trigger": map[string]any{
			"type":           "schedule",
			"interval_hours": 1,
		},
		"action": map[string]any{
			"type":      "send_report",
			"transport": "telegram",
			"chat_id":   "456",
		},
	}
	st, body = mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "compose_propose",
			"arguments": map[string]any{
				"name": "rule-1",
				"kind": compose.KindAutomationRule,
				"spec": spec2,
			},
		},
	})
	if st != http.StatusOK {
		t.Fatalf("compose_propose rule: got %d, want 200; body: %v", st, body)
	}
	result, _ = body["result"].(map[string]any)
	content, _ = result["content"].([]any)
	first, _ = content[0].(map[string]any)
	text, _ = first["text"].(string)
	var r2 map[string]any
	json.Unmarshal([]byte(text), &r2)
	id2, _ := r2["id"].(string)

	_ = id1
	_ = id2

	// Test 1: compose_list without filters -> should return >=2 artifacts
	st, body = mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "compose_list",
			"arguments": map[string]any{},
		},
	})
	if st != http.StatusOK {
		t.Fatalf("compose_list no filter: got %d, want 200; body: %v", st, body)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("compose_list no filter: missing result object: %v", body)
	}
	content, ok = result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("compose_list no filter: missing content array: %v", result)
	}
	first, ok = content[0].(map[string]any)
	if !ok {
		t.Fatalf("compose_list no filter: content[0] not object: %v", content[0])
	}
	text, _ = first["text"].(string)
	var listResult map[string]any
	if err := json.Unmarshal([]byte(text), &listResult); err != nil {
		t.Fatalf("compose_list no filter: unmarshal: %v (text: %s)", err, text)
	}
	artifacts, ok := listResult["artifacts"].([]any)
	if !ok {
		t.Fatalf("compose_list no filter: missing artifacts array: %v", listResult)
	}
	if len(artifacts) < 2 {
		t.Errorf("compose_list no filter: expected >=2 artifacts, got %d", len(artifacts))
	}

	// Test 2: filter kind=automation_rule -> all results should be automation_rule
	st, body = mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "compose_list",
			"arguments": map[string]any{
				"kind": compose.KindAutomationRule,
			},
		},
	})
	if st != http.StatusOK {
		t.Fatalf("compose_list kind filter: got %d, want 200; body: %v", st, body)
	}
	result, ok = body["result"].(map[string]any)
	if !ok {
		t.Fatalf("compose_list kind filter: missing result object: %v", body)
	}
	content, ok = result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("compose_list kind filter: missing content array: %v", result)
	}
	first, ok = content[0].(map[string]any)
	if !ok {
		t.Fatalf("compose_list kind filter: content[0] not object: %v", content[0])
	}
	text, _ = first["text"].(string)
	var listResult2 map[string]any
	if err := json.Unmarshal([]byte(text), &listResult2); err != nil {
		t.Fatalf("compose_list kind filter: unmarshal: %v (text: %s)", err, text)
	}
	artifacts2, ok := listResult2["artifacts"].([]any)
	if !ok {
		t.Fatalf("compose_list kind filter: missing artifacts array: %v", listResult2)
	}
	for _, aAny := range artifacts2 {
		a, _ := aAny.(map[string]any)
		if a["kind"] != compose.KindAutomationRule {
			t.Errorf("compose_list kind filter: got kind %v, want %s", a["kind"], compose.KindAutomationRule)
		}
	}

	// Test 3: filter status=active -> should return 0 (both are pending)
	st, body = mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      5,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "compose_list",
			"arguments": map[string]any{
				"status": "active",
			},
		},
	})
	if st != http.StatusOK {
		t.Fatalf("compose_list status filter: got %d, want 200; body: %v", st, body)
	}
	result, ok = body["result"].(map[string]any)
	if !ok {
		t.Fatalf("compose_list status filter: missing result object: %v", body)
	}
	content, ok = result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("compose_list status filter: missing content array: %v", result)
	}
	first, ok = content[0].(map[string]any)
	if !ok {
		t.Fatalf("compose_list status filter: content[0] not object: %v", content[0])
	}
	text, _ = first["text"].(string)
	var listResult3 map[string]any
	if err := json.Unmarshal([]byte(text), &listResult3); err != nil {
		t.Fatalf("compose_list status filter: unmarshal: %v (text: %s)", err, text)
	}
	artifacts3, ok := listResult3["artifacts"].([]any)
	if !ok {
		t.Fatalf("compose_list status filter: missing artifacts array: %v", listResult3)
	}
	if len(artifacts3) != 0 {
		t.Errorf("compose_list status=active: expected 0 artifacts, got %d", len(artifacts3))
	}
}

// TestMCPComposeGetNotFound tests that compose_get returns error for non-existent ID.
func TestMCPComposeGetNotFound(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")
	defer func() {
		ts.Close()
		db.Close()
	}()

	st, body := mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "compose_get",
			"arguments": map[string]any{
				"id": "cmp-tidak-ada",
			},
		},
	})
	if st != http.StatusOK {
		t.Fatalf("compose_get not found: got %d, want 200 (JSON-RPC error in body); body: %v", st, body)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("compose_get not found: missing error object: %v", body)
	}
	// Should be internal error (-32603) from handler "artifact not found"
	if errObj["code"] != float64(-32603) {
		t.Errorf("compose_get not found: error code got %v, want -32603", errObj["code"])
	}
	msg, _ := errObj["message"].(string)
	if msg != "artifact not found" {
		t.Errorf("compose_get not found: error message got %q, want 'artifact not found'", msg)
	}
}