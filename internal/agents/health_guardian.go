package agents

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/fauzone/kelvios/internal/maintenance"
)

type healthGuardianAgent struct{}

func (healthGuardianAgent) Name() string       { return "health_guardian" }
func (healthGuardianAgent) Describe() string   { return "Disk health from SMART and maintenance data" }

func (healthGuardianAgent) Run(ctx context.Context, deps *Deps) (Report, error) {
	now := deps.Now()
	report := Report{
		Agent:       "health_guardian",
		GeneratedAt: now,
		Findings:    make([]string, 0),
		Actions:     make([]string, 0),
	}

	// Get disk inventory
	disks, err := deps.DisksFunc()
	if err != nil {
		report.Summary = "Disk inventory failed"
		report.Findings = append(report.Findings, fmt.Sprintf("disk inventory error: %v", err))
		return report, nil
	}

	if len(disks) == 0 {
		report.Summary = "No disks found in inventory"
		return report, nil
	}

	// Run maintenance status to get SMART results and scrub status
	maintStatus, err := maintenance.Status(deps.DB)
	if err != nil {
		report.Findings = append(report.Findings, fmt.Sprintf("maintenance status query failed: %v", err))
	}

	// Check SMART data availability per disk
	smartAvailable := 0
	smartMissing := 0
	for _, disk := range disks {
		// Check if we have SMART data from maintenance runs
		// The maintenance engine runs SMART long tests; we check wake_checklist_runs for SMART steps
		var hasSmart int
		err := deps.DB.QueryRow(`
			SELECT COUNT(*) FROM wake_checklist_runs
			WHERE disk = ? AND steps LIKE '%smart%'
		`, disk.Name).Scan(&hasSmart)
		if err != nil && err != sql.ErrNoRows {
			report.Findings = append(report.Findings, fmt.Sprintf("SMART check for %s failed: %v", disk.Name, err))
			continue
		}
		if hasSmart > 0 {
			smartAvailable++
		} else {
			smartMissing++
			report.Findings = append(report.Findings, fmt.Sprintf("disk %s: SMART data unavailable (no maintenance run with SMART step recorded)", disk.Name))
		}
	}

	// Maintenance scrub status summary
	collectionsScrubbed := 0
	collectionsSkipped := 0
	for _, ms := range maintStatus {
		if ms.LastRunAt > 0 {
			collectionsScrubbed++
		} else {
			collectionsSkipped++
		}
	}

	// Build summary
	var summaryParts []string
	summaryParts = append(summaryParts, fmt.Sprintf("%d disks in inventory", len(disks)))
	if smartAvailable > 0 {
		summaryParts = append(summaryParts, fmt.Sprintf("%d with SMART history", smartAvailable))
	}
	if smartMissing > 0 {
		summaryParts = append(summaryParts, fmt.Sprintf("%d without SMART data", smartMissing))
	}
	summaryParts = append(summaryParts, fmt.Sprintf("%d collections scrubbed, %d never scrubbed", collectionsScrubbed, collectionsSkipped))
	report.Summary = strings.Join(summaryParts, "; ")

	// Actions
	if smartMissing > 0 {
		report.Actions = append(report.Actions, "Run monthly maintenance to initiate SMART long tests on disks without history")
	}
	if collectionsSkipped > 0 {
		report.Actions = append(report.Actions, "Schedule maintenance to scrub collections that have never been verified")
	}
	if len(report.Actions) == 0 {
		report.Actions = append(report.Actions, "All disks have SMART history; all collections have been scrubbed at least once")
	}

	return report, nil
}

// Register health_guardian at init time.
func init() {
	Register(healthGuardianAgent{})
}
