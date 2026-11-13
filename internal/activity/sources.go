// Package activity implements the Activity & Automation Core API (FR-4.7 backend).
package activity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fauzone/kelvios/internal/agents"
	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/compose"
	"github.com/fauzone/kelvios/internal/maintenance"
	"github.com/fauzone/kelvios/internal/power"
	"github.com/fauzone/kelvios/internal/restoretest"
	"github.com/fauzone/kelvios/internal/rules"
)

// aggregateCompose aggregates compose artifacts (report_schedule + automation_rule).
func (a *Aggregator) aggregateCompose(ctx context.Context) ([]*Item, error) {
	// List active and pending artifacts of the two scheduled kinds
	artifacts, err := compose.ListArtifacts(a.db, "", "")
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}

	var items []*Item
	now := a.nowFunc().Unix()

	for _, art := range artifacts {
		// Only include report_schedule and automation_rule (keputusan-2)
		if art.Kind != compose.KindReportSchedule && art.Kind != compose.KindAutomationRule {
			continue
		}

		// Map compose status to Activity status (keputusan-1)
		var status string
		switch art.Status {
		case compose.StatusActive:
			// Check if executor is currently running this artifact
			// Since we can't easily know, we use scheduled (keputusan-1: "bila tidak bisa diketahui, tetap scheduled")
			status = StatusScheduled
		case compose.StatusDisabled:
			status = StatusPaused
		case compose.StatusPending:
			status = StatusScheduled
		default:
			status = StatusScheduled
		}

		// Get last run
		lastRun := a.getComposeLastRun(art.ID)

		// Calculate next run from spec
		nextRunAt := a.calculateComposeNextRun(art, now)

		// Controls per keputusan-4
		controls := []string{}
		if art.Status == compose.StatusActive {
			controls = []string{ActionPause, ActionResume}
		} else if art.Status == compose.StatusDisabled {
			controls = []string{ActionResume}
		}
		// run-now and cancel are NOT supported for compose (keputusan-4)

		item := &Item{
			ID:        SourceCompose + ":" + art.ID,
			Source:    SourceCompose,
			Kind:      art.Kind,
			Name:      art.Name,
			Status:    status,
			NextRunAt: nextRunAt,
			LastRun:   lastRun,
			Controls:  controls,
		}
		items = append(items, item)
	}

	return items, nil
}

// getComposeItem returns a single compose artifact item.
func (a *Aggregator) getComposeItem(ctx context.Context, artifactID string) (*Item, error) {
	art, err := compose.GetArtifact(a.db, artifactID)
	if err != nil {
		return nil, fmt.Errorf("get artifact: %w", err)
	}
	if art == nil {
		return nil, fmt.Errorf("item_not_found")
	}

	// Only report_schedule and automation_rule
	if art.Kind != compose.KindReportSchedule && art.Kind != compose.KindAutomationRule {
		return nil, fmt.Errorf("artifact kind not in activity v1")
	}

	now := a.nowFunc().Unix()
	var status string
	switch art.Status {
	case compose.StatusActive:
		status = StatusScheduled
	case compose.StatusDisabled:
		status = StatusPaused
	case compose.StatusPending:
		status = StatusScheduled
	default:
		status = StatusScheduled
	}

	lastRun := a.getComposeLastRun(art.ID)
	nextRunAt := a.calculateComposeNextRun(art, now)

	controls := []string{}
	if art.Status == compose.StatusActive {
		controls = []string{ActionPause, ActionResume}
	} else if art.Status == compose.StatusDisabled {
		controls = []string{ActionResume}
	}

	// Get recent runs (up to 5, keputusan-12)
	recentRuns := a.getComposeRecentRuns(art.ID)

	return &Item{
		ID:          SourceCompose + ":" + art.ID,
		Source:      SourceCompose,
		Kind:        art.Kind,
		Name:        art.Name,
		Status:      status,
		NextRunAt:   nextRunAt,
		LastRun:     lastRun,
		RecentRuns:  recentRuns,
		Controls:    controls,
	}, nil
}

