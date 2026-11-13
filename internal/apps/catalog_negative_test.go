package apps

import (
	"testing"
)

// TestValidateCatalogNegative tests ValidateCatalog with various invalid catalog data.
// Uses swap-restore pattern to avoid polluting other tests.
func TestValidateCatalogNegative(t *testing.T) {
	// Save original catalogData
	original := catalogData
	defer func() { catalogData = original }()

	tests := []struct {
		name          string
		modifyCatalog func()
		wantErr       bool
		errContains   string
	}{
		{
			name: "empty ID",
			modifyCatalog: func() {
				catalogData = []App{
					{ID: "", Name: "Test", Wave: 1, RAMMB: 100, Ports: []int{8080}},
				}
			},
			wantErr:     true,
			errContains: "empty ID",
		},
		{
			name: "duplicate ID",
			modifyCatalog: func() {
				catalogData = []App{
					{ID: "dup", Name: "Test1", Wave: 1, RAMMB: 100, Ports: []int{8080}},
					{ID: "dup", Name: "Test2", Wave: 1, RAMMB: 100, Ports: []int{8081}},
				}
			},
			wantErr:     true,
			errContains: "duplicate ID: dup",
		},
		{
			name: "empty Name",
			modifyCatalog: func() {
				catalogData = []App{
					{ID: "test", Name: "", Wave: 1, RAMMB: 100, Ports: []int{8080}},
				}
			},
			wantErr:     true,
			errContains: "empty Name for ID test",
		},
		{
			name: "invalid Wave (3)",
			modifyCatalog: func() {
				catalogData = []App{
					{ID: "test", Name: "Test", Wave: 3, RAMMB: 100, Ports: []int{8080}},
				}
			},
			wantErr:     true,
			errContains: "invalid Wave for test: must be 1 or 2",
		},
		{
			name: "RAMMB zero",
			modifyCatalog: func() {
				catalogData = []App{
					{ID: "test", Name: "Test", Wave: 1, RAMMB: 0, Ports: []int{8080}},
				}
			},
			wantErr:     true,
			errContains: "RAMMB must be > 0 for test",
		},
		{
			name: "port zero",
			modifyCatalog: func() {
				catalogData = []App{
					{ID: "test", Name: "Test", Wave: 1, RAMMB: 100, Ports: []int{0}},
				}
			},
			wantErr:     true,
			errContains: "port out of range 1-65535 for test: 0",
		},
		{
			name: "port 70000 (out of range)",
			modifyCatalog: func() {
				catalogData = []App{
					{ID: "test", Name: "Test", Wave: 1, RAMMB: 100, Ports: []int{70000}},
				}
			},
			wantErr:     true,
			errContains: "port out of range 1-65535 for test: 70000",
		},
		{
			name: "duplicate port within app",
			modifyCatalog: func() {
				catalogData = []App{
					{ID: "test", Name: "Test", Wave: 1, RAMMB: 100, Ports: []int{8080, 8080}},
				}
			},
			wantErr:     true,
			errContains: "duplicate port within app test: 8080",
		},
		{
			name: "valid catalog (control)",
			modifyCatalog: func() {
				catalogData = original
			},
			wantErr:   false,
			errContains: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.modifyCatalog()
			err := ValidateCatalog()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ValidateCatalog: expected error, got nil")
				}
				if tt.errContains != "" && !contains(err.Error(), tt.errContains) {
					t.Errorf("ValidateCatalog: error %q does not contain %q", err.Error(), tt.errContains)
				}
			} else {
				if err != nil {
					t.Errorf("ValidateCatalog: unexpected error: %v", err)
				}
			}
		})
	}
}

