package httpserver

// KelviOS WS48: FR-3.7 human-gate (P6) end-to-end tests.
// Covers the MCP destructive-tool gate plus the REST approval endpoints:
// a destructive tools/call without an approved, matching, unconsumed
// approval request never executes; the approval lifecycle (pending ->
// approved/rejected -> consumed) is enforced across both surfaces.
//
// Written by Chief under the two-strike takeover rule (two worker sessions
// looped on this file without producing it, 5 Nov 2026).

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
)

// apprCreatePool creates a real pool via REST on the MCP test server and
// returns its ID.
func apprCreatePool(t *testing.T, ts *httptest.Server, token, name string) string {
	t.Helper()
	code, out := tagDoReq(t, ts, "POST", "/api/pools", map[string]any{
		"name":       name,
		"tier":       "hot",
		"mechanism":  "single",
		"disks":      []string{"sda"},
		"mountpoint": t.TempDir(),
	}, token)
	if code != http.StatusCreated {
		t.Fatalf("create pool: got %d, want 201: %v", code, out)
	}
	poolObj, _ := out["pool"].(map[string]any)
	id, _ := poolObj["id"].(string)
	if id == "" {
		t.Fatalf("create pool: missing id in response: %v", out)
	}
	return id
}

// apprCallDeletePool issues an MCP tools/call for delete_pool and returns
// the parsed JSON-RPC response body.
func apprCallDeletePool(t *testing.T, ts *httptest.Server, token string, args map[string]any) map[string]any {
	t.Helper()
	st, body := mcpRequest(t, ts, token, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "delete_pool",
			"arguments": args,
		},
	})
	if st != http.StatusOK {
		t.Fatalf("tools/call delete_pool: got HTTP %d, want 200 (JSON-RPC error in body); body: %v", st, body)
	}
	return body
}

// apprGateError extracts the JSON-RPC error object and its data payload,
// failing the test when the response is not an error.
func apprGateError(t *testing.T, body map[string]any) (float64, map[string]any) {
	t.Helper()
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected JSON-RPC error, got: %v", body)
	}
	code, _ := errObj["code"].(float64)
	data, _ := errObj["data"].(map[string]any)
	return code, data
}

// apprNewRequest calls delete_pool without an approval_id, asserts the
// -32001 pending contract, and returns the created approval request ID.
func apprNewRequest(t *testing.T, ts *httptest.Server, token, poolID string) string {
	t.Helper()
	body := apprCallDeletePool(t, ts, token, map[string]any{"pool_id": poolID})
	code, data := apprGateError(t, body)
	if code != -32001 {
		t.Fatalf("gate refusal code: got %v, want -32001", code)
	}
	if data == nil || data["status"] != "pending" {
		t.Fatalf("gate refusal data: got %v, want status=pending", data)
	}
	id, _ := data["approval_id"].(string)
	if id == "" {
		t.Fatalf("gate refusal data: missing approval_id: %v", data)
	}
	return id
}

// apprDecide calls POST /api/approvals/{id}/{action} with a decider name.
func apprDecide(t *testing.T, ts *httptest.Server, token, id, action string) (int, map[string]any) {
	t.Helper()
	return tagDoReq(t, ts, "POST", "/api/approvals/"+id+"/"+action, map[string]any{"by": "admin"}, token)
}

// apprApprove approves a request and asserts the 200 contract.
func apprApprove(t *testing.T, ts *httptest.Server, token, id string) {
	t.Helper()
	code, out := apprDecide(t, ts, token, id, "approve")
	if code != http.StatusOK {
		t.Fatalf("approve %s: got %d, want 200: %v", id, code, out)
	}
	if out["status"] != "approved" {
		t.Fatalf("approve %s: status got %v, want approved: %v", id, out["status"], out)
	}
}

// apprRow reads (status, consumed_at) for an approval request directly.
func apprRow(t *testing.T, db *sql.DB, id string) (string, int64) {
	t.Helper()
	var status string
	var consumedAt int64
	err := db.QueryRow(`SELECT status, consumed_at FROM mcp_approvals WHERE id = ?`, id).Scan(&status, &consumedAt)
	if err != nil {
		t.Fatalf("read approval row %s: %v", id, err)
	}
	return status, consumedAt
}