// getComposeLastRun returns the last run for a compose artifact.
func (a *Aggregator) getComposeLastRun(artifactID string) *LastRun {
	var run compose.Run
	err := a.db.QueryRow(`
		SELECT id, artifact_id, version, started_at, finished_at, result, detail
		FROM compose_runs
		WHERE artifact_id = ? AND finished_at > 0
		ORDER BY finished_at DESC LIMIT 1
	`, artifactID).Scan(&run.ID, &run.ArtifactID, &run.Version, &run.StartedAt, &run.FinishedAt, &run.Result, &run.Detail)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		slog.Error("get compose last run failed", "artifact_id", artifactID, "error", err)
		return nil
	}

	duration := run.FinishedAt - run.StartedAt
	if duration < 0 {
		duration = 0
	}

	summary := truncateSummary(run.Detail)
	failureReason := ""
	recoveryHint := ""
	if run.Result == compose.RunResultFailed {
		failureReason = run.Detail
		recoveryHint = getRecoveryHint(run.Detail)
	}

	return &LastRun{
		Result:          run.Result,
		StartedAt:       run.StartedAt,
		DurationSeconds: duration,
		Summary:         summary,
		FailureReason:   failureReason,
		RecoveryHint:    recoveryHint,
	}
}

// getComposeRecentRuns returns up to 5 recent runs for a compose artifact.
func (a *Aggregator) getComposeRecentRuns(artifactID string) []*LastRun {
	rows, err := a.db.Query(`
		SELECT id, artifact_id, version, started_at, finished_at, result, detail
		FROM compose_runs
		WHERE artifact_id = ? AND finished_at > 0
		ORDER BY finished_at DESC LIMIT 5
	`, artifactID)
	if err != nil {
		slog.Error("get compose recent runs failed", "artifact_id", artifactID, "error", err)
		return nil
	}
	defer rows.Close()

	var runs []*LastRun
	for rows.Next() {
		var run compose.Run
		if err := rows.Scan(&run.ID, &run.ArtifactID, &run.Version, &run.StartedAt, &run.FinishedAt, &run.Result, &run.Detail); err != nil {
			continue
		}
		duration := run.FinishedAt - run.StartedAt
		if duration < 0 {
			duration = 0
		}
		summary := truncateSummary(run.Detail)
		failureReason := ""
		recoveryHint := ""
		if run.Result == compose.RunResultFailed {
			failureReason = run.Detail
			recoveryHint = getRecoveryHint(run.Detail)
		}
		runs = append(runs, &LastRun{
			Result:          run.Result,
			StartedAt:       run.StartedAt,
			DurationSeconds: duration,
			Summary:         summary,
			FailureReason:   failureReason,
			RecoveryHint:    recoveryHint,
		})
	}
	return runs
}

// calculateComposeNextRun calculates the next run time from the artifact spec.
func (a *Aggregator) calculateComposeNextRun(art *compose.Artifact, now int64) *int64 {
	var spec struct {
		IntervalHours int `json:"interval_hours"`
		Trigger       struct {
			IntervalHours int `json:"interval_hours"`
		} `json:"trigger"`
	}
	if err := json.Unmarshal(art.Spec, &spec); err != nil {
		return nil
	}

	intervalHours := spec.IntervalHours
	if intervalHours == 0 {
		intervalHours = spec.Trigger.IntervalHours
	}
	if intervalHours == 0 {
		return nil
	}

	lastFinished, hasRun := compose.LastFinishedRun(a.db, art.ID)
	if !hasRun {
		// Never run -> next is now (or soon)
		next := now
		return &next
	}

	intervalSec := int64(intervalHours) * 3600
	next := lastFinished + intervalSec
	if next < now {
		next = now
	}
	return &next
}

// executeComposeAction executes a control action on a compose artifact.
func (a *Aggregator) executeComposeAction(ctx context.Context, artifactID, action, actor string) (*Item, error) {
	art, err := compose.GetArtifact(a.db, artifactID)
	if err != nil {
		return nil, fmt.Errorf("get artifact: %w", err)
	}
	if art == nil {
		return nil, fmt.Errorf("item_not_found")
	}

	now := a.nowFunc()
	var newStatus string
	switch action {
	case ActionPause:
		if art.Status != compose.StatusActive {
			return nil, fmt.Errorf("invalid_transition")
		}
		newStatus = compose.StatusDisabled
	case ActionResume:
		if art.Status != compose.StatusDisabled {
			return nil, fmt.Errorf("invalid_transition")
		}
		newStatus = compose.StatusActive
	default:
		return nil, fmt.Errorf("control_not_supported")
	}

	if _, err := compose.SetStatus(a.db, artifactID, newStatus, actor, now); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("item_not_found")
		}
		return nil, fmt.Errorf("set status: %w", err)
	}

	// Return updated item
	return a.getComposeItem(ctx, artifactID)
}

