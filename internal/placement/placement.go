// Package placement implements the pure decision logic for Cold pool
// disk wake planning (FR-1.5). It contains NO I/O, NO hardware calls,
// and NO power engine interaction. It only computes which disks need
// to be awake for a given collection access.
//
// FR-1.5: Cold placement is per-collection-per-disk — one access request
// wakes AT MOST one Cold disk (the collection's disk), plus the parity
// disk ONLY for write operations on parity-scheduled pools.
//
// Parity disk rule (brief WS28): on parity-scheduled pools, the parity
// disk is the LARGEST disk in the pool. If multiple disks tie for
// largest size, the first by alphabetical name order is chosen
// (deterministic). This convention matches ARCHITECTURE §5 example:
// 2+4+8+8 TB → parity is 8 TB (first 8 TB by name).
package placement

import (
	"errors"
	"sort"
)

// Operation is the type of access operation.
type Operation string

const (
	OpRead  Operation = "read"
	OpWrite Operation = "write"
)

// PoolDisk represents a disk in a pool with its size.
type PoolDisk struct {
	Name      string
	SizeBytes int64
}

// Pool represents a storage pool for placement decisions.
type Pool struct {
	ID        string
	Tier      string
	Mechanism string
	Disks     []PoolDisk
}

// Collection represents a collection for placement decisions.
type Collection struct {
	DiskName string
}

// Decide returns the list of disk names that must be awake for the
// given operation on the collection in the pool.
//
// Invariants:
//   - Result never contains duplicates.
//   - Result is never empty when collection.DiskName is known.
//   - Result never has more than 2 entries.
//   - For read (any tier): exactly [collection.DiskName].
//   - For write + parity-scheduled: [collection.DiskName, parityDisk]
//     unless collection disk IS the parity disk, then just [collection.DiskName].
//   - For write + other mechanisms: exactly [collection.DiskName].
//   - Unknown operation returns error.
//   - Empty collection.DiskName returns error (never fallback to wake-all).
func Decide(pool Pool, collection Collection, op Operation) ([]string, error) {
	if op != OpRead && op != OpWrite {
		return nil, errors.New("unknown operation: must be read or write")
	}

	if collection.DiskName == "" {
		return nil, errors.New("placement unknown: collection has no disk assigned")
	}

	// Read: always exactly the collection's disk, regardless of tier/mechanism.
	if op == OpRead {
		return []string{collection.DiskName}, nil
	}

	// Write: depends on mechanism.
	if pool.Mechanism == "parity-scheduled" {
		parityDisk, err := findParityDisk(pool.Disks)
		if err != nil {
			return nil, err
		}

		// If collection disk IS the parity disk, don't duplicate.
		if collection.DiskName == parityDisk {
			return []string{collection.DiskName}, nil
		}
		return []string{collection.DiskName, parityDisk}, nil
	}

	// All other mechanisms (single, mirror, stripe, zfs): just the collection disk.
	return []string{collection.DiskName}, nil
}

// findParityDisk returns the name of the parity disk for a parity-scheduled pool.
// Parity disk = largest disk by SizeBytes. Tie-break: first by alphabetical name.
func findParityDisk(disks []PoolDisk) (string, error) {
	if len(disks) == 0 {
		return "", errors.New("parity-scheduled pool has no disks")
	}

	// Sort by size descending, then name ascending for deterministic tie-break.
	sorted := make([]PoolDisk, len(disks))
	copy(sorted, disks)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].SizeBytes != sorted[j].SizeBytes {
			return sorted[i].SizeBytes > sorted[j].SizeBytes
		}
		return sorted[i].Name < sorted[j].Name
	})

	return sorted[0].Name, nil
}
