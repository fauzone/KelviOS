package catalog

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// BackupRepoRow is one configured restic repository (schema v31, FR-3.2).
type BackupRepoRow struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	TargetType string `json:"target_type"`
	TargetPath string `json:"target_path"`
	ConfigJSON string `json:"config_json,omitempty"` // non-secret params only
	SecretName string `json:"secret_name,omitempty"` // vault secret name holding secrets
	CreatedAt  int64  `json:"created_at"`
}

// BackupSnapshotRow is one recorded backup snapshot (schema v28, FR-3.1).
type BackupSnapshotRow struct {
	ID               string `json:"id"`
	RepoID           string `json:"repo_id"`
	CollectionID     string `json:"collection_id"`
	ResticSnapshotID string `json:"restic_snapshot_id"`
	CreatedAt        int64  `json:"created_at"`
}

func newBackupID(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixNano(), hex.EncodeToString(b))
}

// CreateBackupRepo inserts a repo row; duplicate name returns an error
// from the UNIQUE constraint (the caller maps it to 409).
func CreateBackupRepo(db *sql.DB, name, targetType, targetPath, configJSON, secretName string) (*BackupRepoRow, error) {
	row := &BackupRepoRow{
		ID:         newBackupID("brepo"),
		Name:       name,
		TargetType: targetType,
		TargetPath: targetPath,
		ConfigJSON: configJSON,
		SecretName: secretName,
		CreatedAt:  time.Now().Unix(),
	}
	_, err := db.Exec(`INSERT INTO backup_repos (id, name, target_type, target_path, config_json, secret_name, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		row.ID, row.Name, row.TargetType, row.TargetPath, row.ConfigJSON, row.SecretName, row.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create backup repo: %w", err)
	}
	return row, nil
}

func ListBackupRepos(db *sql.DB) ([]*BackupRepoRow, error) {
	rows, err := db.Query(`SELECT id, name, target_type, target_path, config_json, secret_name, created_at FROM backup_repos ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list backup repos: %w", err)
	}
	defer rows.Close()
	var out []*BackupRepoRow
	for rows.Next() {
		r := &BackupRepoRow{}
		if err := rows.Scan(&r.ID, &r.Name, &r.TargetType, &r.TargetPath, &r.ConfigJSON, &r.SecretName, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan backup repo: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func GetBackupRepo(db *sql.DB, id string) (*BackupRepoRow, error) {
	r := &BackupRepoRow{}
	err := db.QueryRow(`SELECT id, name, target_type, target_path, config_json, secret_name, created_at FROM backup_repos WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.TargetType, &r.TargetPath, &r.ConfigJSON, &r.SecretName, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get backup repo: %w", err)
	}
	return r, nil
}

func CreateBackupSnapshot(db *sql.DB, repoID, collectionID, resticSnapshotID string) (*BackupSnapshotRow, error) {
	row := &BackupSnapshotRow{
		ID:               newBackupID("bsnap"),
		RepoID:           repoID,
		CollectionID:     collectionID,
		ResticSnapshotID: resticSnapshotID,
		CreatedAt:        time.Now().Unix(),
	}
	_, err := db.Exec(`INSERT INTO backup_snapshots (id, repo_id, collection_id, restic_snapshot_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		row.ID, row.RepoID, row.CollectionID, row.ResticSnapshotID, row.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create backup snapshot: %w", err)
	}
	return row, nil
}

func ListBackupSnapshots(db *sql.DB, repoID string) ([]*BackupSnapshotRow, error) {
	rows, err := db.Query(`SELECT id, repo_id, collection_id, restic_snapshot_id, created_at FROM backup_snapshots WHERE repo_id = ? ORDER BY created_at, id`, repoID)
	if err != nil {
		return nil, fmt.Errorf("list backup snapshots: %w", err)
	}
	defer rows.Close()
	var out []*BackupSnapshotRow
	for rows.Next() {
		r := &BackupSnapshotRow{}
		if err := rows.Scan(&r.ID, &r.RepoID, &r.CollectionID, &r.ResticSnapshotID, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan backup snapshot: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}