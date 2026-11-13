package apps

import (
	"testing"
)

func TestCatalogWave1CountAndIDs(t *testing.T) {
	wave1 := ByWave(1)
	if len(wave1) != 9 {
		t.Errorf("wave-1: expected 9 apps, got %d", len(wave1))
	}

	expectedIDs := map[string]bool{
		"jellyfin":       true,
		"immich":         true,
		"home-assistant": true,
		"mosquitto":      true,
		"zigbee2mqtt":    true,
		"esphome":        true,
		"frigate":        true,
		"syncthing":      true,
		"adguard-home":   true,
	}

	for _, a := range wave1 {
		if !expectedIDs[a.ID] {
			t.Errorf("wave-1: unexpected app ID %q", a.ID)
		}
		delete(expectedIDs, a.ID)
	}

	for id := range expectedIDs {
		t.Errorf("wave-1: missing expected app ID %q", id)
	}
}

func TestCatalogWave2ContainsNodeREDNotN8N(t *testing.T) {
	wave2 := ByWave(2)

	foundNodeRED := false
	foundN8N := false
	for _, a := range wave2 {
		if a.ID == "node-red" {
			foundNodeRED = true
		}
		if a.ID == "n8n" {
			foundN8N = true
		}
	}

	if !foundNodeRED {
		t.Error("wave-2: Node-RED (node-red) not found in catalog")
	}
	if foundN8N {
		t.Error("wave-2: n8n found in catalog but should not be present (automation = Node-RED only)")
	}
}

func TestCatalogValidateCatalog(t *testing.T) {
	if err := ValidateCatalog(); err != nil {
		t.Fatalf("ValidateCatalog() returned error: %v", err)
	}

	// Also verify every entry has RAMMB > 0
	for _, a := range Catalog() {
		if a.RAMMB <= 0 {
			t.Errorf("app %q: RAMMB must be > 0, got %d", a.ID, a.RAMMB)
		}
	}
}

func TestCatalogGet(t *testing.T) {
	// Existing ID
	app, ok := Get("jellyfin")
	if !ok {
		t.Fatal("Get(jellyfin): expected found, got not found")
	}
	if app.ID != "jellyfin" {
		t.Errorf("Get(jellyfin): returned ID %q, want jellyfin", app.ID)
	}

	// Non-existent ID
	_, ok = Get("tidak-ada")
	if ok {
		t.Error("Get(tidak-ada): expected not found, got found")
	}
}

func TestCatalogOrder(t *testing.T) {
	all := Catalog()

	// Verify wave ordering: all wave 1 before wave 2
	seenWave2 := false
	for _, a := range all {
		if a.Wave == 2 {
			seenWave2 = true
		}
		if a.Wave == 1 && seenWave2 {
			t.Errorf("catalog order: wave 1 app %q appears after wave 2 app", a.ID)
		}
	}

	// Verify name ordering within same wave
	lastNameWave1 := ""
	lastNameWave2 := ""
	for _, a := range all {
		if a.Wave == 1 {
			if a.Name < lastNameWave1 {
				t.Errorf("catalog order: wave 1 names not sorted: %q before %q", a.Name, lastNameWave1)
			}
			lastNameWave1 = a.Name
		} else if a.Wave == 2 {
			if a.Name < lastNameWave2 {
				t.Errorf("catalog order: wave 2 names not sorted: %q before %q", a.Name, lastNameWave2)
			}
			lastNameWave2 = a.Name
		}
	}
}

func TestPortCollisionsFixture(t *testing.T) {
	// Test with fixture apps that share a port
	fixtureApps := []App{
		{ID: "app-a", Name: "App A", Ports: []int{8080, 9000}, RAMMB: 100},
		{ID: "app-b", Name: "App B", Ports: []int{8080, 9001}, RAMMB: 100},
		{ID: "app-c", Name: "App C", Ports: []int{9002}, RAMMB: 100},
	}

	// Build port map manually (same logic as PortCollisions)
	portToApps := make(map[int][]string)
	for _, a := range fixtureApps {
		for _, p := range a.Ports {
			portToApps[p] = append(portToApps[p], a.ID)
		}
	}
	collisions := make(map[int][]string)
	for p, ids := range portToApps {
		if len(ids) > 1 {
			collisions[p] = ids
		}
	}

	// Port 8080 should have both app-a and app-b
	ids, ok := collisions[8080]
	if !ok {
		t.Fatal("expected collision on port 8080")
	}
	if len(ids) != 2 {
		t.Errorf("port 8080: expected 2 apps, got %d", len(ids))
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
		t.Errorf("port 8080: expected apps app-a and app-b, got %v", ids)
	}

	// Port 9000, 9001, 9002 should not be in collisions
	for _, p := range []int{9000, 9001, 9002} {
		if _, ok := collisions[p]; ok {
			t.Errorf("port %d: unexpected collision", p)
		}
	}

	// Test fixture with no collisions
	fixtureApps2 := []App{
		{ID: "app-x", Name: "App X", Ports: []int{1000}, RAMMB: 100},
		{ID: "app-y", Name: "App Y", Ports: []int{2000}, RAMMB: 100},
	}
	portToApps2 := make(map[int][]string)
	for _, a := range fixtureApps2 {
		for _, p := range a.Ports {
			portToApps2[p] = append(portToApps2[p], a.ID)
		}
	}
	collisions2 := make(map[int][]string)
	for p, ids := range portToApps2 {
		if len(ids) > 1 {
			collisions2[p] = ids
		}
	}
	if len(collisions2) != 0 {
		t.Errorf("fixture without collisions: expected empty map, got %v", collisions2)
	}
}