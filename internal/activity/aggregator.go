// Package activity implements the Activity & Automation Core API (FR-4.7 backend).
//
// This package provides a unified view of all background jobs in the system
// by aggregating data from existing subsystems (compose, restore_test,
// maintenance, power, agents) without introducing new tables or schedulers.
package activity

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/compose"
	"github.com/fauzone/kelvios/internal/power"
	"github.com/fauzone/kelvios/internal/rules"
	"github.com/fauzone/kelvios/internal/storage"
)

// Status constants for Activity items (per §12.3 + keputusan-1).
const (
	StatusRunning     = "running"
	StatusScheduled   = "scheduled"
	StatusPaused      = "paused"
	StatusFailed      = "failed"
	StatusIdle        = "idle"
	StatusManual      = "manual"
	StatusUnavailable = "unavailable"
)

// Action constants for controls.
const (
	ActionPause   = "pause"
	ActionResume  = "resume"
	ActionCancel  = "cancel"
	ActionRunNow  = "run-now"
)

// Source constants for item sources.
const (
	SourceCompose       = "compose"
	SourceRestoreTest   = "restore_test"
	SourceMaintenance   = "maintenance"
	SourcePower         = "power"
	SourceAgent         = "agent"
	SourceRule          = "rule"
)

// Kind constants for item kinds within sources.
const (
	KindReportSchedule   = "report_schedule"
	KindAutomationRule   = "automation_rule"
	KindScrub            = "scrub"
	KindPowerSchedule    = "power_schedule"
	KindAgent            = "agent"
	KindEventRule        = "event_rule"
)

// Item represents a unified activity item.
type Item struct {
	ID            string     `json:"id"`
	Source        string     `json:"source"`
	Kind          string     `json:"kind"`
	Name          string     `json:"name"`
	Status        string     `json:"status"`
	NextRunAt     *int64     `json:"next_run_at,omitempty"`
	LastRun       *LastRun   `json:"last_run,omitempty"`
	RecentRuns    []*LastRun `json:"recent_runs,omitempty"`
	Controls      []string   `json:"controls"`
	SourceError   string     `json:"source_error,omitempty"`
}

// LastRun represents the result of the last execution.
type LastRun struct {
	Result           string  `json:"result"`            // "ok" | "failed" | ...
	StartedAt        int64   `json:"started_at"`
	DurationSeconds  int64   `json:"duration_seconds"`
	Summary          string  `json:"summary,omitempty"`
	FailureReason    string  `json:"failure_reason,omitempty"`
	RecoveryHint     string  `json:"recovery_hint,omitempty"`
}

// ListResponse is the response for GET /api/activity.
type ListResponse struct {
	Items   []*Item       `json:"items"`
	Summary ListSummary   `json:"summary"`
}

// ListSummary provides a quick overview of the activity list.
type ListSummary struct {
	Total      int      `json:"total"`
	ByStatus   map[string]int `json:"by_status"`
	FailedIDs  []string `json:"failed"`
}

// DetailResponse is the response for GET /api/activity/{id}.
type DetailResponse struct {
	Item *Item `json:"item"`
}

// ActionRequest is the request for POST /api/activity/{id}/{action}.
type ActionRequest struct {
	// No body needed for v1; action is in URL path.
}

// ActionResponse is the response for control actions.
type ActionResponse struct {
	Item *Item `json:"item"`
}

// Aggregator aggregates activity items from all sources.
type Aggregator struct {
	db           *sql.DB
	composeExec  *compose.Executor
	powerEngine  *power.Engine
	disksFunc    func() ([]storage.Disk, error)
	nowFunc      func() time.Time
	rulesEngine  *rules.Engine
	// In-memory tracking for async run-now operations (keputusan-11).
	runningMu sync.Mutex
	running   map[string]bool // itemID -> running
}

// NewAggregator creates a new activity aggregator.
func NewAggregator(db *sql.DB, composeExec *compose.Executor, powerEngine *power.Engine, disksFunc func() ([]storage.Disk, error), nowFunc func() time.Time, rulesEngine *rules.Engine) *Aggregator {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &Aggregator{
		db:          db,
		composeExec: composeExec,
		powerEngine: powerEngine,
		disksFunc:   disksFunc,
		nowFunc:     nowFunc,
		rulesEngine: rulesEngine,
		running:     make(map[string]bool),
	}
}

