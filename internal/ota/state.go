package ota

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
)

// State represents the OTA update state.
type State string

const (
	StateAvailable     State = "available"
	StateStaged        State = "staged"
	StatePendingSwitch State = "pending_switch"
	StateApplied       State = "applied"
	StateRolledBack    State = "rolled_back"
	StateFailed        State = "failed"
)

// ValidTransition returns true if the transition from -> to is valid.
func ValidTransition(from, to State) bool {
	switch from {
	case StateAvailable:
		return to == StateStaged || to == StateFailed
	case StateStaged:
		return to == StatePendingSwitch || to == StateFailed
	case StatePendingSwitch:
		return to == StateApplied || to == StateRolledBack || to == StateFailed
	case StateApplied:
		return to == StateRolledBack
	case StateRolledBack:
		return false // terminal
	case StateFailed:
		return false // terminal
	default:
		return false
	}
}

// ErrInvalidTransition is returned for invalid state transitions.
var ErrInvalidTransition = errors.New("invalid_transition")

// UpdateRecord represents a persisted OTA update record.
type UpdateRecord struct {
	ID              string
	Version         string
	Channel         string
	Kind            string
	ManifestPath    string
	ArtifactName    string
	ArtifactSHA256  string
	SignatureStatus string
	SlotTarget      string
	State           State
	LastActor       string
	LastDetail      string
	CreatedAt       int64
	UpdatedAt       int64
}

// Store handles persistence of OTA update records.
type Store struct {
	db *sql.DB
}

// NewStore creates a new Store.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// InitSchema creates the ota_updates table if it doesn't exist.
func (s *Store) InitSchema() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS ota_updates (
			id              TEXT PRIMARY KEY,
			version         TEXT NOT NULL,
			channel         TEXT NOT NULL,
			kind            TEXT NOT NULL,
			manifest_path   TEXT NOT NULL,
			artifact_name   TEXT NOT NULL,
			artifact_sha256 TEXT NOT NULL,
			signature_status TEXT NOT NULL,
			slot_target     TEXT,
			state           TEXT NOT NULL,
			last_actor      TEXT NOT NULL,
			last_detail     TEXT,
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL
		);
	`)
	if err != nil {
		return fmt.Errorf("create ota_updates table: %w", err)
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_ota_updates_state ON ota_updates(state)`)
	if err != nil {
		return fmt.Errorf("create ota_updates state index: %w", err)
	}
	_, err = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_ota_updates_created_at ON ota_updates(created_at DESC)`)
	return err
}

// generateID generates a simple unique ID.
func generateID() string {
	return fmt.Sprintf("ota-%d", time.Now().UnixNano())
}

// Create inserts a new update record with state "available".
func (s *Store) Create(ctx context.Context, pkg *PackageInfo, actor string) (*UpdateRecord, error) {
	id := generateID()
	now := time.Now().Unix()
	rec := &UpdateRecord{
		ID:              id,
		Version:         pkg.Manifest.Version,
		Channel:         pkg.Manifest.Channel,
		Kind:            pkg.Manifest.Kind,
		ManifestPath:    pkg.Dir,
		ArtifactName:    pkg.Manifest.Artifact,
		ArtifactSHA256:  pkg.Manifest.SHA256,
		SignatureStatus: "verified",
		State:           StateAvailable,
		LastActor:       actor,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ota_updates (id, version, channel, kind, manifest_path, artifact_name, artifact_sha256, signature_status, slot_target, state, last_actor, last_detail, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, rec.ID, rec.Version, rec.Channel, rec.Kind, rec.ManifestPath, rec.ArtifactName, rec.ArtifactSHA256, rec.SignatureStatus, rec.SlotTarget, rec.State, rec.LastActor, rec.LastDetail, rec.CreatedAt, rec.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert ota update: %w", err)
	}
	return rec, nil
}

// UpdateState updates the state and related fields of a record.
func (s *Store) UpdateState(ctx context.Context, id string, newState State, actor, detail, slotTarget string) error {
	now := time.Now().Unix()
	var slotArg any
	if slotTarget != "" {
		slotArg = slotTarget
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE ota_updates
		SET state = ?, last_actor = ?, last_detail = ?, updated_at = ?, slot_target = COALESCE(?, slot_target)
		WHERE id = ?
	`, newState, actor, detail, now, slotArg, id)
	if err != nil {
		return fmt.Errorf("update ota state: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("record not found: %s", id)
	}
	return nil
}

