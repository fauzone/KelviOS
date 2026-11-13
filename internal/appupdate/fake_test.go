package appupdate

import (
	"errors"
	"testing"
)

// TestFakeUpdateSourceSetUpdate tests setting and retrieving updates.
func TestFakeUpdateSourceSetUpdate(t *testing.T) {
	fake := NewFakeUpdateSource()

	// Initially empty
	updates, err := fake.ListUpdates()
	if err != nil {
		t.Fatalf("ListUpdates error: %v", err)
	}
	if len(updates) != 0 {
		t.Errorf("expected 0 updates, got %d", len(updates))
	}

	// Set an update
	upd := &AppUpdate{
		AppID:        "test-app",
		Version:      "1.0.0",
		ImageRef:     "test/test:v1.0.0",
		ReleaseNotes: "Test release",
		Ports:        []int{8080},
		RAMMB:        128,
		Digest:       "sha256:abc123",
	}
	fake.SetUpdate("test-app", upd)

	// Retrieve via ListUpdates
	updates, err = fake.ListUpdates()
	if err != nil {
		t.Fatalf("ListUpdates error: %v", err)
	}
	if len(updates) != 1 {
		t.Fatalf("expected 1 update, got %d", len(updates))
	}
	if updates[0].AppID != "test-app" {
		t.Errorf("AppID = %q, want test-app", updates[0].AppID)
	}
	if updates[0].Version != "1.0.0" {
		t.Errorf("Version = %q, want 1.0.0", updates[0].Version)
	}

	// Retrieve via UpdateFor
	got, ok := fake.UpdateFor("test-app")
	if !ok {
		t.Fatal("UpdateFor(test-app) not found")
	}
	if got.Version != "1.0.0" {
		t.Errorf("Version = %q, want 1.0.0", got.Version)
	}

	// Non-existing app
	_, ok = fake.UpdateFor("nonexistent")
	if ok {
		t.Error("UpdateFor(nonexistent) should return false")
	}
}

// TestFakeUpdateSourceSetUpdateNil tests removing an update by setting nil.
func TestFakeUpdateSourceSetUpdateNil(t *testing.T) {
	fake := NewFakeUpdateSource()
	upd := &AppUpdate{AppID: "test-app", Version: "1.0.0", ImageRef: "test:v1"}
	fake.SetUpdate("test-app", upd)

	// Remove by setting nil
	fake.SetUpdate("test-app", nil)

	updates, err := fake.ListUpdates()
	if err != nil {
		t.Fatalf("ListUpdates error: %v", err)
	}
	if len(updates) != 0 {
		t.Errorf("expected 0 updates after nil, got %d", len(updates))
	}

	_, ok := fake.UpdateFor("test-app")
	if ok {
		t.Error("UpdateFor should return false after nil")
	}
}

// TestFakeUpdateSourceSetError tests error injection.
func TestFakeUpdateSourceSetError(t *testing.T) {
	fake := NewFakeUpdateSource()
	upd := &AppUpdate{AppID: "test-app", Version: "1.0.0", ImageRef: "test:v1"}
	fake.SetUpdate("test-app", upd)

	// Set error
	testErr := errors.New("feed unavailable")
	fake.SetError(testErr)

	// ListUpdates should return error
	_, err := fake.ListUpdates()
	if err == nil {
		t.Fatal("expected error from ListUpdates")
	}
	if err.Error() != "feed unavailable" {
		t.Errorf("error = %q, want feed unavailable", err.Error())
	}

	// UpdateFor should still work (not affected by ListUpdates error)
	got, ok := fake.UpdateFor("test-app")
	if !ok {
		t.Fatal("UpdateFor should still work")
	}
	if got.AppID != "test-app" {
		t.Errorf("AppID = %q, want test-app", got.AppID)
	}

	// Clear error
	fake.SetError(nil)
	updates, err := fake.ListUpdates()
	if err != nil {
		t.Fatalf("ListUpdates error after clear: %v", err)
	}
	if len(updates) != 1 {
		t.Errorf("expected 1 update after clear, got %d", len(updates))
	}
}

// TestFakeUpdateSourceMultipleUpdates tests multiple apps.
func TestFakeUpdateSourceMultipleUpdates(t *testing.T) {
	fake := NewFakeUpdateSource()

	fake.SetUpdate("app-a", &AppUpdate{AppID: "app-a", Version: "1.0", ImageRef: "a:v1", Ports: []int{8000}, RAMMB: 100})
	fake.SetUpdate("app-b", &AppUpdate{AppID: "app-b", Version: "2.0", ImageRef: "b:v2", Ports: []int{9000}, RAMMB: 200})

	updates, err := fake.ListUpdates()
	if err != nil {
		t.Fatalf("ListUpdates error: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("expected 2 updates, got %d", len(updates))
	}

	// Verify both present (order not guaranteed)
	foundA, foundB := false, false
	for _, u := range updates {
		if u.AppID == "app-a" {
			foundA = true
			if u.RAMMB != 100 {
				t.Errorf("app-a RAMMB = %d, want 100", u.RAMMB)
			}
		}
		if u.AppID == "app-b" {
			foundB = true
			if u.RAMMB != 200 {
				t.Errorf("app-b RAMMB = %d, want 200", u.RAMMB)
			}
		}
	}
	if !foundA || !foundB {
		t.Error("not all updates found")
	}
}