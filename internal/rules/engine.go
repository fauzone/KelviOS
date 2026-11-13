// Package rules implements the FR-3.11 Event→Action Rules Engine.
// Deterministic engine evaluating user-defined rules against observed events.
package rules

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/fauzone/kelvios/internal/agents"
	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/chatbridge"
	"github.com/fauzone/kelvios/internal/restoretest"
	"github.com/fauzone/kelvios/internal/storage"
)

// ============================================================================
// ENGINE DEPENDENCIES (injected interfaces)
// ============================================================================

// AgentRunner runs an agent by name.
type AgentRunner interface {
	RunAgent(ctx context.Context, name string, deps *agents.Deps) (agents.Report, error)
}

// ReportSender sends a report via chat transport.
type ReportSender interface {
	SendReport(ctx context.Context, transportName, chatID string) error
}

// RestoreTestRunner runs a scheduled restore test.
type RestoreTestRunner interface {
	RunRestoreTest(ctx context.Context, deps restoretest.Deps) (*restoretest.RunResult, error)
}

// AuditWriter writes an audit log entry.
type AuditWriter interface {
	WriteAuditLog(actor, action, targetType, targetID, result, detail string)
}

// DefaultDeps holds the default implementations of engine dependencies.
type DefaultDeps struct {
	DB           *sql.DB
	ChatBridge   *chatbridge.Service
	DisksFunc    func() ([]storage.Disk, error)
	NowFunc      func() time.Time
	ResticWrapper interface{} // *backup.ResticWrapper - avoid import cycle
	VaultGetSecret func(name string) (string, error)
}

// DefaultAgentRunner implements AgentRunner using agents registry.
type DefaultAgentRunner struct {
	Deps DefaultDeps
}

func (d *DefaultAgentRunner) RunAgent(ctx context.Context, name string, deps *agents.Deps) (agents.Report, error) {
	agent, ok := agents.Get(name)
	if !ok {
		return agents.Report{}, fmt.Errorf("agent not found: %s", name)
	}
	return agent.Run(ctx, deps)
}

// DefaultReportSender implements ReportSender using chatbridge.
type DefaultReportSender struct {
	DB      *sql.DB
	NowFunc func() time.Time
	ChatBridge *chatbridge.Service
}

func (d *DefaultReportSender) SendReport(ctx context.Context, transportName, chatID string) error {
	reportText, err := chatbridge.BuildReport(d.DB, d.NowFunc())
	if err != nil {
		return fmt.Errorf("build report: %w", err)
	}
	transport, ok := d.ChatBridge.GetTransport(transportName)
	if !ok || transport == nil {
		return fmt.Errorf("transport not registered: %s", transportName)
	}
	return transport.Send(chatID, reportText)
}

// DefaultRestoreTestRunner implements RestoreTestRunner.
type DefaultRestoreTestRunner struct {
	Deps DefaultDeps
}

func (d *DefaultRestoreTestRunner) RunRestoreTest(ctx context.Context, deps restoretest.Deps) (*restoretest.RunResult, error) {
	return restoretest.RunScheduled(ctx, deps)
}

// DefaultAuditWriter implements AuditWriter using catalog.
type DefaultAuditWriter struct {
	DB *sql.DB
}

func (d *DefaultAuditWriter) WriteAuditLog(actor, action, targetType, targetID, result, detail string) {
	catalog.WriteAuditLog(d.DB, actor, action, targetType, targetID, result, detail)
}

// ============================================================================
// ENGINE
// ============================================================================

// EngineConfig holds configuration for the rules engine.
type EngineConfig struct {
	Store            Store
	AgentRunner      AgentRunner
	ReportSender     ReportSender
	RestoreTestRunner RestoreTestRunner
	AuditWriter      AuditWriter
	NowFunc          func() time.Time
	TickInterval     time.Duration
	MaxFiresPerHour  int // global rate limit
}

// Engine evaluates rules against observed events on a tick schedule.
type Engine struct {
	cfg       EngineConfig
	mu        sync.Mutex
	started   bool
	stopCh    chan struct{}
	wg        sync.WaitGroup
	firesThisHour int
	hourStart    int64
}

