package storage

import (
	"errors"
	"strings"
	"testing"
)

// Test inventory for mergerfs tests
func testInventory() []Disk {
	return []Disk{
		{Name: "sda", Path: "/dev/sda", SizeBytes: 2 * TB, Rotational: true, Mountpoint: "/mnt/disk1", PowerState: PowerUnknown, NodeID: DefaultNodeID},
		{Name: "sdb", Path: "/dev/sdb", SizeBytes: 4 * TB, Rotational: true, Mountpoint: "/mnt/disk2", PowerState: PowerUnknown, NodeID: DefaultNodeID},
		{Name: "sdc", Path: "/dev/sdc", SizeBytes: 8 * TB, Rotational: true, Mountpoint: "/mnt/disk3", PowerState: PowerUnknown, NodeID: DefaultNodeID},
		{Name: "sdd", Path: "/dev/sdd", SizeBytes: 8 * TB, Rotational: true, Mountpoint: "/mnt/disk4", PowerState: PowerUnknown, NodeID: DefaultNodeID},
	}
}

func TestMergerfsCreatePool(t *testing.T) {
	inv := testInventory()
	runner := NewFakeRunner()
	probe := NewFakeProbe()
	probe.SetPath("mergerfs", "/usr/bin/mergerfs")
	probe.SetPath("snapraid", "/usr/bin/snapraid")

	// Configure fake runner to succeed on mergerfs mount
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})

	drv := NewMergerfsDriver(inv, runner, probe)

	pool, err := drv.CreatePool(PoolConfig{
		Name:       "media",
		Tier:       TierWarm,
		Mechanism:  MechanismParityScheduled,
		Disks:      []string{"sda", "sdb", "sdc", "sdd"},
		Mountpoint: "/mnt/media",
		Encrypted:  false,
	})
	if err != nil {
		t.Fatalf("CreatePool failed: %v", err)
	}

	// Verify pool metadata
	if pool.Driver != "mergerfs" {
		t.Errorf("Driver: got %q, want %q", pool.Driver, "mergerfs")
	}
	if pool.Mechanism != MechanismParityScheduled {
		t.Errorf("Mechanism: got %q, want %q", pool.Mechanism, MechanismParityScheduled)
	}
	if pool.Tier != TierWarm {
		t.Errorf("Tier: got %q, want %q", pool.Tier, TierWarm)
	}
	if pool.Mountpoint != "/mnt/media" {
		t.Errorf("Mountpoint: got %q, want %q", pool.Mountpoint, "/mnt/media")
	}

	// Usable = total - largest = (2+4+8+8) - 8 = 14 TB
	expectedUsable := int64(14) * TB
	if pool.SizeBytes != expectedUsable {
		t.Errorf("SizeBytes (usable): got %d, want %d", pool.SizeBytes, expectedUsable)
	}

	// DiskNames should include all 4 disks
	if len(pool.DiskNames) != 4 {
		t.Errorf("DiskNames count: got %d, want 4", len(pool.DiskNames))
	}

	// Verify mergerfs was called with correct args (data disks only, no parity)
	// Parity is sdc (8 TB, first of the two 8TB disks in cfg.Disks order)
	// Data branches: sda, sdb, sdd
	expectedBranches := "/mnt/disk1:/mnt/disk2:/mnt/disk4"
	found := false
	for _, call := range runner.Calls {
		if call.Name == "mergerfs" {
			// Check that branches are in args
			argsStr := strings.Join(call.Args, " ")
			if strings.Contains(argsStr, expectedBranches) && strings.Contains(argsStr, "/mnt/media") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("mergerfs not called with expected branches %q and mountpoint", expectedBranches)
	}
}

