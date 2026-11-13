package systemsettings

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Get retrieves the current system settings from the meta table.
// Returns zero values for keys that are not yet set.
func Get(db *sql.DB) (*SystemSettings, error) {
	var s SystemSettings

	// Language
	var lang sql.NullString
	err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, MetaKeyLanguage).Scan(&lang)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("get language: %w", err)
	}
	if lang.Valid {
		s.Language = lang.String
	}

	// Timezone
	var tz sql.NullString
	err = db.QueryRow(`SELECT value FROM meta WHERE key = ?`, MetaKeyTimezone).Scan(&tz)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("get timezone: %w", err)
	}
	if tz.Valid {
		s.Timezone = tz.String
	}

	// Setup completed at
	var completed sql.NullInt64
	err = db.QueryRow(`SELECT value FROM meta WHERE key = ?`, MetaKeySetupCompleted).Scan(&completed)
	if err != nil && err != sql.ErrNoRows {
		return nil, fmt.Errorf("get setup_completed_at: %w", err)
	}
	if completed.Valid {
		s.SetupCompletedAt = completed.Int64
	}

	return &s, nil
}

// SetLanguage stores the system language.
// Validates against ValidLanguages.
func SetLanguage(db *sql.DB, lang string) error {
	if !ValidLanguages[lang] {
		return fmt.Errorf("invalid language: %s", lang)
	}
	_, err := db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, MetaKeyLanguage, lang)
	if err != nil {
		return fmt.Errorf("set language: %w", err)
	}
	return nil
}

// SetTimezone stores the system timezone.
// Validates by attempting to load the location.
func SetTimezone(db *sql.DB, tz string) error {
	if _, err := time.LoadLocation(tz); err != nil {
		return fmt.Errorf("invalid timezone: %s", tz)
	}
	_, err := db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, MetaKeyTimezone, tz)
	if err != nil {
		return fmt.Errorf("set timezone: %w", err)
	}
	return nil
}

// SetSetupCompleted stores the setup completion timestamp.
func SetSetupCompleted(db *sql.DB, ts int64) error {
	if ts <= 0 {
		ts = time.Now().Unix()
	}
	_, err := db.Exec(`INSERT OR REPLACE INTO meta (key, value) VALUES (?, ?)`, MetaKeySetupCompleted, fmt.Sprintf("%d", ts))
	if err != nil {
		return fmt.Errorf("set setup_completed_at: %w", err)
	}
	return nil
}

// IsSetupComplete returns true if setup has been completed.
func IsSetupComplete(db *sql.DB) (bool, error) {
	s, err := Get(db)
	if err != nil {
		return false, err
	}
	return s.SetupCompletedAt > 0, nil
}

// HasAdmin checks if any admin account exists.
func HasAdmin(db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("count accounts: %w", err)
	}
	return count > 0, nil
}