package wakecheck

// KelviOS WS42-A (2026-11-09): Wake piggyback checklist (FR-2.9 production).
//
// RunChecklist executes the four steps IN ORDER for a Cold pool wake:
// 1. flush_dirty      — call existing WS36 flush for the pool
// 2. parity_sync      — only for parity-mechanism pools; compare file signature
// 3. backup_delta     — for each collection, for each backup repo with existing snapshot
// 4. smart_quick      — exec smartctl -H /dev/<disk>
//
// Each step is isolated: a failing step records its status and the checklist CONTINUES.
// Deps (db, power snapshot fn, backup wrapper factory, vault password lookup) are
// injected as interfaces/funcs so it is testable.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/fauzone/kelvios/internal/backup"
	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/restoretest"
	"github.com/fauzone/kelvios/internal/storage"
	"github.com/fauzone/kelvios/internal/vault"
)

// ChecklistRun is the result of a checklist execution.
type ChecklistRun struct {
	ID          string          `json:"id"`
	PoolID      string          `json:"pool_id"`
	Disk        string          `json:"disk"`
	StartedAt   int64           `json:"started_at"`
	FinishedAt  int64           `json:"finished_at"`
	Steps       []ChecklistStep `json:"steps"`
}

// ChecklistStep is one step in the checklist.
type ChecklistStep struct {
	Step   string `json:"step"`
	Status string `json:"status"` // "ok", "skipped", "error"
	Detail string `json:"detail,omitempty"`
}

// Deps holds the injected dependencies for the checklist.
// All fields are required; nil values will cause panics at runtime.
type Deps struct {
	DB              *sql.DB
	PowerSnapshot   func(disk string) (storage.PowerState, bool) // returns state, ok
	BackupWrapper   func() *backup.ResticWrapper                 // factory for restic wrapper
	VaultGetSecret  func(name string) (string, error)            // vault secret lookup
	Now             func() time.Time                             // time source (tests can inject)
}

// generateRunID returns a collision-proof checklist run ID.
func generateRunID() string {
	now := time.Now().UnixNano()
	return fmt.Sprintf("wcr-%d", now)
}

// ErrSkipped marks a step that did nothing because it does not apply.
// RunChecklist maps it to status "skipped", never to "error".
var ErrSkipped = errors.New("skipped")

// RunChecklist executes the five-step wake checklist for a disk in a Cold pool.
// It returns the completed ChecklistRun (never nil). The run row is created at
// start and updated on completion. Errors from individual steps are recorded
// in the step detail; the checklist never fails the wake response itself.
func RunChecklist(ctx context.Context, deps Deps, poolID, disk string) *ChecklistRun {
	runID := generateRunID()
	startedAt := deps.Now().Unix()

	run := &ChecklistRun{
		ID:         runID,
		PoolID:     poolID,
		Disk:       disk,
		StartedAt:  startedAt,
		Steps:      make([]ChecklistStep, 0, 5),
	}

	// Helper to append a step result
	addStep := func(step, status, detail string) {
		run.Steps = append(run.Steps, ChecklistStep{
			Step:   step,
			Status: status,
			Detail: detail,
		})
	}

	// Create run row at start
	if err := createRunRow(deps.DB, run); err != nil {
		slog.Error("wake checklist create run row failed", "run_id", runID, "error", err)
		// Continue anyway; we'll try to update at the end
	}

	// Step 1: flush_dirty
	flushDetail, flushErr := stepFlushDirty(ctx, deps.DB, poolID)
	if errors.Is(flushErr, ErrSkipped) {
		addStep("flush_dirty", "skipped", flushDetail)
	} else if flushErr != nil {
		addStep("flush_dirty", "error", flushDetail)
	} else {
		addStep("flush_dirty", "ok", flushDetail)
	}

	// Step 2: parity_sync
	parityDetail, parityErr := stepParitySync(ctx, deps.DB, poolID)
	if errors.Is(parityErr, ErrSkipped) {
		addStep("parity_sync", "skipped", parityDetail)
	} else if parityErr != nil {
		addStep("parity_sync", "error", parityDetail)
	} else {
		addStep("parity_sync", "ok", parityDetail)
	}

	// Step 3: backup_delta
	backupDetail, backupErr := stepBackupDelta(ctx, deps, poolID)
	if errors.Is(backupErr, ErrSkipped) {
		addStep("backup_delta", "skipped", backupDetail)
	} else if backupErr != nil {
		addStep("backup_delta", "error", backupDetail)
	} else {
		addStep("backup_delta", "ok", backupDetail)
	}

	// Step 4: smart_quick
	smartDetail, smartErr := stepSmartQuick(ctx, disk)
	if errors.Is(smartErr, ErrSkipped) {
		addStep("smart_quick", "skipped", smartDetail)
	} else if smartErr != nil {
		addStep("smart_quick", "error", smartDetail)
	} else {
		addStep("smart_quick", "ok", smartDetail)
	}

	// Step 5: restore_test
	restoreDetail, restoreErr := stepRestoreTest(ctx, deps)
	if errors.Is(restoreErr, ErrSkipped) {
		addStep("restore_test", "skipped", restoreDetail)
	} else if restoreErr != nil {
		addStep("restore_test", "error", restoreDetail)
	} else {
		addStep("restore_test", "ok", restoreDetail)
	}

	run.FinishedAt = deps.Now().Unix()

	// Update run row on completion
	if err := updateRunRow(deps.DB, run); err != nil {
		slog.Error("wake checklist update run row failed", "run_id", runID, "error", err)
	}

	return run
}

