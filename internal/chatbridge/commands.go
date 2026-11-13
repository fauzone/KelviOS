package chatbridge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/maintenance"
)

// handleCommand routes a command message and returns the reply text.
// Returns (reply, action, detail, error) where action is the audit action name.
func (s *Service) handleCommand(ctx context.Context, link *ChatLink, text string) (string, string, string, error) {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return "", "", "", fmt.Errorf("empty command")
	}
	cmd := strings.ToLower(parts[0])

	switch cmd {
	case "/help":
		reply, _ := s.cmdHelp()
		return reply, "chat.command.help", "help", nil

	case "/status":
		reply, err := s.cmdStatus()
		return reply, "chat.command.status", "status", err

	case "/health":
		reply, err := s.cmdHealth()
		return reply, "chat.command.health", "health", err

	case "/backups":
		reply, err := s.cmdBackups()
		return reply, "chat.command.backups", "backups", err

	case "/approvals":
		reply, err := s.cmdApprovals()
		return reply, "chat.command.approvals", "approvals", err

	case "/approve":
		if len(parts) < 3 {
			return "Usage: /approve <id> <pin>", "chat.command.approve", "error: missing args", fmt.Errorf("missing args")
		}
		return s.cmdApprove(link, parts[1], parts[2])

	case "/reject":
		if len(parts) < 3 {
			return "Usage: /reject <id> <pin>", "chat.command.reject", "error: missing args", fmt.Errorf("missing args")
		}
		return s.cmdReject(link, parts[1], parts[2])

	case "/report":
		reply, err := s.cmdReport()
		return reply, "chat.command.report", "report", err

	default:
		return "Unknown command. Use /help for available commands.", "chat.command.error", "unknown: " + cmd, fmt.Errorf("unknown command")
	}
}

func (s *Service) cmdHelp() (string, error) {
	return `Available commands:
/help - Show this help
/status - Show pools and disks summary
/health - Show maintenance (disk health) status
/backups - Show backup repositories and latest snapshots
/approvals - List pending approvals and recommendations
/approve <id> <pin> - Approve a pending request (requires PIN)
/reject <id> <pin> - Reject a pending request (requires PIN)
/report - Generate full system report on demand`, nil
}