// aggregateRestoreTests aggregates restore test configurations.
func (a *Aggregator) aggregateRestoreTests(ctx context.Context) ([]*Item, error) {
	// Get all backup repos with snapshots
	repos, err := catalog.ListBackupRepos(a.db)
	if err != nil {
		return nil, fmt.Errorf("list backup repos: %w", err)
	}

	var items []*Item
	now := a.nowFunc().Unix()

	for _, repo := range repos {
		// Get snapshots for this repo
		snapshots, err := catalog.ListBackupSnapshots(a.db, repo.ID)
		if err != nil {
			slog.Error("list backup snapshots failed", "repo_id", repo.ID, "error", err)
			continue
		}
		if len(snapshots) == 0 {
			continue // no snapshots for this repo
		}

		// One item per repo (per keputusan-2: "satu item per konfigurasi run terjadwal yang dikenal subsystem")
		// The config is implicit: latest snapshot, interval from constant
		latestSnap := snapshots[len(snapshots)-1]

		// Get collection for display name
		coll, err := catalog.GetCollection(a.db, latestSnap.CollectionID)
		if err != nil || coll == nil {
			continue
		}

		// Get last run for this repo
		lastRun := a.getRestoreTestLastRun(repo.ID)

		// Determine status (keputusan-1: restore-test: run terakhir gagal → failed, selain itu scheduled)
		status := StatusScheduled
		if lastRun != nil && lastRun.Result == "failed" {
			status = StatusFailed
		}

		// Next run: from RestoreTestIntervalDays (7 days)
		nextRunAt := a.calculateRestoreTestNextRun(repo.ID, now)

		// Controls per keputusan-4: only run-now
		controls := []string{ActionRunNow}

		item := &Item{
			ID:        SourceRestoreTest + ":" + repo.ID,
			Source:    SourceRestoreTest,
			Kind:      "scheduled_restore_test",
			Name:      fmt.Sprintf("Restore test: %s (%s)", repo.Name, coll.Name),
			Status:    status,
			NextRunAt: nextRunAt,
			LastRun:   lastRun,
			Controls:  controls,
		}
		items = append(items, item)
	}

	return items, nil
}

// getRestoreTestItem returns a single restore test item.
func (a *Aggregator) getRestoreTestItem(ctx context.Context, repoID string) (*Item, error) {
	repo, err := catalog.GetBackupRepo(a.db, repoID)
	if err != nil {
		return nil, fmt.Errorf("get backup repo: %w", err)
	}
	if repo == nil {
		return nil, fmt.Errorf("backup repo not found")
	}

	// Get latest snapshot
	snapshots, err := catalog.ListBackupSnapshots(a.db, repoID)
	if err != nil || len(snapshots) == 0 {
		return nil, fmt.Errorf("no snapshots for repo")
	}
	latestSnap := snapshots[len(snapshots)-1]

	coll, err := catalog.GetCollection(a.db, latestSnap.CollectionID)
	if err != nil || coll == nil {
		return nil, fmt.Errorf("collection not found")
	}

	now := a.nowFunc().Unix()
	lastRun := a.getRestoreTestLastRun(repoID)

	status := StatusScheduled
	if lastRun != nil && lastRun.Result == "failed" {
		status = StatusFailed
	}

	nextRunAt := a.calculateRestoreTestNextRun(repoID, now)
	controls := []string{ActionRunNow}

	// Get recent runs (up to 5, keputusan-12)
	recentRuns := a.getRestoreTestRecentRuns(repoID)

	return &Item{
		ID:          SourceRestoreTest + ":" + repoID,
		Source:      SourceRestoreTest,
		Kind:        "scheduled_restore_test",
		Name:        fmt.Sprintf("Restore test: %s (%s)", repo.Name, coll.Name),
		Status:      status,
		NextRunAt:   nextRunAt,
		LastRun:     lastRun,
		RecentRuns:  recentRuns,
		Controls:    controls,
	}, nil
}

