package catalog

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestPowerScheduleMigration_v18_to_v19 tests that migration from v18 to v19
// creates the power_schedules table and index, sets schema_version to 19,
// and is idempotent on re-open.
func TestPowerScheduleMigration_v18_to_v19(t *testing.T) {
	// Create a database at v18 (before power_schedules table)
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	// Set up schema at v18 (create meta table and set version to 18)
	_, err = db.Exec(`
		CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO meta (key, value) VALUES ('schema_version', '18');
	`)
	if err != nil {
		t.Fatalf("setup v18 schema: %v", err)
	}

	// Run migration
	err = Migrate(db)
	if err != nil {
		t.Fatalf("migrate v18->v19: %v", err)
	}

	// Verify schema_version is latest (41 after WS62: event_rules; 40 after WS59: ota_updates; 39 after WS55: app_installs columns + v39)
	var version int
	err = db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version)
	if err != nil {
		t.Fatalf("query schema_version: %v", err)
	}
	if version != 41 {
		t.Errorf("expected schema_version 41, got %d", version)
	}

	// Verify power_schedules table exists
	var tableExists int
	err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='power_schedules'`).Scan(&tableExists)
	if err != nil {
		t.Fatalf("check power_schedules table: %v", err)
	}
	if tableExists != 1 {
		t.Error("power_schedules table not created")
	}

	// Verify app_installs table exists (WS53-B)
	err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='app_installs'`).Scan(&tableExists)
	if err != nil {
		t.Fatalf("check app_installs table: %v", err)
	}
	if tableExists != 1 {
		t.Error("app_installs table not created")
	}

	// Verify unique constraint on (disk, days, start_min, end_min)
	var indexExists int
	err = db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_power_schedules_disk'`).Scan(&indexExists)
	if err != nil {
		t.Fatalf("check idx_power_schedules_disk: %v", err)
	}
	if indexExists != 1 {
		t.Error("idx_power_schedules_disk index not created")
	}

	// Verify unique constraint exists (SQLite creates it as part of table definition for UNIQUE column constraint)
	// Check by attempting to insert a duplicate - should fail
	_, err = db.Exec(`INSERT INTO power_schedules (id, disk, days, start_min, end_min, enabled, created_at, updated_at) VALUES ('test1', 'sda', 'Mon', 540, 1020, 1, 0, 0)`)
	if err != nil {
		t.Fatalf("insert first: %v", err)
	}
	_, err = db.Exec(`INSERT INTO power_schedules (id, disk, days, start_min, end_min, enabled, created_at, updated_at) VALUES ('test2', 'sda', 'Mon', 540, 1020, 1, 0, 0)`)
	if err == nil {
		t.Error("unique constraint on (disk, days, start_min, end_min) not enforced")
	}

	// Re-run migration (idempotent)
	err = Migrate(db)
	if err != nil {
		t.Fatalf("re-run migrate: %v", err)
	}

	// Verify schema_version still latest (41) after re-run
	err = db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version)
	if err != nil {
		t.Fatalf("query schema_version after re-run: %v", err)
	}
	if version != 41 {
		t.Errorf("expected schema_version 41 after re-run, got %d", version)
	}
}

