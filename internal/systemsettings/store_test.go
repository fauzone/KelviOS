package systemsettings

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Create meta table
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS meta (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`)
	if err != nil {
		db.Close()
		t.Fatalf("create meta table: %v", err)
	}
	// Create accounts table for HasAdmin
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			failed_count INTEGER NOT NULL DEFAULT 0,
			locked_until INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL
		);
	`)
	if err != nil {
		db.Close()
		t.Fatalf("create accounts table: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSystemSettings_GetEmpty(t *testing.T) {
	db := newTestDB(t)

	s, err := Get(db)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s.Language != "" {
		t.Errorf("Language: got %q, want empty", s.Language)
	}
	if s.Timezone != "" {
		t.Errorf("Timezone: got %q, want empty", s.Timezone)
	}
	if s.SetupCompletedAt != 0 {
		t.Errorf("SetupCompletedAt: got %d, want 0", s.SetupCompletedAt)
	}
}

func TestSystemSettings_SetAndGetLanguage(t *testing.T) {
	db := newTestDB(t)

	if err := SetLanguage(db, "id"); err != nil {
		t.Fatalf("SetLanguage: %v", err)
	}

	s, err := Get(db)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s.Language != "id" {
		t.Errorf("Language: got %q, want 'id'", s.Language)
	}

	// Overwrite
	if err := SetLanguage(db, "en"); err != nil {
		t.Fatalf("SetLanguage overwrite: %v", err)
	}
	s, err = Get(db)
	if err != nil {
		t.Fatalf("Get after overwrite: %v", err)
	}
	if s.Language != "en" {
		t.Errorf("Language after overwrite: got %q, want 'en'", s.Language)
	}
}

func TestSystemSettings_SetLanguageInvalid(t *testing.T) {
	db := newTestDB(t)

	err := SetLanguage(db, "fr")
	if err == nil {
		t.Error("SetLanguage with invalid language: want error, got nil")
	}
}

func TestSystemSettings_SetAndGetTimezone(t *testing.T) {
	db := newTestDB(t)

	if err := SetTimezone(db, "Asia/Jakarta"); err != nil {
		t.Fatalf("SetTimezone: %v", err)
	}

	s, err := Get(db)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s.Timezone != "Asia/Jakarta" {
		t.Errorf("Timezone: got %q, want 'Asia/Jakarta'", s.Timezone)
	}

	// Overwrite
	if err := SetTimezone(db, "UTC"); err != nil {
		t.Fatalf("SetTimezone overwrite: %v", err)
	}
	s, err = Get(db)
	if err != nil {
		t.Fatalf("Get after overwrite: %v", err)
	}
	if s.Timezone != "UTC" {
		t.Errorf("Timezone after overwrite: got %q, want 'UTC'", s.Timezone)
	}
}

func TestSystemSettings_SetTimezoneInvalid(t *testing.T) {
	db := newTestDB(t)

	err := SetTimezone(db, "Invalid/Timezone")
	if err == nil {
		t.Error("SetTimezone with invalid timezone: want error, got nil")
	}
}

func TestSystemSettings_SetSetupCompleted(t *testing.T) {
	db := newTestDB(t)

	ts := int64(1730000000)
	if err := SetSetupCompleted(db, ts); err != nil {
		t.Fatalf("SetSetupCompleted: %v", err)
	}

	s, err := Get(db)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s.SetupCompletedAt != ts {
		t.Errorf("SetupCompletedAt: got %d, want %d", s.SetupCompletedAt, ts)
	}

	// Test default (ts <= 0 uses now)
	if err := SetSetupCompleted(db, 0); err != nil {
		t.Fatalf("SetSetupCompleted with 0: %v", err)
	}
	s, err = Get(db)
	if err != nil {
		t.Fatalf("Get after default: %v", err)
	}
	if s.SetupCompletedAt <= 0 {
		t.Errorf("SetupCompletedAt with default: got %d, want > 0", s.SetupCompletedAt)
	}
}

func TestSystemSettings_IsSetupComplete(t *testing.T) {
	db := newTestDB(t)

	complete, err := IsSetupComplete(db)
	if err != nil {
		t.Fatalf("IsSetupComplete: %v", err)
	}
	if complete {
		t.Error("IsSetupComplete: want false on fresh DB")
	}

	if err := SetSetupCompleted(db, 1730000000); err != nil {
		t.Fatalf("SetSetupCompleted: %v", err)
	}

	complete, err = IsSetupComplete(db)
	if err != nil {
		t.Fatalf("IsSetupComplete after set: %v", err)
	}
	if !complete {
		t.Error("IsSetupComplete: want true after setting timestamp")
	}
}

func TestSystemSettings_HasAdmin(t *testing.T) {
	db := newTestDB(t)

	has, err := HasAdmin(db)
	if err != nil {
		t.Fatalf("HasAdmin: %v", err)
	}
	if has {
		t.Error("HasAdmin: want false on fresh DB")
	}

	// Insert an account
	_, err = db.Exec(`INSERT INTO accounts (id, username, password_hash, created_at) VALUES (?, ?, ?, ?)`,
		"acc1", "admin", "hash", 1730000000)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}

	has, err = HasAdmin(db)
	if err != nil {
		t.Fatalf("HasAdmin after insert: %v", err)
	}
	if !has {
		t.Error("HasAdmin: want true after inserting account")
	}
}