// Package agents tests for the four built-in agents (FR-3.6).
// Pure catalog tests using :memory: database — no HTTP, no external deps.

package agents

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/storage"
)

func TestRegistryListsFourAgents(t *testing.T) {
	all := All()
	if len(all) != 4 {
		t.Fatalf("expected 4 agents, got %d: %v", len(all), all)
	}
	names := make(map[string]bool)
	for _, a := range all {
		names[a.Name()] = true
	}
	expected := []string{"health_guardian", "backup_steward", "tier_curator", "capacity_planner"}
	for _, exp := range expected {
		if !names[exp] {
			t.Errorf("missing agent: %s", exp)
		}
	}
}

func TestGetCaseInsensitive(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"health_guardian", "health_guardian"},
		{"HEALTH_GUARDIAN", "health_guardian"},
		{"Health_Guardian", "health_guardian"},
		{"backup_steward", "backup_steward"},
		{"BACKUP_STEWARD", "backup_steward"},
		{"tier_curator", "tier_curator"},
		{"TIER_CURATOR", "tier_curator"},
		{"capacity_planner", "capacity_planner"},
		{"CAPACITY_PLANNER", "capacity_planner"},
	}
	for _, tc := range tests {
		a, ok := Get(tc.input)
		if !ok {
			t.Errorf("Get(%q) not found", tc.input)
			continue
		}
		if a.Name() != tc.expected {
			t.Errorf("Get(%q) returned %q, want %q", tc.input, a.Name(), tc.expected)
		}
	}
}

func TestGetUnknownReturnsNotFound(t *testing.T) {
	_, ok := Get("nonexistent_agent")
	if ok {
		t.Error("Get(unknown) should return false")
	}
}

func newTestDeps(t *testing.T, db *sql.DB) *Deps {
	t.Helper()
	return NewDeps(db, func() ([]storage.Disk, error) { return nil, nil }, time.Now)
}

func TestHealthGuardianEmptyCatalog(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	agent, ok := Get("health_guardian")
	if !ok {
		t.Fatal("health_guardian not registered")
	}

	deps := newTestDeps(t, db)
	report, err := agent.Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if report.Agent != "health_guardian" {
		t.Errorf("report.Agent = %q, want health_guardian", report.Agent)
	}
	if report.Summary == "" {
		t.Error("report.Summary should not be empty")
	}
	// Should report no disks found (early return when no disks)
	if !strings.Contains(report.Summary, "No disks found in inventory") {
		t.Errorf("expected summary to mention no disks found in inventory, got: %s", report.Summary)
	}
	// With no disks, findings and actions are empty (early return)
	// This is honest behavior - no fabricated verdicts
	if len(report.Findings) != 0 {
		t.Errorf("expected empty findings with no disks, got: %v", report.Findings)
	}
	if len(report.Actions) != 0 {
		t.Errorf("expected empty actions with no disks, got: %v", report.Actions)
	}
}

func TestBackupStewardEmptyCatalog(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	agent, ok := Get("backup_steward")
	if !ok {
		t.Fatal("backup_steward not registered")
	}

	deps := newTestDeps(t, db)
	report, err := agent.Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if report.Agent != "backup_steward" {
		t.Errorf("report.Agent = %q, want backup_steward", report.Agent)
	}
	if report.Summary == "" {
		t.Error("report.Summary should not be empty")
	}
	// Should report zero repos
	if !strings.Contains(report.Summary, "No backup repositories configured") {
		t.Errorf("expected summary to mention no backup repositories configured, got: %s", report.Summary)
	}
	// Should have action to configure a repo (exact text from agent)
	foundConfigureAction := false
	for _, a := range report.Actions {
		if strings.Contains(a, "Configure a backup repository via POST /api/backups/repos") {
			foundConfigureAction = true
			break
		}
	}
	if !foundConfigureAction {
		t.Errorf("expected action to configure backup repository, actions: %v", report.Actions)
	}
}

func TestTierCuratorEmptyCatalog(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	agent, ok := Get("tier_curator")
	if !ok {
		t.Fatal("tier_curator not registered")
	}

	deps := newTestDeps(t, db)
	report, err := agent.Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if report.Agent != "tier_curator" {
		t.Errorf("report.Agent = %q, want tier_curator", report.Agent)
	}
	if report.Summary == "" {
		t.Error("report.Summary should not be empty")
	}
	// Should report no heated collections
	if !strings.Contains(report.Summary, "no heated collections") {
		t.Errorf("expected summary to mention no heated collections, got: %s", report.Summary)
	}
	// Should have action about heat data
	foundHeatAction := false
	for _, a := range report.Actions {
		if strings.Contains(strings.ToLower(a), "heat") || strings.Contains(strings.ToLower(a), "access event") {
			foundHeatAction = true
			break
		}
	}
	if !foundHeatAction {
		t.Errorf("expected action about heat/access events, actions: %v", report.Actions)
	}
}

func TestCapacityPlannerEmptyCatalog(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	agent, ok := Get("capacity_planner")
	if !ok {
		t.Fatal("capacity_planner not registered")
	}

	deps := newTestDeps(t, db)
	report, err := agent.Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if report.Agent != "capacity_planner" {
		t.Errorf("report.Agent = %q, want capacity_planner", report.Agent)
	}
	if report.Summary == "" {
		t.Error("report.Summary should not be empty")
	}
	// Should report no pools configured (early return when no pools)
	if !strings.Contains(report.Summary, "No pools configured") {
		t.Errorf("expected summary to mention no pools configured, got: %s", report.Summary)
	}
	// With no pools, findings and actions are minimal (early return)
	// The projection limitation text is only added when pools exist
	if len(report.Findings) != 0 {
		t.Errorf("expected empty findings with no pools, got: %v", report.Findings)
	}
	// Should have action to create a pool (exact text from agent)
	foundCreatePoolAction := false
	for _, a := range report.Actions {
		if strings.Contains(a, "Create a storage pool via POST /api/pools") {
			foundCreatePoolAction = true
			break
		}
	}
	if !foundCreatePoolAction {
		t.Errorf("expected action to create storage pool, actions: %v", report.Actions)
	}
}