func TestMergerfsCreatePoolValidation(t *testing.T) {
	inv := testInventory()
	runner := NewFakeRunner()
	probe := NewFakeProbe()
	drv := NewMergerfsDriver(inv, runner, probe)

	tests := []struct {
		name        string
		cfg         PoolConfig
		wantErr     bool
		errContains string
	}{
		{
			name: "empty tier",
			cfg: PoolConfig{
				Name:       "media",
				Tier:       "",
				Mechanism:  MechanismParityScheduled,
				Disks:      []string{"sda", "sdb"},
				Mountpoint: "/mnt/media",
			},
			wantErr:     true,
			errContains: "tier is mandatory",
		},
		{
			name: "wrong mechanism",
			cfg: PoolConfig{
				Name:       "media",
				Tier:       TierWarm,
				Mechanism:  MechanismMirror,
				Disks:      []string{"sda", "sdb"},
				Mountpoint: "/mnt/media",
			},
			wantErr:     true,
			errContains: "only supports parity-scheduled",
		},
		{
			name: "single disk",
			cfg: PoolConfig{
				Name:       "media",
				Tier:       TierWarm,
				Mechanism:  MechanismParityScheduled,
				Disks:      []string{"sda"},
				Mountpoint: "/mnt/media",
			},
			wantErr:     true,
			errContains: "at least 2 disks",
		},
		{
			name: "unknown disk",
			cfg: PoolConfig{
				Name:       "media",
				Tier:       TierWarm,
				Mechanism:  MechanismParityScheduled,
				Disks:      []string{"sda", "sdz"},
				Mountpoint: "/mnt/media",
			},
			wantErr:     true,
			errContains: "not in inventory",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Reset runner calls for each subtest
			runner = NewFakeRunner()
			drv = NewMergerfsDriver(inv, runner, probe)

			_, err := drv.CreatePool(tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error but got nil")
				} else if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error mismatch: got %q, want contains %q", err.Error(), tc.errContains)
				}
				// Verify no runner calls were made on validation failure
				if len(runner.Calls) > 0 {
					t.Errorf("runner should not be called on validation failure, got %d calls", len(runner.Calls))
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestMergerfsAddDisk(t *testing.T) {
	inv := testInventory()
	runner := NewFakeRunner()
	probe := NewFakeProbe()
	probe.SetPath("mergerfs", "/usr/bin/mergerfs")
	probe.SetPath("snapraid", "/usr/bin/snapraid")
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})

	drv := NewMergerfsDriver(inv, runner, probe)

	// Create initial pool with sda, sdb, sdc (sdc = 8TB parity)
	pool, err := drv.CreatePool(PoolConfig{
		Name:       "media",
		Tier:       TierWarm,
		Mechanism:  MechanismParityScheduled,
		Disks:      []string{"sda", "sdb", "sdc"},
		Mountpoint: "/mnt/media",
	})
	if err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	// Reset runner calls
	runner = NewFakeRunner()
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})
	drv = NewMergerfsDriver(inv, runner, probe)
	// Re-create pool (in real usage, pool would be registered via RegisterExisting)
	// For test, we need to manually register since we recreated driver
	drv.RegisterExisting(pool)

	// Add sdd (8TB) as new data disk
	err = drv.AddDisk(pool.ID, "sdd")
	if err != nil {
		t.Fatalf("AddDisk failed: %v", err)
	}

	// Verify mergerfs remount was called with new branch list
	found := false
	for _, call := range runner.Calls {
		if call.Name == "mergerfs" {
			argsStr := strings.Join(call.Args, " ")
			if strings.Contains(argsStr, "/mnt/disk4") && strings.Contains(argsStr, "/mnt/media") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("mergerfs remount not called with new branch /mnt/disk4")
	}

	// Test: add disk already in pool
	err = drv.AddDisk(pool.ID, "sdd")
	if err == nil {
		t.Error("AddDisk with existing disk should fail")
	} else if !strings.Contains(err.Error(), "already in pool") {
		t.Errorf("wrong error for existing disk: %v", err)
	}

	// Test: add unknown disk
	err = drv.AddDisk(pool.ID, "sdz")
	if err == nil {
		t.Error("AddDisk with unknown disk should fail")
	} else if !strings.Contains(err.Error(), "not in inventory") {
		t.Errorf("wrong error for unknown disk: %v", err)
	}

	// Test: add disk larger than parity
	// Create a new inventory with a larger disk
	inv2 := append(inv, Disk{Name: "sde", Path: "/dev/sde", SizeBytes: 16 * TB, Rotational: true, Mountpoint: "/mnt/disk5", PowerState: PowerUnknown, NodeID: DefaultNodeID})
	runner2 := NewFakeRunner()
	runner2.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})
	drv2 := NewMergerfsDriver(inv2, runner2, probe)
	pool2, _ := drv2.CreatePool(PoolConfig{
		Name:       "media2",
		Tier:       TierWarm,
		Mechanism:  MechanismParityScheduled,
		Disks:      []string{"sda", "sdb", "sdc"},
		Mountpoint: "/mnt/media2",
	})
	drv2.RegisterExisting(pool2)

	err = drv2.AddDisk(pool2.ID, "sde") // 16TB > 8TB parity
	if err == nil {
		t.Error("AddDisk with disk larger than parity should fail")
	} else if !strings.Contains(err.Error(), "larger than parity") {
		t.Errorf("wrong error for larger disk: %v", err)
	}
}

