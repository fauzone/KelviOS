// Package agents seeded tests for the four built-in agents (FR-3.6).
// Seeds ONE pool + ONE backup repo via catalog funcs and asserts agents reflect seeded state.

package agents

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/storage"
)

func seedPoolAndBackupRepo(t *testing.T, db *sql.DB) (poolID, repoID string) {
	t.Helper()
	// Create a pool
	pool := &catalog.PoolRow{
		ID:         "pool-seed-1",
		Name:       "seed-pool",
		NodeID:     storage.DefaultNodeID,
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/seed",
		SizeBytes:  1000000000000, // 1 TB
		UsedBytes:  500000000000,  // 500 GB used
		Encrypted:  false,
		Status:     "active",
	}
	if err := catalog.CreatePool(db, pool, []string{"sda"}); err != nil {
		t.Fatalf("create pool: %v", err)
	}

	// Create a backup repo
	repo, err := catalog.CreateBackupRepo(db, "seed-repo", "local", "/mnt/backups/seed", "", "")
	if err != nil {
		t.Fatalf("create backup repo: %v", err)
	}

	return pool.ID, repo.ID
}

func TestBackupStewardSeededCountsRepo(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	seedPoolAndBackupRepo(t, db)

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
	// Should report 1 backup repo
	if !strings.Contains(report.Summary, "1 backup repo") {
		t.Errorf("expected summary to mention 1 backup repo, got: %s", report.Summary)
	}
	// Should list the repo in findings
	foundRepo := false
	for _, f := range report.Findings {
		if strings.Contains(f, "seed-repo") {
			foundRepo = true
			break
		}
	}
	if !foundRepo {
		t.Errorf("expected finding to mention seed-repo, findings: %v", report.Findings)
	}
}

func TestCapacityPlannerSeededListsPool(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	seedPoolAndBackupRepo(t, db)

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
	// Should report 1 pool analyzed
	if !strings.Contains(report.Summary, "1 pool") {
		t.Errorf("expected summary to mention 1 pool, got: %s", report.Summary)
	}
	// Should list the pool with ~50% usage
	foundPool := false
	for _, f := range report.Findings {
		if strings.Contains(f, "seed-pool") && strings.Contains(f, "50") {
			foundPool = true
			break
		}
	}
	if !foundPool {
		t.Errorf("expected finding to mention seed-pool with ~50%% usage, findings: %v", report.Findings)
	}
	// Projection limitation should still be present
	limitationText := "projection: CURRENT usage only (no usage-history table); days-to-full unavailable"
	if !strings.Contains(report.Summary, limitationText) {
		t.Errorf("expected summary to contain projection limitation text %q, got: %s", limitationText, report.Summary)
	}
}

func TestTierCuratorSeededNoHeatedCollectionsWithoutAccessEvents(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	seedPoolAndBackupRepo(t, db)

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
	// Should still report no heated collections (no access events recorded)
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
