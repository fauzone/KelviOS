package approval

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	_ "modernc.org/sqlite"
)

func setupApprovalDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		db.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestApprovalCreatePendingWith24hExpiry(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	req, err := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "backup_steward:gpt-4o")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if req == nil {
		t.Fatal("Create returned nil request")
	}
	if req.ID == "" {
		t.Error("ID is empty")
	}
	if req.Tool != "delete_pool" {
		t.Errorf("Tool: got %q, want delete_pool", req.Tool)
	}
	if req.RequestedBy != "backup_steward:gpt-4o" {
		t.Errorf("RequestedBy: got %q, want backup_steward:gpt-4o", req.RequestedBy)
	}
	if req.Status != ApprovalStatusPending {
		t.Errorf("Status: got %q, want pending", req.Status)
	}
	if req.DecidedBy != "" {
		t.Errorf("DecidedBy should be empty for pending, got %q", req.DecidedBy)
	}
	if req.DecidedAt != 0 {
		t.Errorf("DecidedAt should be 0 for pending, got %d", req.DecidedAt)
	}
	if req.ConsumedAt != 0 {
		t.Errorf("ConsumedAt should be 0 for pending, got %d", req.ConsumedAt)
	}

	// Verify expiry is ~24h from now
	now := time.Now().Unix()
	expectedExpiry := now + ApprovalExpiryHours*3600
	if req.ExpiresAt < expectedExpiry-10 || req.ExpiresAt > expectedExpiry+10 {
		t.Errorf("ExpiresAt: got %d, want ~%d (24h from now)", req.ExpiresAt, expectedExpiry)
	}

	// Verify arguments stored correctly
	args, err := req.ArgumentsMap()
	if err != nil {
		t.Fatalf("ArgumentsMap: %v", err)
	}
	if args["pool_id"] != "pool-1" {
		t.Errorf("arguments: got %v, want pool_id=pool-1", args)
	}

	// Verify DB row exists
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM mcp_approvals WHERE id = ?`, req.ID).Scan(&count)
	if err != nil {
		t.Fatalf("query count: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 row in mcp_approvals, got %d", count)
	}
}

func TestApprovalApproveTransition(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	req, err := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "backup_steward:gpt-4o")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated, err := store.Approve(req.ID, "admin")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if updated.Status != ApprovalStatusApproved {
		t.Errorf("Status: got %q, want approved", updated.Status)
	}
	if updated.DecidedBy != "admin" {
		t.Errorf("DecidedBy: got %q, want admin", updated.DecidedBy)
	}
	if updated.DecidedAt == 0 {
		t.Error("DecidedAt should be set")
	}
	if updated.ConsumedAt != 0 {
		t.Errorf("ConsumedAt should be 0 after approve, got %d", updated.ConsumedAt)
	}

	// Verify DB row updated
	var status, decidedBy string
	var decidedAt int64
	err = db.QueryRow(`SELECT status, decided_by, decided_at FROM mcp_approvals WHERE id = ?`, req.ID).Scan(&status, &decidedBy, &decidedAt)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "approved" {
		t.Errorf("DB status: got %q, want approved", status)
	}
	if decidedBy != "admin" {
		t.Errorf("DB decided_by: got %q, want admin", decidedBy)
	}
	if decidedAt == 0 {
		t.Error("DB decided_at should be set")
	}
}

func TestApprovalRejectTransition(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	req, err := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "backup_steward:gpt-4o")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	updated, err := store.Reject(req.ID, "admin")
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}

	if updated.Status != ApprovalStatusRejected {
		t.Errorf("Status: got %q, want rejected", updated.Status)
	}
	if updated.DecidedBy != "admin" {
		t.Errorf("DecidedBy: got %q, want admin", updated.DecidedBy)
	}
	if updated.DecidedAt == 0 {
		t.Error("DecidedAt should be set")
	}

	// Verify DB row updated
	var status string
	err = db.QueryRow(`SELECT status FROM mcp_approvals WHERE id = ?`, req.ID).Scan(&status)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "rejected" {
		t.Errorf("DB status: got %q, want rejected", status)
	}
}

func TestApprovalDoubleApproveReturnsError(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	req, err := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "backup_steward:gpt-4o")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// First approve
	_, err = store.Approve(req.ID, "admin")
	if err != nil {
		t.Fatalf("first Approve: %v", err)
	}

	// Second approve should fail
	_, err = store.Approve(req.ID, "admin")
	if err == nil {
		t.Fatal("second Approve should have returned error")
	}
	if err.Error() != "approval not pending (status: approved)" {
		t.Errorf("error message: got %q, want 'approval not pending (status: approved)'", err.Error())
	}
}

func TestApprovalConsumeOnApproved(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	req, err := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "backup_steward:gpt-4o")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = store.Approve(req.ID, "admin")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}

	err = store.Consume(req.ID)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}

	// Verify consumed_at is set
	var consumedAt int64
	err = db.QueryRow(`SELECT consumed_at FROM mcp_approvals WHERE id = ?`, req.ID).Scan(&consumedAt)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if consumedAt == 0 {
		t.Error("ConsumedAt should be set after Consume")
	}
}

func TestApprovalConsumeTwiceReturnsError(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	req, err := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "backup_steward:gpt-4o")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = store.Approve(req.ID, "admin")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}

	err = store.Consume(req.ID)
	if err != nil {
		t.Fatalf("first Consume: %v", err)
	}

	err = store.Consume(req.ID)
	if err == nil {
		t.Fatal("second Consume should have returned error")
	}
	if err.Error() != "approval already consumed" {
		t.Errorf("error message: got %q, want 'approval already consumed'", err.Error())
	}
}

func TestApprovalConsumeOnPendingReturnsError(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	req, err := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "backup_steward:gpt-4o")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Try to consume without approving
	err = store.Consume(req.ID)
	if err == nil {
		t.Fatal("Consume on pending should have returned error")
	}
	if err.Error() != "approval not approved (status: pending)" {
		t.Errorf("error message: got %q, want 'approval not approved (status: pending)'", err.Error())
	}
}

func TestApprovalExpiredPendingReadsAsExpired(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	req, err := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "backup_steward:gpt-4o")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Manually set expires_at to past
	_, err = db.Exec(`UPDATE mcp_approvals SET expires_at = ? WHERE id = ?`, time.Now().Unix()-3600, req.ID)
	if err != nil {
		t.Fatalf("update expires_at: %v", err)
	}

	// Get should auto-mark as expired
	got, err := store.Get(req.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.Status != ApprovalStatusExpired {
		t.Errorf("Status: got %q, want expired", got.Status)
	}

	// Verify DB was updated
	var status string
	err = db.QueryRow(`SELECT status FROM mcp_approvals WHERE id = ?`, req.ID).Scan(&status)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if status != "expired" {
		t.Errorf("DB status: got %q, want expired", status)
	}
}

func TestApprovalListFiltersByStatus(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	// Create 3 requests
	req1, _ := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "agent1")
	req2, _ := store.Create("delete_pool", map[string]any{"pool_id": "pool-2"}, "agent2")
	req3, _ := store.Create("delete_pool", map[string]any{"pool_id": "pool-3"}, "agent3")

	// Approve req1
	_, _ = store.Approve(req1.ID, "admin")
	// Reject req2
	_, _ = store.Reject(req2.ID, "admin")
	// req3 stays pending

	// List pending
	pending, err := store.List(ApprovalStatusPending)
	if err != nil {
		t.Fatalf("List pending: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("pending count: got %d, want 1", len(pending))
	}
	if pending[0].ID != req3.ID {
		t.Errorf("pending ID: got %q, want %q", pending[0].ID, req3.ID)
	}

	// List approved
	approved, err := store.List(ApprovalStatusApproved)
	if err != nil {
		t.Fatalf("List approved: %v", err)
	}
	if len(approved) != 1 {
		t.Errorf("approved count: got %d, want 1", len(approved))
	}
	if approved[0].ID != req1.ID {
		t.Errorf("approved ID: got %q, want %q", approved[0].ID, req1.ID)
	}

	// List rejected
	rejected, err := store.List(ApprovalStatusRejected)
	if err != nil {
		t.Fatalf("List rejected: %v", err)
	}
	if len(rejected) != 1 {
		t.Errorf("rejected count: got %d, want 1", len(rejected))
	}
	if rejected[0].ID != req2.ID {
		t.Errorf("rejected ID: got %q, want %q", rejected[0].ID, req2.ID)
	}

	// List all (empty status)
	all, err := store.List("")
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("all count: got %d, want 3", len(all))
	}
}

func TestApprovalArgumentsMap(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	args := map[string]any{
		"pool_id":  "pool-1",
		"force":    true,
		"count":    42,
		"nested":   map[string]any{"key": "value"},
		"array":    []any{1, 2, 3},
	}
	req, err := store.Create("delete_pool", args, "agent")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := req.ArgumentsMap()
	if err != nil {
		t.Fatalf("ArgumentsMap: %v", err)
	}

	// Compare via JSON marshaling (same as MatchesToolAndArgs does)
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(args)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("arguments mismatch: got %s, want %s", gotJSON, wantJSON)
	}
}

func TestApprovalMatchesToolAndArgs(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	req, err := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "agent")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Matching tool and args
	if !req.MatchesToolAndArgs("delete_pool", map[string]any{"pool_id": "pool-1"}) {
		t.Error("MatchesToolAndArgs should return true for matching tool and args")
	}

	// Different tool
	if req.MatchesToolAndArgs("wake_disk", map[string]any{"pool_id": "pool-1"}) {
		t.Error("MatchesToolAndArgs should return false for different tool")
	}

	// Different args
	if req.MatchesToolAndArgs("delete_pool", map[string]any{"pool_id": "pool-2"}) {
		t.Error("MatchesToolAndArgs should return false for different args")
	}

	// Extra arg in request
	if req.MatchesToolAndArgs("delete_pool", map[string]any{"pool_id": "pool-1", "extra": "value"}) {
		t.Error("MatchesToolAndArgs should return false when request has extra args")
	}
}

func TestApprovalGetNotFoundReturnsNil(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	got, err := store.Get("appr-nonexistent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Errorf("Get should return nil for not found, got %+v", got)
	}
}

func TestApprovalApproveNotFoundReturnsError(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	_, err := store.Approve("appr-nonexistent", "admin")
	if err == nil {
		t.Fatal("Approve should return error for not found")
	}
	if err.Error() != "approval not found" {
		t.Errorf("error message: got %q, want 'approval not found'", err.Error())
	}
}

func TestApprovalRejectNotFoundReturnsError(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	_, err := store.Reject("appr-nonexistent", "admin")
	if err == nil {
		t.Fatal("Reject should return error for not found")
	}
	if err.Error() != "approval not found" {
		t.Errorf("error message: got %q, want 'approval not found'", err.Error())
	}
}

func TestApprovalConsumeNotFoundReturnsError(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	err := store.Consume("appr-nonexistent")
	if err == nil {
		t.Fatal("Consume should return error for not found")
	}
	if err.Error() != "approval not found" {
		t.Errorf("error message: got %q, want 'approval not found'", err.Error())
	}
}

func TestApprovalApproveRejectedReturnsError(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	req, _ := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "agent")
	_, _ = store.Reject(req.ID, "admin")

	_, err := store.Approve(req.ID, "admin")
	if err == nil {
		t.Fatal("Approve should return error for rejected request")
	}
	if err.Error() != "approval not pending (status: rejected)" {
		t.Errorf("error message: got %q, want 'approval not pending (status: rejected)'", err.Error())
	}
}

func TestApprovalRejectApprovedReturnsError(t *testing.T) {
	db := setupApprovalDB(t)
	store := NewApprovalStore(db)

	req, _ := store.Create("delete_pool", map[string]any{"pool_id": "pool-1"}, "agent")
	_, _ = store.Approve(req.ID, "admin")

	_, err := store.Reject(req.ID, "admin")
	if err == nil {
		t.Fatal("Reject should return error for approved request")
	}
	if err.Error() != "approval not pending (status: approved)" {
		t.Errorf("error message: got %q, want 'approval not pending (status: approved)'", err.Error())
	}
}