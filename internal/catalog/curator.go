package catalog

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// ============================================================================
// WS33 (2026-11-03): Tier Curator recommendations (FR-2.2)
// ============================================================================

// TierRecommendationRow represents a migration recommendation.
type TierRecommendationRow struct {
	ID           string    `json:"id"`
	CollectionID string    `json:"collection_id"`
	PoolID       string    `json:"pool_id"`
	Direction    string    `json:"direction"` // "warm_to_cold" | "cold_to_warm"
	Reason       string    `json:"reason"`    // "idle_<N>d" | "never_accessed" | "heating_<count>_in_7d"
	SizeBytes    int64     `json:"size_bytes"`
	LastAccess   int64     `json:"last_access"`
	Score        float64   `json:"score"`
	Status       string    `json:"status"` // "pending" | "approved" | "rejected" | "superseded"
	CreatedAt    time.Time `json:"created_at"`
	DecidedAt    time.Time `json:"decided_at,omitempty"`
}

// TierPinRow represents a pinned (excluded) collection.
type TierPinRow struct {
	CollectionID string    `json:"collection_id"`
	PinnedAt     time.Time `json:"pinned_at"`
	Note         string    `json:"note,omitempty"`
}

// TierPolicyRow represents a per-pool tier policy.
type TierPolicyRow struct {
	PoolID             string `json:"pool_id"`
	Mode               string `json:"mode"`                 // "recommend_only" | "auto_if_no_response" | "never_automatic"
	ColdThresholdDays  int    `json:"cold_threshold_days"`  // default 21
	AutoMoveWaitDays   int    `json:"auto_move_wait_days"`  // default 7
}

// DefaultTierPolicy returns the default policy when no row exists.
func DefaultTierPolicy() TierPolicyRow {
	return TierPolicyRow{
		Mode:              "recommend_only",
		ColdThresholdDays: 21,
		AutoMoveWaitDays:  7,
	}
}

// ValidTierPolicyModes is the set of valid policy modes.
var ValidTierPolicyModes = map[string]bool{
	"recommend_only":       true,
	"auto_if_no_response":  true,
	"never_automatic":      true,
}

// ErrInvalidTierPolicyMode is returned when an invalid mode is provided.
var ErrInvalidTierPolicyMode = fmt.Errorf("invalid tier policy mode")

// ErrInvalidTierPolicyDays is returned when day values are not positive.
var ErrInvalidTierPolicyDays = fmt.Errorf("tier policy days must be positive")

