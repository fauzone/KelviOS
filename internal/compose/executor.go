package compose

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fauzone/kelvios/internal/chatbridge"

	_ "modernc.org/sqlite"
)

// Executor handles compose artifact execution for report_schedule kind.
type Executor struct {
	db       *sql.DB
	chat     *chatbridge.Service
	nowFunc  func() time.Time
	mu       sync.Mutex
	started  bool
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewExecutor creates a new executor for report_schedule artifacts.
func NewExecutor(db *sql.DB, chat *chatbridge.Service, nowFunc func() time.Time) *Executor {
	if nowFunc == nil {
		nowFunc = time.Now
	}
	return &Executor{
		db:      db,
		chat:    chat,
		nowFunc: nowFunc,
		stopCh:  make(chan struct{}),
	}
}

// Start begins the executor ticker (idempotent, mutex-protected).
func (e *Executor) Start(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started {
		return
	}
	e.started = true
	e.stopCh = make(chan struct{})
	e.wg.Add(1)
	go e.run(ctx)
}

// Stop stops the executor ticker (idempotent).
func (e *Executor) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return
	}
	close(e.stopCh)
	e.started = false
	e.wg.Wait()
}

func (e *Executor) run(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	// Run once immediately on start
	e.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

func (e *Executor) tick(ctx context.Context) {
	now := e.nowFunc().Unix()

	// Find all active report_schedule AND automation_rule artifacts
	rows, err := e.db.Query(`
		SELECT id, spec, kind FROM compose_artifacts
		WHERE (kind = ? OR kind = ?) AND status = ?
	`, KindReportSchedule, KindAutomationRule, StatusActive)
	if err != nil {
		slog.Error("compose executor: query active artifacts failed", "error", err)
		return
	}
	// Kumpulkan dulu, tutup rows, BARU eksekusi: executeIfDue mengeluarkan
	// query lagi pada DB yang sama — dengan satu koneksi (SetMaxOpenConns(1))
	// menahan rows sambil query = deadlock (kelas WS26).
	type dueArtifact struct {
		id   string
		spec []byte
		kind string
	}
	var due []dueArtifact
	for rows.Next() {
		var id string
		var specJSON []byte
		var kind string
		if err := rows.Scan(&id, &specJSON, &kind); err != nil {
			slog.Error("compose executor: scan artifact failed", "error", err)
			continue
		}
		due = append(due, dueArtifact{id: id, spec: specJSON, kind: kind})
	}
	rows.Close()

	for _, a := range due {
		// Panik satu artifact tidak boleh membunuh ticker (kontrak WS52-A).
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("compose executor: panic executing artifact", "artifact_id", a.id, "panic", r)
				}
			}()
			e.executeIfDue(ctx, a.id, a.spec, a.kind, now)
		}()
	}
}

func (e *Executor) executeIfDue(ctx context.Context, artifactID string, specJSON []byte, kind string, now int64) {
	switch kind {
	case KindReportSchedule:
		e.executeReportSchedule(ctx, artifactID, specJSON, now)
	case KindAutomationRule:
		e.executeAutomationRule(ctx, artifactID, specJSON, now)
	default:
		slog.Error("compose executor: unknown kind", "artifact_id", artifactID, "kind", kind)
		e.recordRun(artifactID, 0, RunResultFailed, "unknown_kind")
	}
}

func (e *Executor) executeReportSchedule(ctx context.Context, artifactID string, specJSON []byte, now int64) {
	var spec struct {
		Transport     string `json:"transport"`
		ChatID        string `json:"chat_id"`
		IntervalHours int    `json:"interval_hours"`
	}
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		slog.Error("compose executor: unmarshal spec failed", "artifact_id", artifactID, "error", err)
		e.recordRun(artifactID, 0, RunResultFailed, "unmarshal spec: "+err.Error())
		return
	}

	// Check if due: no finished run OR lastFinished + interval <= now
	lastFinished, hasRun := LastFinishedRun(e.db, artifactID)
	intervalSec := int64(spec.IntervalHours) * 3600
	if hasRun && lastFinished+intervalSec > now {
		return // Not due yet
	}

	// Get current version
	var version int
	err := e.db.QueryRow(`SELECT current_version FROM compose_artifacts WHERE id = ?`, artifactID).Scan(&version)
	if err != nil {
		slog.Error("compose executor: get version failed", "artifact_id", artifactID, "error", err)
		e.recordRun(artifactID, 0, RunResultFailed, "get version: "+err.Error())
		return
	}

	// Build report via chatbridge
	reportText, err := chatbridge.BuildReport(e.db, e.nowFunc())
	if err != nil {
		slog.Error("compose executor: build report failed", "artifact_id", artifactID, "error", err)
		e.recordRun(artifactID, version, RunResultFailed, "build report: "+err.Error())
		return
	}

	// Get transport
	transport, ok := e.chat.GetTransport(spec.Transport)
	if !ok || transport == nil {
		slog.Error("compose executor: transport not registered", "artifact_id", artifactID, "transport", spec.Transport)
		e.recordRun(artifactID, version, RunResultFailed, "transport_not_registered")
		return
	}

	// Send report
	err = transport.Send(spec.ChatID, reportText)

	if err != nil {
		slog.Error("compose executor: send failed", "artifact_id", artifactID, "error", err)
		e.recordRun(artifactID, version, RunResultFailed, "send: "+err.Error())
		return
	}

	// Record successful run
	e.recordRun(artifactID, version, RunResultOK, "sent")

	// Audit log
	catalogWriteAuditLog(e.db, "compose:"+artifactID, "compose.execute", "compose", artifactID, "ok", fmt.Sprintf("transport=%s", spec.Transport))
}