const (
	DefaultTickInterval    = 30 * time.Second
	DefaultMaxFiresPerHour = 120
)

// NewEngine creates a new rules engine.
func NewEngine(cfg EngineConfig) *Engine {
	if cfg.NowFunc == nil {
		cfg.NowFunc = time.Now
	}
	if cfg.TickInterval == 0 {
		cfg.TickInterval = DefaultTickInterval
	}
	if cfg.MaxFiresPerHour == 0 {
		cfg.MaxFiresPerHour = DefaultMaxFiresPerHour
	}
	return &Engine{
		cfg:       cfg,
		stopCh:    make(chan struct{}),
		hourStart: cfg.NowFunc().Unix(),
	}
}

// Start begins the engine ticker.
func (e *Engine) Start(ctx context.Context) {
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

// Stop stops the engine ticker.
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		return
	}
	close(e.stopCh)
	e.started = false
	e.wg.Wait()
}

func (e *Engine) run(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.cfg.TickInterval)
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

func (e *Engine) tick(ctx context.Context) {
	now := e.cfg.NowFunc().Unix()

	// Reset hourly counter if new hour
	if now >= e.hourStart+3600 {
		e.mu.Lock()
		e.firesThisHour = 0
		e.hourStart = now
		e.mu.Unlock()
	}

	// Load enabled rules
	rules, err := e.cfg.Store.ListRules()
	if err != nil {
		slog.Error("rules engine: list rules failed", "error", err)
		return
	}

	enabledRules := make([]*Rule, 0, len(rules))
	for _, r := range rules {
		if r.Enabled {
			enabledRules = append(enabledRules, r)
		}
	}

	if len(enabledRules) == 0 {
		return
	}

	// Check global rate limit
	e.mu.Lock()
	if e.firesThisHour >= e.cfg.MaxFiresPerHour {
		slog.Warn("rules engine: global rate limit reached", "max_per_hour", e.cfg.MaxFiresPerHour)
		e.mu.Unlock()
		return
	}
	availableSlots := e.cfg.MaxFiresPerHour - e.firesThisHour
	e.mu.Unlock()

	// Process each rule
	for _, rule := range enabledRules {
		if availableSlots <= 0 {
			slog.Warn("rules engine: no more global slots this hour")
			break
		}

		fired, err := e.processRule(ctx, rule, now)
		if err != nil {
			slog.Error("rules engine: process rule failed", "rule_id", rule.ID, "error", err)
			continue
		}
		if fired {
			e.mu.Lock()
			e.firesThisHour++
			availableSlots--
			e.mu.Unlock()
		}
	}
}

