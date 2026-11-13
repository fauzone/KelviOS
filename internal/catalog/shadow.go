package catalog

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// ============================================================================
// WS35 (2026-11-05): Shadow read replicas in Warm (FR-2.5)
// ============================================================================

// ShadowRow is the catalog representation of a file shadow (read-only replica in Warm).
type ShadowRow struct {
	ID            string    `json:"id"`
	FileID        string    `json:"file_id"`
	CollectionID  string    `json:"collection_id"`
	RelPath       string    `json:"rel_path"`
	ShadowPoolID  string    `json:"shadow_pool_id"`
	ShadowRelPath string    `json:"shadow_rel_path"`
	SHA256        string    `json:"sha256"`
	SizeBytes     int64     `json:"size_bytes"`
	CreatedAt     time.Time `json:"created_at"`
	LastServedAt  time.Time `json:"last_served_at"`
	// WS36 (FR-2.6): dirty write-back fields
	Dirty     int       `json:"dirty"`      // 1 = shadow has uncommitted writes, 0 = clean
	Version   int       `json:"version"`    // monotonically increasing version number
	DirtyAt   time.Time `json:"dirty_at"`   // when dirty was set (for age-based flush)
}

// ShadowCopyFunc copies a single file from src to dst. Injectable for tests.
type ShadowCopyFunc func(src, dst string) error

// DefaultShadowCopyFunc is the production copy: copies src file to dst.
func DefaultShadowCopyFunc(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("mkdir dest dir: %w", err)
	}
	return os.WriteFile(dst, data, 0o644)
}

// generateShadowID returns a collision-proof shadow ID.
// Format: shdw-<unix-nano>-<8 hex chars from crypto/rand>
func generateShadowID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("shdw-%d-%08x", now, counter)
	}
	return fmt.Sprintf("shdw-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// hashFile computes SHA-256 of a file and returns (hex string, size, error).
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", 0, fmt.Errorf("stat file: %w", err)
	}

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", 0, fmt.Errorf("hash file: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), info.Size(), nil
}

// CreateShadow creates a shadow row and copies the canonical file to the shadow location.
// copyFunc is injectable for tests. On any failure: logs + audits error, returns error.
// The shadow file is written to shadowPath (full filesystem path).
// The shadow_rel_path stored in DB is the relative path within the shadow root (e.g., "<collection-id>/<rel_path>").
// TODO (WS35 FR-2.5): a shadow of an encrypted pool may only live in an equally encrypted tier (no encryption metadata exists yet; do not invent a check).
func CreateShadow(db *sql.DB, collectionID, relPath, shadowPoolID, shadowRelPath, canonicalPath, shadowPath string, copyFunc ShadowCopyFunc, actor string) (*ShadowRow, error) {
	// Compute SHA-256 of canonical
	canonicalSHA, size, err := hashFile(canonicalPath)
	if err != nil {
		WriteAuditLog(db, actor, "shadow.create", "file_shadow", collectionID, "error", "hash canonical: "+err.Error())
		return nil, fmt.Errorf("hash canonical: %w", err)
	}

	// Copy file to shadow location
	if err := copyFunc(canonicalPath, shadowPath); err != nil {
		WriteAuditLog(db, actor, "shadow.create", "file_shadow", collectionID, "error", "copy to shadow: "+err.Error())
		return nil, fmt.Errorf("copy to shadow: %w", err)
	}

	// Compute SHA-256 of shadow copy
	shadowSHA, _, err := hashFile(shadowPath)
	if err != nil {
		// Clean up failed copy
		_ = os.Remove(shadowPath)
		WriteAuditLog(db, actor, "shadow.create", "file_shadow", collectionID, "error", "hash shadow: "+err.Error())
		return nil, fmt.Errorf("hash shadow: %w", err)
	}

	// Verify checksums match
	if canonicalSHA != shadowSHA {
		_ = os.Remove(shadowPath)
		WriteAuditLog(db, actor, "shadow.create", "file_shadow", collectionID, "error", "checksum mismatch between canonical and shadow")
		return nil, fmt.Errorf("checksum mismatch")
	}

	// Insert shadow row (WS36: version=1, dirty=0, dirty_at=0)
	now := time.Now().Unix()
	id := generateShadowID()
	_, err = db.Exec(`
		INSERT INTO file_shadows (id, file_id, collection_id, rel_path, shadow_pool_id, shadow_rel_path, sha256, size_bytes, created_at, last_served_at, dirty, version, dirty_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 1, 0)
	`, id, "", collectionID, relPath, shadowPoolID, shadowRelPath, canonicalSHA, size, now)
	if err != nil {
		// Clean up copy on DB failure
		_ = os.Remove(shadowPath)
		WriteAuditLog(db, actor, "shadow.create", "file_shadow", collectionID, "error", "insert shadow row: "+err.Error())
		return nil, fmt.Errorf("insert shadow row: %w", err)
	}

	// Get the file_id for the canonical file (best effort)
	var fileID string
	_ = db.QueryRow(`SELECT id FROM files WHERE collection_id = ? AND rel_path = ?`, collectionID, relPath).Scan(&fileID)

	row := &ShadowRow{
		ID:            id,
		FileID:        fileID,
		CollectionID:  collectionID,
		RelPath:       relPath,
		ShadowPoolID:  shadowPoolID,
		ShadowRelPath: shadowRelPath,
		SHA256:        canonicalSHA,
		SizeBytes:     size,
		CreatedAt:     time.Unix(now, 0),
		LastServedAt:  time.Time{},
		Dirty:         0,
		Version:       1,
		DirtyAt:       time.Time{},
	}

	WriteAuditLog(db, actor, "shadow.created", "file_shadow", id, "ok", fmt.Sprintf("collection=%s rel_path=%s pool=%s size=%d", collectionID, relPath, shadowPoolID, size))

	// Best-effort eviction after successful shadow creation (WS37 FR-2.8).
	// Never fail the served download on eviction errors.
	go func() {
		_, _ = EvictShadows(db, shadowPoolID, DefaultShadowDeleteFunc, actor)
	}()

	return row, nil
}