// Get retrieves a record by ID.
func (s *Store) Get(ctx context.Context, id string) (*UpdateRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, version, channel, kind, manifest_path, artifact_name, artifact_sha256, signature_status, slot_target, state, last_actor, last_detail, created_at, updated_at
		FROM ota_updates WHERE id = ?
	`, id)
	var rec UpdateRecord
	var slotTarget sql.NullString
	var lastDetail sql.NullString
	err := row.Scan(&rec.ID, &rec.Version, &rec.Channel, &rec.Kind, &rec.ManifestPath, &rec.ArtifactName, &rec.ArtifactSHA256, &rec.SignatureStatus, &slotTarget, &rec.State, &rec.LastActor, &lastDetail, &rec.CreatedAt, &rec.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get ota update: %w", err)
	}
	if slotTarget.Valid {
		rec.SlotTarget = slotTarget.String
	}
	if lastDetail.Valid {
		rec.LastDetail = lastDetail.String
	}
	return &rec, nil
}

// List returns all records, newest first.
func (s *Store) List(ctx context.Context) ([]*UpdateRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, version, channel, kind, manifest_path, artifact_name, artifact_sha256, signature_status, slot_target, state, last_actor, last_detail, created_at, updated_at
		FROM ota_updates ORDER BY created_at DESC, rowid DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list ota updates: %w", err)
	}
	defer rows.Close()
	var recs []*UpdateRecord
	for rows.Next() {
		var rec UpdateRecord
		var slotTarget sql.NullString
		var lastDetail sql.NullString
		if err := rows.Scan(&rec.ID, &rec.Version, &rec.Channel, &rec.Kind, &rec.ManifestPath, &rec.ArtifactName, &rec.ArtifactSHA256, &rec.SignatureStatus, &slotTarget, &rec.State, &rec.LastActor, &lastDetail, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan ota update: %w", err)
		}
		if slotTarget.Valid {
			rec.SlotTarget = slotTarget.String
		}
		if lastDetail.Valid {
			rec.LastDetail = lastDetail.String
		}
		recs = append(recs, &rec)
	}
	return recs, rows.Err()
}

// GetCurrent returns the record with the latest non-terminal state, if any.
func (s *Store) GetCurrent(ctx context.Context) (*UpdateRecord, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, version, channel, kind, manifest_path, artifact_name, artifact_sha256, signature_status, slot_target, state, last_actor, last_detail, created_at, updated_at
		FROM ota_updates
		WHERE state IN (?, ?, ?, ?)
		ORDER BY created_at DESC, rowid DESC
		LIMIT 1
	`, StateAvailable, StateStaged, StatePendingSwitch, StateApplied)
	var rec UpdateRecord
	var slotTarget sql.NullString
	var lastDetail sql.NullString
	err := row.Scan(&rec.ID, &rec.Version, &rec.Channel, &rec.Kind, &rec.ManifestPath, &rec.ArtifactName, &rec.ArtifactSHA256, &rec.SignatureStatus, &slotTarget, &rec.State, &rec.LastActor, &lastDetail, &rec.CreatedAt, &rec.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get current ota update: %w", err)
	}
	if slotTarget.Valid {
		rec.SlotTarget = slotTarget.String
	}
	if lastDetail.Valid {
		rec.LastDetail = lastDetail.String
	}
	return &rec, nil
}

// Audit logs an OTA action using the catalog audit log.
func (s *Store) Audit(db *sql.DB, actor, action, targetID, result, detail string) {
	catalog.WriteAuditLog(db, actor, "ota."+action, "ota_update", targetID, result, detail)
}