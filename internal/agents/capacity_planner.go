package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/fauzone/kelvios/internal/catalog"
)

type capacityPlannerAgent struct{}

func (capacityPlannerAgent) Name() string       { return "capacity_planner" }
func (capacityPlannerAgent) Describe() string   { return "Per-pool fullness, threshold advice, and projection (current usage only)" }

func (capacityPlannerAgent) Run(ctx context.Context, deps *Deps) (Report, error) {
	now := deps.Now()
	report := Report{
		Agent:       "capacity_planner",
		GeneratedAt: now,
		Findings:    make([]string, 0),
		Actions:     make([]string, 0),
	}

	// Get all pools with disk info
	pools, err := catalog.ListPools(deps.DB)
	if err != nil {
		report.Summary = "Failed to list pools"
		report.Findings = append(report.Findings, fmt.Sprintf("list pools: %v", err))
		return report, nil
	}

	if len(pools) == 0 {
		report.Summary = "No pools configured"
		report.Actions = append(report.Actions, "Create a storage pool via POST /api/pools")
		return report, nil
	}

	// Get disk inventory for disk count per pool
	disks, err := deps.DisksFunc()
	if err != nil {
		report.Findings = append(report.Findings, fmt.Sprintf("disk inventory failed: %v", err))
		disks = nil
	}

	// Build disk count per pool
	diskCount := make(map[string]int)
	if disks != nil {
		for _, d := range disks {
			if d.PoolID != "" {
				diskCount[d.PoolID]++
			}
		}
	}

	// Also get disk names from pool_disks for pools that might not have disks in inventory
	for _, p := range pools {
		if _, ok := diskCount[p.ID]; !ok {
			var count int
			err := deps.DB.QueryRow(`SELECT COUNT(*) FROM pool_disks WHERE pool_id = ?`, p.ID).Scan(&count)
			if err == nil {
				diskCount[p.ID] = count
			}
		}
	}

	// Analyze each pool
	urgentCount := 0
	warningCount := 0
	healthyCount := 0
	var poolSummaries []string

	for _, p := range pools {
		if p.SizeBytes <= 0 {
			poolSummaries = append(poolSummaries, fmt.Sprintf("%s: size unknown", p.Name))
			continue
		}

		pct := float64(p.UsedBytes) / float64(p.SizeBytes) * 100
			advice := ""

			if pct >= 90 {
				urgentCount++
				advice = " — URGENT: add disk immediately"
			} else if pct >= 80 {
				warningCount++
				advice = " — WARNING: plan to add a disk"
			} else {
				healthyCount++
			}

		diskNum := diskCount[p.ID]
		poolSummaries = append(poolSummaries,
			fmt.Sprintf("%s: %.1f%% used (%s / %s), %d disk(s)%s",
				p.Name, pct, formatBytes(p.UsedBytes), formatBytes(p.SizeBytes), diskNum, advice))
	}

	// Build summary
	var summaryParts []string
	summaryParts = append(summaryParts, fmt.Sprintf("%d pool(s) analyzed", len(pools)))
	if urgentCount > 0 {
		summaryParts = append(summaryParts, fmt.Sprintf("%d urgent (>90%%)", urgentCount))
	}
	if warningCount > 0 {
		summaryParts = append(summaryParts, fmt.Sprintf("%d warning (>80%%)", warningCount))
	}
	if healthyCount > 0 {
		summaryParts = append(summaryParts, fmt.Sprintf("%d healthy", healthyCount))
	}
	// Projection note
	summaryParts = append(summaryParts, "projection: CURRENT usage only (no usage-history table); days-to-full unavailable")
	report.Summary = strings.Join(summaryParts, "; ")

	// Findings
	report.Findings = append(report.Findings, poolSummaries...)

	// Projection limitation finding
	report.Findings = append(report.Findings,
		"Projection basis: current usage snapshot only. No usage-history table exists; growth rate and days-to-full cannot be estimated.")

	// Actions
	if urgentCount > 0 {
		report.Actions = append(report.Actions, fmt.Sprintf("%d pool(s) over 90%% — add disk(s) immediately", urgentCount))
	}
	if warningCount > 0 {
		report.Actions = append(report.Actions, fmt.Sprintf("%d pool(s) over 80%% — schedule disk addition", warningCount))
	}
	if urgentCount == 0 && warningCount == 0 && len(pools) > 0 {
		report.Actions = append(report.Actions, "All pools below 80% — no immediate capacity action needed")
	}
	report.Actions = append(report.Actions, "Enable usage-history tracking to unlock growth projections and days-to-full estimates")

	return report, nil
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func init() {
	Register(capacityPlannerAgent{})
}