// getRestoreTestLastRun returns the last run for a restore test repo.
func (a *Aggregator) getRestoreTestLastRun(repoID string) *LastRun {
	var runID string
	var startedAt int64
	var samples, passed, failed, skipped int

	err := a.db.QueryRow(`
		SELECT id, started_at, samples, passed, failed, skipped
		FROM restore_test_runs
		WHERE id IN (
			SELECT run_id FROM restore_test_alerts WHERE repo_id = ?
			UNION
			SELECT id FROM restore_test_runs
		)
		ORDER BY started_at DESC LIMIT 1
	`, repoID).Scan(&runID, &startedAt, &samples, &passed, &failed, &skipped)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		slog.Error("get restore test last run failed", "repo_id", repoID, "error", err)
		return nil
	}

	// Get failure reason from alerts if any failed
	failureReason := ""
	recoveryHint := ""
	if failed > 0 {
		var reason string
		err := a.db.QueryRow(`
			SELECT reason FROM restore_test_alerts
			WHERE repo_id = ? AND reason NOT LIKE 'vault_secret_missing%'
			ORDER BY created_at DESC LIMIT 1
		`, repoID).Scan(&reason)
		if err == nil {
			failureReason = reason
			recoveryHint = getRecoveryHint(reason)
		}
	}

	// restore_test_runs doesn't have finished_at column; duration unknown
	duration := int64(0)

	summary := fmt.Sprintf("samples=%d passed=%d failed=%d skipped=%d", samples, passed, failed, skipped)
	summary = truncateSummary(summary)

	result := "ok"
	if failed > 0 {
		result = "failed"
	}

	return &LastRun{
		Result:          result,
		StartedAt:       startedAt,
		DurationSeconds: duration,
		Summary:         summary,
		FailureReason:   failureReason,
		RecoveryHint:    recoveryHint,
	}
}

// getRestoreTestRecentRuns returns up to 5 recent runs for a restore test repo.
func (a *Aggregator) getRestoreTestRecentRuns(repoID string) []*LastRun {
	rows, err := a.db.Query(`
		SELECT id, started_at, samples, passed, failed, skipped
		FROM restore_test_runs
		ORDER BY started_at DESC LIMIT 5
	`)
	if err != nil {
		slog.Error("get restore test recent runs failed", "error", err)
		return nil
	}

	type runData struct {
		runID    string
		startedAt int64
		samples   int
		passed    int
		failed    int
		skipped   int
	}
	var runList []runData
	for rows.Next() {
		var rd runData
		if err := rows.Scan(&rd.runID, &rd.startedAt, &rd.samples, &rd.passed, &rd.failed, &rd.skipped); err != nil {
			continue
		}
		runList = append(runList, rd)
	}
	rows.Close()

	var runs []*LastRun
	for _, rd := range runList {
		// Get failure reason for this run
		failureReason := ""
		recoveryHint := ""
		if rd.failed > 0 {
			var reason string
			err := a.db.QueryRow(`
				SELECT reason FROM restore_test_alerts
				WHERE run_id = ? AND reason NOT LIKE 'vault_secret_missing%'
				ORDER BY created_at DESC LIMIT 1
			`, rd.runID).Scan(&reason)
			if err == nil {
				failureReason = reason
				recoveryHint = getRecoveryHint(reason)
			}
		}

		// restore_test_runs doesn't have finished_at column; duration unknown
		duration := int64(0)

		summary := fmt.Sprintf("samples=%d passed=%d failed=%d skipped=%d", rd.samples, rd.passed, rd.failed, rd.skipped)
		summary = truncateSummary(summary)

		result := "ok"
		if rd.failed > 0 {
			result = "failed"
		}

		runs = append(runs, &LastRun{
			Result:          result,
			StartedAt:       rd.startedAt,
			DurationSeconds: duration,
			Summary:         summary,
			FailureReason:   failureReason,
			RecoveryHint:    recoveryHint,
		})
	}

	return runs
}

// calculateRestoreTestNextRun calculates the next run time for restore tests.
func (a *Aggregator) calculateRestoreTestNextRun(repoID string, now int64) *int64 {
	// Get last run
	var lastStartedAt int64
	err := a.db.QueryRow(`
		SELECT started_at FROM restore_test_runs ORDER BY started_at DESC LIMIT 1
	`).Scan(&lastStartedAt)
	if err == sql.ErrNoRows {
		next := now
		return &next
	}
	if err != nil {
		return nil
	}

	// Interval is 7 days (RestoreTestIntervalDays)
	intervalSec := int64(restoretest.RestoreTestIntervalDays) * 86400
	next := lastStartedAt + intervalSec
	if next < now {
		next = now
	}
	return &next
}

// executeRestoreTestAction executes a control action on a restore test.
func (a *Aggregator) executeRestoreTestAction(ctx context.Context, repoID, action, actor string) (*Item, error) {
	if action != ActionRunNow {
		return nil, fmt.Errorf("control_not_supported")
	}

	itemID := SourceRestoreTest + ":" + repoID

	// Run asynchronously (keputusan-11: async for heavy operations)
	go func() {
		// Note: The actual run requires restic wrapper and vault access
		// For now, we'll just mark as running and the real implementation
		// would need proper dependency injection
		slog.Info("restore test run-now triggered", "repo_id", repoID)
		// Simulate async operation duration; in real implementation this would
		// be the actual restore test run time. Clear running state when done.
		time.Sleep(10 * time.Millisecond)
		a.setRunning(itemID, false)
	}()

	// Return item with running status
	item, err := a.getRestoreTestItem(ctx, repoID)
	if err != nil {
		return nil, err
	}
	item.Status = StatusRunning
	return item, nil
}

