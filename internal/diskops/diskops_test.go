// Package diskops provides pure planning logic for disk replacement and
// expansion operations.
package diskops

import (
	"testing"
)

func TestStrategyFor(t *testing.T) {
	tests := []struct {
		mechanism string
		want      Strategy
	}{
		{"single", StrategyCopyOut},
		{"mirror", StrategyCopyOut},
		{"parity-scheduled", StrategyRebuildParity},
		{"zfs", StrategyNativeReplace},
		{"stripe", StrategyUnsupported},
		{"unknown-mechanism", StrategyUnsupported},
		{"", StrategyUnsupported},
	}

	for _, tc := range tests {
		t.Run(tc.mechanism, func(t *testing.T) {
			got := StrategyFor(tc.mechanism)
			if got != tc.want {
				t.Errorf("StrategyFor(%q) = %q, want %q", tc.mechanism, got, tc.want)
			}
		})
	}
}

func TestBuildPlan_Validations(t *testing.T) {
	poolDisks := []DiskRef{
		{Name: "sda", SizeBytes: 4_000_000_000_000},
		{Name: "sdb", SizeBytes: 4_000_000_000_000},
	}

	t.Run("replacement already in pool", func(t *testing.T) {
		_, err := BuildPlan("pool1", poolDisks,
			DiskRef{Name: "sda", SizeBytes: 4_000_000_000_000},
			DiskRef{Name: "sdb", SizeBytes: 6_000_000_000_000}, // sdb already in pool
			"mirror")
		if err == nil {
			t.Error("expected error for replacement already in pool")
		}
		if !contains(err.Error(), "already in pool") {
			t.Errorf("wrong error: %v", err)
		}
	})

	t.Run("failing disk not in pool", func(t *testing.T) {
		_, err := BuildPlan("pool1", poolDisks,
			DiskRef{Name: "sdc", SizeBytes: 4_000_000_000_000}, // sdc not in pool
			DiskRef{Name: "sdd", SizeBytes: 6_000_000_000_000},
			"mirror")
		if err == nil {
			t.Error("expected error for failing disk not in pool")
		}
		if !contains(err.Error(), "not in pool") {
			t.Errorf("wrong error: %v", err)
		}
	})

	t.Run("replacement smaller than failing", func(t *testing.T) {
		_, err := BuildPlan("pool1", poolDisks,
			DiskRef{Name: "sda", SizeBytes: 4_000_000_000_000},
			DiskRef{Name: "sdd", SizeBytes: 2_000_000_000_000}, // smaller
			"mirror")
		if err == nil {
			t.Error("expected error for replacement too small")
		}
		if !contains(err.Error(), "smaller") {
			t.Errorf("wrong error: %v", err)
		}
	})
}

func TestBuildExpansionPlan_Validations(t *testing.T) {
	poolDisks := []DiskRef{
		{Name: "sda", SizeBytes: 4_000_000_000_000},
		{Name: "sdb", SizeBytes: 4_000_000_000_000},
	}

	t.Run("new disk already in pool", func(t *testing.T) {
		_, err := BuildExpansionPlan("pool1", poolDisks,
			DiskRef{Name: "sda", SizeBytes: 2_000_000_000_000}, // sda already in pool
			"mirror")
		if err == nil {
			t.Error("expected error for new disk already in pool")
		}
		if !contains(err.Error(), "already in pool") {
			t.Errorf("wrong error: %v", err)
		}
	})
}

