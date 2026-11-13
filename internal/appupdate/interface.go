package appupdate

// AppUpdate represents an available update for an app from the update feed.
type AppUpdate struct {
	AppID           string
	Version         string
	ImageRef        string
	ReleaseNotes    string
	Ports           []int
	RAMMB           int
	Digest          string
}

// UpdateSource is the interface for update data sources.
// Implementations provide available updates from various backends
// (static feed, registry API, etc.).
type UpdateSource interface {
	// ListUpdates returns all known update entries from the feed.
	ListUpdates() ([]AppUpdate, error)
	// UpdateFor returns the update entry for a specific app, if any.
	UpdateFor(appID string) (AppUpdate, bool)
}