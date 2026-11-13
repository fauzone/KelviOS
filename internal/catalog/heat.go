package catalog

import (
	"database/sql"
	"fmt"
	"time"
)

// CollectionHeat represents the computed heat score for a collection.
type CollectionHeat struct {
	CollectionID string    `json:"collection_id"`
	Score        float64   `json:"score"`
	EventCount   int       `json:"event_count"`
	LastAccess   int64     `json:"last_access"`
	SizeBytes    int64     `json:"size_bytes"`
	ComputedAt   time.Time `json:"computed_at"`
}

// weightForKind returns the weight for a given access event kind.
// content=2, share=2, preview=1, unknown=1. write=3 is reserved for future.
func weightForKind(kind string) float64 {
	switch kind {
	case "content":
		return 2.0
	case "share":
		return 2.0
	case "preview":
		return 1.0
	default:
		return 1.0
	}
}

// ComputeHeatScores computes heat scores for ALL collections (including zero-event ones)
// and upserts into collection_heat. Returns the number of collections written.
// Pure catalog-DB work: no imports of internal/power or internal/storage (no-wake by construction).
func ComputeHeatScores(db *sql.DB, now time.Time) (int, error) {
	nowUnix := now.Unix()

	// Get all collections
	rows, err := db.Query(`SELECT id FROM collections`)
	if err != nil {
		return 0, fmt.Errorf("query collections: %w", err)
	}
	defer rows.Close()

	var collectionIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scan collection id: %w", err)
		}
		collectionIDs = append(collectionIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate collections: %w", err)
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	written := 0
	for _, collID := range collectionIDs {
		// Compute score from access_events
		var score float64
		var eventCount int
		var lastAccess sql.NullInt64

		err := tx.QueryRow(`
			SELECT
				COALESCE(SUM(
					CASE kind
						WHEN 'content' THEN 2.0
						WHEN 'share' THEN 2.0
						WHEN 'preview' THEN 1.0
						ELSE 1.0
					END * POWER(0.5, ((? - accessed_at) / 86400.0) / 7.0)
				), 0),
				COUNT(*),
				MAX(accessed_at)
			FROM access_events
			WHERE collection_id = ?
		`, nowUnix, collID).Scan(&score, &eventCount, &lastAccess)
		if err != nil {
			return 0, fmt.Errorf("query heat for collection %s: %w", collID, err)
		}

		lastAccessUnix := int64(0)
		if lastAccess.Valid {
			lastAccessUnix = lastAccess.Int64
		}

		// Compute size_bytes = SUM(files.size_bytes) for the collection
		var sizeBytes sql.NullInt64
		err = tx.QueryRow(`
			SELECT COALESCE(SUM(size_bytes), 0)
			FROM files
			WHERE collection_id = ?
		`, collID).Scan(&sizeBytes)
		if err != nil {
			return 0, fmt.Errorf("query size for collection %s: %w", collID, err)
		}
		sizeBytesVal := int64(0)
		if sizeBytes.Valid {
			sizeBytesVal = sizeBytes.Int64
		}

		// Upsert into collection_heat
		_, err = tx.Exec(`
			INSERT INTO collection_heat (collection_id, score, event_count, last_access, size_bytes, computed_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(collection_id) DO UPDATE SET
				score = excluded.score,
				event_count = excluded.event_count,
				last_access = excluded.last_access,
				size_bytes = excluded.size_bytes,
				computed_at = excluded.computed_at
		`, collID, score, eventCount, lastAccessUnix, sizeBytesVal, nowUnix)
		if err != nil {
			return 0, fmt.Errorf("upsert collection_heat for %s: %w", collID, err)
		}
		written++
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit tx: %w", err)
	}

	return written, nil
}

// GetCollectionHeat retrieves the stored heat row for a collection.
// Returns nil when the collection does not exist (not when heat row is missing).
func GetCollectionHeat(db *sql.DB, collectionID string) (*CollectionHeat, error) {
	// First verify collection exists
	var exists int
	err := db.QueryRow(`SELECT 1 FROM collections WHERE id = ?`, collectionID).Scan(&exists)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("check collection exists: %w", err)
	}

	var ch CollectionHeat
	var computedAt int64
	err = db.QueryRow(`
		SELECT collection_id, score, event_count, last_access, size_bytes, computed_at
		FROM collection_heat
		WHERE collection_id = ?
	`, collectionID).Scan(&ch.CollectionID, &ch.Score, &ch.EventCount, &ch.LastAccess, &ch.SizeBytes, &computedAt)
	if err == sql.ErrNoRows {
		// Collection exists but no heat row yet — return zero score with computed_at 0
		ch.CollectionID = collectionID
		ch.Score = 0
		ch.EventCount = 0
		ch.LastAccess = 0
		ch.SizeBytes = 0
		ch.ComputedAt = time.Unix(0, 0)
		return &ch, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query collection_heat: %w", err)
	}
	ch.ComputedAt = time.Unix(computedAt, 0)
	return &ch, nil
}