// TestPowerScheduleCreateListRoundtrip tests create + list roundtrip.
func TestPowerScheduleCreateListRoundtrip(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	err = Migrate(db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Create a schedule
	sch, err := CreatePowerSchedule(db, "sda", "Mon,Tue,Wed", 540, 1020) // 09:00-17:00
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	if sch == nil {
		t.Fatal("create schedule returned nil")
	}
	if sch.Disk != "sda" {
		t.Errorf("expected disk sda, got %s", sch.Disk)
	}
	if sch.Days != "Mon,Tue,Wed" {
		t.Errorf("expected days Mon,Tue,Wed, got %s", sch.Days)
	}
	if sch.StartMin != 540 {
		t.Errorf("expected start_min 540, got %d", sch.StartMin)
	}
	if sch.EndMin != 1020 {
		t.Errorf("expected end_min 1020, got %d", sch.EndMin)
	}
	if !sch.Enabled {
		t.Error("expected enabled=true by default")
	}
	if sch.CreatedAt.IsZero() || sch.UpdatedAt.IsZero() {
		t.Error("created_at/updated_at should be set")
	}

	// List schedules for the disk
	scheds, err := ListPowerSchedules(db, "sda")
	if err != nil {
		t.Fatalf("list schedules: %v", err)
	}
	if len(scheds) != 1 {
		t.Errorf("expected 1 schedule, got %d", len(scheds))
	}
	if scheds[0].ID != sch.ID {
		t.Errorf("listed schedule ID mismatch: %s vs %s", scheds[0].ID, sch.ID)
	}
}

// TestPowerScheduleDuplicate tests that duplicate (disk, days, start_min, end_min) returns ErrPowerScheduleDuplicate.
func TestPowerScheduleDuplicate(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	err = Migrate(db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Create first schedule
	_, err = CreatePowerSchedule(db, "sda", "Mon,Tue,Wed", 540, 1020)
	if err != nil {
		t.Fatalf("create first schedule: %v", err)
	}

	// Create duplicate (same disk, days, start, end)
	_, err = CreatePowerSchedule(db, "sda", "Mon,Tue,Wed", 540, 1020)
	if err == nil {
		t.Error("expected ErrPowerScheduleDuplicate for duplicate schedule")
	} else if err != ErrPowerScheduleDuplicate {
		t.Errorf("expected ErrPowerScheduleDuplicate, got %v", err)
	}
}

// TestPowerScheduleDifferentCaseDays tests that days are normalized (sorted, deduplicated).
func TestPowerScheduleDifferentCaseDays(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	err = Migrate(db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Create with unsorted, duplicate days
	sch, err := CreatePowerSchedule(db, "sda", "Wed,Mon,Mon,Tue", 540, 1020)
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	// Should be normalized to Mon,Tue,Wed
	if sch.Days != "Mon,Tue,Wed" {
		t.Errorf("expected normalized days Mon,Tue,Wed, got %s", sch.Days)
	}

	// Creating with same days in different order should be duplicate
	_, err = CreatePowerSchedule(db, "sda", "Tue,Wed,Mon", 540, 1020)
	if err == nil {
		t.Error("expected duplicate error for same days in different order")
	} else if err != ErrPowerScheduleDuplicate {
		t.Errorf("expected ErrPowerScheduleDuplicate, got %v", err)
	}
}

// TestPowerScheduleInvalid tests various invalid inputs return ErrPowerScheduleInvalid.
func TestPowerScheduleInvalid(t *testing.T) {
	tests := []struct {
		name     string
		disk     string
		days     string
		startMin int
		endMin   int
	}{
		{"empty disk", "", "Mon", 540, 1020},
		{"empty days", "sda", "", 540, 1020},
		{"invalid weekday", "sda", "Mon,Invalid", 540, 1020},
		{"start out of range negative", "sda", "Mon", -1, 1020},
		{"start out of range >1439", "sda", "Mon", 1440, 1020},
		{"end out of range negative", "sda", "Mon", 540, -1},
		{"end out of range >1439", "sda", "Mon", 540, 1440},
		{"start equals end", "sda", "Mon", 540, 540},
		{"only whitespace days", "sda", "   ", 540, 1020},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := Open(":memory:")
			if err != nil {
				t.Fatalf("open catalog: %v", err)
			}
			defer db.Close()

			err = Migrate(db)
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}

			_, err = CreatePowerSchedule(db, tt.disk, tt.days, tt.startMin, tt.endMin)
			if err == nil {
				t.Error("expected ErrPowerScheduleInvalid")
			} else if err != ErrPowerScheduleInvalid {
				t.Errorf("expected ErrPowerScheduleInvalid, got %v", err)
			}
		})
	}
}

