package httpserver

// KelviOS Fase 1 WS12: power engine HTTP surface (FR-1.6/FR-1.7).
//
// Policy + state only, NO real hardware calls. All routes are behind
// requireAuth like the other /api/* data routes. Reads (GET) never change
// state and never wake a disk; every state-changing call writes an audit
// row (actor = session username).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/fauzone/kelvios/internal/backup"
	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/power"
	"github.com/fauzone/kelvios/internal/storage"
	"github.com/fauzone/kelvios/internal/wakecheck"
)

// syncPower reconciles engine membership with the catalog pools and the
// disk inventory. Best-effort: on catalog/inventory errors it logs and
// keeps the previous membership. It never wakes a disk (membership only).
// Also refreshes scheduled keep-awake windows from catalog.
func (s *Server) syncPower() {
	if s.power == nil {
		return
	}
	pools, err := catalog.ListPools(s.db)
	if err != nil {
		slog.Error("sync power pools failed", "error", err)
		return
	}
	info := make(map[string]power.PoolInfo, len(pools))
	allDisks := make(map[string]bool)
	for _, p := range pools {
		var disks []string
		rows, err := s.db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, p.ID)
		if err != nil {
			slog.Error("sync power pool disks failed", "pool_id", p.ID, "error", err)
			continue
		}
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				continue
			}
			disks = append(disks, d)
			allDisks[d] = true
		}
		rows.Close()
		info[p.ID] = power.PoolInfo{Tier: storage.Tier(p.Tier), Name: p.Name, Disks: disks}
	}
	var inv []string
	if s.disksFunc != nil {
		if disks, err := s.disksFunc(); err == nil {
			for _, d := range disks {
				inv = append(inv, d.Name)
				allDisks[d.Name] = true
			}
		} else {
			slog.Error("sync power inventory failed", "error", err)
		}
	}
	s.power.SyncFromCatalog(info, inv)

	// Refresh schedules for all known disks
	for disk := range allDisks {
		s.refreshPowerSchedules(disk)
	}
}

// powerEntryJSON is the wire form of one per-disk power entry.
// Tier is null when the disk belongs to no pool.
type powerEntryJSON struct {
	Disk           string  `json:"disk"`
	PoolID         string  `json:"pool_id,omitempty"`
	PoolName       string  `json:"pool_name,omitempty"`
	Tier           *string `json:"tier"`
	PowerState     string  `json:"power_state"`
	LastActivity   int64   `json:"last_activity"`
	KeepAwakeUntil int64   `json:"keep_awake_until"`
	WindowMinutes  int     `json:"window_minutes"`
}

func powerEntryJSONFrom(e power.DiskEntry) powerEntryJSON {
	out := powerEntryJSON{
		Disk:           e.Disk,
		PoolID:         e.PoolID,
		PoolName:       e.PoolName,
		PowerState:     string(e.State),
		LastActivity:   e.LastActivity,
		KeepAwakeUntil: e.KeepAwake,
		WindowMinutes:  e.WindowMin,
	}
	if e.Tier != nil {
		t := string(*e.Tier)
		out.Tier = &t
	}
	return out
}

func writePowerJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writePowerError(w http.ResponseWriter, status int, msg string) {
	writePowerJSON(w, status, map[string]string{"error": msg})
}

// handlePowerList handles GET /api/power — list of per-disk entries.
// Pure read: never mutates engine state, never wakes a disk.
func (s *Server) handlePowerList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	s.syncPower()
	entries := s.power.List()
	out := make([]powerEntryJSON, 0, len(entries))
	for _, e := range entries {
		out = append(out, powerEntryJSONFrom(e))
	}
	writePowerJSON(w, http.StatusOK, map[string]any{"disks": out})
}

