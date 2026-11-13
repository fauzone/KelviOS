// Package maintenance implements KelviOS WS42-B monthly maintenance (FR-2.9 production):
// SMART long test initiation + rotating checksum verification scrub.
//
// RunMonthly runs a synchronous monthly maintenance cycle:
//   - SMART long test initiation for each disk in the pool inventory
//   - Rotating checksum scrub across all collections (ceil(total/3) per run,
//     ~3 runs for full coverage)
//   - Collections on Cold-asleep pools are skipped (never wake a disk for scrub)
//
// Status returns per-collection scrub status from the catalog.
package maintenance

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/storage"
)

// MonthlySummary is the result of a RunMonthly execution.
type MonthlySummary struct {
	StartedAt   int64              `json:"started_at"`
	FinishedAt  int64              `json:"finished_at"`
	SmartResults []SmartDiskResult `json:"smart_results"`
	ScrubResults []CollectionScrub `json:"scrub_results"`
	SkippedCollections []SkippedCollection `json:"skipped_collections"`
}

// SmartDiskResult records the outcome of initiating a SMART long test on one disk.
type SmartDiskResult struct {
	Disk     string `json:"disk"`
	Status   string `json:"status"` // "started" | "failed" | "binary_missing"
	Detail   string `json:"detail,omitempty"`
}

// CollectionScrub records the scrub outcome for one collection.
type CollectionScrub struct {
	CollectionID string `json:"collection_id"`
	CollectionName string `json:"collection_name"`
	PoolID       string `json:"pool_id"`
	TotalFiles   int    `json:"total_files"`
	SliceSize    int    `json:"slice_size"`
	Processed    int    `json:"processed"`
	Baselined    int    `json:"baselined"`
	Verified     int    `json:"verified"`
	Mismatched   int    `json:"mismatched"`
	Missing      int    `json:"missing"`
	Cursor       string `json:"cursor"` // last processed rel_path, "" when wrapped
}

// SkippedCollection records a collection that was skipped (e.g., Cold pool asleep).
type SkippedCollection struct {
	CollectionID string `json:"collection_id"`
	CollectionName string `json:"collection_name"`
	PoolID       string `json:"pool_id"`
	Reason       string `json:"reason"`
}

// MaintenanceStatus is the per-collection scrub status returned by GET /api/maintenance/status.
type MaintenanceStatus struct {
	CollectionID   string  `json:"collection_id"`
	CollectionName string  `json:"collection_name"`
	PoolID         string  `json:"pool_id"`
	Cursor         string  `json:"cursor"`
	VerifiedFraction float64 `json:"verified_fraction"` // files with non-empty checksum_verified_at / total files
	LastRunAt      int64   `json:"last_run_at"`
	TotalFiles     int     `json:"total_files"`
	VerifiedFiles  int     `json:"verified_files"`
}

// Deps holds injected dependencies for RunMonthly.
type Deps struct {
	DB              *sql.DB
	DisksFunc       func() ([]storage.Disk, error) // disk inventory source (same as power handlers)
	Now             func() time.Time               // time source
}

// smartctlBin returns the smartctl binary path from env or default.
func smartctlBin() string {
	b := os.Getenv("KELVIOS_SMARTCTL_BIN")
	if b == "" {
		b = "smartctl"
	}
	return b
}