// TestPowerScheduleCrossMidnight tests that cross-midnight windows (start > end) are accepted.
func TestPowerScheduleCrossMidnight(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	err = Migrate(db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// 22:00 (1320) to 02:00 (120) - cross midnight
	sch, err := CreatePowerSchedule(db, "sda", "Mon,Tue,Wed,Thu,Fri", 1320, 120)
	if err != nil {
		t.Fatalf("create cross-midnight schedule: %v", err)
	}
	if sch.StartMin != 1320 || sch.EndMin != 120 {
		t.Errorf("expected start=1320 end=120, got start=%d end=%d", sch.StartMin, sch.EndMin)
	}
	if !sch.Enabled {
		t.Error("expected enabled=true")
	}
}

// TestPowerScheduleEnableDisablePatch tests updating the enabled state.
func TestPowerScheduleEnableDisablePatch(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	err = Migrate(db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	sch, err := CreatePowerSchedule(db, "sda", "Mon", 540, 1020)
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	if !sch.Enabled {
		t.Error("expected enabled=true initially")
	}

	// Disable
	err = UpdatePowerScheduleEnabled(db, sch.ID, false)
	if err != nil {
		t.Fatalf("disable schedule: %v", err)
	}

	sch, err = GetPowerSchedule(db, sch.ID)
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if sch.Enabled {
		t.Error("expected enabled=false after disable")
	}

	// Enable
	err = UpdatePowerScheduleEnabled(db, sch.ID, true)
	if err != nil {
		t.Fatalf("enable schedule: %v", err)
	}

	sch, err = GetPowerSchedule(db, sch.ID)
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	if !sch.Enabled {
		t.Error("expected enabled=true after enable")
	}
}

// TestPowerScheduleDelete tests delete and delete-missing behavior.
func TestPowerScheduleDelete(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	err = Migrate(db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	sch, err := CreatePowerSchedule(db, "sda", "Mon", 540, 1020)
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	scheduleID := sch.ID
	// Delete existing
	err = DeletePowerSchedule(db, scheduleID)
	if err != nil {
		t.Fatalf("delete schedule: %v", err)
	}

	// Verify gone
	sch, err = GetPowerSchedule(db, scheduleID)
	if err != nil {
		t.Fatalf("get deleted schedule: %v", err)
	}
	if sch != nil {
		t.Error("expected nil for deleted schedule")
	}

	// Delete missing -> sql.ErrNoRows
	err = DeletePowerSchedule(db, scheduleID)
	if err == nil {
		t.Error("expected sql.ErrNoRows for deleting missing schedule")
	} else if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}

// TestPowerScheduleListEmpty tests that listing schedules for a disk with none returns empty slice (not nil).
func TestPowerScheduleListEmpty(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	err = Migrate(db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	scheds, err := ListPowerSchedules(db, "nonexistent")
	if err != nil {
		t.Fatalf("list schedules: %v", err)
	}
	if scheds == nil {
		t.Error("expected empty slice, not nil")
	}
	if len(scheds) != 0 {
		t.Errorf("expected 0 schedules, got %d", len(scheds))
	}
}

// TestPowerScheduleGetMissing tests that getting a missing schedule returns nil, not error.
func TestPowerScheduleGetMissing(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	err = Migrate(db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	sch, err := GetPowerSchedule(db, "nonexistent-id")
	if err != nil {
		t.Fatalf("get missing schedule: %v", err)
	}
	if sch != nil {
		t.Error("expected nil for missing schedule")
	}
}

// TestPowerScheduleUpdateMissing tests that updating a missing schedule returns sql.ErrNoRows.
func TestPowerScheduleUpdateMissing(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	err = Migrate(db)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}

	err = UpdatePowerScheduleEnabled(db, "nonexistent-id", true)
	if err == nil {
		t.Error("expected sql.ErrNoRows for updating missing schedule")
	} else if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}

// TestPowerScheduleMatchesNow tests the PowerScheduleMatchesNow helper.
func TestPowerScheduleMatchesNow(t *testing.T) {
	tests := []struct {
		name     string
		days     string
		startMin int
		endMin   int
		enabled  bool
		now      time.Time
		expected bool
	}{
		{"normal window inside", "Mon", 540, 1020, true, time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), true},  // Monday 12:00
		{"normal window before", "Mon", 540, 1020, true, time.Date(2024, 1, 1, 8, 0, 0, 0, time.UTC), false},   // Monday 08:00
		{"normal window after", "Mon", 540, 1020, true, time.Date(2024, 1, 1, 18, 0, 0, 0, time.UTC), false},   // Monday 18:00
		{"normal window boundary start", "Mon", 540, 1020, true, time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC), true},  // Monday 09:00 (inclusive)
		{"normal window boundary end", "Mon", 540, 1020, true, time.Date(2024, 1, 1, 17, 0, 0, 0, time.UTC), false}, // Monday 17:00 (exclusive)
		{"cross-midnight before midnight", "Mon", 1320, 120, true, time.Date(2024, 1, 1, 23, 0, 0, 0, time.UTC), true},  // Monday 23:00
		{"cross-midnight after midnight (needs Mon,Tue)", "Mon", 1320, 120, true, time.Date(2024, 1, 2, 1, 0, 0, 0, time.UTC), false},   // Tuesday 01:00 - weekday Tue not in Mon
		{"cross-midnight after midnight with both days", "Mon,Tue", 1320, 120, true, time.Date(2024, 1, 2, 1, 0, 0, 0, time.UTC), true}, // Tuesday 01:00 with Mon,Tue
		{"cross-midnight outside", "Mon", 1320, 120, true, time.Date(2024, 1, 2, 3, 0, 0, 0, time.UTC), false},       // Tuesday 03:00
		{"wrong weekday", "Mon", 540, 1020, true, time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC), false},             // Tuesday 12:00
		{"disabled schedule", "Mon", 540, 1020, false, time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), false},        // Monday 12:00 but disabled
		{"multiple days match", "Mon,Wed,Fri", 540, 1020, true, time.Date(2024, 1, 3, 12, 0, 0, 0, time.UTC), true}, // Wednesday 12:00
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sch := &PowerScheduleRow{
				Days:     tt.days,
				StartMin: tt.startMin,
				EndMin:   tt.endMin,
				Enabled:  tt.enabled,
			}
			result := PowerScheduleMatchesNow(sch, tt.now)
			if result != tt.expected {
				t.Errorf("PowerScheduleMatchesNow(%v, %v) = %v, want %v", sch, tt.now, result, tt.expected)
			}
		})
	}
}
