package agents

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
)

type backupStewardAgent struct{}

func (backupStewardAgent) Name() string       { return "backup_steward" }
func (backupStewardAgent) Describe() string   { return "Backup repos, snapshots, restore-test status, and capacity notes" }

func (backupStewardAgent) Run(ctx context.Context, deps *Deps) (Report, error) {
	now := deps.Now()
	report := Report{
		Agent:       "backup_steward",
		GeneratedAt: now,
		Findings:    make([]string, 0),
		Actions:     make([]string, 0),
	}

	// Get all backup repos
	repos, err := catalog.ListBackupRepos(deps.DB)
	if err != nil {
		report.Summary = "Failed to list backup repos"
		report.Findings = append(report.Findings, fmt.Sprintf("list backup repos: %v", err))
		return report, nil
	}

	if len(repos) == 0 {
		report.Summary = "No backup repositories configured"
		report.Actions = append(report.Actions, "Configure a backup repository via POST /api/backups/repos")
		return report, nil
	}

	// Get latest restore test run
	var latestRunID string
	var latestRunStartedAt int64
	var latestRunSamples, latestRunPassed, latestRunFailed, latestRunSkipped, latestRunAlerts int
	err = deps.DB.QueryRow(`
		SELECT id, started_at, samples, passed, failed, skipped, alerts
		FROM restore_test_runs
		ORDER BY started_at DESC
		LIMIT 1
	`).Scan(&latestRunID, &latestRunStartedAt, &latestRunSamples, &latestRunPassed, &latestRunFailed, &latestRunSkipped, &latestRunAlerts)
	hasRestoreTest := err == nil

	// Get open alerts count
	var openAlerts int
	err = deps.DB.QueryRow(`
		SELECT COUNT(*) FROM restore_test_alerts WHERE resolved = 0
	`).Scan(&openAlerts)
	if err != nil {
		openAlerts = -1 // unknown
	}

	// Per-repo details
	var repoSummaries []string
	totalSnapshots := 0
	for _, repo := range repos {
		snapshots, err := catalog.ListBackupSnapshots(deps.DB, repo.ID)
		if err != nil {
			report.Findings = append(report.Findings, fmt.Sprintf("repo %s: list snapshots failed: %v", repo.Name, err))
			continue
		}
		totalSnapshots += len(snapshots)

		var latestSnapTime int64
		if len(snapshots) > 0 {
			// snapshots are ordered by created_at, id ascending
			latestSnapTime = snapshots[len(snapshots)-1].CreatedAt
		}

		// Capacity note from target config
		capacityNote := ""
		if repo.TargetType == "local" {
			capacityNote = fmt.Sprintf(" (target: %s)", repo.TargetPath)
		} else {
			capacityNote = fmt.Sprintf(" (target: %s, config: %s)", repo.TargetType, repo.ConfigJSON)
		}

		repoSummaries = append(repoSummaries, fmt.Sprintf("%s: %d snapshot(s), latest %s%s",
			repo.Name, len(snapshots), formatTime(latestSnapTime), capacityNote))
	}

	// Build summary
	var summaryParts []string
	summaryParts = append(summaryParts, fmt.Sprintf("%d backup repo(s)", len(repos)))
	summaryParts = append(summaryParts, fmt.Sprintf("%d total snapshot(s)", totalSnapshots))
	if hasRestoreTest {
		summaryParts = append(summaryParts, fmt.Sprintf("latest restore test: %s (samples=%d passed=%d failed=%d skipped=%d alerts=%d)",
			formatTime(latestRunStartedAt), latestRunSamples, latestRunPassed, latestRunFailed, latestRunSkipped, latestRunAlerts))
	} else {
		summaryParts = append(summaryParts, "no restore test runs recorded")
	}
	if openAlerts >= 0 {
		summaryParts = append(summaryParts, fmt.Sprintf("%d open restore-test alert(s)", openAlerts))
	}
	report.Summary = strings.Join(summaryParts, "; ")

	// Findings
	report.Findings = append(report.Findings, repoSummaries...)
	if openAlerts > 0 {
		report.Findings = append(report.Findings, fmt.Sprintf("%d unresolved restore-test alert(s) need attention", openAlerts))
	}
	if !hasRestoreTest {
		report.Findings = append(report.Findings, "No scheduled restore tests have run yet")
	}

	// Actions
	if len(repos) == 0 {
		report.Actions = append(report.Actions, "Create a backup repository")
	} else {
		if !hasRestoreTest {
			report.Actions = append(report.Actions, "Run a scheduled restore test (POST /api/restore-tests/run)")
		}
		if openAlerts > 0 {
			report.Actions = append(report.Actions, "Review and resolve open restore-test alerts")
		}
		for _, repo := range repos {
			snapshots, _ := catalog.ListBackupSnapshots(deps.DB, repo.ID)
			if len(snapshots) == 0 {
				report.Actions = append(report.Actions, fmt.Sprintf("Repo %s has no snapshots — run a backup", repo.Name))
			}
		}
	}
	if len(report.Actions) == 0 {
		report.Actions = append(report.Actions, "All repos have snapshots; restore tests running; no open alerts")
	}

	return report, nil
}

func formatTime(unix int64) string {
	if unix <= 0 {
		return "never"
	}
	return time.Unix(unix, 0).Format("2006-01-02 15:04")
}

func init() {
	Register(backupStewardAgent{})
}