// handlePowerByDisk routes /api/power/{disk} and /api/power/{disk}/{action}
// and /api/power/{disk}/schedules[/{id}].
func (s *Server) handlePowerByDisk(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/power/")
	if path == "" || strings.HasPrefix(path, "/") {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(path, "/")
	disk := parts[0]
	if disk == "" {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handlePowerGet(w, r, disk)
		return
	}
	// Handle schedules: /api/power/{disk}/schedules[/{id}]
	if parts[1] == "schedules" {
		if len(parts) == 2 {
			// /api/power/{disk}/schedules
			switch r.Method {
			case http.MethodGet:
				s.handlePowerSchedulesList(w, r, disk)
			case http.MethodPost:
				s.handlePowerSchedulesCreate(w, r, disk)
			default:
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			}
			return
		}
		if len(parts) == 3 {
			// /api/power/{disk}/schedules/{id}
			scheduleID := parts[2]
			switch r.Method {
			case http.MethodDelete:
				s.handlePowerSchedulesDelete(w, r, disk, scheduleID)
			case http.MethodPatch:
				s.handlePowerSchedulesPatch(w, r, disk, scheduleID)
			default:
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			}
			return
		}
		http.NotFound(w, r)
		return
	}
	// Handle wake-checklist: /api/power/{disk}/wake-checklist[/run]
	if parts[1] == "wake-checklist" {
		if len(parts) == 2 {
			// /api/power/{disk}/wake-checklist
			if r.Method != http.MethodGet {
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
				return
			}
			s.handleWakeChecklistList(w, r, disk)
			return
		}
		if len(parts) == 3 && parts[2] == "run" {
			// /api/power/{disk}/wake-checklist/run
			if r.Method != http.MethodPost {
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
				return
			}
			s.handleWakeChecklistRun(w, r, disk)
			return
		}
		http.NotFound(w, r)
		return
	}
	// Existing single-action endpoints (wake, sleep, activity, stay-awake, window)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	switch parts[1] {
	case "wake":
		s.handlePowerWake(w, r, disk)
	case "sleep":
		s.handlePowerSleep(w, r, disk)
	case "activity":
		s.handlePowerActivity(w, r, disk)
	case "stay-awake":
		s.handlePowerStayAwake(w, r, disk)
	case "window":
		s.handlePowerWindow(w, r, disk)
	default:
		http.NotFound(w, r)
	}
}

// handlePowerGet handles GET /api/power/{disk} — single entry, pure read.
func (s *Server) handlePowerGet(w http.ResponseWriter, r *http.Request, disk string) {
	s.syncPower()
	e, ok := s.power.Snapshot(disk)
	if !ok {
		writePowerError(w, http.StatusNotFound, "disk not found")
		return
	}
	writePowerJSON(w, http.StatusOK, map[string]any{"disk": powerEntryJSONFrom(e)})
}

// handlePowerWake handles POST /api/power/{disk}/wake — queued staggered
// wake. Unknown disk → 404. Already-active disk is a no-op success.
// WS36 (FR-2.6): After successful wake, best-effort flush dirty shadows for
// pools on the woken disk(s). Never fail the wake response on flush errors.
// WS42-A (FR-2.9): After successful wake of a disk in a Cold pool, start the
// wake piggyback checklist asynchronously.
func (s *Server) handlePowerWake(w http.ResponseWriter, r *http.Request, disk string) {
	s.syncPower()
	e, ok := s.power.Wake(disk)
	if !ok {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.wake", "power", disk, "error", "disk not found")
		writePowerError(w, http.StatusNotFound, "disk not found")
		return
	}
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.wake", "power", disk, "ok", "")

	// WS36: Flush dirty shadows for pools on the woken disk (best-effort)
	// Find pools that contain this disk
	poolRows, err := s.db.Query(`SELECT pool_id FROM pool_disks WHERE disk_name = ?`, disk)
	if err == nil {
		var poolIDs []string
		for poolRows.Next() {
			var poolID string
			if err := poolRows.Scan(&poolID); err == nil {
				poolIDs = append(poolIDs, poolID)
			}
		}
		poolRows.Close()
		for _, poolID := range poolIDs {
			// Best-effort flush: never fail the wake response on flush errors
			flushed, failed, err := catalog.FlushDirtyShadows(s.db, poolID, "system:wake-flush")
			if err != nil {
				slog.Error("wake flush dirty shadows failed", "pool_id", poolID, "error", err)
			} else if flushed > 0 || failed > 0 {
				slog.Info("wake flush dirty shadows done", "pool_id", poolID, "flushed", flushed, "failed", failed)
			}

			// WS42-A: If this is a Cold pool, start the wake checklist asynchronously
			pool, _, err := catalog.GetPool(s.db, poolID)
			if err == nil && pool != nil && pool.Tier == "cold" {
				go s.runWakeChecklistAsync(poolID, disk)
			}
		}
	}

	writePowerJSON(w, http.StatusOK, map[string]any{"disk": powerEntryJSONFrom(e)})
}

