package apps

import (
	"testing"
)

func TestValidTransition(t *testing.T) {
	tests := []struct {
		from     Status
		to       Status
		expected bool
		desc     string
	}{
		// Valid transitions
		{StatusAvailable, StatusInstalled, true, "available -> installed (install)"},
		{StatusInstalled, StatusActive, true, "installed -> active (activate)"},
		{StatusActive, StatusInstalled, true, "active -> installed (deactivate)"},
		{StatusInstalled, StatusAvailable, true, "installed -> available (uninstall)"},

		// Invalid transitions (including self-transitions)
		{StatusAvailable, StatusAvailable, false, "available -> available (self)"},
		{StatusInstalled, StatusInstalled, false, "installed -> installed (self)"},
		{StatusActive, StatusActive, false, "active -> active (self)"},
		{StatusAvailable, StatusActive, false, "available -> active (skip installed)"},
		{StatusActive, StatusAvailable, false, "active -> available (skip installed)"},

		// Unknown status
		{Status("unknown"), StatusInstalled, false, "unknown -> installed"},
		{StatusAvailable, Status("unknown"), false, "available -> unknown"},
		{Status("unknown"), Status("unknown"), false, "unknown -> unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			result := ValidTransition(tt.from, tt.to)
			if result != tt.expected {
				t.Errorf("ValidTransition(%q, %q) = %v, want %v", tt.from, tt.to, result, tt.expected)
			}
		})
	}
}