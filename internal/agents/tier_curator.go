package agents

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type tierCuratorAgent struct{}

func (tierCuratorAgent) Name() string       { return "tier_curator" }
func (tierCuratorAgent) Describe() string   { return "Collection heat summary, pending curator recommendations, and pins" }

func (tierCuratorAgent) Run(ctx context.Context, deps *Deps) (Report, error) {
	now := deps.Now()
	report := Report{
		Agent:       "tier_curator",
		GeneratedAt: now,
		Findings:    make([]string, 0),
		Actions:     make([]string, 0),
	}

	// Get top heated collections (by heat score)
	rows, err := deps.DB.Query(`
		SELECT c.id, c.name, c.pool_id, ch.score, ch.event_count, ch.last_access, ch.size_bytes
		FROM collection_heat ch
		JOIN collections c ON c.id = ch.collection_id
		WHERE ch.score > 0
		ORDER BY ch.score DESC
		LIMIT 10
	`)
	if err != nil {
		report.Summary = "Failed to query collection heat"
		report.Findings = append(report.Findings, fmt.Sprintf("heat query: %v", err))
		return report, nil
	}
	defer rows.Close()

	type heatedColl struct {
		id         string
		name       string
		poolID     string
		score      float64
		eventCount int
		lastAccess int64
		sizeBytes  int64
	}
	var heated []heatedColl
	for rows.Next() {
		var h heatedColl
		if err := rows.Scan(&h.id, &h.name, &h.poolID, &h.score, &h.eventCount, &h.lastAccess, &h.sizeBytes); err != nil {
			continue
		}
		heated = append(heated, h)
	}

	// Get pending recommendations count
	var pendingRecs int
	err = deps.DB.QueryRow(`
		SELECT COUNT(*) FROM tier_recommendations WHERE status = 'pending'
	`).Scan(&pendingRecs)
	if err != nil {
		pendingRecs = -1
	}

	// Get pins count
	var pinsCount int
	err = deps.DB.QueryRow(`
		SELECT COUNT(*) FROM tier_pins
	`).Scan(&pinsCount)
	if err != nil {
		pinsCount = -1
	}

	// Get recommendations by direction
	var warmToCold, coldToWarm int
	err = deps.DB.QueryRow(`
		SELECT COUNT(*) FROM tier_recommendations WHERE status = 'pending' AND direction = 'warm_to_cold'
	`).Scan(&warmToCold)
	if err != nil {
		warmToCold = -1
	}
	err = deps.DB.QueryRow(`
		SELECT COUNT(*) FROM tier_recommendations WHERE status = 'pending' AND direction = 'cold_to_warm'
	`).Scan(&coldToWarm)
	if err != nil {
		coldToWarm = -1
	}

	// Build summary
	var summaryParts []string
	if len(heated) > 0 {
		summaryParts = append(summaryParts, fmt.Sprintf("top heated: %s (score %.1f, %d events)", heated[0].name, heated[0].score, heated[0].eventCount))
	} else {
		summaryParts = append(summaryParts, "no heated collections (no access events recorded)")
	}
	if pendingRecs >= 0 {
		summaryParts = append(summaryParts, fmt.Sprintf("%d pending recommendation(s) (%d warm→cold, %d cold→warm)", pendingRecs, warmToCold, coldToWarm))
	}
	if pinsCount >= 0 {
		summaryParts = append(summaryParts, fmt.Sprintf("%d pinned collection(s)", pinsCount))
	}
	report.Summary = strings.Join(summaryParts, "; ")

	// Findings
	if len(heated) > 0 {
		for i, h := range heated {
			if i >= 5 {
				report.Findings = append(report.Findings, fmt.Sprintf("... and %d more heated collections", len(heated)-5))
				break
			}
			lastAccessStr := "never"
			if h.lastAccess > 0 {
				lastAccessStr = time.Unix(h.lastAccess, 0).Format("2006-01-02")
			}
			report.Findings = append(report.Findings,
				fmt.Sprintf("#%d %s (pool %s): score=%.1f, events=%d, last_access=%s, size=%s",
					i+1, h.name, h.poolID, h.score, h.eventCount, lastAccessStr, formatBytes(h.sizeBytes)))
		}
	}
	if pendingRecs > 0 {
		// List pending recommendations
		recRows, err := deps.DB.Query(`
			SELECT tr.id, c.name, tr.direction, tr.reason, tr.size_bytes, tr.created_at
			FROM tier_recommendations tr
			JOIN collections c ON c.id = tr.collection_id
			WHERE tr.status = 'pending'
			ORDER BY tr.created_at DESC
			LIMIT 10
		`)
		if err == nil {
			defer recRows.Close()
			for recRows.Next() {
				var id, collName, direction, reason string
				var sizeBytes int64
				var createdAt int64
				if err := recRows.Scan(&id, &collName, &direction, &reason, &sizeBytes, &createdAt); err != nil {
					continue
				}
				report.Findings = append(report.Findings,
					fmt.Sprintf("pending: %s → %s (%s, %s, created %s)",
						collName, direction, reason, formatBytes(sizeBytes), time.Unix(createdAt, 0).Format("2006-01-02")))
			}
		}
	}
	if pinsCount > 0 {
		pinRows, err := deps.DB.Query(`
			SELECT c.name, tp.pinned_at, tp.note
			FROM tier_pins tp
			JOIN collections c ON c.id = tp.collection_id
			ORDER BY tp.pinned_at DESC
		`)
		if err == nil {
			defer pinRows.Close()
			for pinRows.Next() {
				var name string
				var pinnedAt int64
				var note string
				if err := pinRows.Scan(&name, &pinnedAt, &note); err != nil {
					continue
				}
				noteStr := ""
				if note != "" {
					noteStr = fmt.Sprintf(" — %s", note)
				}
				report.Findings = append(report.Findings,
					fmt.Sprintf("pinned: %s (since %s%s)", name, time.Unix(pinnedAt, 0).Format("2006-01-02"), noteStr))
			}
		}
	}

	// Actions
	if len(heated) == 0 {
		report.Actions = append(report.Actions, "No heat data — ensure collections are being accessed and access events are recorded")
	}
	if pendingRecs > 0 {
		report.Actions = append(report.Actions, fmt.Sprintf("Review %d pending tier recommendation(s) via GET /api/curator/recommendations", pendingRecs))
	}
	if pinsCount > 0 && pendingRecs == 0 {
		report.Actions = append(report.Actions, "Collections are pinned; no pending recommendations to review")
	}
	if len(report.Actions) == 0 {
		report.Actions = append(report.Actions, "No pending recommendations; curator is up to date")
	}

	return report, nil
}

func init() {
	Register(tierCuratorAgent{})
}
