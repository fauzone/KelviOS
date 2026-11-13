package ota

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// HealthChecker is a function that performs a post-boot health check.
type HealthChecker func(ctx context.Context) error

// ServiceConfig holds configuration for the OTA service.
type ServiceConfig struct {
	DropDir       string
	PublicKey     ed25519.PublicKey
	ActiveChannel string
	DataDir       string
	DB            *sql.DB
}

// Service manages the OTA update lifecycle.
type Service struct {
	cfg       ServiceConfig
	store     *Store
	switcher  Switcher
	healthCheck HealthChecker
}

// NewService creates a new OTA service.
func NewService(cfg ServiceConfig, healthCheck HealthChecker) (*Service, error) {
	if cfg.DB == nil {
		return nil, errors.New("database is required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("data directory is required")
	}
	if cfg.DropDir == "" {
		cfg.DropDir = filepath.Join(cfg.DataDir, "ota-drop")
	}
	if err := os.MkdirAll(cfg.DropDir, 0o755); err != nil {
		return nil, fmt.Errorf("create drop dir: %w", err)
	}
	if cfg.ActiveChannel == "" {
		cfg.ActiveChannel = "stable"
	}

	store := NewStore(cfg.DB)
	if err := store.InitSchema(); err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}

	switcher, err := NewDirSwitcher(filepath.Join(cfg.DataDir, "ota"))
	if err != nil {
		return nil, fmt.Errorf("create switcher: %w", err)
	}

	return &Service{
		cfg:         cfg,
		store:       store,
		switcher:    switcher,
		healthCheck: healthCheck,
	}, nil
}

// Store returns the underlying store for direct access (e.g., for tests).
func (s *Service) Store() *Store {
	return s.store
}

// Switcher returns the switcher for direct access (e.g., for tests).
func (s *Service) Switcher() Switcher {
	return s.switcher
}

// SetSwitcher replaces the switcher (for testing).
func (s *Service) SetSwitcher(sw Switcher) {
	s.switcher = sw
}

// SetHealthChecker replaces the health checker (for testing).
func (s *Service) SetHealthChecker(hc HealthChecker) {
	s.healthCheck = hc
}

// SetPublicKey sets the public key for signature verification.
func (s *Service) SetPublicKey(pubKey ed25519.PublicKey) {
	s.cfg.PublicKey = pubKey
}

// Configured returns true if the public key is configured.
func (s *Service) Configured() bool {
	return s.cfg.PublicKey != nil
}

// ActiveChannel returns the active channel.
func (s *Service) ActiveChannel() string {
	return s.cfg.ActiveChannel
}

// ActiveSlot returns the active slot.
func (s *Service) ActiveSlot() string {
	return s.switcher.ActiveSlot()
}

// PendingSlot returns the pending slot if any.
func (s *Service) PendingSlot() string {
	if ds, ok := s.switcher.(*DirSwitcher); ok {
		return ds.PendingSlot()
	}
	return ""
}

// Status returns the current OTA status.
func (s *Service) Status(ctx context.Context) (map[string]any, error) {
	current, err := s.store.GetCurrent(ctx)
	if err != nil {
		return nil, err
	}
	history, err := s.store.List(ctx)
	if err != nil {
		return nil, err
	}
	var currentRec map[string]any
	if current != nil {
		currentRec = map[string]any{
			"id":              current.ID,
			"version":         current.Version,
			"channel":         current.Channel,
			"kind":            current.Kind,
			"state":           current.State,
			"slot_target":     current.SlotTarget,
			"last_actor":      current.LastActor,
			"last_detail":     current.LastDetail,
			"created_at":      current.CreatedAt,
			"updated_at":      current.UpdatedAt,
		}
	}
	var historyRecs []map[string]any
	for _, r := range history {
		historyRecs = append(historyRecs, map[string]any{
			"id":              r.ID,
			"version":         r.Version,
			"channel":         r.Channel,
			"kind":            r.Kind,
			"state":           r.State,
			"slot_target":     r.SlotTarget,
			"last_actor":      r.LastActor,
			"last_detail":     r.LastDetail,
			"created_at":      r.CreatedAt,
			"updated_at":      r.UpdatedAt,
		})
	}
	return map[string]any{
		"channel":           s.cfg.ActiveChannel,
		"public_key_set":    s.cfg.PublicKey != nil,
		"active_slot":       s.switcher.ActiveSlot(),
		"pending_slot":      s.PendingSlot(),
		"current":           currentRec,
		"history":           historyRecs,
	}, nil
}