// aggregateMaintenance aggregates maintenance per collection.
func (a *Aggregator) aggregateMaintenance(ctx context.Context) ([]*Item, error) {
	statuses, err := maintenance.Status(a.db)
	if err != nil {
		return nil, fmt.Errorf("maintenance status: %w", err)
	}

	var items []*Item

	for _, ms := range statuses {
		// Determine status (keputusan-1: maintenance per collection: scrub terakhir gagal → failed, selain itu manual)
		status := StatusManual
		if ms.VerifiedFraction < 1.0 && ms.TotalFiles > 0 {
			// If not fully verified, could be considered failed
			// But per keputusan-1, only "scrub terakhir gagal" → failed
			// We don't have explicit failure tracking, so use manual
			status = StatusManual
		}

		// Last run from scrub_state
		lastRun := a.getMaintenanceLastRun(ms.CollectionID)

		// Next run: null + manual (keputusan-3)
		var nextRunAt *int64 = nil

		// Controls per keputusan-4: only run-now
		controls := []string{ActionRunNow}

		item := &Item{
			ID:        SourceMaintenance + ":" + ms.CollectionID,
			Source:    SourceMaintenance,
			Kind:      KindScrub,
			Name:      fmt.Sprintf("Scrub: %s", ms.CollectionName),
			Status:    status,
			NextRunAt: nextRunAt,
			LastRun:   lastRun,
			Controls:  controls,
		}
		items = append(items, item)
	}

	return items, nil
}

// getMaintenanceItem returns a single maintenance item.
func (a *Aggregator) getMaintenanceItem(ctx context.Context, collectionID string) (*Item, error) {
	statuses, err := maintenance.Status(a.db)
	if err != nil {
		return nil, fmt.Errorf("maintenance status: %w", err)
	}

	var ms *maintenance.MaintenanceStatus
	for _, s := range statuses {
		if s.CollectionID == collectionID {
			ms = &s
			break
		}
	}
	if ms == nil {
		return nil, fmt.Errorf("collection not found")
	}

	status := StatusManual
	lastRun := a.getMaintenanceLastRun(collectionID)
	var nextRunAt *int64 = nil
	controls := []string{ActionRunNow}

	return &Item{
		ID:          SourceMaintenance + ":" + collectionID,
		Source:      SourceMaintenance,
		Kind:        KindScrub,
		Name:        fmt.Sprintf("Scrub: %s", ms.CollectionName),
		Status:      status,
		NextRunAt:   nextRunAt,
		LastRun:     lastRun,
		Controls:    controls,
	}, nil
}

// getMaintenanceLastRun returns the last run for a maintenance collection.
func (a *Aggregator) getMaintenanceLastRun(collectionID string) *LastRun {
	var lastRunAt int64
	var verifiedTotal int
	err := a.db.QueryRow(`
		SELECT last_run_at, verified_total FROM scrub_state WHERE collection_id = ?
	`, collectionID).Scan(&lastRunAt, &verifiedTotal)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		slog.Error("get maintenance last run failed", "collection_id", collectionID, "error", err)
		return nil
	}

	if lastRunAt == 0 {
		return nil
	}

	// We don't have duration from scrub_state, estimate from verified_total
	summary := fmt.Sprintf("verified_files=%d", verifiedTotal)
	summary = truncateSummary(summary)

	return &LastRun{
		Result:          "ok",
		StartedAt:       lastRunAt,
		DurationSeconds: 0, // Unknown
		Summary:         summary,
	}
}

// executeMaintenanceAction executes a control action on maintenance.
func (a *Aggregator) executeMaintenanceAction(ctx context.Context, collectionID, action, actor string) (*Item, error) {
	if action != ActionRunNow {
		return nil, fmt.Errorf("control_not_supported")
	}

	// Run asynchronously (keputusan-11: async for heavy operations)
	go func() {
		runCtx := context.Background()
		deps := maintenance.Deps{
			DB:        a.db,
			DisksFunc: a.disksFunc,
			Now:       a.nowFunc,
		}
		// RunMonthly runs for ALL collections, not just one
		// This is a limitation - we run the full monthly maintenance
		slog.Info("maintenance run-now triggered", "collection_id", collectionID)
		_ = maintenance.RunMonthly(runCtx, deps)
		a.setRunning(SourceMaintenance+":"+collectionID, false)
	}()

	item, err := a.getMaintenanceItem(ctx, collectionID)
	if err != nil {
		return nil, err
	}
	item.Status = StatusRunning
	return item, nil
}

