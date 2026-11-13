package approval

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
)

// ApprovalStatus represents the status of an approval request.
type ApprovalStatus string

const (
	ApprovalStatusPending  ApprovalStatus = "pending"
	ApprovalStatusApproved ApprovalStatus = "approved"
	ApprovalStatusRejected ApprovalStatus = "rejected"
	ApprovalStatusExpired  ApprovalStatus = "expired"
)

// ApprovalRequest represents a pending approval request for a destructive MCP tool call.
type ApprovalRequest struct {
	ID           string          `json:"id"`
	Tool         string          `json:"tool"`
	Arguments    json.RawMessage `json:"arguments"`
	RequestedBy  string          `json:"requested_by"`  // agent name + model id (e.g., "backup_steward:gpt-4o")
	RequestedAt  int64           `json:"requested_at"`  // unix timestamp
	Status       ApprovalStatus  `json:"status"`
	DecidedBy    string          `json:"decided_by,omitempty"`    // username who approved/rejected
	DecidedAt    int64           `json:"decided_at,omitempty"`    // unix timestamp
	ExpiresAt    int64           `json:"expires_at"`              // unix timestamp (24h from requested_at)
	ConsumedAt   int64           `json:"consumed_at,omitempty"`   // unix timestamp when executed
}

// ApprovalStore manages approval requests in the catalog database.
type ApprovalStore struct {
	db *sql.DB
}

// NewApprovalStore creates a new ApprovalStore.
func NewApprovalStore(db *sql.DB) *ApprovalStore {
	return &ApprovalStore{db: db}
}

// ApprovalExpiryHours is the fixed expiry duration for pending requests (24 hours).
const ApprovalExpiryHours = 24

// generateApprovalID returns a collision-proof approval ID.
// Format: appr-<unix-nano>-<8 hex chars from crypto/rand>
func generateApprovalID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := catalog.RandRead(randBytes[:]); err != nil {
		return fmt.Sprintf("appr-%d-%08x", now, time.Now().UnixNano()&0xffffffff)
	}
	return fmt.Sprintf("appr-%d-%s", now, catalog.HexEncode(randBytes[:]))
}

// Create creates a new pending approval request.
func (s *ApprovalStore) Create(tool string, arguments map[string]any, requestedBy string) (*ApprovalRequest, error) {
	argsJSON, err := json.Marshal(arguments)
	if err != nil {
		return nil, fmt.Errorf("marshal arguments: %w", err)
	}

	now := time.Now().Unix()
	expiresAt := now + ApprovalExpiryHours*3600
	id := generateApprovalID()

	req := &ApprovalRequest{
		ID:          id,
		Tool:        tool,
		Arguments:   argsJSON,
		RequestedBy: requestedBy,
		RequestedAt: now,
		Status:      ApprovalStatusPending,
		ExpiresAt:   expiresAt,
	}

	_, err = s.db.Exec(`
		INSERT INTO mcp_approvals (id, tool, arguments, requested_by, requested_at, status, decided_by, decided_at, expires_at, consumed_at)
		VALUES (?, ?, ?, ?, ?, ?, '', 0, ?, 0)
	`, id, tool, argsJSON, requestedBy, now, ApprovalStatusPending, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("insert approval: %w", err)
	}

	return req, nil
}

// Get retrieves an approval request by ID.
// Returns nil if not found. Expired pending requests are auto-marked as expired.
func (s *ApprovalStore) Get(id string) (*ApprovalRequest, error) {
	row := s.db.QueryRow(`
		SELECT id, tool, arguments, requested_by, requested_at, status, decided_by, decided_at, expires_at, consumed_at
		FROM mcp_approvals WHERE id = ?
	`, id)

	var req ApprovalRequest
	var argsJSON []byte
	var decidedBy sql.NullString
	var decidedAt, consumedAt sql.NullInt64

	err := row.Scan(&req.ID, &req.Tool, &argsJSON, &req.RequestedBy, &req.RequestedAt, &req.Status, &decidedBy, &decidedAt, &req.ExpiresAt, &consumedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan approval: %w", err)
	}

	req.Arguments = argsJSON
	if decidedBy.Valid {
		req.DecidedBy = decidedBy.String
	}
	if decidedAt.Valid {
		req.DecidedAt = decidedAt.Int64
	}
	if consumedAt.Valid {
		req.ConsumedAt = consumedAt.Int64
	}

	// Auto-expire pending requests past their expiry
	if req.Status == ApprovalStatusPending && time.Now().Unix() > req.ExpiresAt {
		req.Status = ApprovalStatusExpired
		// Update in DB (best-effort)
		_, _ = s.db.Exec(`UPDATE mcp_approvals SET status = ? WHERE id = ?`, ApprovalStatusExpired, id)
	}

	return &req, nil
}