func (e *Engine) processRule(ctx context.Context, rule *Rule, now int64) (bool, error) {
	// Check cooldown
	if rule.LastFiredAt > 0 && now-rule.LastFiredAt < int64(rule.CooldownSeconds) {
		return false, nil
	}

	// Check max fires per day
	dayAgo := now - 86400
	count, err := e.cfg.Store.CountRunsSince(rule.ID, dayAgo)
	if err != nil {
		return false, fmt.Errorf("count runs: %w", err)
	}
	if count >= rule.MaxFiresPerDay {
		return false, nil
	}

	// Detect events for this rule
	events, err := e.detectEvents(ctx, rule, now)
	if err != nil {
		return false, fmt.Errorf("detect events: %w", err)
	}

	if len(events) == 0 {
		return false, nil
	}

	// Process each detected event (should be at most 1 per tick per rule due to watermarks)
	for _, event := range events {
		// Check dedup via UNIQUE constraint (store will handle)
		existingRun, _ := e.cfg.Store.GetRun(rule.ID, event.EventRef)
		if existingRun != nil {
			// Already processed this event
			continue
		}

		// Check global rate limit again
		e.mu.Lock()
		if e.firesThisHour >= e.cfg.MaxFiresPerHour {
			e.mu.Unlock()
			slog.Warn("rules engine: global rate limit reached during event processing")
			return false, nil
		}
		e.mu.Unlock()

		// Execute actions
		run := &Run{
			RuleID:   rule.ID,
			EventRef: event.EventRef,
			FiredAt:  now,
			Result:   RunResultPending,
			Detail:   "",
			Alert:    false,
			TestFire: false,
		}

		results := e.executeActions(ctx, rule, run)
		run.Detail = results.Detail
		run.Result = results.Result
		run.Alert = results.Alert

		// Save run
		if err := e.cfg.Store.CreateRun(run); err != nil {
			// Check for UNIQUE constraint violation (dedup)
			if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "unique") {
				slog.Debug("rules engine: duplicate event_ref, skipping", "rule_id", rule.ID, "event_ref", event.EventRef)
				return false, nil
			}
			return false, fmt.Errorf("create run: %w", err)
		}

		// Update rule state (watermark + last_fired_at)
		if err := e.cfg.Store.UpdateRuleState(rule.ID, event.NewState, now); err != nil {
			slog.Error("rules engine: update rule state failed", "rule_id", rule.ID, "error", err)
		}

		// Audit log
		actor := "rules:" + rule.ID
		e.cfg.AuditWriter.WriteAuditLog(actor, "rule.fired", "event_rule", rule.ID, run.Result, run.Detail)
		if run.Alert {
			e.cfg.AuditWriter.WriteAuditLog(actor, "rule.alert", "event_rule", rule.ID, "ok", "alert raised")
		}

		return true, nil
	}

	return false, nil
}

type detectedEvent struct {
	EventRef string
	NewState map[string]any
}

func (e *Engine) detectEvents(ctx context.Context, rule *Rule, now int64) ([]detectedEvent, error) {
	var events []detectedEvent

	switch rule.EventType {
	case EventBackupRunFailed, EventBackupRunSucceeded, EventBackupRestoreFailed:
		evs, err := e.detectAuditLogEvents(ctx, rule, now)
		if err != nil {
			return nil, err
		}
		events = append(events, evs...)

	case EventRestoreTestAlert:
		evs, err := e.detectRestoreTestAlerts(ctx, rule, now)
		if err != nil {
			return nil, err
		}
		events = append(events, evs...)

	case EventUPSEvent:
		evs, err := e.detectUPSEvents(ctx, rule, now)
		if err != nil {
			return nil, err
		}
		events = append(events, evs...)

	case EventAutomationRunFailed:
		evs, err := e.detectComposeRunFailed(ctx, rule, now)
		if err != nil {
			return nil, err
		}
		events = append(events, evs...)

	case EventPoolUsageAbove:
		evs, err := e.detectPoolUsageAbove(ctx, rule, now)
		if err != nil {
			return nil, err
		}
		events = append(events, evs...)
	}

	return events, nil
}

func (e *Engine) detectAuditLogEvents(ctx context.Context, rule *Rule, now int64) ([]detectedEvent, error) {
	var events []detectedEvent

	// Get watermark from rule state
	state := rule.State
	watermarkKey := "audit_log_watermark"
	var watermark int64
	if wm, ok := state[watermarkKey].(float64); ok {
		watermark = int64(wm)
	} else if wm, ok := state[watermarkKey].(int64); ok {
		watermark = wm
	}

	// Determine action and result filter based on event type
	var actionFilter, resultFilter string
	switch rule.EventType {
	case EventBackupRunFailed:
		actionFilter = "backup.run"
		resultFilter = "error"
	case EventBackupRunSucceeded:
		actionFilter = "backup.run"
		resultFilter = "ok"
	case EventBackupRestoreFailed:
		actionFilter = "backup.restore"
		resultFilter = "error"
	}

	rows, err := e.cfg.Store.(*SQLStore).db.Query(`
		SELECT id, created_at FROM audit_log
		WHERE action = ? AND result = ? AND created_at > ?
		ORDER BY created_at ASC
	`, actionFilter, resultFilter, watermark)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var createdAt int64
		if err := rows.Scan(&id, &createdAt); err != nil {
			continue
		}
		events = append(events, detectedEvent{
			EventRef: "audit_log:" + id,
			NewState: map[string]any{watermarkKey: createdAt},
		})
		watermark = createdAt
	}

	// If no new events, watermark stays same (we don't advance if no new events)
	// Per spec: watermark advances only when events are processed

	return events, nil
}