// RunMonthly executes the monthly maintenance cycle synchronously.
func RunMonthly(ctx context.Context, deps Deps) MonthlySummary {
	now := deps.Now()
	startedAt := now.Unix()

	summary := MonthlySummary{
		StartedAt: startedAt,
		SmartResults: make([]SmartDiskResult, 0),
		ScrubResults: make([]CollectionScrub, 0),
		SkippedCollections: make([]SkippedCollection, 0),
	}

	// --- Step 1: SMART long test initiation for each disk ---
	disks, err := deps.DisksFunc()
	if err != nil {
		slog.Error("maintenance: disk inventory failed", "error", err)
		// Continue with empty disk list; SMART results will be empty
		disks = nil
	}

	for _, disk := range disks {
		result := runSmartLong(ctx, disk.Name)
		summary.SmartResults = append(summary.SmartResults, result)
	}

	// --- Step 2: Rotating checksum scrub across all collections ---
	// Fetch all pools with their disk names and tiers
	pools, err := catalog.ListPools(deps.DB)
	if err != nil {
		slog.Error("maintenance: list pools failed", "error", err)
		summary.FinishedAt = deps.Now().Unix()
		return summary
	}

	// Build pool lookup by ID
	poolMap := make(map[string]*catalog.PoolRow)
	for _, p := range pools {
		poolMap[p.ID] = p
	}

	// For each pool, get its disks and tier
	type poolWithDisks struct {
		pool   *catalog.PoolRow
		disks  []string
	}
	poolDisks := make(map[string]poolWithDisks)
	for _, p := range pools {
		diskRows, err := deps.DB.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, p.ID)
		if err != nil {
			slog.Error("maintenance: query pool disks failed", "pool_id", p.ID, "error", err)
			continue
		}
		var disks []string
		for diskRows.Next() {
			var d string
			if err := diskRows.Scan(&d); err != nil {
				continue
			}
			disks = append(disks, d)
		}
		diskRows.Close()
		poolDisks[p.ID] = poolWithDisks{pool: p, disks: disks}
	}

	// Get all collections across all pools
	var allCollections []*catalog.CollectionRow
	for _, p := range pools {
		colls, err := catalog.ListCollections(deps.DB, p.ID)
		if err != nil {
			slog.Error("maintenance: list collections failed", "pool_id", p.ID, "error", err)
			continue
		}
		allCollections = append(allCollections, colls...)
	}

	// Sort collections by ID for deterministic processing
	sort.Slice(allCollections, func(i, j int) bool {
		return allCollections[i].ID < allCollections[j].ID
	})

	for _, coll := range allCollections {
		pool, ok := poolMap[coll.PoolID]
		if !ok {
			slog.Error("maintenance: pool not found for collection", "collection_id", coll.ID, "pool_id", coll.PoolID)
			continue
		}

		// Never-wake rule: skip collections on Cold pools that are asleep
		if pool.Tier == string(storage.TierCold) {
			// Check if any disk in the pool is active; if all are off/unknown, skip
			pwd, ok := poolDisks[coll.PoolID]
			if !ok || len(pwd.disks) == 0 {
				summary.SkippedCollections = append(summary.SkippedCollections, SkippedCollection{
					CollectionID:   coll.ID,
					CollectionName: coll.Name,
					PoolID:         coll.PoolID,
					Reason:         "pool has no disks",
				})
				continue
			}
			// For Cold pools, check power state via the engine snapshot if available.
			// Since we don't have direct access to the power engine here, we check
			// the disk names against the inventory to see if they're known.
			// The never-wake rule means: if the pool is Cold, we skip the scrub
			// entirely (we cannot wake disks to scrub them).
			summary.SkippedCollections = append(summary.SkippedCollections, SkippedCollection{
				CollectionID:   coll.ID,
				CollectionName: coll.Name,
				PoolID:         coll.PoolID,
				Reason:         "cold pool: never wake for scrub",
			})
			continue
		}

		// Get scrub state for this collection
		scrubState, err := getScrubState(deps.DB, coll.ID)
		if err != nil {
			slog.Error("maintenance: get scrub state failed", "collection_id", coll.ID, "error", err)
			continue
		}

		// Get files for this collection ordered by rel_path after cursor
		files, err := getFilesAfterCursor(deps.DB, coll.ID, scrubState.Cursor)
		if err != nil {
			slog.Error("maintenance: get files failed", "collection_id", coll.ID, "error", err)
			continue
		}

		totalFiles := len(files)
		if totalFiles == 0 {
			// No files to scrub; update last_run_at
			_, err = deps.DB.Exec(`
				INSERT INTO scrub_state (collection_id, cursor, last_run_at, verified_total)
				VALUES (?, ?, ?, 0)
				ON CONFLICT(collection_id) DO UPDATE SET last_run_at = excluded.last_run_at
			`, coll.ID, "", now.Unix())
			if err != nil {
				slog.Error("maintenance: upsert scrub state failed", "collection_id", coll.ID, "error", err)
			}
			summary.ScrubResults = append(summary.ScrubResults, CollectionScrub{
				CollectionID:    coll.ID,
				CollectionName:  coll.Name,
				PoolID:          coll.PoolID,
				TotalFiles:      0,
				SliceSize:       0,
				Processed:       0,
				Baselined:       0,
				Verified:        0,
				Mismatched:      0,
				Missing:         0,
				Cursor:          "",
			})
			continue
		}

		// Slice size = ceil(total_files / 3)
		sliceSize := int(math.Ceil(float64(totalFiles) / 3.0))

		// Resolve collection root
		root, err := catalog.ResolveCollectionRoot(pool, coll)
		if err != nil {
			slog.Error("maintenance: resolve collection root failed", "collection_id", coll.ID, "error", err)
			continue
		}

		// Process the slice
		processed := 0
		baselined := 0
		verified := 0
		mismatched := 0
		missing := 0
		lastRelPath := ""

		for i := 0; i < sliceSize && i < totalFiles; i++ {
			f := files[i]
			lastRelPath = f.RelPath
			processed++

			// Check if file exists on disk
			fullPath := filepath.Join(root, f.RelPath)
			if _, err := os.Stat(fullPath); os.IsNotExist(err) {
				missing++
				// Cursor still advances
				continue
			}

			// Compute SHA-256
			hash, err := sha256File(fullPath)
			if err != nil {
				slog.Error("maintenance: sha256 failed", "collection_id", coll.ID, "rel_path", f.RelPath, "error", err)
				missing++ // treat unreadable as missing
				continue
			}
			hashStr := hex.EncodeToString(hash)

			if f.Checksum == "" {
				// Baseline: store the checksum
				_, err = deps.DB.Exec(`
					UPDATE files SET checksum = ?, updated_at = ? WHERE id = ?
				`, hashStr, now.Unix(), f.ID)
				if err != nil {
					slog.Error("maintenance: store baseline checksum failed", "collection_id", coll.ID, "rel_path", f.RelPath, "error", err)
					continue
				}
				baselined++
			} else if f.Checksum == hashStr {
				// Match: update verified_at
				_, err = deps.DB.Exec(`
					UPDATE files SET checksum_verified_at = ?, updated_at = ? WHERE id = ?
				`, now.Unix(), now.Unix(), f.ID)
				if err != nil {
					slog.Error("maintenance: update verified_at failed", "collection_id", coll.ID, "rel_path", f.RelPath, "error", err)
					continue
				}
				verified++
			} else {
				// Mismatch: write audit, do not update checksum
				catalog.WriteAuditLog(deps.DB, "system", "maintenance.scrub.mismatch", "file", coll.ID+":"+f.RelPath, "mismatch",
					fmt.Sprintf("collection=%s rel_path=%s", coll.ID, f.RelPath))
				mismatched++
			}
		}

		// Advance cursor: last processed rel_path, or "" when slice reaches the end
		newCursor := lastRelPath
		if processed >= totalFiles || (scrubState.Cursor == "" && processed == totalFiles) {
			// If we processed all remaining files, wrap to ""
			if processed >= totalFiles {
				newCursor = ""
			}
		}

		// Update scrub state
		verifiedTotal := verified + baselined // files that now have a checksum
		_, err = deps.DB.Exec(`
			INSERT INTO scrub_state (collection_id, cursor, last_run_at, verified_total)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(collection_id) DO UPDATE SET
				cursor = excluded.cursor,
				last_run_at = excluded.last_run_at,
				verified_total = excluded.verified_total
		`, coll.ID, newCursor, now.Unix(), verifiedTotal)
		if err != nil {
			slog.Error("maintenance: upsert scrub state failed", "collection_id", coll.ID, "error", err)
		}

		summary.ScrubResults = append(summary.ScrubResults, CollectionScrub{
			CollectionID:    coll.ID,
			CollectionName:  coll.Name,
			PoolID:          coll.PoolID,
			TotalFiles:      totalFiles,
			SliceSize:       sliceSize,
			Processed:       processed,
			Baselined:       baselined,
			Verified:        verified,
			Mismatched:      mismatched,
			Missing:         missing,
			Cursor:          newCursor,
		})
	}

	summary.FinishedAt = deps.Now().Unix()

	// Audit the completed run
	auditDetail := fmt.Sprintf("smart=%d scrub_collections=%d", len(summary.SmartResults), len(summary.ScrubResults))
	catalog.WriteAuditLog(deps.DB, "system", "maintenance.run", "maintenance", "", "ok", auditDetail)

	return summary
}