func TestMergerfsRemoveDisk(t *testing.T) {
	inv := testInventory()
	runner := NewFakeRunner()
	probe := NewFakeProbe()
	probe.SetPath("mergerfs", "/usr/bin/mergerfs")
	probe.SetPath("snapraid", "/usr/bin/snapraid")
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})

	drv := NewMergerfsDriver(inv, runner, probe)

	// Create pool with sda, sdb, sdc, sdd (sdc = 8TB parity)
	pool, err := drv.CreatePool(PoolConfig{
		Name:       "media",
		Tier:       TierWarm,
		Mechanism:  MechanismParityScheduled,
		Disks:      []string{"sda", "sdb", "sdc", "sdd"},
		Mountpoint: "/mnt/media",
	})
	if err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	// Reset runner
	runner = NewFakeRunner()
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})
	drv = NewMergerfsDriver(inv, runner, probe)
	drv.RegisterExisting(pool)

	// Remove sdd (data disk)
	err = drv.RemoveDisk(pool.ID, "sdd")
	if err != nil {
		t.Fatalf("RemoveDisk failed: %v", err)
	}

	// Verify mergerfs remount was called without sdd
	found := false
	for _, call := range runner.Calls {
		if call.Name == "mergerfs" {
			argsStr := strings.Join(call.Args, " ")
			if !strings.Contains(argsStr, "/mnt/disk4") && strings.Contains(argsStr, "/mnt/media") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("mergerfs remount not called without removed branch")
	}

	// Test: remove non-member disk
	err = drv.RemoveDisk(pool.ID, "sdz")
	if err == nil {
		t.Error("RemoveDisk with non-member should fail")
	} else if !strings.Contains(err.Error(), "not a data member") {
		t.Errorf("wrong error for non-member: %v", err)
	}

	// Test: remove parity disk
	err = drv.RemoveDisk(pool.ID, "sdc")
	if err == nil {
		t.Error("RemoveDisk with parity disk should fail")
	} else if !strings.Contains(err.Error(), "cannot remove parity") {
		t.Errorf("wrong error for parity disk: %v", err)
	}
}

