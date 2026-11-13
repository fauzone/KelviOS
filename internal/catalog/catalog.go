package catalog

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fauzone/kelvios/internal/supplychain"
	_ "modernc.org/sqlite"
)

const schemaVersion = 41

// ErrPoolNameExists is returned when a pool create violates the unique name constraint.
var ErrPoolNameExists = fmt.Errorf("pool_name_exists")

func Open(path string) (*sql.DB, error) {
	if path != ":memory:" && path != "" {
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create catalog directory: %w", err)
			}
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// Serialize all catalog access through a single connection.
	// This eliminates transaction collision races (SQLITE_BUSY, "cannot start a transaction within a transaction")
	// under concurrent writers. sql.DB is safe for concurrent use by multiple goroutines.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	// Enable WAL mode for file databases (not applicable for :memory:)
	if path != ":memory:" && path != "" {
		if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
			return nil, fmt.Errorf("enable WAL: %w", err)
		}
	}

	// Set busy_timeout to 5000ms for all databases
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}

	// Enable foreign key constraints for cascade deletes
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	// Verify foreign_keys is ON
	var fk int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		return nil, fmt.Errorf("verify foreign_keys: %w", err)
	}
	if fk != 1 {
		return nil, fmt.Errorf("foreign_keys pragma not enabled (got %d)", fk)
	}

	// Verify journal_mode is WAL for file databases
	if path != ":memory:" && path != "" {
		var jm string
		if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&jm); err != nil {
			return nil, fmt.Errorf("verify journal_mode: %w", err)
		}
		if !strings.EqualFold(jm, "wal") {
			return nil, fmt.Errorf("journal_mode not WAL (got %s)", jm)
		}
	}

	return db, nil
}

func Migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		CREATE TABLE IF NOT EXISTS meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`)
	if err != nil {
		return fmt.Errorf("create meta table: %w", err)
	}

	var currentVersion int
	err = tx.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&currentVersion)
	if err == sql.ErrNoRows {
		currentVersion = 0
	} else if err != nil {
		return fmt.Errorf("query schema_version: %w", err)
	}

	if currentVersion < 1 {
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS devices (
				id           TEXT PRIMARY KEY,
				name         TEXT NOT NULL,
				type         TEXT NOT NULL,
				path         TEXT NOT NULL,
				size_bytes   INTEGER NOT NULL,
				filesystem   TEXT,
				mountpoint   TEXT,
				created_at   INTEGER NOT NULL,
				updated_at   INTEGER NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create devices table: %w", err)
		}

		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS volumes (
				id           TEXT PRIMARY KEY,
				name         TEXT NOT NULL,
				device_ids   TEXT NOT NULL,
				raid_level   TEXT,
				size_bytes   INTEGER NOT NULL,
				filesystem   TEXT,
				mountpoint   TEXT,
				created_at   INTEGER NOT NULL,
				updated_at   INTEGER NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create volumes table: %w", err)
		}

		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS shares (
				id           TEXT PRIMARY KEY,
				name         TEXT NOT NULL,
				volume_id    TEXT NOT NULL,
				path         TEXT NOT NULL,
				protocol     TEXT NOT NULL,
				options      TEXT,
				created_at   INTEGER NOT NULL,
				updated_at   INTEGER NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create shares table: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, 1)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 2 {
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS pools (
				id           TEXT PRIMARY KEY,
				name         TEXT NOT NULL,
				node_id      TEXT NOT NULL,
				tier         TEXT NOT NULL,
				mechanism    TEXT NOT NULL,
				driver       TEXT NOT NULL,
				mountpoint   TEXT,
				size_bytes   INTEGER NOT NULL,
				used_bytes   INTEGER NOT NULL DEFAULT 0,
				encrypted    INTEGER NOT NULL DEFAULT 0,
				status       TEXT NOT NULL DEFAULT 'active',
				created_at   INTEGER NOT NULL,
				updated_at   INTEGER NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create pools table: %w", err)
		}

		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS pool_disks (
				pool_id      TEXT NOT NULL,
				disk_name    TEXT NOT NULL,
				created_at   INTEGER NOT NULL,
				PRIMARY KEY (pool_id, disk_name),
				FOREIGN KEY (pool_id) REFERENCES pools(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create pool_disks table: %w", err)
		}

		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS collections (
				id                TEXT PRIMARY KEY,
				name              TEXT NOT NULL,
				pool_id           TEXT NOT NULL,
				path              TEXT NOT NULL,
				disk_name         TEXT,
				size_bytes        INTEGER NOT NULL DEFAULT 0,
				last_accessed_at  INTEGER,
				created_at        INTEGER NOT NULL,
				updated_at        INTEGER NOT NULL,
				FOREIGN KEY (pool_id) REFERENCES pools(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create collections table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_collections_pool ON collections(pool_id)`)
		if err != nil {
			return fmt.Errorf("create collections pool index: %w", err)
		}

		// Shares table (for collections) - added in schema version 3
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS collection_shares (
				id              TEXT PRIMARY KEY,
				collection_id   TEXT NOT NULL,
				name            TEXT NOT NULL,
				protocol        TEXT NOT NULL,
				read_only       INTEGER NOT NULL DEFAULT 0,
				metadata        TEXT,
				created_at      INTEGER NOT NULL,
				updated_at      INTEGER NOT NULL,
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create collection_shares table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_collection_shares_collection ON collection_shares(collection_id)`)
		if err != nil {
			return fmt.Errorf("create collection_shares collection index: %w", err)
		}

		_, err = tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_collection_shares_name_unique ON collection_shares(collection_id, name)`)
		if err != nil {
			return fmt.Errorf("create collection_shares unique name index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 4 {
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS audit_log (
				id          TEXT PRIMARY KEY,
				created_at  INTEGER NOT NULL,
				actor       TEXT NOT NULL,
				action      TEXT NOT NULL,
				target_type TEXT NOT NULL,
				target_id   TEXT NOT NULL,
				result      TEXT NOT NULL,
				detail      TEXT
			);
		`)
		if err != nil {
			return fmt.Errorf("create audit_log table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_audit_log_created_at ON audit_log(created_at DESC)`)
		if err != nil {
			return fmt.Errorf("create audit_log created_at index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_audit_log_action ON audit_log(action)`)
		if err != nil {
			return fmt.Errorf("create audit_log action index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_audit_log_target ON audit_log(target_type, target_id)`)
		if err != nil {
			return fmt.Errorf("create audit_log target index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 6 {
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS accounts (
				id            TEXT PRIMARY KEY,
				username      TEXT NOT NULL UNIQUE,
				password_hash TEXT NOT NULL,
				failed_count  INTEGER NOT NULL DEFAULT 0,
				locked_until  INTEGER NOT NULL DEFAULT 0,
				created_at    INTEGER NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create accounts table: %w", err)
		}

		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS sessions (
				token_hash TEXT PRIMARY KEY,
				username   TEXT NOT NULL,
				created_at INTEGER NOT NULL,
				expires_at INTEGER NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create sessions table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_username ON sessions(username)`)
		if err != nil {
			return fmt.Errorf("create sessions username index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 7 {
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS vault_config (
				id         TEXT PRIMARY KEY,
				salt       BLOB NOT NULL,
				verifier   BLOB NOT NULL,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create vault_config table: %w", err)
		}

		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS secrets (
				name       TEXT PRIMARY KEY,
				ciphertext BLOB NOT NULL,
				nonce      BLOB NOT NULL,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create secrets table: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 8 {
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS files (
				id              TEXT PRIMARY KEY,
				collection_id   TEXT NOT NULL,
				rel_path        TEXT NOT NULL,
				size_bytes      INTEGER NOT NULL,
				modtime_unix    INTEGER NOT NULL,
				disk_name       TEXT NOT NULL DEFAULT '',
				created_at      INTEGER NOT NULL,
				updated_at      INTEGER NOT NULL,
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create files table: %w", err)
		}

		_, err = tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_files_collection_relpath ON files(collection_id, rel_path)`)
		if err != nil {
			return fmt.Errorf("create files collection_relpath index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_files_collection ON files(collection_id)`)
		if err != nil {
			return fmt.Errorf("create files collection index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 9 {
		// WS15 fix (Chief takeover, 2026-10-29): recorded directories.
		// BrowseFiles derives directories from file paths, so EMPTY
		// directories were invisible in browse even though the WS15 brief
		// requires mkdir results to show up. The dirs table records
		// directories created via the API; browse merges them in without
		// touching disk and without changing the browse response contract.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS dirs (
				id              TEXT PRIMARY KEY,
				collection_id   TEXT NOT NULL,
				rel_path        TEXT NOT NULL,
				created_at      INTEGER NOT NULL,
				updated_at      INTEGER NOT NULL,
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create dirs table: %w", err)
		}

		_, err = tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_dirs_collection_relpath ON dirs(collection_id, rel_path)`)
		if err != nil {
			return fmt.Errorf("create dirs collection_relpath index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_dirs_collection ON dirs(collection_id)`)
		if err != nil {
			return fmt.Errorf("create dirs collection index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 10 {
		// WS16 LANGKAH-A (2026-10-30): share links with expiry & password (FR-1.13).
		// Catalog-only: creating a link does NOT wake the pool. Download via
		// public endpoint checks tier and returns 409 wake_required for cold.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS share_links (
				id              TEXT PRIMARY KEY,
				collection_id   TEXT NOT NULL,
				rel_path        TEXT NOT NULL,
				token_hash      TEXT NOT NULL UNIQUE,
				password_hash   TEXT,
				expires_at      INTEGER,
				created_by      TEXT NOT NULL,
				created_at      INTEGER NOT NULL,
				revoked_at      INTEGER,
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create share_links table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_sharelinks_collection ON share_links(collection_id)`)
		if err != nil {
			return fmt.Errorf("create sharelinks collection index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_sharelinks_token ON share_links(token_hash)`)
		if err != nil {
			return fmt.Errorf("create sharelinks token index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 11 {
		// WS18 LANGKAH-B (2026-10-31): supply-chain entries persistence (FR-1.14).
		// Catalog-only: entries are validated via supplychain.ValidateEntry before
		// persistence; verify endpoint uses supplychain.VerifyInstall decision core.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS supplychain_entries (
				id                  TEXT PRIMARY KEY,
				name                TEXT NOT NULL UNIQUE,
				publisher           TEXT NOT NULL,
				source_repo         TEXT NOT NULL,
				image_ref           TEXT NOT NULL,
				pinned_digest       TEXT NOT NULL,
				signature_kind      TEXT NOT NULL,
				source_kind         TEXT NOT NULL,
				unverified_confirmed INTEGER NOT NULL DEFAULT 0,
				created_at          INTEGER NOT NULL,
				updated_at          INTEGER NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create supplychain_entries table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_supplychain_entries_name ON supplychain_entries(name)`)
		if err != nil {
			return fmt.Errorf("create supplychain_entries name index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 12 {
		// WS19 (2026-11-01): UPS basics core (FR-1.12).
		// Catalog-only: ups_events table for persisted UPS event log.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS ups_events (
				id              TEXT PRIMARY KEY,
				ts              INTEGER NOT NULL,
				kind            TEXT NOT NULL,
				state           TEXT NOT NULL,
				charge_percent  INTEGER NOT NULL,
				detail          TEXT NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create ups_events table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_ups_events_ts ON ups_events(ts DESC)`)
		if err != nil {
			return fmt.Errorf("create ups_events ts index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 13 {
		// WS20 LANGKAH-B (2026-11-02): Import wiring (FR-1.10).
		// Catalog-only: imports table for persisted import completion reports.
		_, err = tx.Exec(`
				CREATE TABLE IF NOT EXISTS imports (
					id              TEXT PRIMARY KEY,
					collection_id   TEXT NOT NULL,
					source_path     TEXT NOT NULL,
					fs_type         TEXT NOT NULL,
					total_files     INTEGER NOT NULL,
					total_bytes     INTEGER NOT NULL,
					copied          INTEGER NOT NULL,
					failed          INTEGER NOT NULL,
					bytes_copied    INTEGER NOT NULL,
					ts              INTEGER NOT NULL
				);
			`)
		if err != nil {
			return fmt.Errorf("create imports table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_imports_collection ON imports(collection_id)`)
		if err != nil {
			return fmt.Errorf("create imports collection index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_imports_ts ON imports(ts DESC)`)
		if err != nil {
			return fmt.Errorf("create imports ts index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 14 {
		// WS21 LANGKAH-B (2026-10-30): Disk operations catalog (FR-1.11).
		// Persists replacement/expansion plans with status for approval workflow.
		_, err = tx.Exec(`
				CREATE TABLE IF NOT EXISTS disk_operations (
					id                  TEXT PRIMARY KEY,
					pool_id             TEXT NOT NULL,
					kind                TEXT NOT NULL, -- 'replace' | 'expand'
					strategy            TEXT NOT NULL,
					failing_disk        TEXT,
					replacement_disk    TEXT,
					status              TEXT NOT NULL DEFAULT 'planned', -- 'planned' | 'approved' | 'done' | 'failed'
					detail              TEXT, -- JSON summary (counts/strategy only, no disk names/paths)
					created_at          INTEGER NOT NULL,
					FOREIGN KEY (pool_id) REFERENCES pools(id) ON DELETE CASCADE
				);
			`)
		if err != nil {
			return fmt.Errorf("create disk_operations table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_diskops_pool ON disk_operations(pool_id)`)
		if err != nil {
			return fmt.Errorf("create diskops pool index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_diskops_created ON disk_operations(created_at DESC)`)
		if err != nil {
			return fmt.Errorf("create diskops created_at index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 15 {
		// WS22 STEP A (2026-10-27): Per-collection recycle bin (FR-2.10).
		// Files deleted via File Manager move to a recycle area on the SAME pool
		// instead of being erased; retention default 30 days (configurable).
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS trash_items (
				id                  TEXT PRIMARY KEY,
				collection_id       TEXT NOT NULL,
				original_rel_path   TEXT NOT NULL,
				trash_rel_path      TEXT NOT NULL,
				kind                TEXT NOT NULL, -- 'file' only in Step A
				size_bytes          INTEGER NOT NULL,
				disk_name           TEXT NOT NULL DEFAULT '',
				deleted_at          INTEGER NOT NULL,
				retention_days      INTEGER NOT NULL DEFAULT 30,
				actor               TEXT NOT NULL DEFAULT '',
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create trash_items table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_trash_collection_deleted ON trash_items(collection_id, deleted_at DESC)`)
		if err != nil {
			return fmt.Errorf("create trash collection_deleted index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 16 {
		// WS25 LANGKAH-B (2026-10-30): Pool encryption mode (FR-1.1/1.2).
		// Adds encryption_mode column to pools table. Empty string = "none".
		// Existing pools get default '' (none) and remain valid.
		// Chief fix (WS25 takeover, 2026-11-01): guard the ALTER via
		// PRAGMA table_info — a bare ALTER fails on schemas whose pools
		// table is absent (minimal hand-built migration fixtures) and on
		// a retried migration where the column already exists.
		rows, err := tx.Query(`PRAGMA table_info(pools)`)
		if err != nil {
			return fmt.Errorf("inspect pools table: %w", err)
		}
		columnCount := 0
		hasEncryptionMode := false
		for rows.Next() {
			var cid int
			var colName, colType string
			var notNull int
			var defaultValue any
			var pk int
			if err := rows.Scan(&cid, &colName, &colType, &notNull, &defaultValue, &pk); err != nil {
				rows.Close()
				return fmt.Errorf("scan pools column: %w", err)
			}
			columnCount++
			if colName == "encryption_mode" {
				hasEncryptionMode = true
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("inspect pools columns: %w", err)
		}
		if columnCount > 0 && !hasEncryptionMode {
			_, err = tx.Exec(`ALTER TABLE pools ADD COLUMN encryption_mode TEXT NOT NULL DEFAULT ''`)
			if err != nil {
				return fmt.Errorf("add encryption_mode column: %w", err)
			}
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 17 {
		// WS27 (2026-10-31): Heat catalog from access log (FR-1.3).
		// access_events table records successful file accesses for heat computation.
		// Kind values: "preview", "content", "share" (public share access).
		// Actor for public share: "share:<token-prefix-8>" (never full token).
		// Idempotent guard: skip if table already exists.
		var tableExists int
		err = tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='access_events'`).Scan(&tableExists)
		if err != nil {
			return fmt.Errorf("check access_events table: %w", err)
		}
		if tableExists == 0 {
			_, err = tx.Exec(`
				CREATE TABLE access_events (
					id              TEXT PRIMARY KEY,
					collection_id   TEXT NOT NULL,
					rel_path        TEXT NOT NULL,
					actor           TEXT NOT NULL,
					accessed_at     INTEGER NOT NULL,
					kind            TEXT NOT NULL,
					FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE
				);
			`)
			if err != nil {
				return fmt.Errorf("create access_events table: %w", err)
			}

			_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_access_events_collection_path_time ON access_events(collection_id, rel_path, accessed_at DESC)`)
			if err != nil {
				return fmt.Errorf("create access_events collection_path_time index: %w", err)
			}

			_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_access_events_collection_time ON access_events(collection_id, accessed_at DESC)`)
			if err != nil {
				return fmt.Errorf("create access_events collection_time index: %w", err)
			}
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 18 {
		// WS29 (2026-11-01): Pool name uniqueness (Kapten decision).
		// Create UNIQUE index on pools(name) with COLLATE NOCASE for case-insensitive uniqueness.
		// Pre-existing duplicates: rename later-created ones by appending -dup-<id-suffix>
		// so the index creation cannot fail. Write audit row per rename (action pool.rename_dedup,
		// actor system) if audit table exists at this stage; otherwise log via slog.
		// Never fail migration silently; never drop a pool.

		// First, check if the pools table exists (guard for minimal schemas / test fixtures)
		var poolsTableExists int
		err = tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='pools'`).Scan(&poolsTableExists)
		if err != nil {
			return fmt.Errorf("check pools table: %w", err)
		}
		if poolsTableExists == 0 {
			// Pools table doesn't exist yet (e.g., migrating from v14 or earlier test fixture)
			// Skip the uniqueness migration; the index will be created when pools table is created later
			_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
			if err != nil {
				return fmt.Errorf("set schema_version: %w", err)
			}
			return tx.Commit()
		}

		// First, check if the unique index already exists (idempotent guard)
		var indexExists int
		err = tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_pools_name_unique'`).Scan(&indexExists)
		if err != nil {
			return fmt.Errorf("check idx_pools_name_unique: %w", err)
		}

		if indexExists == 0 {
			// Detect duplicate names (case-insensitive) before creating the index
			// We need to find all pools with duplicate names (NOCASE)
			rows, err := tx.Query(`
				SELECT id, name FROM pools
				WHERE name IN (
					SELECT name FROM pools
					GROUP BY LOWER(name)
					HAVING COUNT(*) > 1
				)
				ORDER BY LOWER(name), created_at
			`)
			if err != nil {
				return fmt.Errorf("query duplicate pool names: %w", err)
			}

			type dupPool struct {
				id   string
				name string
			}
			var duplicates []dupPool
			for rows.Next() {
				var d dupPool
				if err := rows.Scan(&d.id, &d.name); err != nil {
					rows.Close()
					return fmt.Errorf("scan duplicate pool: %w", err)
				}
				duplicates = append(duplicates, d)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("iterate duplicate pools: %w", err)
			}

			// Group by lowercase name and rename later entries
			nameGroups := make(map[string][]dupPool)
			for _, d := range duplicates {
				key := strings.ToLower(d.name)
				nameGroups[key] = append(nameGroups[key], d)
			}

			// Check if audit_log table exists at this migration stage
			var auditTableExists int
			err = tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='audit_log'`).Scan(&auditTableExists)
			if err != nil {
				return fmt.Errorf("check audit_log table: %w", err)
			}
			hasAuditTable := auditTableExists > 0

			for _, group := range nameGroups {
				// Keep the first (earliest created_at), rename the rest
				for i := 1; i < len(group); i++ {
					oldName := group[i].name
					// Use last 8 chars of pool ID as suffix for determinism
					suffix := group[i].id
					if len(suffix) > 8 {
						suffix = suffix[len(suffix)-8:]
					}
					newName := oldName + "-dup-" + suffix

					_, err = tx.Exec(`UPDATE pools SET name = ?, updated_at = ? WHERE id = ?`, newName, time.Now().Unix(), group[i].id)
					if err != nil {
						return fmt.Errorf("rename duplicate pool %s: %w", group[i].id, err)
					}

					// Write audit log if table exists, otherwise log via slog
					if hasAuditTable {
						_, err = tx.Exec(`
							INSERT INTO audit_log (id, created_at, actor, action, target_type, target_id, result, detail)
							VALUES (?, ?, ?, ?, ?, ?, ?, ?)
						`, generateAuditID(), time.Now().Unix(), "system", "pool.rename_dedup", "pool", group[i].id, "ok", fmt.Sprintf("renamed from %s to %s for uniqueness", oldName, newName))
						if err != nil {
							return fmt.Errorf("write audit log for pool rename: %w", err)
						}
					} else {
						slog.Warn("pool renamed for uniqueness during migration", "pool_id", group[i].id, "old_name", oldName, "new_name", newName)
					}
				}
			}

			// Now create the unique index
			_, err = tx.Exec(`CREATE UNIQUE INDEX idx_pools_name_unique ON pools(name COLLATE NOCASE)`)
			if err != nil {
				return fmt.Errorf("create unique index on pools(name): %w", err)
			}
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 19 {
		// WS30 (2026-11-01): Scheduled keep-awake windows (FR-1.7).
		// power_schedules table stores recurring windows per disk.
		// days: comma-separated weekday names (Mon,Tue,Wed,Thu,Fri,Sat,Sun) - at least one required.
		// start_min/end_min: minutes since midnight (0-1439), start != end (cross-midnight allowed).
		// enabled: 1 = active, 0 = disabled.
		// Unique constraint on (disk, days, start_min, end_min) prevents duplicates.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS power_schedules (
				id           TEXT PRIMARY KEY,
				disk         TEXT NOT NULL,
				days         TEXT NOT NULL,
				start_min    INTEGER NOT NULL,
				end_min      INTEGER NOT NULL,
				enabled      INTEGER NOT NULL DEFAULT 1,
				created_at   INTEGER NOT NULL,
				updated_at   INTEGER NOT NULL,
				UNIQUE(disk, days, start_min, end_min)
			);
		`)
		if err != nil {
			return fmt.Errorf("create power_schedules table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_power_schedules_disk ON power_schedules(disk)`)
		if err != nil {
			return fmt.Errorf("create power_schedules disk index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 20 {
		// WS32 (2026-11-02): Heat scoring per collection (FR-2.1).
		// collection_heat stores the computed heat score per collection.
		// score: Σ weight(kind) × 0.5^(ageDays/7), half-life 7 days.
		// event_count: number of access_events for this collection.
		// last_access: max(accessed_at) from access_events, 0 when none.
		// size_bytes: SUM(files.size_bytes) for the collection.
		// computed_at: unix timestamp when this row was computed.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS collection_heat (
				collection_id   TEXT PRIMARY KEY,
				score           REAL NOT NULL,
				event_count     INTEGER NOT NULL,
				last_access     INTEGER NOT NULL,
				size_bytes      INTEGER NOT NULL,
				computed_at     INTEGER NOT NULL,
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create collection_heat table: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 21 {
		// WS33 (2026-11-03): Tier Curator recommendations (FR-2.2).
		// tier_recommendations: migration recommendations as approval cards.
		// tier_pins: user-facing exclusion mechanism (pin = exclude from recommendations).
		// tier_policies: per-pool policy for auto-move behavior.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS tier_recommendations (
				id              TEXT PRIMARY KEY,
				collection_id   TEXT NOT NULL,
				pool_id         TEXT NOT NULL,
				direction       TEXT NOT NULL,
				reason          TEXT NOT NULL,
				size_bytes      INTEGER NOT NULL,
				last_access     INTEGER NOT NULL,
				score           REAL NOT NULL,
				status          TEXT NOT NULL,
				created_at      INTEGER NOT NULL,
				decided_at      INTEGER NOT NULL DEFAULT 0,
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create tier_recommendations table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_tier_recommendations_collection ON tier_recommendations(collection_id)`)
		if err != nil {
			return fmt.Errorf("create tier_recommendations collection index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_tier_recommendations_status ON tier_recommendations(status)`)
		if err != nil {
			return fmt.Errorf("create tier_recommendations status index: %w", err)
		}

		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS tier_pins (
				collection_id   TEXT PRIMARY KEY,
				pinned_at       INTEGER NOT NULL,
				note            TEXT NOT NULL DEFAULT '',
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create tier_pins table: %w", err)
		}

		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS tier_policies (
				pool_id             TEXT PRIMARY KEY,
				mode                TEXT NOT NULL,
				cold_threshold_days INTEGER NOT NULL,
				auto_move_wait_days INTEGER NOT NULL,
				FOREIGN KEY (pool_id) REFERENCES pools(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create tier_policies table: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 22 {
		// WS34-A (2026-11-04): Tier Curator auto-move engine (FR-2.3).
		// tier_migrations tracks the execution of approved/silent-consent migrations.
		// Status values: scheduled, executing, completed, failed, finalized, undone, skipped_pinned.
		// manifest stores JSON array of {rel_path, sha256} for source tree verification.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS tier_migrations (
				id              TEXT PRIMARY KEY,
				collection_id   TEXT NOT NULL,
				recommendation_id TEXT,
				from_pool_id    TEXT NOT NULL,
				to_pool_id      TEXT NOT NULL,
				direction       TEXT NOT NULL,
				status          TEXT NOT NULL,
				manifest        TEXT NOT NULL DEFAULT '',
				reason          TEXT NOT NULL DEFAULT '',
				created_at      INTEGER NOT NULL,
				executed_at     INTEGER NOT NULL DEFAULT 0,
				retain_until    INTEGER NOT NULL DEFAULT 0,
				finalized_at    INTEGER NOT NULL DEFAULT 0,
				undone_at       INTEGER NOT NULL DEFAULT 0,
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create tier_migrations table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_tier_migrations_collection ON tier_migrations(collection_id)`)
		if err != nil {
			return fmt.Errorf("create tier_migrations collection index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_tier_migrations_status ON tier_migrations(status)`)
		if err != nil {
			return fmt.Errorf("create tier_migrations status index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_tier_migrations_recommendation ON tier_migrations(recommendation_id)`)
		if err != nil {
			return fmt.Errorf("create tier_migrations recommendation index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 23 {
		// WS35 (2026-11-05): Shadow read replicas in Warm (FR-2.5).
		// file_shadows tracks read-only replicas of Cold files created in Warm pools.
		// The shadow root on disk is <warm-mount>/.kelvios-shadow/<collection-id>/<rel_path>.
		// The catalog row records shadow_pool_id + shadow_rel_path; the logical path
		// the user sees never changes.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS file_shadows (
				id              TEXT PRIMARY KEY,
				file_id         TEXT NOT NULL,
				collection_id   TEXT NOT NULL,
				rel_path        TEXT NOT NULL,
				shadow_pool_id  TEXT NOT NULL,
				shadow_rel_path TEXT NOT NULL,
				sha256          TEXT NOT NULL,
				size_bytes      INTEGER NOT NULL,
				created_at      INTEGER NOT NULL,
				last_served_at  INTEGER NOT NULL DEFAULT 0,
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE,
				FOREIGN KEY (shadow_pool_id) REFERENCES pools(id)
			);
		`)
		if err != nil {
			return fmt.Errorf("create file_shadows table: %w", err)
		}

		_, err = tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_file_shadows_collection_relpath ON file_shadows(collection_id, rel_path)`)
		if err != nil {
			return fmt.Errorf("create file_shadows collection_relpath index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 24 {
		// WS36 (FR-2.6): dirty write-back columns on file_shadows.
		for _, col := range []string{
			`ALTER TABLE file_shadows ADD COLUMN dirty INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE file_shadows ADD COLUMN version INTEGER NOT NULL DEFAULT 1`,
			`ALTER TABLE file_shadows ADD COLUMN dirty_at INTEGER NOT NULL DEFAULT 0`,
		} {
			if _, err = tx.Exec(col); err != nil {
				return fmt.Errorf("add file_shadows column: %w", err)
			}
		}
		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 25 {
		// WS37 (FR-2.8): Shadow eviction + promotion recommendations.
		// shadow_promotions tracks per-file promotion recommendations from shadow to Warm.
		// status: pending | approved | rejected | superseded
		// Unique index on (collection_id, rel_path) for pending rows enforces one
		// non-superseded promotion per (collection, rel_path) (like WS33 tier_recommendations).
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS shadow_promotions (
				id                  TEXT PRIMARY KEY,
				collection_id       TEXT NOT NULL,
				rel_path            TEXT NOT NULL,
				shadow_pool_id      TEXT NOT NULL,
				access_count_30d    INTEGER NOT NULL,
				status              TEXT NOT NULL, -- pending | approved | rejected | superseded
				created_at          INTEGER NOT NULL,
				decided_at          INTEGER NOT NULL DEFAULT 0,
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE,
				FOREIGN KEY (shadow_pool_id) REFERENCES pools(id)
			);
		`)
		if err != nil {
			return fmt.Errorf("create shadow_promotions table: %w", err)
		}

		_, err = tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_shadow_promotions_pending_unique ON shadow_promotions(collection_id, rel_path) WHERE status != 'superseded'`)
		if err != nil {
			return fmt.Errorf("create shadow_promotions pending unique index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_shadow_promotions_collection ON shadow_promotions(collection_id)`)
		if err != nil {
			return fmt.Errorf("create shadow_promotions collection index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_shadow_promotions_status ON shadow_promotions(status)`)
		if err != nil {
			return fmt.Errorf("create shadow_promotions status index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 26 {
		// WS38 (2026-11-06): Auto-Arrange for media collections (FR-2.11).
		// arrange_plans: approval-first arrangement plans per collection.
		// arrange_moves: individual source->dest moves with conflict tracking.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS arrange_plans (
				id              TEXT PRIMARY KEY,
				collection_id   TEXT NOT NULL,
				status          TEXT NOT NULL, -- pending | approved | rejected | applied | undone
				created_at      INTEGER NOT NULL,
				decided_at      INTEGER NOT NULL DEFAULT 0,
				applied_at      INTEGER NOT NULL DEFAULT 0,
				undone_at       INTEGER NOT NULL DEFAULT 0,
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create arrange_plans table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_arrange_plans_collection_status ON arrange_plans(collection_id, status)`)
		if err != nil {
			return fmt.Errorf("create arrange_plans collection_status index: %w", err)
		}

		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS arrange_moves (
				id              TEXT PRIMARY KEY,
				plan_id         TEXT NOT NULL,
				source_rel      TEXT NOT NULL,
				dest_rel        TEXT NOT NULL,
				conflict        TEXT NOT NULL DEFAULT '', -- '' | 'dest_exists' | 'duplicate_dest'
				status          TEXT NOT NULL DEFAULT 'planned', -- planned | moved | skipped_conflict | undone
				sha256          TEXT NOT NULL DEFAULT '',
				created_at      INTEGER NOT NULL,
				FOREIGN KEY (plan_id) REFERENCES arrange_plans(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create arrange_moves table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_arrange_moves_plan ON arrange_moves(plan_id)`)
		if err != nil {
			return fmt.Errorf("create arrange_moves plan index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 27 {
		// WS39 (2026-11-07): AI content tagging & search index (FR-2.12 Phase-2).
		// tag_clusters: deterministic clusters by device|gps|date.
		// tag_cluster_members: cluster membership.
		// media_tags: tags per file with origin, confidence, source.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS tag_clusters (
				id              TEXT PRIMARY KEY,
				collection_id   TEXT NOT NULL,
				cluster_key     TEXT NOT NULL,
				sample_rel_path TEXT NOT NULL,
				created_at      INTEGER NOT NULL,
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create tag_clusters table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_tag_clusters_collection ON tag_clusters(collection_id)`)
		if err != nil {
			return fmt.Errorf("create tag_clusters collection index: %w", err)
		}

		_, err = tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_tag_clusters_collection_key ON tag_clusters(collection_id, cluster_key)`)
		if err != nil {
			return fmt.Errorf("create tag_clusters collection_key unique index: %w", err)
		}

		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS tag_cluster_members (
				cluster_id TEXT NOT NULL,
				rel_path   TEXT NOT NULL,
				PRIMARY KEY (cluster_id, rel_path),
				FOREIGN KEY (cluster_id) REFERENCES tag_clusters(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create tag_cluster_members table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_tag_cluster_members_relpath ON tag_cluster_members(rel_path)`)
		if err != nil {
			return fmt.Errorf("create tag_cluster_members rel_path index: %w", err)
		}

		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS media_tags (
				id              TEXT PRIMARY KEY,
				collection_id   TEXT NOT NULL,
				rel_path        TEXT NOT NULL,
				tag             TEXT NOT NULL,
				origin          TEXT NOT NULL, -- 'rule' | 'cluster' | 'sample' | 'manual'
				confidence      REAL NOT NULL,
				source          TEXT NOT NULL, -- 'individual' | 'cluster'
				created_at      INTEGER NOT NULL,
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE,
				UNIQUE(collection_id, rel_path, tag, origin)
			);
		`)
		if err != nil {
			return fmt.Errorf("create media_tags table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_media_tags_collection_path ON media_tags(collection_id, rel_path)`)
		if err != nil {
			return fmt.Errorf("create media_tags collection_path index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_media_tags_tag ON media_tags(tag)`)
		if err != nil {
			return fmt.Errorf("create media_tags tag index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_media_tags_origin ON media_tags(origin)`)
		if err != nil {
			return fmt.Errorf("create media_tags origin index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 28 {
		// WS40 (2026-11-08): Restic backup catalog (FR-3.1).
		// backup_repos: configured restic repositories.
		// backup_snapshots: snapshots taken per repo per collection.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS backup_repos (
				id           TEXT PRIMARY KEY,
				name         TEXT NOT NULL UNIQUE,
				target_type  TEXT NOT NULL DEFAULT 'local',
				target_path  TEXT NOT NULL,
				created_at   INTEGER NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create backup_repos table: %w", err)
		}

		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS backup_snapshots (
				id                   TEXT PRIMARY KEY,
				repo_id              TEXT NOT NULL,
				collection_id        TEXT NOT NULL,
				restic_snapshot_id   TEXT NOT NULL,
				created_at           INTEGER NOT NULL,
				FOREIGN KEY (repo_id) REFERENCES backup_repos(id) ON DELETE CASCADE,
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create backup_snapshots table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_backup_snapshots_repo ON backup_snapshots(repo_id)`)
		if err != nil {
			return fmt.Errorf("create backup_snapshots repo index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_backup_snapshots_collection ON backup_snapshots(collection_id)`)
		if err != nil {
			return fmt.Errorf("create backup_snapshots collection index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 29 {
		// WS42-A (2026-11-09): Wake piggyback checklist (FR-2.9 production).
		// wake_checklist_runs: records each checklist execution with per-step statuses.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS wake_checklist_runs (
				id           TEXT PRIMARY KEY,
				pool_id      TEXT NOT NULL,
				disk         TEXT NOT NULL,
				started_at   INTEGER NOT NULL,
				finished_at  INTEGER NOT NULL,
				steps        TEXT NOT NULL -- JSON array of {step, status, detail}
			);
		`)
		if err != nil {
			return fmt.Errorf("create wake_checklist_runs table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_wake_checklist_runs_disk ON wake_checklist_runs(disk, started_at DESC)`)
		if err != nil {
			return fmt.Errorf("create wake_checklist_runs disk index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 30 {
		// WS42-B (2026-11-10): Monthly maintenance (FR-2.9 production).
		// files table gains checksum TEXT + checksum_verified_at INTEGER.
		// scrub_state table tracks rotating scrub cursor per collection.
		// Guard ALTERs via PRAGMA table_info to be idempotent.

		// Check if files table exists (guard for minimal schemas / test fixtures)
		var filesTableExists int
		err = tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='files'`).Scan(&filesTableExists)
		if err != nil {
			return fmt.Errorf("check files table: %w", err)
		}
		if filesTableExists > 0 {
			// Check for existing checksum column (might be named differently)
			rows, err := tx.Query(`PRAGMA table_info(files)`)
			if err != nil {
				return fmt.Errorf("inspect files table: %w", err)
			}
			hasChecksum := false
			hasChecksumVerifiedAt := false
			for rows.Next() {
				var cid int
				var colName, colType string
				var notNull int
				var defaultValue any
				var pk int
				if err := rows.Scan(&cid, &colName, &colType, &notNull, &defaultValue, &pk); err != nil {
					rows.Close()
					return fmt.Errorf("scan files column: %w", err)
				}
				if colName == "checksum" {
					hasChecksum = true
				}
				if colName == "checksum_verified_at" {
					hasChecksumVerifiedAt = true
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("inspect files columns: %w", err)
			}
			if !hasChecksum {
				_, err = tx.Exec(`ALTER TABLE files ADD COLUMN checksum TEXT`)
				if err != nil {
					return fmt.Errorf("add checksum column: %w", err)
				}
			}
			if !hasChecksumVerifiedAt {
				_, err = tx.Exec(`ALTER TABLE files ADD COLUMN checksum_verified_at INTEGER NOT NULL DEFAULT 0`)
				if err != nil {
					return fmt.Errorf("add checksum_verified_at column: %w", err)
				}
			}
		}

		// Create scrub_state table
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS scrub_state (
				collection_id   TEXT PRIMARY KEY,
				cursor          TEXT NOT NULL DEFAULT '',
				last_run_at     INTEGER NOT NULL DEFAULT 0,
				verified_total  INTEGER NOT NULL DEFAULT 0,
				FOREIGN KEY (collection_id) REFERENCES collections(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create scrub_state table: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 31 {
		// WS43 (2026-11-11): Remote backup provider registry (FR-3.2).
		// backup_repos gains config_json (non-secret params) and secret_name
		// (vault secret name holding secrets). Guard ALTERs via PRAGMA table_info.
		var backupReposTableExists int
		err = tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='backup_repos'`).Scan(&backupReposTableExists)
		if err != nil {
			return fmt.Errorf("check backup_repos table: %w", err)
		}
		if backupReposTableExists > 0 {
			rows, err := tx.Query(`PRAGMA table_info(backup_repos)`)
			if err != nil {
				return fmt.Errorf("inspect backup_repos table: %w", err)
			}
			hasConfigJSON := false
			hasSecretName := false
			for rows.Next() {
				var cid int
				var colName, colType string
				var notNull int
				var defaultValue any
				var pk int
				if err := rows.Scan(&cid, &colName, &colType, &notNull, &defaultValue, &pk); err != nil {
					rows.Close()
					return fmt.Errorf("scan backup_repos column: %w", err)
				}
				if colName == "config_json" {
					hasConfigJSON = true
				}
				if colName == "secret_name" {
					hasSecretName = true
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("inspect backup_repos columns: %w", err)
			}
			if !hasConfigJSON {
				_, err = tx.Exec(`ALTER TABLE backup_repos ADD COLUMN config_json TEXT NOT NULL DEFAULT ''`)
				if err != nil {
					return fmt.Errorf("add config_json column: %w", err)
				}
			}
			if !hasSecretName {
				_, err = tx.Exec(`ALTER TABLE backup_repos ADD COLUMN secret_name TEXT NOT NULL DEFAULT ''`)
				if err != nil {
					return fmt.Errorf("add secret_name column: %w", err)
				}
			}
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 32 {
		// WS44 (2026-11-12): Scheduled restore tests (FR-3.3).
		// restore_test_runs: one row per scheduled run with aggregate counts.
		// restore_test_alerts: one row per failed/skipped sample with reason.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS restore_test_runs (
				id           TEXT PRIMARY KEY,
				started_at   INTEGER NOT NULL,
				samples      INTEGER NOT NULL DEFAULT 0,
				passed       INTEGER NOT NULL DEFAULT 0,
				failed       INTEGER NOT NULL DEFAULT 0,
				skipped      INTEGER NOT NULL DEFAULT 0,
				alerts       INTEGER NOT NULL DEFAULT 0
			);
		`)
		if err != nil {
			return fmt.Errorf("create restore_test_runs table: %w", err)
		}

		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS restore_test_alerts (
				id           TEXT PRIMARY KEY,
				run_id       TEXT NOT NULL,
				repo_id      TEXT NOT NULL,
				snapshot_id  TEXT NOT NULL,
				rel_path     TEXT NOT NULL,
				reason       TEXT NOT NULL,
				created_at   INTEGER NOT NULL,
				resolved     INTEGER NOT NULL DEFAULT 0,
				FOREIGN KEY (run_id) REFERENCES restore_test_runs(id) ON DELETE CASCADE
			);
		`)
		if err != nil {
			return fmt.Errorf("create restore_test_alerts table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_restore_test_alerts_run ON restore_test_alerts(run_id)`)
		if err != nil {
			return fmt.Errorf("create restore_test_alerts run index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 33 {
		// WS46 (2026-11-13): LLM registry (FR-3.5).
		// llm_models catalog table for LLM model entries.
		// secret_name references a vault secret (never stores the key itself).
		// Active model pointer stored in meta key 'llm_active_model'.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS llm_models (
				id           TEXT PRIMARY KEY,
				name         TEXT NOT NULL UNIQUE,
				provider     TEXT NOT NULL,
				endpoint_env TEXT NOT NULL,
				model_id     TEXT NOT NULL,
				capabilities TEXT NOT NULL DEFAULT '',
				secret_name  TEXT NOT NULL DEFAULT '',
				created_at   INTEGER NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create llm_models table: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 34 {
		// WS48 (2026-11-14): Human gate (P6) at MCP level (FR-3.7).
		// mcp_approvals table for destructive tool approval workflow.
		// Status: pending | approved | rejected | expired
		// Expiry: 24h from requested_at (enforced on read + sweep on list).
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS mcp_approvals (
				id              TEXT PRIMARY KEY,
				tool            TEXT NOT NULL,
				arguments       TEXT NOT NULL, -- JSON
				requested_by    TEXT NOT NULL, -- agent name + model id
				requested_at    INTEGER NOT NULL,
				status          TEXT NOT NULL, -- pending | approved | rejected | expired
				decided_by      TEXT NOT NULL DEFAULT '',
				decided_at      INTEGER NOT NULL DEFAULT 0,
				expires_at      INTEGER NOT NULL,
				consumed_at     INTEGER NOT NULL DEFAULT 0
			);
		`)
		if err != nil {
			return fmt.Errorf("create mcp_approvals table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_mcp_approvals_status ON mcp_approvals(status)`)
		if err != nil {
			return fmt.Errorf("create mcp_approvals status index: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_mcp_approvals_requested_at ON mcp_approvals(requested_at DESC)`)
		if err != nil {
			return fmt.Errorf("create mcp_approvals requested_at index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 35 {
		// WS49-A (2026-11-06): Chat Bridge CORE (FR-3.8).
		// chat_links: paired chats (allowlist) with PIN hash for sensitive commands.
		// chat_pairing_codes: one-time pairing codes with 10-min expiry.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS chat_links (
				id           TEXT PRIMARY KEY,
				transport    TEXT NOT NULL,
				chat_id      TEXT NOT NULL,
				label        TEXT,
				pin_hash     TEXT NOT NULL, -- salt:hash (hex-encoded SHA-256)
				created_at   INTEGER NOT NULL,
				last_seen_at INTEGER NOT NULL,
				UNIQUE(transport, chat_id)
			);
		`)
		if err != nil {
			return fmt.Errorf("create chat_links table: %w", err)
		}

		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS chat_pairing_codes (
				code       TEXT PRIMARY KEY,
				transport  TEXT NOT NULL,
				expires_at INTEGER NOT NULL,
				used_at    INTEGER
			);
		`)
		if err != nil {
			return fmt.Errorf("create chat_pairing_codes table: %w", err)
		}

		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_chat_pairing_codes_transport ON chat_pairing_codes(transport)`)
		if err != nil {
			return fmt.Errorf("create chat_pairing_codes transport index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 36 {
		// WS51-A (2026-11-07): Agent setup modes (FR-3.9).
		// agent_modes table with single-row design (id=1 enforced by CHECK constraint).
		// Columns: resident_harness, external_mcp, preset, updated_at.
		// Default row inserted if table is empty: harness=1, mcp=0, preset='agent_inside'.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS agent_modes (
				id                  INTEGER PRIMARY KEY CHECK (id = 1),
				resident_harness    INTEGER NOT NULL,
				external_mcp        INTEGER NOT NULL,
				preset              TEXT NOT NULL,
				updated_at          INTEGER NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create agent_modes table: %w", err)
		}

		// Insert default row if table is empty
		var count int
		err = tx.QueryRow(`SELECT COUNT(*) FROM agent_modes`).Scan(&count)
		if err != nil {
			return fmt.Errorf("count agent_modes rows: %w", err)
		}
		if count == 0 {
			_, err = tx.Exec(`
				INSERT INTO agent_modes (id, resident_harness, external_mcp, preset, updated_at)
				VALUES (1, 1, 0, 'agent_inside', ?)
			`, time.Now().Unix())
			if err != nil {
				return fmt.Errorf("insert default agent_modes row: %w", err)
			}
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 37 {
		// WS52-A (2026-11-07): Compose backend core (FR-3.10).
		// compose_artifacts: report_schedule, automation_rule, recipe, tool_chain definitions with versioning.
		// compose_artifact_versions: immutable spec history per artifact.
		// compose_runs: execution records with result + detail.
		_, err = tx.Exec(
			"CREATE TABLE IF NOT EXISTS compose_artifacts (" +
				"id              TEXT PRIMARY KEY," +
				"name            TEXT NOT NULL," +
				"kind            TEXT NOT NULL," +
				"status          TEXT NOT NULL DEFAULT 'pending'," +
				"current_version INTEGER NOT NULL DEFAULT 1," +
				"spec            TEXT NOT NULL DEFAULT ''," +
				"created_by      TEXT NOT NULL DEFAULT ''," +
				"created_at      INTEGER NOT NULL," +
				"updated_at      INTEGER NOT NULL" +
			")")
		if err != nil {
			return fmt.Errorf("create compose_artifacts table: %w", err)
		}

		_, err = tx.Exec(
			"CREATE TABLE IF NOT EXISTS compose_artifact_versions (" +
				"artifact_id     TEXT NOT NULL," +
				"version         INTEGER NOT NULL," +
				"spec            TEXT NOT NULL," +
				"created_by      TEXT NOT NULL DEFAULT ''," +
				"created_at      INTEGER NOT NULL," +
				"PRIMARY KEY (artifact_id, version)" +
			")")
		if err != nil {
			return fmt.Errorf("create compose_artifact_versions table: %w", err)
		}

		_, err = tx.Exec(
			"CREATE TABLE IF NOT EXISTS compose_runs (" +
				"id              INTEGER PRIMARY KEY AUTOINCREMENT," +
				"artifact_id     TEXT NOT NULL," +
				"version         INTEGER NOT NULL," +
				"started_at      INTEGER NOT NULL," +
				"finished_at     INTEGER NOT NULL DEFAULT 0," +
				"result          TEXT NOT NULL DEFAULT ''," +
				"detail          TEXT NOT NULL DEFAULT ''" +
			")")
		if err != nil {
			return fmt.Errorf("create compose_runs table: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 38 {
		// WS53-B (2026-11-07): REST App Library (FR-4.1/FR-4.4).
		// app_installs tracks installed app status only (app_id, status, updated_at).
		// Status values: available, installed, active.
		// Apps without a row are implicitly "available".
		_, err = tx.Exec(
			"CREATE TABLE IF NOT EXISTS app_installs (" +
				"app_id      TEXT PRIMARY KEY," +
				"status      TEXT NOT NULL," +
				"updated_at  INTEGER NOT NULL" +
			")")
		if err != nil {
			return fmt.Errorf("create app_installs table: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 39 {
		// WS55 (2026-11-08): App Update Management (FR-4.8).
		// Add columns to app_installs for tracking installed image, version, digest, and previous image for rollback.
		// Guard ALTERs via PRAGMA table_info to be idempotent.
		var appInstallsTableExists int
		err = tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='app_installs'`).Scan(&appInstallsTableExists)
		if err != nil {
			return fmt.Errorf("check app_installs table: %w", err)
		}
		if appInstallsTableExists > 0 {
			rows, err := tx.Query(`PRAGMA table_info(app_installs)`)
			if err != nil {
				return fmt.Errorf("inspect app_installs table: %w", err)
			}
			hasImageRef := false
			hasVersionLabel := false
			hasDigest := false
			hasPreviousImageRef := false
			for rows.Next() {
				var cid int
				var colName, colType string
				var notNull int
				var defaultValue any
				var pk int
				if err := rows.Scan(&cid, &colName, &colType, &notNull, &defaultValue, &pk); err != nil {
					rows.Close()
					return fmt.Errorf("scan app_installs column: %w", err)
				}
				if colName == "image_ref" {
					hasImageRef = true
				}
				if colName == "version_label" {
					hasVersionLabel = true
				}
				if colName == "digest" {
					hasDigest = true
				}
				if colName == "previous_image_ref" {
					hasPreviousImageRef = true
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("inspect app_installs columns: %w", err)
			}
			if !hasImageRef {
				_, err = tx.Exec(`ALTER TABLE app_installs ADD COLUMN image_ref TEXT`)
				if err != nil {
					return fmt.Errorf("add image_ref column: %w", err)
				}
			}
			if !hasVersionLabel {
				_, err = tx.Exec(`ALTER TABLE app_installs ADD COLUMN version_label TEXT`)
				if err != nil {
					return fmt.Errorf("add version_label column: %w", err)
				}
			}
			if !hasDigest {
				_, err = tx.Exec(`ALTER TABLE app_installs ADD COLUMN digest TEXT`)
				if err != nil {
					return fmt.Errorf("add digest column: %w", err)
				}
			}
			if !hasPreviousImageRef {
				_, err = tx.Exec(`ALTER TABLE app_installs ADD COLUMN previous_image_ref TEXT`)
				if err != nil {
					return fmt.Errorf("add previous_image_ref column: %w", err)
				}
			}
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 40 {
		// WS59 (2026-11-09): OTA Core (FR-5.1).
		// Create ota_updates table for tracking OTA update packages and their state.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS ota_updates (
				id              TEXT PRIMARY KEY,
				version         TEXT NOT NULL,
				channel         TEXT NOT NULL,
				kind            TEXT NOT NULL,
				manifest_path   TEXT NOT NULL,
				artifact_name   TEXT NOT NULL,
				artifact_sha256 TEXT NOT NULL,
				signature_status TEXT NOT NULL,
				slot_target     TEXT,
				state           TEXT NOT NULL,
				last_actor      TEXT NOT NULL,
				last_detail     TEXT,
				created_at      INTEGER NOT NULL,
				updated_at      INTEGER NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create ota_updates table: %w", err)
		}
		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_ota_updates_state ON ota_updates(state)`)
		if err != nil {
			return fmt.Errorf("create ota_updates state index: %w", err)
		}
		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_ota_updates_created_at ON ota_updates(created_at DESC)`)
		if err != nil {
			return fmt.Errorf("create ota_updates created_at index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	if currentVersion < 41 {
		// WS62 (2026-11-10): FR-3.11 Event→Action Rules Engine.
		// Create event_rules table for user-defined event→action rules.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS event_rules (
				id                   TEXT PRIMARY KEY,
				name                 TEXT NOT NULL,
				event_type           TEXT NOT NULL,
				event_params         TEXT NOT NULL DEFAULT '{}',
				actions              TEXT NOT NULL DEFAULT '[]',
				enabled              INTEGER NOT NULL DEFAULT 1,
				cooldown_seconds     INTEGER NOT NULL DEFAULT 3600,
				max_fires_per_day    INTEGER NOT NULL DEFAULT 24,
				state                TEXT NOT NULL DEFAULT '{}',
				last_fired_at        INTEGER NOT NULL DEFAULT 0,
				created_at           INTEGER NOT NULL,
				updated_at           INTEGER NOT NULL
			);
		`)
		if err != nil {
			return fmt.Errorf("create event_rules table: %w", err)
		}
		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_event_rules_enabled ON event_rules(enabled, event_type)`)
		if err != nil {
			return fmt.Errorf("create event_rules enabled index: %w", err)
		}

		// Create event_rule_runs table for tracking rule executions.
		_, err = tx.Exec(`
			CREATE TABLE IF NOT EXISTS event_rule_runs (
				id                INTEGER PRIMARY KEY AUTOINCREMENT,
				rule_id           TEXT NOT NULL,
				event_ref         TEXT NOT NULL,
				fired_at          INTEGER NOT NULL,
				result            TEXT NOT NULL DEFAULT '',
				detail            TEXT NOT NULL DEFAULT '',
				alert             INTEGER NOT NULL DEFAULT 0,
				test_fire         INTEGER NOT NULL DEFAULT 0,
				UNIQUE (rule_id, event_ref)
			);
		`)
		if err != nil {
			return fmt.Errorf("create event_rule_runs table: %w", err)
		}
		_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_event_rule_runs_rule ON event_rule_runs(rule_id, fired_at DESC)`)
		if err != nil {
			return fmt.Errorf("create event_rule_runs rule index: %w", err)
		}

		_, err = tx.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion)
		if err != nil {
			return fmt.Errorf("set schema_version: %w", err)
		}
	}

	return tx.Commit()
}

// PoolRow is the catalog representation of a pool.
// Mirrors storage.Pool with catalog-specific fields.
type PoolRow struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	NodeID     string    `json:"node_id"`
	Tier       string    `json:"tier"`
	Mechanism  string    `json:"mechanism"`
	Driver     string    `json:"driver"`
	Mountpoint string    `json:"mountpoint"`
	SizeBytes  int64     `json:"size_bytes"`
	UsedBytes  int64     `json:"used_bytes"`
	Encrypted  bool      `json:"encrypted"`
	EncryptionMode string `json:"encryption_mode"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// CollectionRow is the catalog representation of a collection.
type CollectionRow struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	PoolID         string     `json:"pool_id"`
	Path           string     `json:"path"`
	DiskName       string     `json:"disk_name,omitempty"`
	SizeBytes      int64      `json:"size_bytes"`
	LastAccessedAt *time.Time `json:"last_accessed_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// CreatePool inserts a pool and its disk associations into the catalog.
// Returns ErrPoolNameExists if a pool with the same name (case-insensitive) already exists.
func CreatePool(db *sql.DB, pool *PoolRow, diskNames []string) error {
	now := time.Now().Unix()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT INTO pools (id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, pool.ID, pool.Name, pool.NodeID, pool.Tier, pool.Mechanism, pool.Driver, pool.Mountpoint, pool.SizeBytes, pool.UsedBytes, boolToInt(pool.Encrypted), pool.EncryptionMode, pool.Status, now, now)
	if err != nil {
		// Check for unique constraint violation on pools.name (case-insensitive)
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "unique") {
			return ErrPoolNameExists
		}
		return fmt.Errorf("insert pool: %w", err)
	}

	for _, diskName := range diskNames {
		_, err = tx.Exec(`INSERT INTO pool_disks (pool_id, disk_name, created_at) VALUES (?, ?, ?)`, pool.ID, diskName, now)
		if err != nil {
			return fmt.Errorf("insert pool_disk: %w", err)
		}
	}

	return tx.Commit()
}

// GetPool retrieves a pool by ID with its disk names.
func GetPool(db *sql.DB, id string) (*PoolRow, []string, error) {
	row := db.QueryRow(`SELECT id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at FROM pools WHERE id = ?`, id)
	var p PoolRow
	var createdAt, updatedAt int64
	var encrypted int
	if err := row.Scan(&p.ID, &p.Name, &p.NodeID, &p.Tier, &p.Mechanism, &p.Driver, &p.Mountpoint, &p.SizeBytes, &p.UsedBytes, &encrypted, &p.EncryptionMode, &p.Status, &createdAt, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("scan pool: %w", err)
	}
	p.Encrypted = encrypted != 0
	p.CreatedAt = time.Unix(createdAt, 0)
	p.UpdatedAt = time.Unix(updatedAt, 0)

	diskRows, err := db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("query pool disks: %w", err)
	}
	defer diskRows.Close()

	var disks []string
	for diskRows.Next() {
		var d string
		if err := diskRows.Scan(&d); err != nil {
			return nil, nil, fmt.Errorf("scan pool disk: %w", err)
		}
		disks = append(disks, d)
	}

	return &p, disks, nil
}

// ListPools returns all pools with their disk names.
func ListPools(db *sql.DB) ([]*PoolRow, error) {
	rows, err := db.Query(`SELECT id, name, node_id, tier, mechanism, driver, mountpoint, size_bytes, used_bytes, encrypted, encryption_mode, status, created_at, updated_at FROM pools ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("query pools: %w", err)
	}
	defer rows.Close()

	var pools []*PoolRow
	for rows.Next() {
		var p PoolRow
		var createdAt, updatedAt int64
		var encrypted int
		if err := rows.Scan(&p.ID, &p.Name, &p.NodeID, &p.Tier, &p.Mechanism, &p.Driver, &p.Mountpoint, &p.SizeBytes, &p.UsedBytes, &encrypted, &p.EncryptionMode, &p.Status, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan pool: %w", err)
		}
		p.Encrypted = encrypted != 0
		p.CreatedAt = time.Unix(createdAt, 0)
		p.UpdatedAt = time.Unix(updatedAt, 0)
		pools = append(pools, &p)
	}

	// Fetch disk names for each pool
	for _, p := range pools {
		diskRows, err := db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, p.ID)
		if err != nil {
			return nil, fmt.Errorf("query pool disks: %w", err)
		}
		var disks []string
		for diskRows.Next() {
			var d string
			if err := diskRows.Scan(&d); err != nil {
				diskRows.Close()
				return nil, fmt.Errorf("scan pool disk: %w", err)
			}
			disks = append(disks, d)
		}
		diskRows.Close()
		// Store disks in a way that can be accessed - we'll add a field or return separately
		_ = disks // placeholder; in practice we'd return a combined struct
	}

	return pools, nil
}

// DeletePool removes a pool and its disk associations (cascades to pool_disks).
func DeletePool(db *sql.DB, id string) error {
	res, err := db.Exec(`DELETE FROM pools WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete pool: %w", err)
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

// CreateCollection inserts a collection into the catalog.
func CreateCollection(db *sql.DB, coll *CollectionRow) error {
	now := time.Now().Unix()
	var lastAccessed interface{}
	if coll.LastAccessedAt != nil {
		lastAccessed = coll.LastAccessedAt.Unix()
	}
	_, err := db.Exec(`
		INSERT INTO collections (id, name, pool_id, path, disk_name, size_bytes, last_accessed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, coll.ID, coll.Name, coll.PoolID, coll.Path, coll.DiskName, coll.SizeBytes, lastAccessed, now, now)
	return err
}

// ListCollections returns all collections for a pool.
func ListCollections(db *sql.DB, poolID string) ([]*CollectionRow, error) {
	rows, err := db.Query(`SELECT id, name, pool_id, path, disk_name, size_bytes, last_accessed_at, created_at, updated_at FROM collections WHERE pool_id = ? ORDER BY created_at`, poolID)
	if err != nil {
		return nil, fmt.Errorf("query collections: %w", err)
	}
	defer rows.Close()

	var collections []*CollectionRow
	for rows.Next() {
		var c CollectionRow
		var createdAt, updatedAt int64
		var lastAccessed sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Name, &c.PoolID, &c.Path, &c.DiskName, &c.SizeBytes, &lastAccessed, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan collection: %w", err)
		}
		if lastAccessed.Valid {
			t := time.Unix(lastAccessed.Int64, 0)
			c.LastAccessedAt = &t
		}
		c.CreatedAt = time.Unix(createdAt, 0)
		c.UpdatedAt = time.Unix(updatedAt, 0)
		collections = append(collections, &c)
	}
	return collections, nil
}

// GetCollection retrieves a collection by ID.
func GetCollection(db *sql.DB, id string) (*CollectionRow, error) {
	row := db.QueryRow(`SELECT id, name, pool_id, path, disk_name, size_bytes, last_accessed_at, created_at, updated_at FROM collections WHERE id = ?`, id)
	var c CollectionRow
	var createdAt, updatedAt int64
	var lastAccessed sql.NullInt64
	if err := row.Scan(&c.ID, &c.Name, &c.PoolID, &c.Path, &c.DiskName, &c.SizeBytes, &lastAccessed, &createdAt, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan collection: %w", err)
	}
	if lastAccessed.Valid {
		t := time.Unix(lastAccessed.Int64, 0)
		c.LastAccessedAt = &t
	}
	c.CreatedAt = time.Unix(createdAt, 0)
	c.UpdatedAt = time.Unix(updatedAt, 0)
	return &c, nil
}

// DeleteCollection removes a collection by ID.
func DeleteCollection(db *sql.DB, id string) error {
	res, err := db.Exec(`DELETE FROM collections WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete collection: %w", err)
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

// ShareRow is the catalog representation of a collection share.
type ShareRow struct {
	ID           string    `json:"id"`
	CollectionID string    `json:"collection_id"`
	Name         string    `json:"name"`
	Protocol     string    `json:"protocol"`
	ReadOnly     bool      `json:"read_only"`
	Metadata     string    `json:"metadata,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// CreateShare inserts a share for a collection into the catalog.
func CreateShare(db *sql.DB, share *ShareRow) error {
	now := time.Now().Unix()
	_, err := db.Exec(`
		INSERT INTO collection_shares (id, collection_id, name, protocol, read_only, metadata, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, share.ID, share.CollectionID, share.Name, share.Protocol, boolToInt(share.ReadOnly), share.Metadata, now, now)
	return err
}

// GetShare retrieves a share by ID.
func GetShare(db *sql.DB, id string) (*ShareRow, error) {
	row := db.QueryRow(`SELECT id, collection_id, name, protocol, read_only, metadata, created_at, updated_at FROM collection_shares WHERE id = ?`, id)
	var s ShareRow
	var createdAt, updatedAt int64
	var readOnly int
	var metadata sql.NullString
	if err := row.Scan(&s.ID, &s.CollectionID, &s.Name, &s.Protocol, &readOnly, &metadata, &createdAt, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan share: %w", err)
	}
	s.ReadOnly = readOnly != 0
	if metadata.Valid {
		s.Metadata = metadata.String
	}
	s.CreatedAt = time.Unix(createdAt, 0)
	s.UpdatedAt = time.Unix(updatedAt, 0)
	return &s, nil
}

// ListShares returns all shares for a collection.
func ListShares(db *sql.DB, collectionID string) ([]*ShareRow, error) {
	rows, err := db.Query(`SELECT id, collection_id, name, protocol, read_only, metadata, created_at, updated_at FROM collection_shares WHERE collection_id = ? ORDER BY created_at`, collectionID)
	if err != nil {
		return nil, fmt.Errorf("query shares: %w", err)
	}
	defer rows.Close()

	var shares []*ShareRow
	for rows.Next() {
		var s ShareRow
		var createdAt, updatedAt int64
		var readOnly int
		var metadata sql.NullString
		if err := rows.Scan(&s.ID, &s.CollectionID, &s.Name, &s.Protocol, &readOnly, &metadata, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan share: %w", err)
		}
		s.ReadOnly = readOnly != 0
		if metadata.Valid {
			s.Metadata = metadata.String
		}
		s.CreatedAt = time.Unix(createdAt, 0)
		s.UpdatedAt = time.Unix(updatedAt, 0)
		shares = append(shares, &s)
	}
	return shares, nil
}

// DeleteShare removes a share by ID.
func DeleteShare(db *sql.DB, id string) error {
	res, err := db.Exec(`DELETE FROM collection_shares WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete share: %w", err)
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

// ShareLinkRow is the catalog representation of a share link.
type ShareLinkRow struct {
	ID           string     `json:"id"`
	CollectionID string     `json:"collection_id"`
	RelPath      string     `json:"rel_path"`
	TokenHash    string     `json:"-"` // never serialized
	PasswordHash string     `json:"-"` // never serialized
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	CreatedBy    string     `json:"created_by"`
	CreatedAt    time.Time  `json:"created_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
}

// generateShareLinkID returns a collision-proof share link ID.
// Format: link-<unix-nano>-<8 hex chars from crypto/rand>
// Uses only standard library (crypto/rand, encoding/hex).
func generateShareLinkID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("link-%d-%08x", now, counter)
	}
	return fmt.Sprintf("link-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// CreateShareLink inserts a share link for a file in a collection.
// token is the raw token (returned once to caller); only its SHA-256 is stored.
// password is optional; if provided, only its hash is stored.
// expiresAt is optional unix timestamp (seconds); nil = no expiry.
func CreateShareLink(db *sql.DB, collectionID, relPath, tokenHash, passwordHash string, expiresAt *int64, createdBy string) (*ShareLinkRow, error) {
	now := time.Now().Unix()
	id := generateShareLinkID()
	var expires interface{}
	if expiresAt != nil {
		expires = *expiresAt
	}
	_, err := db.Exec(`
		INSERT INTO share_links (id, collection_id, rel_path, token_hash, password_hash, expires_at, created_by, created_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)
	`, id, collectionID, relPath, tokenHash, passwordHash, expires, createdBy, now)
	if err != nil {
		return nil, fmt.Errorf("insert share link: %w", err)
	}
	return GetShareLinkByTokenHash(db, tokenHash)
}

// GetShareLinkByTokenHash retrieves a share link by its token hash.
// Returns nil if not found.
func GetShareLinkByTokenHash(db *sql.DB, tokenHash string) (*ShareLinkRow, error) {
	row := db.QueryRow(`SELECT id, collection_id, rel_path, token_hash, password_hash, expires_at, created_by, created_at, revoked_at FROM share_links WHERE token_hash = ?`, tokenHash)
	var sl ShareLinkRow
	var createdAt, expiresAt, revokedAt sql.NullInt64
	var passwordHash sql.NullString
	if err := row.Scan(&sl.ID, &sl.CollectionID, &sl.RelPath, &sl.TokenHash, &passwordHash, &expiresAt, &sl.CreatedBy, &createdAt, &revokedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan share link: %w", err)
	}
	if passwordHash.Valid {
		sl.PasswordHash = passwordHash.String
	}
	if expiresAt.Valid {
		t := time.Unix(expiresAt.Int64, 0)
		sl.ExpiresAt = &t
	}
	if revokedAt.Valid {
		t := time.Unix(revokedAt.Int64, 0)
		sl.RevokedAt = &t
	}
	sl.CreatedAt = time.Unix(createdAt.Int64, 0)
	return &sl, nil
}

// ListShareLinks returns all share links for a collection.
func ListShareLinks(db *sql.DB, collectionID string) ([]*ShareLinkRow, error) {
	rows, err := db.Query(`SELECT id, collection_id, rel_path, token_hash, password_hash, expires_at, created_by, created_at, revoked_at FROM share_links WHERE collection_id = ? ORDER BY created_at`, collectionID)
	if err != nil {
		return nil, fmt.Errorf("query share links: %w", err)
	}
	defer rows.Close()

	var links []*ShareLinkRow
	for rows.Next() {
		var sl ShareLinkRow
		var createdAt, expiresAt, revokedAt sql.NullInt64
		var passwordHash sql.NullString
		if err := rows.Scan(&sl.ID, &sl.CollectionID, &sl.RelPath, &sl.TokenHash, &passwordHash, &expiresAt, &sl.CreatedBy, &createdAt, &revokedAt); err != nil {
			return nil, fmt.Errorf("scan share link: %w", err)
		}
		if passwordHash.Valid {
			sl.PasswordHash = passwordHash.String
		}
		if expiresAt.Valid {
			t := time.Unix(expiresAt.Int64, 0)
			sl.ExpiresAt = &t
		}
		if revokedAt.Valid {
			t := time.Unix(revokedAt.Int64, 0)
			sl.RevokedAt = &t
		}
		sl.CreatedAt = time.Unix(createdAt.Int64, 0)
		links = append(links, &sl)
	}
	return links, nil
}

// RevokeShareLink marks a share link as revoked (sets revoked_at).
// Idempotent: returns nil if already revoked or not found.
func RevokeShareLink(db *sql.DB, id string) error {
	now := time.Now().Unix()
	res, err := db.Exec(`UPDATE share_links SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now, id)
	if err != nil {
		return fmt.Errorf("revoke share link: %w", err)
	}
	_, _ = res.RowsAffected() // idempotent: ignore affected count
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// AuditLogEntry represents a single audit log entry.
type AuditLogEntry struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	Actor      string    `json:"actor"`
	Action     string    `json:"action"`
	TargetType string    `json:"target_type"`
	TargetID   string    `json:"target_id"`
	Result     string    `json:"result"`
	Detail     string    `json:"detail,omitempty"`
}

// generateAuditID returns a collision-proof audit log ID.
// Format: audit-<unix-nano>-<8 hex chars from crypto/rand>
// Uses only standard library (crypto/rand, encoding/hex).
func generateAuditID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		// Fallback: use timestamp with atomic counter for additional entropy
		// This should never happen in practice, but ensures we never panic
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("audit-%d-%08x", now, counter)
	}
	return fmt.Sprintf("audit-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// RandRead reads random bytes into b. Exported for use by other packages.
func RandRead(b []byte) (int, error) {
	return rand.Read(b)
}

// HexEncode returns the hex encoding of b. Exported for use by other packages.
func HexEncode(b []byte) string {
	return hex.EncodeToString(b)
}

// WriteAuditLog writes an audit log entry. Best-effort: logs error but never returns it.
// NEVER write secrets into detail — no passwords, tokens, keys.
// TODO(FR-1.15): replace actor with authenticated principal.
// TODO(NFR-4): consider cryptographic tamper-evidence/signing chain for agent-immutability.
// TODO: log rotation/retention pruning.
func WriteAuditLog(db *sql.DB, actor, action, targetType, targetID, result, detail string) {
	id := generateAuditID()
	now := time.Now().Unix()
	_, err := db.Exec(`
		INSERT INTO audit_log (id, created_at, actor, action, target_type, target_id, result, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, id, now, actor, action, targetType, targetID, result, detail)
	if err != nil {
		// Best-effort: log the failure but never fail the calling operation
		// Using slog would require importing log/slog; keep it simple for now
		_ = err // explicitly ignored
	}
}

// ListAuditLog returns audit log entries with filtering and pagination.
// Newest first. Empty list [] never null.
func ListAuditLog(db *sql.DB, limit, offset int, action, targetType, result string) ([]*AuditLogEntry, int, error) {
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

	if action != "" {
		whereClauses = append(whereClauses, "action = ?")
		args = append(args, action)
	}
	if targetType != "" {
		whereClauses = append(whereClauses, "target_type = ?")
		args = append(args, targetType)
	}
	if result != "" {
		whereClauses = append(whereClauses, "result = ?")
		args = append(args, result)
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	// Count total
	countQuery := "SELECT COUNT(*) FROM audit_log " + whereSQL
	var total int
	err := db.QueryRow(countQuery, args...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count audit log: %w", err)
	}

	// Query entries
	query := fmt.Sprintf("SELECT id, created_at, actor, action, target_type, target_id, result, detail FROM audit_log %s ORDER BY created_at DESC, rowid DESC LIMIT ? OFFSET ?", whereSQL)
	args = append(args, limit, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query audit log: %w", err)
	}
	defer rows.Close()

	var entries []*AuditLogEntry
	for rows.Next() {
		var e AuditLogEntry
		var createdAt int64
		var detail sql.NullString
		if err := rows.Scan(&e.ID, &createdAt, &e.Actor, &e.Action, &e.TargetType, &e.TargetID, &e.Result, &detail); err != nil {
			return nil, 0, fmt.Errorf("scan audit log: %w", err)
		}
		e.CreatedAt = time.Unix(createdAt, 0)
		if detail.Valid {
			e.Detail = detail.String
		}
		entries = append(entries, &e)
	}

	if entries == nil {
		entries = []*AuditLogEntry{}
	}

	return entries, total, nil
}

// ============================================================================
// WS27 (2026-10-31): Heat catalog from access log (FR-1.3)
// ============================================================================

// AccessEventRow is the catalog representation of an access event.
type AccessEventRow struct {
	ID            string    `json:"id"`
	CollectionID  string    `json:"collection_id"`
	RelPath       string    `json:"rel_path"`
	Actor         string    `json:"actor"`
	AccessedAt    time.Time `json:"accessed_at"`
	Kind          string    `json:"kind"` // "preview", "content", "share"
}

// generateAccessEventID returns a collision-proof access event ID.
// Format: acc-<unix-nano>-<8 hex chars from crypto/rand>
// Uses only standard library (crypto/rand, encoding/hex).
func generateAccessEventID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("acc-%d-%08x", now, counter)
	}
	return fmt.Sprintf("acc-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// RecordAccessEvent records a successful file access for heat computation.
// Best-effort: logs error but never returns it (never fails the calling operation).
// kind must be one of: "preview", "content", "share".
// actor for public share access should be "share:<token-prefix-8>".
func RecordAccessEvent(db *sql.DB, collectionID, relPath, actor, kind string) {
	id := generateAccessEventID()
	now := time.Now().Unix()
	_, err := db.Exec(`
		INSERT INTO access_events (id, collection_id, rel_path, actor, accessed_at, kind)
		VALUES (?, ?, ?, ?, ?, ?)
	`, id, collectionID, relPath, actor, now, kind)
	if err != nil {
		// Best-effort: log the failure but never fail the calling operation
		_ = err // explicitly ignored
	}
}

// FileHeat returns the access count and last access time for a file within a time window.
// windowDays <= 0 means default 30 days.
// Returns (count, lastAccessUnix, error). lastAccessUnix is 0 if no accesses in window.
func FileHeat(db *sql.DB, collectionID, relPath string, windowDays int) (int, int64, error) {
	if windowDays <= 0 {
		windowDays = 30
	}
	cutoff := time.Now().AddDate(0, 0, -windowDays).Unix()

	var count int
	var lastAccess sql.NullInt64
	err := db.QueryRow(`
		SELECT COUNT(*), MAX(accessed_at)
		FROM access_events
		WHERE collection_id = ? AND rel_path = ? AND accessed_at >= ?
	`, collectionID, relPath, cutoff).Scan(&count, &lastAccess)
	if err != nil {
		return 0, 0, fmt.Errorf("query file heat: %w", err)
	}
	var lastAccessUnix int64
	if lastAccess.Valid {
		lastAccessUnix = lastAccess.Int64
	}
	return count, lastAccessUnix, nil
}

// HeatedFileEntry represents a file with its heat metrics.
type HeatedFileEntry struct {
	RelPath     string `json:"rel_path"`
	Count       int    `json:"count"`
	LastAccess  int64  `json:"last_access"` // unix timestamp
}

// TopHeated returns the top heated files in a collection within the last 30 days.
// limit <= 0 means default 50; limit > 500 is clamped to 500.
// Returns (entries, total, error). Entries sorted by count DESC, then last_access DESC.
func TopHeated(db *sql.DB, collectionID string, limit int) ([]*HeatedFileEntry, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	cutoff := time.Now().AddDate(0, 0, -30).Unix()

	// Count total distinct files with accesses in window
	var total int
	err := db.QueryRow(`
		SELECT COUNT(DISTINCT rel_path)
		FROM access_events
		WHERE collection_id = ? AND accessed_at >= ?
	`, collectionID, cutoff).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count top heated: %w", err)
	}

	// Query top files
	rows, err := db.Query(`
		SELECT rel_path, COUNT(*) as cnt, MAX(accessed_at) as last_acc
		FROM access_events
		WHERE collection_id = ? AND accessed_at >= ?
		GROUP BY rel_path
		ORDER BY cnt DESC, last_acc DESC
		LIMIT ?
	`, collectionID, cutoff, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("query top heated: %w", err)
	}
	defer rows.Close()

	var entries []*HeatedFileEntry
	for rows.Next() {
		var e HeatedFileEntry
		var lastAcc sql.NullInt64
		if err := rows.Scan(&e.RelPath, &e.Count, &lastAcc); err != nil {
			return nil, 0, fmt.Errorf("scan heated file: %w", err)
		}
		if lastAcc.Valid {
			e.LastAccess = lastAcc.Int64
		}
		entries = append(entries, &e)
	}

	if entries == nil {
		entries = []*HeatedFileEntry{}
	}
	return entries, total, nil
}

// FileRow is the catalog representation of a file record.
type FileRow struct {
	ID                string    `json:"id"`
	CollectionID      string    `json:"collection_id"`
	RelPath           string    `json:"rel_path"`
	SizeBytes         int64     `json:"size_bytes"`
	ModTimeUnix       int64     `json:"modtime_unix"`
	DiskName          string    `json:"disk_name"`
	Checksum          string    `json:"checksum,omitempty"`
	ChecksumVerifiedAt int64    `json:"checksum_verified_at,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// ScanResult holds the counts from a collection scan.
type ScanResult struct {
	Added   int `json:"added"`
	Updated int `json:"updated"`
	Removed int `json:"removed"`
}

// generateFileID returns a collision-proof file ID.
// Format: file-<unix-nano>-<8 hex chars from crypto/rand>
// Uses only standard library (crypto/rand, encoding/hex).
func generateFileID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("file-%d-%08x", now, counter)
	}
	return fmt.Sprintf("file-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// IndexCollectionFiles scans the collection root and upserts file records.
// Returns counts of added, updated, removed records.
// Safety: never follows symlinked directories; never records paths outside root;
// if root does not exist, returns error and writes NO records.
// If actor is provided (optional), invalidates shadows for updated/removed files (WS35 FR-2.5).
func IndexCollectionFiles(db *sql.DB, collection *CollectionRow, pool *PoolRow, actor ...string) (ScanResult, error) {
	actorStr := ""
	if len(actor) > 0 {
		actorStr = actor[0]
	}
	// Resolve scan root: pool.Mountpoint or fallback /mnt/<pool-name>
	mountpoint := pool.Mountpoint
	if mountpoint == "" {
		mountpoint = "/mnt/" + pool.Name
	}
	root := filepath.Join(mountpoint, collection.Path)
	root = filepath.Clean(root)

	// Verify root exists and is a directory
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return ScanResult{}, fmt.Errorf("collection root does not exist: %s", root)
		}
		return ScanResult{}, fmt.Errorf("stat collection root: %w", err)
	}
	if !info.IsDir() {
		return ScanResult{}, fmt.Errorf("collection root is not a directory: %s", root)
	}

	// Walk the tree and collect files
	seen := make(map[string]FileInfo)
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Skip directories we can't read
			return filepath.SkipDir
		}
		if d.IsDir() {
			// Check if this directory is a symlink - skip it
			if d.Type()&os.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			return nil
		}
		// Regular file
		info, err := d.Info()
		if err != nil {
			return nil // skip unreadable files
		}
		// Skip symlinks
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		// Get relative path
		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		// Safety: reject paths escaping the root
		if strings.HasPrefix(relPath, "..") {
			return nil
		}
		// Normalize to slash-separated
		relPath = filepath.ToSlash(relPath)
		// Skip any path segment named .kelvios-trash (recycle bin area)
		if strings.Contains(relPath, "/.kelvios-trash/") || strings.HasPrefix(relPath, ".kelvios-trash/") || relPath == ".kelvios-trash" {
			return nil
		}
		seen[relPath] = FileInfo{
			Size:    info.Size(),
			ModTime: info.ModTime().Unix(),
		}
		return nil
	})
	if err != nil {
		return ScanResult{}, fmt.Errorf("walk collection root: %w", err)
	}

	// Get existing records for this collection
	existingRows, err := db.Query(`SELECT id, rel_path, size_bytes, modtime_unix FROM files WHERE collection_id = ?`, collection.ID)
	if err != nil {
		return ScanResult{}, fmt.Errorf("query existing files: %w", err)
	}
	defer existingRows.Close()

	existing := make(map[string]struct {
		id      string
		size    int64
		modtime int64
	})
	for existingRows.Next() {
		var id, relPath string
		var size, modtime int64
		if err := existingRows.Scan(&id, &relPath, &size, &modtime); err != nil {
			return ScanResult{}, fmt.Errorf("scan existing file: %w", err)
		}
		existing[relPath] = struct {
			id      string
			size    int64
			modtime int64
		}{id, size, modtime}
	}

	// Determine changes
	var added, updated, removed int
	var updatedPaths, removedPaths []string
	now := time.Now().Unix()
	tx, err := db.Begin()
	if err != nil {
		return ScanResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Insert or update
	for relPath, info := range seen {
		if ex, ok := existing[relPath]; ok {
			// Check if changed
			if ex.size != info.Size || ex.modtime != info.ModTime {
				_, err = tx.Exec(`UPDATE files SET size_bytes = ?, modtime_unix = ?, disk_name = ?, updated_at = ? WHERE id = ?`,
					info.Size, info.ModTime, collection.DiskName, now, ex.id)
				if err != nil {
					return ScanResult{}, fmt.Errorf("update file: %w", err)
				}
				updated++
				updatedPaths = append(updatedPaths, relPath)
			}
			delete(existing, relPath)
		} else {
			// New file
			id := generateFileID()
			_, err = tx.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, checksum, checksum_verified_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, '', 0, ?, ?)`,
				id, collection.ID, relPath, info.Size, info.ModTime, collection.DiskName, now, now)
			if err != nil {
				return ScanResult{}, fmt.Errorf("insert file: %w", err)
			}
			added++
		}
	}

	// Delete removed files
	for relPath, ex := range existing {
		_, err = tx.Exec(`DELETE FROM files WHERE id = ?`, ex.id)
		if err != nil {
			return ScanResult{}, fmt.Errorf("delete file: %w", err)
		}
		removed++
		removedPaths = append(removedPaths, relPath)
	}

	if err := tx.Commit(); err != nil {
		return ScanResult{}, fmt.Errorf("commit tx: %w", err)
	}

	// Invalidate shadows for updated and removed files (WS35 FR-2.5)
	if actorStr != "" {
		for _, relPath := range updatedPaths {
			_ = InvalidateShadow(db, collection.ID, relPath, actorStr)
		}
		for _, relPath := range removedPaths {
			_ = InvalidateShadow(db, collection.ID, relPath, actorStr)
		}
	}

	return ScanResult{Added: added, Updated: updated, Removed: removed}, nil
}

// BrowseEntry represents a file or directory entry for browsing.
type BrowseEntry struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // "file" or "dir"
	RelPath   string `json:"rel_path"`
	SizeBytes int64  `json:"size_bytes,omitempty"` // only for files
	Offline   bool   `json:"offline"`              // true when pool tier is cold
	Shadowed  bool   `json:"shadowed"`             // true when a Warm shadow replica exists
	Dirty     bool   `json:"dirty"`                // true when shadow has uncommitted writes (WS36 FR-2.6)
}

// BrowseFiles returns entries directly under the given prefix (relative to collection root).
// prefix is slash-separated, no leading slash. Empty prefix = root.
// Returns files immediately under prefix + subdirectory names derived from deeper paths.
// limit/offset for pagination (0 = no limit).
func BrowseFiles(db *sql.DB, collectionID, prefix string, limit, offset int) ([]BrowseEntry, error) {
	// Normalize prefix: ensure no leading/trailing slash, empty = root
	prefix = strings.Trim(prefix, "/")

	// Query files under this collection
	var rows *sql.Rows
	var err error
	if prefix == "" {
		rows, err = db.Query(`
			SELECT rel_path, size_bytes, modtime_unix, disk_name
			FROM files
			WHERE collection_id = ?
			ORDER BY rel_path
		`, collectionID)
	} else {
		// Files directly under prefix: rel_path starts with prefix/ and has no more slashes after
		// OR subdirectories: rel_path starts with prefix/ and has more slashes
		rows, err = db.Query(`
			SELECT rel_path, size_bytes, modtime_unix, disk_name
			FROM files
			WHERE collection_id = ? AND rel_path LIKE ?
			ORDER BY rel_path
		`, collectionID, prefix+"/%")
	}
	if err != nil {
		return nil, fmt.Errorf("query files: %w", err)
	}
	defer rows.Close()

	// Collect all matching rel_paths
	type fileRecord struct {
		relPath  string
		size     int64
		modtime  int64
		diskName string
	}
	var allFiles []fileRecord
	for rows.Next() {
		var r fileRecord
		if err := rows.Scan(&r.relPath, &r.size, &r.modtime, &r.diskName); err != nil {
			return nil, fmt.Errorf("scan file: %w", err)
		}
		allFiles = append(allFiles, r)
	}
	// Release the files cursor before the dirs query below: a second
	// query while this cursor is open can land on another connection.
	_ = rows.Close()

	// Build entries: files directly under prefix + unique subdirectory names
	entriesMap := make(map[string]BrowseEntry) // key = name, to deduplicate dirs

	for _, f := range allFiles {
		// Get the part after prefix/
		var suffix string
		if prefix == "" {
			suffix = f.relPath
		} else {
			suffix = strings.TrimPrefix(f.relPath, prefix+"/")
		}
		if suffix == "" {
			continue // shouldn't happen
		}

		// Find first path segment
		parts := strings.SplitN(suffix, "/", 2)
		name := parts[0]

		if len(parts) == 1 {
			// This is a file directly under prefix
			entriesMap[name] = BrowseEntry{
				Name:      name,
				Kind:      "file",
				RelPath:   f.relPath,
				SizeBytes: f.size,
				Offline:   false, // will be set by caller based on pool tier
			}
		} else {
			// This is a file deeper down -> implies a subdirectory
			if _, exists := entriesMap[name]; !exists {
				entriesMap[name] = BrowseEntry{
					Name:    name,
					Kind:    "dir",
					RelPath: prefix + "/" + name,
					Offline: false,
				}
			}
		}
	}

	// Merge recorded directories (WS15): mkdir stores a dirs row so an
	// EMPTY directory is visible in browse. A recorded dir directly under
	// prefix appears as a dir entry; a deeper recorded dir implies its
	// first segment under prefix is a dir entry. File entries and
	// file-derived dir entries already in the map win.
	dirRows, derr := db.Query(`SELECT rel_path FROM dirs WHERE collection_id = ? ORDER BY rel_path`, collectionID)
	if derr == nil {
		for dirRows.Next() {
			var d string
			if err := dirRows.Scan(&d); err != nil {
				continue
			}
			var suffix string
			switch {
			case prefix == "":
				suffix = d
			case d == prefix:
				continue
			case strings.HasPrefix(d, prefix+"/"):
				suffix = strings.TrimPrefix(d, prefix+"/")
			default:
				continue
			}
			if suffix == "" {
				continue
			}
			name := strings.SplitN(suffix, "/", 2)[0]
			rel := name
			if prefix != "" {
				rel = prefix + "/" + name
			}
			if _, exists := entriesMap[name]; !exists {
				entriesMap[name] = BrowseEntry{
					Name:    name,
					Kind:    "dir",
					RelPath: rel,
					Offline: false,
				}
			}
		}
		_ = dirRows.Close()
	}

	// Convert map to sorted slice
	names := make([]string, 0, len(entriesMap))
	for n := range entriesMap {
		names = append(names, n)
	}
	sort.Strings(names)

	entries := make([]BrowseEntry, 0, len(names))
	for _, n := range names {
		entries = append(entries, entriesMap[n])
	}

	// Apply pagination
	if offset > 0 && offset < len(entries) {
		entries = entries[offset:]
	} else if offset >= len(entries) {
		entries = []BrowseEntry{}
	}
	if limit > 0 && limit < len(entries) {
		entries = entries[:limit]
	}

	return entries, nil
}

// FileInfo holds file metadata during scan.
type FileInfo struct {
	Size    int64
	ModTime int64
}

var idFallbackCounter uint64

// GetFile retrieves a file record by collection ID and relative path.
func GetFile(db *sql.DB, collectionID, relPath string) (*FileRow, error) {
	row := db.QueryRow(`SELECT id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, checksum, checksum_verified_at, created_at, updated_at FROM files WHERE collection_id = ? AND rel_path = ?`, collectionID, relPath)
	var f FileRow
	var createdAt, updatedAt int64
	var checksum, checksumVerifiedAt sql.NullString
	if err := row.Scan(&f.ID, &f.CollectionID, &f.RelPath, &f.SizeBytes, &f.ModTimeUnix, &f.DiskName, &checksum, &checksumVerifiedAt, &createdAt, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan file: %w", err)
	}
	if checksum.Valid {
		f.Checksum = checksum.String
	}
	if checksumVerifiedAt.Valid {
		if v, err := strconv.ParseInt(checksumVerifiedAt.String, 10, 64); err == nil {
			f.ChecksumVerifiedAt = v
		}
	}
	f.CreatedAt = time.Unix(createdAt, 0)
	f.UpdatedAt = time.Unix(updatedAt, 0)
	return &f, nil
}

// ResolveCollectionRoot returns the absolute filesystem root for a collection.
// Returns the root path, or an error if the pool mountpoint is not set and
// the fallback cannot be determined.
func ResolveCollectionRoot(pool *PoolRow, collection *CollectionRow) (string, error) {
	mountpoint := pool.Mountpoint
	if mountpoint == "" {
		mountpoint = "/mnt/" + pool.Name
	}
	root := filepath.Join(mountpoint, collection.Path)
	return filepath.Clean(root), nil
}

// UpsertFile creates or updates a file record in the catalog.
// Returns the file ID (new or existing).
func UpsertFile(db *sql.DB, collectionID, relPath, diskName string, sizeBytes int64, modTimeUnix int64) (string, error) {
	now := time.Now().Unix()
	// Check if file already exists
	var existingID string
	err := db.QueryRow(`SELECT id FROM files WHERE collection_id = ? AND rel_path = ?`, collectionID, relPath).Scan(&existingID)
	if err != nil && err != sql.ErrNoRows {
		return "", fmt.Errorf("check existing file: %w", err)
	}
	if existingID != "" {
		// Update existing
		_, err = db.Exec(`UPDATE files SET size_bytes = ?, modtime_unix = ?, disk_name = ?, updated_at = ? WHERE id = ?`,
			sizeBytes, modTimeUnix, diskName, now, existingID)
		if err != nil {
			return "", fmt.Errorf("update file: %w", err)
		}
		return existingID, nil
	}
	// Insert new
	id := generateFileID()
	_, err = db.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, checksum, checksum_verified_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, '', 0, ?, ?)`,
		id, collectionID, relPath, sizeBytes, modTimeUnix, diskName, now, now)
	if err != nil {
		return "", fmt.Errorf("insert file: %w", err)
	}
	return id, nil
}

// DeleteFile removes a file record from the catalog.
// Returns true if a record was deleted, false if not found.
func DeleteFile(db *sql.DB, collectionID, relPath string) (bool, error) {
	res, err := db.Exec(`DELETE FROM files WHERE collection_id = ? AND rel_path = ?`, collectionID, relPath)
	if err != nil {
		return false, fmt.Errorf("delete file: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return affected > 0, nil
}

// MoveFile updates the rel_path of a file record in the catalog.
// Returns error if source not found or destination already exists.
func MoveFile(db *sql.DB, collectionID, fromRelPath, toRelPath string) error {
	// Check if destination already exists
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE collection_id = ? AND rel_path = ?`, collectionID, toRelPath).Scan(&count)
	if err != nil {
		return fmt.Errorf("check destination: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("destination already exists")
	}
	// Update source to new path
	res, err := db.Exec(`UPDATE files SET rel_path = ?, updated_at = ? WHERE collection_id = ? AND rel_path = ?`,
		toRelPath, time.Now().Unix(), collectionID, fromRelPath)
	if err != nil {
		return fmt.Errorf("move file: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("source not found")
	}
	return nil
}

// CopyFile creates a new file record with the same metadata but different path.
// Returns the new file ID.
func CopyFile(db *sql.DB, collectionID, fromRelPath, toRelPath, diskName string) (string, error) {
	// Get source file
	source, err := GetFile(db, collectionID, fromRelPath)
	if err != nil {
		return "", fmt.Errorf("get source file: %w", err)
	}
	if source == nil {
		return "", fmt.Errorf("source not found")
	}
	// Check if destination already exists
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM files WHERE collection_id = ? AND rel_path = ?`, collectionID, toRelPath).Scan(&count)
	if err != nil {
		return "", fmt.Errorf("check destination: %w", err)
	}
	if count > 0 {
		return "", fmt.Errorf("destination already exists")
	}
	// Insert new record with same metadata but new path
	id := generateFileID()
	now := time.Now().Unix()
	_, err = db.Exec(`INSERT INTO files (id, collection_id, rel_path, size_bytes, modtime_unix, disk_name, checksum, checksum_verified_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, '', 0, ?, ?)`,
		id, collectionID, toRelPath, source.SizeBytes, source.ModTimeUnix, diskName, now, now)
	if err != nil {
		return "", fmt.Errorf("insert copied file: %w", err)
	}
	return id, nil
}

// EnsureDir creates a directory entry in the catalog by ensuring parent directories exist.
// For browse to show empty directories, we don't store directory entries in the files table.
// This function is a no-op for the catalog but validates the path.
func EnsureDir(db *sql.DB, collectionID, relPath string) error {
	// Directories are not stored in the files table; browse derives them
	// from file paths and merges recorded dirs (see RecordDir, v9).
	// This function only validates that the path is safe.
	if strings.HasPrefix(relPath, "..") || strings.Contains(relPath, "../") {
		return fmt.Errorf("path escape not allowed")
	}
	return nil
}

// RecordDir records a directory created via the API so BrowseFiles can
// show it even while it is empty (WS15). Idempotent per (collection, path).
func RecordDir(db *sql.DB, collectionID, relPath string) error {
	now := time.Now().Unix()
	_, err := db.Exec(`INSERT OR IGNORE INTO dirs (id, collection_id, rel_path, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		generateFileID(), collectionID, relPath, now, now)
	if err != nil {
		return fmt.Errorf("record dir: %w", err)
	}
	return nil
}

// DeleteDirRecord removes a recorded directory. Returns true when a
// record was removed, false when none existed.
func DeleteDirRecord(db *sql.DB, collectionID, relPath string) (bool, error) {
	res, err := db.Exec(`DELETE FROM dirs WHERE collection_id = ? AND rel_path = ?`, collectionID, relPath)
	if err != nil {
		return false, fmt.Errorf("delete dir record: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return affected > 0, nil
}

// SupplychainEntryRow is the catalog representation of a supply-chain entry.
// Mirrors supplychain.Entry with catalog-specific fields.
type SupplychainEntryRow struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Publisher           string    `json:"publisher"`
	SourceRepo          string    `json:"source_repo"`
	ImageRef            string    `json:"image_ref"`
	PinnedDigest        string    `json:"pinned_digest"`
	SignatureKind       string    `json:"signature_kind"`
	SourceKind          string    `json:"source_kind"`
	UnverifiedConfirmed bool      `json:"unverified_confirmed"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// generateSupplychainEntryID returns a collision-proof supplychain entry ID.
// Format: sce-<unix-nano>-<8 hex chars from crypto/rand>
// Uses only standard library (crypto/rand, encoding/hex).
func generateSupplychainEntryID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("sce-%d-%08x", now, counter)
	}
	return fmt.Sprintf("sce-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// CreateSupplychainEntry inserts a supply-chain entry into the catalog.
// Validates with supplychain.ValidateEntry FIRST (never persist an invalid entry).
// Duplicate name returns a distinct error that the HTTP layer maps to 409.
func CreateSupplychainEntry(db *sql.DB, entry *SupplychainEntryRow) error {
	// Validate with supplychain core first
	scEntry := supplychain.Entry{
		Name:                entry.Name,
		Publisher:           entry.Publisher,
		SourceRepo:          entry.SourceRepo,
		ImageRef:            entry.ImageRef,
		PinnedDigest:        entry.PinnedDigest,
		SignatureKind:       supplychain.SignatureKind(entry.SignatureKind),
		SourceKind:          supplychain.SourceKind(entry.SourceKind),
		UnverifiedConfirmed: entry.UnverifiedConfirmed,
	}
	if err := supplychain.ValidateEntry(scEntry); err != nil {
		return err
	}

	now := time.Now().Unix()
	entry.ID = generateSupplychainEntryID()
	entry.CreatedAt = time.Unix(now, 0)
	entry.UpdatedAt = time.Unix(now, 0)

	_, err := db.Exec(`
		INSERT INTO supplychain_entries (id, name, publisher, source_repo, image_ref, pinned_digest, signature_kind, source_kind, unverified_confirmed, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, entry.ID, entry.Name, entry.Publisher, entry.SourceRepo, entry.ImageRef, entry.PinnedDigest, entry.SignatureKind, entry.SourceKind, boolToInt(entry.UnverifiedConfirmed), now, now)
	if err != nil {
		// Check for unique constraint violation on name
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "unique") {
			return fmt.Errorf("duplicate name")
		}
		return fmt.Errorf("insert supplychain entry: %w", err)
	}
	return nil
}

// ListSupplychainEntries returns all supply-chain entries.
// Returns [] (never null) when empty.
func ListSupplychainEntries(db *sql.DB) ([]*SupplychainEntryRow, error) {
	rows, err := db.Query(`SELECT id, name, publisher, source_repo, image_ref, pinned_digest, signature_kind, source_kind, unverified_confirmed, created_at, updated_at FROM supplychain_entries ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("query supplychain entries: %w", err)
	}
	defer rows.Close()

	var entries []*SupplychainEntryRow
	for rows.Next() {
		var e SupplychainEntryRow
		var createdAt, updatedAt int64
		var unverifiedConfirmed int
		if err := rows.Scan(&e.ID, &e.Name, &e.Publisher, &e.SourceRepo, &e.ImageRef, &e.PinnedDigest, &e.SignatureKind, &e.SourceKind, &unverifiedConfirmed, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan supplychain entry: %w", err)
		}
		e.UnverifiedConfirmed = unverifiedConfirmed != 0
		e.CreatedAt = time.Unix(createdAt, 0)
		e.UpdatedAt = time.Unix(updatedAt, 0)
		entries = append(entries, &e)
	}
	if entries == nil {
		entries = []*SupplychainEntryRow{}
	}
	return entries, nil
}

// GetSupplychainEntry retrieves a supply-chain entry by ID.
// Returns nil if not found (sql.ErrNoRows -> nil).
func GetSupplychainEntry(db *sql.DB, id string) (*SupplychainEntryRow, error) {
	row := db.QueryRow(`SELECT id, name, publisher, source_repo, image_ref, pinned_digest, signature_kind, source_kind, unverified_confirmed, created_at, updated_at FROM supplychain_entries WHERE id = ?`, id)
	var e SupplychainEntryRow
	var createdAt, updatedAt int64
	var unverifiedConfirmed int
	if err := row.Scan(&e.ID, &e.Name, &e.Publisher, &e.SourceRepo, &e.ImageRef, &e.PinnedDigest, &e.SignatureKind, &e.SourceKind, &unverifiedConfirmed, &createdAt, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan supplychain entry: %w", err)
	}
	e.UnverifiedConfirmed = unverifiedConfirmed != 0
	e.CreatedAt = time.Unix(createdAt, 0)
	e.UpdatedAt = time.Unix(updatedAt, 0)
	return &e, nil
}

// DeleteSupplychainEntry removes a supply-chain entry by ID.
// Deleting a missing id returns sql.ErrNoRows (HTTP 404).
func DeleteSupplychainEntry(db *sql.DB, id string) error {
	res, err := db.Exec(`DELETE FROM supplychain_entries WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete supplychain entry: %w", err)
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

// ============================================================================
// WS19: UPS events persistence (FR-1.12)
// ============================================================================

// UPSEventRow is the catalog representation of a UPS event.
// Mirrors ups.Decision/Reading with catalog-specific fields.
type UPSEventRow struct {
	ID            string    `json:"id"`
	TS            time.Time `json:"ts"`
	Kind          string    `json:"kind"`
	State         string    `json:"state"`
	ChargePercent int       `json:"charge_percent"`
	Detail        string    `json:"detail"`
}

// generateUPSEventID returns a collision-proof UPS event ID.
// Format: upse-<unix-nano>-<8 hex chars from crypto/rand>
// Uses only standard library (crypto/rand, encoding/hex).
func generateUPSEventID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("upse-%d-%08x", now, counter)
	}
	return fmt.Sprintf("upse-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// InsertUPSEvent inserts a UPS event into the catalog.
func InsertUPSEvent(db *sql.DB, e *UPSEventRow) error {
	now := time.Now().Unix()
	if e.ID == "" {
		e.ID = generateUPSEventID()
	}
	if e.TS.IsZero() {
		e.TS = time.Unix(now, 0)
	}
	_, err := db.Exec(`
		INSERT INTO ups_events (id, ts, kind, state, charge_percent, detail)
		VALUES (?, ?, ?, ?, ?, ?)
	`, e.ID, e.TS.Unix(), e.Kind, e.State, e.ChargePercent, e.Detail)
	if err != nil {
		return fmt.Errorf("insert ups event: %w", err)
	}
	return nil
}

// ListUPSEvents returns UPS events with pagination.
// Newest first. Empty list [] never null.
func ListUPSEvents(db *sql.DB, limit, offset int) ([]*UPSEventRow, int, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	// Count total
	var total int
	err := db.QueryRow(`SELECT COUNT(*) FROM ups_events`).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count ups events: %w", err)
	}

	// Query entries
	rows, err := db.Query(`SELECT id, ts, kind, state, charge_percent, detail FROM ups_events ORDER BY ts DESC, rowid DESC LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query ups events: %w", err)
	}
	defer rows.Close()

	var entries []*UPSEventRow
	for rows.Next() {
		var e UPSEventRow
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.Kind, &e.State, &e.ChargePercent, &e.Detail); err != nil {
			return nil, 0, fmt.Errorf("scan ups event: %w", err)
		}
		e.TS = time.Unix(ts, 0)
		entries = append(entries, &e)
	}

	if entries == nil {
		entries = []*UPSEventRow{}
	}

	return entries, total, nil
}

// IntegrityCheck runs PRAGMA integrity_check on the database.
// Returns "ok" if the check passes, otherwise returns the error detail.
func IntegrityCheck(db *sql.DB) (string, error) {
	row := db.QueryRow(`PRAGMA integrity_check`)
	var result string
	if err := row.Scan(&result); err != nil {
		return "", fmt.Errorf("integrity_check query failed: %w", err)
	}
	if result != "ok" {
		return result, nil
	}
	return "ok", nil
}

// ============================================================================
// WS20 LANGKAH-B (2026-11-02): Import wiring (FR-1.10)
// ============================================================================

// ImportRow is the catalog representation of an import completion report.
// Mirrors the importer.Report with catalog-specific fields.
type ImportRow struct {
	ID           string    `json:"id"`
	CollectionID string    `json:"collection_id"`
	SourcePath   string    `json:"source_path"`
	FSType       string    `json:"fs_type"`
	TotalFiles   int       `json:"total_files"`
	TotalBytes   int64     `json:"total_bytes"`
	Copied       int       `json:"copied"`
	Failed       int       `json:"failed"`
	BytesCopied  int64     `json:"bytes_copied"`
	TS           time.Time `json:"ts"`
}

// generateImportID returns a collision-proof import ID.
// Format: imp-<unix-nano>-<8 hex chars from crypto/rand>
// Uses only standard library (crypto/rand, encoding/hex).
func generateImportID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("imp-%d-%08x", now, counter)
	}
	return fmt.Sprintf("imp-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// InsertImport inserts an import completion report into the catalog.
func InsertImport(db *sql.DB, rec *ImportRow) error {
	now := time.Now().Unix()
	if rec.ID == "" {
		rec.ID = generateImportID()
	}
	if rec.TS.IsZero() {
		rec.TS = time.Unix(now, 0)
	}
	_, err := db.Exec(`
		INSERT INTO imports (id, collection_id, source_path, fs_type, total_files, total_bytes, copied, failed, bytes_copied, ts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, rec.ID, rec.CollectionID, rec.SourcePath, rec.FSType, rec.TotalFiles, rec.TotalBytes, rec.Copied, rec.Failed, rec.BytesCopied, rec.TS.Unix())
	if err != nil {
		return fmt.Errorf("insert import: %w", err)
	}
	return nil
}

// ListImports returns import reports for a collection with pagination.
// Newest first. Empty list [] never null.
func ListImports(db *sql.DB, collectionID string, limit, offset int) ([]*ImportRow, int, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	// Count total for this collection
	var total int
	err := db.QueryRow(`SELECT COUNT(*) FROM imports WHERE collection_id = ?`, collectionID).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count imports: %w", err)
	}

	// Query entries
	rows, err := db.Query(`SELECT id, collection_id, source_path, fs_type, total_files, total_bytes, copied, failed, bytes_copied, ts FROM imports WHERE collection_id = ? ORDER BY ts DESC, rowid DESC LIMIT ? OFFSET ?`, collectionID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query imports: %w", err)
	}
	defer rows.Close()

	var entries []*ImportRow
	for rows.Next() {
		var e ImportRow
		var ts int64
		if err := rows.Scan(&e.ID, &e.CollectionID, &e.SourcePath, &e.FSType, &e.TotalFiles, &e.TotalBytes, &e.Copied, &e.Failed, &e.BytesCopied, &ts); err != nil {
			return nil, 0, fmt.Errorf("scan import: %w", err)
		}
		e.TS = time.Unix(ts, 0)
		entries = append(entries, &e)
	}

	if entries == nil {
		entries = []*ImportRow{}
	}

	return entries, total, nil
}

// ============================================================================
// WS21 LANGKAH-B (2026-10-30): Disk operations catalog (FR-1.11)
// ============================================================================

// DiskOpRow is the catalog representation of a disk operation (replace/expand plan).
type DiskOpRow struct {
	ID              string    `json:"id"`
	PoolID          string    `json:"pool_id"`
	Kind            string    `json:"kind"` // "replace" | "expand"
	Strategy        string    `json:"strategy"`
	FailingDisk     string    `json:"failing_disk,omitempty"`
	ReplacementDisk string    `json:"replacement_disk,omitempty"`
	Status          string    `json:"status"`           // "planned" | "approved" | "done" | "failed"
	Detail          string    `json:"detail,omitempty"` // JSON summary (counts/strategy only)
	CreatedAt       time.Time `json:"created_at"`
}

// generateDiskOpID returns a collision-proof disk operation ID.
// Format: dop-<unix-nano>-<8 hex chars from crypto/rand>
// Uses only standard library (crypto/rand, encoding/hex).
func generateDiskOpID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("dop-%d-%08x", now, counter)
	}
	return fmt.Sprintf("dop-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// InsertDiskOp inserts a disk operation plan into the catalog.
func InsertDiskOp(db *sql.DB, rec *DiskOpRow) error {
	now := time.Now().Unix()
	if rec.ID == "" {
		rec.ID = generateDiskOpID()
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Unix(now, 0)
	}
	if rec.Status == "" {
		rec.Status = "planned"
	}
	_, err := db.Exec(`
		INSERT INTO disk_operations (id, pool_id, kind, strategy, failing_disk, replacement_disk, status, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, rec.ID, rec.PoolID, rec.Kind, rec.Strategy, rec.FailingDisk, rec.ReplacementDisk, rec.Status, rec.Detail, rec.CreatedAt.Unix())
	if err != nil {
		return fmt.Errorf("insert disk operation: %w", err)
	}
	return nil
}

// ListDiskOps returns disk operations for a pool with pagination.
// Newest first (created_at DESC, rowid DESC). Empty list [] never null.
// limit clamp: default 100, max 1000.
func ListDiskOps(db *sql.DB, poolID string, limit, offset int) ([]*DiskOpRow, int, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	// Count total for this pool
	var total int
	err := db.QueryRow(`SELECT COUNT(*) FROM disk_operations WHERE pool_id = ?`, poolID).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count disk operations: %w", err)
	}

	// Query entries
	rows, err := db.Query(`SELECT id, pool_id, kind, strategy, failing_disk, replacement_disk, status, detail, created_at FROM disk_operations WHERE pool_id = ? ORDER BY created_at DESC, rowid DESC LIMIT ? OFFSET ?`, poolID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query disk operations: %w", err)
	}
	defer rows.Close()

	var entries []*DiskOpRow
	for rows.Next() {
		var e DiskOpRow
		var ts int64
		var failingDisk, replacementDisk, detail sql.NullString
		if err := rows.Scan(&e.ID, &e.PoolID, &e.Kind, &e.Strategy, &failingDisk, &replacementDisk, &e.Status, &detail, &ts); err != nil {
			return nil, 0, fmt.Errorf("scan disk operation: %w", err)
		}
		if failingDisk.Valid {
			e.FailingDisk = failingDisk.String
		}
		if replacementDisk.Valid {
			e.ReplacementDisk = replacementDisk.String
		}
		if detail.Valid {
			e.Detail = detail.String
		}
		e.CreatedAt = time.Unix(ts, 0)
		entries = append(entries, &e)
	}

	if entries == nil {
		entries = []*DiskOpRow{}
	}

	return entries, total, nil
}

// ============================================================================
// WS22 STEP A (2026-10-27): Per-collection recycle bin (FR-2.10)
// ============================================================================

// TrashItemRow is the catalog representation of a trash item.
// Mirrors the trash item with catalog-specific fields.
type TrashItemRow struct {
	ID              string    `json:"id"`
	CollectionID    string    `json:"collection_id"`
	OriginalRelPath string    `json:"original_rel_path"`
	TrashRelPath    string    `json:"trash_rel_path"`
	Kind            string    `json:"kind"` // "file" only in Step A
	SizeBytes       int64     `json:"size_bytes"`
	DiskName        string    `json:"disk_name"`
	DeletedAt       time.Time `json:"deleted_at"`
	RetentionDays   int       `json:"retention_days"`
	Actor           string    `json:"actor"`
}

// generateTrashID returns a collision-proof trash item ID.
// Format: trash-<unix-nano>-<8 hex chars from crypto/rand>
// Uses only standard library (crypto/rand, encoding/hex).
// If crypto/rand fails, falls back to atomic counter (never plain unix-seconds).
func generateTrashID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("trash-%d-%08x", now, counter)
	}
	return fmt.Sprintf("trash-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// RecordTrash inserts a trash item record into the catalog.
// If id is empty, a new collision-proof ID is generated.
// Returns the trash item ID.
func RecordTrash(db *sql.DB, id, collectionID, originalRelPath, trashRelPath, kind, diskName string, sizeBytes int64, retentionDays int, actor string) (string, error) {
	now := time.Now().Unix()
	if id == "" {
		id = generateTrashID()
	}
	if retentionDays <= 0 {
		retentionDays = 30
	}
	_, err := db.Exec(`
		INSERT INTO trash_items (id, collection_id, original_rel_path, trash_rel_path, kind, size_bytes, disk_name, deleted_at, retention_days, actor)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, collectionID, originalRelPath, trashRelPath, kind, sizeBytes, diskName, now, retentionDays, actor)
	if err != nil {
		return "", fmt.Errorf("insert trash item: %w", err)
	}
	return id, nil
}

// ListTrash returns trash items for a collection, newest first.
// limit clamp: default 100, max 1000 (same shape as ListAuditLog).
func ListTrash(db *sql.DB, collectionID string, limit, offset int) ([]*TrashItemRow, int, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if offset < 0 {
		offset = 0
	}

	// Count total for this collection
	var total int
	err := db.QueryRow(`SELECT COUNT(*) FROM trash_items WHERE collection_id = ?`, collectionID).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count trash items: %w", err)
	}

	// Query entries
	rows, err := db.Query(`SELECT id, collection_id, original_rel_path, trash_rel_path, kind, size_bytes, disk_name, deleted_at, retention_days, actor FROM trash_items WHERE collection_id = ? ORDER BY deleted_at DESC, rowid DESC LIMIT ? OFFSET ?`, collectionID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query trash items: %w", err)
	}
	defer rows.Close()

	var entries []*TrashItemRow
	for rows.Next() {
		var e TrashItemRow
		var deletedAt int64
		if err := rows.Scan(&e.ID, &e.CollectionID, &e.OriginalRelPath, &e.TrashRelPath, &e.Kind, &e.SizeBytes, &e.DiskName, &deletedAt, &e.RetentionDays, &e.Actor); err != nil {
			return nil, 0, fmt.Errorf("scan trash item: %w", err)
		}
		e.DeletedAt = time.Unix(deletedAt, 0)
		entries = append(entries, &e)
	}

	if entries == nil {
		entries = []*TrashItemRow{}
	}

	return entries, total, nil
}

// GetTrash retrieves a trash item by ID.
// Returns nil if not found (sql.ErrNoRows -> nil).
func GetTrash(db *sql.DB, id string) (*TrashItemRow, error) {
	row := db.QueryRow(`SELECT id, collection_id, original_rel_path, trash_rel_path, kind, size_bytes, disk_name, deleted_at, retention_days, actor FROM trash_items WHERE id = ?`, id)
	var e TrashItemRow
	var deletedAt int64
	if err := row.Scan(&e.ID, &e.CollectionID, &e.OriginalRelPath, &e.TrashRelPath, &e.Kind, &e.SizeBytes, &e.DiskName, &deletedAt, &e.RetentionDays, &e.Actor); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan trash item: %w", err)
	}
	e.DeletedAt = time.Unix(deletedAt, 0)
	return &e, nil
}

// DeleteTrashRow removes a trash item by ID.
// Returns error if not found (sql.ErrNoRows).
func DeleteTrashRow(db *sql.DB, id string) error {
	res, err := db.Exec(`DELETE FROM trash_items WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete trash item: %w", err)
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

// ============================================================================
// WS30 (2026-11-01): Scheduled keep-awake windows (FR-1.7)
// ============================================================================

// PowerScheduleRow is the catalog representation of a power schedule.
type PowerScheduleRow struct {
	ID        string    `json:"id"`
	Disk      string    `json:"disk"`
	Days      string    `json:"days"`       // comma-separated: Mon,Tue,Wed,Thu,Fri,Sat,Sun
	StartMin  int       `json:"start_min"`  // 0-1439
	EndMin    int       `json:"end_min"`    // 0-1439
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// generatePowerScheduleID returns a collision-proof power schedule ID.
// Format: ps-<unix-nano>-<8 hex chars from crypto/rand>
func generatePowerScheduleID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("ps-%d-%08x", now, counter)
	}
	return fmt.Sprintf("ps-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// validWeekdays is the set of valid weekday abbreviations.
var validWeekdays = map[string]bool{
	"Mon": true, "Tue": true, "Wed": true, "Thu": true,
	"Fri": true, "Sat": true, "Sun": true,
}

// parseAndValidateDays parses a comma-separated days string and validates it.
// Returns normalized days string (sorted, deduplicated) or error.
func parseAndValidateDays(days string) (string, error) {
	if days == "" {
		return "", fmt.Errorf("days cannot be empty")
	}
	parts := strings.Split(days, ",")
	seen := make(map[string]bool)
	var valid []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !validWeekdays[p] {
			return "", fmt.Errorf("invalid weekday: %s (must be Mon,Tue,Wed,Thu,Fri,Sat,Sun)", p)
		}
		if !seen[p] {
			seen[p] = true
			valid = append(valid, p)
		}
	}
	if len(valid) == 0 {
		return "", fmt.Errorf("days cannot be empty")
	}
	// Sort for consistent storage (Mon-Sun order)
	order := map[string]int{"Mon": 0, "Tue": 1, "Wed": 2, "Thu": 3, "Fri": 4, "Sat": 5, "Sun": 6}
	sort.Slice(valid, func(i, j int) bool {
		return order[valid[i]] < order[valid[j]]
	})
	return strings.Join(valid, ","), nil
}

// ErrPowerScheduleDuplicate is returned when a schedule with the same
// (disk, days, start_min, end_min) already exists.
var ErrPowerScheduleDuplicate = fmt.Errorf("power_schedule_duplicate")

// ErrPowerScheduleInvalid is returned when schedule validation fails.
var ErrPowerScheduleInvalid = fmt.Errorf("power_schedule_invalid")

// CreatePowerSchedule inserts a power schedule for a disk.
// Validation: disk non-empty; days non-empty valid weekdays; start/end in 0-1439, start != end.
// Duplicate (disk, days, start_min, end_min) returns ErrPowerScheduleDuplicate.
func CreatePowerSchedule(db *sql.DB, disk, days string, startMin, endMin int) (*PowerScheduleRow, error) {
	if strings.TrimSpace(disk) == "" {
		return nil, ErrPowerScheduleInvalid
	}
	normDays, err := parseAndValidateDays(days)
	if err != nil {
		return nil, ErrPowerScheduleInvalid
	}
	if startMin < 0 || startMin > 1439 || endMin < 0 || endMin > 1439 {
		return nil, ErrPowerScheduleInvalid
	}
	if startMin == endMin {
		return nil, ErrPowerScheduleInvalid
	}

	now := time.Now().Unix()
	id := generatePowerScheduleID()
	_, err = db.Exec(`
		INSERT INTO power_schedules (id, disk, days, start_min, end_min, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?)
	`, id, disk, normDays, startMin, endMin, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "unique") {
			return nil, ErrPowerScheduleDuplicate
		}
		return nil, fmt.Errorf("insert power schedule: %w", err)
	}
	return GetPowerSchedule(db, id)
}

// GetPowerSchedule retrieves a power schedule by ID.
// Returns nil if not found.
func GetPowerSchedule(db *sql.DB, id string) (*PowerScheduleRow, error) {
	row := db.QueryRow(`SELECT id, disk, days, start_min, end_min, enabled, created_at, updated_at FROM power_schedules WHERE id = ?`, id)
	var s PowerScheduleRow
	var createdAt, updatedAt int64
	var enabled int
	if err := row.Scan(&s.ID, &s.Disk, &s.Days, &s.StartMin, &s.EndMin, &enabled, &createdAt, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan power schedule: %w", err)
	}
	s.Enabled = enabled != 0
	s.CreatedAt = time.Unix(createdAt, 0)
	s.UpdatedAt = time.Unix(updatedAt, 0)
	return &s, nil
}

// ListPowerSchedules returns all power schedules for a disk.
// Returns [] (never nil) when empty.
func ListPowerSchedules(db *sql.DB, disk string) ([]*PowerScheduleRow, error) {
	rows, err := db.Query(`SELECT id, disk, days, start_min, end_min, enabled, created_at, updated_at FROM power_schedules WHERE disk = ? ORDER BY created_at`, disk)
	if err != nil {
		return nil, fmt.Errorf("query power schedules: %w", err)
	}
	defer rows.Close()

	var schedules []*PowerScheduleRow
	for rows.Next() {
		var s PowerScheduleRow
		var createdAt, updatedAt int64
		var enabled int
		if err := rows.Scan(&s.ID, &s.Disk, &s.Days, &s.StartMin, &s.EndMin, &enabled, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan power schedule: %w", err)
		}
		s.Enabled = enabled != 0
		s.CreatedAt = time.Unix(createdAt, 0)
		s.UpdatedAt = time.Unix(updatedAt, 0)
		schedules = append(schedules, &s)
	}
	if schedules == nil {
		schedules = []*PowerScheduleRow{}
	}
	return schedules, nil
}

// UpdatePowerScheduleEnabled updates the enabled state of a power schedule.
// Returns sql.ErrNoRows if not found.
func UpdatePowerScheduleEnabled(db *sql.DB, id string, enabled bool) error {
	now := time.Now().Unix()
	res, err := db.Exec(`UPDATE power_schedules SET enabled = ?, updated_at = ? WHERE id = ?`, boolToInt(enabled), now, id)
	if err != nil {
		return fmt.Errorf("update power schedule: %w", err)
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

// DeletePowerSchedule removes a power schedule by ID.
// Returns sql.ErrNoRows if not found.
func DeletePowerSchedule(db *sql.DB, id string) error {
	res, err := db.Exec(`DELETE FROM power_schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete power schedule: %w", err)
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

// PowerScheduleMatchesNow checks if a schedule matches the given time.
// now is expected to be in local time (the server's local timezone).
// days: comma-separated weekdays (Mon,Tue,...). startMin/endMin: minutes since midnight.
// Handles cross-midnight windows (e.g., 22:00-02:00 means startMin=1320, endMin=120).
func PowerScheduleMatchesNow(s *PowerScheduleRow, now time.Time) bool {
	if !s.Enabled {
		return false
	}
	// Check weekday
	weekday := now.Weekday().String()[:3] // Mon, Tue, etc.
	dayMap := map[string]bool{}
	for _, d := range strings.Split(s.Days, ",") {
		dayMap[d] = true
	}
	if !dayMap[weekday] {
		return false
	}
	// Check time window
	nowMin := now.Hour()*60 + now.Minute()
	if s.StartMin < s.EndMin {
		// Normal window: start < end (e.g., 09:00-17:00)
		return nowMin >= s.StartMin && nowMin < s.EndMin
	}
	// Cross-midnight window: start > end (e.g., 22:00-02:00)
	return nowMin >= s.StartMin || nowMin < s.EndMin
}

// ============================================================================
// WS39 (2026-11-07): AI content tagging & search index (FR-2.12 Phase-2)
// ============================================================================

// TagClusterRow is the catalog representation of a tag cluster.
type TagClusterRow struct {
	ID             string    `json:"id"`
	CollectionID   string    `json:"collection_id"`
	ClusterKey     string    `json:"cluster_key"`
	SampleRelPath  string    `json:"sample_rel_path"`
	CreatedAt      time.Time `json:"created_at"`
}

// TagClusterMemberRow is the catalog representation of a cluster member.
type TagClusterMemberRow struct {
	ClusterID string `json:"cluster_id"`
	RelPath   string `json:"rel_path"`
}

// MediaTagRow is the catalog representation of a media tag.
type MediaTagRow struct {
	ID           string    `json:"id"`
	CollectionID string    `json:"collection_id"`
	RelPath      string    `json:"rel_path"`
	Tag          string    `json:"tag"`
	Origin       string    `json:"origin"`       // "rule", "cluster", "sample", "manual"
	Confidence   float64   `json:"confidence"`   // 0.0-1.0
	Source       string    `json:"source"`       // "individual", "cluster"
	CreatedAt    time.Time `json:"created_at"`
}

// generateTagClusterID returns a collision-proof tag cluster ID.
func generateTagClusterID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("clu-%d-%08x", now, counter)
	}
	return fmt.Sprintf("clu-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// generateMediaTagID returns a collision-proof media tag ID.
func generateMediaTagID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("tag-%d-%08x", now, counter)
	}
	return fmt.Sprintf("tag-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// CreateTagCluster inserts a tag cluster.
func CreateTagCluster(db *sql.DB, cluster *TagClusterRow) error {
	now := time.Now().Unix()
	if cluster.ID == "" {
		cluster.ID = generateTagClusterID()
	}
	if cluster.CreatedAt.IsZero() {
		cluster.CreatedAt = time.Unix(now, 0)
	}
	_, err := db.Exec(`
		INSERT INTO tag_clusters (id, collection_id, cluster_key, sample_rel_path, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, cluster.ID, cluster.CollectionID, cluster.ClusterKey, cluster.SampleRelPath, cluster.CreatedAt.Unix())
	return err
}

// GetTagCluster retrieves a tag cluster by ID.
func GetTagCluster(db *sql.DB, id string) (*TagClusterRow, error) {
	row := db.QueryRow(`SELECT id, collection_id, cluster_key, sample_rel_path, created_at FROM tag_clusters WHERE id = ?`, id)
	var c TagClusterRow
	var createdAt int64
	if err := row.Scan(&c.ID, &c.CollectionID, &c.ClusterKey, &c.SampleRelPath, &createdAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan tag cluster: %w", err)
	}
	c.CreatedAt = time.Unix(createdAt, 0)
	return &c, nil
}

// ListTagClusters returns all tag clusters for a collection.
func ListTagClusters(db *sql.DB, collectionID string) ([]*TagClusterRow, error) {
	rows, err := db.Query(`SELECT id, collection_id, cluster_key, sample_rel_path, created_at FROM tag_clusters WHERE collection_id = ? ORDER BY created_at`, collectionID)
	if err != nil {
		return nil, fmt.Errorf("query tag clusters: %w", err)
	}
	defer rows.Close()

	var clusters []*TagClusterRow
	for rows.Next() {
		var c TagClusterRow
		var createdAt int64
		if err := rows.Scan(&c.ID, &c.CollectionID, &c.ClusterKey, &c.SampleRelPath, &createdAt); err != nil {
			return nil, fmt.Errorf("scan tag cluster: %w", err)
		}
		c.CreatedAt = time.Unix(createdAt, 0)
		clusters = append(clusters, &c)
	}
	if clusters == nil {
		clusters = []*TagClusterRow{}
	}
	return clusters, nil
}

// DeleteTagCluster removes a tag cluster and its members (cascades).
func DeleteTagCluster(db *sql.DB, id string) error {
	res, err := db.Exec(`DELETE FROM tag_clusters WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete tag cluster: %w", err)
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

// CreateTagClusterMember adds a member to a cluster.
func CreateTagClusterMember(db *sql.DB, clusterID, relPath string) error {
	_, err := db.Exec(`INSERT OR IGNORE INTO tag_cluster_members (cluster_id, rel_path) VALUES (?, ?)`, clusterID, relPath)
	return err
}

// ListTagClusterMembers returns all members of a cluster.
func ListTagClusterMembers(db *sql.DB, clusterID string) ([]*TagClusterMemberRow, error) {
	rows, err := db.Query(`SELECT cluster_id, rel_path FROM tag_cluster_members WHERE cluster_id = ? ORDER BY rel_path`, clusterID)
	if err != nil {
		return nil, fmt.Errorf("query tag cluster members: %w", err)
	}
	defer rows.Close()

	var members []*TagClusterMemberRow
	for rows.Next() {
		var m TagClusterMemberRow
		if err := rows.Scan(&m.ClusterID, &m.RelPath); err != nil {
			return nil, fmt.Errorf("scan tag cluster member: %w", err)
		}
		members = append(members, &m)
	}
	if members == nil {
		members = []*TagClusterMemberRow{}
	}
	return members, nil
}

// CreateMediaTag inserts a media tag.
// Returns error if duplicate (UNIQUE constraint on collection_id, rel_path, tag, origin).
func CreateMediaTag(db *sql.DB, tag *MediaTagRow) error {
	now := time.Now().Unix()
	if tag.ID == "" {
		tag.ID = generateMediaTagID()
	}
	if tag.CreatedAt.IsZero() {
		tag.CreatedAt = time.Unix(now, 0)
	}
	_, err := db.Exec(`
		INSERT INTO media_tags (id, collection_id, rel_path, tag, origin, confidence, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, tag.ID, tag.CollectionID, tag.RelPath, tag.Tag, tag.Origin, tag.Confidence, tag.Source, tag.CreatedAt.Unix())
	return err
}

// CreateMediaTagIgnore inserts a media tag, ignoring duplicates.
func CreateMediaTagIgnore(db *sql.DB, tag *MediaTagRow) error {
	now := time.Now().Unix()
	if tag.ID == "" {
		tag.ID = generateMediaTagID()
	}
	if tag.CreatedAt.IsZero() {
		tag.CreatedAt = time.Unix(now, 0)
	}
	_, err := db.Exec(`
		INSERT OR IGNORE INTO media_tags (id, collection_id, rel_path, tag, origin, confidence, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, tag.ID, tag.CollectionID, tag.RelPath, tag.Tag, tag.Origin, tag.Confidence, tag.Source, tag.CreatedAt.Unix())
	return err
}

// GetMediaTag retrieves a media tag by collection, rel_path, tag, and origin.
func GetMediaTag(db *sql.DB, collectionID, relPath, tag, origin string) (*MediaTagRow, error) {
	row := db.QueryRow(`SELECT id, collection_id, rel_path, tag, origin, confidence, source, created_at FROM media_tags WHERE collection_id = ? AND rel_path = ? AND tag = ? AND origin = ?`, collectionID, relPath, tag, origin)
	var t MediaTagRow
	var createdAt int64
	if err := row.Scan(&t.ID, &t.CollectionID, &t.RelPath, &t.Tag, &t.Origin, &t.Confidence, &t.Source, &createdAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan media tag: %w", err)
	}
	t.CreatedAt = time.Unix(createdAt, 0)
	return &t, nil
}

// ListMediaTags returns all tags for a collection, optionally filtered by rel_path and/or tag.
func ListMediaTags(db *sql.DB, collectionID, relPath, tag string, limit, offset int) ([]*MediaTagRow, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	whereClauses := []string{"collection_id = ?"}
	args := []any{collectionID}

	if relPath != "" {
		whereClauses = append(whereClauses, "rel_path = ?")
		args = append(args, relPath)
	}
	if tag != "" {
		whereClauses = append(whereClauses, "tag = ?")
		args = append(args, tag)
	}

	whereSQL := "WHERE " + strings.Join(whereClauses, " AND ")

	// Count total
	countQuery := "SELECT COUNT(*) FROM media_tags " + whereSQL
	var total int
	err := db.QueryRow(countQuery, args...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count media tags: %w", err)
	}

	// Query entries
	query := fmt.Sprintf("SELECT id, collection_id, rel_path, tag, origin, confidence, source, created_at FROM media_tags %s ORDER BY created_at DESC, rowid DESC LIMIT ? OFFSET ?", whereSQL)
	args = append(args, limit, offset)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query media tags: %w", err)
	}
	defer rows.Close()

	var tags []*MediaTagRow
	for rows.Next() {
		var t MediaTagRow
		var createdAt int64
		if err := rows.Scan(&t.ID, &t.CollectionID, &t.RelPath, &t.Tag, &t.Origin, &t.Confidence, &t.Source, &createdAt); err != nil {
			return nil, 0, fmt.Errorf("scan media tag: %w", err)
		}
		t.CreatedAt = time.Unix(createdAt, 0)
		tags = append(tags, &t)
	}

	if tags == nil {
		tags = []*MediaTagRow{}
	}
	return tags, total, nil
}

// DeleteMediaTag removes a media tag by collection, rel_path, tag, and origin.
func DeleteMediaTag(db *sql.DB, collectionID, relPath, tag, origin string) error {
	res, err := db.Exec(`DELETE FROM media_tags WHERE collection_id = ? AND rel_path = ? AND tag = ? AND origin = ?`, collectionID, relPath, tag, origin)
	if err != nil {
		return fmt.Errorf("delete media tag: %w", err)
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

// SearchMediaTags searches for files by tag (exact match) and/or keyword (substring over tags).
// Returns (files, total) where files are distinct rel_paths.
func SearchMediaTags(db *sql.DB, collectionID, tag, keyword string, limit int) ([]string, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	whereClauses := []string{"collection_id = ?"}
	args := []any{collectionID}

	if tag != "" {
		whereClauses = append(whereClauses, "tag = ?")
		args = append(args, tag)
	}
	if keyword != "" {
		whereClauses = append(whereClauses, "tag LIKE ?")
		args = append(args, "%"+keyword+"%")
	}

	whereSQL := "WHERE " + strings.Join(whereClauses, " AND ")

	// Count total distinct files
	countQuery := fmt.Sprintf("SELECT COUNT(DISTINCT rel_path) FROM media_tags %s", whereSQL)
	var total int
	err := db.QueryRow(countQuery, args...).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count search tags: %w", err)
	}

	// Query distinct rel_paths
	query := fmt.Sprintf("SELECT DISTINCT rel_path FROM media_tags %s ORDER BY rel_path LIMIT ?", whereSQL)
	args = append(args, limit)

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("query search tags: %w", err)
	}
	defer rows.Close()

	var files []string
	for rows.Next() {
		var relPath string
		if err := rows.Scan(&relPath); err != nil {
			return nil, 0, fmt.Errorf("scan search tag: %w", err)
		}
		files = append(files, relPath)
	}

	if files == nil {
		files = []string{}
	}
	return files, total, nil
}
