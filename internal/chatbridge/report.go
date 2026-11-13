package chatbridge

import (
	"bytes"
	"database/sql"
	"fmt"
	"text/template"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/maintenance"
)

// ReportData holds the data for the deterministic report template.
type ReportData struct {
	GeneratedAt        string
	DiskHealth         DiskHealthSection
	BackupStatus       BackupStatusSection
	AwaitingApproval   AwaitingApprovalSection
}

type DiskHealthSection struct {
	Summary string
	Details []MaintenanceStatusLine
}

type MaintenanceStatusLine struct {
	CollectionID   string
	CollectionName string
	PoolID         string
	Cursor         string
	VerifiedPct    float64
	LastRunAt      int64
	TotalFiles     int
	VerifiedFiles  int
}

type BackupStatusSection struct {
	Repos []BackupRepoStatus
}

type BackupRepoStatus struct {
	RepoID       string
	RepoName     string
	TargetType   string
	LastSnapshot *BackupSnapshotInfo
}

type BackupSnapshotInfo struct {
	SnapshotID   string
	CollectionID string
	CreatedAt    int64
}

type AwaitingApprovalSection struct {
	ApprovalCount       int
	ApprovalIDs         []string
	RecommendationCount int
	RecommendationIDs   []string
}

// reportTemplate is the embedded text template for the deterministic report.
// NO LLM calls anywhere in this package's report path.
const reportTemplate = `KelviOS System Report
Generated: {{.GeneratedAt}}

=== DISK HEALTH (Maintenance Status) ===
{{if .DiskHealth.Details}}
{{range .DiskHealth.Details}}
Collection: {{.CollectionName}} ({{.CollectionID}})
  Pool: {{.PoolID}}
  Total Files: {{.TotalFiles}}
  Verified: {{.VerifiedFiles}} ({{printf "%.1f" .VerifiedPct}}%)
  Last Scrub: {{if gt .LastRunAt 0}}{{.LastRunAt | unixTime}}{{else}}never{{end}}
  Cursor: {{if .Cursor}}{{.Cursor}}{{else}}(wrapped){{end}}
{{end}}
{{else}}
No collections found.
{{end}}

=== BACKUP STATUS ===
{{if .BackupStatus.Repos}}
{{range .BackupStatus.Repos}}
Repo: {{.RepoName}} ({{.RepoID}}) [{{.TargetType}}]
{{if .LastSnapshot}}
  Latest Snapshot: {{.LastSnapshot.SnapshotID}}
  Collection: {{.LastSnapshot.CollectionID}}
  Time: {{.LastSnapshot.CreatedAt | unixTime}}
{{else}}
  No snapshots yet.
{{end}}
{{end}}
{{else}}
No backup repositories configured.
{{end}}

=== AWAITING APPROVAL ===
FR-3.7 Approvals (pending): {{.AwaitingApproval.ApprovalCount}}
{{if .AwaitingApproval.ApprovalIDs}}
{{range .AwaitingApproval.ApprovalIDs}}
  - {{.}}
{{end}}
{{else}}
  (none)
{{end}}

Tier Migration Recommendations (pending): {{.AwaitingApproval.RecommendationCount}}
{{if .AwaitingApproval.RecommendationIDs}}
{{range .AwaitingApproval.RecommendationIDs}}
  - {{.}}
{{end}}
{{else}}
  (none)
{{end}}
`

// BuildReport builds the deterministic system report.
// NO LLM calls — pure template rendering from catalog data.
func BuildReport(db *sql.DB, now time.Time) (string, error) {
	data := ReportData{
		GeneratedAt: now.Format(time.RFC3339),
	}

	// Disk Health: use maintenance.Status
	statuses, err := maintenance.Status(db)
	if err != nil {
		return "", fmt.Errorf("maintenance status: %w", err)
	}
	data.DiskHealth.Details = make([]MaintenanceStatusLine, len(statuses))
	for i, s := range statuses {
		data.DiskHealth.Details[i] = MaintenanceStatusLine{
			CollectionID:   s.CollectionID,
			CollectionName: s.CollectionName,
			PoolID:         s.PoolID,
			Cursor:         s.Cursor,
			VerifiedPct:    s.VerifiedFraction * 100,
			LastRunAt:      s.LastRunAt,
			TotalFiles:     s.TotalFiles,
			VerifiedFiles:  s.VerifiedFiles,
		}
	}

	// Backup Status: list repos and their latest snapshot per repo
	repos, err := catalog.ListBackupRepos(db)
	if err != nil {
		return "", fmt.Errorf("list backup repos: %w", err)
	}
	data.BackupStatus.Repos = make([]BackupRepoStatus, len(repos))
	for i, repo := range repos {
		snapshots, err := catalog.ListBackupSnapshots(db, repo.ID)
		if err != nil {
			return "", fmt.Errorf("list backup snapshots for repo %s: %w", repo.ID, err)
		}
		var lastSnap *BackupSnapshotInfo
		if len(snapshots) > 0 {
			// Latest is last in the list (ordered by created_at)
			last := snapshots[len(snapshots)-1]
			lastSnap = &BackupSnapshotInfo{
				SnapshotID:   last.ResticSnapshotID,
				CollectionID: last.CollectionID,
				CreatedAt:    last.CreatedAt,
			}
		}
		data.BackupStatus.Repos[i] = BackupRepoStatus{
			RepoID:       repo.ID,
			RepoName:     repo.Name,
			TargetType:   repo.TargetType,
			LastSnapshot: lastSnap,
		}
	}

	// Awaiting Approval: FR-3.7 pending approvals + pending tier recommendations
	approvalRows, err := db.Query(`
		SELECT id FROM mcp_approvals WHERE status = 'pending' ORDER BY requested_at DESC
	`)
	if err != nil {
		return "", fmt.Errorf("query pending approvals: %w", err)
	}
	var approvalIDs []string
	for approvalRows.Next() {
		var id string
		if err := approvalRows.Scan(&id); err != nil {
			approvalRows.Close()
			return "", fmt.Errorf("scan approval id: %w", err)
		}
		approvalIDs = append(approvalIDs, id)
	}
	approvalRows.Close()
	data.AwaitingApproval.ApprovalCount = len(approvalIDs)
	data.AwaitingApproval.ApprovalIDs = approvalIDs

	// Pending tier recommendations
	recs, _, err := catalog.ListRecommendations(db, "pending", "")
	if err != nil {
		return "", fmt.Errorf("list pending recommendations: %w", err)
	}
	var recIDs []string
	for _, r := range recs {
		recIDs = append(recIDs, r.ID)
	}
	data.AwaitingApproval.RecommendationCount = len(recIDs)
	data.AwaitingApproval.RecommendationIDs = recIDs

	// Render template
	tmpl, err := template.New("report").Funcs(template.FuncMap{
		"unixTime": func(ts int64) string {
			if ts <= 0 {
				return "never"
			}
			return time.Unix(ts, 0).Format(time.RFC3339)
		},
	}).Parse(reportTemplate)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}