func TestMergerfsSyncParity(t *testing.T) {
	inv := testInventory()
	runner := NewFakeRunner()
	probe := NewFakeProbe()
	probe.SetPath("mergerfs", "/usr/bin/mergerfs")
	probe.SetPath("snapraid", "/usr/bin/snapraid")
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})

	drv := NewMergerfsDriver(inv, runner, probe)
	pool, _ := drv.CreatePool(PoolConfig{
		Name:       "media",
		Tier:       TierWarm,
		Mechanism:  MechanismParityScheduled,
		Disks:      []string{"sda", "sdb", "sdc"},
		Mountpoint: "/mnt/media",
	})
	drv.RegisterExisting(pool)

	// Reset runner
	runner = NewFakeRunner()
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})
	drv = NewMergerfsDriver(inv, runner, probe)
	drv.RegisterExisting(pool)

	err := drv.SyncParity(pool.ID)
	if err != nil {
		t.Fatalf("SyncParity failed: %v", err)
	}

	// Verify snapraid sync was called
	found := false
	for _, call := range runner.Calls {
		if call.Name == "snapraid" {
			argsStr := strings.Join(call.Args, " ")
			if strings.Contains(argsStr, "sync") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("snapraid sync not called")
	}

	// Test: unknown pool
	err = drv.SyncParity("pool-999")
	if err == nil {
		t.Error("SyncParity with unknown pool should fail")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("wrong error for unknown pool: %v", err)
	}
}

func TestMergerfsScrub(t *testing.T) {
	inv := testInventory()
	runner := NewFakeRunner()
	probe := NewFakeProbe()
	probe.SetPath("mergerfs", "/usr/bin/mergerfs")
	probe.SetPath("snapraid", "/usr/bin/snapraid")
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})

	drv := NewMergerfsDriver(inv, runner, probe)
	pool, _ := drv.CreatePool(PoolConfig{
		Name:       "media",
		Tier:       TierWarm,
		Mechanism:  MechanismParityScheduled,
		Disks:      []string{"sda", "sdb", "sdc"},
		Mountpoint: "/mnt/media",
	})
	drv.RegisterExisting(pool)

	runner = NewFakeRunner()
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})
	drv = NewMergerfsDriver(inv, runner, probe)
	drv.RegisterExisting(pool)

	err := drv.Scrub(pool.ID)
	if err != nil {
		t.Fatalf("Scrub failed: %v", err)
	}

	// Verify snapraid scrub was called
	found := false
	for _, call := range runner.Calls {
		if call.Name == "snapraid" {
			argsStr := strings.Join(call.Args, " ")
			if strings.Contains(argsStr, "scrub") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("snapraid scrub not called")
	}

	// Test: unknown pool
	err = drv.Scrub("pool-999")
	if err == nil {
		t.Error("Scrub with unknown pool should fail")
	}
}

func TestMergerfsHealth(t *testing.T) {
	inv := testInventory()
	probe := NewFakeProbe()
	probe.SetPath("mergerfs", "/usr/bin/mergerfs")
	probe.SetPath("snapraid", "/usr/bin/snapraid")

	// Test 1: Healthy output
	runner := NewFakeRunner()
	healthyOutput := `SnapRAID status
  RAID level: 1
  Data disks: 2
  Parity disks: 1
  All OK.`
	runner.SetDefaultResponse(FakeResponse{Output: []byte(healthyOutput), Err: nil})

	drv := NewMergerfsDriver(inv, runner, probe)
	pool, _ := drv.CreatePool(PoolConfig{
		Name:       "media",
		Tier:       TierWarm,
		Mechanism:  MechanismParityScheduled,
		Disks:      []string{"sda", "sdb", "sdc"},
		Mountpoint: "/mnt/media",
	})
	drv.RegisterExisting(pool)

	rep, err := drv.Health(pool.ID)
	if err != nil {
		t.Fatalf("Health failed: %v", err)
	}
	if rep.Status != "ok" {
		t.Errorf("Health status: got %q, want %q", rep.Status, "ok")
	}

	// Test 2: Degraded output (missing disk)
	runner = NewFakeRunner()
	degradedOutput := `SnapRAID status
  RAID level: 1
  Data disks: 2
  Parity disks: 1
  ERROR: Disk missing!`
	runner.SetDefaultResponse(FakeResponse{Output: []byte(degradedOutput), Err: nil})

	drv = NewMergerfsDriver(inv, runner, probe)
	drv.RegisterExisting(pool)

	rep, err = drv.Health(pool.ID)
	if err != nil {
		t.Fatalf("Health failed: %v", err)
	}
	if rep.Status != "degraded" {
		t.Errorf("Health status: got %q, want %q", rep.Status, "degraded")
	}

	// Test 3: Command failure
	runner = NewFakeRunner()
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: errors.New("command not found")})

	drv = NewMergerfsDriver(inv, runner, probe)
	drv.RegisterExisting(pool)

	_, err = drv.Health(pool.ID)
	if err == nil {
		t.Error("Health with command failure should return error")
	}

	// Test 4: Unknown pool
	runner = NewFakeRunner()
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})
	drv = NewMergerfsDriver(inv, runner, probe)
	drv.RegisterExisting(pool)

	_, err = drv.Health("pool-999")
	if err == nil {
		t.Error("Health with unknown pool should fail")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("wrong error for unknown pool: %v", err)
	}
}

