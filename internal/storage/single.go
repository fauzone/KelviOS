package storage

import (
	"fmt"
	"sync"
)

// SingleDriver is the single-disk driver stub (ARCHITECTURE §5: ext4/XFS/
// Btrfs on one disk, no protection). In WS3 it is a metadata-only
// contract implementation: CreatePool validates the request against the
// disk inventory and returns the pool metadata it *would* create; no
// filesystem is formatted and nothing is mounted. Real behavior lands in
// a later workstream.
type SingleDriver struct {
	mu    sync.Mutex
	disks map[string]Disk
	pools map[string]*Pool
	seq   int
}

// NewSingleDriver builds the stub from an inventory snapshot.
func NewSingleDriver(disks []Disk) *SingleDriver {
	byName := make(map[string]Disk, len(disks))
	for _, d := range disks {
		byName[d.Name] = d
	}
	return &SingleDriver{disks: byName, pools: map[string]*Pool{}}
}

func (d *SingleDriver) Name() string { return "single" }

func (d *SingleDriver) Capabilities() Caps {
	return Caps{
		MixSizes:         false,
		Parity:           ParityNone,
		Mirror:           false,
		Stripe:           false,
		Snapshot:         false,
		Checksum:         false,
		EncryptionNative: false,
		PowerControl:     false, // stub has no power engine
	}
}

func (d *SingleDriver) CreatePool(cfg PoolConfig) (*Pool, error) {
	if cfg.Tier == "" {
		return nil, fmt.Errorf("tier is mandatory (ARCHITECTURE §6)")
	}
	if len(cfg.Disks) != 1 {
		return nil, fmt.Errorf("single-disk driver needs exactly 1 disk, got %d", len(cfg.Disks))
	}
	disk, ok := d.disks[cfg.Disks[0]]
	if !ok {
		return nil, fmt.Errorf("disk %q not in inventory", cfg.Disks[0])
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	pool := &Pool{
		ID:         fmt.Sprintf("pool-%d", d.seq),
		Name:       cfg.Name,
		NodeID:     DefaultNodeID,
		Tier:       cfg.Tier,
		Mechanism:  MechanismSingle,
		Driver:     d.Name(),
		DiskNames:  []string{disk.Name},
		Mountpoint: cfg.Mountpoint,
		SizeBytes:  disk.SizeBytes,
		Encrypted:  cfg.Encrypted,
	}
	d.pools[pool.ID] = pool
	return pool, nil
}

func (d *SingleDriver) AddDisk(poolID, dev string) error {
	return fmt.Errorf("single-disk pools cannot grow; create a new pool instead")
}

func (d *SingleDriver) RemoveDisk(poolID, dev string) error {
	return fmt.Errorf("single-disk pools have no removable disk")
}

func (d *SingleDriver) Health(poolID string) (HealthReport, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.pools[poolID]; !ok {
		return HealthReport{}, fmt.Errorf("pool %q not found", poolID)
	}
	return HealthReport{
		PoolID:  poolID,
		Status:  "unknown",
		Message: "stub driver: health probing lands with the real single-disk driver",
	}, nil
}

func (d *SingleDriver) Scrub(poolID string) error {
	return fmt.Errorf("scrub is not supported by the single-disk stub driver")
}

func (d *SingleDriver) DestroyPool(poolID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.pools[poolID]; !ok {
		return fmt.Errorf("pool %q not found", poolID)
	}
	delete(d.pools, poolID)
	return nil
}

// RegisterExisting registers a pool that already exists in the catalog.
// Used during boot-time rehydration after a server restart.
func (d *SingleDriver) RegisterExisting(pool *Pool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pools[pool.ID] = pool
}
