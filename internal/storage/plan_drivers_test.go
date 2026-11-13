package storage

import (
	"errors"
	"testing"
)

func TestPlanPool_ArchitectureExample(t *testing.T) {
	// ARCHITECTURE §5 example: disks 2, 4, 8, 8 TB → parity-scheduled usable 14 TB, overhead 8 TB
	disks := []PlanDisk{
		{Name: "sda", SizeBytes: 2 * TB},
		{Name: "sdb", SizeBytes: 4 * TB},
		{Name: "sdc", SizeBytes: 8 * TB},
		{Name: "sdd", SizeBytes: 8 * TB},
	}

	plan, err := PlanPool(MechanismParityScheduled, disks)
	if err != nil {
		t.Fatalf("PlanPool parity-scheduled: %v", err)
	}
	if plan.UsableBytes != 14*TB {
		t.Errorf("usable: got %d, want %d", plan.UsableBytes, 14*TB)
	}
	if plan.OverheadBytes != 8*TB {
		t.Errorf("overhead: got %d, want %d", plan.OverheadBytes, 8*TB)
	}
	if plan.Mechanism != MechanismParityScheduled {
		t.Errorf("mechanism: got %q", plan.Mechanism)
	}
}

func TestPlanPool_Mirror(t *testing.T) {
	// mirror 2+4 TB → usable 3 TB, overhead 3 TB
	disks := []PlanDisk{
		{Name: "sda", SizeBytes: 2 * TB},
		{Name: "sdb", SizeBytes: 4 * TB},
	}

	plan, err := PlanPool(MechanismMirror, disks)
	if err != nil {
		t.Fatalf("PlanPool mirror: %v", err)
	}
	if plan.UsableBytes != 3*TB {
		t.Errorf("usable: got %d, want %d", plan.UsableBytes, 3*TB)
	}
	if plan.OverheadBytes != 3*TB {
		t.Errorf("overhead: got %d, want %d", plan.OverheadBytes, 3*TB)
	}
	if plan.Mechanism != MechanismMirror {
		t.Errorf("mechanism: got %q", plan.Mechanism)
	}
	if plan.RequiresConfirmation {
		t.Errorf("mirror should not require confirmation")
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("mirror should have no warnings: %v", plan.Warnings)
	}
}

func TestPlanPool_Stripe(t *testing.T) {
	// stripe 2+4 TB → usable 6 TB, RequiresConfirmation true, exact warning
	disks := []PlanDisk{
		{Name: "sda", SizeBytes: 2 * TB},
		{Name: "sdb", SizeBytes: 4 * TB},
	}

	plan, err := PlanPool(MechanismStripe, disks)
	if err != nil {
		t.Fatalf("PlanPool stripe: %v", err)
	}
	if plan.UsableBytes != 6*TB {
		t.Errorf("usable: got %d, want %d", plan.UsableBytes, 6*TB)
	}
	if plan.OverheadBytes != 0 {
		t.Errorf("overhead: got %d, want 0", plan.OverheadBytes)
	}
	if !plan.RequiresConfirmation {
		t.Error("stripe must require confirmation")
	}
	if len(plan.Warnings) != 1 {
		t.Fatalf("expected exactly 1 warning, got %d: %v", len(plan.Warnings), plan.Warnings)
	}
	const expectedWarning = "RAID0 striping has no protection: if one disk fails, the entire pool is lost."
	if plan.Warnings[0] != expectedWarning {
		t.Errorf("warning mismatch:\ngot:  %q\nexp:  %q", plan.Warnings[0], expectedWarning)
	}
}

func TestPlanPool_Single(t *testing.T) {
	// single 8 TB → usable 8 TB
	disks := []PlanDisk{{Name: "sda", SizeBytes: 8 * TB}}

	plan, err := PlanPool(MechanismSingle, disks)
	if err != nil {
		t.Fatalf("PlanPool single: %v", err)
	}
	if plan.UsableBytes != 8*TB {
		t.Errorf("usable: got %d, want %d", plan.UsableBytes, 8*TB)
	}
	if plan.OverheadBytes != 0 {
		t.Errorf("overhead: got %d, want 0", plan.OverheadBytes)
	}
	if plan.RequiresConfirmation {
		t.Error("single should not require confirmation")
	}
	if len(plan.Warnings) != 0 {
		t.Errorf("single should have no warnings: %v", plan.Warnings)
	}
}

func TestPlanPool_Single_TwoDisks_Error(t *testing.T) {
	disks := []PlanDisk{
		{Name: "sda", SizeBytes: 2 * TB},
		{Name: "sdb", SizeBytes: 4 * TB},
	}
	_, err := PlanPool(MechanismSingle, disks)
	if err == nil {
		t.Error("single with 2 disks must error")
	}
	if !errors.Is(err, ErrSingleDiskCount) {
		t.Errorf("expected ErrSingleDiskCount, got %v", err)
	}
}

func TestPlanPool_Mirror_OneDisk_Error(t *testing.T) {
	disks := []PlanDisk{{Name: "sda", SizeBytes: 2 * TB}}
	_, err := PlanPool(MechanismMirror, disks)
	if err == nil {
		t.Error("mirror with 1 disk must error")
	}
	if !errors.Is(err, ErrMinTwoDisks) {
		t.Errorf("expected ErrMinTwoDisks, got %v", err)
	}
}

