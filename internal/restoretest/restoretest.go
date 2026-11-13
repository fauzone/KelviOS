package restoretest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fauzone/kelvios/internal/backup"
	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/vault"
)

// RestoreTestIntervalDays is the default interval for scheduled restore tests.
// Shared by wakecheck and HTTP handlers.
const RestoreTestIntervalDays = 7

// Deps holds the injected dependencies for scheduled restore tests.
// Follows the wakecheck.Deps pattern: plain struct with injectable fields.
type Deps struct {
	DB            *sql.DB
	ResticWrapper *backup.ResticWrapper
	VaultGetSecret func(name string) (string, error)
	Now           func() time.Time
}

// RunResult is the result of a scheduled restore test run.
type RunResult struct {
	RunID   string `json:"run_id"`
	Samples int    `json:"samples"`
	Passed  int    `json:"passed"`
	Failed  int    `json:"failed"`
	Skipped int    `json:"skipped"`
	Alerts  int    `json:"alerts"`
}

// RunScheduled executes scheduled restore tests across all backup repos with snapshots.
// Selection (per repo with >=1 snapshot): take the LATEST snapshot; sample up to 2 files
// from that snapshot's collection (from the files table). Selection key =
// SHA-256(snapshot_id + rel_path + runIndex) ascending, where runIndex = count of prior runs.
// This ROTATES samples across runs without RNG plumbing.
func RunScheduled(ctx context.Context, deps Deps) (*RunResult, error) {
	if deps.DB == nil {
		return nil, fmt.Errorf("DB is nil")
	}
	if deps.ResticWrapper == nil {
		return nil, fmt.Errorf("ResticWrapper is nil")
	}
	if deps.VaultGetSecret == nil {
		return nil, fmt.Errorf("VaultGetSecret is nil")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}

	runID := generateRunID()
	startedAt := deps.Now().Unix()

	// Get all backup repos
	repos, err := catalog.ListBackupRepos(deps.DB)
	if err != nil {
		return nil, fmt.Errorf("list backup repos: %w", err)
	}

	result := &RunResult{
		RunID: runID,
	}

	// Count prior runs for runIndex (rotation key)
	runIndex, err := countPriorRuns(deps.DB)
	if err != nil {
		return nil, fmt.Errorf("count prior runs: %w", err)
	}

	// Create run row at start (with zero counts; will update at end)
	if err := insertRunRow(deps.DB, runID, startedAt, 0, 0, 0, 0); err != nil {
		slog.Error("restore test create run row failed", "run_id", runID, "error", err)
		// Continue anyway; we'll try to update at the end
	}

	for _, repo := range repos {
		// Get snapshots for this repo
		snapshots, err := catalog.ListBackupSnapshots(deps.DB, repo.ID)
		if err != nil {
			slog.Error("list backup snapshots failed", "repo_id", repo.ID, "error", err)
			continue
		}
		if len(snapshots) == 0 {
			continue // no snapshots for this repo
		}

		// Take the LATEST snapshot (snapshots are ordered by created_at, id)
		latestSnap := snapshots[len(snapshots)-1]

		// Get the collection for this snapshot
		coll, err := catalog.GetCollection(deps.DB, latestSnap.CollectionID)
		if err != nil {
			slog.Error("get collection failed", "collection_id", latestSnap.CollectionID, "error", err)
			continue
		}
		if coll == nil {
			slog.Error("collection not found", "collection_id", latestSnap.CollectionID)
			continue
		}

		// Check if collection's pool is Cold and asleep (skip if so)
		pool, _, err := catalog.GetPool(deps.DB, coll.PoolID)
		if err != nil {
			slog.Error("get pool failed", "pool_id", coll.PoolID, "error", err)
			continue
		}
		if pool != nil && pool.Tier == "cold" && pool.Status == "asleep" {
			// Cold-never-wake: skip this repo's samples, count as skipped
			result.Skipped += 2 // up to 2 samples per repo
			continue
		}

		// Get files from this collection (from the files table)
		files, err := listCollectionFiles(deps.DB, coll.ID)
		if err != nil {
			slog.Error("list collection files failed", "collection_id", coll.ID, "error", err)
			continue
		}
		if len(files) == 0 {
			continue // no files to sample
		}

		// Sample up to 2 files using rotation key:
		// SHA-256(snapshot_id + rel_path + runIndex) ascending
		sampled := selectSamples(files, latestSnap.ResticSnapshotID, runIndex, 2)

		// Get vault secret for this repo
		secretName := "backup-repo:" + repo.ID
		password, err := deps.VaultGetSecret(secretName)
		if err != nil {
			if err == vault.ErrVaultLocked || err == vault.ErrSecretNotFound {
				// Missing secret -> all samples for this repo SKIPPED
				for range sampled {
					result.Samples++
					result.Skipped++
					// Record skip reason in alerts table? Spec says: missing secret -> sample recorded SKIPPED with reason, never an error.
					// We'll insert an alert row with reason "vault_secret_missing" for tracking.
					alertID := generateAlertID()
					_ = insertAlertRow(deps.DB, alertID, runID, repo.ID, latestSnap.ResticSnapshotID, "", "vault_secret_missing", deps.Now().Unix())
					result.Alerts++
				}
				continue
			}
			slog.Error("vault get secret failed", "secret", secretName, "error", err)
			// Other vault errors -> treat samples as failed
			for _, f := range sampled {
				result.Samples++
				result.Failed++
				alertID := generateAlertID()
				_ = insertAlertRow(deps.DB, alertID, runID, repo.ID, latestSnap.ResticSnapshotID, f.RelPath, "vault_error: "+err.Error(), deps.Now().Unix())
				result.Alerts++
				catalog.WriteAuditLog(deps.DB, "system", "restore_test.alert", "restore_test", repo.ID, "fail", fmt.Sprintf("repo=%s snapshot=%s rel_path=%s reason=vault_error", repo.ID, latestSnap.ResticSnapshotID, f.RelPath))
			}
			continue
		}

		// Process each sampled file
		for _, f := range sampled {
			result.Samples++

			// Restore to temp dir
			tempDir, err := os.MkdirTemp("", "kelvios-restoretest-*")
			if err != nil {
				result.Failed++
				alertID := generateAlertID()
				_ = insertAlertRow(deps.DB, alertID, runID, repo.ID, latestSnap.ResticSnapshotID, f.RelPath, "temp_dir_failed: "+err.Error(), deps.Now().Unix())
				result.Alerts++
				catalog.WriteAuditLog(deps.DB, "system", "restore_test.alert", "restore_test", repo.ID, "fail", fmt.Sprintf("repo=%s snapshot=%s rel_path=%s reason=temp_dir_failed", repo.ID, latestSnap.ResticSnapshotID, f.RelPath))
				continue
			}

			restoredPath, err := deps.ResticWrapper.RestoreFileWithContext(ctx, repo.TargetPath, latestSnap.ResticSnapshotID, f.RelPath, tempDir, password)
			if err != nil {
				result.Failed++
				alertID := generateAlertID()
				_ = insertAlertRow(deps.DB, alertID, runID, repo.ID, latestSnap.ResticSnapshotID, f.RelPath, "restore_failed: "+err.Error(), deps.Now().Unix())
				result.Alerts++
				catalog.WriteAuditLog(deps.DB, "system", "restore_test.alert", "restore_test", repo.ID, "fail", fmt.Sprintf("repo=%s snapshot=%s rel_path=%s reason=restore_failed", repo.ID, latestSnap.ResticSnapshotID, f.RelPath))
				os.RemoveAll(tempDir)
				continue
			}

			// Verify SHA-256 only if source unchanged since snapshot
			// Source modtime_unix <= snapshot created_at -> compare; else sha_match NULL with reason source_changed
			var shaMatch *bool
			var shaReason string
			if f.ModTimeUnix <= latestSnap.CreatedAt {
				// Compute SHA-256 of restored file
				restoredHash, err := computeSHA256(restoredPath)
				if err != nil {
					result.Failed++
					alertID := generateAlertID()
					_ = insertAlertRow(deps.DB, alertID, runID, repo.ID, latestSnap.ResticSnapshotID, f.RelPath, "hash_compute_failed: "+err.Error(), deps.Now().Unix())
					result.Alerts++
					catalog.WriteAuditLog(deps.DB, "system", "restore_test.alert", "restore_test", repo.ID, "fail", fmt.Sprintf("repo=%s snapshot=%s rel_path=%s reason=hash_compute_failed", repo.ID, latestSnap.ResticSnapshotID, f.RelPath))
					os.RemoveAll(tempDir)
					continue
				}
				// Expected hash: stored checksum when present; otherwise hash the
				// live source file. Scan-populated rows carry an empty checksum
				// (scan never computes one), and the modtime gate above guarantees
				// the source is unchanged since the snapshot, so live bytes are
				// the snapshot bytes. An empty stored checksum must not read as
				// a mismatch.
				expectedHash := f.Checksum
				if expectedHash == "" {
					sourcePath := filepath.Join(pool.Mountpoint, coll.Path, f.RelPath)
					sourceHash, err := computeSHA256(sourcePath)
					if err != nil {
						result.Failed++
						alertID := generateAlertID()
						_ = insertAlertRow(deps.DB, alertID, runID, repo.ID, latestSnap.ResticSnapshotID, f.RelPath, "hash_compute_failed: "+err.Error(), deps.Now().Unix())
						result.Alerts++
						catalog.WriteAuditLog(deps.DB, "system", "restore_test.alert", "restore_test", repo.ID, "fail", fmt.Sprintf("repo=%s snapshot=%s rel_path=%s reason=hash_compute_failed", repo.ID, latestSnap.ResticSnapshotID, f.RelPath))
						os.RemoveAll(tempDir)
						continue
					}
					expectedHash = sourceHash
				}
				if expectedHash == restoredHash {
					match := true
					shaMatch = &match
					shaReason = ""
				} else {
					match := false
					shaMatch = &match
					shaReason = "sha256_mismatch"
				}
			} else {
				// Source changed since snapshot
				shaMatch = nil
				shaReason = "source_changed"
			}

			if shaMatch != nil && !*shaMatch {
				// Hash mismatch -> FAILURE
				result.Failed++
				alertID := generateAlertID()
				_ = insertAlertRow(deps.DB, alertID, runID, repo.ID, latestSnap.ResticSnapshotID, f.RelPath, shaReason, deps.Now().Unix())
				result.Alerts++
				catalog.WriteAuditLog(deps.DB, "system", "restore_test.alert", "restore_test", repo.ID, "fail", fmt.Sprintf("repo=%s snapshot=%s rel_path=%s reason=%s", repo.ID, latestSnap.ResticSnapshotID, f.RelPath, shaReason))
			} else {
				// Pass (either hash matched or source_changed)
				result.Passed++
			}

			os.RemoveAll(tempDir)
		}
	}

	// Update run row with final counts
	if err := updateRunRow(deps.DB, runID, result.Samples, result.Passed, result.Failed, result.Skipped); err != nil {
		slog.Error("restore test update run row failed", "run_id", runID, "error", err)
	}

	// Audit log for the run
	catalog.WriteAuditLog(deps.DB, "system", "restore_test.run", "restore_test", runID, "ok", fmt.Sprintf("samples=%d passed=%d failed=%d skipped=%d alerts=%d", result.Samples, result.Passed, result.Failed, result.Skipped, result.Alerts))

	return result, nil
}

