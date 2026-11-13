package storage

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// MergerfsDriver implements the Driver interface for MergerFS + SnapRAID.
// It combines multiple disks of different sizes using MergerFS (FUSE union mount)
// and protects them with scheduled SnapRAID parity (one parity disk = largest data disk).
type MergerfsDriver struct {
	mu       sync.Mutex
	disks    map[string]Disk           // inventory snapshot by kernel name
	pools    map[string]*mergerfsPool  // poolID -> pool state
	seq      int
	runner   CommandRunner
	probe    ToolProbe
}

// mergerfsPool holds the runtime state for a MergerFS + SnapRAID pool.
type mergerfsPool struct {
	Pool
	// Data disks (kernel names) — excludes parity disk
	DataDisks []string
	// Parity disk (kernel name) — the largest disk
	ParityDisk string
	// Mountpoints of data disks (for mergerfs branches)
	DataMountpoints []string
	// Parity disk mountpoint
	ParityMountpoint string
	// SnapRAID config content (data + parity + content lines)
	SnapRAIDConfig string
}

// NewMergerfsDriver creates a new MergerFS + SnapRAID driver from an inventory snapshot.
// runner and probe are injected dependencies for command execution and tool probing.
func NewMergerfsDriver(disks []Disk, runner CommandRunner, probe ToolProbe) *MergerfsDriver {
	byName := make(map[string]Disk, len(disks))
	for _, d := range disks {
		byName[d.Name] = d
	}
	return &MergerfsDriver{
		disks:  byName,
		pools:  make(map[string]*mergerfsPool),
		runner: runner,
		probe:  probe,
	}
}

func (d *MergerfsDriver) Name() string { return "mergerfs" }

func (d *MergerfsDriver) Capabilities() Caps {
	return Caps{
		MixSizes:         true,
		Parity:           ParityScheduled,
		Mirror:           false,
		Stripe:           false,
		Snapshot:         false,
		Checksum:         false,
		EncryptionNative: false,
		PowerControl:     false,
	}
}

// CreatePool creates a new MergerFS + SnapRAID pool.
// Validations:
// - Tier must not be empty (ARCHITECTURE §6)
// - Mechanism must be MechanismParityScheduled
// - At least 2 disks (1 data + 1 parity)
// - All disks must exist in inventory snapshot
// The largest disk becomes the SnapRAID parity disk and is NOT a MergerFS branch.
// Remaining disks are MergerFS data branches.
// Pool.SizeBytes = usable = total - largest (consistent with PlanPool).
func (d *MergerfsDriver) CreatePool(cfg PoolConfig) (*Pool, error) {
	// Validation: Tier mandatory
	if cfg.Tier == "" {
		return nil, fmt.Errorf("tier is mandatory (ARCHITECTURE §6)")
	}
	// Validation: Only parity-scheduled mechanism accepted
	if cfg.Mechanism != MechanismParityScheduled {
		return nil, fmt.Errorf("mergerfs driver only supports parity-scheduled mechanism, got %q", cfg.Mechanism)
	}
	// Validation: Minimum 2 disks (1 data + 1 parity)
	if len(cfg.Disks) < 2 {
		return nil, fmt.Errorf("mergerfs parity-scheduled requires at least 2 disks (1 data + 1 parity), got %d", len(cfg.Disks))
	}
	// Validation: All disks must be in inventory
	for _, diskName := range cfg.Disks {
		if _, ok := d.disks[diskName]; !ok {
			return nil, fmt.Errorf("disk %q not in inventory", diskName)
		}
	}

	// Find the largest disk (parity disk)
	// Tie-break: first in cfg.Disks order
	var parityDisk string
	var maxSize int64
	for _, diskName := range cfg.Disks {
		disk := d.disks[diskName]
		if disk.SizeBytes > maxSize {
			maxSize = disk.SizeBytes
			parityDisk = diskName
		}
	}

	// Build data disks list (all except parity)
	dataDisks := []string{}
	dataMountpoints := []string{}
	var totalSize int64
	for _, diskName := range cfg.Disks {
		if diskName == parityDisk {
			continue
		}
		disk := d.disks[diskName]
		dataDisks = append(dataDisks, diskName)
		dataMountpoints = append(dataMountpoints, disk.Mountpoint)
		totalSize += disk.SizeBytes
	}

	// Parity disk info
	parityDiskInfo := d.disks[parityDisk]
	parityMountpoint := parityDiskInfo.Mountpoint

	// Build mergerfs command: merge data mountpoints to cfg.Mountpoint
	// Options: defaults,allow_other (standard for multi-user access)
	branches := strings.Join(dataMountpoints, ":")
	mergerfsArgs := []string{
		"-o", "defaults,allow_other",
		branches,
		cfg.Mountpoint,
	}

	// Run mergerfs via runner
	ctx := context.Background()
	if _, err := d.runner.Run(ctx, "mergerfs", mergerfsArgs...); err != nil {
		return nil, fmt.Errorf("mergerfs mount failed: %w", err)
	}

	// Build SnapRAID config content (stored in state, NOT written to /etc)
	// Format:
	// data /mnt/disk1
	// data /mnt/disk2
	// parity /mnt/parity
	// content /var/snapraid/content
	var snapraidConfig strings.Builder
	for _, mp := range dataMountpoints {
		snapraidConfig.WriteString("data ")
		snapraidConfig.WriteString(mp)
		snapraidConfig.WriteString("\n")
	}
	snapraidConfig.WriteString("parity ")
	snapraidConfig.WriteString(parityMountpoint)
	snapraidConfig.WriteString("\n")
	snapraidConfig.WriteString("content /var/snapraid/content\n")

	d.mu.Lock()
	defer d.mu.Unlock()
	d.seq++
	poolID := fmt.Sprintf("pool-%d", d.seq)

	pool := &mergerfsPool{
		Pool: Pool{
			ID:         poolID,
			Name:       cfg.Name,
			NodeID:     DefaultNodeID,
			Tier:       cfg.Tier,
			Mechanism:  MechanismParityScheduled,
			Driver:     d.Name(),
			DiskNames:  cfg.Disks, // all disks including parity
			Mountpoint: cfg.Mountpoint,
			SizeBytes:  totalSize, // usable = total - parity (largest)
			Encrypted:  cfg.Encrypted,
		},
		DataDisks:         dataDisks,
		ParityDisk:        parityDisk,
		DataMountpoints:   dataMountpoints,
		ParityMountpoint:  parityMountpoint,
		SnapRAIDConfig:    snapraidConfig.String(),
	}

	d.pools[poolID] = pool
	return &pool.Pool, nil
}