// List returns all activity items aggregated from all sources.
func (a *Aggregator) List(ctx context.Context, sourceFilter, statusFilter string) (*ListResponse, error) {
	var allItems []*Item
	var errors []string

	// Compose artifacts (report_schedule + automation_rule)
	items, err := a.aggregateCompose(ctx)
	if err != nil {
		errors = append(errors, fmt.Sprintf("compose: %v", err))
		allItems = append(allItems, &Item{
			ID:          SourceCompose + ":error",
			Source:      SourceCompose,
			Kind:        "error",
			Name:        "Compose artifacts",
			Status:      StatusUnavailable,
			SourceError: err.Error(),
			Controls:    []string{},
		})
	} else {
		allItems = append(allItems, items...)
	}

	// Restore tests
	items, err = a.aggregateRestoreTests(ctx)
	if err != nil {
		errors = append(errors, fmt.Sprintf("restore_test: %v", err))
		allItems = append(allItems, &Item{
			ID:          SourceRestoreTest + ":error",
			Source:      SourceRestoreTest,
			Kind:        "error",
			Name:        "Restore tests",
			Status:      StatusUnavailable,
			SourceError: err.Error(),
			Controls:    []string{},
		})
	} else {
		allItems = append(allItems, items...)
	}

	// Maintenance (per collection scrub)
	items, err = a.aggregateMaintenance(ctx)
	if err != nil {
		errors = append(errors, fmt.Sprintf("maintenance: %v", err))
		allItems = append(allItems, &Item{
			ID:          SourceMaintenance + ":error",
			Source:      SourceMaintenance,
			Kind:        "error",
			Name:        "Maintenance",
			Status:      StatusUnavailable,
			SourceError: err.Error(),
			Controls:    []string{},
		})
	} else {
		allItems = append(allItems, items...)
	}

	// Power schedules
	items, err = a.aggregatePower(ctx)
	if err != nil {
		errors = append(errors, fmt.Sprintf("power: %v", err))
		allItems = append(allItems, &Item{
			ID:          SourcePower + ":error",
			Source:      SourcePower,
			Kind:        "error",
			Name:        "Power schedules",
			Status:      StatusUnavailable,
			SourceError: err.Error(),
			Controls:    []string{},
		})
	} else {
		allItems = append(allItems, items...)
	}

	// Agents
	items, err = a.aggregateAgents(ctx)
	if err != nil {
		errors = append(errors, fmt.Sprintf("agents: %v", err))
		allItems = append(allItems, &Item{
			ID:          SourceAgent + ":error",
			Source:      SourceAgent,
			Kind:        "error",
			Name:        "Agents",
			Status:      StatusUnavailable,
			SourceError: err.Error(),
			Controls:    []string{},
		})
	} else {
		allItems = append(allItems, items...)
	}

	// Rules (FR-3.11)
	items, err = a.aggregateRules(ctx)
	if err != nil {
		errors = append(errors, fmt.Sprintf("rules: %v", err))
		allItems = append(allItems, &Item{
			ID:          SourceRule + ":error",
			Source:      SourceRule,
			Kind:        "error",
			Name:        "Event Rules",
			Status:      StatusUnavailable,
			SourceError: err.Error(),
			Controls:    []string{},
		})
	} else {
		allItems = append(allItems, items...)
	}

	// Apply filters
	if sourceFilter != "" {
		filtered := make([]*Item, 0, len(allItems))
		for _, item := range allItems {
			if item.Source == sourceFilter {
				filtered = append(filtered, item)
			}
		}
		allItems = filtered
	}
	if statusFilter != "" {
		filtered := make([]*Item, 0, len(allItems))
		for _, item := range allItems {
			if item.Status == statusFilter {
				filtered = append(filtered, item)
			}
		}
		allItems = filtered
	}

	// Sort: failed first, then running, then by source then name
	sort.Slice(allItems, func(i, j int) bool {
		// Failed first
		if allItems[i].Status == StatusFailed && allItems[j].Status != StatusFailed {
			return true
		}
		if allItems[j].Status == StatusFailed && allItems[i].Status != StatusFailed {
			return false
		}
		// Running second
		if allItems[i].Status == StatusRunning && allItems[j].Status != StatusRunning {
			return true
		}
		if allItems[j].Status == StatusRunning && allItems[i].Status != StatusRunning {
			return false
		}
		// Then by source, then name
		if allItems[i].Source != allItems[j].Source {
			return allItems[i].Source < allItems[j].Source
		}
		return allItems[i].Name < allItems[j].Name
	})

	// Build summary
	byStatus := make(map[string]int)
	failedIDs := make([]string, 0)
	for _, item := range allItems {
		byStatus[item.Status]++
		if item.Status == StatusFailed {
			failedIDs = append(failedIDs, item.ID)
		}
	}

	return &ListResponse{
		Items: allItems,
		Summary: ListSummary{
			Total:     len(allItems),
			ByStatus:  byStatus,
			FailedIDs: failedIDs,
		},
	}, nil
}