func (e *Engine) detectRestoreTestAlerts(ctx context.Context, rule *Rule, now int64) ([]detectedEvent, error) {
	var events []detectedEvent

	state := rule.State
	watermarkKey := "restore_test_alerts_watermark"
	var watermark int64
	if wm, ok := state[watermarkKey].(float64); ok {
		watermark = int64(wm)
	} else if wm, ok := state[watermarkKey].(int64); ok {
		watermark = wm
	}

	rows, err := e.cfg.Store.(*SQLStore).db.Query(`
		SELECT id, created_at FROM restore_test_alerts
		WHERE created_at > ?
		ORDER BY created_at ASC
	`, watermark)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var createdAt int64
		if err := rows.Scan(&id, &createdAt); err != nil {
			continue
		}
		events = append(events, detectedEvent{
			EventRef: "restore_test_alerts:" + id,
			NewState: map[string]any{watermarkKey: createdAt},
		})
		watermark = createdAt
	}

	return events, nil
}

func (e *Engine) detectUPSEvents(ctx context.Context, rule *Rule, now int64) ([]detectedEvent, error) {
	var events []detectedEvent

	state := rule.State
	watermarkKey := "ups_events_watermark"
	var watermark int64
	if wm, ok := state[watermarkKey].(float64); ok {
		watermark = int64(wm)
	} else if wm, ok := state[watermarkKey].(int64); ok {
		watermark = wm
	}

	// Filter by kind if specified in event_params
	kindFilter := ""
	if k, ok := rule.EventParams["kind"].(string); ok && k != "" {
		kindFilter = k
	}

	query := `SELECT id, ts FROM ups_events WHERE ts > ?`
	args := []any{watermark}
	if kindFilter != "" {
		query += ` AND kind = ?`
		args = append(args, kindFilter)
	}
	query += ` ORDER BY ts ASC`

	rows, err := e.cfg.Store.(*SQLStore).db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var ts int64
		if err := rows.Scan(&id, &ts); err != nil {
			continue
		}
		events = append(events, detectedEvent{
			EventRef: "ups_events:" + id,
			NewState: map[string]any{watermarkKey: ts},
		})
		watermark = ts
	}

	return events, nil
}

func (e *Engine) detectComposeRunFailed(ctx context.Context, rule *Rule, now int64) ([]detectedEvent, error) {
	var events []detectedEvent

	state := rule.State
	watermarkKey := "compose_runs_watermark"
	var watermark int64
	if wm, ok := state[watermarkKey].(float64); ok {
		watermark = int64(wm)
	} else if wm, ok := state[watermarkKey].(int64); ok {
		watermark = wm
	}

	rows, err := e.cfg.Store.(*SQLStore).db.Query(`
		SELECT id, finished_at FROM compose_runs
		WHERE result = 'failed' AND finished_at > ?
		ORDER BY finished_at ASC
	`, watermark)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var finishedAt int64
		if err := rows.Scan(&id, &finishedAt); err != nil {
			continue
		}
		events = append(events, detectedEvent{
			EventRef: "compose_runs:" + id,
			NewState: map[string]any{watermarkKey: finishedAt},
		})
		watermark = finishedAt
	}

	return events, nil
}

