package agentmodes_test

import (
	"database/sql"
	"testing"

	"github.com/fauzone/kelvios/internal/agentmodes"
	"github.com/fauzone/kelvios/internal/catalog"
	_ "modernc.org/sqlite"
)

func TestPresetFor(t *testing.T) {
	tests := []struct {
		name     string
		harness  bool
		mcp      bool
		want     string
	}{
		{"agent_inside", true, false, agentmodes.PresetAgentInside},
		{"managed_outside", false, true, agentmodes.PresetManagedOutside},
		{"both", true, true, agentmodes.PresetBoth},
		{"no_agent", false, false, agentmodes.PresetNoAgent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := agentmodes.PresetFor(tc.harness, tc.mcp)
			if got != tc.want {
				t.Errorf("PresetFor(%v, %v) = %q, want %q", tc.harness, tc.mcp, got, tc.want)
			}
		})
	}
}

func TestSwitchesFor(t *testing.T) {
	tests := []struct {
		name     string
		preset   string
		wantH    bool
		wantM    bool
		wantErr  bool
	}{
		{"agent_inside", agentmodes.PresetAgentInside, true, false, false},
		{"managed_outside", agentmodes.PresetManagedOutside, false, true, false},
		{"both", agentmodes.PresetBoth, true, true, false},
		{"no_agent", agentmodes.PresetNoAgent, false, false, false},
		{"unknown", "unknown_preset", false, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotH, gotM, err := agentmodes.SwitchesFor(tc.preset)
			if tc.wantErr {
				if err == nil {
					t.Errorf("SwitchesFor(%q) expected error, got nil", tc.preset)
				}
				return
			}
			if err != nil {
				t.Errorf("SwitchesFor(%q) unexpected error: %v", tc.preset, err)
				return
			}
			if gotH != tc.wantH || gotM != tc.wantM {
				t.Errorf("SwitchesFor(%q) = (%v, %v), want (%v, %v)", tc.preset, gotH, gotM, tc.wantH, tc.wantM)
			}
		})
	}
}

func TestGetModesDefaultOnFreshDB(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Delete the default row inserted by migration (v36 inserts default if table empty)
	if _, err := db.Exec(`DELETE FROM agent_modes`); err != nil {
		t.Fatalf("delete agent_modes: %v", err)
	}

	modes, err := agentmodes.GetModes(db)
	if err != nil {
		t.Fatalf("GetModes: %v", err)
	}
	if modes == nil {
		t.Fatal("GetModes returned nil")
	}
	if !modes.ResidentHarness {
		t.Errorf("default ResidentHarness = false, want true")
	}
	if modes.ExternalMCP {
		t.Errorf("default ExternalMCP = true, want false")
	}
	if modes.Preset != agentmodes.PresetAgentInside {
		t.Errorf("default Preset = %q, want %q", modes.Preset, agentmodes.PresetAgentInside)
	}

	// Verify no row was written
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_modes`).Scan(&count); err != nil {
		t.Fatalf("count agent_modes: %v", err)
	}
	if count != 0 {
		t.Errorf("agent_modes row count = %d, want 0 (GetModes must not write)", count)
	}
}

func TestGetModesReturnsExistingRow(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Migration v36 inserts default row (harness=1, mcp=0, preset='agent_inside')
	modes, err := agentmodes.GetModes(db)
	if err != nil {
		t.Fatalf("GetModes: %v", err)
	}
	if modes == nil {
		t.Fatal("GetModes returned nil")
	}
	if !modes.ResidentHarness {
		t.Errorf("ResidentHarness = false, want true (from migration default)")
	}
	if modes.ExternalMCP {
		t.Errorf("ExternalMCP = true, want false (from migration default)")
	}
	if modes.Preset != agentmodes.PresetAgentInside {
		t.Errorf("Preset = %q, want %q", modes.Preset, agentmodes.PresetAgentInside)
	}

	// Row count should be 1 (the migration default)
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_modes`).Scan(&count); err != nil {
		t.Fatalf("count agent_modes: %v", err)
	}
	if count != 1 {
		t.Errorf("agent_modes row count = %d, want 1", count)
	}
}