func TestBuildPlan_Preview(t *testing.T) {
	// Pool: 2x 4TB mirror -> usable = 4TB (min of both)
	// Replace one 4TB with 6TB -> still mirror, min is 4TB (other disk) -> usable = 4TB
	poolDisks := []DiskRef{
		{Name: "sda", SizeBytes: 4_000_000_000_000},
		{Name: "sdb", SizeBytes: 4_000_000_000_000},
	}

	t.Run("mirror replace 4TB with 6TB - usable unchanged", func(t *testing.T) {
		plan, err := BuildPlan("pool1", poolDisks,
			DiskRef{Name: "sda", SizeBytes: 4_000_000_000_000},
			DiskRef{Name: "sdc", SizeBytes: 6_000_000_000_000},
			"mirror")
		if err != nil {
			t.Fatalf("BuildPlan: %v", err)
		}
		if plan.Preview.OldUsableBytes != 4_000_000_000_000 {
			t.Errorf("OldUsableBytes = %d, want 4TB", plan.Preview.OldUsableBytes)
		}
		if plan.Preview.NewUsableBytes != 4_000_000_000_000 {
			t.Errorf("NewUsableBytes = %d, want 4TB (limited by other 4TB disk)", plan.Preview.NewUsableBytes)
		}
		if plan.Preview.DeltaBytes != 0 {
			t.Errorf("DeltaBytes = %d, want 0", plan.Preview.DeltaBytes)
		}
		if plan.Strategy != StrategyCopyOut {
			t.Errorf("Strategy = %q, want copy-out", plan.Strategy)
		}
	})

	t.Run("single replace 4TB with 6TB - usable increases", func(t *testing.T) {
		poolDisks := []DiskRef{{Name: "sda", SizeBytes: 4_000_000_000_000}}
		plan, err := BuildPlan("pool1", poolDisks,
			DiskRef{Name: "sda", SizeBytes: 4_000_000_000_000},
			DiskRef{Name: "sdc", SizeBytes: 6_000_000_000_000},
			"single")
		if err != nil {
			t.Fatalf("BuildPlan: %v", err)
		}
		if plan.Preview.OldUsableBytes != 4_000_000_000_000 {
			t.Errorf("OldUsableBytes = %d, want 4TB", plan.Preview.OldUsableBytes)
		}
		if plan.Preview.NewUsableBytes != 6_000_000_000_000 {
			t.Errorf("NewUsableBytes = %d, want 6TB", plan.Preview.NewUsableBytes)
		}
		if plan.Preview.DeltaBytes != 2_000_000_000_000 {
			t.Errorf("DeltaBytes = %d, want 2TB", plan.Preview.DeltaBytes)
		}
	})

	t.Run("parity-scheduled 3x4TB replace one 4TB with 6TB", func(t *testing.T) {
		// 3 disks, 1 parity -> usable = 2 * min(4,4,4) = 8TB
		// After: 4,4,6 -> min=4 -> usable = 2*4 = 8TB (no change)
		poolDisks := []DiskRef{
			{Name: "sda", SizeBytes: 4_000_000_000_000},
			{Name: "sdb", SizeBytes: 4_000_000_000_000},
			{Name: "sdc", SizeBytes: 4_000_000_000_000},
		}
		plan, err := BuildPlan("pool1", poolDisks,
			DiskRef{Name: "sda", SizeBytes: 4_000_000_000_000},
			DiskRef{Name: "sdd", SizeBytes: 6_000_000_000_000},
			"parity-scheduled")
		if err != nil {
			t.Fatalf("BuildPlan: %v", err)
		}
		if plan.Preview.OldUsableBytes != 8_000_000_000_000 {
			t.Errorf("OldUsableBytes = %d, want 8TB", plan.Preview.OldUsableBytes)
		}
		if plan.Preview.NewUsableBytes != 8_000_000_000_000 {
			t.Errorf("NewUsableBytes = %d, want 8TB (limited by 4TB disks)", plan.Preview.NewUsableBytes)
		}
		if plan.Strategy != StrategyRebuildParity {
			t.Errorf("Strategy = %q, want rebuild-parity", plan.Strategy)
		}
	})
}