// Get returns a single activity item by ID.
func (a *Aggregator) Get(ctx context.Context, id string) (*Item, error) {
	// Parse ID: source:source-id
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid item id format")
	}
	source, sourceID := parts[0], parts[1]

	switch source {
	case SourceCompose:
		return a.getComposeItem(ctx, sourceID)
	case SourceRestoreTest:
		return a.getRestoreTestItem(ctx, sourceID)
	case SourceMaintenance:
		return a.getMaintenanceItem(ctx, sourceID)
	case SourcePower:
		return a.getPowerItem(ctx, sourceID)
	case SourceAgent:
		return a.getAgentItem(ctx, sourceID)
	case SourceRule:
		return a.getRulesItem(ctx, sourceID)
	default:
		return nil, fmt.Errorf("item_not_found")
	}
}

// ExecuteAction executes a control action on an item.
func (a *Aggregator) ExecuteAction(ctx context.Context, id, action, actor string) (*Item, error) {
	// Parse ID
	parts := strings.SplitN(id, ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid item id format")
	}
	source, sourceID := parts[0], parts[1]

	// Get item first to validate it exists and check controls
	item, err := a.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("item_not_found")
	}

	// Check if action is supported
	supported := false
	for _, c := range item.Controls {
		if c == action {
			supported = true
			break
		}
	}
	if !supported {
		// Audit the rejection
		catalog.WriteAuditLog(a.db, actor, "activity."+action, source, sourceID, "error", "control_not_supported")
		return nil, fmt.Errorf("control_not_supported")
	}

	// Check if already running (for run-now)
	if action == ActionRunNow {
		a.runningMu.Lock()
		if a.running[id] {
			a.runningMu.Unlock()
			catalog.WriteAuditLog(a.db, actor, "activity."+action, source, sourceID, "error", "already_running")
			return nil, fmt.Errorf("already_running")
		}
		a.running[id] = true
		a.runningMu.Unlock()

		// For async run-now (restore_test, maintenance), the source action manages
		// the running state and clears it when the background operation completes.
		// For sync run-now (agent), clear immediately after.
		isAsync := (source == SourceRestoreTest || source == SourceMaintenance)
		if !isAsync {
			defer func() {
				a.runningMu.Lock()
				delete(a.running, id)
				a.runningMu.Unlock()
			}()
		}
	}

	// Execute action based on source
	var resultItem *Item
	switch source {
	case SourceCompose:
		resultItem, err = a.executeComposeAction(ctx, sourceID, action, actor)
	case SourceRestoreTest:
		resultItem, err = a.executeRestoreTestAction(ctx, sourceID, action, actor)
	case SourceMaintenance:
		resultItem, err = a.executeMaintenanceAction(ctx, sourceID, action, actor)
	case SourceAgent:
		resultItem, err = a.executeAgentAction(ctx, sourceID, action, actor)
	case SourceRule:
		resultItem, err = a.executeRulesAction(ctx, sourceID, action, actor)
	default:
		err = fmt.Errorf("source_unavailable")
	}

	if err != nil {
		catalog.WriteAuditLog(a.db, actor, "activity."+action, source, sourceID, "error", err.Error())
		return nil, err
	}

	catalog.WriteAuditLog(a.db, actor, "activity."+action, source, sourceID, "ok", fmt.Sprintf("action=%s", action))
	return resultItem, nil
}