// GetShadow retrieves a shadow row by collection ID and relative path.
// Returns nil if not found.
func GetShadow(db *sql.DB, collectionID, relPath string) (*ShadowRow, error) {
	row := db.QueryRow(`
		SELECT id, file_id, collection_id, rel_path, shadow_pool_id, shadow_rel_path, sha256, size_bytes, created_at, last_served_at, dirty, version, dirty_at
		FROM file_shadows WHERE collection_id = ? AND rel_path = ?
	`, collectionID, relPath)
	var s ShadowRow
	var createdAt, lastServedAt, dirtyAt int64
	var fileID sql.NullString
	var dirty, version int
	if err := row.Scan(&s.ID, &fileID, &s.CollectionID, &s.RelPath, &s.ShadowPoolID, &s.ShadowRelPath, &s.SHA256, &s.SizeBytes, &createdAt, &lastServedAt, &dirty, &version, &dirtyAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan shadow: %w", err)
	}
	if fileID.Valid {
		s.FileID = fileID.String
	}
	s.CreatedAt = time.Unix(createdAt, 0)
	if lastServedAt > 0 {
		s.LastServedAt = time.Unix(lastServedAt, 0)
	}
	s.Dirty = dirty
	s.Version = version
	if dirtyAt > 0 {
		s.DirtyAt = time.Unix(dirtyAt, 0)
	}
	return &s, nil
}

// InvalidateShadow deletes the shadow row and the shadow file (best-effort).
// Audits shadow.invalidated ok. Returns error only if DB delete fails.
func InvalidateShadow(db *sql.DB, collectionID, relPath, actor string) error {
	// Get shadow row first to know the file path
	shadow, err := GetShadow(db, collectionID, relPath)
	if err != nil {
		return fmt.Errorf("get shadow: %w", err)
	}
	if shadow == nil {
		return nil // nothing to invalidate
	}

	// Get shadow pool to resolve mountpoint
	pool, _, err := GetPool(db, shadow.ShadowPoolID)
	if err != nil {
		WriteAuditLog(db, actor, "shadow.invalidated", "file_shadow", shadow.ID, "error", "get shadow pool: "+err.Error())
		// Continue to delete row anyway
	} else if pool != nil {
		// Resolve shadow file path and delete (best-effort)
		mountpoint := pool.Mountpoint
		if mountpoint == "" {
			mountpoint = "/mnt/" + pool.Name
		}
		shadowPath := filepath.Join(mountpoint, ".kelvios-shadow", shadow.ShadowRelPath)
		shadowPath = filepath.Clean(shadowPath)
		if strings.HasPrefix(shadowPath, mountpoint) {
			_ = os.Remove(shadowPath) // best-effort
		}
	}

	// Delete shadow row
	_, err = db.Exec(`DELETE FROM file_shadows WHERE collection_id = ? AND rel_path = ?`, collectionID, relPath)
	if err != nil {
		WriteAuditLog(db, actor, "shadow.invalidated", "file_shadow", shadow.ID, "error", "delete shadow row: "+err.Error())
		return fmt.Errorf("delete shadow row: %w", err)
	}

	WriteAuditLog(db, actor, "shadow.invalidated", "file_shadow", shadow.ID, "ok", fmt.Sprintf("collection=%s rel_path=%s", collectionID, relPath))
	return nil
}