// runWakeChecklistAsync runs the wake piggyback checklist asynchronously.
// It creates the checklist run, executes all four steps, and audits the result.
func (s *Server) runWakeChecklistAsync(poolID, disk string) {
	ctx := context.Background()
	deps := wakecheck.Deps{
		DB: s.db,
		PowerSnapshot: func(d string) (storage.PowerState, bool) {
			entry, ok := s.power.Snapshot(d)
			if !ok {
				return storage.PowerUnknown, false
			}
			return entry.State, true
		},
		BackupWrapper: func() *backup.ResticWrapper {
			return backup.NewResticWrapper()
		},
		VaultGetSecret: func(name string) (string, error) {
			return s.vault.Get(name)
		},
		Now: time.Now,
	}

	run := wakecheck.RunChecklist(ctx, deps, poolID, disk)

	// Audit the completed checklist run
	detailParts := make([]string, 0, len(run.Steps))
	for _, step := range run.Steps {
		detailParts = append(detailParts, fmt.Sprintf("%s=%s", step.Step, step.Status))
	}
	detail := strings.Join(detailParts, ", ")
	catalog.WriteAuditLog(s.db, "system", "power.wake.checklist", "power", disk, "ok", detail)
}

// handleWakeChecklistRun handles POST /api/power/{disk}/wake-checklist/run
// Runs the checklist synchronously and returns the run.
func (s *Server) handleWakeChecklistRun(w http.ResponseWriter, r *http.Request, disk string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Verify disk exists
	if !s.power.Exists(disk) {
		writePowerError(w, http.StatusNotFound, "disk not found")
		return
	}

	// Find the pool for this disk (must be Cold)
	poolRows, err := s.db.Query(`SELECT pool_id FROM pool_disks WHERE disk_name = ?`, disk)
	if err != nil {
		slog.Error("wake checklist run: query pool failed", "disk", disk, "error", err)
		writePowerError(w, http.StatusInternalServerError, "query pool failed")
		return
	}
	var poolID string
	found := false
	for poolRows.Next() {
		var pid string
		if err := poolRows.Scan(&pid); err == nil {
			poolID = pid
			found = true
			break
		}
	}
	poolRows.Close()

	if !found {
		writePowerError(w, http.StatusNotFound, "disk not in any pool")
		return
	}

	pool, _, err := catalog.GetPool(s.db, poolID)
	if err != nil || pool == nil {
		writePowerError(w, http.StatusInternalServerError, "get pool failed")
		return
	}
	if pool.Tier != "cold" {
		writePowerError(w, http.StatusConflict, "wake checklist only applies to Cold pools")
		return
	}

	ctx := r.Context()
	deps := wakecheck.Deps{
		DB: s.db,
		PowerSnapshot: func(d string) (storage.PowerState, bool) {
			entry, ok := s.power.Snapshot(d)
			if !ok {
				return storage.PowerUnknown, false
			}
			return entry.State, true
		},
		BackupWrapper: func() *backup.ResticWrapper {
			return backup.NewResticWrapper()
		},
		VaultGetSecret: func(name string) (string, error) {
			return s.vault.Get(name)
		},
		Now: time.Now,
	}

	run := wakecheck.RunChecklist(ctx, deps, poolID, disk)

	// Audit the completed checklist run
	detailParts := make([]string, 0, len(run.Steps))
	for _, step := range run.Steps {
		detailParts = append(detailParts, fmt.Sprintf("%s=%s", step.Step, step.Status))
	}
	detail := strings.Join(detailParts, ", ")
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.wake.checklist", "power", disk, "ok", detail)

	writePowerJSON(w, http.StatusOK, map[string]any{"run": run})
}