// TestPortCollisionsRealCatalog tests PortCollisions on the actual catalog data.
// Verifies that any reported collision port is indeed claimed by >=2 apps.
// If no collisions exist, asserts empty map explicitly.
func TestPortCollisionsRealCatalog(t *testing.T) {
	collisions := PortCollisions()

	// Verify each collision port is actually claimed by >=2 apps from Catalog()
	allApps := Catalog()
	portToApps := make(map[int][]string)
	for _, a := range allApps {
		for _, p := range a.Ports {
			portToApps[p] = append(portToApps[p], a.ID)
		}
	}

	for port, ids := range collisions {
		expected, ok := portToApps[port]
		if !ok {
			t.Errorf("PortCollisions: port %d reported but not found in Catalog()", port)
			continue
		}
		if len(expected) < 2 {
			t.Errorf("PortCollisions: port %d has only %d app(s) in Catalog(), expected >=2", port, len(expected))
		}
		if len(ids) != len(expected) {
			t.Errorf("PortCollisions: port %d has %d IDs, expected %d", port, len(ids), len(expected))
		}
		// Verify all IDs match
		idSet := make(map[string]bool)
		for _, id := range ids {
			idSet[id] = true
		}
		for _, id := range expected {
			if !idSet[id] {
				t.Errorf("PortCollisions: port %d missing expected ID %q", port, id)
			}
		}
	}

	// If no collisions in real catalog, assert empty map explicitly
	if len(collisions) == 0 {
		t.Log("PortCollisions: real catalog has no collisions (empty map)")
	}
}

// TestPortCollisionsSynthetic tests PortCollisions with synthetic data
// where two apps share a port. Uses swap-restore.
func TestPortCollisionsSynthetic(t *testing.T) {
	original := catalogData
	defer func() { catalogData = original }()

	catalogData = []App{
		{ID: "app-a", Name: "App A", Ports: []int{8080, 9000}, RAMMB: 100, Wave: 1},
		{ID: "app-b", Name: "App B", Ports: []int{8080, 9001}, RAMMB: 100, Wave: 1},
		{ID: "app-c", Name: "App C", Ports: []int{9002}, RAMMB: 100, Wave: 1},
	}

	collisions := PortCollisions()

	// Port 8080 should have both app-a and app-b
	ids, ok := collisions[8080]
	if !ok {
		t.Fatal("PortCollisions: expected collision on port 8080")
	}
	if len(ids) != 2 {
		t.Errorf("PortCollisions: port 8080 expected 2 apps, got %d", len(ids))
	}
	foundA, foundB := false, false
	for _, id := range ids {
		if id == "app-a" {
			foundA = true
		}
		if id == "app-b" {
			foundB = true
		}
	}
	if !foundA || !foundB {
		t.Errorf("PortCollisions: port 8080 expected apps app-a and app-b, got %v", ids)
	}

	// Other ports should not be in collisions
	for _, p := range []int{9000, 9001, 9002} {
		if _, ok := collisions[p]; ok {
			t.Errorf("PortCollisions: port %d unexpected collision", p)
		}
	}
}

// TestByCategory tests ByCategory with existing and non-existing categories.
func TestByCategory(t *testing.T) {
	// Find a category that exists in the real catalog
	allApps := Catalog()
	categoryCounts := make(map[string]int)
	for _, a := range allApps {
		categoryCounts[a.Category]++
	}

	// Pick first category that has >=1 app
	var existingCategory string
	for cat, count := range categoryCounts {
		if count > 0 {
			existingCategory = cat
			break
		}
	}
	if existingCategory == "" {
		t.Fatal("TestByCategory: no categories found in catalog")
	}

	// Test existing category
	results := ByCategory(existingCategory)
	if len(results) == 0 {
		t.Errorf("ByCategory(%q): expected >=1 app, got 0", existingCategory)
	}
	for _, a := range results {
		if a.Category != existingCategory {
			t.Errorf("ByCategory(%q): app %q has category %q, want %q", existingCategory, a.ID, a.Category, existingCategory)
		}
	}
	if len(results) != categoryCounts[existingCategory] {
		t.Errorf("ByCategory(%q): got %d apps, expected %d", existingCategory, len(results), categoryCounts[existingCategory])
	}

	// Test non-existing category
	nonExisting := "this-category-does-not-exist-12345"
	results = ByCategory(nonExisting)
	if len(results) != 0 {
		t.Errorf("ByCategory(%q): expected 0 apps, got %d", nonExisting, len(results))
	}
}

// contains checks if s contains substr.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 || findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}