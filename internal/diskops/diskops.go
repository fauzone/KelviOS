// Package diskops provides pure planning logic for disk replacement and
// expansion operations. It contains no I/O, no driver execution, and no
// wiring to catalog/HTTP layers. Execution and wiring belong to later steps.
package diskops

import (
	"errors"
	"fmt"
)

// Strategy names the high-level replacement strategy for a pool mechanism.
type Strategy string

const (
	StrategyCopyOut       Strategy = "copy-out"       // MergerFS: copy data out, replace, copy back
	StrategyRebuildParity Strategy = "rebuild-parity" // SnapRAID: rebuild from parity
	StrategyNativeReplace Strategy = "native-replace" // Btrfs/ZFS: native device replace
	StrategyUnsupported   Strategy = "unsupported"    // No safe replacement path (e.g., stripe/RAID0)
)

// DiskRef is a minimal disk reference used in plans.
type DiskRef struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

// CapacityPreview describes the usable capacity change from a plan.
type CapacityPreview struct {
	OldUsableBytes int64 `json:"old_usable_bytes"`
	NewUsableBytes int64 `json:"new_usable_bytes"`
	DeltaBytes     int64 `json:"delta_bytes"`
}

// ReplacePlan is a pure-data replacement or expansion plan.
type ReplacePlan struct {
	PoolID          string          `json:"pool_id"`
	PoolMechanism   string          `json:"pool_mechanism"`
	FailingDisk     DiskRef         `json:"failing_disk,omitempty"`     // empty for expansion
	ReplacementDisk DiskRef         `json:"replacement_disk,omitempty"` // empty for expansion
	NewDisk         DiskRef         `json:"new_disk,omitempty"`         // for expansion
	Strategy        Strategy        `json:"strategy"`
	Steps           []string        `json:"steps"`
	Preview         CapacityPreview `json:"preview"`
}

// FileRef is a minimal file reference for inventory verification.
type FileRef struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

// ErrReplacementDiskInPool is returned when the replacement disk is already in the pool.
var ErrReplacementDiskInPool = errors.New("replacement disk already in pool")

// ErrFailingDiskNotInPool is returned when the failing disk is not in the pool.
var ErrFailingDiskNotInPool = errors.New("failing disk not in pool")

// ErrReplacementTooSmall is returned when the replacement disk is smaller than the failing disk.
var ErrReplacementTooSmall = errors.New("replacement disk smaller than failing disk")

// ErrNewDiskInPool is returned when the new disk for expansion is already in the pool.
var ErrNewDiskInPool = errors.New("new disk already in pool")

// ErrUnknownMechanism is returned for an unrecognized pool mechanism.
var ErrUnknownMechanism = errors.New("unknown pool mechanism")

// StrategyFor returns the replacement strategy for a given pool mechanism.
// Mapping confirmed by Captain 2026-10-30 17:28 (per PRD FR-1.11):
//
//	single           -> CopyOut
//	mirror           -> CopyOut (copy from surviving mirror)
//	parity-scheduled -> RebuildParity (SnapRAID path per PRD)
//	zfs              -> NativeReplace
//	stripe (RAID0)   -> Unsupported (Phase 1: replacement = rebuild pool)
func StrategyFor(mechanism string) Strategy {
	switch mechanism {
	case "single":
		return StrategyCopyOut
	case "mirror":
		return StrategyCopyOut
	case "parity-scheduled":
		return StrategyRebuildParity
	case "zfs":
		return StrategyNativeReplace
	case "stripe":
		return StrategyUnsupported
	default:
		return StrategyUnsupported
	}
}