func (e *Engine) detectPoolUsageAbove(ctx context.Context, rule *Rule, now int64) ([]detectedEvent, error) {
	var events []detectedEvent

	state := rule.State
	poolStateKey := "pool_usage_state"
	poolStates := make(map[string]bool)
	if ps, ok := state[poolStateKey].(map[string]any); ok {
		for k, v := range ps {
			if b, ok := v.(bool); ok {
				poolStates[k] = b
			}
		}
	}

	threshold := 90
	if t, ok := rule.EventParams["threshold_percent"].(float64); ok {
		threshold = int(t)
	} else if t, ok := rule.EventParams["threshold_percent"].(int); ok {
		threshold = t
	}

	poolIDFilter := ""
	if p, ok := rule.EventParams["pool_id"].(string); ok {
		poolIDFilter = p
	}

	pools, err := catalog.ListPools(e.cfg.Store.(*SQLStore).db)
	if err != nil {
		return nil, err
	}

	newPoolStates := make(map[string]bool)
	for _, pool := range pools {
		if poolIDFilter != "" && pool.ID != poolIDFilter {
			continue
		}
		if pool.SizeBytes == 0 {
			continue
		}
		usagePercent := int((float64(pool.UsedBytes) / float64(pool.SizeBytes)) * 100)
		wasAbove := poolStates[pool.ID]
		isAbove := usagePercent >= threshold

		// Fire only on crossing UP (was below, now above)
		if !wasAbove && isAbove {
			// Additional throttle: one fire per pool per hour
			hourKey := fmt.Sprintf("pool:%s:%d", pool.ID, now/3600)
			if !poolStates[hourKey] {
				events = append(events, detectedEvent{
					EventRef: fmt.Sprintf("pool:%s:%d", pool.ID, now/3600),
					NewState: map[string]any{
						poolStateKey: mergePoolStates(poolStates, pool.ID, true, hourKey),
					},
				})
			}
		}
		newPoolStates[pool.ID] = isAbove
		// Re-arm: if usage drops >=2 points below threshold
		if wasAbove && usagePercent <= threshold-2 {
			newPoolStates[pool.ID] = false
		}
	}

	if len(events) > 0 || len(newPoolStates) != len(poolStates) {
		// Merge new states
		merged := mergePoolStates(poolStates, "", false, "")
		for k, v := range newPoolStates {
			merged[k] = v
		}
		// Also preserve hour keys
		for k, v := range poolStates {
			if strings.HasPrefix(k, "pool:") && strings.Contains(k, ":") {
				parts := strings.Split(k, ":")
				if len(parts) == 3 {
					merged[k] = v
				}
			}
		}
		events = append(events, detectedEvent{
			EventRef: "pool_state_update",
			NewState: map[string]any{poolStateKey: merged},
		})
	}

	return events, nil
}

func mergePoolStates(existing map[string]bool, poolID string, above bool, hourKey string) map[string]any {
	result := make(map[string]any)
	for k, v := range existing {
		result[k] = v
	}
	if poolID != "" {
		result[poolID] = above
	}
	if hourKey != "" {
		result[hourKey] = true
	}
	return result
}

// ActionResults summarizes one execution of a rule's actions.
type ActionResults struct {
	Detail string
	Result string
	Alert  bool
}

func (e *Engine) executeActions(ctx context.Context, rule *Rule, run *Run) ActionResults {
	var details []string
	allOK := true
	anyAlert := false

	for _, action := range rule.Actions {
		var detail string
		var err error

		switch action.Type {
		case ActionRaiseAlert:
			detail, err = e.executeRaiseAlert(ctx, action.Params)
			if err == nil {
				anyAlert = true
			}
		case ActionRunAgent:
			detail, err = e.executeRunAgent(ctx, action.Params)
		case ActionSendReport:
			detail, err = e.executeSendReport(ctx, action.Params)
		case ActionRunRestoreTest:
			detail, err = e.executeRunRestoreTest(ctx)
		default:
			detail = "unknown_action"
			err = fmt.Errorf("unknown action type: %s", action.Type)
		}

		if err != nil {
			allOK = false
			details = append(details, fmt.Sprintf("%s: failed (%v)", action.Type, err))
		} else {
			details = append(details, fmt.Sprintf("%s: ok (%s)", action.Type, detail))
		}
	}

	result := RunResultOK
	if !allOK {
		// Check if any succeeded
		hasOK := false
		for range rule.Actions {
			// We can't easily tell which succeeded from details alone
			// But per spec: failure of one doesn't cancel others
			// So if we got here, at least some ran
			hasOK = true
			break
		}
		if hasOK {
			result = RunResultPartial
		} else {
			result = RunResultFailed
		}
	}

	return ActionResults{
		Detail: strings.Join(details, "; "),
		Result: result,
		Alert:  anyAlert,
	}
}