func TestSetModesRoundTrip(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Delete migration default row to start clean
	if _, err := db.Exec(`DELETE FROM agent_modes`); err != nil {
		t.Fatalf("delete agent_modes: %v", err)
	}

	// Test managed_outside (harness=false, mcp=true)
	if err := agentmodes.SetModes(db, &agentmodes.Modes{ResidentHarness: false, ExternalMCP: true}); err != nil {
		t.Fatalf("SetModes managed_outside: %v", err)
	}
	modes, err := agentmodes.GetModes(db)
	if err != nil {
		t.Fatalf("GetModes after SetModes: %v", err)
	}
	if modes.ResidentHarness {
		t.Errorf("ResidentHarness = true, want false")
	}
	if !modes.ExternalMCP {
		t.Errorf("ExternalMCP = false, want true")
	}
	if modes.Preset != agentmodes.PresetManagedOutside {
		t.Errorf("Preset = %q, want %q", modes.Preset, agentmodes.PresetManagedOutside)
	}

	// Test both (harness=true, mcp=true)
	if err := agentmodes.SetModes(db, &agentmodes.Modes{ResidentHarness: true, ExternalMCP: true}); err != nil {
		t.Fatalf("SetModes both: %v", err)
	}
	modes, err = agentmodes.GetModes(db)
	if err != nil {
		t.Fatalf("GetModes after SetModes both: %v", err)
	}
	if !modes.ResidentHarness {
		t.Errorf("ResidentHarness = false, want true")
	}
	if !modes.ExternalMCP {
		t.Errorf("ExternalMCP = false, want true")
	}
	if modes.Preset != agentmodes.PresetBoth {
		t.Errorf("Preset = %q, want %q", modes.Preset, agentmodes.PresetBoth)
	}

	// Verify exactly 1 row in table
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_modes`).Scan(&count); err != nil {
		t.Fatalf("count agent_modes: %v", err)
	}
	if count != 1 {
		t.Errorf("agent_modes row count = %d, want 1", count)
	}
}

func TestSetModesUpsert(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// First set
	if err := agentmodes.SetModes(db, &agentmodes.Modes{ResidentHarness: true, ExternalMCP: false}); err != nil {
		t.Fatalf("first SetModes: %v", err)
	}
	// Second set (should upsert, not insert duplicate)
	if err := agentmodes.SetModes(db, &agentmodes.Modes{ResidentHarness: false, ExternalMCP: true}); err != nil {
		t.Fatalf("second SetModes: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_modes`).Scan(&count); err != nil {
		t.Fatalf("count agent_modes: %v", err)
	}
	if count != 1 {
		t.Errorf("agent_modes row count after upsert = %d, want 1", count)
	}

	modes, err := agentmodes.GetModes(db)
	if err != nil {
		t.Fatalf("GetModes: %v", err)
	}
	if modes.Preset != agentmodes.PresetManagedOutside {
		t.Errorf("Preset after upsert = %q, want %q", modes.Preset, agentmodes.PresetManagedOutside)
	}
}

func TestGetModesErrorOnCorruptRow(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Corrupt the row: invalid preset value
	if _, err := db.Exec(`UPDATE agent_modes SET preset = 'corrupted' WHERE id = 1`); err != nil {
		t.Fatalf("corrupt row: %v", err)
	}

	modes, err := agentmodes.GetModes(db)
	if err != nil {
		t.Fatalf("GetModes on corrupt row: %v", err)
	}
	// Should still return the row (even with invalid preset)
	if modes == nil {
		t.Fatal("GetModes returned nil on corrupt row")
	}
	if modes.Preset != "corrupted" {
		t.Errorf("Preset = %q, want 'corrupted' (raw value from DB)", modes.Preset)
	}
}

// Test that GetModes works with a raw sql.DB (not catalog-wrapped)
func TestGetModesWithRawSQLDB(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	// Create the table manually (simulating a DB that has the table but no catalog migration)
	if _, err := db.Exec(`
		CREATE TABLE agent_modes (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			resident_harness INTEGER NOT NULL,
			external_mcp INTEGER NOT NULL,
			preset TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)
	`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Insert a custom row
	if _, err := db.Exec(`INSERT INTO agent_modes (id, resident_harness, external_mcp, preset, updated_at) VALUES (1, 0, 1, 'managed_outside', 1234567890)`); err != nil {
		t.Fatalf("insert row: %v", err)
	}

	modes, err := agentmodes.GetModes(db)
	if err != nil {
		t.Fatalf("GetModes: %v", err)
	}
	if modes == nil {
		t.Fatal("GetModes returned nil")
	}
	if modes.ResidentHarness {
		t.Errorf("ResidentHarness = true, want false")
	}
	if !modes.ExternalMCP {
		t.Errorf("ExternalMCP = false, want true")
	}
	if modes.Preset != "managed_outside" {
		t.Errorf("Preset = %q, want managed_outside", modes.Preset)
	}
}