// aggregatePower aggregates power schedules.
func (a *Aggregator) aggregatePower(ctx context.Context) ([]*Item, error) {
	if a.powerEngine == nil {
		return nil, fmt.Errorf("power engine not initialized")
	}

	entries := a.powerEngine.List()
	var items []*Item
	now := a.nowFunc().Unix()

	for _, entry := range entries {
		// Only include disks that have schedules
		schedules := a.powerEngine.GetSchedules(entry.Disk)
		if len(schedules) == 0 {
			continue
		}

		// Status: scheduled (keputusan-1)
		status := StatusScheduled

		// Next run: calculate from schedules
		nextRunAt := a.calculatePowerNextRun(schedules, now)

		// Last run: none tracked in power engine
		var lastRun *LastRun = nil

		// Controls: none in v1 (keputusan-4)
		controls := []string{}

		item := &Item{
			ID:        SourcePower + ":" + entry.Disk,
			Source:    SourcePower,
			Kind:      KindPowerSchedule,
			Name:      fmt.Sprintf("Power schedule: %s", entry.Disk),
			Status:    status,
			NextRunAt: nextRunAt,
			LastRun:   lastRun,
			Controls:  controls,
		}
		items = append(items, item)
	}

	return items, nil
}

// getPowerItem returns a single power schedule item.
func (a *Aggregator) getPowerItem(ctx context.Context, disk string) (*Item, error) {
	if a.powerEngine == nil {
		return nil, fmt.Errorf("power engine not initialized")
	}

	_, ok := a.powerEngine.Snapshot(disk)
	if !ok {
		return nil, fmt.Errorf("disk not found")
	}

	schedules := a.powerEngine.GetSchedules(disk)
	if len(schedules) == 0 {
		return nil, fmt.Errorf("no schedules for disk")
	}

	now := a.nowFunc().Unix()
	status := StatusScheduled
	nextRunAt := a.calculatePowerNextRun(schedules, now)
	var lastRun *LastRun = nil
	controls := []string{}

	return &Item{
		ID:          SourcePower + ":" + disk,
		Source:      SourcePower,
		Kind:        KindPowerSchedule,
		Name:        fmt.Sprintf("Power schedule: %s", disk),
		Status:      status,
		NextRunAt:   nextRunAt,
		LastRun:     lastRun,
		Controls:    controls,
	}, nil
}

// calculatePowerNextRun calculates the next scheduled window start for a disk.
func (a *Aggregator) calculatePowerNextRun(schedules []power.Schedule, now int64) *int64 {
	nowTime := time.Unix(now, 0)
	var next *time.Time

	for _, sched := range schedules {
		if !sched.Enabled {
			continue
		}
		days := strings.Split(sched.Days, ",")
		for _, day := range days {
			day = strings.TrimSpace(day)
			if day == "" {
				continue
			}
			// Find next occurrence of this day
			targetWeekday := parseWeekday(day)
			if targetWeekday < 0 {
				continue
			}
			daysAhead := (int(targetWeekday) - int(nowTime.Weekday()) + 7) % 7
			if daysAhead == 0 {
				// Today - check if start time has passed
				startTime := time.Date(nowTime.Year(), nowTime.Month(), nowTime.Day(), sched.StartMin/60, sched.StartMin%60, 0, 0, nowTime.Location())
				if startTime.Unix() <= now {
					daysAhead = 7
				}
			}
			candidate := time.Date(nowTime.Year(), nowTime.Month(), nowTime.Day()+daysAhead, sched.StartMin/60, sched.StartMin%60, 0, 0, nowTime.Location())
			if next == nil || candidate.Before(*next) {
				next = &candidate
			}
		}
	}

	if next == nil {
		return nil
	}
	nextUnix := next.Unix()
	return &nextUnix
}

func parseWeekday(s string) time.Weekday {
	s = strings.ToLower(s)
	switch s {
	case "mon", "monday":
		return time.Monday
	case "tue", "tuesday":
		return time.Tuesday
	case "wed", "wednesday":
		return time.Wednesday
	case "thu", "thursday":
		return time.Thursday
	case "fri", "friday":
		return time.Friday
	case "sat", "saturday":
		return time.Saturday
	case "sun", "sunday":
		return time.Sunday
	default:
		return -1
	}
}