func TestBuildExpansionPlan_Preview(t *testing.T) {
	t.Run("mirror expansion +2TB", func(t *testing.T) {
		// 2x 4TB mirror -> usable = 4TB
		// Add 2TB -> 3 disks mirror -> usable = min(4,4,2) = 2TB (DECREASES!)
		// This is correct behavior - adding smaller disk to mirror reduces usable
		poolDisks := []DiskRef{
			{Name: "sda", SizeBytes: 4_000_000_000_000},
			{Name: "sdb", SizeBytes: 4_000_000_000_000},
		}
		plan, err := BuildExpansionPlan("pool1", poolDisks,
			DiskRef{Name: "sdc", SizeBytes: 2_000_000_000_000},
			"mirror")
		if err != nil {
			t.Fatalf("BuildExpansionPlan: %v", err)
		}
		if plan.Preview.OldUsableBytes != 4_000_000_000_000 {
			t.Errorf("OldUsableBytes = %d, want 4TB", plan.Preview.OldUsableBytes)
		}
		if plan.Preview.NewUsableBytes != 2_000_000_000_000 {
			t.Errorf("NewUsableBytes = %d, want 2TB (limited by new 2TB disk)", plan.Preview.NewUsableBytes)
		}
		if plan.Preview.DeltaBytes != -2_000_000_000_000 {
			t.Errorf("DeltaBytes = %d, want -2TB", plan.Preview.DeltaBytes)
		}
	})

	t.Run("single expansion +2TB", func(t *testing.T) {
		// Single disk 4TB -> add 2TB? Single mechanism doesn't really expand
		// But per our calculateUsableBytes, single uses first disk only
		poolDisks := []DiskRef{{Name: "sda", SizeBytes: 4_000_000_000_000}}
		plan, err := BuildExpansionPlan("pool1", poolDisks,
			DiskRef{Name: "sdb", SizeBytes: 2_000_000_000_000},
			"single")
		if err != nil {
			t.Fatalf("BuildExpansionPlan: %v", err)
		}
		// Single mechanism: usable = first disk size (4TB), adding second doesn't change
		if plan.Preview.OldUsableBytes != 4_000_000_000_000 {
			t.Errorf("OldUsableBytes = %d, want 4TB", plan.Preview.OldUsableBytes)
		}
		if plan.Preview.NewUsableBytes != 4_000_000_000_000 {
			t.Errorf("NewUsableBytes = %d, want 4TB (single uses first disk)", plan.Preview.NewUsableBytes)
		}
	})

	t.Run("parity-scheduled expansion +2TB (3->4 disks)", func(t *testing.T) {
		// 3x 4TB parity -> usable = 2 * 4 = 8TB
		// Add 2TB -> 4 disks, min=2 -> usable = 3 * 2 = 6TB (decreases!)
		poolDisks := []DiskRef{
			{Name: "sda", SizeBytes: 4_000_000_000_000},
			{Name: "sdb", SizeBytes: 4_000_000_000_000},
			{Name: "sdc", SizeBytes: 4_000_000_000_000},
		}
		plan, err := BuildExpansionPlan("pool1", poolDisks,
			DiskRef{Name: "sdd", SizeBytes: 2_000_000_000_000},
			"parity-scheduled")
		if err != nil {
			t.Fatalf("BuildExpansionPlan: %v", err)
		}
		if plan.Preview.OldUsableBytes != 8_000_000_000_000 {
			t.Errorf("OldUsableBytes = %d, want 8TB", plan.Preview.OldUsableBytes)
		}
		if plan.Preview.NewUsableBytes != 6_000_000_000_000 {
			t.Errorf("NewUsableBytes = %d, want 6TB (3 data * 2TB min)", plan.Preview.NewUsableBytes)
		}
	})

	t.Run("stripe expansion +2TB", func(t *testing.T) {
		// 2x 4TB stripe -> usable = 8TB
		// Add 2TB -> usable = 10TB
		poolDisks := []DiskRef{
			{Name: "sda", SizeBytes: 4_000_000_000_000},
			{Name: "sdb", SizeBytes: 4_000_000_000_000},
		}
		plan, err := BuildExpansionPlan("pool1", poolDisks,
			DiskRef{Name: "sdc", SizeBytes: 2_000_000_000_000},
			"stripe")
		if err != nil {
			t.Fatalf("BuildExpansionPlan: %v", err)
		}
		if plan.Preview.OldUsableBytes != 8_000_000_000_000 {
			t.Errorf("OldUsableBytes = %d, want 8TB", plan.Preview.OldUsableBytes)
		}
		if plan.Preview.NewUsableBytes != 10_000_000_000_000 {
			t.Errorf("NewUsableBytes = %d, want 10TB", plan.Preview.NewUsableBytes)
		}
		if plan.Preview.DeltaBytes != 2_000_000_000_000 {
			t.Errorf("DeltaBytes = %d, want 2TB", plan.Preview.DeltaBytes)
		}
	})
}