// calculateUsableBytes computes usable bytes for a pool given its mechanism and disk sizes.
// This is a pure planning function — no I/O, no driver knowledge.
func calculateUsableBytes(mechanism string, diskSizes []int64) int64 {
	if len(diskSizes) == 0 {
		return 0
	}

	switch mechanism {
	case "single":
		// Single disk: usable = disk size
		return diskSizes[0]
	case "mirror":
		// Mirror: usable = smallest disk (all disks same data)
		minSize := diskSizes[0]
		for _, s := range diskSizes[1:] {
			if s < minSize {
				minSize = s
			}
		}
		return minSize
	case "parity-scheduled":
		// Parity (SnapRAID-style): usable = sum of data disks = (N-1) * min_size
		// Assuming 1 parity disk, N-1 data disks of equal size (min of all)
		if len(diskSizes) < 2 {
			return 0
		}
		minSize := diskSizes[0]
		for _, s := range diskSizes[1:] {
			if s < minSize {
				minSize = s
			}
		}
		return minSize * int64(len(diskSizes)-1)
	case "zfs":
		// ZFS: depends on vdev layout; for planning assume mirror/raidz similar to parity
		// Simplified: if 2 disks -> mirror, if 3+ -> raidz1 (N-1)
		if len(diskSizes) == 2 {
			minSize := diskSizes[0]
			if diskSizes[1] < minSize {
				minSize = diskSizes[1]
			}
			return minSize
		}
		if len(diskSizes) >= 3 {
			minSize := diskSizes[0]
			for _, s := range diskSizes[1:] {
				if s < minSize {
					minSize = s
				}
			}
			return minSize * int64(len(diskSizes)-1)
		}
		return 0
	case "stripe":
		// Stripe (RAID0): usable = sum of all disks
		var sum int64
		for _, s := range diskSizes {
			sum += s
		}
		return sum
	default:
		return 0
	}
}

// BuildPlan creates a replacement plan for a failing disk.
// Validates:
//   - replacement disk not already in pool
//   - failing disk is in pool
//   - replacement size >= failing disk size
//
// Preview: replacement takes failing disk's place (not additive).
func BuildPlan(poolID string, poolDisks []DiskRef, failing, replacement DiskRef, mechanism string) (ReplacePlan, error) {
	// Validate failing disk is in pool
	failingInPool := false
	failingIdx := -1
	for i, d := range poolDisks {
		if d.Name == failing.Name {
			failingInPool = true
			failingIdx = i
			break
		}
	}
	if !failingInPool {
		return ReplacePlan{}, fmt.Errorf("%w: %s", ErrFailingDiskNotInPool, failing.Name)
	}

	// Validate replacement not in pool
	for _, d := range poolDisks {
		if d.Name == replacement.Name {
			return ReplacePlan{}, fmt.Errorf("%w: %s", ErrReplacementDiskInPool, replacement.Name)
		}
	}

	// Validate replacement size >= failing size
	if replacement.SizeBytes < failing.SizeBytes {
		return ReplacePlan{}, fmt.Errorf("%w: replacement=%d failing=%d", ErrReplacementTooSmall, replacement.SizeBytes, failing.SizeBytes)
	}

	strategy := StrategyFor(mechanism)

	// Build new disk list: replace failing with replacement
	newDisks := make([]DiskRef, len(poolDisks))
	copy(newDisks, poolDisks)
	newDisks[failingIdx] = replacement

	// Calculate preview
	oldSizes := make([]int64, len(poolDisks))
	for i, d := range poolDisks {
		oldSizes[i] = d.SizeBytes
	}
	newSizes := make([]int64, len(newDisks))
	for i, d := range newDisks {
		newSizes[i] = d.SizeBytes
	}

	oldUsable := calculateUsableBytes(mechanism, oldSizes)
	newUsable := calculateUsableBytes(mechanism, newSizes)

	// Build steps based on strategy
	steps := buildSteps(strategy, failing.Name, replacement.Name)

	return ReplacePlan{
		PoolID:          poolID,
		PoolMechanism:   mechanism,
		FailingDisk:     failing,
		ReplacementDisk: replacement,
		Strategy:        strategy,
		Steps:           steps,
		Preview: CapacityPreview{
			OldUsableBytes: oldUsable,
			NewUsableBytes: newUsable,
			DeltaBytes:     newUsable - oldUsable,
		},
	}, nil
}

