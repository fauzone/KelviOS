package appupdate

import (
	"testing"
)

// TestStaticFeedListUpdates tests that the static feed returns the expected updates.
func TestStaticFeedListUpdates(t *testing.T) {
	feed := NewStaticFeed()
	updates, err := feed.ListUpdates()
	if err != nil {
		t.Fatalf("ListUpdates error: %v", err)
	}

	if len(updates) != 2 {
		t.Fatalf("expected 2 updates, got %d", len(updates))
	}

	// Verify adguard-home update
	foundAdGuard := false
	for _, u := range updates {
		if u.AppID == "adguard-home" {
			foundAdGuard = true
			if u.Version != "0.108.0" {
				t.Errorf("adguard-home version = %q, want 0.108.0", u.Version)
			}
			if u.ImageRef != "adguard/adguardhome:v0.108.0" {
				t.Errorf("adguard-home ImageRef = %q, want adguard/adguardhome:v0.108.0", u.ImageRef)
			}
			if u.RAMMB != 256 {
				t.Errorf("adguard-home RAMMB = %d, want 256", u.RAMMB)
			}
			// Check port 8443 added
			has8443 := false
			for _, p := range u.Ports {
				if p == 8443 {
					has8443 = true
					break
				}
			}
			if !has8443 {
				t.Errorf("adguard-home ports missing 8443: %v", u.Ports)
			}
			if u.Digest != "sha256:a1b2c3d4e5f6789012345678901234567890abcdef1234567890abcdef123456" {
				t.Errorf("adguard-home digest mismatch")
			}
		}
	}
	if !foundAdGuard {
		t.Error("adguard-home update not found")
	}

	// Verify esphome update
	foundESPhome := false
	for _, u := range updates {
		if u.AppID == "esphome" {
			foundESPhome = true
			if u.Version != "2024.12.0" {
				t.Errorf("esphome version = %q, want 2024.12.0", u.Version)
			}
			if u.ImageRef != "ghcr.io/esphome/esphome:2024.12.0" {
				t.Errorf("esphome ImageRef = %q, want ghcr.io/esphome/esphome:2024.12.0", u.ImageRef)
			}
			if u.RAMMB != 256 {
				t.Errorf("esphome RAMMB = %d, want 256", u.RAMMB)
			}
			// Ports unchanged
			if len(u.Ports) != 2 || u.Ports[0] != 6052 || u.Ports[1] != 6053 {
				t.Errorf("esphome ports = %v, want [6052 6053]", u.Ports)
			}
			if u.Digest != "sha256:f6e5d4c3b2a109876543210fedcba9876543210fedcba9876543210fedcba987" {
				t.Errorf("esphome digest mismatch")
			}
		}
	}
	if !foundESPhome {
		t.Error("esphome update not found")
	}
}

// TestStaticFeedUpdateFor tests UpdateFor returns correct entry.
func TestStaticFeedUpdateFor(t *testing.T) {
	feed := NewStaticFeed()

	// Existing app
	upd, ok := feed.UpdateFor("adguard-home")
	if !ok {
		t.Fatal("UpdateFor(adguard-home) not found")
	}
	if upd.Version != "0.108.0" {
		t.Errorf("version = %q, want 0.108.0", upd.Version)
	}

	// Non-existing app
	_, ok = feed.UpdateFor("nonexistent")
	if ok {
		t.Error("UpdateFor(nonexistent) should return false")
	}
}

// TestStaticFeedImmutability tests that returned updates are copies.
func TestStaticFeedImmutability(t *testing.T) {
	feed := NewStaticFeed()
	updates, _ := feed.ListUpdates()
	if len(updates) == 0 {
		t.Fatal("no updates")
	}
	// Mutate the returned slice
	updates[0].AppID = "mutated"
	// Original should be unchanged
	upd, _ := feed.UpdateFor("adguard-home")
	if upd.AppID == "mutated" {
		t.Error("ListUpdates returned mutable reference to internal data")
	}
}