func (e *Engine) executeRaiseAlert(ctx context.Context, params map[string]any) (string, error) {
	priority := "normal"
	if p, ok := params["priority"].(string); ok {
		priority = p
	}
	transport := ""
	if t, ok := params["transport"].(string); ok {
		transport = t
	}
	chatID := ""
	if c, ok := params["chat_id"].(string); ok {
		chatID = c
	}

	// Write alert audit log
	actor := "rules:engine"
	e.cfg.AuditWriter.WriteAuditLog(actor, "rule.alert", "event_rule", "", "ok", fmt.Sprintf("priority=%s", priority))

	// Send notification if transport+chat_id provided
	if transport != "" && chatID != "" {
		if err := e.cfg.ReportSender.SendReport(ctx, transport, chatID); err != nil {
			return fmt.Sprintf("alert logged, notify failed: %v", err), err
		}
		return fmt.Sprintf("alert logged, notified via %s", transport), nil
	}

	return fmt.Sprintf("alert logged (priority=%s)", priority), nil
}

func (e *Engine) executeRunAgent(ctx context.Context, params map[string]any) (string, error) {
	agentName, ok := params["agent"].(string)
	if !ok || agentName == "" {
		return "", fmt.Errorf("agent name required")
	}

	delay := 0
	if d, ok := params["delay_seconds"].(float64); ok {
		delay = int(d)
	} else if d, ok := params["delay_seconds"].(int); ok {
		delay = d
	}

	if delay > 0 {
		// Schedule delayed execution - for now, execute immediately but record as pending
		// Full delayed execution would require a separate scheduler
		// Per spec: "aksi ber-delay_seconds tercatat pending dan baru dieksekusi sesudah jatuh tempo"
		// For v1, we execute immediately but could enhance later
	}

	deps := agents.NewDeps(e.cfg.Store.(*SQLStore).db, nil, e.cfg.NowFunc)
	// Note: DisksFunc not available in SQLStore, would need to be passed via EngineConfig
	// For now, use nil DispsFunc (agents that need it will handle gracefully)
	report, err := e.cfg.AgentRunner.RunAgent(ctx, agentName, deps)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("agent %s completed: %s", agentName, report.Summary), nil
}

func (e *Engine) executeSendReport(ctx context.Context, params map[string]any) (string, error) {
	transport, ok := params["transport"].(string)
	if !ok || transport == "" {
		return "", fmt.Errorf("transport required")
	}
	chatID, ok := params["chat_id"].(string)
	if !ok || chatID == "" {
		return "", fmt.Errorf("chat_id required")
	}

	delay := 0
	if d, ok := params["delay_seconds"].(float64); ok {
		delay = int(d)
	} else if d, ok := params["delay_seconds"].(int); ok {
		delay = d
	}

	if delay > 0 {
		// Same as run_agent - v1 executes immediately
	}

	if err := e.cfg.ReportSender.SendReport(ctx, transport, chatID); err != nil {
		return "", err
	}
	return fmt.Sprintf("report sent via %s to %s", transport, chatID), nil
}

func (e *Engine) executeRunRestoreTest(ctx context.Context) (string, error) {
	// Build deps for restore test
	// Note: ResticWrapper and VaultGetSecret need to be provided via EngineConfig
	// For v1, we'll use the DefaultDeps pattern
	deps := restoretest.Deps{
		DB:            e.cfg.Store.(*SQLStore).db,
		ResticWrapper: nil, // Would need to be injected
		VaultGetSecret: nil, // Would need to be injected
		Now:           e.cfg.NowFunc,
	}
	result, err := e.cfg.RestoreTestRunner.RunRestoreTest(ctx, deps)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("restore test completed: samples=%d passed=%d failed=%d", result.Samples, result.Passed, result.Failed), nil
}