func TestMergerfsDestroyPool(t *testing.T) {
	inv := testInventory()
	runner := NewFakeRunner()
	probe := NewFakeProbe()
	probe.SetPath("mergerfs", "/usr/bin/mergerfs")
	probe.SetPath("snapraid", "/usr/bin/snapraid")
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})

	drv := NewMergerfsDriver(inv, runner, probe)
	pool, _ := drv.CreatePool(PoolConfig{
		Name:       "media",
		Tier:       TierWarm,
		Mechanism:  MechanismParityScheduled,
		Disks:      []string{"sda", "sdb", "sdc"},
		Mountpoint: "/mnt/media",
	})
	drv.RegisterExisting(pool)

	runner = NewFakeRunner()
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})
	drv = NewMergerfsDriver(inv, runner, probe)
	drv.RegisterExisting(pool)

	err := drv.DestroyPool(pool.ID)
	if err != nil {
		t.Fatalf("DestroyPool failed: %v", err)
	}

	// Verify fusermount -u was called
	found := false
	for _, call := range runner.Calls {
		if call.Name == "fusermount" {
			argsStr := strings.Join(call.Args, " ")
			if strings.Contains(argsStr, "-u") && strings.Contains(argsStr, "/mnt/media") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("fusermount -u not called with mountpoint")
	}

	// Pool should be gone from state
	_, err = drv.Health(pool.ID)
	if err == nil {
		t.Error("Health of destroyed pool should fail")
	} else if !strings.Contains(err.Error(), "not found") {
		t.Errorf("wrong error for destroyed pool: %v", err)
	}

	// Test: unknown pool
	err = drv.DestroyPool("pool-999")
	if err == nil {
		t.Error("DestroyPool with unknown pool should fail")
	}
}

func TestMergerfsToolMissing(t *testing.T) {
	// Probe missing mergerfs
	probe := NewFakeProbe()
	probe.SetPath("mergerfs", "") // not found
	probe.SetPath("snapraid", "/usr/bin/snapraid")

	registry := DriverRegistryWithProbe(probe)
	var mergerfsInfo DriverInfo
	for _, d := range registry {
		if d.Name == "mergerfs" {
			mergerfsInfo = d
			break
		}
	}
	if mergerfsInfo.Available {
		t.Error("mergerfs should be unavailable when mergerfs binary missing")
	}
	if mergerfsInfo.Reason != "MergerFS + SnapRAID tooling on device (hardware phase)" {
		t.Errorf("Reason: got %q, want %q", mergerfsInfo.Reason, "MergerFS + SnapRAID tooling on device (hardware phase)")
	}

	// Probe missing snapraid
	probe = NewFakeProbe()
	probe.SetPath("mergerfs", "/usr/bin/mergerfs")
	probe.SetPath("snapraid", "") // not found

	registry = DriverRegistryWithProbe(probe)
	for _, d := range registry {
		if d.Name == "mergerfs" {
			mergerfsInfo = d
			break
		}
	}
	if mergerfsInfo.Available {
		t.Error("mergerfs should be unavailable when snapraid binary missing")
	}
	if mergerfsInfo.Reason != "MergerFS + SnapRAID tooling on device (hardware phase)" {
		t.Errorf("Reason: got %q, want %q", mergerfsInfo.Reason, "MergerFS + SnapRAID tooling on device (hardware phase)")
	}

	// Probe missing both
	probe = NewFakeProbe()
	probe.SetPath("mergerfs", "")
	probe.SetPath("snapraid", "")

	registry = DriverRegistryWithProbe(probe)
	for _, d := range registry {
		if d.Name == "mergerfs" {
			mergerfsInfo = d
			break
		}
	}
	if mergerfsInfo.Available {
		t.Error("mergerfs should be unavailable when both binaries missing")
	}

	// Other drivers unchanged
	for _, d := range registry {
		if d.Name == "single" {
			if !d.Available {
				t.Error("single driver should remain available")
			}
		}
		if d.Name == "btrfs" || d.Name == "zfs" {
			if d.Available {
				t.Errorf("%s driver should remain unavailable", d.Name)
			}
			if d.Reason == "" {
				t.Errorf("%s driver should have non-empty reason", d.Name)
			}
		}
	}
}