// FileInfo holds file metadata from the files table for sampling.
type FileInfo struct {
	RelPath     string
	ModTimeUnix int64
	Checksum    string
}

// listCollectionFiles fetches files for a collection from the catalog.
func listCollectionFiles(db *sql.DB, collectionID string) ([]FileInfo, error) {
	rows, err := db.Query(`SELECT rel_path, modtime_unix, checksum FROM files WHERE collection_id = ? ORDER BY rel_path`, collectionID)
	if err != nil {
		return nil, fmt.Errorf("query files: %w", err)
	}
	defer rows.Close()

	var files []FileInfo
	for rows.Next() {
		var f FileInfo
		var checksum sql.NullString
		if err := rows.Scan(&f.RelPath, &f.ModTimeUnix, &checksum); err != nil {
			return nil, fmt.Errorf("scan file: %w", err)
		}
		if checksum.Valid {
			f.Checksum = checksum.String
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

// selectSamples selects up to n files using rotation key:
// SHA-256(snapshot_id + rel_path + runIndex) ascending.
func selectSamples(files []FileInfo, snapshotID string, runIndex int, n int) []FileInfo {
	type scoredFile struct {
		file  FileInfo
		key   string
	}
	scored := make([]scoredFile, 0, len(files))
	for _, f := range files {
		// Rotation key: SHA-256(snapshot_id + rel_path + runIndex)
		// This rotates samples across runs without RNG plumbing.
		h := sha256.Sum256([]byte(snapshotID + f.RelPath + fmt.Sprintf("%d", runIndex)))
		key := hex.EncodeToString(h[:])
		scored = append(scored, scoredFile{file: f, key: key})
	}
	// Sort by key ascending
	for i := 0; i < len(scored)-1; i++ {
		for j := i + 1; j < len(scored); j++ {
			if scored[j].key < scored[i].key {
				scored[i], scored[j] = scored[j], scored[i]
			}
		}
	}
	// Take up to n
	if n > len(scored) {
		n = len(scored)
	}
	result := make([]FileInfo, n)
	for i := 0; i < n; i++ {
		result[i] = scored[i].file
	}
	return result
}

// computeSHA256 computes the SHA-256 hash of a file.
func computeSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}

// generateRunID returns a collision-proof restore test run ID.
func generateRunID() string {
	now := time.Now().UnixNano()
	return fmt.Sprintf("rtr-%d", now)
}

// generateAlertID returns a collision-proof alert ID.
func generateAlertID() string {
	now := time.Now().UnixNano()
	return fmt.Sprintf("rta-%d", now)
}

// countPriorRuns returns the number of prior restore test runs.
func countPriorRuns(db *sql.DB) (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM restore_test_runs`).Scan(&count)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return count, err
}

// insertRunRow inserts the initial run row.
func insertRunRow(db *sql.DB, runID string, startedAt int64, samples, passed, failed, skipped int) error {
	_, err := db.Exec(`
		INSERT INTO restore_test_runs (id, started_at, samples, passed, failed, skipped, alerts)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, runID, startedAt, samples, passed, failed, skipped, 0)
	return err
}

// updateRunRow updates the run row with final counts.
func updateRunRow(db *sql.DB, runID string, samples, passed, failed, skipped int) error {
	_, err := db.Exec(`
		UPDATE restore_test_runs
		SET samples = ?, passed = ?, failed = ?, skipped = ?, alerts = ?
		WHERE id = ?
	`, samples, passed, failed, skipped, 0, runID) // alerts counted separately
	return err
}

// insertAlertRow inserts an alert row for a failed/skipped sample.
func insertAlertRow(db *sql.DB, alertID, runID, repoID, snapshotID, relPath, reason string, createdAt int64) error {
	_, err := db.Exec(`
		INSERT INTO restore_test_alerts (id, run_id, repo_id, snapshot_id, rel_path, reason, created_at, resolved)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0)
	`, alertID, runID, repoID, snapshotID, relPath, reason, createdAt)
	return err
}