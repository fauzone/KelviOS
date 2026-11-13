package storage

import (
	"os"
	"testing"
)

// Fixture-based: the test never touches the machine's real disks. The
// fixture is `lsblk -J -b` output containing two HDD/SSD disks, one NVMe,
// a loop device and a ROM drive (both must be filtered out).
func TestParseLSBLKFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/lsblk.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	disks, err := ParseLSBLK(data)
	if err != nil {
		t.Fatalf("ParseLSBLK: %v", err)
	}

	if len(disks) != 3 {
		t.Fatalf("expected 3 disks (loop/rom excluded), got %d: %+v", len(disks), disks)
	}

	// Sorted by name: nvme0n1, sda, sdb.
	byName := map[string]Disk{}
	for _, d := range disks {
		byName[d.Name] = d
	}

	sda := byName["sda"]
	if sda.Model != "WDC WD40EFRX-68N32N0" || sda.Serial != "WD-WCC7K1234567" {
		t.Errorf("sda identity wrong: %+v", sda)
	}
	if sda.SizeBytes != 4000787030016 {
		t.Errorf("sda size: got %d", sda.SizeBytes)
	}
	if !sda.Rotational {
		t.Errorf("sda should be rotational (HDD)")
	}
	if !sda.Mounted || sda.Mountpoint != "/mnt/media" {
		t.Errorf("sda should be mounted via its partition at /mnt/media: %+v", sda)
	}
	if sda.Path != "/dev/sda" || sda.PowerState != PowerUnknown || sda.NodeID != DefaultNodeID {
		t.Errorf("sda metadata wrong: %+v", sda)
	}

	sdb := byName["sdb"]
	if sdb.Rotational {
		t.Errorf("sdb should be non-rotational (SSD)")
	}
	if sdb.Mounted {
		t.Errorf("sdb should be unmounted: %+v", sdb)
	}

	nvme := byName["nvme0n1"]
	if !nvme.Mounted || nvme.Mountpoint != "/" {
		t.Errorf("nvme0n1 should be mounted at /: %+v", nvme)
	}
}

// Without `-b`, lsblk reports sizes as human strings; the parser must
// still produce exact-enough byte counts.
func TestParseLSBLKHumanSizes(t *testing.T) {
	data := []byte(`{"blockdevices":[
		{"name":"sda","model":null,"serial":null,"size":"7.5G","type":"disk","rota":true,"mountpoint":null},
		{"name":"sdb","model":null,"serial":null,"size":"500G","type":"disk","rota":true,"mountpoint":null}
	]}`)

	disks, err := ParseLSBLK(data)
	if err != nil {
		t.Fatalf("ParseLSBLK: %v", err)
	}
	if len(disks) != 2 {
		t.Fatalf("expected 2 disks, got %d", len(disks))
	}
	if disks[0].SizeBytes != int64(7.5*(1<<30)) {
		t.Errorf("7.5G parsed as %d bytes", disks[0].SizeBytes)
	}
	if disks[1].SizeBytes != 500*(1<<30) {
		t.Errorf("500G parsed as %d bytes", disks[1].SizeBytes)
	}
}

func TestParseLSBLKInvalid(t *testing.T) {
	if _, err := ParseLSBLK([]byte("{not json")); err == nil {
		t.Errorf("expected error for invalid JSON")
	}
}

func TestSingleDriverStub(t *testing.T) {
	disks := []Disk{
		{Name: "sda", Path: "/dev/sda", SizeBytes: 4000787030016, Rotational: true, PowerState: PowerUnknown, NodeID: DefaultNodeID},
	}
	drv := NewSingleDriver(disks)

	if drv.Name() != "single" {
		t.Errorf("driver name: got %q", drv.Name())
	}
	if caps := drv.Capabilities(); caps.Parity != ParityNone || caps.MixSizes {
		t.Errorf("single driver caps wrong: %+v", caps)
	}

	if _, err := drv.CreatePool(PoolConfig{Name: "x", Disks: []string{"sda"}}); err == nil {
		t.Errorf("CreatePool without tier must fail (tier is mandatory)")
	}
	if _, err := drv.CreatePool(PoolConfig{Name: "x", Tier: TierWarm, Disks: []string{"sdz"}}); err == nil {
		t.Errorf("CreatePool with unknown disk must fail")
	}

	pool, err := drv.CreatePool(PoolConfig{Name: "media", Tier: TierWarm, Disks: []string{"sda"}, Mountpoint: "/mnt/media"})
	if err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	if pool.SizeBytes != disks[0].SizeBytes || pool.Mechanism != MechanismSingle || pool.Tier != TierWarm {
		t.Errorf("pool metadata wrong: %+v", pool)
	}

	rep, err := drv.Health(pool.ID)
	if err != nil || rep.Status != "unknown" {
		t.Errorf("Health: rep=%+v err=%v", rep, err)
	}
	if err := drv.DestroyPool(pool.ID); err != nil {
		t.Errorf("DestroyPool: %v", err)
	}
	if _, err := drv.Health(pool.ID); err == nil {
		t.Errorf("Health of destroyed pool must fail")
	}
}
