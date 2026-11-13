package storage

import (
	"errors"
	"fmt"
)

const TB = 1_000_000_000_000

// PlanDisk represents a disk candidate for pool planning.
type PlanDisk struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

// PoolPlan is the result of planning a pool for a given mechanism and disks.
type PoolPlan struct {
	Mechanism            Mechanism `json:"mechanism"`
	UsableBytes          int64     `json:"usable_bytes"`
	OverheadBytes        int64     `json:"overhead_bytes"`
	Warnings             []string  `json:"warnings"`
	RequiresConfirmation bool      `json:"requires_confirmation"`
}

// ErrEmptyDisks is returned when no disks are provided.
var ErrEmptyDisks = errors.New("no disks provided")

// ErrInvalidDiskSize is returned when a disk has non-positive size.
var ErrInvalidDiskSize = errors.New("disk size must be positive")

// ErrUnknownMechanism is returned when the mechanism is not recognized.
var ErrUnknownMechanism = errors.New("unknown mechanism")

// ErrSingleDiskCount is returned when single mechanism doesn't have exactly 1 disk.
var ErrSingleDiskCount = errors.New("single mechanism requires exactly 1 disk")

// ErrMinTwoDisks is returned when a mechanism requires at least 2 disks.
var ErrMinTwoDisks = errors.New("mechanism requires at least 2 disks")

// ErrZFSNotPlanned is returned for ZFS mechanism in Phase 1.
var ErrZFSNotPlanned = errors.New("zfs is not planned in Phase 1 (optional driver, installed separately on device)")

// PlanPool computes the pool plan for a given mechanism and disks.
func PlanPool(mechanism Mechanism, disks []PlanDisk) (PoolPlan, error) {
	if len(disks) == 0 {
		return PoolPlan{}, ErrEmptyDisks
	}

	var totalSize int64
	var maxSize int64
	for _, d := range disks {
		if d.SizeBytes <= 0 {
			return PoolPlan{}, fmt.Errorf("%w: disk %q has size %d", ErrInvalidDiskSize, d.Name, d.SizeBytes)
		}
		totalSize += d.SizeBytes
		if d.SizeBytes > maxSize {
			maxSize = d.SizeBytes
		}
	}

	switch mechanism {
	case MechanismSingle:
		if len(disks) != 1 {
			return PoolPlan{}, ErrSingleDiskCount
		}
		return PoolPlan{
			Mechanism:     MechanismSingle,
			UsableBytes:   disks[0].SizeBytes,
			OverheadBytes: 0,
		}, nil

	case MechanismStripe:
		if len(disks) < 2 {
			return PoolPlan{}, ErrMinTwoDisks
		}
		return PoolPlan{
			Mechanism:            MechanismStripe,
			UsableBytes:          totalSize,
			OverheadBytes:        0,
			Warnings:             []string{"RAID0 striping has no protection: if one disk fails, the entire pool is lost."},
			RequiresConfirmation: true,
		}, nil

	case MechanismMirror:
		if len(disks) < 2 {
			return PoolPlan{}, ErrMinTwoDisks
		}
		usable := totalSize / 2
		return PoolPlan{
			Mechanism:     MechanismMirror,
			UsableBytes:   usable,
			OverheadBytes: totalSize - usable,
		}, nil

	case MechanismParityScheduled:
		if len(disks) < 2 {
			return PoolPlan{}, ErrMinTwoDisks
		}
		usable := totalSize - maxSize
		return PoolPlan{
			Mechanism:     MechanismParityScheduled,
			UsableBytes:   usable,
			OverheadBytes: maxSize,
		}, nil

	case MechanismZFS:
		return PoolPlan{}, ErrZFSNotPlanned

	default:
		return PoolPlan{}, fmt.Errorf("%w: %q", ErrUnknownMechanism, mechanism)
	}
}