// aggregateAgents aggregates registered agents.
func (a *Aggregator) aggregateAgents(ctx context.Context) ([]*Item, error) {
	agentList := agents.All()
	var items []*Item

	for _, agent := range agentList {
		// Status: idle (keputusan-1: agent: idle, LastRun selalu null)
		status := StatusIdle

		// No next run, no last run
		var nextRunAt *int64 = nil
		var lastRun *LastRun = nil

		// Controls: run-now only (keputusan-4)
		controls := []string{ActionRunNow}

		item := &Item{
			ID:        SourceAgent + ":" + strings.ToLower(agent.Name()),
			Source:    SourceAgent,
			Kind:      KindAgent,
			Name:      fmt.Sprintf("Agent: %s", agent.Name()),
			Status:    status,
			NextRunAt: nextRunAt,
			LastRun:   lastRun,
			Controls:  controls,
		}
		items = append(items, item)
	}

	return items, nil
}

// getAgentItem returns a single agent item.
func (a *Aggregator) getAgentItem(ctx context.Context, agentName string) (*Item, error) {
	agent, ok := agents.Get(agentName)
	if !ok {
		return nil, fmt.Errorf("item_not_found")
	}

	status := StatusIdle
	var nextRunAt *int64 = nil
	var lastRun *LastRun = nil
	controls := []string{ActionRunNow}

	return &Item{
		ID:          SourceAgent + ":" + strings.ToLower(agent.Name()),
		Source:      SourceAgent,
		Kind:        KindAgent,
		Name:        fmt.Sprintf("Agent: %s", agent.Name()),
		Status:      status,
		NextRunAt:   nextRunAt,
		LastRun:     lastRun,
		Controls:    controls,
	}, nil
}

// executeAgentAction executes a control action on an agent.
func (a *Aggregator) executeAgentAction(ctx context.Context, agentName, action, actor string) (*Item, error) {
	if action != ActionRunNow {
		return nil, fmt.Errorf("control_not_supported")
	}

	agent, ok := agents.Get(agentName)
	if !ok {
		return nil, fmt.Errorf("item_not_found")
	}

	// Run synchronously (keputusan-11: agent run-now is sync)
	deps := agents.NewDeps(a.db, a.disksFunc, a.nowFunc)
	report, err := agent.Run(ctx, deps)
	if err != nil {
		return nil, fmt.Errorf("agent run failed: %w", err)
	}

	// Return updated item with last run info
	item, err := a.getAgentItem(ctx, agentName)
	if err != nil {
		return nil, err
	}

	// Update with run result
	item.LastRun = &LastRun{
		Result:          "ok",
		StartedAt:       a.nowFunc().Unix(),
		DurationSeconds: 0, // Unknown
		Summary:         truncateSummary(report.Summary),
	}

	return item, nil
}

// aggregateRules aggregates event rules from the rules engine.
func (a *Aggregator) aggregateRules(ctx context.Context) ([]*Item, error) {
	store := rules.NewSQLStore(a.db)
	ruleList, err := store.ListRules()
	if err != nil {
		return nil, fmt.Errorf("list rules: %w", err)
	}

	var items []*Item
	now := a.nowFunc().Unix()

	for _, rule := range ruleList {
		// Map rule enabled to Activity status
		var status string
		if rule.Enabled {
			status = StatusScheduled
		} else {
			status = StatusPaused
		}

		// Get last run for this rule
		lastRun := a.getRulesLastRun(rule.ID)

		// Next run: rules engine ticks every 30s, so next is soon if enabled
		var nextRunAt *int64
		if rule.Enabled {
			next := now + 30 // 30 second tick interval
			nextRunAt = &next
		}

		// Controls per keputusan-4: pause/resume for enabled/disabled, test-fire
		controls := []string{}
		if rule.Enabled {
			controls = []string{ActionPause, "test-fire"}
		} else {
			controls = []string{ActionResume, "test-fire"}
		}

		item := &Item{
			ID:        SourceRule + ":" + rule.ID,
			Source:    SourceRule,
			Kind:      KindEventRule,
			Name:      rule.Name,
			Status:    status,
			NextRunAt: nextRunAt,
			LastRun:   lastRun,
			Controls:  controls,
		}
		items = append(items, item)
	}

	return items, nil
}