// handleWakeChecklistList handles GET /api/power/{disk}/wake-checklist?limit=
// Returns recent checklist runs for the disk, newest first.
func (s *Server) handleWakeChecklistList(w http.ResponseWriter, r *http.Request, disk string) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Verify disk exists
	if !s.power.Exists(disk) {
		writePowerError(w, http.StatusNotFound, "disk not found")
		return
	}

	limit := parseIntQuery(r, "limit", 50)
	if limit > 500 {
		limit = 500
	}

	runs, err := wakecheck.ListRuns(s.db, disk, limit)
	if err != nil {
		slog.Error("wake checklist list failed", "disk", disk, "error", err)
		writePowerError(w, http.StatusInternalServerError, "list runs failed")
		return
	}

	// Convert to response format
	result := make([]map[string]any, 0, len(runs))
	for _, run := range runs {
		steps := make([]map[string]any, 0, len(run.Steps))
		for _, step := range run.Steps {
			steps = append(steps, map[string]any{
				"step":   step.Step,
				"status": step.Status,
				"detail": step.Detail,
			})
		}
		result = append(result, map[string]any{
			"id":           run.ID,
			"pool_id":      run.PoolID,
			"disk":         run.Disk,
			"started_at":   run.StartedAt,
			"finished_at":  run.FinishedAt,
			"steps":        steps,
		})
	}

	writePowerJSON(w, http.StatusOK, map[string]any{"runs": result})
}

// handlePowerSleep handles POST /api/power/{disk}/sleep — explicit sleep.
// Hot → 409, unpooled → 409, unknown → 404. Warm → standby, Cold → off.
func (s *Server) handlePowerSleep(w http.ResponseWriter, r *http.Request, disk string) {
	s.syncPower()
	e, ok, err := s.power.Sleep(disk)
	if !ok {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.sleep", "power", disk, "error", "disk not found")
		writePowerError(w, http.StatusNotFound, "disk not found")
		return
	}
	if err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.sleep", "power", disk, "error", err.Error())
		if err == power.ErrHotNeverSleeps || err == power.ErrUnpooled {
			writePowerError(w, http.StatusConflict, err.Error())
			return
		}
		writePowerError(w, http.StatusInternalServerError, err.Error())
		return
	}
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.sleep", "power", disk, "ok", "")
	writePowerJSON(w, http.StatusOK, map[string]any{"disk": powerEntryJSONFrom(e)})
}

// handlePowerActivity handles POST /api/power/{disk}/activity — records
// activity (resets the idle timer). Unknown disk → 404.
func (s *Server) handlePowerActivity(w http.ResponseWriter, r *http.Request, disk string) {
	s.syncPower()
	e, ok := s.power.MarkActivity(disk)
	if !ok {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.activity", "power", disk, "error", "disk not found")
		writePowerError(w, http.StatusNotFound, "disk not found")
		return
	}
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.activity", "power", disk, "ok", "")
	writePowerJSON(w, http.StatusOK, map[string]any{"disk": powerEntryJSONFrom(e)})
}

