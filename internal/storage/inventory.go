package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// lsblkDevice mirrors one entry of `lsblk -J` output. Model, Serial and
// Mountpoint are pointers because lsblk emits JSON null for them. Size is
// kept raw because it is a number with `-b` but a human string ("7.5G")
// without it.
type lsblkDevice struct {
	Name       string          `json:"name"`
	Model      *string         `json:"model"`
	Serial     *string         `json:"serial"`
	Size       json.RawMessage `json:"size"`
	Type       string          `json:"type"`
	Rotational bool            `json:"rota"`
	Mountpoint *string         `json:"mountpoint"`
	Children   []lsblkDevice   `json:"children"`
}

type lsblkOutput struct {
	BlockDevices []lsblkDevice `json:"blockdevices"`
}

// Inventory enumerates the machine's disks passively: it runs
// `lsblk -J -b` and cross-checks the result against /sys/block. It never
// issues a command that could wake a sleeping disk (no hdparm -C or
// similar probes), so PowerState is always PowerUnknown at this layer.
func Inventory() ([]Disk, error) {
	out, err := exec.Command("lsblk", "-J", "-b",
		"-o", "NAME,MODEL,SERIAL,SIZE,TYPE,ROTA,MOUNTPOINT").Output()
	if err != nil {
		return nil, fmt.Errorf("run lsblk: %w", err)
	}

	disks, err := ParseLSBLK(out)
	if err != nil {
		return nil, err
	}

	// Cross-check against /sys/block: drop anything the kernel does not
	// currently expose as a block device, and fill an empty model from
	// sysfs (a passive file read — it never touches the disk itself).
	if entries, err := os.ReadDir("/sys/block"); err == nil {
		present := make(map[string]bool, len(entries))
		for _, e := range entries {
			present[e.Name()] = true
		}
		kept := disks[:0]
		for _, d := range disks {
			if !present[d.Name] {
				continue
			}
			if d.Model == "" {
				if b, err := os.ReadFile(filepath.Join("/sys/block", d.Name, "device", "model")); err == nil {
					d.Model = strings.TrimSpace(string(b))
				}
			}
			kept = append(kept, d)
		}
		disks = kept
	}

	return disks, nil
}

// ParseLSBLK converts `lsblk -J` output into the inventory disk list.
// Only top-level devices of type "disk" are reported (partitions, loop
// devices, ROM drives etc. are skipped); a disk counts as mounted when
// the disk itself or any of its partitions is mounted. Exported so tests
// can run against fixture output without real hardware.
func ParseLSBLK(data []byte) ([]Disk, error) {
	var parsed lsblkOutput
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("parse lsblk output: %w", err)
	}

	disks := []Disk{}
	for _, dev := range parsed.BlockDevices {
		if dev.Type != "disk" {
			continue
		}
		size, err := parseSize(dev.Size)
		if err != nil {
			return nil, fmt.Errorf("disk %s: %w", dev.Name, err)
		}
		mountpoint := firstMountpoint(dev)
		disks = append(disks, Disk{
			Name:       dev.Name,
			Path:       "/dev/" + dev.Name,
			Model:      deref(dev.Model),
			Serial:     deref(dev.Serial),
			SizeBytes:  size,
			Rotational: dev.Rotational,
			Mounted:    mountpoint != "",
			Mountpoint: mountpoint,
			PowerState: PowerUnknown,
			NodeID:     DefaultNodeID,
		})
	}

	sort.Slice(disks, func(i, j int) bool { return disks[i].Name < disks[j].Name })
	return disks, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// firstMountpoint returns the device's own mountpoint, or the first
// non-empty mountpoint found among its children (partitions).
func firstMountpoint(dev lsblkDevice) string {
	if dev.Mountpoint != nil && *dev.Mountpoint != "" {
		return *dev.Mountpoint
	}
	for _, child := range dev.Children {
		if mp := firstMountpoint(child); mp != "" {
			return mp
		}
	}
	return ""
}

// parseSize accepts a byte count as a JSON number, a quoted integer, or
// a human-readable lsblk size such as "7.5G" (1024-based suffixes).
func parseSize(raw json.RawMessage) (int64, error) {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(s, "B"), 64)
	if err == nil {
		return int64(f), nil
	}
	num := strings.TrimRight(s, "KMGTPEB")
	mult, ok := sizeSuffixMultiplier(s[len(num):])
	if !ok {
		return 0, fmt.Errorf("unrecognized size %q", s)
	}
	f, err = strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("unrecognized size %q", s)
	}
	return int64(f * float64(mult)), nil
}

func sizeSuffixMultiplier(suffix string) (int64, bool) {
	switch strings.ToUpper(strings.TrimSuffix(suffix, "B")) {
	case "K":
		return 1 << 10, true
	case "M":
		return 1 << 20, true
	case "G":
		return 1 << 30, true
	case "T":
		return 1 << 40, true
	case "P":
		return 1 << 50, true
	case "E":
		return 1 << 60, true
	}
	return 0, false
}