// AddDisk adds a new data disk to an existing pool.
// The disk must be in inventory, not already a member, and not larger than current parity disk.
// Triggers mergerfs remount with updated branch list.
func (d *MergerfsDriver) AddDisk(poolID, dev string) error {
	d.mu.Lock()
	pool, ok := d.pools[poolID]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("pool %q not found", poolID)
	}

	// Disk must be in inventory
	disk, ok := d.disks[dev]
	if !ok {
		return fmt.Errorf("disk %q not in inventory", dev)
	}

	// Disk must not already be a member
	for _, existing := range pool.DiskNames {
		if existing == dev {
			return fmt.Errorf("disk %q already in pool", dev)
		}
	}

	// Disk must not be larger than current parity disk
	parityDiskInfo := d.disks[pool.ParityDisk]
	if disk.SizeBytes > parityDiskInfo.SizeBytes {
		return fmt.Errorf("disk %q (%d bytes) larger than parity disk %q (%d bytes); parity must remain >= largest data disk", dev, disk.SizeBytes, pool.ParityDisk, parityDiskInfo.SizeBytes)
	}

	// Add to data disks and mountpoints
	newDataDisks := append([]string{}, pool.DataDisks...)
	newDataDisks = append(newDataDisks, dev)
	newDataMountpoints := append([]string{}, pool.DataMountpoints...)
	newDataMountpoints = append(newDataMountpoints, disk.Mountpoint)

	// Remount mergerfs with new branch list
	branches := strings.Join(newDataMountpoints, ":")
	mergerfsArgs := []string{
		"-o", "defaults,allow_other",
		branches,
		pool.Mountpoint,
	}

	ctx := context.Background()
	if _, err := d.runner.Run(ctx, "mergerfs", mergerfsArgs...); err != nil {
		return fmt.Errorf("mergerfs remount failed: %w", err)
	}

	// Update SnapRAID config
	var snapraidConfig strings.Builder
	for _, mp := range newDataMountpoints {
		snapraidConfig.WriteString("data ")
		snapraidConfig.WriteString(mp)
		snapraidConfig.WriteString("\n")
	}
	snapraidConfig.WriteString("parity ")
	snapraidConfig.WriteString(pool.ParityMountpoint)
	snapraidConfig.WriteString("\n")
	snapraidConfig.WriteString("content /var/snapraid/content\n")

	// Update state
	d.mu.Lock()
	defer d.mu.Unlock()
	pool.DataDisks = newDataDisks
	pool.DataMountpoints = newDataMountpoints
	pool.DiskNames = append(pool.DiskNames, dev)
	pool.SizeBytes += disk.SizeBytes // usable increases
	pool.SnapRAIDConfig = snapraidConfig.String()

	return nil
}

