package ota

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Switcher abstracts the A/B slot mechanism.
type Switcher interface {
	// ActiveSlot returns the currently active slot identifier ("slot-a" or "slot-b").
	ActiveSlot() string
	// InactiveSlot returns the inactive slot identifier.
	InactiveSlot() string
	// StageToInactive copies the artifact to the inactive slot.
	StageToInactive(ctx context.Context, artifactPath string) error
	// MarkPendingSwitch marks the inactive slot as the next boot slot.
	MarkPendingSwitch(ctx context.Context) error
	// CommitSwitch makes the pending slot the active slot.
	CommitSwitch(ctx context.Context) error
	// RollbackSwitch reverts to the previous active slot.
	RollbackSwitch(ctx context.Context) error
}

// DirSwitcher implements Switcher using directories on the local filesystem.
type DirSwitcher struct {
	dataDir    string
	activeFile string
	pendingFile string
	slotA      string
	slotB      string
}

// NewDirSwitcher creates a new DirSwitcher.
// dataDir is the base directory for OTA data (e.g., ~/.kelvios/ota/).
func NewDirSwitcher(dataDir string) (*DirSwitcher, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	slotA := filepath.Join(dataDir, "slot-a")
	slotB := filepath.Join(dataDir, "slot-b")
	if err := os.MkdirAll(slotA, 0o755); err != nil {
		return nil, fmt.Errorf("create slot-a: %w", err)
	}
	if err := os.MkdirAll(slotB, 0o755); err != nil {
		return nil, fmt.Errorf("create slot-b: %w", err)
	}
	ds := &DirSwitcher{
		dataDir:     dataDir,
		activeFile:  filepath.Join(dataDir, "active.slot"),
		pendingFile: filepath.Join(dataDir, "pending.slot"),
		slotA:       slotA,
		slotB:       slotB,
	}
	// Initialize active slot to slot-a if not set
	if _, err := os.Stat(ds.activeFile); os.IsNotExist(err) {
		if err := os.WriteFile(ds.activeFile, []byte("slot-a"), 0o644); err != nil {
			return nil, fmt.Errorf("write initial active slot: %w", err)
		}
	}
	return ds, nil
}

// ActiveSlot returns the currently active slot.
func (d *DirSwitcher) ActiveSlot() string {
	data, err := os.ReadFile(d.activeFile)
	if err != nil {
		return "slot-a" // default
	}
	return string(data)
}

// InactiveSlot returns the inactive slot.
func (d *DirSwitcher) InactiveSlot() string {
	active := d.ActiveSlot()
	if active == "slot-a" {
		return "slot-b"
	}
	return "slot-a"
}

// slotPath returns the filesystem path for a slot.
func (d *DirSwitcher) slotPath(slot string) string {
	if slot == "slot-a" {
		return d.slotA
	}
	return d.slotB
}

// StageToInactive copies the artifact to the inactive slot directory.
func (d *DirSwitcher) StageToInactive(ctx context.Context, artifactPath string) error {
	inactive := d.InactiveSlot()
	targetDir := d.slotPath(inactive)
	// Clear the inactive slot directory
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		return fmt.Errorf("read inactive slot: %w", err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(targetDir, e.Name())); err != nil {
			return fmt.Errorf("clear inactive slot: %w", err)
		}
	}
	// Copy artifact to inactive slot
	src, err := os.Open(artifactPath)
	if err != nil {
		return fmt.Errorf("open artifact: %w", err)
	}
	defer src.Close()
	dstPath := filepath.Join(targetDir, "artifact")
	dst, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create artifact in slot: %w", err)
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy artifact: %w", err)
	}
	return nil
}

// MarkPendingSwitch marks the inactive slot as pending for next boot.
func (d *DirSwitcher) MarkPendingSwitch(ctx context.Context) error {
	inactive := d.InactiveSlot()
	return os.WriteFile(d.pendingFile, []byte(inactive), 0o644)
}

// CommitSwitch makes the pending slot the active slot.
func (d *DirSwitcher) CommitSwitch(ctx context.Context) error {
	pendingData, err := os.ReadFile(d.pendingFile)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New("no pending switch to commit")
		}
		return fmt.Errorf("read pending slot: %w", err)
	}
	pending := string(pendingData)
	if pending != "slot-a" && pending != "slot-b" {
		return fmt.Errorf("invalid pending slot: %s", pending)
	}
	// Write new active slot
	if err := os.WriteFile(d.activeFile, []byte(pending), 0o644); err != nil {
		return fmt.Errorf("write active slot: %w", err)
	}
	// Clear pending
	if err := os.Remove(d.pendingFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear pending slot: %w", err)
	}
	return nil
}

// RollbackSwitch reverts to the previous active slot (clears pending).
func (d *DirSwitcher) RollbackSwitch(ctx context.Context) error {
	// Simply clear the pending marker; active slot remains unchanged
	if err := os.Remove(d.pendingFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear pending slot: %w", err)
	}
	return nil
}

// PendingSlot returns the pending slot if any, empty string otherwise.
func (d *DirSwitcher) PendingSlot() string {
	data, err := os.ReadFile(d.pendingFile)
	if err != nil {
		return ""
	}
	return string(data)
}

// ArtifactPath returns the path to the artifact in the given slot.
func (d *DirSwitcher) ArtifactPath(slot string) string {
	return filepath.Join(d.slotPath(slot), "artifact")
}

// HasArtifact returns true if the slot has an artifact staged.
func (d *DirSwitcher) HasArtifact(slot string) bool {
	_, err := os.Stat(d.ArtifactPath(slot))
	return err == nil
}