// CountRecentAccesses counts access events for a file within the last windowDays.
// Returns the count (0 if none).
func CountRecentAccesses(db *sql.DB, collectionID, relPath string, windowDays int) (int, error) {
	if windowDays <= 0 {
		windowDays = 14
	}
	cutoff := time.Now().AddDate(0, 0, -windowDays).Unix()

	var count int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM access_events
		WHERE collection_id = ? AND rel_path = ? AND accessed_at >= ?
	`, collectionID, relPath, cutoff).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count recent accesses: %w", err)
	}
	return count, nil
}

// PickWarmShadowPool returns the Warm pool with the most free space (size_bytes - used_bytes).
// Returns error if no Warm pool exists.
func PickWarmShadowPool(db *sql.DB) (*PoolRow, error) {
	pools, err := ListPools(db)
	if err != nil {
		return nil, fmt.Errorf("list pools: %w", err)
	}
	var best *PoolRow
	var bestFree int64
	for _, p := range pools {
		if p.Tier != "warm" {
			continue
		}
		free := p.SizeBytes - p.UsedBytes
		if best == nil || free > bestFree {
			best = p
			bestFree = free
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no warm pool available")
	}
	return best, nil
}

// UpdateShadowLastServed updates the last_served_at timestamp for a shadow.
// Best-effort: logs error but never returns it.
func UpdateShadowLastServed(db *sql.DB, collectionID, relPath string) {
	now := time.Now().Unix()
	_, err := db.Exec(`UPDATE file_shadows SET last_served_at = ? WHERE collection_id = ? AND rel_path = ?`, now, collectionID, relPath)
	if err != nil {
		_ = err // explicitly ignored, best-effort
	}
}

// ============================================================================
// WS36 (2026-11-06): Dirty write-back shadows (FR-2.6)
// ============================================================================

// DirtyAgeLimitHours is the age limit (in hours) after which a dirty shadow
// triggers a combined wake to flush. Default 72h per §3.
const DirtyAgeLimitHours = 72

// ============================================================================
// WS37 (2026-11-07): Shadow eviction + promotion recommendations (FR-2.8)
// ============================================================================

// ShadowQuotaPercent is the maximum percentage of a Warm pool's size_bytes
// that can be occupied by shadows. Default 20% per §3.
const ShadowQuotaPercent = 20

// ShadowIdleDays is the number of days a shadow can go unserved before
// becoming eligible for idle eviction. Default 30 days per §3.
const ShadowIdleDays = 30

// ShadowPromotionAccessThreshold is the minimum number of access events
// in the promotion window for a shadow to become a promotion candidate.
// Default 10 per §3.
const ShadowPromotionAccessThreshold = 10

// ShadowPromotionWindowDays is the lookback window (in days) for counting
// access events for promotion eligibility. Default 30 days per §3.
const ShadowPromotionWindowDays = 30

// WriteDirtyShadow writes uploaded content to the shadow file (Warm path) and
// marks the shadow row as dirty. This is the deferred write-back path for
// Cold collections when the disk is asleep and a shadow exists.
// On any failure: logs + audits error, canonical untouched, returns error.
// The write uses temp + rename inside Warm for atomicity (no partial shadow).
func WriteDirtyShadow(db *sql.DB, collectionID, relPath, shadowPoolID, shadowRelPath string, data []byte, actor string) (*ShadowRow, error) {
	// Get shadow row first
	shadow, err := GetShadow(db, collectionID, relPath)
	if err != nil {
		return nil, fmt.Errorf("get shadow: %w", err)
	}
	if shadow == nil {
		return nil, fmt.Errorf("shadow not found")
	}

	// Get shadow pool to resolve mountpoint
	pool, _, err := GetPool(db, shadow.ShadowPoolID)
	if err != nil {
		WriteAuditLog(db, actor, "shadow.dirty", "file_shadow", shadow.ID, "error", "get shadow pool: "+err.Error())
		return nil, fmt.Errorf("get shadow pool: %w", err)
	}
	if pool == nil {
		WriteAuditLog(db, actor, "shadow.dirty", "file_shadow", shadow.ID, "error", "shadow pool not found")
		return nil, fmt.Errorf("shadow pool not found")
	}

	// Resolve shadow file path
	mountpoint := pool.Mountpoint
	if mountpoint == "" {
		mountpoint = "/mnt/" + pool.Name
	}
	shadowPath := filepath.Join(mountpoint, ".kelvios-shadow", shadow.ShadowRelPath)
	shadowPath = filepath.Clean(shadowPath)
	if !strings.HasPrefix(shadowPath, mountpoint) {
		WriteAuditLog(db, actor, "shadow.dirty", "file_shadow", shadow.ID, "error", "shadow path escape")
		return nil, fmt.Errorf("shadow path escape")
	}

	// Write to temp file first, then atomic rename (no partial shadow)
	tempPath := shadowPath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(tempPath), 0o755); err != nil {
		WriteAuditLog(db, actor, "shadow.dirty", "file_shadow", shadow.ID, "error", "mkdir temp: "+err.Error())
		return nil, fmt.Errorf("mkdir temp: %w", err)
	}
	if err := os.WriteFile(tempPath, data, 0o644); err != nil {
		WriteAuditLog(db, actor, "shadow.dirty", "file_shadow", shadow.ID, "error", "write temp: "+err.Error())
		_ = os.Remove(tempPath)
		return nil, fmt.Errorf("write temp: %w", err)
	}
	if err := os.Rename(tempPath, shadowPath); err != nil {
		WriteAuditLog(db, actor, "shadow.dirty", "file_shadow", shadow.ID, "error", "rename temp: "+err.Error())
		_ = os.Remove(tempPath)
		return nil, fmt.Errorf("rename temp: %w", err)
	}

	// Compute SHA-256 and size of new shadow content
	newSHA, newSize, err := hashFile(shadowPath)
	if err != nil {
		WriteAuditLog(db, actor, "shadow.dirty", "file_shadow", shadow.ID, "error", "hash shadow: "+err.Error())
		return nil, fmt.Errorf("hash shadow: %w", err)
	}

	// Update shadow row: dirty=1, version+1, dirty_at=now, sha256/size updated
	now := time.Now().Unix()
	newVersion := shadow.Version + 1
	_, err = db.Exec(`
		UPDATE file_shadows
		SET sha256 = ?, size_bytes = ?, dirty = 1, version = ?, dirty_at = ?
		WHERE collection_id = ? AND rel_path = ?
	`, newSHA, newSize, newVersion, now, collectionID, relPath)
	if err != nil {
		WriteAuditLog(db, actor, "shadow.dirty", "file_shadow", shadow.ID, "error", "update shadow row: "+err.Error())
		return nil, fmt.Errorf("update shadow row: %w", err)
	}

	WriteAuditLog(db, actor, "shadow.dirty", "file_shadow", shadow.ID, "ok", fmt.Sprintf("collection=%s rel_path=%s version=%d size=%d", collectionID, relPath, newVersion, newSize))

	return &ShadowRow{
		ID:            shadow.ID,
		FileID:        shadow.FileID,
		CollectionID:  shadow.CollectionID,
		RelPath:       shadow.RelPath,
		ShadowPoolID:  shadow.ShadowPoolID,
		ShadowRelPath: shadow.ShadowRelPath,
		SHA256:        newSHA,
		SizeBytes:     newSize,
		CreatedAt:     shadow.CreatedAt,
		LastServedAt:  shadow.LastServedAt,
		Dirty:         1,
		Version:       newVersion,
		DirtyAt:       time.Unix(now, 0),
	}, nil
}

// FlushDirtyShadows flushes all dirty shadows for a given pool to their canonical locations.
// Crash-safe per A2.2: copy shadow -> canonical via temp + atomic rename, verify SHA-256,
// then clear dirty + dirty_at. On ANY failure: abort that row, keep dirty set, audit error,
// canonical untouched. Flush NEVER wakes a disk by itself.
// Returns (flushedCount, errorCount, error).
func FlushDirtyShadows(db *sql.DB, poolID string, actor string) (int, int, error) {
	// Get all dirty shadows for this pool
	rows, err := db.Query(`
		SELECT fs.id, fs.file_id, fs.collection_id, fs.rel_path, fs.shadow_pool_id, fs.shadow_rel_path, fs.sha256, fs.size_bytes, fs.created_at, fs.last_served_at, fs.dirty, fs.version, fs.dirty_at
		FROM file_shadows fs JOIN collections c ON c.id = fs.collection_id WHERE fs.dirty = 1 AND (fs.shadow_pool_id = ? OR c.pool_id = ?)
	`, poolID, poolID)
	if err != nil {
		return 0, 0, fmt.Errorf("query dirty shadows: %w", err)
	}
	defer rows.Close()

	type dirtyShadow struct {
		ShadowRow
	}
	var dirtyShadows []dirtyShadow
	for rows.Next() {
		var s ShadowRow
		var createdAt, lastServedAt, dirtyAt int64
		var fileID sql.NullString
		var dirty, version int
		if err := rows.Scan(&s.ID, &fileID, &s.CollectionID, &s.RelPath, &s.ShadowPoolID, &s.ShadowRelPath, &s.SHA256, &s.SizeBytes, &createdAt, &lastServedAt, &dirty, &version, &dirtyAt); err != nil {
			return 0, 0, fmt.Errorf("scan dirty shadow: %w", err)
		}
		if fileID.Valid {
			s.FileID = fileID.String
		}
		s.CreatedAt = time.Unix(createdAt, 0)
		if lastServedAt > 0 {
			s.LastServedAt = time.Unix(lastServedAt, 0)
		}
		s.Dirty = dirty
		s.Version = version
		if dirtyAt > 0 {
			s.DirtyAt = time.Unix(dirtyAt, 0)
		}
		dirtyShadows = append(dirtyShadows, dirtyShadow{s})
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("iterate dirty shadows: %w", err)
	}

	flushed := 0
	failed := 0
	for _, ds := range dirtyShadows {
		s := ds.ShadowRow
		// Get canonical collection to resolve canonical path
		coll, err := GetCollection(db, s.CollectionID)
		if err != nil || coll == nil {
			WriteAuditLog(db, actor, "shadow.flushed", "file_shadow", s.ID, "error", "get collection: "+err.Error())
			failed++
			continue
		}
		// Get canonical pool (the collection's pool)
		canonicalPool, _, err := GetPool(db, coll.PoolID)
		if err != nil || canonicalPool == nil {
			WriteAuditLog(db, actor, "shadow.flushed", "file_shadow", s.ID, "error", "get canonical pool: "+err.Error())
			failed++
			continue
		}

		// Resolve shadow file path
		shadowPool, _, err := GetPool(db, s.ShadowPoolID)
		if err != nil || shadowPool == nil {
			WriteAuditLog(db, actor, "shadow.flushed", "file_shadow", s.ID, "error", "get shadow pool: "+err.Error())
			failed++
			continue
		}
		shadowMountpoint := shadowPool.Mountpoint
		if shadowMountpoint == "" {
			shadowMountpoint = "/mnt/" + shadowPool.Name
		}
		shadowPath := filepath.Join(shadowMountpoint, ".kelvios-shadow", s.ShadowRelPath)
		shadowPath = filepath.Clean(shadowPath)
		if !strings.HasPrefix(shadowPath, shadowMountpoint) {
			WriteAuditLog(db, actor, "shadow.flushed", "file_shadow", s.ID, "error", "shadow path escape")
			failed++
			continue
		}

		// Resolve canonical file path
		canonicalMountpoint := canonicalPool.Mountpoint
		if canonicalMountpoint == "" {
			canonicalMountpoint = "/mnt/" + canonicalPool.Name
		}
		canonicalPath := filepath.Join(canonicalMountpoint, coll.Path, s.RelPath)
		canonicalPath = filepath.Clean(canonicalPath)
		if !strings.HasPrefix(canonicalPath, canonicalMountpoint) {
			WriteAuditLog(db, actor, "shadow.flushed", "file_shadow", s.ID, "error", "canonical path escape")
			failed++
			continue
		}

		// Copy shadow -> canonical via temp + atomic rename
		tempPath := canonicalPath + ".tmp"
		if err := os.MkdirAll(filepath.Dir(tempPath), 0o755); err != nil {
			WriteAuditLog(db, actor, "shadow.flushed", "file_shadow", s.ID, "error", "mkdir canonical temp: "+err.Error())
			failed++
			continue
		}
		// Copy file
		data, err := os.ReadFile(shadowPath)
		if err != nil {
			WriteAuditLog(db, actor, "shadow.flushed", "file_shadow", s.ID, "error", "read shadow: "+err.Error())
			failed++
			continue
		}
		if err := os.WriteFile(tempPath, data, 0o644); err != nil {
			WriteAuditLog(db, actor, "shadow.flushed", "file_shadow", s.ID, "error", "write canonical temp: "+err.Error())
			_ = os.Remove(tempPath)
			failed++
			continue
		}
		// Verify SHA-256 of new canonical against row
		canonicalSHA, _, err := hashFile(tempPath)
		if err != nil {
			WriteAuditLog(db, actor, "shadow.flushed", "file_shadow", s.ID, "error", "hash canonical temp: "+err.Error())
			_ = os.Remove(tempPath)
			failed++
			continue
		}
		if canonicalSHA != s.SHA256 {
			WriteAuditLog(db, actor, "shadow.flushed", "file_shadow", s.ID, "error", "checksum mismatch after copy")
			_ = os.Remove(tempPath)
			failed++
			continue
		}
		// Atomic rename
		if err := os.Rename(tempPath, canonicalPath); err != nil {
			WriteAuditLog(db, actor, "shadow.flushed", "file_shadow", s.ID, "error", "rename canonical: "+err.Error())
			_ = os.Remove(tempPath)
			failed++
			continue
		}

		// Clear dirty + dirty_at (version stays as the newest version number)
		_, err = db.Exec(`
			UPDATE file_shadows SET dirty = 0, dirty_at = 0 WHERE collection_id = ? AND rel_path = ?
		`, s.CollectionID, s.RelPath)
		if err != nil {
			WriteAuditLog(db, actor, "shadow.flushed", "file_shadow", s.ID, "error", "clear dirty: "+err.Error())
			failed++
			continue
		}

		WriteAuditLog(db, actor, "shadow.flushed", "file_shadow", s.ID, "ok", fmt.Sprintf("collection=%s rel_path=%s version=%d", s.CollectionID, s.RelPath, s.Version))
		flushed++
	}

	return flushed, failed, nil
}

// ListDirtyShadowsOlderThan returns dirty shadows for a pool older than the given age limit.
// Used by the daily age-trigger job.
func ListDirtyShadowsOlderThan(db *sql.DB, poolID string, ageLimitHours int) ([]*ShadowRow, error) {
	cutoff := time.Now().Add(-time.Duration(ageLimitHours) * time.Hour).Unix()
	rows, err := db.Query(`
		SELECT id, file_id, collection_id, rel_path, shadow_pool_id, shadow_rel_path, sha256, size_bytes, created_at, last_served_at, dirty, version, dirty_at
		FROM file_shadows WHERE shadow_pool_id = ? AND dirty = 1 AND dirty_at > 0 AND dirty_at <= ?
	`, poolID, cutoff)
	if err != nil {
		return nil, fmt.Errorf("query old dirty shadows: %w", err)
	}
	defer rows.Close()

	var shadows []*ShadowRow
	for rows.Next() {
		var s ShadowRow
		var createdAt, lastServedAt, dirtyAt int64
		var fileID sql.NullString
		var dirty, version int
		if err := rows.Scan(&s.ID, &fileID, &s.CollectionID, &s.RelPath, &s.ShadowPoolID, &s.ShadowRelPath, &s.SHA256, &s.SizeBytes, &createdAt, &lastServedAt, &dirty, &version, &dirtyAt); err != nil {
			return nil, fmt.Errorf("scan old dirty shadow: %w", err)
		}
		if fileID.Valid {
			s.FileID = fileID.String
		}
		s.CreatedAt = time.Unix(createdAt, 0)
		if lastServedAt > 0 {
			s.LastServedAt = time.Unix(lastServedAt, 0)
		}
		s.Dirty = dirty
		s.Version = version
		if dirtyAt > 0 {
			s.DirtyAt = time.Unix(dirtyAt, 0)
		}
		shadows = append(shadows, &s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate old dirty shadows: %w", err)
	}
	return shadows, nil
}

// GetDirtyShadowsByPool returns all dirty shadows for a collection, grouped by shadow_pool_id.
// Returns a map of shadow_pool_id -> []*ShadowRow. Returns empty map if none found.
func GetDirtyShadowsByPool(db *sql.DB, collectionID string) (map[string][]*ShadowRow, error) {
	rows, err := db.Query(`
		SELECT id, file_id, collection_id, rel_path, shadow_pool_id, shadow_rel_path, sha256, size_bytes, created_at, last_served_at, dirty, version, dirty_at
		FROM file_shadows WHERE collection_id = ? AND dirty = 1
	`, collectionID)
	if err != nil {
		return nil, fmt.Errorf("query dirty shadows: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]*ShadowRow)
	for rows.Next() {
		var s ShadowRow
		var createdAt, lastServedAt, dirtyAt int64
		var fileID sql.NullString
		var dirty, version int
		if err := rows.Scan(&s.ID, &fileID, &s.CollectionID, &s.RelPath, &s.ShadowPoolID, &s.ShadowRelPath, &s.SHA256, &s.SizeBytes, &createdAt, &lastServedAt, &dirty, &version, &dirtyAt); err != nil {
			return nil, fmt.Errorf("scan dirty shadow: %w", err)
		}
		if fileID.Valid {
			s.FileID = fileID.String
		}
		s.CreatedAt = time.Unix(createdAt, 0)
		if lastServedAt > 0 {
			s.LastServedAt = time.Unix(lastServedAt, 0)
		}
		s.Dirty = dirty
		s.Version = version
		if dirtyAt > 0 {
			s.DirtyAt = time.Unix(dirtyAt, 0)
		}
		result[s.ShadowPoolID] = append(result[s.ShadowPoolID], &s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dirty shadows: %w", err)
	}
	return result, nil
}

// ShadowPromotionRow is the catalog representation of a shadow promotion
// recommendation (FR-2.8).
type ShadowPromotionRow struct {
	ID              string    `json:"id"`
	CollectionID    string    `json:"collection_id"`
	RelPath         string    `json:"rel_path"`
	ShadowPoolID    string    `json:"shadow_pool_id"`
	AccessCount30d  int       `json:"access_count_30d"`
	Status          string    `json:"status"` // pending | approved | rejected | superseded
	CreatedAt       time.Time `json:"created_at"`
	DecidedAt       time.Time `json:"decided_at,omitempty"`
}

// ShadowDeleteFunc deletes a shadow file. Injectable for tests.
type ShadowDeleteFunc func(path string) error

// DefaultShadowDeleteFunc is the production delete: removes the file.
func DefaultShadowDeleteFunc(path string) error {
	return os.Remove(path)
}

// EvictShadows evicts shadows from a Warm pool according to FR-2.8 rules:
// (a) OVER QUOTA: evict in LRU order (least recently served first; last_served_at,
//     fallback created_at when 0) until total shadow size_bytes <= quota.
// (b) IDLE: evict any shadow unserved for >= ShadowIdleDays even when under quota.
// HARD SAFETY: NEVER evict a dirty shadow (dirty=1 holds unflushed edits).
// If a pool is over quota purely because of dirty shadows, evict nothing and audit a warning.
// deleteFunc is injectable for tests. Returns (evictedCount, error).
func EvictShadows(db *sql.DB, poolID string, deleteFunc ShadowDeleteFunc, actor string) (int, error) {
	if deleteFunc == nil {
		deleteFunc = DefaultShadowDeleteFunc
	}

	// Get all shadows for this pool (clean only - dirty shadows are excluded)
	rows, err := db.Query(`
		SELECT id, collection_id, rel_path, shadow_rel_path, size_bytes, created_at, last_served_at, dirty
		FROM file_shadows WHERE shadow_pool_id = ?
	`, poolID)
	if err != nil {
		return 0, fmt.Errorf("query shadows: %w", err)
	}
	defer rows.Close()

	type shadowInfo struct {
		ID            string
		CollectionID  string
		RelPath       string
		ShadowRelPath string
		SizeBytes     int64
		CreatedAt     int64
		LastServedAt  int64
		Dirty         int
	}
	var allShadows []shadowInfo
	var totalSize int64
	var dirtySize int64
	for rows.Next() {
		var s shadowInfo
		if err := rows.Scan(&s.ID, &s.CollectionID, &s.RelPath, &s.ShadowRelPath, &s.SizeBytes, &s.CreatedAt, &s.LastServedAt, &s.Dirty); err != nil {
			return 0, fmt.Errorf("scan shadow: %w", err)
		}
		allShadows = append(allShadows, s)
		totalSize += s.SizeBytes
		if s.Dirty == 1 {
			dirtySize += s.SizeBytes
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate shadows: %w", err)
	}

	// Get pool size_bytes for quota calculation
	pool, _, err := GetPool(db, poolID)
	if err != nil || pool == nil {
		return 0, fmt.Errorf("get shadow pool: %w", err)
	}
	quotaBytes := pool.SizeBytes * int64(ShadowQuotaPercent) / 100

	evicted := 0

	// HARD SAFETY: If pool is over quota purely because of dirty shadows,
	// evict nothing and audit a warning.
	if totalSize > quotaBytes && dirtySize == totalSize {
		WriteAuditLog(db, actor, "shadow.evicted", "file_shadow", poolID, "warning", fmt.Sprintf("pool over quota (%d bytes > %d quota) purely due to dirty shadows (%d bytes); evicting nothing", totalSize, quotaBytes, dirtySize))
		return 0, nil
	}

	// Separate clean shadows (eligible for eviction) from dirty shadows (never evicted)
	var cleanShadows []shadowInfo
	for _, s := range allShadows {
		if s.Dirty == 0 {
			cleanShadows = append(cleanShadows, s)
		}
	}

	// Trigger (a): OVER QUOTA - evict in LRU order until total <= quota
	if totalSize > quotaBytes {
		// Sort clean shadows by last_served_at ASC (LRU first), fallback created_at
		sort.Slice(cleanShadows, func(i, j int) bool {
			li := cleanShadows[i].LastServedAt
			if li == 0 {
				li = cleanShadows[i].CreatedAt
			}
			lj := cleanShadows[j].LastServedAt
			if lj == 0 {
				lj = cleanShadows[j].CreatedAt
			}
			return li < lj
		})

		for _, s := range cleanShadows {
			if totalSize <= quotaBytes {
				break
			}
			// Resolve shadow file path (poolID is known from function parameter)
			shadowPath, err := resolveShadowPath(db, poolID, s.ShadowRelPath)
			if err != nil {
				WriteAuditLog(db, actor, "shadow.evicted", "file_shadow", s.ID, "error", "resolve shadow path: "+err.Error())
				continue
			}
			// Delete shadow file (best-effort)
			if err := deleteFunc(shadowPath); err != nil {
				WriteAuditLog(db, actor, "shadow.evicted", "file_shadow", s.ID, "error", "delete shadow file: "+err.Error())
				continue
			}
			// Delete shadow row
			if _, err := db.Exec(`DELETE FROM file_shadows WHERE id = ?`, s.ID); err != nil {
				WriteAuditLog(db, actor, "shadow.evicted", "file_shadow", s.ID, "error", "delete shadow row: "+err.Error())
				continue
			}
			totalSize -= s.SizeBytes
			evicted++
			WriteAuditLog(db, actor, "shadow.evicted", "file_shadow", s.ID, "ok", fmt.Sprintf("reason=over_quota collection=%s rel_path=%s size=%d", s.CollectionID, s.RelPath, s.SizeBytes))
		}
	}

	// Trigger (b): IDLE - evict any clean shadow unserved for >= ShadowIdleDays
	idleCutoff := time.Now().AddDate(0, 0, -ShadowIdleDays).Unix()
	for _, s := range cleanShadows {
		// Skip already-evicted shadows (from over-quota pass above)
		var exists int
		err := db.QueryRow(`SELECT COUNT(*) FROM file_shadows WHERE id = ?`, s.ID).Scan(&exists)
		if err != nil || exists == 0 {
			continue
		}
		lastServed := s.LastServedAt
		if lastServed == 0 {
			lastServed = s.CreatedAt
		}
		if lastServed < idleCutoff {
			shadowPath, err := resolveShadowPath(db, poolID, s.ShadowRelPath)
			if err != nil {
				WriteAuditLog(db, actor, "shadow.evicted", "file_shadow", s.ID, "error", "resolve shadow path: "+err.Error())
				continue
			}
			if err := deleteFunc(shadowPath); err != nil {
				WriteAuditLog(db, actor, "shadow.evicted", "file_shadow", s.ID, "error", "delete shadow file: "+err.Error())
				continue
			}
			if _, err := db.Exec(`DELETE FROM file_shadows WHERE id = ?`, s.ID); err != nil {
				WriteAuditLog(db, actor, "shadow.evicted", "file_shadow", s.ID, "error", "delete shadow row: "+err.Error())
				continue
			}
			evicted++
			WriteAuditLog(db, actor, "shadow.evicted", "file_shadow", s.ID, "ok", fmt.Sprintf("reason=idle collection=%s rel_path=%s last_served=%d", s.CollectionID, s.RelPath, lastServed))
		}
	}

	return evicted, nil
}

// resolveShadowPath resolves the full filesystem path of a shadow file.
func resolveShadowPath(db *sql.DB, shadowPoolID, shadowRelPath string) (string, error) {
	pool, _, err := GetPool(db, shadowPoolID)
	if err != nil || pool == nil {
		return "", fmt.Errorf("shadow pool not found: %s", shadowPoolID)
	}
	mountpoint := pool.Mountpoint
	if mountpoint == "" {
		mountpoint = "/mnt/" + pool.Name
	}
	shadowPath := filepath.Join(mountpoint, ".kelvios-shadow", shadowRelPath)
	shadowPath = filepath.Clean(shadowPath)
	if !strings.HasPrefix(shadowPath, mountpoint) {
		return "", fmt.Errorf("shadow path escape")
	}
	return shadowPath, nil
}

// GenerateShadowPromotions creates PENDING promotion recommendations for
// shadows that exceed the access threshold (> ShadowPromotionAccessThreshold
// access events in the last ShadowPromotionWindowDays days).
// Exclusions: dirty shadows, shadows with an already non-superseded promotion row.
// Returns (createdCount, error).
func GenerateShadowPromotions(db *sql.DB, actor string) (int, error) {
	// Get all clean shadows
	rows, err := db.Query(`
		SELECT fs.id, fs.collection_id, fs.rel_path, fs.shadow_pool_id, fs.size_bytes
		FROM file_shadows fs
		WHERE fs.dirty = 0
	`)
	if err != nil {
		return 0, fmt.Errorf("query clean shadows: %w", err)
	}
	defer rows.Close()

	type shadowCandidate struct {
		ID           string
		CollectionID string
		RelPath      string
		ShadowPoolID string
		SizeBytes    int64
	}
	var candidates []shadowCandidate
	for rows.Next() {
		var s shadowCandidate
		if err := rows.Scan(&s.ID, &s.CollectionID, &s.RelPath, &s.ShadowPoolID, &s.SizeBytes); err != nil {
			return 0, fmt.Errorf("scan shadow candidate: %w", err)
		}
		candidates = append(candidates, s)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate shadow candidates: %w", err)
	}

	created := 0
	now := time.Now().Unix()

	for _, c := range candidates {
		// Count recent accesses
		accessCount, err := CountRecentAccesses(db, c.CollectionID, c.RelPath, ShadowPromotionWindowDays)
		if err != nil {
			WriteAuditLog(db, actor, "shadow.promotion.generate", "shadow_promotion", c.ID, "error", "count recent accesses: "+err.Error())
			continue
		}

		// Must exceed threshold (> threshold, not >=)
		if accessCount <= ShadowPromotionAccessThreshold {
			continue
		}

		// Check if there's already a non-superseded promotion row for this (collection, rel_path)
		var existingStatus string
		err = db.QueryRow(`
			SELECT status FROM shadow_promotions
			WHERE collection_id = ? AND rel_path = ? AND status != 'superseded'
			LIMIT 1
		`, c.CollectionID, c.RelPath).Scan(&existingStatus)
		if err == nil {
			// Already has a non-superseded promotion row - skip
			continue
		}
		if err != sql.ErrNoRows {
			WriteAuditLog(db, actor, "shadow.promotion.generate", "shadow_promotion", c.ID, "error", "check existing promotion: "+err.Error())
			continue
		}

		// Insert PENDING promotion row
		id := generateShadowPromotionID()
		_, err = db.Exec(`
			INSERT INTO shadow_promotions (id, collection_id, rel_path, shadow_pool_id, access_count_30d, status, created_at, decided_at)
			VALUES (?, ?, ?, ?, ?, 'pending', ?, 0)
		`, id, c.CollectionID, c.RelPath, c.ShadowPoolID, accessCount, now)
		if err != nil {
			WriteAuditLog(db, actor, "shadow.promotion.generate", "shadow_promotion", c.ID, "error", "insert promotion row: "+err.Error())
			continue
		}

		WriteAuditLog(db, actor, "shadow.promotion.generate", "shadow_promotion", id, "ok", fmt.Sprintf("collection=%s rel_path=%s access_count_30d=%d reason=hot_%d_in_30d", c.CollectionID, c.RelPath, accessCount, accessCount))
		created++
	}

	return created, nil
}

// generateShadowPromotionID returns a collision-proof shadow promotion ID.
// Format: shp-<unix-nano>-<8 hex chars from crypto/rand>
func generateShadowPromotionID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("shp-%d-%08x", now, counter)
	}
	return fmt.Sprintf("shp-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// DecideShadowPromotion records the decision (approve/reject) on a pending
// promotion recommendation. Only records the decision - the actual per-file
// canonical move to Warm is EXPLICITLY OUT of WS37.
// Returns error if the promotion row is not found or not in pending status.
func DecideShadowPromotion(db *sql.DB, promotionID, decision, actor string) error {
	if decision != "approve" && decision != "reject" {
		return fmt.Errorf("invalid decision: %s (must be approve or reject)", decision)
	}

	now := time.Now().Unix()

	// Get the promotion row and verify it's pending
	var status string
	err := db.QueryRow(`SELECT status FROM shadow_promotions WHERE id = ?`, promotionID).Scan(&status)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("promotion not found: %s", promotionID)
		}
		return fmt.Errorf("get promotion: %w", err)
	}
	if status != "pending" {
		return fmt.Errorf("promotion %s is not pending (status=%s)", promotionID, status)
	}

	// Update status and decided_at
	_, err = db.Exec(`
		UPDATE shadow_promotions SET status = ?, decided_at = ? WHERE id = ?
	`, decision, now, promotionID)
	if err != nil {
		return fmt.Errorf("update promotion: %w", err)
	}

	// Audit the decision
	action := "shadow.promotion." + decision
	WriteAuditLog(db, actor, action, "shadow_promotion", promotionID, "ok", fmt.Sprintf("decision=%s", decision))

	return nil
}

// ListShadowPromotions returns shadow promotions with optional status filter.
// Returns (entries, total, error).
func ListShadowPromotions(db *sql.DB, status string, limit, offset int) ([]*ShadowPromotionRow, int, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	// Build WHERE clause
	whereClauses := []string{}
	args := []any{}

	if status != "" {
		whereClauses = append(whereClauses, "status = ?")
		args = append(args, status)
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	// Count total
	countQuery := "SELECT COUNT(*) FROM shadow_promotions " + whereSQL
	var total int
	err := db.QueryRow(countQuery, args...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count shadow promotions: %w", err)
	}

	// Query entries
	query := fmt.Sprintf("SELECT id, collection_id, rel_path, shadow_pool_id, access_count_30d, status, created_at, decided_at FROM shadow_promotions %s ORDER BY created_at DESC LIMIT ? OFFSET ?", whereSQL)
	args = append(args, limit, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query shadow promotions: %w", err)
	}
	defer rows.Close()

	var entries []*ShadowPromotionRow
	for rows.Next() {
		var e ShadowPromotionRow
		var createdAt, decidedAt int64
		if err := rows.Scan(&e.ID, &e.CollectionID, &e.RelPath, &e.ShadowPoolID, &e.AccessCount30d, &e.Status, &createdAt, &decidedAt); err != nil {
			return nil, 0, fmt.Errorf("scan shadow promotion: %w", err)
		}
		e.CreatedAt = time.Unix(createdAt, 0)
		if decidedAt > 0 {
			e.DecidedAt = time.Unix(decidedAt, 0)
		}
		entries = append(entries, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate shadow promotions: %w", err)
	}

	if entries == nil {
		entries = []*ShadowPromotionRow{}
	}

	return entries, total, nil
}