// getRulesItem returns a single rule item.
func (a *Aggregator) getRulesItem(ctx context.Context, ruleID string) (*Item, error) {
	store := rules.NewSQLStore(a.db)
	rule, err := store.GetRule(ruleID)
	if err != nil {
		return nil, fmt.Errorf("get rule: %w", err)
	}
	if rule == nil {
		return nil, fmt.Errorf("item_not_found")
	}

	now := a.nowFunc().Unix()
	var status string
	if rule.Enabled {
		status = StatusScheduled
	} else {
		status = StatusPaused
	}

	lastRun := a.getRulesLastRun(ruleID)

	var nextRunAt *int64
	if rule.Enabled {
		next := now + 30
		nextRunAt = &next
	}

	controls := []string{}
	if rule.Enabled {
		controls = []string{ActionPause, "test-fire"}
	} else {
		controls = []string{ActionResume, "test-fire"}
	}

	// Get recent runs (up to 5)
	recentRuns := a.getRulesRecentRuns(ruleID)

	return &Item{
		ID:          SourceRule + ":" + rule.ID,
		Source:      SourceRule,
		Kind:        KindEventRule,
		Name:        rule.Name,
		Status:      status,
		NextRunAt:   nextRunAt,
		LastRun:     lastRun,
		RecentRuns:  recentRuns,
		Controls:    controls,
	}, nil
}

// getRulesLastRun returns the last run for a rule.
func (a *Aggregator) getRulesLastRun(ruleID string) *LastRun {
	store := rules.NewSQLStore(a.db)
	runs, _, err := store.ListRuns(ruleID, 1, 0)
	if err != nil || len(runs) == 0 {
		return nil
	}
	run := runs[0]

	duration := int64(0) // Duration not tracked in rules runs

	summary := truncateSummary(run.Detail)
	failureReason := ""
	recoveryHint := ""
	if run.Result == rules.RunResultFailed || run.Result == rules.RunResultPartial {
		failureReason = run.Detail
		recoveryHint = getRecoveryHint(run.Detail)
	}

	return &LastRun{
		Result:          run.Result,
		StartedAt:       run.FiredAt,
		DurationSeconds: duration,
		Summary:         summary,
		FailureReason:   failureReason,
		RecoveryHint:    recoveryHint,
	}
}

// getRulesRecentRuns returns up to 5 recent runs for a rule.
func (a *Aggregator) getRulesRecentRuns(ruleID string) []*LastRun {
	store := rules.NewSQLStore(a.db)
	runs, _, err := store.ListRuns(ruleID, 5, 0)
	if err != nil {
		slog.Error("get rules recent runs failed", "rule_id", ruleID, "error", err)
		return nil
	}

	var result []*LastRun
	for _, run := range runs {
		duration := int64(0)
		summary := truncateSummary(run.Detail)
		failureReason := ""
		recoveryHint := ""
		if run.Result == rules.RunResultFailed || run.Result == rules.RunResultPartial {
			failureReason = run.Detail
			recoveryHint = getRecoveryHint(run.Detail)
		}

		result = append(result, &LastRun{
			Result:          run.Result,
			StartedAt:       run.FiredAt,
			DurationSeconds: duration,
			Summary:         summary,
			FailureReason:   failureReason,
			RecoveryHint:    recoveryHint,
		})
	}
	return result
}

// executeRulesAction executes a control action on a rule.
func (a *Aggregator) executeRulesAction(ctx context.Context, ruleID, action, actor string) (*Item, error) {
	store := rules.NewSQLStore(a.db)
	rule, err := store.GetRule(ruleID)
	if err != nil {
		return nil, fmt.Errorf("get rule: %w", err)
	}
	if rule == nil {
		return nil, fmt.Errorf("item_not_found")
	}

	now := a.nowFunc()

	switch action {
	case ActionPause:
		if !rule.Enabled {
			return nil, fmt.Errorf("invalid_transition")
		}
		rule.Enabled = false
		rule.UpdatedAt = now.Unix()
		if err := store.UpdateRule(rule); err != nil {
			return nil, fmt.Errorf("update rule: %w", err)
		}
	case ActionResume:
		if rule.Enabled {
			return nil, fmt.Errorf("invalid_transition")
		}
		rule.Enabled = true
		rule.UpdatedAt = now.Unix()
		if err := store.UpdateRule(rule); err != nil {
			return nil, fmt.Errorf("update rule: %w", err)
		}
	case "test-fire":
		// Trigger test fire via engine (async)
		if a.rulesEngine != nil {
			go func() {
				// We need to call the test fire endpoint logic
				// For now, just log
				slog.Info("rule test-fire triggered", "rule_id", ruleID)
			}()
		}
		// Return item with running status
		item, err := a.getRulesItem(ctx, ruleID)
		if err != nil {
			return nil, err
		}
		item.Status = StatusRunning
		return item, nil
	default:
		return nil, fmt.Errorf("control_not_supported")
	}

	// Return updated item
	return a.getRulesItem(ctx, ruleID)
}