// apprList fetches GET /api/approvals with an optional status filter and
// returns the approvals array.
func apprList(t *testing.T, ts *httptest.Server, token, status string) []map[string]any {
	t.Helper()
	path := "/api/approvals"
	if status != "" {
		path += "?status=" + status
	}
	code, out := tagDoReq(t, ts, "GET", path, nil, token)
	if code != http.StatusOK {
		t.Fatalf("list approvals (%s): got %d, want 200: %v", path, code, out)
	}
	raw, _ := out["approvals"].([]any)
	list := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			list = append(list, m)
		}
	}
	return list
}

func apprListHas(list []map[string]any, id string) bool {
	for _, item := range list {
		if item["id"] == id {
			return true
		}
	}
	return false
}

// TestApprovalGateCreatesPendingRequest: a destructive call with no
// approval_id is refused with -32001, creates a pending request row, and
// the handler never runs (the pool survives).
func TestApprovalGateCreatesPendingRequest(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")
	poolID := apprCreatePool(t, ts, token, "apprpool")

	approvalID := apprNewRequest(t, ts, token, poolID)

	status, consumedAt := apprRow(t, db, approvalID)
	if status != "pending" {
		t.Errorf("approval row status: got %q, want pending", status)
	}
	if consumedAt != 0 {
		t.Errorf("approval row consumed_at: got %d, want 0", consumedAt)
	}
	var tool, argsJSON string
	if err := db.QueryRow(`SELECT tool, arguments FROM mcp_approvals WHERE id = ?`, approvalID).Scan(&tool, &argsJSON); err != nil {
		t.Fatalf("read approval row: %v", err)
	}
	if tool != "delete_pool" {
		t.Errorf("approval row tool: got %q, want delete_pool", tool)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		t.Fatalf("unmarshal stored arguments: %v", err)
	}
	if args["pool_id"] != poolID {
		t.Errorf("stored arguments pool_id: got %v, want %v", args["pool_id"], poolID)
	}

	pool, _, err := catalog.GetPool(db, poolID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if pool == nil {
		t.Errorf("pool %s was deleted by a refused call", poolID)
	}
}

// TestApprovalPendingRequestCannotExecute: presenting a still-pending
// approval_id does not execute the tool.
func TestApprovalPendingRequestCannotExecute(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")
	poolID := apprCreatePool(t, ts, token, "apprpool")
	approvalID := apprNewRequest(t, ts, token, poolID)

	body := apprCallDeletePool(t, ts, token, map[string]any{"pool_id": poolID, "approval_id": approvalID})
	code, data := apprGateError(t, body)
	if code != -32001 {
		t.Fatalf("pending execution code: got %v, want -32001", code)
	}
	if data == nil || data["status"] != "pending" {
		t.Errorf("pending execution data: got %v, want status=pending", data)
	}

	pool, _, err := catalog.GetPool(db, poolID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if pool == nil {
		t.Errorf("pool %s was deleted while its approval was still pending", poolID)
	}
}

// TestApprovalApproveEndpointMarksApproved: the REST approve endpoint
// records the decision, and the request shows up in the approved list
// with its arguments intact.
func TestApprovalApproveEndpointMarksApproved(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")
	poolID := apprCreatePool(t, ts, token, "apprpool")
	approvalID := apprNewRequest(t, ts, token, poolID)

	if list := apprList(t, ts, token, "pending"); !apprListHas(list, approvalID) {
		t.Fatalf("pending list does not contain %s: %v", approvalID, list)
	}

	code, out := apprDecide(t, ts, token, approvalID, "approve")
	if code != http.StatusOK {
		t.Fatalf("approve: got %d, want 200: %v", code, out)
	}
	if out["status"] != "approved" {
		t.Errorf("approve response status: got %v, want approved", out["status"])
	}
	if out["decided_by"] != "admin" {
		t.Errorf("approve response decided_by: got %v, want admin", out["decided_by"])
	}

	status, _ := apprRow(t, db, approvalID)
	if status != "approved" {
		t.Errorf("approval row status: got %q, want approved", status)
	}
	var decidedAt int64
	if err := db.QueryRow(`SELECT decided_at FROM mcp_approvals WHERE id = ?`, approvalID).Scan(&decidedAt); err != nil {
		t.Fatalf("read decided_at: %v", err)
	}
	if decidedAt == 0 {
		t.Errorf("approval row decided_at is 0 after approve")
	}

	list := apprList(t, ts, token, "approved")
	if !apprListHas(list, approvalID) {
		t.Fatalf("approved list does not contain %s: %v", approvalID, list)
	}
	for _, item := range list {
		if item["id"] == approvalID {
			args, _ := item["arguments"].(map[string]any)
			if args == nil || args["pool_id"] != poolID {
				t.Errorf("list entry arguments: got %v, want pool_id=%s", item["arguments"], poolID)
			}
		}
	}
}

// TestApprovalApprovedExecutionDeletesPool: the full happy path — an
// approved, matching call executes exactly once: the pool is deleted and
// the request is marked consumed.
func TestApprovalApprovedExecutionDeletesPool(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")
	poolID := apprCreatePool(t, ts, token, "apprpool")
	approvalID := apprNewRequest(t, ts, token, poolID)
	apprApprove(t, ts, token, approvalID)

	body := apprCallDeletePool(t, ts, token, map[string]any{"pool_id": poolID, "approval_id": approvalID})
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("approved execution: missing result: %v", body)
	}
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("approved execution: empty content: %v", result)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if !strings.Contains(text, `"deleted"`) || !strings.Contains(text, poolID) {
		t.Errorf("approved execution text: got %q, want deleted=%s", text, poolID)
	}

	pool, _, err := catalog.GetPool(db, poolID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if pool != nil {
		t.Errorf("pool %s still exists after approved execution", poolID)
	}
	status, consumedAt := apprRow(t, db, approvalID)
	if status != "approved" {
		t.Errorf("approval row status after execution: got %q, want approved", status)
	}
	if consumedAt == 0 {
		t.Errorf("approval row consumed_at is 0 after execution")
	}
}

// TestApprovalConsumedApprovalCannotBeReused: a second call with the
// same approval_id after execution is refused as consumed.
func TestApprovalConsumedApprovalCannotBeReused(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")
	poolID := apprCreatePool(t, ts, token, "apprpool")
	approvalID := apprNewRequest(t, ts, token, poolID)
	apprApprove(t, ts, token, approvalID)

	body := apprCallDeletePool(t, ts, token, map[string]any{"pool_id": poolID, "approval_id": approvalID})
	if _, ok := body["result"].(map[string]any); !ok {
		t.Fatalf("first approved execution failed: %v", body)
	}

	body = apprCallDeletePool(t, ts, token, map[string]any{"pool_id": poolID, "approval_id": approvalID})
	code, data := apprGateError(t, body)
	if code != -32001 {
		t.Fatalf("reuse code: got %v, want -32001", code)
	}
	if data == nil || data["status"] != "consumed" {
		t.Errorf("reuse data: got %v, want status=consumed", data)
	}
}

// TestApprovalRejectedRequestCannotExecute: a rejected request refuses
// execution and the pool survives.
func TestApprovalRejectedRequestCannotExecute(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")
	poolID := apprCreatePool(t, ts, token, "apprpool")
	approvalID := apprNewRequest(t, ts, token, poolID)

	code, out := apprDecide(t, ts, token, approvalID, "reject")
	if code != http.StatusOK {
		t.Fatalf("reject: got %d, want 200: %v", code, out)
	}
	if out["status"] != "rejected" {
		t.Errorf("reject response status: got %v, want rejected", out["status"])
	}

	body := apprCallDeletePool(t, ts, token, map[string]any{"pool_id": poolID, "approval_id": approvalID})
	gateCode, data := apprGateError(t, body)
	if gateCode != -32001 {
		t.Fatalf("rejected execution code: got %v, want -32001", gateCode)
	}
	if data == nil || data["status"] != "rejected" {
		t.Errorf("rejected execution data: got %v, want status=rejected", data)
	}

	pool, _, err := catalog.GetPool(db, poolID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if pool == nil {
		t.Errorf("pool %s was deleted by a rejected approval", poolID)
	}
}

// TestApprovalsListStatusFilter: the list endpoint filters by status and
// the unfiltered list contains every request.
func TestApprovalsListStatusFilter(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")
	approvedID := apprNewRequest(t, ts, token, "pool-alpha")
	pendingID := apprNewRequest(t, ts, token, "pool-beta")
	apprApprove(t, ts, token, approvedID)

	pending := apprList(t, ts, token, "pending")
	if !apprListHas(pending, pendingID) || apprListHas(pending, approvedID) {
		t.Errorf("pending filter wrong: %v", pending)
	}
	approved := apprList(t, ts, token, "approved")
	if !apprListHas(approved, approvedID) || apprListHas(approved, pendingID) {
		t.Errorf("approved filter wrong: %v", approved)
	}
	all := apprList(t, ts, token, "")
	if !apprListHas(all, approvedID) || !apprListHas(all, pendingID) {
		t.Errorf("unfiltered list missing requests: %v", all)
	}
}

// TestApprovalsEndpointsRequireAuth: the approval REST surface and the
// MCP gate both refuse unauthenticated callers.
func TestApprovalsEndpointsRequireAuth(t *testing.T) {
	ts, _, _ := newMCPTestServer(t, ":memory:")

	code, _ := tagDoReq(t, ts, "GET", "/api/approvals", nil, "")
	if code != http.StatusUnauthorized {
		t.Errorf("GET /api/approvals without token: got %d, want 401", code)
	}
	code, _ = tagDoReq(t, ts, "POST", "/api/approvals/appr-x/approve", map[string]any{"by": "admin"}, "")
	if code != http.StatusUnauthorized {
		t.Errorf("POST approve without token: got %d, want 401", code)
	}
	st, _ := mcpRequest(t, ts, "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "delete_pool",
			"arguments": map[string]any{"pool_id": "pool-x"},
		},
	})
	if st != http.StatusUnauthorized {
		t.Errorf("MCP delete_pool without token: got %d, want 401", st)
	}
}

// TestApprovalMismatchedArgumentsRefused: an approved request does not
// authorize different arguments; the request stays unconsumed.
func TestApprovalMismatchedArgumentsRefused(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")
	poolID := apprCreatePool(t, ts, token, "apprpool")
	approvalID := apprNewRequest(t, ts, token, poolID)
	apprApprove(t, ts, token, approvalID)

	body := apprCallDeletePool(t, ts, token, map[string]any{"pool_id": "pool-other", "approval_id": approvalID})
	code, data := apprGateError(t, body)
	if code != -32001 {
		t.Fatalf("mismatch code: got %v, want -32001", code)
	}
	if data == nil || data["status"] != "mismatch" {
		t.Errorf("mismatch data: got %v, want status=mismatch", data)
	}

	_, consumedAt := apprRow(t, db, approvalID)
	if consumedAt != 0 {
		t.Errorf("mismatched call consumed the approval (consumed_at=%d)", consumedAt)
	}
	pool, _, err := catalog.GetPool(db, poolID)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if pool == nil {
		t.Errorf("pool %s was deleted by a mismatched call", poolID)
	}
}

// TestApprovalUnknownIDNotFound: unknown approval IDs are refused on
// both surfaces — 404 on the REST decide endpoint, not_found on the gate.
func TestApprovalUnknownIDNotFound(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")

	code, _ := apprDecide(t, ts, token, "appr-does-not-exist", "approve")
	if code != http.StatusNotFound {
		t.Errorf("approve unknown id: got %d, want 404", code)
	}

	body := apprCallDeletePool(t, ts, token, map[string]any{"pool_id": "pool-x", "approval_id": "appr-does-not-exist"})
	gateCode, data := apprGateError(t, body)
	if gateCode != -32001 {
		t.Fatalf("unknown approval code: got %v, want -32001", gateCode)
	}
	if data == nil || data["status"] != "not_found" {
		t.Errorf("unknown approval data: got %v, want status=not_found", data)
	}
}

// TestApprovalDecideNonPendingConflict: deciding an already-decided
// request conflicts on both actions.
func TestApprovalDecideNonPendingConflict(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")
	approvalID := apprNewRequest(t, ts, token, "pool-alpha")
	apprApprove(t, ts, token, approvalID)

	code, _ := apprDecide(t, ts, token, approvalID, "approve")
	if code != http.StatusConflict {
		t.Errorf("re-approve: got %d, want 409", code)
	}
	code, _ = apprDecide(t, ts, token, approvalID, "reject")
	if code != http.StatusConflict {
		t.Errorf("reject after approve: got %d, want 409", code)
	}
}

// TestApprovalDecideRequiresBy: the decide endpoints require the decider
// identity in the body.
func TestApprovalDecideRequiresBy(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")
	approvalID := apprNewRequest(t, ts, token, "pool-alpha")

	code, out := tagDoReq(t, ts, "POST", "/api/approvals/"+approvalID+"/approve", map[string]any{}, token)
	if code != http.StatusBadRequest {
		t.Errorf("approve without by: got %d, want 400: %v", code, out)
	}
}

// TestApprovalExpiredPendingCannotExecute: a pending request past its
// 24h expiry reads as expired and can neither execute nor be approved.
func TestApprovalExpiredPendingCannotExecute(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")
	approvalID := apprNewRequest(t, ts, token, "pool-alpha")

	past := time.Now().Unix() - 60
	if _, err := db.Exec(`UPDATE mcp_approvals SET expires_at = ? WHERE id = ?`, past, approvalID); err != nil {
		t.Fatalf("backdate expiry: %v", err)
	}

	body := apprCallDeletePool(t, ts, token, map[string]any{"pool_id": "pool-alpha", "approval_id": approvalID})
	code, data := apprGateError(t, body)
	if code != -32001 {
		t.Fatalf("expired execution code: got %v, want -32001", code)
	}
	if data == nil || data["status"] != "expired" {
		t.Errorf("expired execution data: got %v, want status=expired", data)
	}

	status, _ := apprRow(t, db, approvalID)
	if status != "expired" {
		t.Errorf("approval row status after expiry: got %q, want expired", status)
	}

	decideCode, _ := apprDecide(t, ts, token, approvalID, "approve")
	if decideCode != http.StatusConflict {
		t.Errorf("approve expired request: got %d, want 409", decideCode)
	}
}

// TestApprovalGateWritesAuditRows: the gate's three audit moments —
// request creation, the human decision, and the approved execution —
// all land in the audit log.
func TestApprovalGateWritesAuditRows(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")
	poolID := apprCreatePool(t, ts, token, "apprpool")
	approvalID := apprNewRequest(t, ts, token, poolID)
	apprApprove(t, ts, token, approvalID)

	body := apprCallDeletePool(t, ts, token, map[string]any{"pool_id": poolID, "approval_id": approvalID})
	if _, ok := body["result"].(map[string]any); !ok {
		t.Fatalf("approved execution failed: %v", body)
	}

	count := func(query string, args ...any) int {
		var n int
		if err := db.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatalf("audit query: %v", err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM audit_log WHERE target_type = 'mcp_approval' AND target_id = ? AND result = 'pending'`, approvalID); n != 1 {
		t.Errorf("audit rows for request creation: got %d, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM audit_log WHERE action = 'approval.approve' AND target_type = 'approval' AND target_id = ? AND result = 'ok'`, approvalID); n != 1 {
		t.Errorf("audit rows for approve decision: got %d, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM audit_log WHERE action = 'mcp.tool.delete_pool' AND target_type = 'mcp_tool' AND target_id = 'delete_pool' AND result = 'ok'`); n != 1 {
		t.Errorf("audit rows for approved execution: got %d, want 1", n)
	}
}