func TestVerifyInventory(t *testing.T) {
	before := []FileRef{
		{Path: "a.txt", SizeBytes: 100, SHA256: "hash1"},
		{Path: "b.txt", SizeBytes: 200, SHA256: "hash2"},
		{Path: "c.txt", SizeBytes: 300, SHA256: "hash3"},
	}

	t.Run("identical -> no mismatch", func(t *testing.T) {
		after := []FileRef{
			{Path: "a.txt", SizeBytes: 100, SHA256: "hash1"},
			{Path: "b.txt", SizeBytes: 200, SHA256: "hash2"},
			{Path: "c.txt", SizeBytes: 300, SHA256: "hash3"},
		}
		mismatches := VerifyInventory(before, after)
		if len(mismatches) != 0 {
			t.Errorf("expected 0 mismatches, got %d: %v", len(mismatches), mismatches)
		}
	})

	t.Run("one hash changed", func(t *testing.T) {
		after := []FileRef{
			{Path: "a.txt", SizeBytes: 100, SHA256: "hash1"},
			{Path: "b.txt", SizeBytes: 200, SHA256: "hash2-CHANGED"},
			{Path: "c.txt", SizeBytes: 300, SHA256: "hash3"},
		}
		mismatches := VerifyInventory(before, after)
		if len(mismatches) != 1 {
			t.Errorf("expected 1 mismatch, got %d: %v", len(mismatches), mismatches)
		}
		if !contains(mismatches[0], "hash mismatch") || !contains(mismatches[0], "b.txt") {
			t.Errorf("wrong mismatch: %v", mismatches)
		}
	})

	t.Run("one file missing", func(t *testing.T) {
		after := []FileRef{
			{Path: "a.txt", SizeBytes: 100, SHA256: "hash1"},
			{Path: "c.txt", SizeBytes: 300, SHA256: "hash3"},
		}
		mismatches := VerifyInventory(before, after)
		if len(mismatches) != 1 {
			t.Errorf("expected 1 mismatch, got %d: %v", len(mismatches), mismatches)
		}
		if !contains(mismatches[0], "missing") || !contains(mismatches[0], "b.txt") {
			t.Errorf("wrong mismatch: %v", mismatches)
		}
	})

	t.Run("one extra file", func(t *testing.T) {
		after := []FileRef{
			{Path: "a.txt", SizeBytes: 100, SHA256: "hash1"},
			{Path: "b.txt", SizeBytes: 200, SHA256: "hash2"},
			{Path: "c.txt", SizeBytes: 300, SHA256: "hash3"},
			{Path: "d.txt", SizeBytes: 400, SHA256: "hash4"},
		}
		mismatches := VerifyInventory(before, after)
		if len(mismatches) != 1 {
			t.Errorf("expected 1 mismatch, got %d: %v", len(mismatches), mismatches)
		}
		if !contains(mismatches[0], "extra") || !contains(mismatches[0], "d.txt") {
			t.Errorf("wrong mismatch: %v", mismatches)
		}
	})

	t.Run("size mismatch", func(t *testing.T) {
		after := []FileRef{
			{Path: "a.txt", SizeBytes: 100, SHA256: "hash1"},
			{Path: "b.txt", SizeBytes: 250, SHA256: "hash2"}, // size changed
			{Path: "c.txt", SizeBytes: 300, SHA256: "hash3"},
		}
		mismatches := VerifyInventory(before, after)
		if len(mismatches) != 1 {
			t.Errorf("expected 1 mismatch, got %d: %v", len(mismatches), mismatches)
		}
		if !contains(mismatches[0], "size mismatch") || !contains(mismatches[0], "b.txt") {
			t.Errorf("wrong mismatch: %v", mismatches)
		}
	})

	t.Run("multiple mismatches", func(t *testing.T) {
		after := []FileRef{
			{Path: "a.txt", SizeBytes: 100, SHA256: "hash1-CHANGED"}, // hash mismatch
			// b.txt missing
			{Path: "c.txt", SizeBytes: 300, SHA256: "hash3"},
			{Path: "d.txt", SizeBytes: 400, SHA256: "hash4"}, // extra
		}
		mismatches := VerifyInventory(before, after)
		if len(mismatches) != 3 {
			t.Errorf("expected 3 mismatches, got %d: %v", len(mismatches), mismatches)
		}
	})
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || contains(s[1:], substr)))
}
