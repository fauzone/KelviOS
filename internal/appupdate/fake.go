package appupdate

// FakeUpdateSource is a controllable update source for tests.
// It allows setting per-app update availability and feed errors.
type FakeUpdateSource struct {
	updates map[string]AppUpdate
	err     error
}

// NewFakeUpdateSource creates a new fake update source.
func NewFakeUpdateSource() *FakeUpdateSource {
	return &FakeUpdateSource{
		updates: make(map[string]AppUpdate),
	}
}

// SetUpdate sets an update entry for the given app ID.
// Pass nil AppUpdate to indicate no update available.
func (f *FakeUpdateSource) SetUpdate(appID string, u *AppUpdate) {
	if u == nil {
		delete(f.updates, appID)
	} else {
		f.updates[appID] = *u
	}
}

// SetError sets an error to be returned by ListUpdates.
func (f *FakeUpdateSource) SetError(err error) {
	f.err = err
}

// ListUpdates returns all configured update entries.
func (f *FakeUpdateSource) ListUpdates() ([]AppUpdate, error) {
	if f.err != nil {
		return nil, f.err
	}
	result := make([]AppUpdate, 0, len(f.updates))
	for _, u := range f.updates {
		result = append(result, u)
	}
	return result, nil
}

// UpdateFor returns the update entry for the given app ID.
func (f *FakeUpdateSource) UpdateFor(appID string) (AppUpdate, bool) {
	u, ok := f.updates[appID]
	return u, ok
}