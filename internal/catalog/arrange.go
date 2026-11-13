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
)

// ============================================================================
// WS38 (2026-11-06): Auto-Arrange for media collections (FR-2.11)
// ============================================================================

// ArrangePlanRow represents an auto-arrange plan.
type ArrangePlanRow struct {
	ID            string    `json:"id"`
	CollectionID  string    `json:"collection_id"`
	Status        string    `json:"status"` // pending | approved | rejected | applied | undone
	CreatedAt     time.Time `json:"created_at"`
	DecidedAt     time.Time `json:"decided_at,omitempty"`
	AppliedAt     time.Time `json:"applied_at,omitempty"`
	UndoneAt      time.Time `json:"undone_at,omitempty"`
	ConflictCount int       `json:"conflict_count"`
	MoveCount     int       `json:"move_count"`
}

// ArrangeMoveRow represents a single move within an arrange plan.
type ArrangeMoveRow struct {
	ID        string    `json:"id"`
	PlanID    string    `json:"plan_id"`
	SourceRel string    `json:"source_rel"`
	DestRel   string    `json:"dest_rel"`
	Conflict  string    `json:"conflict"` // "" | "dest_exists" | "duplicate_dest"
	Status    string    `json:"status"`   // planned | moved | skipped_conflict | undone
	SHA256    string    `json:"sha256,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Arrange plan status values.
const (
	ArrangeStatusPending  = "pending"
	ArrangeStatusApproved = "approved"
	ArrangeStatusRejected = "rejected"
	ArrangeStatusApplied  = "applied"
	ArrangeStatusUndone   = "undone"
)

// Arrange move status values.
const (
	ArrangeMovePlanned         = "planned"
	ArrangeMoveMoved           = "moved"
	ArrangeMoveSkippedConflict = "skipped_conflict"
	ArrangeMoveUndone          = "undone"
)

// Arrange move conflict values.
const (
	ArrangeConflictNone          = ""
	ArrangeConflictDestExists    = "dest_exists"
	ArrangeConflictDuplicateDest = "duplicate_dest"
)

// Media extensions (case-insensitive).
var mediaExts = map[string]bool{
	".jpg":  true,
	".jpeg": true,
	".png":  true,
	".heic": true,
	".mp4":  true,
	".mov":  true,
	".mkv":  true,
	".avi":  true,
	".webm": true,
}

// isMediaFile returns true if the file has a media extension.
func isMediaFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return mediaExts[ext]
}

// sanitizeSegment sanitizes a path segment for use as a directory name.
// Strips path separators and other problematic characters.
func sanitizeSegment(s string) string {
	// Replace path separators
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	// Remove other problematic chars
	s = strings.ReplaceAll(s, ":", "_")
	s = strings.ReplaceAll(s, "*", "_")
	s = strings.ReplaceAll(s, "?", "_")
	s = strings.ReplaceAll(s, "\"", "_")
	s = strings.ReplaceAll(s, "<", "_")
	s = strings.ReplaceAll(s, ">", "_")
	s = strings.ReplaceAll(s, "|", "_")
	// Trim whitespace and dots
	s = strings.TrimSpace(s)
	s = strings.Trim(s, ".")
	// Limit length
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		s = "unknown"
	}
	return s
}

// formatGPSBucket formats lat/lon to 2 decimal places for Places bucket.
func formatGPSBucket(lat, lon float64) string {
	return fmt.Sprintf("%.2f,%.2f", lat, lon)
}

// computeDestination computes the destination relative path for a media file.
// Rules (deterministic):
// - dated media (EXIF DateTimeOriginal or mtime) → <YYYY>/<YYYY-MM-DD>/<filename>
// - EXIF Model present → prefix Devices/<Model>/ before date folders
// - GPS present → prefix Places/<lat2dp>,<lon2dp>/ (wins over device prefix)
// - undated + no EXIF → Undated/<filename>
func computeDestination(relPath string, exif *EXIFData, modTime time.Time) string {
	filename := filepath.Base(relPath)
	var datePrefix string
	var hasDate bool

	if exif != nil && !exif.DateTimeOriginal.IsZero() {
		datePrefix = exif.DateTimeOriginal.Format("2006/2006-01-02")
		hasDate = true
	} else if !modTime.IsZero() {
		datePrefix = modTime.Format("2006/2006-01-02")
		hasDate = true
	}

	var prefix string
	if exif != nil && exif.HasGPS {
		// GPS prefix wins over device prefix
		prefix = "Places/" + sanitizeSegment(formatGPSBucket(exif.Latitude, exif.Longitude))
	} else if exif != nil && exif.Model != "" {
		prefix = "Devices/" + sanitizeSegment(exif.Model)
	}

	if hasDate {
		if prefix != "" {
			return prefix + "/" + datePrefix + "/" + filename
		}
		return datePrefix + "/" + filename
	}

	// Undated
	if prefix != "" {
		return prefix + "/Undated/" + filename
	}
	return "Undated/" + filename
}

// generateArrangePlanID returns a collision-proof arrange plan ID.
// Format: ap-<unix-nano>-<8 hex chars from crypto/rand>
func generateArrangePlanID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("ap-%d-%08x", now, counter)
	}
	return fmt.Sprintf("ap-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// generateArrangeMoveID returns a collision-proof arrange move ID.
// Format: am-<unix-nano>-<8 hex chars from crypto/rand>
func generateArrangeMoveID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("am-%d-%08x", now, counter)
	}
	return fmt.Sprintf("am-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// ArrangePlanResult holds the result of plan generation.
type ArrangePlanResult struct {
	Plan         *ArrangePlanRow
	Moves        []*ArrangeMoveRow
	ConflictCount int
}

// GenerateArrangePlan walks the collection tree on disk and generates an
// approval-first arrangement plan. Returns the plan, moves, and conflict count.
// collectionRoot is the absolute filesystem path to the collection root.
// db is used only for audit logging (can be nil for dry-run).
func GenerateArrangePlan(db *sql.DB, collectionID, collectionRoot string, actor string) (*ArrangePlanResult, error) {
	// Verify root exists
	info, err := os.Stat(collectionRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("collection root does not exist: %s", collectionRoot)
		}
		return nil, fmt.Errorf("stat collection root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("collection root is not a directory: %s", collectionRoot)
	}

	now := time.Now().Unix()
	planID := generateArrangePlanID()

	// Walk the tree and collect media files
	type fileInfo struct {
		relPath  string
		modTime  time.Time
		size     int64
		exifData *EXIFData
	}
	var files []fileInfo

	err = filepath.WalkDir(collectionRoot, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return filepath.SkipDir
		}
		if d.IsDir() {
			// Skip dot directories and .kelvios-shadow
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == ".kelvios-shadow" {
				return filepath.SkipDir
			}
			// Skip symlinked directories
			if d.Type()&os.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			return nil
		}
		// Regular file
		name := d.Name()
		// Skip dotfiles
		if strings.HasPrefix(name, ".") {
			return nil
		}
		// Only media files
		if !isMediaFile(name) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // skip unreadable
		}
		// Skip symlinks
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		relPath, err := filepath.Rel(collectionRoot, path)
		if err != nil {
			return nil
		}
		if strings.HasPrefix(relPath, "..") {
			return nil
		}
		relPath = filepath.ToSlash(relPath)

		// Parse EXIF for JPEG files
		var exifData *EXIFData
		if strings.EqualFold(filepath.Ext(name), ".jpg") || strings.EqualFold(filepath.Ext(name), ".jpeg") {
			exifData, _ = ParseEXIF(path) // best effort
		}

		files = append(files, fileInfo{
			relPath:  relPath,
			modTime:  info.ModTime(),
			size:     info.Size(),
			exifData: exifData,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk collection root: %w", err)
	}

	// Sort files by relPath for deterministic order
	sort.Slice(files, func(i, j int) bool {
		return files[i].relPath < files[j].relPath
	})

	// Compute destinations and detect conflicts
	destMap := make(map[string][]int) // destRel -> indices in files
	var moves []*ArrangeMoveRow

	for _, f := range files {
		destRel := computeDestination(f.relPath, f.exifData, f.modTime)

		// Idempotent: skip if source already at destination
		if f.relPath == destRel {
			continue
		}

		move := &ArrangeMoveRow{
			ID:        generateArrangeMoveID(),
			PlanID:    planID,
			SourceRel: f.relPath,
			DestRel:   destRel,
			Conflict:  ArrangeConflictNone,
			Status:    ArrangeMovePlanned,
			CreatedAt: time.Unix(now, 0),
		}
		moves = append(moves, move)
		destMap[destRel] = append(destMap[destRel], len(moves)-1)
	}

	// Detect duplicate_dest conflicts (multiple sources -> same dest)
	conflictCount := 0
	for _, indices := range destMap {
		if len(indices) > 1 {
			for _, moveIdx := range indices {
				if moves[moveIdx].Conflict == ArrangeConflictNone {
					moves[moveIdx].Conflict = ArrangeConflictDuplicateDest
					conflictCount++
				}
			}
		}
	}

	// Check for dest_exists conflicts (dest already exists on disk and is not the source)
	for _, move := range moves {
		if move.Conflict != ArrangeConflictNone {
			continue // already has conflict
		}
		destPath := filepath.Join(collectionRoot, move.DestRel)
		destPath = filepath.Clean(destPath)
		if !strings.HasPrefix(destPath, collectionRoot) {
			continue // safety
		}
		if _, err := os.Stat(destPath); err == nil {
			// Dest exists - check if it's the same file (source == dest already handled above)
			move.Conflict = ArrangeConflictDestExists
			conflictCount++
		}
	}

	// Create plan row
	plan := &ArrangePlanRow{
		ID:            planID,
		CollectionID:  collectionID,
		Status:        ArrangeStatusPending,
		CreatedAt:     time.Unix(now, 0),
		ConflictCount: conflictCount,
		MoveCount:     len(moves),
	}

	// Persist to database
	if db != nil {
		tx, err := db.Begin()
		if err != nil {
			return nil, fmt.Errorf("begin tx: %w", err)
		}
		defer tx.Rollback()

		_, err = tx.Exec(`
			INSERT INTO arrange_plans (id, collection_id, status, created_at, decided_at, applied_at, undone_at)
			VALUES (?, ?, ?, ?, 0, 0, 0)
		`, plan.ID, plan.CollectionID, plan.Status, plan.CreatedAt.Unix())
		if err != nil {
			return nil, fmt.Errorf("insert arrange plan: %w", err)
		}

		for _, move := range moves {
			_, err = tx.Exec(`
						INSERT INTO arrange_moves (id, plan_id, source_rel, dest_rel, conflict, status, sha256, created_at)
						VALUES (?, ?, ?, ?, ?, ?, '', ?)
					`, move.ID, move.PlanID, move.SourceRel, move.DestRel, move.Conflict, move.Status, move.CreatedAt.Unix())
					if err != nil {
						return nil, fmt.Errorf("insert arrange move: %w", err)
					}
				}

				if err := tx.Commit(); err != nil {
					return nil, fmt.Errorf("commit tx: %w", err)
				}

				// Audit log
				WriteAuditLog(db, actor, "arrange.plan.generate", "arrange_plan", planID, "ok",
					fmt.Sprintf("collection=%s moves=%d conflicts=%d", collectionID, len(moves), conflictCount))
	}

	return &ArrangePlanResult{
		Plan:          plan,
		Moves:         moves,
		ConflictCount: conflictCount,
	}, nil
}

// ListArrangePlans returns arrange plans for a collection, optionally filtered by status.
func ListArrangePlans(db *sql.DB, collectionID, status string, limit, offset int) ([]*ArrangePlanRow, int, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	whereClauses := []string{"collection_id = ?"}
	args := []any{collectionID}
	if status != "" {
		whereClauses = append(whereClauses, "status = ?")
		args = append(args, status)
	}
	whereSQL := "WHERE " + strings.Join(whereClauses, " AND ")

	// Count total
	countQuery := "SELECT COUNT(*) FROM arrange_plans " + whereSQL
	var total int
	err := db.QueryRow(countQuery, args...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count arrange plans: %w", err)
	}

	// Query plans
	query := fmt.Sprintf(`
		SELECT id, collection_id, status, created_at, decided_at, applied_at, undone_at
		FROM arrange_plans %s
		ORDER BY created_at DESC, rowid DESC
		LIMIT ? OFFSET ?
	`, whereSQL)
	args = append(args, limit, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query arrange plans: %w", err)
	}

	var plans []*ArrangePlanRow
	for rows.Next() {
		var p ArrangePlanRow
		var createdAt, decidedAt, appliedAt, undoneAt int64
		if err := rows.Scan(&p.ID, &p.CollectionID, &p.Status, &createdAt, &decidedAt, &appliedAt, &undoneAt); err != nil {
			return nil, 0, fmt.Errorf("scan arrange plan: %w", err)
		}
		p.CreatedAt = time.Unix(createdAt, 0)
		if decidedAt > 0 {
			p.DecidedAt = time.Unix(decidedAt, 0)
		}
		if appliedAt > 0 {
			p.AppliedAt = time.Unix(appliedAt, 0)
		}
		if undoneAt > 0 {
			p.UndoneAt = time.Unix(undoneAt, 0)
		}
		plans = append(plans, &p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, fmt.Errorf("iterate arrange plans: %w", err)
	}
	// Close BEFORE the per-plan count queries: the catalog holds a single
	// connection (MaxOpenConns(1)), so any query issued while rows are open
	// deadlocks. Fetch first, then query.
	rows.Close()

	// Fetch move count and conflict count per plan.
	for _, p := range plans {
		err = db.QueryRow(`
			SELECT COUNT(*), COALESCE(SUM(CASE WHEN conflict != '' THEN 1 ELSE 0 END), 0)
			FROM arrange_moves WHERE plan_id = ?
		`, p.ID).Scan(&p.MoveCount, &p.ConflictCount)
		if err != nil {
			return nil, 0, fmt.Errorf("query move counts: %w", err)
		}
	}

	if plans == nil {
		plans = []*ArrangePlanRow{}
	}
	return plans, total, nil
}

// GetArrangePlan retrieves an arrange plan by ID with its moves.
func GetArrangePlan(db *sql.DB, planID string) (*ArrangePlanRow, []*ArrangeMoveRow, error) {
	row := db.QueryRow(`
		SELECT id, collection_id, status, created_at, decided_at, applied_at, undone_at
		FROM arrange_plans WHERE id = ?
	`, planID)
	var p ArrangePlanRow
	var createdAt, decidedAt, appliedAt, undoneAt int64
	if err := row.Scan(&p.ID, &p.CollectionID, &p.Status, &createdAt, &decidedAt, &appliedAt, &undoneAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("scan arrange plan: %w", err)
	}
	p.CreatedAt = time.Unix(createdAt, 0)
	if decidedAt > 0 {
		p.DecidedAt = time.Unix(decidedAt, 0)
	}
	if appliedAt > 0 {
		p.AppliedAt = time.Unix(appliedAt, 0)
	}
	if undoneAt > 0 {
		p.UndoneAt = time.Unix(undoneAt, 0)
	}

	// Fetch moves
	rows, err := db.Query(`
		SELECT id, plan_id, source_rel, dest_rel, conflict, status, sha256, created_at
		FROM arrange_moves WHERE plan_id = ? ORDER BY created_at, rowid
	`, planID)
	if err != nil {
		return nil, nil, fmt.Errorf("query arrange moves: %w", err)
	}

	var moves []*ArrangeMoveRow
	for rows.Next() {
		var m ArrangeMoveRow
		var createdAt int64
		var sha256 sql.NullString
		if err := rows.Scan(&m.ID, &m.PlanID, &m.SourceRel, &m.DestRel, &m.Conflict, &m.Status, &sha256, &createdAt); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan arrange move: %w", err)
		}
		if sha256.Valid {
			m.SHA256 = sha256.String
		}
		m.CreatedAt = time.Unix(createdAt, 0)
		moves = append(moves, &m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, fmt.Errorf("iterate arrange moves: %w", err)
	}
	rows.Close()

	// Counts come from the fetched slice: no second query is needed, and
	// issuing one while rows are open would deadlock the single connection.
	p.MoveCount = len(moves)
	for _, m := range moves {
		if m.Conflict != "" {
			p.ConflictCount++
		}
	}

	if moves == nil {
		moves = []*ArrangeMoveRow{}
	}
	return &p, moves, nil
}

// DecideArrangePlan approves or rejects a pending plan.
// decision must be "approved" or "rejected".
func DecideArrangePlan(db *sql.DB, planID, decision string, now time.Time, actor string) error {
	if decision != ArrangeStatusApproved && decision != ArrangeStatusRejected {
		return fmt.Errorf("invalid decision: %s", decision)
	}

	nowUnix := now.Unix()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Check current status
	var currentStatus string
	err = tx.QueryRow(`SELECT status FROM arrange_plans WHERE id = ?`, planID).Scan(&currentStatus)
	if err == sql.ErrNoRows {
		return sql.ErrNoRows
	}
	if err != nil {
		return fmt.Errorf("query plan status: %w", err)
	}
	if currentStatus != ArrangeStatusPending {
		return fmt.Errorf("plan not pending (status: %s)", currentStatus)
	}

	// Update plan
	decidedCol := "decided_at"
	if decision == ArrangeStatusApproved {
		decidedCol = "decided_at"
	}
	_, err = tx.Exec(fmt.Sprintf(`
		UPDATE arrange_plans SET status = ?, %s = ? WHERE id = ?
	`, decidedCol), decision, nowUnix, planID)
	if err != nil {
		return fmt.Errorf("update plan: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}

	WriteAuditLog(db, actor, "arrange.plan."+decision, "arrange_plan", planID, "ok", "")
	return nil
}

// MoveFunc is the injectable function for executing a single move.
// Default is os.Rename (same filesystem). Tests can inject a stub.
type MoveFunc func(src, dst string) error

// DefaultMoveFunc is the production move implementation using os.Rename.
func DefaultMoveFunc(src, dst string) error {
	return os.Rename(src, dst)
}

// ApplyArrangePlan executes an approved plan.
// Moves are executed in sorted order (by created_at, rowid).
// Any failure stops execution; plan stays 'approved' with moved rows kept as journal.
// Returns error if plan not approved, has conflicts, or any move fails.
func ApplyArrangePlan(db *sql.DB, planID string, collectionRoot string, now time.Time, actor string, moveFunc MoveFunc) error {
	if moveFunc == nil {
		moveFunc = DefaultMoveFunc
	}

	nowUnix := now.Unix()

	// Fetch plan and verify status
	var planStatus, planCollectionID string
	err := db.QueryRow(`SELECT status, collection_id FROM arrange_plans WHERE id = ?`, planID).Scan(&planStatus, &planCollectionID)
	if err == sql.ErrNoRows {
		return sql.ErrNoRows
	}
	if err != nil {
		return fmt.Errorf("query plan: %w", err)
	}
	if planStatus != ArrangeStatusApproved {
		return fmt.Errorf("plan not approved (status: %s)", planStatus)
	}

	// Check for conflicts
	var conflictCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM arrange_moves WHERE plan_id = ? AND conflict != ''`, planID).Scan(&conflictCount)
	if err != nil {
		return fmt.Errorf("query conflicts: %w", err)
	}
	if conflictCount > 0 {
		return fmt.Errorf("plan has %d conflicts, cannot apply", conflictCount)
	}

	// Fetch moves in order
	rows, err := db.Query(`
		SELECT id, source_rel, dest_rel FROM arrange_moves
		WHERE plan_id = ? AND status = ? ORDER BY created_at, rowid
	`, planID, ArrangeMovePlanned)
	if err != nil {
		return fmt.Errorf("query moves: %w", err)
	}
	defer rows.Close()

	type moveItem struct {
		id       string
		sourceRel string
		destRel   string
	}
	var moves []moveItem
	for rows.Next() {
		var m moveItem
		if err := rows.Scan(&m.id, &m.sourceRel, &m.destRel); err != nil {
			return fmt.Errorf("scan move: %w", err)
		}
		moves = append(moves, m)
	}

	// Release the rows before issuing updates: single-connection DB.
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate moves: %w", err)
	}
	rows.Close()

	// Execute moves sequentially
	for _, m := range moves {
		srcPath := filepath.Join(collectionRoot, m.sourceRel)
		srcPath = filepath.Clean(srcPath)
		dstPath := filepath.Join(collectionRoot, m.destRel)
		dstPath = filepath.Clean(dstPath)

		// Safety: ensure paths stay within collection root
		if !strings.HasPrefix(srcPath, collectionRoot) || !strings.HasPrefix(dstPath, collectionRoot) {
			// Mark as skipped_conflict and stop
			_, _ = db.Exec(`UPDATE arrange_moves SET status = ? WHERE id = ?`, ArrangeMoveSkippedConflict, m.id)
			return fmt.Errorf("path escape detected for move %s", m.id)
		}

		// Re-check dest exists at apply time (race condition guard)
		if _, err := os.Stat(dstPath); err == nil {
			// Dest appeared since planning
			_, _ = db.Exec(`UPDATE arrange_moves SET status = ? WHERE id = ?`, ArrangeMoveSkippedConflict, m.id)
			return fmt.Errorf("destination appeared since planning: %s", m.destRel)
		}

		// Compute SHA-256 of source before move
		var sha256Hash string
		f, err := os.Open(srcPath)
		if err == nil {
			h := sha256.New()
			if _, err := io.Copy(h, f); err == nil {
				sha256Hash = hex.EncodeToString(h.Sum(nil))
			}
			f.Close()
		}

		// Ensure dest directory exists
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return fmt.Errorf("mkdir dest for %s: %w", m.destRel, err)
		}

		// Execute move
		if err := moveFunc(srcPath, dstPath); err != nil {
			return fmt.Errorf("move %s -> %s: %w", m.sourceRel, m.destRel, err)
		}

		// Verify dest exists
		if _, err := os.Stat(dstPath); err != nil {
			return fmt.Errorf("dest missing after move: %s", m.destRel)
		}

		// Update move row: status=moved, sha256=hash
		_, err = db.Exec(`
			UPDATE arrange_moves SET status = ?, sha256 = ? WHERE id = ?
		`, ArrangeMoveMoved, sha256Hash, m.id)
		if err != nil {
			return fmt.Errorf("update move row: %w", err)
		}
	}

	// All moves succeeded - mark plan applied
	_, err = db.Exec(`
		UPDATE arrange_plans SET status = ?, applied_at = ? WHERE id = ?
	`, ArrangeStatusApplied, nowUnix, planID)
	if err != nil {
		return fmt.Errorf("update plan applied: %w", err)
	}

	WriteAuditLog(db, actor, "arrange.plan.apply", "arrange_plan", planID, "ok",
		fmt.Sprintf("moves=%d", len(moves)))
	return nil
}

