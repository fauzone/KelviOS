package apps

// Status represents the lifecycle status of an application.
type Status string

const (
	// StatusAvailable means the app is in the catalog but not installed.
	StatusAvailable Status = "available"
	// StatusInstalled means the app is installed but not running.
	StatusInstalled Status = "installed"
	// StatusActive means the app is installed and running.
	StatusActive Status = "active"
)

// ValidTransition returns true if the transition from 'from' to 'to' is allowed.
// Allowed transitions:
//   - available -> installed (install)
//   - installed -> active (activate)
//   - active -> installed (deactivate - stops completely, returns to installed)
//   - installed -> available (uninstall)
// All other transitions (including self-transitions and unknown statuses) return false.
func ValidTransition(from, to Status) bool {
	switch from {
	case StatusAvailable:
		return to == StatusInstalled
	case StatusInstalled:
		return to == StatusActive || to == StatusAvailable
	case StatusActive:
		return to == StatusInstalled
	default:
		return false
	}
}