func TestMergerfsToolsAvailable(t *testing.T) {
	probe := NewFakeProbe()
	probe.SetPath("mergerfs", "/usr/bin/mergerfs")
	probe.SetPath("snapraid", "/usr/bin/snapraid")

	registry := DriverRegistryWithProbe(probe)
	var mergerfsInfo DriverInfo
	for _, d := range registry {
		if d.Name == "mergerfs" {
			mergerfsInfo = d
			break
		}
	}
	if !mergerfsInfo.Available {
		t.Error("mergerfs should be available when both binaries found")
	}
	if mergerfsInfo.Reason != "" {
		t.Errorf("Reason should be empty when available, got %q", mergerfsInfo.Reason)
	}

	// Caps should be unchanged
	expectedCaps := Caps{
		MixSizes:         true,
		Parity:           ParityScheduled,
		Mirror:           false,
		Stripe:           false,
		Snapshot:         false,
		Checksum:         false,
		EncryptionNative: false,
		PowerControl:     false,
	}
	if mergerfsInfo.Caps != expectedCaps {
		t.Errorf("Caps mismatch: got %+v, want %+v", mergerfsInfo.Caps, expectedCaps)
	}
}

func TestMergerfsCommandFailure(t *testing.T) {
	inv := testInventory()
	probe := NewFakeProbe()
	probe.SetPath("mergerfs", "/usr/bin/mergerfs")
	probe.SetPath("snapraid", "/usr/bin/snapraid")

	// Test: mergerfs mount fails
	runner := NewFakeRunner()
	runner.SetDefaultResponse(FakeResponse{Output: []byte("mount failed"), Err: errors.New("permission denied")})

	drv := NewMergerfsDriver(inv, runner, probe)
	_, err := drv.CreatePool(PoolConfig{
		Name:       "media",
		Tier:       TierWarm,
		Mechanism:  MechanismParityScheduled,
		Disks:      []string{"sda", "sdb", "sdc"},
		Mountpoint: "/mnt/media",
	})
	if err == nil {
		t.Error("CreatePool should fail when mergerfs mount fails")
	}

	// Pool should not be registered on failure
	// (We can't easily test internal state, but we can verify no pool was returned)

	// Test: snapraid sync fails
	runner = NewFakeRunner()
	runner.SetResponse("mergerfs|-o|defaults,allow_other|/mnt/disk1:/mnt/disk2|/mnt/media2", FakeResponse{Output: []byte(""), Err: nil})
	runner.SetDefaultResponse(FakeResponse{Output: []byte("sync failed"), Err: errors.New("snapraid error")})

	drv = NewMergerfsDriver(inv, runner, probe)
	pool, err := drv.CreatePool(PoolConfig{
		Name:       "media2",
		Tier:       TierWarm,
		Mechanism:  MechanismParityScheduled,
		Disks:      []string{"sda", "sdb", "sdc"},
		Mountpoint: "/mnt/media2",
	})
	if err != nil {
		t.Fatalf("CreatePool should succeed: %v", err)
	}
	drv.RegisterExisting(pool)

	err = drv.SyncParity(pool.ID)
	if err == nil {
		t.Error("SyncParity should fail when snapraid sync fails")
	}
}