func (e *Executor) executeAutomationRule(ctx context.Context, artifactID string, specJSON []byte, now int64) {
	var spec struct {
		Trigger struct {
			Type           string `json:"type"`
			IntervalHours  int    `json:"interval_hours"`
		} `json:"trigger"`
		Action struct {
			Type      string `json:"type"`
			Transport string `json:"transport"`
			ChatID    string `json:"chat_id"`
		} `json:"action"`
	}
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		slog.Error("compose executor: unmarshal automation_rule spec failed", "artifact_id", artifactID, "error", err)
		e.recordRun(artifactID, 0, RunResultFailed, "unmarshal spec: "+err.Error())
		return
	}

	// Whitelist v1: only schedule trigger + send_report action
	if spec.Trigger.Type != "schedule" {
		slog.Error("compose executor: unsupported trigger type", "artifact_id", artifactID, "trigger_type", spec.Trigger.Type)
		e.recordRun(artifactID, 0, RunResultFailed, "trigger_not_supported")
		return
	}
	if spec.Action.Type != "send_report" {
		slog.Error("compose executor: unsupported action type", "artifact_id", artifactID, "action_type", spec.Action.Type)
		e.recordRun(artifactID, 0, RunResultFailed, "action_not_whitelisted")
		return
	}

	// Check if due: no finished run OR lastFinished + interval <= now
	lastFinished, hasRun := LastFinishedRun(e.db, artifactID)
	intervalSec := int64(spec.Trigger.IntervalHours) * 3600
	if hasRun && lastFinished+intervalSec > now {
		return // Not due yet
	}

	// Get current version
	var version int
	err := e.db.QueryRow(`SELECT current_version FROM compose_artifacts WHERE id = ?`, artifactID).Scan(&version)
	if err != nil {
		slog.Error("compose executor: get version failed", "artifact_id", artifactID, "error", err)
		e.recordRun(artifactID, 0, RunResultFailed, "get version: "+err.Error())
		return
	}

	// Build report via chatbridge
	reportText, err := chatbridge.BuildReport(e.db, e.nowFunc())
	if err != nil {
		slog.Error("compose executor: build report failed", "artifact_id", artifactID, "error", err)
		e.recordRun(artifactID, version, RunResultFailed, "build report: "+err.Error())
		return
	}

	// Get transport
	transport, ok := e.chat.GetTransport(spec.Action.Transport)
	if !ok || transport == nil {
		slog.Error("compose executor: transport not registered", "artifact_id", artifactID, "transport", spec.Action.Transport)
		e.recordRun(artifactID, version, RunResultFailed, "transport_not_registered")
		return
	}

	// Send report
	err = transport.Send(spec.Action.ChatID, reportText)

	if err != nil {
		slog.Error("compose executor: send failed", "artifact_id", artifactID, "error", err)
		e.recordRun(artifactID, version, RunResultFailed, "send: "+err.Error())
		return
	}

	// Record successful run
	e.recordRun(artifactID, version, RunResultOK, "sent")

	// Audit log
	catalogWriteAuditLog(e.db, "compose:"+artifactID, "compose.execute", "compose", artifactID, "ok", fmt.Sprintf("transport=%s", spec.Action.Transport))
}

func (e *Executor) recordRun(artifactID string, version int, result, detail string) {
	run := &Run{
		ArtifactID: artifactID,
		Version:    version,
		StartedAt:  e.nowFunc().Unix(),
		FinishedAt: e.nowFunc().Unix(),
		Result:     result,
		Detail:     detail,
	}
	if err := RecordRun(e.db, run); err != nil {
		slog.Error("compose executor: record run failed", "artifact_id", artifactID, "error", err)
	}
}

// catalogWriteAuditLog is a local copy to avoid import cycle.
// In practice, this would call catalog.WriteAuditLog.
func catalogWriteAuditLog(db *sql.DB, actor, action, targetType, targetID, result, detail string) {
	_, _ = db.Exec(`
		INSERT INTO audit_log (id, created_at, actor, action, target_type, target_id, result, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, generateAuditID(), time.Now().Unix(), actor, action, targetType, targetID, result, detail)
}

func generateAuditID() string {
	now := time.Now().UnixNano()
	return fmt.Sprintf("audit-%d", now)
}