func TestPlanPool_Stripe_OneDisk_Error(t *testing.T) {
	disks := []PlanDisk{{Name: "sda", SizeBytes: 2 * TB}}
	_, err := PlanPool(MechanismStripe, disks)
	if err == nil {
		t.Error("stripe with 1 disk must error")
	}
	if !errors.Is(err, ErrMinTwoDisks) {
		t.Errorf("expected ErrMinTwoDisks, got %v", err)
	}
}

func TestPlanPool_Parity_OneDisk_Error(t *testing.T) {
	disks := []PlanDisk{{Name: "sda", SizeBytes: 2 * TB}}
	_, err := PlanPool(MechanismParityScheduled, disks)
	if err == nil {
		t.Error("parity-scheduled with 1 disk must error")
	}
	if !errors.Is(err, ErrMinTwoDisks) {
		t.Errorf("expected ErrMinTwoDisks, got %v", err)
	}
}

func TestPlanPool_ZFS_Error(t *testing.T) {
	disks := []PlanDisk{
		{Name: "sda", SizeBytes: 2 * TB},
		{Name: "sdb", SizeBytes: 4 * TB},
	}
	_, err := PlanPool(MechanismZFS, disks)
	if err == nil {
		t.Error("zfs must error in Phase 1")
	}
	if !errors.Is(err, ErrZFSNotPlanned) {
		t.Errorf("expected ErrZFSNotPlanned, got %v", err)
	}
}

func TestPlanPool_UnknownMechanism_Error(t *testing.T) {
	disks := []PlanDisk{{Name: "sda", SizeBytes: 2 * TB}}
	_, err := PlanPool(Mechanism("bogus"), disks)
	if err == nil {
		t.Error("unknown mechanism must error")
	}
	if !errors.Is(err, ErrUnknownMechanism) {
		t.Errorf("expected ErrUnknownMechanism, got %v", err)
	}
}

func TestPlanPool_EmptyDisks_Error(t *testing.T) {
	_, err := PlanPool(MechanismSingle, []PlanDisk{})
	if err == nil {
		t.Error("empty disks must error")
	}
	if !errors.Is(err, ErrEmptyDisks) {
		t.Errorf("expected ErrEmptyDisks, got %v", err)
	}
}

func TestPlanPool_ZeroSizeDisk_Error(t *testing.T) {
	disks := []PlanDisk{{Name: "sda", SizeBytes: 0}}
	_, err := PlanPool(MechanismSingle, disks)
	if err == nil {
		t.Error("zero size disk must error")
	}
	if !errors.Is(err, ErrInvalidDiskSize) {
		t.Errorf("expected ErrInvalidDiskSize, got %v", err)
	}
}

func TestPlanPool_NegativeSizeDisk_Error(t *testing.T) {
	disks := []PlanDisk{{Name: "sda", SizeBytes: -1}}
	_, err := PlanPool(MechanismSingle, disks)
	if err == nil {
		t.Error("negative size disk must error")
	}
	if !errors.Is(err, ErrInvalidDiskSize) {
		t.Errorf("expected ErrInvalidDiskSize, got %v", err)
	}
}

func TestDriverRegistry(t *testing.T) {
	registry := DriverRegistry()
	if len(registry) != 4 {
		t.Fatalf("expected 4 drivers, got %d: %v", len(registry), registry)
	}

	// Order: single, mergerfs, btrfs, zfs
	expectedNames := []string{"single", "mergerfs", "btrfs", "zfs"}
	for i, name := range expectedNames {
		if registry[i].Name != name {
			t.Errorf("driver[%d]: expected %q, got %q", i, name, registry[i].Name)
		}
	}

	// single: Available = true
	if !registry[0].Available {
		t.Error("single driver must be available")
	}
	if registry[0].Reason != "" {
		t.Errorf("single driver reason should be empty, got %q", registry[0].Reason)
	}
	// Caps from SingleDriver{}.Capabilities()
	singleCaps := (&SingleDriver{}).Capabilities()
	if registry[0].Caps != singleCaps {
		t.Errorf("single driver caps mismatch: got %+v, want %+v", registry[0].Caps, singleCaps)
	}

	// mergerfs, btrfs, zfs: Available = false with non-empty Reason
	for i := 1; i < 4; i++ {
		if registry[i].Available {
			t.Errorf("%s driver must be unavailable", registry[i].Name)
		}
		if registry[i].Reason == "" {
			t.Errorf("%s driver must have non-empty reason", registry[i].Name)
		}
	}
}

func TestLookupDriver(t *testing.T) {
	info, ok := LookupDriver("btrfs")
	if !ok {
		t.Fatal("LookupDriver(btrfs) must find driver")
	}
	if info.Name != "btrfs" {
		t.Errorf("name: got %q", info.Name)
	}
	if info.Available {
		t.Error("btrfs must be unavailable")
	}
	if info.Reason == "" {
		t.Error("btrfs must have reason")
	}
	if !info.Caps.Mirror || !info.Caps.Stripe || !info.Caps.Snapshot || !info.Caps.Checksum {
		t.Errorf("btrfs caps missing expected capabilities: %+v", info.Caps)
	}

	_, ok = LookupDriver("nope")
	if ok {
		t.Error("LookupDriver(nope) must not find driver")
	}
}