// List returns approval requests filtered by status.
// Pending requests past expiry are auto-marked as expired (sweep).
func (s *ApprovalStore) List(status ApprovalStatus) ([]*ApprovalRequest, error) {
	// Sweep: auto-expire pending requests past expiry
	now := time.Now().Unix()
	_, _ = s.db.Exec(`UPDATE mcp_approvals SET status = ? WHERE status = ? AND expires_at < ?`, ApprovalStatusExpired, ApprovalStatusPending, now)

	whereClauses := []string{}
	args := []any{}

	if status != "" {
		whereClauses = append(whereClauses, "status = ?")
		args = append(args, status)
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + whereClauses[0]
	}

	query := fmt.Sprintf(`SELECT id, tool, arguments, requested_by, requested_at, status, decided_by, decided_at, expires_at, consumed_at FROM mcp_approvals %s ORDER BY requested_at DESC`, whereSQL)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query approvals: %w", err)
	}
	defer rows.Close()

	var requests []*ApprovalRequest
	for rows.Next() {
		var req ApprovalRequest
		var argsJSON []byte
		var decidedBy sql.NullString
		var decidedAt, consumedAt sql.NullInt64

		if err := rows.Scan(&req.ID, &req.Tool, &argsJSON, &req.RequestedBy, &req.RequestedAt, &req.Status, &decidedBy, &decidedAt, &req.ExpiresAt, &consumedAt); err != nil {
			return nil, fmt.Errorf("scan approval: %w", err)
		}

		req.Arguments = argsJSON
		if decidedBy.Valid {
			req.DecidedBy = decidedBy.String
		}
		if decidedAt.Valid {
			req.DecidedAt = decidedAt.Int64
		}
		if consumedAt.Valid {
			req.ConsumedAt = consumedAt.Int64
		}

		requests = append(requests, &req)
	}

	if requests == nil {
		requests = []*ApprovalRequest{}
	}

	return requests, nil
}

// Approve marks a pending request as approved.
// Returns error if not found, not pending, or already decided.
func (s *ApprovalStore) Approve(id, decidedBy string) (*ApprovalRequest, error) {
	now := time.Now().Unix()

	// Check current status
	req, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("approval not found")
	}
	if req.Status != ApprovalStatusPending {
		return nil, fmt.Errorf("approval not pending (status: %s)", req.Status)
	}

	_, err = s.db.Exec(`
		UPDATE mcp_approvals SET status = ?, decided_by = ?, decided_at = ? WHERE id = ?
	`, ApprovalStatusApproved, decidedBy, now, id)
	if err != nil {
		return nil, fmt.Errorf("update approval: %w", err)
	}

	req.Status = ApprovalStatusApproved
	req.DecidedBy = decidedBy
	req.DecidedAt = now
	return req, nil
}

// Reject marks a pending request as rejected.
// Returns error if not found, not pending, or already decided.
func (s *ApprovalStore) Reject(id, decidedBy string) (*ApprovalRequest, error) {
	now := time.Now().Unix()

	req, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("approval not found")
	}
	if req.Status != ApprovalStatusPending {
		return nil, fmt.Errorf("approval not pending (status: %s)", req.Status)
	}

	_, err = s.db.Exec(`
		UPDATE mcp_approvals SET status = ?, decided_by = ?, decided_at = ? WHERE id = ?
	`, ApprovalStatusRejected, decidedBy, now, id)
	if err != nil {
		return nil, fmt.Errorf("update approval: %w", err)
	}

	req.Status = ApprovalStatusRejected
	req.DecidedBy = decidedBy
	req.DecidedAt = now
	return req, nil
}

// Consume marks an approved request as consumed (executed).
// Returns error if not found, not approved, or already consumed.
func (s *ApprovalStore) Consume(id string) error {
	now := time.Now().Unix()

	req, err := s.Get(id)
	if err != nil {
		return err
	}
	if req == nil {
		return fmt.Errorf("approval not found")
	}
	if req.Status != ApprovalStatusApproved {
		return fmt.Errorf("approval not approved (status: %s)", req.Status)
	}
	if req.ConsumedAt != 0 {
		return fmt.Errorf("approval already consumed")
	}

	_, err = s.db.Exec(`UPDATE mcp_approvals SET consumed_at = ? WHERE id = ?`, now, id)
	if err != nil {
		return fmt.Errorf("update approval consumed: %w", err)
	}

	return nil
}

// ArgumentsMap returns the arguments as a map for tool execution.
func (r *ApprovalRequest) ArgumentsMap() (map[string]any, error) {
	var args map[string]any
	if err := json.Unmarshal(r.Arguments, &args); err != nil {
		return nil, fmt.Errorf("unmarshal arguments: %w", err)
	}
	if args == nil {
		args = map[string]any{}
	}
	return args, nil
}

// MatchesToolAndArgs checks if the request matches the given tool and arguments.
func (r *ApprovalRequest) MatchesToolAndArgs(tool string, arguments map[string]any) bool {
	if r.Tool != tool {
		return false
	}
	args, err := r.ArgumentsMap()
	if err != nil {
		return false
	}
	// Simple deep equality check for JSON-serializable maps
	argsJSON, _ := json.Marshal(args)
	reqArgsJSON, _ := json.Marshal(arguments)
	return string(argsJSON) == string(reqArgsJSON)
}