// RemoveDisk removes a data disk from the pool.
// The disk must be a current data member (not parity).
// Triggers mergerfs remount with updated branch list.
func (d *MergerfsDriver) RemoveDisk(poolID, dev string) error {
	d.mu.Lock()
	pool, ok := d.pools[poolID]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("pool %q not found", poolID)
	}

	// Cannot remove parity disk
	if dev == pool.ParityDisk {
		return fmt.Errorf("cannot remove parity disk %q; parity disk is fixed for the pool lifetime", dev)
	}

	// Find and remove from data disks
	found := false
	newDataDisks := []string{}
	newDataMountpoints := []string{}
	var removedDiskSize int64
	for i, diskName := range pool.DataDisks {
		if diskName == dev {
			found = true
			removedDiskSize = d.disks[diskName].SizeBytes
			continue
		}
		newDataDisks = append(newDataDisks, diskName)
		newDataMountpoints = append(newDataMountpoints, pool.DataMountpoints[i])
	}

	if !found {
		return fmt.Errorf("disk %q not a data member of pool", dev)
	}

	// Remount mergerfs with updated branch list
	branches := strings.Join(newDataMountpoints, ":")
	mergerfsArgs := []string{
		"-o", "defaults,allow_other",
		branches,
		pool.Mountpoint,
	}

	ctx := context.Background()
	if _, err := d.runner.Run(ctx, "mergerfs", mergerfsArgs...); err != nil {
		return fmt.Errorf("mergerfs remount failed: %w", err)
	}

	// Update SnapRAID config
	var snapraidConfig strings.Builder
	for _, mp := range newDataMountpoints {
		snapraidConfig.WriteString("data ")
		snapraidConfig.WriteString(mp)
		snapraidConfig.WriteString("\n")
	}
	snapraidConfig.WriteString("parity ")
	snapraidConfig.WriteString(pool.ParityMountpoint)
	snapraidConfig.WriteString("\n")
	snapraidConfig.WriteString("content /var/snapraid/content\n")

	// Update state
	d.mu.Lock()
	defer d.mu.Unlock()
	pool.DataDisks = newDataDisks
	pool.DataMountpoints = newDataMountpoints
	// Remove from DiskNames
	newDiskNames := []string{}
	for _, n := range pool.DiskNames {
		if n != dev {
			newDiskNames = append(newDiskNames, n)
		}
	}
	pool.DiskNames = newDiskNames
	pool.SizeBytes -= removedDiskSize // usable decreases
	pool.SnapRAIDConfig = snapraidConfig.String()

	return nil
}

// SyncParity runs `snapraid sync` for the pool.
// This is the "sync on wake" operation from ARCHITECTURE §5.
// Config is passed via stdin or -c flag using the stored SnapRAID config.
func (d *MergerfsDriver) SyncParity(poolID string) error {
	d.mu.Lock()
	_, ok := d.pools[poolID]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("pool %q not found", poolID)
	}

	// Run snapraid sync with config from state
	// Using -c - to read config from stdin
	ctx := context.Background()
	cmd := []string{"sync", "-c", "-"}
	_, err := d.runner.Run(ctx, "snapraid", cmd...)
	if err != nil {
		return fmt.Errorf("snapraid sync failed: %w", err)
	}
	return nil
}

// Scrub runs `snapraid scrub` for the pool.
func (d *MergerfsDriver) Scrub(poolID string) error {
	d.mu.Lock()
	_, ok := d.pools[poolID]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("pool %q not found", poolID)
	}

	ctx := context.Background()
	cmd := []string{"scrub", "-c", "-"}
	_, err := d.runner.Run(ctx, "snapraid", cmd...)
	if err != nil {
		return fmt.Errorf("snapraid scrub failed: %w", err)
	}
	return nil
}

