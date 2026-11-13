package agentmodes

import "fmt"

// Modes represents the agent setup modes configuration.
type Modes struct {
	ResidentHarness bool   `json:"resident_harness"`
	ExternalMCP     bool   `json:"external_mcp"`
	Preset          string `json:"preset"`
}

// Preset constants
const (
	PresetAgentInside    = "agent_inside"
	PresetManagedOutside = "managed_outside"
	PresetBoth           = "both"
	PresetNoAgent        = "no_agent"
)

// PresetFor returns the preset name for the given harness and MCP switch states.
func PresetFor(harness, mcp bool) string {
	switch {
	case harness && mcp:
		return PresetBoth
	case harness && !mcp:
		return PresetAgentInside
	case !harness && mcp:
		return PresetManagedOutside
	default:
		return PresetNoAgent
	}
}

// SwitchesFor returns the harness and MCP switch states for a given preset.
func SwitchesFor(preset string) (bool, bool, error) {
	switch preset {
	case PresetAgentInside:
		return true, false, nil
	case PresetManagedOutside:
		return false, true, nil
	case PresetBoth:
		return true, true, nil
	case PresetNoAgent:
		return false, false, nil
	default:
		return false, false, ErrInvalidPreset
	}
}

// ErrInvalidPreset is returned when an unknown preset is provided.
var ErrInvalidPreset = fmt.Errorf("invalid preset")