// isRunning checks if an item is currently running (for async run-now).
func (a *Aggregator) isRunning(id string) bool {
	a.runningMu.Lock()
	defer a.runningMu.Unlock()
	return a.running[id]
}

// setRunning sets the running state for an item.
func (a *Aggregator) setRunning(id string, running bool) {
	a.runningMu.Lock()
	defer a.runningMu.Unlock()
	if running {
		a.running[id] = true
	} else {
		delete(a.running, id)
	}
}

// truncateSummary truncates a summary to 500 characters (keputusan-6).
func truncateSummary(s string) string {
	const maxLen = 500
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// recoveryHints maps failure reasons to recovery hints (keputusan-6).
var recoveryHints = map[string]string{
	"snapshot_not_found":       "Periksa apakah snapshot masih ada di repo backup; jalankan backup baru bila perlu.",
	"restore_failed":           "Periksa koneksi ke repo backup dan kredensial vault; coba restore manual.",
	"sha256_mismatch":          "Data rusak di snapshot atau sumber; periksa disk sumber dan jalankan scrub.",
	"vault_secret_missing":     "Tambahkan secret vault untuk repo backup (backup-repo:<repo-id>).",
	"vault_error":              "Periksa status vault (terkunci/terbuka) dan coba lagi.",
	"temp_dir_failed":          "Periksa ruang disk dan izin direktori temporary.",
	"hash_compute_failed":      "Periksa akses file dan ruang disk.",
	"transport_not_registered": "Daftarkan transport chat yang diperlukan (Telegram/WhatsApp).",
	"unmarshal_spec":           "Perbaiki spec artefak compose (JSON tidak valid).",
	"get_version":              "Periksa koneksi database dan keberadaan artefak.",
	"build_report":             "Periksa koneksi database dan data untuk laporan.",
	"send_failed":              "Periksa koneksi transport chat dan kredensial.",
	"smart_failed":             "Periksa smartctl binary dan kesehatan disk.",
	"scrub_mismatch":           "File tidak cocok dengan checksum; periksa disk dan jalankan scrub ulang.",
	"source_changed":           "File sumber berubah sejak snapshot; ini normal, bukan kegagalan.",
	"trigger_not_supported":    "Hanya trigger 'schedule' yang didukung di v1.",
	"action_not_whitelisted":   "Hanya action 'send_report' yang didukung di v1.",
	"unknown_kind":             "Jenis artefak tidak dikenal; periksa kind artefak.",
	"driver_unavailable":       "Pastikan Docker daemon berjalan dan driver tersedia.",
	"health_check_failed":      "Container gagal health check; periksa log container.",
	"port_conflict":            "Port sudah digunakan; hentikan aplikasi lain atau ubah port.",
	"resource_exhausted":       "Sumber daya habis (RAM/disk); tambahkan kapasitas atau hentikan aplikasi lain.",
	"container_not_found":      "Container tidak ditemukan; mungkin sudah dihapus.",
	"not_installed":            "Aplikasi belum terpasang; pasang terlebih dahulu.",
	"no_update_available":      "Tidak ada update tersedia dari feed.",
	"no_previous_image":        "Tidak ada image sebelumnya untuk rollback.",
	"rollback_failed":          "Rollback gagal; periksa driver dan coba manual.",
	"rollback_health_failed":   "Health check gagal setelah rollback; periksa container.",
	"invalid_transition":       "Transisi status tidak valid untuk status saat ini.",
	"already_running":          "Item sudah sedang dijalankan; tunggu selesai atau batalkan.",
	"control_not_supported":    "Kontrol ini tidak didukung untuk item ini.",
	"item_not_found":           "Item tidak ditemukan.",
	"source_unavailable":       "Sumber data tidak tersedia saat ini.",
}

// getRecoveryHint returns a recovery hint for a failure reason.
func getRecoveryHint(reason string) string {
	// Try exact match first
	if hint, ok := recoveryHints[reason]; ok {
		return hint
	}
	// Try prefix match
	for k, v := range recoveryHints {
		if strings.HasPrefix(reason, k) {
			return v
		}
	}
	// Generic fallback (keputusan-6)
	return "Periksa detail run dan jalankan ulang; bila gagal berulang, catat sebagai insiden."
}