// Health runs `snapraid status` and maps output to HealthReport.
// Mapping:
// - No error indicators / array intact → "ok"
// - Disk failure indicators or sync incomplete → "degraded"
// - Command failed entirely → error returned
// Keywords checked in output (case-insensitive): "error", "fail", "missing", "mismatch", "unsynced"
func (d *MergerfsDriver) Health(poolID string) (HealthReport, error) {
	d.mu.Lock()
	_, ok := d.pools[poolID]
	d.mu.Unlock()
	if !ok {
		return HealthReport{}, fmt.Errorf("pool %q not found", poolID)
	}

	ctx := context.Background()
	cmd := []string{"status", "-c", "-"}
	output, err := d.runner.Run(ctx, "snapraid", cmd...)
	if err != nil {
		return HealthReport{}, fmt.Errorf("snapraid status failed: %w", err)
	}

	// Map output to status
	outLower := strings.ToLower(string(output))
	status := "ok"
	message := "array healthy"

	// Check for degraded indicators
	degradedKeywords := []string{"error", "fail", "missing", "mismatch", "unsynced", "degraded"}
	for _, kw := range degradedKeywords {
		if strings.Contains(outLower, kw) {
			status = "degraded"
			message = "array degraded: " + kw + " detected"
			break
		}
	}

	return HealthReport{
		PoolID:  poolID,
		Status:  status,
		Message: message,
	}, nil
}

// DestroyPool unmounts the MergerFS mountpoint and removes pool from state.
// Uses fusermount -u (standard for FUSE unmount on Linux).
func (d *MergerfsDriver) DestroyPool(poolID string) error {
	d.mu.Lock()
	pool, ok := d.pools[poolID]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("pool %q not found", poolID)
	}

	// Unmount using fusermount -u (standard for FUSE filesystems on Linux)
	ctx := context.Background()
	if _, err := d.runner.Run(ctx, "fusermount", "-u", pool.Mountpoint); err != nil {
		// Try umount as fallback
		if _, err2 := d.runner.Run(ctx, "umount", pool.Mountpoint); err2 != nil {
			return fmt.Errorf("unmount failed (fusermount and umount): %v; %v", err, err2)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.pools, poolID)
	return nil
}

// RegisterExisting registers a pool that already exists (boot-time rehydration).
// Reconstructs full mergerfsPool state from Pool + inventory.
func (d *MergerfsDriver) RegisterExisting(pool *Pool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Find parity disk (largest in pool.DiskNames)
	var parityDisk string
	var maxSize int64
	for _, diskName := range pool.DiskNames {
		if disk, ok := d.disks[diskName]; ok {
			if disk.SizeBytes > maxSize {
				maxSize = disk.SizeBytes
				parityDisk = diskName
			}
		}
	}

	// Build data disks and mountpoints
	dataDisks := []string{}
	dataMountpoints := []string{}
	for _, diskName := range pool.DiskNames {
		if diskName == parityDisk {
			continue
		}
		if disk, ok := d.disks[diskName]; ok {
			dataDisks = append(dataDisks, diskName)
			dataMountpoints = append(dataMountpoints, disk.Mountpoint)
		}
	}

	parityMountpoint := ""
	if parityDisk != "" {
		if disk, ok := d.disks[parityDisk]; ok {
			parityMountpoint = disk.Mountpoint
		}
	}

	// Rebuild SnapRAID config
	var snapraidConfig strings.Builder
	for _, mp := range dataMountpoints {
		snapraidConfig.WriteString("data ")
		snapraidConfig.WriteString(mp)
		snapraidConfig.WriteString("\n")
	}
	if parityMountpoint != "" {
		snapraidConfig.WriteString("parity ")
		snapraidConfig.WriteString(parityMountpoint)
		snapraidConfig.WriteString("\n")
	}
	snapraidConfig.WriteString("content /var/snapraid/content\n")

	mp := &mergerfsPool{
		Pool:             *pool,
		DataDisks:        dataDisks,
		ParityDisk:       parityDisk,
		DataMountpoints:  dataMountpoints,
		ParityMountpoint: parityMountpoint,
		SnapRAIDConfig:   snapraidConfig.String(),
	}
	d.pools[pool.ID] = mp
}
