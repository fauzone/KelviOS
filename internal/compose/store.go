package compose

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// generateArtifactID generates a collision-proof artifact ID.
// Format: cmp-<unix-nano>-<8 hex chars from crypto/rand>
func generateArtifactID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		return fmt.Sprintf("cmp-%d-%08x", now, time.Now().UnixNano()&0xffffffff)
	}
	return fmt.Sprintf("cmp-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// CreateArtifact creates a new compose artifact with status pending and version 1.
func CreateArtifact(db *sql.DB, name, kind string, spec json.RawMessage, createdBy string, now time.Time) (*Artifact, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if kind == "" {
		return nil, fmt.Errorf("kind is required")
	}

	id := generateArtifactID()
	createdAt := now.Unix()

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Insert artifact
	_, err = tx.Exec(`
		INSERT INTO compose_artifacts (id, name, kind, status, current_version, spec, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, 1, ?, ?, ?, ?)
	`, id, name, kind, StatusPending, spec, createdBy, createdAt, createdAt)
	if err != nil {
		return nil, fmt.Errorf("insert artifact: %w", err)
	}

	// Insert version 1
	_, err = tx.Exec(`
		INSERT INTO compose_artifact_versions (artifact_id, version, spec, created_by, created_at)
		VALUES (?, 1, ?, ?, ?)
	`, id, spec, createdBy, createdAt)
	if err != nil {
		return nil, fmt.Errorf("insert artifact version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &Artifact{
		ID:             id,
		Name:           name,
		Kind:           kind,
		Status:         StatusPending,
		CurrentVersion: 1,
		Spec:           spec,
		CreatedBy:      createdBy,
		CreatedAt:      createdAt,
		UpdatedAt:      createdAt,
	}, nil
}

// GetArtifact retrieves an artifact by ID including its current spec.
func GetArtifact(db *sql.DB, id string) (*Artifact, error) {
	row := db.QueryRow(`
		SELECT id, name, kind, status, current_version, spec, created_by, created_at, updated_at
		FROM compose_artifacts WHERE id = ?
	`, id)
	var a Artifact
	var spec []byte
	if err := row.Scan(&a.ID, &a.Name, &a.Kind, &a.Status, &a.CurrentVersion, &spec, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan artifact: %w", err)
	}
	a.Spec = spec
	return &a, nil
}

// ListArtifacts returns artifacts filtered by status and kind (empty = all).
func ListArtifacts(db *sql.DB, status, kind string) ([]*Artifact, error) {
	query := `SELECT id, name, kind, status, current_version, spec, created_by, created_at, updated_at FROM compose_artifacts WHERE 1=1`
	args := []any{}

	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	if kind != "" {
		query += ` AND kind = ?`
		args = append(args, kind)
	}
	query += ` ORDER BY created_at DESC`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query artifacts: %w", err)
	}
	defer rows.Close()

	var artifacts []*Artifact
	for rows.Next() {
		var a Artifact
		var spec []byte
		if err := rows.Scan(&a.ID, &a.Name, &a.Kind, &a.Status, &a.CurrentVersion, &spec, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan artifact: %w", err)
		}
		a.Spec = spec
		artifacts = append(artifacts, &a)
	}
	return artifacts, nil
}

// ListVersions returns all versions for an artifact.
func ListVersions(db *sql.DB, artifactID string) ([]*Version, error) {
	rows, err := db.Query(`
		SELECT artifact_id, version, spec, created_by, created_at
		FROM compose_artifact_versions
		WHERE artifact_id = ?
		ORDER BY version
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("query versions: %w", err)
	}
	defer rows.Close()

	var versions []*Version
	for rows.Next() {
		var v Version
		var spec []byte
		if err := rows.Scan(&v.ArtifactID, &v.Version, &spec, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		v.Spec = spec
		versions = append(versions, &v)
	}
	return versions, nil
}

// UpdateSpec creates a new version with the given spec.
func UpdateSpec(db *sql.DB, id string, spec json.RawMessage, actor string, now time.Time) (*Artifact, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Get current version
	var currentVersion int
	err = tx.QueryRow(`SELECT current_version FROM compose_artifacts WHERE id = ?`, id).Scan(&currentVersion)
	if err == sql.ErrNoRows {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("get current version: %w", err)
	}

	newVersion := currentVersion + 1
	createdAt := now.Unix()

	// Insert new version
	_, err = tx.Exec(`
		INSERT INTO compose_artifact_versions (artifact_id, version, spec, created_by, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, id, newVersion, spec, actor, createdAt)
	if err != nil {
		return nil, fmt.Errorf("insert new version: %w", err)
	}

	// Update artifact
	_, err = tx.Exec(`
		UPDATE compose_artifacts SET current_version = ?, spec = ?, updated_at = ? WHERE id = ?
	`, newVersion, spec, createdAt, id)
	if err != nil {
		return nil, fmt.Errorf("update artifact: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return GetArtifact(db, id)
}

// SetStatus updates the artifact status.
func SetStatus(db *sql.DB, id, status, actor string, now time.Time) (*Artifact, error) {
	nowUnix := now.Unix()
	res, err := db.Exec(`
		UPDATE compose_artifacts SET status = ?, updated_at = ? WHERE id = ?
	`, status, nowUnix, id)
	if err != nil {
		return nil, fmt.Errorf("update status: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("update status rows: %w", err)
	}
	if n == 0 {
		return nil, sql.ErrNoRows
	}
	return GetArtifact(db, id)
}

// Rollback creates a new version with the spec from the target version.
func Rollback(db *sql.DB, id string, targetVersion int, actor string, now time.Time) (*Artifact, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Get target version spec
	var spec []byte
	err = tx.QueryRow(`
		SELECT spec FROM compose_artifact_versions WHERE artifact_id = ? AND version = ?
	`, id, targetVersion).Scan(&spec)
	if err == sql.ErrNoRows {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("get target version spec: %w", err)
	}

	// Get current version
	var currentVersion int
	err = tx.QueryRow(`SELECT current_version FROM compose_artifacts WHERE id = ?`, id).Scan(&currentVersion)
	if err == sql.ErrNoRows {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("get current version: %w", err)
	}

	newVersion := currentVersion + 1
	createdAt := now.Unix()

	// Insert new version with target spec
	_, err = tx.Exec(`
		INSERT INTO compose_artifact_versions (artifact_id, version, spec, created_by, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, id, newVersion, spec, actor, createdAt)
	if err != nil {
		return nil, fmt.Errorf("insert rollback version: %w", err)
	}

	// Update artifact
	_, err = tx.Exec(`
		UPDATE compose_artifacts SET current_version = ?, spec = ?, updated_at = ? WHERE id = ?
	`, newVersion, spec, createdAt, id)
	if err != nil {
		return nil, fmt.Errorf("update artifact: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return GetArtifact(db, id)
}

// DeleteArtifact deletes an artifact and all its versions and runs.
func DeleteArtifact(db *sql.DB, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Delete runs
	_, err = tx.Exec(`DELETE FROM compose_runs WHERE artifact_id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete runs: %w", err)
	}

	// Delete versions
	_, err = tx.Exec(`DELETE FROM compose_artifact_versions WHERE artifact_id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete versions: %w", err)
	}

	// Delete artifact
	res, err := tx.Exec(`DELETE FROM compose_artifacts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete artifact: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		return sql.ErrNoRows
	}

	return tx.Commit()
}

// RecordRun records an execution run.
func RecordRun(db *sql.DB, run *Run) error {
	_, err := db.Exec(`
		INSERT INTO compose_runs (artifact_id, version, started_at, finished_at, result, detail)
		VALUES (?, ?, ?, ?, ?, ?)
	`, run.ArtifactID, run.Version, run.StartedAt, run.FinishedAt, run.Result, run.Detail)
	if err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	return nil
}

// LastFinishedRun returns the finished_at timestamp of the last finished run for an artifact.
func LastFinishedRun(db *sql.DB, artifactID string) (int64, bool) {
	var finishedAt int64
	err := db.QueryRow(`
		SELECT finished_at FROM compose_runs
		WHERE artifact_id = ? AND finished_at > 0
		ORDER BY finished_at DESC LIMIT 1
	`, artifactID).Scan(&finishedAt)
	if err == sql.ErrNoRows {
		return 0, false
	}
	if err != nil {
		return 0, false
	}
	return finishedAt, true
}