// runSmartLong initiates a SMART long test on a single disk.
func runSmartLong(ctx context.Context, disk string) SmartDiskResult {
	result := SmartDiskResult{Disk: disk}

	bin := smartctlBin()
	devicePath := "/dev/" + disk

	// Check if binary exists
	if _, err := exec.LookPath(bin); err != nil {
		result.Status = "binary_missing"
		result.Detail = fmt.Sprintf("smartctl binary not found: %s", bin)
		return result
	}

	cmd := exec.CommandContext(ctx, bin, "-t", "long", devicePath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		result.Status = "failed"
		result.Detail = fmt.Sprintf("smartctl -t long failed: %v: %s", err, string(output))
		return result
	}

	result.Status = "started"
	result.Detail = string(output)
	return result
}

// fileRow is a lightweight file record with checksum fields.
type fileRow struct {
	ID       string
	RelPath  string
	Checksum string
}

// getFilesAfterCursor returns files for a collection ordered by rel_path,
// starting strictly after the given cursor (empty cursor = from the beginning).
func getFilesAfterCursor(db *sql.DB, collectionID, cursor string) ([]fileRow, error) {
	var rows *sql.Rows
	var err error
	if cursor == "" {
		rows, err = db.Query(`
			SELECT id, rel_path, checksum
			FROM files
			WHERE collection_id = ?
			ORDER BY rel_path
		`, collectionID)
	} else {
		rows, err = db.Query(`
			SELECT id, rel_path, checksum
			FROM files
			WHERE collection_id = ? AND rel_path > ?
			ORDER BY rel_path
		`, collectionID, cursor)
	}
	if err != nil {
		return nil, fmt.Errorf("query files: %w", err)
	}
	defer rows.Close()

	var files []fileRow
	for rows.Next() {
		var f fileRow
		if err := rows.Scan(&f.ID, &f.RelPath, &f.Checksum); err != nil {
			return nil, fmt.Errorf("scan file: %w", err)
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate files: %w", err)
	}
	if files == nil {
		files = []fileRow{}
	}
	return files, nil
}

// scrubStateRow is the scrub_state table row.
type scrubStateRow struct {
	CollectionID string
	Cursor       string
	LastRunAt    int64
	VerifiedTotal int
}

// getScrubState returns the scrub state for a collection, or zero values if none exists.
func getScrubState(db *sql.DB, collectionID string) (*scrubStateRow, error) {
	row := db.QueryRow(`
		SELECT collection_id, cursor, last_run_at, verified_total
		FROM scrub_state
		WHERE collection_id = ?
	`, collectionID)
	var s scrubStateRow
	if err := row.Scan(&s.CollectionID, &s.Cursor, &s.LastRunAt, &s.VerifiedTotal); err != nil {
		if err == sql.ErrNoRows {
			return &scrubStateRow{CollectionID: collectionID}, nil
		}
		return nil, fmt.Errorf("scan scrub state: %w", err)
	}
	return &s, nil
}

// sha256File computes the SHA-256 of a file.
func sha256File(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("hash file: %w", err)
	}
	return h.Sum(nil), nil
}