// BuildExpansionPlan creates an expansion plan (adding a new disk to the pool).
// Validates: new disk not already in pool.
// Preview: usable increases by new disk's contribution.
func BuildExpansionPlan(poolID string, poolDisks []DiskRef, newDisk DiskRef, mechanism string) (ReplacePlan, error) {
	// Validate new disk not in pool
	for _, d := range poolDisks {
		if d.Name == newDisk.Name {
			return ReplacePlan{}, fmt.Errorf("%w: %s", ErrNewDiskInPool, newDisk.Name)
		}
	}

	strategy := StrategyFor(mechanism)

	// Build new disk list: add new disk
	newDisks := append([]DiskRef{}, poolDisks...)
	newDisks = append(newDisks, newDisk)

	// Calculate preview
	oldSizes := make([]int64, len(poolDisks))
	for i, d := range poolDisks {
		oldSizes[i] = d.SizeBytes
	}
	newSizes := make([]int64, len(newDisks))
	for i, d := range newDisks {
		newSizes[i] = d.SizeBytes
	}

	oldUsable := calculateUsableBytes(mechanism, oldSizes)
	newUsable := calculateUsableBytes(mechanism, newSizes)

	// Expansion steps
	steps := []string{
		fmt.Sprintf("Add disk %s to pool %s", newDisk.Name, poolID),
		fmt.Sprintf("Expand filesystem/pool to include new disk"),
		fmt.Sprintf("Verify pool health"),
	}

	return ReplacePlan{
		PoolID:        poolID,
		PoolMechanism: mechanism,
		NewDisk:       newDisk,
		Strategy:      strategy,
		Steps:         steps,
		Preview: CapacityPreview{
			OldUsableBytes: oldUsable,
			NewUsableBytes: newUsable,
			DeltaBytes:     newUsable - oldUsable,
		},
	}, nil
}

// buildSteps returns the high-level steps for a given strategy.
func buildSteps(strategy Strategy, failing, replacement string) []string {
	switch strategy {
	case StrategyCopyOut:
		return []string{
			fmt.Sprintf("Unmount pool / stop services using pool"),
			fmt.Sprintf("Copy all data from failing disk %s to temporary location", failing),
			fmt.Sprintf("Physically replace disk %s with %s", failing, replacement),
			fmt.Sprintf("Copy data back to new disk %s", replacement),
			fmt.Sprintf("Remount pool / restart services"),
			fmt.Sprintf("Verify pool health"),
		}
	case StrategyRebuildParity:
		return []string{
			fmt.Sprintf("Physically replace disk %s with %s", failing, replacement),
			fmt.Sprintf("Run parity rebuild (e.g., snapraid sync) to reconstruct data on %s", replacement),
			fmt.Sprintf("Verify pool health"),
		}
	case StrategyNativeReplace:
		return []string{
			fmt.Sprintf("Initiate native device replace: %s -> %s", failing, replacement),
			fmt.Sprintf("Wait for replace operation to complete"),
			fmt.Sprintf("Verify pool health"),
		}
	case StrategyUnsupported:
		return []string{
			fmt.Sprintf("No safe replacement path for this mechanism"),
			fmt.Sprintf("Full pool rebuild required (backup data, recreate pool, restore)"),
		}
	default:
		return []string{"Unknown strategy"}
	}
}

// VerifyInventory compares two file inventories and returns mismatches.
// Pure data comparison — no I/O. Returns slice of mismatch descriptions.
func VerifyInventory(before, after []FileRef) []string {
	beforeMap := make(map[string]FileRef, len(before))
	for _, f := range before {
		beforeMap[f.Path] = f
	}

	afterMap := make(map[string]FileRef, len(after))
	for _, f := range after {
		afterMap[f.Path] = f
	}

	var mismatches []string

	// Check for missing or changed files in 'after'
	for path, bf := range beforeMap {
		af, ok := afterMap[path]
		if !ok {
			mismatches = append(mismatches, fmt.Sprintf("missing: %s", path))
			continue
		}
		if bf.SizeBytes != af.SizeBytes {
			mismatches = append(mismatches, fmt.Sprintf("size mismatch: %s (before=%d after=%d)", path, bf.SizeBytes, af.SizeBytes))
		}
		if bf.SHA256 != af.SHA256 {
			mismatches = append(mismatches, fmt.Sprintf("hash mismatch: %s", path))
		}
	}

	// Check for extra files in 'after'
	for path := range afterMap {
		if _, ok := beforeMap[path]; !ok {
			mismatches = append(mismatches, fmt.Sprintf("extra: %s", path))
		}
	}

	return mismatches
}