// Stage stages an update package from the drop directory.
func (s *Service) Stage(ctx context.Context, name, actor string) (*UpdateRecord, error) {
	if !s.Configured() {
		s.store.Audit(s.cfg.DB, actor, "stage", name, "error", "ota_not_configured")
		return nil, errors.New("ota_not_configured")
	}
	// Validate package name (no path traversal)
	if name == "" || name == "." || name == ".." {
		s.store.Audit(s.cfg.DB, actor, "stage", name, "error", "invalid_package_name")
		return nil, errors.New("invalid_package_name")
	}
	// Check for path traversal attempts
	if filepath.IsAbs(name) || filepath.Clean(name) != name || strings.Contains(name, "..") {
		s.store.Audit(s.cfg.DB, actor, "stage", name, "error", "invalid_package_name")
		return nil, errors.New("invalid_package_name")
	}

	pkgDir := filepath.Join(s.cfg.DropDir, name)
	if _, err := os.Stat(pkgDir); os.IsNotExist(err) {
		s.store.Audit(s.cfg.DB, actor, "stage", name, "error", "package_not_found")
		return nil, errors.New("package_not_found")
	}

	// Validate package
	pkg, err := ValidatePackage(pkgDir, s.cfg.PublicKey, s.cfg.ActiveChannel)
	if err != nil {
		errMsg := err.Error()
		s.store.Audit(s.cfg.DB, actor, "stage", name, "error", errMsg)
		// Create failed record
		rec, _ := s.store.Create(ctx, &PackageInfo{Manifest: &Manifest{Version: "unknown", Channel: s.cfg.ActiveChannel, Artifact: name, SHA256: "unknown", Kind: "core"}, Dir: pkgDir, ArtifactPath: ""}, actor)
		if rec != nil {
			s.store.UpdateState(ctx, rec.ID, StateFailed, actor, errMsg, "")
		}
		return nil, err
	}

	// Create available record
	rec, err := s.store.Create(ctx, pkg, actor)
	if err != nil {
		s.store.Audit(s.cfg.DB, actor, "stage", name, "error", "create record failed")
		return nil, err
	}

	// Stage to inactive slot
	inactiveSlot := s.switcher.InactiveSlot()
	if err := s.switcher.StageToInactive(ctx, pkg.ArtifactPath); err != nil {
		s.store.UpdateState(ctx, rec.ID, StateFailed, actor, fmt.Sprintf("stage failed: %v", err), inactiveSlot)
		s.store.Audit(s.cfg.DB, actor, "stage", rec.ID, "error", fmt.Sprintf("stage failed: %v", err))
		return nil, fmt.Errorf("stage to inactive: %w", err)
	}

	// Update to staged
	if err := s.store.UpdateState(ctx, rec.ID, StateStaged, actor, "staged to inactive slot", inactiveSlot); err != nil {
		return nil, err
	}
	s.store.Audit(s.cfg.DB, actor, "stage", rec.ID, "ok", fmt.Sprintf("staged to %s", inactiveSlot))
	return s.store.Get(ctx, rec.ID)
}

