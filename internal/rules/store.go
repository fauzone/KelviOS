// Package rules implements the FR-3.11 Event→Action Rules Engine.
// SQLStore provides database persistence for rules and runs.
// NOTE: Store interface and SQLStore implementation are in catalog.go to avoid duplication.
package rules

import (
	"database/sql"
	"fmt"

	"github.com/fauzone/kelvios/internal/catalog"
)

// ============================================================================
// MIGRATION HELPER (called from catalog migration block)
// ============================================================================

// MigrateSchema creates the event_rules and event_rule_runs tables.
func MigrateSchema(db *sql.DB) error {
	// event_rules table
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS event_rules (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			event_type TEXT NOT NULL,
			event_params TEXT NOT NULL DEFAULT '{}',
			actions TEXT NOT NULL DEFAULT '[]',
			enabled INTEGER NOT NULL DEFAULT 1,
			cooldown_seconds INTEGER NOT NULL DEFAULT 3600,
			max_fires_per_day INTEGER NOT NULL DEFAULT 10,
			state TEXT NOT NULL DEFAULT '{}',
			last_fired_at INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("create event_rules: %w", err)
	}

	// event_rule_runs table
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS event_rule_runs (
			id TEXT PRIMARY KEY,
			rule_id TEXT NOT NULL,
			event_ref TEXT NOT NULL,
			fired_at INTEGER NOT NULL,
			result TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			alert INTEGER NOT NULL DEFAULT 0,
			test_fire INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY (rule_id) REFERENCES event_rules(id) ON DELETE CASCADE,
			UNIQUE (rule_id, event_ref)
		)
	`)
	if err != nil {
		return fmt.Errorf("create event_rule_runs: %w", err)
	}

	// Indexes
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_event_rules_enabled ON event_rules(enabled)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_event_rule_runs_rule_id ON event_rule_runs(rule_id)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS idx_event_rule_runs_fired_at ON event_rule_runs(fired_at)`)
	if err != nil {
		return err
	}

	return nil
}

// ============================================================================
// CATALOG INTEGRATION (for audit_log, restore_test_alerts, ups_events, compose_runs)
// ============================================================================

// These functions are thin wrappers around catalog functions to avoid import cycles
// and provide the specific queries the engine needs.

// ListPools wraps catalog.ListPools
func ListPools(db *sql.DB) ([]*catalog.PoolRow, error) {
	return catalog.ListPools(db)
}

// WriteAuditLog wraps catalog.WriteAuditLog
func WriteAuditLog(db *sql.DB, actor, action, targetType, targetID, result, detail string) {
	catalog.WriteAuditLog(db, actor, action, targetType, targetID, result, detail)
}