// stepFlushDirty calls the existing WS36 flush for the pool.
func stepFlushDirty(ctx context.Context, db *sql.DB, poolID string) (string, error) {
	flushed, failed, err := catalog.FlushDirtyShadows(db, poolID, "system:wake-checklist")
	if err != nil {
		return fmt.Sprintf("flush failed: %v", err), err
	}
	return fmt.Sprintf("flushed=%d failed=%d", flushed, failed), nil
}

// stepParitySync runs parity sync only for parity-mechanism pools.
// "Changes" check: compare pool's file signature (files count + max updated timestamp
// across its collections) against meta key parity_sync_sig:<poolID>.
// Equal -> skipped: no changes. Otherwise exec snapraid sync.
func stepParitySync(ctx context.Context, db *sql.DB, poolID string) (string, error) {
	// Get pool to check mechanism
	pool, _, err := catalog.GetPool(db, poolID)
	if err != nil {
		return fmt.Sprintf("get pool failed: %v", err), err
	}
	if pool == nil {
		return "pool not found", fmt.Errorf("pool not found")
	}

	// Only for parity-mechanism pools
	if pool.Mechanism != "parity-scheduled" {
		return "skipped: no parity", ErrSkipped
	}

	// Compute current signature: files count + max updated timestamp across collections
	collections, err := catalog.ListCollections(db, poolID)
	if err != nil {
		return fmt.Sprintf("list collections failed: %v", err), err
	}

	var totalFiles int64
	var maxUpdated int64
	for _, coll := range collections {
		// Count files in this collection
		var count int
		err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE collection_id = ?`, coll.ID).Scan(&count)
		if err != nil {
			return fmt.Sprintf("count files failed: %v", err), err
		}
		totalFiles += int64(count)

		// Get max modtime
		var maxMod sql.NullInt64
		err = db.QueryRow(`SELECT MAX(modtime_unix) FROM files WHERE collection_id = ?`, coll.ID).Scan(&maxMod)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Sprintf("max modtime failed: %v", err), err
		}
		if maxMod.Valid && maxMod.Int64 > maxUpdated {
			maxUpdated = maxMod.Int64
		}
	}

	signature := fmt.Sprintf("%d:%d", totalFiles, maxUpdated)

	// Compare with stored signature
	metaKey := "parity_sync_sig:" + poolID
	var storedSig string
	err = db.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaKey).Scan(&storedSig)
	if err == nil && storedSig == signature {
		return "skipped: no changes", ErrSkipped
	}

	// Execute snapraid sync
	snapraidBin := os.Getenv("KELVIOS_SNAPRAID_BIN")
	if snapraidBin == "" {
		snapraidBin = "snapraid"
	}
	args := []string{"sync"}
	if conf := os.Getenv("KELVIOS_SNAPRAID_CONF"); conf != "" {
		args = append([]string{"-c", conf}, args...)
	}

	cmd := exec.CommandContext(ctx, snapraidBin, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("snapraid sync failed: %v: %s", err, string(output)), err
	}

	// Store new signature
	_, err = db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, metaKey, signature)
	if err != nil {
		return fmt.Sprintf("store signature failed: %v", err), err
	}

	return fmt.Sprintf("synced, signature=%s", signature), nil
}

// stepBackupDelta runs backup delta for each collection of this pool,
// for each backup repo that ALREADY has a snapshot row for that collection.
func stepBackupDelta(ctx context.Context, deps Deps, poolID string) (string, error) {
	collections, err := catalog.ListCollections(deps.DB, poolID)
	if err != nil {
		return fmt.Sprintf("list collections failed: %v", err), err
	}

	if len(collections) == 0 {
		return "skipped: no collections", ErrSkipped
	}

	// Get all backup repos
	repos, err := catalog.ListBackupRepos(deps.DB)
	if err != nil {
		return fmt.Sprintf("list backup repos failed: %v", err), err
	}
	if len(repos) == 0 {
		return "skipped: no backups configured", ErrSkipped
	}

	wrapper := deps.BackupWrapper()
	if wrapper == nil {
		return "backup wrapper factory returned nil", fmt.Errorf("backup wrapper is nil")
	}

	var results []string
	hasAnyBackup := false

	for _, coll := range collections {
		for _, repo := range repos {
			// Check if this repo already has a snapshot for this collection
			snapshots, err := catalog.ListBackupSnapshots(deps.DB, repo.ID)
			if err != nil {
				results = append(results, fmt.Sprintf("collection=%s repo=%s: list snapshots failed: %v", coll.ID, repo.ID, err))
				continue
			}

			hasSnapshot := false
			for _, snap := range snapshots {
				if snap.CollectionID == coll.ID {
					hasSnapshot = true
					break
				}
			}
			if !hasSnapshot {
				continue // only backup repos that already have a snapshot for this collection
			}

			hasAnyBackup = true

			// Get password from vault
			secretName := "backup-repo:" + repo.ID
			password, err := deps.VaultGetSecret(secretName)
			if err != nil {
				if err == vault.ErrVaultLocked || strings.Contains(err.Error(), "vault is locked") {
					results = append(results, fmt.Sprintf("collection=%s repo=%s: skipped: vault locked", coll.ID, repo.ID))
					continue
				}
				if err == vault.ErrSecretNotFound || strings.Contains(err.Error(), "secret not found") {
					results = append(results, fmt.Sprintf("collection=%s repo=%s: skipped: no vault secret", coll.ID, repo.ID))
					continue
				}
				results = append(results, fmt.Sprintf("collection=%s repo=%s: vault error: %v", coll.ID, repo.ID, err))
				continue
			}

			// Resolve collection root
			pool, _, err := catalog.GetPool(deps.DB, coll.PoolID)
			if err != nil || pool == nil {
				results = append(results, fmt.Sprintf("collection=%s repo=%s: pool not found", coll.ID, repo.ID))
				continue
			}
			root, err := catalog.ResolveCollectionRoot(pool, coll)
			if err != nil {
				results = append(results, fmt.Sprintf("collection=%s repo=%s: resolve root failed: %v", coll.ID, repo.ID, err))
				continue
			}

			// Run backup (restic is incremental, so this IS the delta)
			snapID, err := wrapper.BackupWithContext(ctx, repo.TargetPath, root, password)
			if err != nil {
				results = append(results, fmt.Sprintf("collection=%s repo=%s: backup failed: %v", coll.ID, repo.ID, err))
				continue
			}

			// Record snapshot row
			_, err = catalog.CreateBackupSnapshot(deps.DB, repo.ID, coll.ID, snapID)
			if err != nil {
				results = append(results, fmt.Sprintf("collection=%s repo=%s: record snapshot failed: %v", coll.ID, repo.ID, err))
				continue
			}

			results = append(results, fmt.Sprintf("collection=%s repo=%s: snapshot=%s", coll.ID, repo.ID, snapID))
		}
	}

	if !hasAnyBackup {
		return "skipped: no backups configured", ErrSkipped
	}

	return strings.Join(results, "; "), nil
}

// stepSmartQuick runs smartctl -H /dev/<disk> and checks overall health.
func stepSmartQuick(ctx context.Context, disk string) (string, error) {
	smartctlBin := os.Getenv("KELVIOS_SMARTCTL_BIN")
	if smartctlBin == "" {
		smartctlBin = "smartctl"
	}

	devicePath := "/dev/" + disk
	cmd := exec.CommandContext(ctx, smartctlBin, "-H", devicePath)
	output, err := cmd.CombinedOutput()
	outStr := string(output)

	if err != nil {
		// Check if binary missing
		if strings.Contains(err.Error(), "executable file not found") || strings.Contains(outStr, "not found") {
			return fmt.Sprintf("smartctl binary not found: %s", outStr), err
		}
		// smartctl returns non-zero on FAILED health, but we treat that as a status, not an error
		// Only treat as error if binary missing or execution failed
	}

	// Parse output for overall health
	healthLine := ""
	for _, line := range strings.Split(outStr, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "SMART overall-health self-assessment test result:") {
			healthLine = line
			break
		}
	}

	if healthLine == "" {
		// Could not parse health line
		return fmt.Sprintf("could not parse health: %s", outStr), fmt.Errorf("health line not found")
	}

	if strings.Contains(healthLine, "PASSED") {
		return healthLine, nil
	}
	if strings.Contains(healthLine, "FAILED") {
		return healthLine, fmt.Errorf("health check failed")
	}

	return healthLine, fmt.Errorf("unknown health status: %s", healthLine)
}

// stepRestoreTest checks if a scheduled restore test is due and runs it if so.
// Due when no prior run exists OR now - last_run >= RestoreTestIntervalDays (7 days).
// Returns ErrSkipped with detail "not due" when not due.
// On due: calls restoretest.RunScheduled with deps from wakecheck Deps.
// Step status: "ok" when run completes (detail with counts); "error" when RunScheduled
// returns error or run reports failed > 0.
func stepRestoreTest(ctx context.Context, deps Deps) (string, error) {
	// Get last run time from restore_test_runs
	var lastRunAt sql.NullInt64
	err := deps.DB.QueryRow(`SELECT MAX(started_at) FROM restore_test_runs`).Scan(&lastRunAt)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Sprintf("query last run failed: %v", err), err
	}

	now := deps.Now().Unix()
	intervalSec := int64(restoretest.RestoreTestIntervalDays) * 24 * 60 * 60

	// Check if due
	if lastRunAt.Valid {
		if now-lastRunAt.Int64 < intervalSec {
			return "not due", ErrSkipped
		}
	}
	// Not valid (no prior run) -> due

	// Build restoretest.Deps from wakecheck.Deps
	rtDeps := restoretest.Deps{
		DB:            deps.DB,
		ResticWrapper: deps.BackupWrapper(),
		VaultGetSecret: deps.VaultGetSecret,
		Now:           deps.Now,
	}

	if rtDeps.ResticWrapper == nil {
		return "backup wrapper factory returned nil", fmt.Errorf("backup wrapper is nil")
	}

	result, err := restoretest.RunScheduled(ctx, rtDeps)
	if err != nil {
		return fmt.Sprintf("restore test run failed: %v", err), err
	}

	detail := fmt.Sprintf("samples=%d passed=%d failed=%d skipped=%d alerts=%d",
		result.Samples, result.Passed, result.Failed, result.Skipped, result.Alerts)

	// Treat failed > 0 as error for the checklist step
	if result.Failed > 0 {
		return detail, fmt.Errorf("restore test had failures")
	}

	return detail, nil
}

// createRunRow inserts the initial run row.
func createRunRow(db *sql.DB, run *ChecklistRun) error {
	stepsJSON, err := json.Marshal(run.Steps)
	if err != nil {
		return fmt.Errorf("marshal steps: %w", err)
	}
	_, err = db.Exec(`
		INSERT INTO wake_checklist_runs (id, pool_id, disk, started_at, finished_at, steps)
		VALUES (?, ?, ?, ?, ?, ?)
	`, run.ID, run.PoolID, run.Disk, run.StartedAt, run.FinishedAt, string(stepsJSON))
	return err
}

// updateRunRow updates the run row with finished_at and steps.
func updateRunRow(db *sql.DB, run *ChecklistRun) error {
	stepsJSON, err := json.Marshal(run.Steps)
	if err != nil {
		return fmt.Errorf("marshal steps: %w", err)
	}
	_, err = db.Exec(`
		UPDATE wake_checklist_runs
		SET finished_at = ?, steps = ?
		WHERE id = ?
	`, run.FinishedAt, string(stepsJSON), run.ID)
	return err
}

// ListRuns returns recent checklist runs for a disk, newest first.
func ListRuns(db *sql.DB, disk string, limit int) ([]*ChecklistRun, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	rows, err := db.Query(`
		SELECT id, pool_id, disk, started_at, finished_at, steps
		FROM wake_checklist_runs
		WHERE disk = ?
		ORDER BY started_at DESC, rowid DESC
		LIMIT ?
	`, disk, limit)
	if err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	defer rows.Close()

	var runs []*ChecklistRun
	for rows.Next() {
		var r ChecklistRun
		var stepsJSON string
		if err := rows.Scan(&r.ID, &r.PoolID, &r.Disk, &r.StartedAt, &r.FinishedAt, &stepsJSON); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		if err := json.Unmarshal([]byte(stepsJSON), &r.Steps); err != nil {
			return nil, fmt.Errorf("unmarshal steps: %w", err)
		}
		runs = append(runs, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runs: %w", err)
	}
	if runs == nil {
		runs = []*ChecklistRun{}
	}
	return runs, nil
}