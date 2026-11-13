package ups

import (
	"os"
	"time"
)

// MarkCleanShutdown writes a marker file with the current timestamp (RFC3339).
// The marker indicates that the previous shutdown was clean.
func MarkCleanShutdown(path string) error {
	ts := time.Now().Format(time.RFC3339)
	return os.WriteFile(path, []byte(ts), 0o644)
}

// CheckBoot checks for the clean shutdown marker.
// - If marker exists: removes it and returns (true, nil) — clean shutdown.
// - If marker does not exist: returns (false, nil) — unclean shutdown (first boot also counts).
// - Other errors: returns (false, err).
func CheckBoot(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	// Marker exists — clean shutdown. Remove it so next boot is unclean unless marked again.
	_ = os.Remove(path)
	_ = data // timestamp available if needed
	return true, nil
}