func (s *Service) cmdStatus() (string, error) {
	pools, err := catalog.ListPools(s.db)
	if err != nil {
		return "", fmt.Errorf("list pools: %w", err)
	}
	if len(pools) == 0 {
		return "No pools configured.", nil
	}

	var lines []string
	lines = append(lines, "Pools & Disks Summary:")
	for _, p := range pools {
		diskRows, err := s.db.Query(`SELECT disk_name FROM pool_disks WHERE pool_id = ?`, p.ID)
		if err != nil {
			return "", fmt.Errorf("query pool disks: %w", err)
		}
		var disks []string
		for diskRows.Next() {
			var d string
			if err := diskRows.Scan(&d); err != nil {
				diskRows.Close()
				return "", fmt.Errorf("scan disk: %w", err)
			}
			disks = append(disks, d)
		}
		diskRows.Close()

		usedPct := float64(0)
		if p.SizeBytes > 0 {
			usedPct = float64(p.UsedBytes) / float64(p.SizeBytes) * 100
		}
		lines = append(lines, fmt.Sprintf("  %s (%s) - %s - %.1f%% used (%d/%d bytes) - Disks: %v",
			p.Name, p.ID, p.Tier, usedPct, p.UsedBytes, p.SizeBytes, disks))
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) cmdHealth() (string, error) {
	statuses, err := maintenance.Status(s.db)
	if err != nil {
		return "", fmt.Errorf("maintenance status: %w", err)
	}
	if len(statuses) == 0 {
		return "No collections found.", nil
	}

	var lines []string
	lines = append(lines, "Maintenance Status (Disk Health):")
	for _, s := range statuses {
		verifiedPct := s.VerifiedFraction * 100
		lastRun := "never"
		if s.LastRunAt > 0 {
			lastRun = time.Unix(s.LastRunAt, 0).Format(time.RFC3339)
		}
		cursor := s.Cursor
		if cursor == "" {
			cursor = "(wrapped)"
		}
		lines = append(lines, fmt.Sprintf("  %s (%s) - Pool: %s - Verified: %.1f%% (%d/%d) - Last: %s - Cursor: %s",
			s.CollectionName, s.CollectionID, s.PoolID, verifiedPct, s.VerifiedFiles, s.TotalFiles, lastRun, cursor))
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) cmdBackups() (string, error) {
	repos, err := catalog.ListBackupRepos(s.db)
	if err != nil {
		return "", fmt.Errorf("list backup repos: %w", err)
	}
	if len(repos) == 0 {
		return "No backup repositories configured.", nil
	}

	var lines []string
	lines = append(lines, "Backup Repositories:")
	for _, repo := range repos {
		snapshots, err := catalog.ListBackupSnapshots(s.db, repo.ID)
		if err != nil {
			return "", fmt.Errorf("list snapshots for repo %s: %w", repo.ID, err)
		}
		lines = append(lines, fmt.Sprintf("  %s (%s) [%s]", repo.Name, repo.ID, repo.TargetType))
		if len(snapshots) > 0 {
			last := snapshots[len(snapshots)-1]
			lines = append(lines, fmt.Sprintf("    Latest: %s (collection: %s, time: %s)",
				last.ResticSnapshotID, last.CollectionID, time.Unix(last.CreatedAt, 0).Format(time.RFC3339)))
		} else {
			lines = append(lines, "    No snapshots yet.")
		}
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) cmdApprovals() (string, error) {
	var lines []string

	// FR-3.7 pending approvals
	approvalRows, err := s.db.Query(`
		SELECT id, tool, requested_by, requested_at FROM mcp_approvals
		WHERE status = 'pending' ORDER BY requested_at DESC
	`)
	if err != nil {
		return "", fmt.Errorf("query pending approvals: %w", err)
	}
	var approvals []string
	for approvalRows.Next() {
		var id, tool, requestedBy string
		var requestedAt int64
		if err := approvalRows.Scan(&id, &tool, &requestedBy, &requestedAt); err != nil {
			approvalRows.Close()
			return "", fmt.Errorf("scan approval: %w", err)
		}
		approvals = append(approvals, fmt.Sprintf("  %s - tool: %s - by: %s - at: %s",
			id, tool, requestedBy, time.Unix(requestedAt, 0).Format(time.RFC3339)))
	}
	approvalRows.Close()

	lines = append(lines, fmt.Sprintf("FR-3.7 Pending Approvals (%d):", len(approvals)))
	if len(approvals) > 0 {
		lines = append(lines, approvals...)
	} else {
		lines = append(lines, "  (none)")
	}

	// Pending tier recommendations
	recs, _, err := catalog.ListRecommendations(s.db, "pending", "")
	if err != nil {
		return "", fmt.Errorf("list pending recommendations: %w", err)
	}
	lines = append(lines, fmt.Sprintf("\nTier Migration Recommendations Pending (%d):", len(recs)))
	if len(recs) > 0 {
		for _, r := range recs {
			lines = append(lines, fmt.Sprintf("  %s - collection: %s - %s - %d bytes - reason: %s",
				r.ID, r.CollectionID, r.Direction, r.SizeBytes, r.Reason))
		}
	} else {
		lines = append(lines, "  (none)")
	}

	return strings.Join(lines, "\n"), nil
}

func (s *Service) cmdApprove(link *ChatLink, id, pin string) (string, string, string, error) {
	if s.pinTracker.IsLocked(link.ID) {
		return "Sensitive commands are locked for this chat after repeated PIN failures. Try again in 15 minutes.", "chat.command.approve", "locked", fmt.Errorf("link locked")
	}

	// Verify PIN
	if !verifyPIN(pin, link.PinHash) {
		locked := s.pinTracker.RecordFailure(link.ID)
		auditDetail := fmt.Sprintf("PIN verification failed for approve %s", id)
		if locked {
			auditDetail += " - LINK LOCKED (3 failures)"
		}
		return "Invalid PIN.", "chat.command.approve", auditDetail, fmt.Errorf("invalid PIN")
	}
	s.pinTracker.Reset(link.ID)

	// Determine if it's an FR-3.7 approval or a tier recommendation
	// Check mcp_approvals first
	var status string
	err := s.db.QueryRow(`SELECT status FROM mcp_approvals WHERE id = ?`, id).Scan(&status)
	if err == nil {
		// It's an FR-3.7 approval
		if status != "pending" {
			return fmt.Sprintf("Approval %s is not pending (status: %s).", id, status), "chat.command.approve", "not pending", fmt.Errorf("not pending")
		}
		decidedBy := link.Label
		if decidedBy == "" {
			decidedBy = fmt.Sprintf("chat:%s:%s", link.Transport, link.ChatID)
		}
		_, err = s.approvalStore.Approve(id, decidedBy)
		if err != nil {
			return fmt.Sprintf("Failed to approve: %v", err), "chat.command.approve", "error: " + err.Error(), err
		}
		// IMPORTANT: Chat-approving ONLY marks it approved. Execution still happens
		// through the original MCP tools/call carrying the approval_id.
		return fmt.Sprintf("Approval %s marked as approved. Execution will proceed via the original MCP call.", id), "chat.command.approve", "approved", nil
	}

	// Check tier_recommendations
	var recStatus string
	err = s.db.QueryRow(`SELECT status FROM tier_recommendations WHERE id = ?`, id).Scan(&recStatus)
	if err == nil {
		if recStatus != "pending" {
			return fmt.Sprintf("Recommendation %s is not pending (status: %s).", id, recStatus), "chat.command.approve", "not pending", fmt.Errorf("not pending")
		}
		err = catalog.DecideRecommendation(s.db, id, "approved", time.Now())
		if err != nil {
			return fmt.Sprintf("Failed to approve recommendation: %v", err), "chat.command.approve", "error: " + err.Error(), err
		}
		return fmt.Sprintf("Tier migration recommendation %s approved.", id), "chat.command.approve", "approved", nil
	}

	return fmt.Sprintf("Unknown approval/recommendation ID: %s", id), "chat.command.approve", "unknown id", fmt.Errorf("unknown id")
}

func (s *Service) cmdReject(link *ChatLink, id, pin string) (string, string, string, error) {
	if s.pinTracker.IsLocked(link.ID) {
		return "Sensitive commands are locked for this chat after repeated PIN failures. Try again in 15 minutes.", "chat.command.reject", "locked", fmt.Errorf("link locked")
	}

	// Verify PIN
	if !verifyPIN(pin, link.PinHash) {
		locked := s.pinTracker.RecordFailure(link.ID)
		auditDetail := fmt.Sprintf("PIN verification failed for reject %s", id)
		if locked {
			auditDetail += " - LINK LOCKED (3 failures)"
		}
		return "Invalid PIN.", "chat.command.reject", auditDetail, fmt.Errorf("invalid PIN")
	}
	s.pinTracker.Reset(link.ID)

	// Check mcp_approvals first
	var status string
	err := s.db.QueryRow(`SELECT status FROM mcp_approvals WHERE id = ?`, id).Scan(&status)
	if err == nil {
		if status != "pending" {
			return fmt.Sprintf("Approval %s is not pending (status: %s).", id, status), "chat.command.reject", "not pending", fmt.Errorf("not pending")
		}
		decidedBy := link.Label
		if decidedBy == "" {
			decidedBy = fmt.Sprintf("chat:%s:%s", link.Transport, link.ChatID)
		}
		_, err = s.approvalStore.Reject(id, decidedBy)
		if err != nil {
			return fmt.Sprintf("Failed to reject: %v", err), "chat.command.reject", "error: " + err.Error(), err
		}
		return fmt.Sprintf("Approval %s rejected.", id), "chat.command.reject", "rejected", nil
	}

	// Check tier_recommendations
	var recStatus string
	err = s.db.QueryRow(`SELECT status FROM tier_recommendations WHERE id = ?`, id).Scan(&recStatus)
	if err == nil {
		if recStatus != "pending" {
			return fmt.Sprintf("Recommendation %s is not pending (status: %s).", id, recStatus), "chat.command.reject", "not pending", fmt.Errorf("not pending")
		}
		err = catalog.DecideRecommendation(s.db, id, "rejected", time.Now())
		if err != nil {
			return fmt.Sprintf("Failed to reject recommendation: %v", err), "chat.command.reject", "error: " + err.Error(), err
		}
		return fmt.Sprintf("Tier migration recommendation %s rejected.", id), "chat.command.reject", "rejected", nil
	}

	return fmt.Sprintf("Unknown approval/recommendation ID: %s", id), "chat.command.reject", "unknown id", fmt.Errorf("unknown id")
}

func (s *Service) cmdReport() (string, error) {
	report, err := BuildReport(s.db, time.Now())
	if err != nil {
		return "", fmt.Errorf("build report: %w", err)
	}
	return report, nil
}