// TestMergerfsTieBreakParity tests that when two disks have the same max size,
// the first one in cfg.Disks becomes parity.
func TestMergerfsTieBreakParity(t *testing.T) {
	inv := []Disk{
		{Name: "sda", Path: "/dev/sda", SizeBytes: 8 * TB, Rotational: true, Mountpoint: "/mnt/disk1", PowerState: PowerUnknown, NodeID: DefaultNodeID},
		{Name: "sdb", Path: "/dev/sdb", SizeBytes: 8 * TB, Rotational: true, Mountpoint: "/mnt/disk2", PowerState: PowerUnknown, NodeID: DefaultNodeID},
		{Name: "sdc", Path: "/dev/sdc", SizeBytes: 4 * TB, Rotational: true, Mountpoint: "/mnt/disk3", PowerState: PowerUnknown, NodeID: DefaultNodeID},
	}
	runner := NewFakeRunner()
	probe := NewFakeProbe()
	probe.SetPath("mergerfs", "/usr/bin/mergerfs")
	probe.SetPath("snapraid", "/usr/bin/snapraid")
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})

	drv := NewMergerfsDriver(inv, runner, probe)

	// sda and sdb both 8TB, sda comes first in cfg.Disks -> sda is parity
	pool, err := drv.CreatePool(PoolConfig{
		Name:       "media",
		Tier:       TierWarm,
		Mechanism:  MechanismParityScheduled,
		Disks:      []string{"sda", "sdb", "sdc"},
		Mountpoint: "/mnt/media",
	})
	if err != nil {
		t.Fatalf("CreatePool: %v", err)
	}

	// Usable = total - max = (8+8+4) - 8 = 12 TB
	expectedUsable := int64(12) * TB
	if pool.SizeBytes != expectedUsable {
		t.Errorf("Usable: got %d, want %d", pool.SizeBytes, expectedUsable)
	}

	// Verify mergerfs branches: sdb and sdc (sda is parity)
	found := false
	for _, call := range runner.Calls {
		if call.Name == "mergerfs" {
			argsStr := strings.Join(call.Args, " ")
			// Should contain sdb and sdc mountpoints, NOT sda
			if strings.Contains(argsStr, "/mnt/disk2") && strings.Contains(argsStr, "/mnt/disk3") && !strings.Contains(argsStr, "/mnt/disk1") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("mergerfs branches should exclude first disk (sda) as parity")
	}
}

// TestMergerfsRegisterExisting tests boot-time rehydration.
func TestMergerfsRegisterExisting(t *testing.T) {
	inv := testInventory()
	runner := NewFakeRunner()
	probe := NewFakeProbe()
	probe.SetPath("mergerfs", "/usr/bin/mergerfs")
	probe.SetPath("snapraid", "/usr/bin/snapraid")
	runner.SetDefaultResponse(FakeResponse{Output: []byte(""), Err: nil})

	drv := NewMergerfsDriver(inv, runner, probe)
	pool, _ := drv.CreatePool(PoolConfig{
		Name:       "media",
		Tier:       TierWarm,
		Mechanism:  MechanismParityScheduled,
		Disks:      []string{"sda", "sdb", "sdc"},
		Mountpoint: "/mnt/media",
	})

	// Create new driver instance (simulating restart)
	drv2 := NewMergerfsDriver(inv, runner, probe)
	drv2.RegisterExisting(pool)

	// Pool should be accessible
	rep, err := drv2.Health(pool.ID)
	if err != nil {
		t.Errorf("Health after RegisterExisting failed: %v", err)
	}
	if rep.Status == "" {
		t.Error("Health report should have status")
	}
}