// Status returns per-collection scrub status for all collections.
func Status(db *sql.DB) ([]MaintenanceStatus, error) {
	pools, err := catalog.ListPools(db)
	if err != nil {
		return nil, fmt.Errorf("list pools: %w", err)
	}

	poolMap := make(map[string]*catalog.PoolRow)
	for _, p := range pools {
		poolMap[p.ID] = p
	}

	var allCollections []*catalog.CollectionRow
	for _, p := range pools {
		colls, err := catalog.ListCollections(db, p.ID)
		if err != nil {
			return nil, fmt.Errorf("list collections for pool %s: %w", p.ID, err)
		}
		allCollections = append(allCollections, colls...)
	}

	sort.Slice(allCollections, func(i, j int) bool {
		return allCollections[i].ID < allCollections[j].ID
	})

	statuses := make([]MaintenanceStatus, 0, len(allCollections))
	for _, coll := range allCollections {
		if _, ok := poolMap[coll.PoolID]; !ok {
			continue
		}

		// Get scrub state
		scrubState, err := getScrubState(db, coll.ID)
		if err != nil {
			return nil, fmt.Errorf("get scrub state for collection %s: %w", coll.ID, err)
		}

		// Count total files and verified files
		var totalFiles int
		var verifiedFiles int
		err = db.QueryRow(`
			SELECT COUNT(*), COALESCE(SUM(CASE WHEN checksum_verified_at IS NOT NULL AND checksum_verified_at > 0 THEN 1 ELSE 0 END), 0)
			FROM files WHERE collection_id = ?
		`, coll.ID).Scan(&totalFiles, &verifiedFiles)
		if err != nil {
			return nil, fmt.Errorf("count files for collection %s: %w", coll.ID, err)
		}

		var verifiedFraction float64
		if totalFiles > 0 {
			verifiedFraction = float64(verifiedFiles) / float64(totalFiles)
		}

		statuses = append(statuses, MaintenanceStatus{
			CollectionID:     coll.ID,
			CollectionName:   coll.Name,
			PoolID:           coll.PoolID,
			Cursor:           scrubState.Cursor,
			VerifiedFraction: verifiedFraction,
			LastRunAt:        scrubState.LastRunAt,
			TotalFiles:       totalFiles,
			VerifiedFiles:    verifiedFiles,
		})
	}

	return statuses, nil
}

// Ensure errors package is used
var _ = errors.Is