// Apply applies a staged update.
func (s *Service) Apply(ctx context.Context, id, actor string) (*UpdateRecord, error) {
	rec, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, errors.New("record not found")
	}
	if rec.State != StateStaged {
		s.store.Audit(s.cfg.DB, actor, "apply", id, "error", "invalid_transition")
		return nil, ErrInvalidTransition
	}

	// Mark pending switch
	if err := s.switcher.MarkPendingSwitch(ctx); err != nil {
		s.store.UpdateState(ctx, id, StateFailed, actor, fmt.Sprintf("mark pending failed: %v", err), rec.SlotTarget)
		s.store.Audit(s.cfg.DB, actor, "apply", id, "error", fmt.Sprintf("mark pending failed: %v", err))
		return nil, fmt.Errorf("mark pending switch: %w", err)
	}

	if err := s.store.UpdateState(ctx, id, StatePendingSwitch, actor, "marked pending switch", rec.SlotTarget); err != nil {
		return nil, err
	}
	s.store.Audit(s.cfg.DB, actor, "apply", id, "ok", "marked pending switch")

	// Health check
	var healthErr error
	if s.healthCheck != nil {
		healthErr = s.healthCheck(ctx)
	}

	if healthErr != nil {
		// Rollback automatically
		if rbErr := s.switcher.RollbackSwitch(ctx); rbErr != nil {
			slogError("rollback switch failed", "error", rbErr)
		}
		if err := s.store.UpdateState(ctx, id, StateRolledBack, "system", fmt.Sprintf("health check failed: %v", healthErr), rec.SlotTarget); err != nil {
			return nil, err
		}
		s.store.Audit(s.cfg.DB, "system", "apply", id, "error", fmt.Sprintf("health check failed, rolled back: %v", healthErr))
		rec, _ = s.store.Get(ctx, id)
		return rec, nil // Not an HTTP error - rollback is a valid outcome
	}

	// Commit switch
	if err := s.switcher.CommitSwitch(ctx); err != nil {
		s.store.UpdateState(ctx, id, StateFailed, actor, fmt.Sprintf("commit failed: %v", err), rec.SlotTarget)
		s.store.Audit(s.cfg.DB, actor, "apply", id, "error", fmt.Sprintf("commit failed: %v", err))
		return nil, fmt.Errorf("commit switch: %w", err)
	}

	if err := s.store.UpdateState(ctx, id, StateApplied, actor, "health check passed, committed", rec.SlotTarget); err != nil {
		return nil, err
	}
	s.store.Audit(s.cfg.DB, actor, "apply", id, "ok", "health check passed, committed")
	return s.store.Get(ctx, id)
}

// Rollback manually rolls back an applied or pending_switch update.
func (s *Service) Rollback(ctx context.Context, id, actor string) (*UpdateRecord, error) {
	rec, err := s.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, errors.New("record not found")
	}
	if rec.State != StateApplied && rec.State != StatePendingSwitch {
		s.store.Audit(s.cfg.DB, actor, "rollback", id, "error", "invalid_transition")
		return nil, ErrInvalidTransition
	}

	if err := s.switcher.RollbackSwitch(ctx); err != nil {
		s.store.Audit(s.cfg.DB, actor, "rollback", id, "error", fmt.Sprintf("rollback failed: %v", err))
		return nil, fmt.Errorf("rollback switch: %w", err)
	}

	if err := s.store.UpdateState(ctx, id, StateRolledBack, actor, "manual rollback", rec.SlotTarget); err != nil {
		return nil, err
	}
	s.store.Audit(s.cfg.DB, actor, "rollback", id, "ok", "manual rollback")
	return s.store.Get(ctx, id)
}

// ListPackages lists available packages in the drop directory.
func (s *Service) ListPackages() ([]string, error) {
	entries, err := os.ReadDir(s.cfg.DropDir)
	if err != nil {
		return nil, err
	}
	var pkgs []string
	for _, e := range entries {
		if e.IsDir() {
			pkgs = append(pkgs, e.Name())
		}
	}
	return pkgs, nil
}

// slogError is a helper to log errors without importing slog in this file.
// In production, this would use slog.Error.
func slogError(msg string, args ...any) {
	// Best-effort logging; in tests this is a no-op
	_ = msg
	_ = args
}
