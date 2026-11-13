package agentmodes

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

// GetModes retrieves the agent modes from the database.
// If no row exists, returns the default modes (harness=true, mcp=false, preset=agent_inside)
// WITHOUT writing to the database.
func GetModes(db *sql.DB) (*Modes, error) {
	row := db.QueryRow(`SELECT resident_harness, external_mcp, preset, updated_at FROM agent_modes WHERE id = 1`)
	var m Modes
	var residentHarness, externalMCP int
	var updatedAt int64
	err := row.Scan(&residentHarness, &externalMCP, &m.Preset, &updatedAt)
	if err == sql.ErrNoRows {
		// Return default without writing
		return &Modes{
			ResidentHarness: true,
			ExternalMCP:     false,
			Preset:          PresetAgentInside,
		}, nil
	}
	if err != nil {
		return nil, err
	}
	m.ResidentHarness = residentHarness != 0
	m.ExternalMCP = externalMCP != 0
	return &m, nil
}

// SetModes updates the agent modes in the database.
// The preset is computed from the two switches and stored.
func SetModes(db *sql.DB, m *Modes) error {
	m.Preset = PresetFor(m.ResidentHarness, m.ExternalMCP)
	now := time.Now().Unix()
	residentHarness := 0
	if m.ResidentHarness {
		residentHarness = 1
	}
	externalMCP := 0
	if m.ExternalMCP {
		externalMCP = 1
	}
	_, err := db.Exec(`
		INSERT INTO agent_modes (id, resident_harness, external_mcp, preset, updated_at)
		VALUES (1, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			resident_harness = excluded.resident_harness,
			external_mcp = excluded.external_mcp,
			preset = excluded.preset,
			updated_at = excluded.updated_at
	`, residentHarness, externalMCP, m.Preset, now)
	return err
}
