package catalog

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// ============================================================================
// WS34-A (2026-11-04): Tier Curator auto-move engine (FR-2.3)
// ============================================================================

// TierMigrationRow represents a migration execution record.
type TierMigrationRow struct {
	ID               string    `json:"id"`
	CollectionID     string    `json:"collection_id"`
	RecommendationID string    `json:"recommendation_id,omitempty"`
	FromPoolID       string    `json:"from_pool_id"`
	ToPoolID         string    `json:"to_pool_id"`
	Direction        string    `json:"direction"`
	Status           string    `json:"status"` // scheduled | executing | completed | failed | finalized | undone | skipped_pinned
	Manifest         string    `json:"manifest,omitempty"` // JSON array of {rel_path, sha256}
	Reason           string    `json:"reason,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	ExecutedAt       time.Time `json:"executed_at,omitempty"`
	RetainUntil      time.Time `json:"retain_until,omitempty"`
	FinalizedAt      time.Time `json:"finalized_at,omitempty"`
	UndoneAt         time.Time `json:"undone_at,omitempty"`
}

// MigrationStatus values.
const (
	MigrationStatusScheduled   = "scheduled"
	MigrationStatusExecuting   = "executing"
	MigrationStatusCompleted   = "completed"
	MigrationStatusFailed      = "failed"
	MigrationStatusFinalized   = "finalized"
	MigrationStatusUndone      = "undone"
	MigrationStatusSkippedPinned = "skipped_pinned"
)

// UndoWindowDays is the fixed undo window (7 days) for v1.
const UndoWindowDays = 7

// generateMigrationID returns a collision-proof migration ID.
// Format: mig-<unix-nano>-<8 hex chars from crypto/rand>
func generateMigrationID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("mig-%d-%08x", now, counter)
	}
	return fmt.Sprintf("mig-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// manifestEntry is one entry in the SHA-256 manifest.
type manifestEntry struct {
	RelPath string `json:"rel_path"`
	SHA256  string `json:"sha256"`
}

// buildManifest walks root and returns a JSON manifest of all regular files
// (excluding symlinks) with their relative paths and SHA-256 hashes.
func buildManifest(root string) (string, error) {
	var entries []manifestEntry
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return filepath.SkipDir
		}
		if d.IsDir() {
			if d.Type()&os.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		relPath = filepath.ToSlash(relPath)
		if strings.HasPrefix(relPath, "..") {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return nil
		}
		entries = append(entries, manifestEntry{
			RelPath: relPath,
			SHA256:  hex.EncodeToString(h.Sum(nil)),
		})
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk tree: %w", err)
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("marshal manifest: %w", err)
	}
	return string(b), nil
}

// verifyManifest checks that every file in the manifest exists at root with
// the same SHA-256 hash. Returns error on first mismatch or missing file.
func verifyManifest(root, manifestJSON string) error {
	var entries []manifestEntry
	if err := json.Unmarshal([]byte(manifestJSON), &entries); err != nil {
		return fmt.Errorf("unmarshal manifest: %w", err)
	}
	for _, e := range entries {
		path := filepath.Join(root, e.RelPath)
		path = filepath.Clean(path)
		if !strings.HasPrefix(path, root) {
			return fmt.Errorf("manifest path escapes root: %s", e.RelPath)
		}
		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("missing file: %s", e.RelPath)
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return fmt.Errorf("hash file %s: %w", e.RelPath, err)
		}
		f.Close()
		if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
			return fmt.Errorf("checksum mismatch: %s", e.RelPath)
		}
	}
	return nil
}

// copyTree copies the entire directory tree from src to dst.
// Returns error if src does not exist or copy fails.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a directory")
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("mkdir dest: %w", err)
	}
	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return filepath.SkipDir
		}
		relPath, err := filepath.Rel(src, path)
		if err != nil {
			return nil
		}
		if relPath == "." {
			return nil
		}
		dstPath := filepath.Join(dst, relPath)
		if d.IsDir() {
			if d.Type()&os.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			return os.MkdirAll(dstPath, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dstPath, data, info.Mode().Perm())
	})
}

// deleteTree removes the entire directory tree at root.
func deleteTree(root string) error {
	return os.RemoveAll(root)
}

// CopyFunc copies a source tree to a destination path. Used for injection in tests.
type CopyFunc func(src, dst string) error

// DeleteFunc removes a directory tree. Used for injection in tests.
type DeleteFunc func(root string) error

// DefaultCopyFunc is the production copy: copies tree under source pool mountpoint
// + collection path to target pool mountpoint + collection path.
func DefaultCopyFunc(src, dst string) error {
	return copyTree(src, dst)
}

// DefaultDeleteFunc is the production delete: removes the retained source tree.
func DefaultDeleteFunc(root string) error {
	return deleteTree(root)
}

// ResolveTargetPool finds a pool of the given tier with the most free space.
// Returns error if no pool of that tier exists.
func ResolveTargetPool(db *sql.DB, tier string) (*PoolRow, error) {
	pools, err := ListPools(db)
	if err != nil {
		return nil, fmt.Errorf("list pools: %w", err)
	}
	var best *PoolRow
	var bestFree int64
	for _, p := range pools {
		if p.Tier != tier {
			continue
		}
		free := p.SizeBytes - p.UsedBytes
		if best == nil || free > bestFree {
			best = p
			bestFree = free
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no pool of tier %s available", tier)
	}
	return best, nil
}

// CreateMigrationForRecommendation creates a migration row from an approved
// recommendation. Resolves collection + from/to pools. Target pool is resolved
// explicitly by tier (opposite of source tier), picking the pool with most free
// space. Returns error if recommendation not found, not approved, no target
// pool, or a live migration already exists for the collection.
func CreateMigrationForRecommendation(db *sql.DB, recID string, now time.Time) (*TierMigrationRow, error) {
	nowUnix := now.Unix()

	// Fetch the recommendation
	var recIDStr, recCollectionID, recPoolID, recDirection, recStatus string
	var recSizeBytes int64
	err := db.QueryRow(`
		SELECT id, collection_id, pool_id, direction, size_bytes, status
		FROM tier_recommendations WHERE id = ?
	`, recID).Scan(&recIDStr, &recCollectionID, &recPoolID, &recDirection, &recSizeBytes, &recStatus)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("recommendation not found")
	}
	if err != nil {
		return nil, fmt.Errorf("query recommendation: %w", err)
	}
	if recStatus != "approved" {
		return nil, fmt.Errorf("recommendation not approved (status: %s)", recStatus)
	}

	// Fetch the collection
	coll, err := GetCollection(db, recCollectionID)
	if err != nil {
		return nil, fmt.Errorf("get collection: %w", err)
	}
	if coll == nil {
		return nil, fmt.Errorf("collection not found")
	}

	// Fetch the source pool
	fromPool, _, err := GetPool(db, recPoolID)
	if err != nil {
		return nil, fmt.Errorf("get source pool: %w", err)
	}
	if fromPool == nil {
		return nil, fmt.Errorf("source pool not found")
	}

	// Determine target tier (opposite of source tier)
	var targetTier string
	switch fromPool.Tier {
	case "warm":
		targetTier = "cold"
	case "cold":
		targetTier = "warm"
	default:
		return nil, fmt.Errorf("source pool tier %s is not migratable", fromPool.Tier)
	}

	// Resolve target pool: most free space among pools of target tier
	toPool, err := ResolveTargetPool(db, targetTier)
	if err != nil {
		return nil, fmt.Errorf("resolve target pool: %w", err)
	}

	// Check for existing live migration for this collection
	var existingID string
	err = db.QueryRow(`
		SELECT id FROM tier_migrations
		WHERE collection_id = ? AND status IN (?, ?, ?)
		LIMIT 1
	`, recCollectionID, MigrationStatusScheduled, MigrationStatusExecuting, MigrationStatusCompleted).Scan(&existingID)
	if err == nil {
		return nil, fmt.Errorf("live migration already exists for collection (id: %s)", existingID)
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("check existing migration: %w", err)
	}

	// Determine direction string
	direction := recDirection

	// Insert migration row
	id := generateMigrationID()
	_, err = db.Exec(`
		INSERT INTO tier_migrations (id, collection_id, recommendation_id, from_pool_id, to_pool_id, direction, status, manifest, reason, created_at, executed_at, retain_until, finalized_at, undone_at)
		VALUES (?, ?, ?, ?, ?, ?, 'scheduled', '', '', ?, 0, 0, 0, 0)
	`, id, recCollectionID, recID, recPoolID, toPool.ID, direction, nowUnix)
	if err != nil {
		return nil, fmt.Errorf("insert migration: %w", err)
	}

	return &TierMigrationRow{
		ID:               id,
		CollectionID:     recCollectionID,
		RecommendationID: recID,
		FromPoolID:       recPoolID,
		ToPoolID:         toPool.ID,
		Direction:        direction,
		Status:           MigrationStatusScheduled,
		CreatedAt:        now,
	}, nil
}

// ExecuteMigration performs the copy -> checksum -> pointer -> retain sequence.
// copyFunc is injectable for tests; production uses defaultCopyFunc.
// On any failure: status = failed + reason, source untouched, pointer unswitched.
func ExecuteMigration(db *sql.DB, migID string, now time.Time, copyFunc CopyFunc) error {
	nowUnix := now.Unix()

	// Pre-fetch all data needed BEFORE opening transaction (avoids deadlock with SetMaxOpenConns(1))
	// Fetch migration row
	var migIDStr, migCollectionID, migRecID, migFromPool, migToPool, migDirection, migStatus, migManifest, migReason string
	var migCreatedAt, migExecutedAt, migRetainUntil, migFinalizedAt, migUndoneAt int64
	err := db.QueryRow(`
		SELECT id, collection_id, recommendation_id, from_pool_id, to_pool_id, direction, status, manifest, reason, created_at, executed_at, retain_until, finalized_at, undone_at
		FROM tier_migrations WHERE id = ?
	`, migID).Scan(&migIDStr, &migCollectionID, &migRecID, &migFromPool, &migToPool, &migDirection, &migStatus, &migManifest, &migReason, &migCreatedAt, &migExecutedAt, &migRetainUntil, &migFinalizedAt, &migUndoneAt)
	if err == sql.ErrNoRows {
		return fmt.Errorf("migration not found")
	}
	if err != nil {
		return fmt.Errorf("query migration: %w", err)
	}
	if migStatus != MigrationStatusScheduled {
		return fmt.Errorf("migration not scheduled (status: %s)", migStatus)
	}

	// Fetch collection
	coll, err := GetCollection(db, migCollectionID)
	if err != nil {
		return fmt.Errorf("get collection: %w", err)
	}
	if coll == nil {
		return fmt.Errorf("collection not found")
	}

	// Check if collection is pinned -> skip
	var pinExists int
	err = db.QueryRow(`SELECT COUNT(*) FROM tier_pins WHERE collection_id = ?`, migCollectionID).Scan(&pinExists)
	if err != nil {
		return fmt.Errorf("check pin: %w", err)
	}
	if pinExists > 0 {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}
		_, err = tx.Exec(`
			UPDATE tier_migrations
			SET status = ?, reason = ?
			WHERE id = ?
		`, MigrationStatusSkippedPinned, "collection is pinned", migID)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("update migration skipped_pinned: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit tx: %w", err)
		}
		WriteAuditLog(db, "system", "tier.migration.skipped_pinned", "migration", migID, "ok", "collection is pinned")
		return nil
	}

	// Fetch source and target pools
	fromPool, _, err := GetPool(db, migFromPool)
	if err != nil {
		return fmt.Errorf("get source pool: %w", err)
	}
	if fromPool == nil {
		return fmt.Errorf("source pool not found")
	}
	toPool, _, err := GetPool(db, migToPool)
	if err != nil {
		return fmt.Errorf("get target pool: %w", err)
	}
	if toPool == nil {
		return fmt.Errorf("target pool not found")
	}

	// Resolve filesystem roots
	srcRoot, err := ResolveCollectionRoot(fromPool, coll)
	if err != nil {
		return fmt.Errorf("resolve source root: %w", err)
	}
	dstRoot, err := ResolveCollectionRoot(toPool, coll)
	if err != nil {
		return fmt.Errorf("resolve target root: %w", err)
	}

	// Now open transaction for state transitions
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Mark executing
	_, err = tx.Exec(`
		UPDATE tier_migrations SET status = ? WHERE id = ?
	`, MigrationStatusExecuting, migID)
	if err != nil {
		return fmt.Errorf("mark executing: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit executing: %w", err)
	}

	// Step 1: Copy the whole collection tree source -> target
	if err := copyFunc(srcRoot, dstRoot); err != nil {
		stepErr := err
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for failure: %w", err)
		}
		_, uerr := tx.Exec(`UPDATE tier_migrations SET status = ?, reason = ? WHERE id = ?`,
			MigrationStatusFailed, "copy failed: "+stepErr.Error(), migID)
		if uerr != nil {
			tx.Rollback()
			return fmt.Errorf("copy failed and update failed: %w; %w", stepErr, uerr)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit failure: %w", err)
		}
		WriteAuditLog(db, "system", "tier.migration.failed", "migration", migID, "error", "copy failed: "+stepErr.Error())
		return fmt.Errorf("copy failed: %w", stepErr)
	}

	// Step 2: Build manifests and verify checksums
	srcManifest, err := buildManifest(srcRoot)
	if err != nil {
		stepErr := err
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for failure: %w", err)
		}
		_, uerr := tx.Exec(`UPDATE tier_migrations SET status = ?, reason = ? WHERE id = ?`,
			MigrationStatusFailed, "source manifest failed: "+stepErr.Error(), migID)
		if uerr != nil {
			tx.Rollback()
			return fmt.Errorf("source manifest failed and update failed: %w; %w", stepErr, uerr)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit failure: %w", err)
		}
		WriteAuditLog(db, "system", "tier.migration.failed", "migration", migID, "error", "source manifest failed: "+stepErr.Error())
		return fmt.Errorf("source manifest failed: %w", stepErr)
	}
	if _, err := buildManifest(dstRoot); err != nil {
		stepErr := err
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for failure: %w", err)
		}
		_, uerr := tx.Exec(`UPDATE tier_migrations SET status = ?, reason = ? WHERE id = ?`,
			MigrationStatusFailed, "target manifest failed: "+stepErr.Error(), migID)
		if uerr != nil {
			tx.Rollback()
			return fmt.Errorf("target manifest failed and update failed: %w; %w", stepErr, uerr)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit failure: %w", err)
		}
		WriteAuditLog(db, "system", "tier.migration.failed", "migration", migID, "error", "target manifest failed: "+stepErr.Error())
		return fmt.Errorf("target manifest failed: %w", stepErr)
	}
	if err := verifyManifest(dstRoot, srcManifest); err != nil {
		// Mismatch -> ABORT, source untouched, pointer unswitched
		stepErr := err
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for failure: %w", err)
		}
		_, uerr := tx.Exec(`UPDATE tier_migrations SET status = ?, reason = ? WHERE id = ?`,
			MigrationStatusFailed, "checksum verification failed: "+stepErr.Error(), migID)
		if uerr != nil {
			tx.Rollback()
			return fmt.Errorf("checksum failed and update failed: %w; %w", stepErr, uerr)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit failure: %w", err)
		}
		WriteAuditLog(db, "system", "tier.migration.failed", "migration", migID, "error", "checksum verification failed: "+stepErr.Error())
		return fmt.Errorf("checksum verification failed: %w", stepErr)
	}

	// Step 3: Switch the catalog pointer (collection's pool_id)
	tx, err = db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx for pointer switch: %w", err)
	}
	_, err = tx.Exec(`UPDATE collections SET pool_id = ?, updated_at = ? WHERE id = ?`,
		migToPool, nowUnix, migCollectionID)
	if err != nil {
		tx.Rollback()
		tx2, _ := db.Begin()
		_, uerr := tx2.Exec(`UPDATE tier_migrations SET status = ?, reason = ? WHERE id = ?`,
			MigrationStatusFailed, "pointer switch failed: "+err.Error(), migID)
		if uerr != nil {
			tx2.Rollback()
			return fmt.Errorf("pointer switch failed and update failed: %w; %w", err, uerr)
		}
		if err := tx2.Commit(); err != nil {
			return fmt.Errorf("commit failure: %w", err)
		}
		WriteAuditLog(db, "system", "tier.migration.failed", "migration", migID, "error", "pointer switch failed: "+err.Error())
		return fmt.Errorf("pointer switch failed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pointer switch: %w", err)
	}

	// Step 4: RETAIN source tree, marked retained-until = executed_at + 7 days
	retainUntil := nowUnix + int64(UndoWindowDays*24*3600)
	tx, err = db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx for completion: %w", err)
	}
	_, err = tx.Exec(`
		UPDATE tier_migrations
		SET status = ?, executed_at = ?, retain_until = ?, manifest = ?
		WHERE id = ?
	`, MigrationStatusCompleted, nowUnix, retainUntil, srcManifest, migID)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("mark completed: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit completion: %w", err)
	}

	WriteAuditLog(db, "system", "tier.migration.completed", "migration", migID, "ok",
		fmt.Sprintf("collection=%s from_pool=%s to_pool=%s retain_until=%d", migCollectionID, migFromPool, migToPool, retainUntil))
	return nil
}

// UndoMigration switches the pointer back and deletes the new copy.
// Only from completed within retain_until. deleteFunc is injectable.
func UndoMigration(db *sql.DB, migID string, now time.Time, deleteFunc DeleteFunc) error {
	nowUnix := now.Unix()

	// Pre-fetch all data needed BEFORE opening transaction (avoids deadlock with SetMaxOpenConns(1))
	// Fetch migration row
	var migIDStr, migCollectionID, migFromPool, migToPool, migStatus string
	var migExecutedAt, migRetainUntil, migUndoneAt int64
	err := db.QueryRow(`
		SELECT id, collection_id, from_pool_id, to_pool_id, status, executed_at, retain_until, undone_at
		FROM tier_migrations WHERE id = ?
	`, migID).Scan(&migIDStr, &migCollectionID, &migFromPool, &migToPool, &migStatus, &migExecutedAt, &migRetainUntil, &migUndoneAt)
	if err == sql.ErrNoRows {
		return fmt.Errorf("migration not found")
	}
	if err != nil {
		return fmt.Errorf("query migration: %w", err)
	}
	if migStatus != MigrationStatusCompleted {
		return fmt.Errorf("migration not completed (status: %s)", migStatus)
	}
	if nowUnix > migRetainUntil {
		return fmt.Errorf("undo window expired (retain_until=%d, now=%d)", migRetainUntil, nowUnix)
	}

	// Fetch collection and target pool to resolve the new copy path
	coll, err := GetCollection(db, migCollectionID)
	if err != nil {
		return fmt.Errorf("get collection: %w", err)
	}
	if coll == nil {
		return fmt.Errorf("collection not found")
	}
	// Collection's pool_id is now the target pool (switched at execution)
	toPool, _, err := GetPool(db, coll.PoolID)
	if err != nil {
		return fmt.Errorf("get current pool: %w", err)
	}
	if toPool == nil {
		return fmt.Errorf("current pool not found")
	}
	fromPool, _, err := GetPool(db, migFromPool)
	if err != nil {
		return fmt.Errorf("get source pool: %w", err)
	}
	if fromPool == nil {
		return fmt.Errorf("source pool not found")
	}

	// Resolve the target root (new copy to delete)
	dstRoot, err := ResolveCollectionRoot(toPool, coll)
	if err != nil {
		return fmt.Errorf("resolve target root: %w", err)
	}

	// Step 1: Switch pointer back to source pool (in transaction)
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	_, err = tx.Exec(`UPDATE collections SET pool_id = ?, updated_at = ? WHERE id = ?`,
		migFromPool, nowUnix, migCollectionID)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("switch pointer back: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pointer switch: %w", err)
	}

	// Step 2: Delete the new copy (on the target pool, filesystem operation, no tx)
	if err := deleteFunc(dstRoot); err != nil {
		return fmt.Errorf("delete new copy: %w", err)
	}

	// Step 3: Mark undone (in transaction)
	tx, err = db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx for undone: %w", err)
	}
	_, err = tx.Exec(`
		UPDATE tier_migrations
		SET status = ?, undone_at = ?
		WHERE id = ?
	`, MigrationStatusUndone, nowUnix, migID)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("mark undone: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit undone: %w", err)
	}

	WriteAuditLog(db, "system", "tier.migration.undone", "migration", migID, "ok",
		fmt.Sprintf("collection=%s from_pool=%s to_pool=%s", migCollectionID, migFromPool, migToPool))
	return nil
}

// FinalizeExpiredMigrations deletes retained source trees for completed
// migrations whose retain_until has passed. deleteFunc is injectable.
func FinalizeExpiredMigrations(db *sql.DB, now time.Time, deleteFunc DeleteFunc) (int, error) {
	nowUnix := now.Unix()

	rows, err := db.Query(`
		SELECT id, collection_id, from_pool_id, to_pool_id
		FROM tier_migrations
		WHERE status = ? AND retain_until > 0 AND retain_until <= ?
	`, MigrationStatusCompleted, nowUnix)
	if err != nil {
		return 0, fmt.Errorf("query expired migrations: %w", err)
	}
	defer rows.Close()

	type expiredMig struct {
		id          string
		collectionID string
		fromPoolID   string
		toPoolID     string
	}
	var expired []expiredMig
	for rows.Next() {
		var e expiredMig
		if err := rows.Scan(&e.id, &e.collectionID, &e.fromPoolID, &e.toPoolID); err != nil {
			return 0, fmt.Errorf("scan expired migration: %w", err)
		}
		expired = append(expired, e)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate expired migrations: %w", err)
	}

	count := 0
	for _, e := range expired {
		// Fetch collection and source pool (retained source is on from_pool)
		coll, err := GetCollection(db, e.collectionID)
		if err != nil || coll == nil {
			// Collection may have been deleted; just mark finalized
			_, _ = db.Exec(`UPDATE tier_migrations SET status = ?, finalized_at = ? WHERE id = ?`,
				MigrationStatusFinalized, nowUnix, e.id)
			WriteAuditLog(db, "system", "tier.migration.finalized", "migration", e.id, "ok", "collection gone, marked finalized")
			count++
			continue
		}
		fromPool, _, err := GetPool(db, e.fromPoolID)
		if err != nil || fromPool == nil {
			_, _ = db.Exec(`UPDATE tier_migrations SET status = ?, finalized_at = ? WHERE id = ?`,
				MigrationStatusFinalized, nowUnix, e.id)
			WriteAuditLog(db, "system", "tier.migration.finalized", "migration", e.id, "ok", "source pool gone, marked finalized")
			count++
			continue
		}

		// Resolve the retained source root (on from_pool, collection path)
		srcRoot, err := ResolveCollectionRoot(fromPool, coll)
		if err != nil {
			_, _ = db.Exec(`UPDATE tier_migrations SET status = ?, finalized_at = ? WHERE id = ?`,
				MigrationStatusFinalized, nowUnix, e.id)
			WriteAuditLog(db, "system", "tier.migration.finalized", "migration", e.id, "ok", "resolve root failed, marked finalized")
			count++
			continue
		}

		// Delete retained source tree
		if err := deleteFunc(srcRoot); err != nil {
			// Log but still mark finalized? No — return error for this one.
			WriteAuditLog(db, "system", "tier.migration.finalized", "migration", e.id, "error", "delete retained source failed: "+err.Error())
			continue
		}

		_, err = db.Exec(`UPDATE tier_migrations SET status = ?, finalized_at = ? WHERE id = ?`,
			MigrationStatusFinalized, nowUnix, e.id)
		if err != nil {
			return count, fmt.Errorf("mark finalized: %w", err)
		}
		WriteAuditLog(db, "system", "tier.migration.finalized", "migration", e.id, "ok",
			fmt.Sprintf("collection=%s from_pool=%s", e.collectionID, e.fromPoolID))
		count++
	}
	return count, nil
}

// ProcessSilentConsents finds PENDING recommendations older than their pool's
// auto_move_wait_days in auto_if_no_response pools, marks them approved-with-silent-consent,
// and creates migrations (status scheduled). Returns count created.
// The caller layer writes the audit row curator.recommendation.silent_consent.
func ProcessSilentConsents(db *sql.DB, now time.Time) (int, error) {
	nowUnix := now.Unix()

	// Find pending recommendations whose pool is auto_if_no_response and
	// older than auto_move_wait_days.
	rows, err := db.Query(`
		SELECT r.id, r.collection_id, r.pool_id, r.direction, r.created_at, p.auto_move_wait_days
		FROM tier_recommendations r
		JOIN tier_policies p ON r.pool_id = p.pool_id
		WHERE r.status = 'pending' AND p.mode = 'auto_if_no_response'
	`)
	if err != nil {
		return 0, fmt.Errorf("query pending recommendations: %w", err)
	}
	defer rows.Close()

	type consentRec struct {
		id            string
		collectionID  string
		poolID        string
		direction     string
		createdAt     int64
		autoMoveWait  int
	}
	var consents []consentRec
	for rows.Next() {
		var c consentRec
		if err := rows.Scan(&c.id, &c.collectionID, &c.poolID, &c.direction, &c.createdAt, &c.autoMoveWait); err != nil {
			return 0, fmt.Errorf("scan consent rec: %w", err)
		}
		consents = append(consents, c)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate consent recs: %w", err)
	}

	created := 0
	for _, c := range consents {
		waitSeconds := int64(c.autoMoveWait) * 24 * 3600
		if nowUnix-c.createdAt < waitSeconds {
			continue // not yet past wait period
		}

		// Mark recommendation as approved (silent consent)
		if _, err := db.Exec(`UPDATE tier_recommendations SET status = 'approved', decided_at = ? WHERE id = ?`, nowUnix, c.id); err != nil {
			WriteAuditLog(db, "system", "tier.migration.silent_consent", "recommendation", c.id, "error", "approve recommendation failed: "+err.Error())
			continue
		}

		// Create migration (status scheduled)
		mig, err := CreateMigrationForRecommendation(db, c.id, now)
		if err != nil {
			// Log and continue; don't fail the whole batch
			WriteAuditLog(db, "system", "tier.migration.silent_consent", "recommendation", c.id, "error", "create migration failed: "+err.Error())
			continue
		}
		_ = mig
		created++
		WriteAuditLog(db, "system", "tier.migration.silent_consent", "recommendation", c.id, "ok",
			fmt.Sprintf("migration=%s created", mig.ID))
	}
	return created, nil
}

// ListMigrations returns migrations filtered by status and/or collection_id.
func ListMigrations(db *sql.DB, status, collectionID string) ([]*TierMigrationRow, int, error) {
	whereClauses := []string{}
	args := []any{}

	if status != "" {
		whereClauses = append(whereClauses, "status = ?")
		args = append(args, status)
	}
	if collectionID != "" {
		whereClauses = append(whereClauses, "collection_id = ?")
		args = append(args, collectionID)
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	countQuery := "SELECT COUNT(*) FROM tier_migrations " + whereSQL
	var total int
	err := db.QueryRow(countQuery, args...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count migrations: %w", err)
	}

	query := fmt.Sprintf(`SELECT id, collection_id, recommendation_id, from_pool_id, to_pool_id, direction, status, manifest, reason, created_at, executed_at, retain_until, finalized_at, undone_at FROM tier_migrations %s ORDER BY created_at DESC, rowid DESC`, whereSQL)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query migrations: %w", err)
	}
	defer rows.Close()

	var migrations []*TierMigrationRow
	for rows.Next() {
		var m TierMigrationRow
		var createdAt, executedAt, retainUntil, finalizedAt, undoneAt int64
		var recID sql.NullString
		var manifest sql.NullString
		var reason sql.NullString
		if err := rows.Scan(&m.ID, &m.CollectionID, &recID, &m.FromPoolID, &m.ToPoolID, &m.Direction, &m.Status, &manifest, &reason, &createdAt, &executedAt, &retainUntil, &finalizedAt, &undoneAt); err != nil {
			return nil, 0, fmt.Errorf("scan migration: %w", err)
		}
		if recID.Valid {
			m.RecommendationID = recID.String
		}
		if manifest.Valid {
			m.Manifest = manifest.String
		}
		if reason.Valid {
			m.Reason = reason.String
		}
		m.CreatedAt = time.Unix(createdAt, 0)
		if executedAt > 0 {
			m.ExecutedAt = time.Unix(executedAt, 0)
		}
		if retainUntil > 0 {
			m.RetainUntil = time.Unix(retainUntil, 0)
		}
		if finalizedAt > 0 {
			m.FinalizedAt = time.Unix(finalizedAt, 0)
		}
		if undoneAt > 0 {
			m.UndoneAt = time.Unix(undoneAt, 0)
		}
		migrations = append(migrations, &m)
	}

	if migrations == nil {
		migrations = []*TierMigrationRow{}
	}

	return migrations, total, nil
}

// GetMigration retrieves a migration by ID.
// Returns nil if not found.
func GetMigration(db *sql.DB, id string) (*TierMigrationRow, error) {
	row := db.QueryRow(`
		SELECT id, collection_id, recommendation_id, from_pool_id, to_pool_id, direction, status, manifest, reason, created_at, executed_at, retain_until, finalized_at, undone_at
		FROM tier_migrations WHERE id = ?
	`, id)
	var m TierMigrationRow
	var createdAt, executedAt, retainUntil, finalizedAt, undoneAt int64
	var recID sql.NullString
	var manifest sql.NullString
	var reason sql.NullString
	if err := row.Scan(&m.ID, &m.CollectionID, &recID, &m.FromPoolID, &m.ToPoolID, &m.Direction, &m.Status, &manifest, &reason, &createdAt, &executedAt, &retainUntil, &finalizedAt, &undoneAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan migration: %w", err)
	}
	if recID.Valid {
		m.RecommendationID = recID.String
	}
	if manifest.Valid {
		m.Manifest = manifest.String
	}
	if reason.Valid {
		m.Reason = reason.String
	}
	m.CreatedAt = time.Unix(createdAt, 0)
	if executedAt > 0 {
		m.ExecutedAt = time.Unix(executedAt, 0)
	}
	if retainUntil > 0 {
		m.RetainUntil = time.Unix(retainUntil, 0)
	}
	if finalizedAt > 0 {
		m.FinalizedAt = time.Unix(finalizedAt, 0)
	}
	if undoneAt > 0 {
		m.UndoneAt = time.Unix(undoneAt, 0)
	}
	return &m, nil
}