// handlePowerStayAwake handles POST /api/power/{disk}/stay-awake with body
// {until: <unix seconds>}. Past until → 400, unknown disk → 404.
func (s *Server) handlePowerStayAwake(w http.ResponseWriter, r *http.Request, disk string) {
	var req struct {
		Until int64 `json:"until"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.stay_awake", "power", disk, "error", "invalid JSON")
		writePowerError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	s.syncPower()
	e, ok, err := s.power.StayAwake(disk, req.Until)
	if !ok {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.stay_awake", "power", disk, "error", "disk not found")
		writePowerError(w, http.StatusNotFound, "disk not found")
		return
	}
	if err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.stay_awake", "power", disk, "error", err.Error())
		if err == power.ErrPastStayAwake {
			writePowerError(w, http.StatusBadRequest, err.Error())
			return
		}
		writePowerError(w, http.StatusInternalServerError, err.Error())
		return
	}
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.stay_awake", "power", disk, "ok", "")
	writePowerJSON(w, http.StatusOK, map[string]any{"disk": powerEntryJSONFrom(e)})
}

// handlePowerWindow handles POST /api/power/{disk}/window with body
// {minutes: <int>} for the owning pool's window. Out of 15–240 → 400,
// unpooled → 409, unknown → 404.
func (s *Server) handlePowerWindow(w http.ResponseWriter, r *http.Request, disk string) {
	var req struct {
		Minutes int `json:"minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.window", "power", disk, "error", "invalid JSON")
		writePowerError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	s.syncPower()
	e, ok, err := s.power.SetWindow(disk, req.Minutes)
	if !ok {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.window", "power", disk, "error", "disk not found")
		writePowerError(w, http.StatusNotFound, "disk not found")
		return
	}
	if err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.window", "power", disk, "error", err.Error())
		switch err {
		case power.ErrWindowOutOfRange:
			writePowerError(w, http.StatusBadRequest, err.Error())
			return
		case power.ErrUnpooled:
			writePowerError(w, http.StatusConflict, err.Error())
			return
		}
		writePowerError(w, http.StatusInternalServerError, err.Error())
		return
	}
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.window", "power", disk, "ok", "")
	writePowerJSON(w, http.StatusOK, map[string]any{"disk": powerEntryJSONFrom(e)})
}

// scheduleJSON is the wire form of a power schedule.
type scheduleJSON struct {
	ID        string `json:"id"`
	Disk      string `json:"disk"`
	Days      string `json:"days"`
	StartMin  int    `json:"start_min"`
	EndMin    int    `json:"end_min"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func scheduleJSONFrom(s *catalog.PowerScheduleRow) scheduleJSON {
	return scheduleJSON{
		ID:        s.ID,
		Disk:      s.Disk,
		Days:      s.Days,
		StartMin:  s.StartMin,
		EndMin:    s.EndMin,
		Enabled:   s.Enabled,
		CreatedAt: s.CreatedAt.Unix(),
		UpdatedAt: s.UpdatedAt.Unix(),
	}
}

// handlePowerSchedulesList handles GET /api/power/{disk}/schedules.
func (s *Server) handlePowerSchedulesList(w http.ResponseWriter, r *http.Request, disk string) {
	s.syncPower()
	// Verify disk exists
	if !s.power.Exists(disk) {
		writePowerError(w, http.StatusNotFound, "disk not found")
		return
	}
	scheds, err := catalog.ListPowerSchedules(s.db, disk)
	if err != nil {
		writePowerError(w, http.StatusInternalServerError, "list schedules failed")
		return
	}
	out := make([]scheduleJSON, 0, len(scheds))
	for _, sc := range scheds {
		out = append(out, scheduleJSONFrom(sc))
	}
	writePowerJSON(w, http.StatusOK, map[string]any{"schedules": out})
}

// handlePowerSchedulesCreate handles POST /api/power/{disk}/schedules with body
// {days, start_min, end_min}. Validation 400, unknown disk 404, duplicate 409.
func (s *Server) handlePowerSchedulesCreate(w http.ResponseWriter, r *http.Request, disk string) {
	var req struct {
		Days     string `json:"days"`
		StartMin int    `json:"start_min"`
		EndMin   int    `json:"end_min"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.schedule.create", "power", disk, "error", "invalid JSON")
		writePowerError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	s.syncPower()
	// Verify disk exists
	if !s.power.Exists(disk) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.schedule.create", "power", disk, "error", "disk not found")
		writePowerError(w, http.StatusNotFound, "disk not found")
		return
	}
	sch, err := catalog.CreatePowerSchedule(s.db, disk, req.Days, req.StartMin, req.EndMin)
	if err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.schedule.create", "power", disk, "error", err.Error())
		if err == catalog.ErrPowerScheduleDuplicate {
			writePowerError(w, http.StatusConflict, "schedule already exists")
			return
		}
		if err == catalog.ErrPowerScheduleInvalid {
			writePowerError(w, http.StatusBadRequest, "invalid schedule: days (Mon-Sun), start_min/end_min (0-1439, start!=end)")
			return
		}
		writePowerError(w, http.StatusInternalServerError, err.Error())
		return
	}
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.schedule.create", "power", disk, "ok", fmt.Sprintf("id=%s days=%s %d-%d", sch.ID, sch.Days, sch.StartMin, sch.EndMin))
	// Refresh engine schedules
	s.refreshPowerSchedules(disk)
	writePowerJSON(w, http.StatusCreated, map[string]any{"schedule": scheduleJSONFrom(sch)})
}