// UndoArrangePlan reverses an applied plan.
// Moves are undone in reverse order. Source directories are recreated as needed.
// Plan must be in 'applied' status.
func UndoArrangePlan(db *sql.DB, planID string, collectionRoot string, now time.Time, actor string, moveFunc MoveFunc) error {
	if moveFunc == nil {
		moveFunc = DefaultMoveFunc
	}

	nowUnix := now.Unix()

	// Fetch plan and verify status
	var planStatus string
	err := db.QueryRow(`SELECT status FROM arrange_plans WHERE id = ?`, planID).Scan(&planStatus)
	if err == sql.ErrNoRows {
		return sql.ErrNoRows
	}
	if err != nil {
		return fmt.Errorf("query plan: %w", err)
	}
	if planStatus != ArrangeStatusApplied {
		return fmt.Errorf("plan not applied (status: %s)", planStatus)
	}

	// Fetch moved moves in reverse order
	rows, err := db.Query(`
		SELECT id, source_rel, dest_rel FROM arrange_moves
		WHERE plan_id = ? AND status = ? ORDER BY created_at DESC, rowid DESC
	`, planID, ArrangeMoveMoved)
	if err != nil {
		return fmt.Errorf("query moves: %w", err)
	}
	defer rows.Close()

	type moveItem struct {
		id       string
		sourceRel string
		destRel   string
	}
	var moves []moveItem
	for rows.Next() {
		var m moveItem
		if err := rows.Scan(&m.id, &m.sourceRel, &m.destRel); err != nil {
			return fmt.Errorf("scan move: %w", err)
		}
		moves = append(moves, m)
	}

	// Release the rows before issuing updates: single-connection DB.
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate moves: %w", err)
	}
	rows.Close()

	// Execute undo in reverse order
	for _, m := range moves {
		srcPath := filepath.Join(collectionRoot, m.sourceRel)
		srcPath = filepath.Clean(srcPath)
		dstPath := filepath.Join(collectionRoot, m.destRel)
		dstPath = filepath.Clean(dstPath)

		// Safety check
		if !strings.HasPrefix(srcPath, collectionRoot) || !strings.HasPrefix(dstPath, collectionRoot) {
			return fmt.Errorf("path escape detected for undo %s", m.id)
		}

		// Ensure source directory exists
		if err := os.MkdirAll(filepath.Dir(srcPath), 0o755); err != nil {
			return fmt.Errorf("mkdir source for undo %s: %w", m.sourceRel, err)
		}

		// Move back: dest -> source
		if err := moveFunc(dstPath, srcPath); err != nil {
			return fmt.Errorf("undo move %s -> %s: %w", m.destRel, m.sourceRel, err)
		}

		// Update move row: status=undone
		_, err = db.Exec(`
			UPDATE arrange_moves SET status = ? WHERE id = ?
		`, ArrangeMoveUndone, m.id)
		if err != nil {
			return fmt.Errorf("update move row undo: %w", err)
		}
	}

	// Mark plan undone
	_, err = db.Exec(`
		UPDATE arrange_plans SET status = ?, undone_at = ? WHERE id = ?
	`, ArrangeStatusUndone, nowUnix, planID)
	if err != nil {
		return fmt.Errorf("update plan undone: %w", err)
	}

	WriteAuditLog(db, actor, "arrange.plan.undo", "arrange_plan", planID, "ok",
		fmt.Sprintf("moves=%d", len(moves)))
	return nil
}