// generateRecommendationID returns a collision-proof recommendation ID.
// Format: rec-<unix-nano>-<8 hex chars from crypto/rand>
func generateRecommendationID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("rec-%d-%08x", now, counter)
	}
	return fmt.Sprintf("rec-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// GetTierPolicy retrieves the tier policy for a pool.
// Returns defaults when no row exists.
// Accepts any querier (sql.DB or sql.Tx).
func GetTierPolicy(q interface{ QueryRow(query string, args ...any) *sql.Row }, poolID string) (TierPolicyRow, error) {
	var p TierPolicyRow
	var mode string
	var coldDays, waitDays int
	err := q.QueryRow(`
		SELECT pool_id, mode, cold_threshold_days, auto_move_wait_days
		FROM tier_policies WHERE pool_id = ?
	`, poolID).Scan(&p.PoolID, &mode, &coldDays, &waitDays)
	if err == sql.ErrNoRows {
		return DefaultTierPolicy(), nil
	}
	if err != nil {
		return TierPolicyRow{}, fmt.Errorf("query tier policy: %w", err)
	}
	p.Mode = mode
	p.ColdThresholdDays = coldDays
	p.AutoMoveWaitDays = waitDays
	return p, nil
}

// SetTierPolicy sets or updates the tier policy for a pool.
// Validates mode and day values (>0).
func SetTierPolicy(db *sql.DB, poolID, mode string, coldDays, waitDays int) error {
	if !ValidTierPolicyModes[mode] {
		return ErrInvalidTierPolicyMode
	}
	if coldDays <= 0 || waitDays <= 0 {
		return ErrInvalidTierPolicyDays
	}
	_, err := db.Exec(`
		INSERT INTO tier_policies (pool_id, mode, cold_threshold_days, auto_move_wait_days)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(pool_id) DO UPDATE SET
			mode = excluded.mode,
			cold_threshold_days = excluded.cold_threshold_days,
			auto_move_wait_days = excluded.auto_move_wait_days
	`, poolID, mode, coldDays, waitDays)
	if err != nil {
		return fmt.Errorf("upsert tier policy: %w", err)
	}
	return nil
}

// GenerateRecommendations scans all collections and creates pending recommendations
// for qualifying candidates. Returns count of recommendations created.
// Idempotent: skips a (collection, direction) pair when ANY recommendation exists
// for it with status pending, approved, or rejected (only superseded rows do not block).
func GenerateRecommendations(db *sql.DB, now time.Time) (int, error) {
	nowUnix := now.Unix()

	// Get all collections with their pool tier and heat data
	rows, err := db.Query(`
		SELECT c.id, c.pool_id, p.tier, c.size_bytes,
		       COALESCE(ch.last_access, 0) as last_access,
		       COALESCE(ch.score, 0) as score
		FROM collections c
		JOIN pools p ON c.pool_id = p.id
		LEFT JOIN collection_heat ch ON c.id = ch.collection_id
	`)
	if err != nil {
		return 0, fmt.Errorf("query collections: %w", err)
	}
	defer rows.Close()

	type collInfo struct {
		id         string
		poolID     string
		tier       string
		sizeBytes  int64
		lastAccess int64
		score      float64
	}
	var collections []collInfo
	for rows.Next() {
		var c collInfo
		if err := rows.Scan(&c.id, &c.poolID, &c.tier, &c.sizeBytes, &c.lastAccess, &c.score); err != nil {
			return 0, fmt.Errorf("scan collection: %w", err)
		}
		collections = append(collections, c)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate collections: %w", err)
	}

	// Get all pinned collections
	pinnedRows, err := db.Query(`SELECT collection_id FROM tier_pins`)
	if err != nil {
		return 0, fmt.Errorf("query pins: %w", err)
	}
	pinned := make(map[string]bool)
	for pinnedRows.Next() {
		var id string
		if err := pinnedRows.Scan(&id); err != nil {
			pinnedRows.Close()
			return 0, fmt.Errorf("scan pin: %w", err)
		}
		pinned[id] = true
	}
	pinnedRows.Close()

	// Get (collection_id, direction) pairs with non-superseded recommendations
	// (pending, approved, rejected block; superseded does not)
	recRows, err := db.Query(`
		SELECT collection_id, direction
		FROM tier_recommendations
		WHERE status IN ('pending', 'approved', 'rejected')
	`)
	if err != nil {
		return 0, fmt.Errorf("query recommendations: %w", err)
	}
	hasRec := make(map[string]bool)
	for recRows.Next() {
		var collID, direction string
		if err := recRows.Scan(&collID, &direction); err != nil {
			recRows.Close()
			return 0, fmt.Errorf("scan recommendation: %w", err)
		}
		hasRec[collID+"|"+direction] = true
	}
	recRows.Close()

	// For cold->warm: need access event counts in last 7 days
	// Build a map of collection_id -> event count in last 7 days
	sevenDaysAgo := nowUnix - 7*24*3600
	eventRows, err := db.Query(`
		SELECT collection_id, COUNT(*)
		FROM access_events
		WHERE accessed_at >= ?
		GROUP BY collection_id
	`, sevenDaysAgo)
	if err != nil {
		return 0, fmt.Errorf("query recent events: %w", err)
	}
	recentEvents := make(map[string]int)
	for eventRows.Next() {
		var collID string
		var count int
		if err := eventRows.Scan(&collID, &count); err != nil {
			eventRows.Close()
			return 0, fmt.Errorf("scan recent event: %w", err)
		}
		recentEvents[collID] = count
	}
	eventRows.Close()

	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	created := 0
	for _, c := range collections {
		// Skip if pool tier is not warm or cold (e.g., hot)
		if c.tier != "warm" && c.tier != "cold" {
			continue
		}
		// Skip pinned
		if pinned[c.id] {
			continue
		}

		// Get policy for this pool
		policy, err := GetTierPolicy(tx, c.poolID)
		if err != nil {
			return 0, fmt.Errorf("get policy for pool %s: %w", c.poolID, err)
		}

		var direction, reason string
		qualifies := false

		if c.tier == "warm" {
			// Warm->Cold candidate
			if c.lastAccess == 0 {
				// Never accessed
				direction = "warm_to_cold"
				reason = "never_accessed"
				qualifies = true
			} else {
				ageDays := (nowUnix - c.lastAccess) / 86400
				if ageDays >= int64(policy.ColdThresholdDays) {
					direction = "warm_to_cold"
					reason = fmt.Sprintf("idle_%dd", ageDays)
					qualifies = true
				}
			}
		} else if c.tier == "cold" {
			// Cold->Warm candidate (heating up)
			eventCount := recentEvents[c.id]
			if eventCount >= 3 {
				direction = "cold_to_warm"
				reason = fmt.Sprintf("heating_%d_in_7d", eventCount)
				qualifies = true
			}
		}

		// Skip if already has a non-superseded recommendation for this (collection, direction)
		if qualifies && hasRec[c.id+"|"+direction] {
			continue
		}

		if qualifies {
			id := generateRecommendationID()
			_, err = tx.Exec(`
				INSERT INTO tier_recommendations (id, collection_id, pool_id, direction, reason, size_bytes, last_access, score, status, created_at, decided_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, 0)
			`, id, c.id, c.poolID, direction, reason, c.sizeBytes, c.lastAccess, c.score, nowUnix)
			if err != nil {
				return 0, fmt.Errorf("insert recommendation for %s: %w", c.id, err)
			}
			created++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit tx: %w", err)
	}

	return created, nil
}

// ListRecommendations returns recommendations filtered by status and/or direction.
// Both filters are optional (empty string = no filter).
func ListRecommendations(db *sql.DB, status, direction string) ([]*TierRecommendationRow, int, error) {
	whereClauses := []string{}
	args := []any{}

	if status != "" {
		whereClauses = append(whereClauses, "status = ?")
		args = append(args, status)
	}
	if direction != "" {
		whereClauses = append(whereClauses, "direction = ?")
		args = append(args, direction)
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	// Count total
	countQuery := "SELECT COUNT(*) FROM tier_recommendations " + whereSQL
	var total int
	err := db.QueryRow(countQuery, args...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count recommendations: %w", err)
	}

	// Query entries (newest first)
	query := fmt.Sprintf("SELECT id, collection_id, pool_id, direction, reason, size_bytes, last_access, score, status, created_at, decided_at FROM tier_recommendations %s ORDER BY created_at DESC, rowid DESC", whereSQL)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query recommendations: %w", err)
	}
	defer rows.Close()

	var recs []*TierRecommendationRow
	for rows.Next() {
		var r TierRecommendationRow
		var createdAt, decidedAt int64
		if err := rows.Scan(&r.ID, &r.CollectionID, &r.PoolID, &r.Direction, &r.Reason, &r.SizeBytes, &r.LastAccess, &r.Score, &r.Status, &createdAt, &decidedAt); err != nil {
			return nil, 0, fmt.Errorf("scan recommendation: %w", err)
		}
		r.CreatedAt = time.Unix(createdAt, 0)
		if decidedAt > 0 {
			r.DecidedAt = time.Unix(decidedAt, 0)
		}
		recs = append(recs, &r)
	}

	if recs == nil {
		recs = []*TierRecommendationRow{}
	}

	return recs, total, nil
}

// DecideRecommendation updates a pending recommendation to approved or rejected.
// Only valid from pending status; returns error otherwise.
func DecideRecommendation(db *sql.DB, id, decision string, now time.Time) error {
	if decision != "approved" && decision != "rejected" {
		return fmt.Errorf("invalid decision: must be approved or rejected")
	}
	nowUnix := now.Unix()

	// Check current status
	var currentStatus string
	err := db.QueryRow(`SELECT status FROM tier_recommendations WHERE id = ?`, id).Scan(&currentStatus)
	if err == sql.ErrNoRows {
		return sql.ErrNoRows
	}
	if err != nil {
		return fmt.Errorf("query recommendation status: %w", err)
	}
	if currentStatus != "pending" {
		return fmt.Errorf("recommendation not pending (current: %s)", currentStatus)
	}

	_, err = db.Exec(`
		UPDATE tier_recommendations
		SET status = ?, decided_at = ?
		WHERE id = ?
	`, decision, nowUnix, id)
	if err != nil {
		return fmt.Errorf("update recommendation: %w", err)
	}
	return nil
}

// PinCollection pins a collection (excludes from recommendations).
// Also supersedes any pending recommendation for that collection.
func PinCollection(db *sql.DB, collectionID string, now time.Time, note string) error {
	nowUnix := now.Unix()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Insert or update pin
	_, err = tx.Exec(`
		INSERT INTO tier_pins (collection_id, pinned_at, note)
		VALUES (?, ?, ?)
		ON CONFLICT(collection_id) DO UPDATE SET
			pinned_at = excluded.pinned_at,
			note = excluded.note
	`, collectionID, nowUnix, note)
	if err != nil {
		return fmt.Errorf("upsert pin: %w", err)
	}

	// Supersede any pending recommendation for this collection
	_, err = tx.Exec(`
		UPDATE tier_recommendations
		SET status = 'superseded', decided_at = ?
		WHERE collection_id = ? AND status = 'pending'
	`, nowUnix, collectionID)
	if err != nil {
		return fmt.Errorf("supersede pending recommendations: %w", err)
	}

	return tx.Commit()
}

// UnpinCollection removes a pin from a collection.
func UnpinCollection(db *sql.DB, collectionID string) error {
	res, err := db.Exec(`DELETE FROM tier_pins WHERE collection_id = ?`, collectionID)
	if err != nil {
		return fmt.Errorf("delete pin: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}