// handlePowerSchedulesDelete handles DELETE /api/power/{disk}/schedules/{id}.
func (s *Server) handlePowerSchedulesDelete(w http.ResponseWriter, r *http.Request, disk, scheduleID string) {
	s.syncPower()
	// Verify disk exists
	if !s.power.Exists(disk) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.schedule.delete", "power", disk, "error", "disk not found")
		writePowerError(w, http.StatusNotFound, "disk not found")
		return
	}
	err := catalog.DeletePowerSchedule(s.db, scheduleID)
	if err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.schedule.delete", "power", disk, "error", err.Error())
		if err == sql.ErrNoRows {
			writePowerError(w, http.StatusNotFound, "schedule not found")
			return
		}
		writePowerError(w, http.StatusInternalServerError, err.Error())
		return
	}
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.schedule.delete", "power", disk, "ok", fmt.Sprintf("id=%s", scheduleID))
	s.refreshPowerSchedules(disk)
	w.WriteHeader(http.StatusNoContent)
}

// handlePowerSchedulesPatch handles PATCH /api/power/{disk}/schedules/{id} with body
// {enabled: <bool>}.
func (s *Server) handlePowerSchedulesPatch(w http.ResponseWriter, r *http.Request, disk, scheduleID string) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.schedule.update", "power", disk, "error", "invalid JSON")
		writePowerError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	s.syncPower()
	// Verify disk exists
	if !s.power.Exists(disk) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.schedule.update", "power", disk, "error", "disk not found")
		writePowerError(w, http.StatusNotFound, "disk not found")
		return
	}
	err := catalog.UpdatePowerScheduleEnabled(s.db, scheduleID, req.Enabled)
	if err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.schedule.update", "power", disk, "error", err.Error())
		if err == sql.ErrNoRows {
			writePowerError(w, http.StatusNotFound, "schedule not found")
			return
		}
		writePowerError(w, http.StatusInternalServerError, err.Error())
		return
	}
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "power.schedule.update", "power", disk, "ok", fmt.Sprintf("id=%s enabled=%v", scheduleID, req.Enabled))
	s.refreshPowerSchedules(disk)
	sch, _ := catalog.GetPowerSchedule(s.db, scheduleID)
	if sch != nil {
		writePowerJSON(w, http.StatusOK, map[string]any{"schedule": scheduleJSONFrom(sch)})
	} else {
		writePowerJSON(w, http.StatusOK, map[string]any{"schedule": nil})
	}
}

// refreshPowerSchedules reloads schedules from catalog into the engine for a disk.
func (s *Server) refreshPowerSchedules(disk string) {
	scheds, err := catalog.ListPowerSchedules(s.db, disk)
	if err != nil {
		slog.Error("refresh power schedules failed", "disk", disk, "error", err)
		return
	}
	engineScheds := make([]power.Schedule, len(scheds))
	for i, sc := range scheds {
		engineScheds[i] = power.Schedule{
			Days:     sc.Days,
			StartMin: sc.StartMin,
			EndMin:   sc.EndMin,
			Enabled:  sc.Enabled,
		}
	}
	s.power.SetSchedules(disk, engineScheds)
}
