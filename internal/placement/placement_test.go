// Package placement tests the pure decision logic for Cold pool
// disk wake planning (FR-1.5).
package placement

import (
	"testing"
)

const TB = int64(1_000_000_000_000)

func TestDecide_ReadAlwaysSingleDisk(t *testing.T) {
	basePool := Pool{
		ID:        "pool-cold",
		Tier:      "cold",
		Mechanism: "parity-scheduled",
		Disks: []PoolDisk{
			{Name: "sda", SizeBytes: 2 * TB},
			{Name: "sdb", SizeBytes: 4 * TB},
			{Name: "sdc", SizeBytes: 8 * TB},
			{Name: "sdd", SizeBytes: 8 * TB},
		},
	}

	tests := []struct {
		name       string
		pool       Pool
		collection Collection
		want       []string
	}{
		{"collection on 2TB disk", basePool, Collection{DiskName: "sda"}, []string{"sda"}},
		{"collection on 4TB disk", basePool, Collection{DiskName: "sdb"}, []string{"sdb"}},
		{"collection on 8TB disk (first)", basePool, Collection{DiskName: "sdc"}, []string{"sdc"}},
		{"collection on 8TB disk (second)", basePool, Collection{DiskName: "sdd"}, []string{"sdd"}},
		{"hot tier read", func() Pool { p := basePool; p.Tier = "hot"; return p }(), Collection{DiskName: "sda"}, []string{"sda"}},
		{"warm tier read", func() Pool { p := basePool; p.Tier = "warm"; return p }(), Collection{DiskName: "sda"}, []string{"sda"}},
		{"single mechanism read", func() Pool {
			p := basePool
			p.Mechanism = "single"
			p.Disks = []PoolDisk{{Name: "sda", SizeBytes: 4 * TB}}
			return p
		}(), Collection{DiskName: "sda"}, []string{"sda"}},
		{"mirror mechanism read", func() Pool { p := basePool; p.Mechanism = "mirror"; return p }(), Collection{DiskName: "sda"}, []string{"sda"}},
		{"stripe mechanism read", func() Pool { p := basePool; p.Mechanism = "stripe"; return p }(), Collection{DiskName: "sda"}, []string{"sda"}},
		{"zfs mechanism read", func() Pool { p := basePool; p.Mechanism = "zfs"; return p }(), Collection{DiskName: "sda"}, []string{"sda"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decide(tt.pool, tt.collection, OpRead)
			if err != nil {
				t.Fatalf("Decide read: unexpected error: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("read result len = %d, want 1", len(got))
			}
			if got[0] != tt.want[0] {
				t.Errorf("read result = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDecide_WriteParityScheduledTwoDisksUnlessSame(t *testing.T) {
	pool := Pool{
		ID:        "pool-cold",
		Tier:      "cold",
		Mechanism: "parity-scheduled",
		Disks: []PoolDisk{
			{Name: "sda", SizeBytes: 2 * TB},
			{Name: "sdb", SizeBytes: 4 * TB},
			{Name: "sdc", SizeBytes: 8 * TB},
			{Name: "sdd", SizeBytes: 8 * TB},
		},
	}

	tests := []struct {
		name       string
		collection Collection
		want       []string
		wantLen    int
	}{
		{"collection on 2TB (smallest), parity is 8TB (sdc)", Collection{DiskName: "sda"}, []string{"sda", "sdc"}, 2},
		{"collection on 4TB, parity is 8TB (sdc)", Collection{DiskName: "sdb"}, []string{"sdb", "sdc"}, 2},
		{"collection on 8TB (first, IS parity), single entry", Collection{DiskName: "sdc"}, []string{"sdc"}, 1},
		{"collection on 8TB (second, not parity), two entries", Collection{DiskName: "sdd"}, []string{"sdd", "sdc"}, 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Decide(pool, tt.collection, OpWrite)
			if err != nil {
				t.Fatalf("Decide write: unexpected error: %v", err)
			}
			if len(got) != tt.wantLen {
				t.Fatalf("write result len = %d, want %d", len(got), tt.wantLen)
			}
			// Check contents (order: collection disk first, then parity)
			if len(got) == 1 {
				if got[0] != tt.want[0] {
					t.Errorf("write result = %v, want %v", got, tt.want)
				}
			} else if len(got) == 2 {
				if got[0] != tt.want[0] || got[1] != tt.want[1] {
					t.Errorf("write result = %v, want %v", got, tt.want)
				}
			}
			// Invariant: no duplicates
			if len(got) == 2 && got[0] == got[1] {
				t.Errorf("write result has duplicate: %v", got)
			}
		})
	}
}

func TestDecide_WriteOtherMechanismsSingleDisk(t *testing.T) {
	basePool := Pool{
		ID:   "pool-cold",
		Tier: "cold",
		Disks: []PoolDisk{
			{Name: "sda", SizeBytes: 2 * TB},
			{Name: "sdb", SizeBytes: 4 * TB},
		},
	}

	mechanisms := []string{"single", "mirror", "stripe", "zfs"}
	for _, mech := range mechanisms {
		t.Run("write_"+mech, func(t *testing.T) {
			pool := basePool
			pool.Mechanism = mech
			// single needs exactly 1 disk
			if mech == "single" {
				pool.Disks = []PoolDisk{{Name: "sda", SizeBytes: 4 * TB}}
			}

			got, err := Decide(pool, Collection{DiskName: "sda"}, OpWrite)
			if err != nil {
				t.Fatalf("Decide write %s: unexpected error: %v", mech, err)
			}
			if len(got) != 1 {
				t.Fatalf("write %s result len = %d, want 1", mech, len(got))
			}
			if got[0] != "sda" {
				t.Errorf("write %s result = %v, want [sda]", mech, got)
			}
		})
	}
}

func TestDecide_UnknownOperationError(t *testing.T) {
	pool := Pool{ID: "p", Tier: "cold", Mechanism: "single", Disks: []PoolDisk{{Name: "sda", SizeBytes: 4 * TB}}}
	_, err := Decide(pool, Collection{DiskName: "sda"}, Operation("delete"))
	if err == nil {
		t.Fatal("expected error for unknown operation")
	}
	if err.Error() != "unknown operation: must be read or write" {
		t.Errorf("error message = %q, want %q", err.Error(), "unknown operation: must be read or write")
	}
}

func TestDecide_EmptyDiskNameError(t *testing.T) {
	pool := Pool{ID: "p", Tier: "cold", Mechanism: "single", Disks: []PoolDisk{{Name: "sda", SizeBytes: 4 * TB}}}
	_, err := Decide(pool, Collection{DiskName: ""}, OpRead)
	if err == nil {
		t.Fatal("expected error for empty disk name")
	}
	if err.Error() != "placement unknown: collection has no disk assigned" {
		t.Errorf("error message = %q, want %q", err.Error(), "placement unknown: collection has no disk assigned")
	}

	_, err = Decide(pool, Collection{DiskName: ""}, OpWrite)
	if err == nil {
		t.Fatal("expected error for empty disk name on write")
	}
}

func TestDecide_ParityScheduledNoDisksError(t *testing.T) {
	pool := Pool{ID: "p", Tier: "cold", Mechanism: "parity-scheduled", Disks: []PoolDisk{}}
	_, err := Decide(pool, Collection{DiskName: "sda"}, OpWrite)
	if err == nil {
		t.Fatal("expected error for parity-scheduled pool with no disks")
	}
	if err.Error() != "parity-scheduled pool has no disks" {
		t.Errorf("error message = %q, want %q", err.Error(), "parity-scheduled pool has no disks")
	}
}

func TestDecide_Invariants(t *testing.T) {
	basePool := Pool{
		ID:        "pool-cold",
		Tier:      "cold",
		Mechanism: "parity-scheduled",
		Disks: []PoolDisk{
			{Name: "sda", SizeBytes: 2 * TB},
			{Name: "sdb", SizeBytes: 4 * TB},
			{Name: "sdc", SizeBytes: 8 * TB},
		},
	}

	// Test all combinations of tier x mechanism x read/write
	tiers := []string{"hot", "warm", "cold"}
	mechanisms := []string{"single", "mirror", "parity-scheduled", "stripe", "zfs"}
	operations := []Operation{OpRead, OpWrite}

	for _, tier := range tiers {
		for _, mech := range mechanisms {
			for _, op := range operations {
				t.Run(tier+"_"+mech+"_"+string(op), func(t *testing.T) {
					p := basePool
					p.Tier = tier
					p.Mechanism = mech
					if mech == "single" {
						p.Disks = []PoolDisk{{Name: "sda", SizeBytes: 4 * TB}}
					}

					got, err := Decide(p, Collection{DiskName: "sda"}, op)
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}

					// Invariant: never empty when DiskName known
					if len(got) == 0 {
						t.Fatal("result is empty")
					}

					// Invariant: never more than 2 entries
					if len(got) > 2 {
						t.Fatalf("result has %d entries, max is 2", len(got))
					}

					// Invariant: no duplicates
					if len(got) == 2 && got[0] == got[1] {
						t.Fatal("result has duplicate entries")
					}

					// Invariant: read always exactly 1
					if op == OpRead && len(got) != 1 {
						t.Fatalf("read result has %d entries, want 1", len(got))
					}

					// Invariant: write parity-scheduled = 2 unless collection is parity
					if op == OpWrite && mech == "parity-scheduled" {
						parityDisk := "sdc" // largest
						if got[0] == parityDisk && len(got) != 1 {
							t.Fatalf("collection is parity disk but got %d entries", len(got))
						}
						if got[0] != parityDisk && len(got) != 2 {
							t.Fatalf("collection not parity but got %d entries, want 2", len(got))
						}
					}

					// Invariant: write other mechanisms = 1
					if op == OpWrite && mech != "parity-scheduled" && len(got) != 1 {
						t.Fatalf("write %s result has %d entries, want 1", mech, len(got))
					}
				})
			}
		}
	}
}

func TestFindParityDisk_LargestDisk(t *testing.T) {
	disks := []PoolDisk{
		{Name: "sda", SizeBytes: 2 * TB},
		{Name: "sdb", SizeBytes: 4 * TB},
		{Name: "sdc", SizeBytes: 8 * TB},
		{Name: "sdd", SizeBytes: 8 * TB},
	}
	parity, err := findParityDisk(disks)
	if err != nil {
		t.Fatalf("findParityDisk error: %v", err)
	}
	if parity != "sdc" {
		t.Errorf("parity disk = %q, want %q (largest, first by name)", parity, "sdc")
	}
}

func TestFindParityDisk_TieBreakAlphabetical(t *testing.T) {
	// Same sizes, different names - first alphabetically wins
	disks := []PoolDisk{
		{Name: "sdz", SizeBytes: 8 * TB},
		{Name: "sda", SizeBytes: 8 * TB},
		{Name: "sdm", SizeBytes: 8 * TB},
	}
	parity, err := findParityDisk(disks)
	if err != nil {
		t.Fatalf("findParityDisk error: %v", err)
	}
	if parity != "sda" {
		t.Errorf("parity disk = %q, want %q (alphabetical tie-break)", parity, "sda")
	}
}

func TestFindParityDisk_SingleDisk(t *testing.T) {
	disks := []PoolDisk{{Name: "sda", SizeBytes: 4 * TB}}
	parity, err := findParityDisk(disks)
	if err != nil {
		t.Fatalf("findParityDisk error: %v", err)
	}
	if parity != "sda" {
		t.Errorf("parity disk = %q, want %q", parity, "sda")
	}
}

func TestFindParityDisk_EmptyError(t *testing.T) {
	_, err := findParityDisk([]PoolDisk{})
	if err == nil {
		t.Fatal("expected error for empty disks")
	}
	if err.Error() != "parity-scheduled pool has no disks" {
		t.Errorf("error message = %q, want %q", err.Error(), "parity-scheduled pool has no disks")
	}
}
