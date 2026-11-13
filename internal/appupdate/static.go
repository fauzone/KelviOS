package appupdate

// StaticFeed is a bundled static update feed (v1).
// It provides deterministic update data without network dependencies.
type StaticFeed struct {
	updates []AppUpdate
	byID    map[string]AppUpdate
}

// NewStaticFeed creates a new static feed with the bundled v1 data.
// The feed contains exactly 2 wave-1 apps as real examples:
// - adguard-home: WITH permission changes (ports + RAMMB delta)
// - esphome: WITHOUT permission changes (version/image only)
func NewStaticFeed() *StaticFeed {
	updates := []AppUpdate{
		{
			AppID:        "adguard-home",
			Version:      "0.108.0",
			ImageRef:     "adguard/adguardhome:v0.108.0",
			ReleaseNotes: "Added DNS-over-HTTP/3 support; improved query log filtering; fixed memory leak in statistics module.",
			Ports:        []int{53, 67, 68, 80, 443, 3000, 853, 784, 8853, 5443, 8443}, // +8443
			RAMMB:        256, // 128 -> 256
			Digest:       "sha256:a1b2c3d4e5f6789012345678901234567890abcdef1234567890abcdef123456",
		},
		{
			AppID:        "esphome",
			Version:      "2024.12.0",
			ImageRef:     "ghcr.io/esphome/esphome:2024.12.0",
			ReleaseNotes: "New ESP32-C6 support; improved Home Assistant integration; bug fixes for deep sleep.",
			Ports:        []int{6052, 6053}, // same
			RAMMB:        256, // same
			Digest:       "sha256:f6e5d4c3b2a109876543210fedcba9876543210fedcba9876543210fedcba987",
		},
	}

	byID := make(map[string]AppUpdate, len(updates))
	for _, u := range updates {
		byID[u.AppID] = u
	}

	return &StaticFeed{
		updates: updates,
		byID:    byID,
	}
}

// ListUpdates returns all update entries in the static feed.
func (s *StaticFeed) ListUpdates() ([]AppUpdate, error) {
	// Return a copy to prevent external mutation
	result := make([]AppUpdate, len(s.updates))
	copy(result, s.updates)
	return result, nil
}

// UpdateFor returns the update entry for the given app ID.
func (s *StaticFeed) UpdateFor(appID string) (AppUpdate, bool) {
	u, ok := s.byID[appID]
	return u, ok
}