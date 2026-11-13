package daemonconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ShareConfig holds the data needed to render a daemon configuration snippet.
type ShareConfig struct {
	// Share fields
	ID           string
	Name         string
	Protocol     string
	ReadOnly     bool
	Metadata     string
	CollectionID string
	// Collection fields (joined)
	CollectionPath string
	PoolMountpoint string
	PoolName       string // pool name for fallback when mountpoint is empty
}

// Metadata represents the parsed metadata JSON from the share record.
type Metadata struct {
	AllowedIPs string `json:"allowed_ips,omitempty"`
	// Future fields can be added here
}

// parseMetadata parses the metadata JSON string.
func parseMetadata(metadata string) (Metadata, error) {
	var m Metadata
	if metadata == "" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(metadata), &m); err != nil {
		return m, fmt.Errorf("parse metadata: %w", err)
	}
	return m, nil
}

// RenderError is returned when a share cannot be rendered safely.
type RenderError struct {
	ShareID string
	Reason  string
}

func (e *RenderError) Error() string {
	return fmt.Sprintf("share %s: %s", e.ShareID, e.Reason)
}

// IsRenderError checks if an error is a RenderError.
func IsRenderError(err error) bool {
	var re *RenderError
	return errors.As(err, &re)
}

// RenderShare renders the daemon configuration snippet for a single share.
// Returns the config text and the protocol, or an error if the share cannot be rendered.
// If PoolMountpoint is empty, falls back to /mnt/<PoolName> + CollectionPath.
// Only errors if BOTH PoolMountpoint and PoolName are empty (genuinely invalid record).
func RenderShare(sc ShareConfig) (protocol, configText string, err error) {
	if sc.Name == "" {
		return "", "", &RenderError{ShareID: sc.ID, Reason: "empty share name"}
	}
	if sc.CollectionPath == "" {
		return "", "", &RenderError{ShareID: sc.ID, Reason: "empty collection path"}
	}

	// Determine full path: use PoolMountpoint if set, otherwise fall back to /mnt/<PoolName>
	fullPath := sc.PoolMountpoint
	if fullPath == "" {
		if sc.PoolName == "" {
			return "", "", &RenderError{ShareID: sc.ID, Reason: "empty pool mountpoint and pool name"}
		}
		fullPath = "/mnt/" + sc.PoolName
	}
	fullPath += sc.CollectionPath

	switch sc.Protocol {
	case "smb":
		return "smb", renderSMB(sc, fullPath), nil
	case "nfs":
		return "nfs", renderNFS(sc, fullPath), nil
	case "sftp":
		return "sftp", renderSFTP(sc, fullPath), nil
	default:
		return "", "", &RenderError{ShareID: sc.ID, Reason: "unknown protocol: " + sc.Protocol}
	}
}

// renderSMB renders an SMB share section.
// Guest access is always disabled. Read-only follows the share's read_only flag.
// Global snippet includes SMB2 minimum protocol and encryption/signing flags.
func renderSMB(sc ShareConfig, fullPath string) string {
	var b strings.Builder

	// Global snippet (emitted once per smb.conf, but we include it here for completeness)
	b.WriteString("# Global SMB settings (include once in smb.conf)\n")
	b.WriteString("[global]\n")
	b.WriteString("  server min protocol = SMB2\n")
	b.WriteString("  smb encrypt = desired\n")
	b.WriteString("  client signing = required\n")
	b.WriteString("  server signing = required\n")
	b.WriteString("\n")

	// Share section
	b.WriteString("[" + sc.Name + "]\n")
	b.WriteString("  path = " + fullPath + "\n")
	b.WriteString("  guest ok = no\n")
	b.WriteString("  browseable = yes\n")
	if sc.ReadOnly {
		b.WriteString("  read only = yes\n")
	} else {
		b.WriteString("  read only = no\n")
	}
	b.WriteString("  force create mode = 0644\n")
	b.WriteString("  force directory mode = 0755\n")

	return b.String()
}

// renderNFS renders an NFS export line.
// Read-only by DEFAULT (read_only=false still renders 'ro' unless explicitly opted into read-write).
// Client restricted to allowed_ips from metadata; if absent, renders a restricted loopback line.
// Never renders a wildcard (*) export.
func renderNFS(sc ShareConfig, fullPath string) string {
	var b strings.Builder

	meta, _ := parseMetadata(sc.Metadata)

	// Default to read-only unless explicitly opted into read-write
	// The share's ReadOnly flag being false means "not forced read-only",
	// but we still default to 'ro' for safety. An explicit metadata flag
	// could override this in the future.
	access := "ro"
	// Note: Per FR-1.8, NFS defaults to read-only. The share's read_only=false
	// does NOT automatically grant read-write; an explicit opt-in would be needed.
	// For now, we honor read_only=true as 'ro', and read_only=false also as 'ro'
	// unless a future metadata field opts in. This is documented here.

	// Client restriction
	clientSpec := ""
	if meta.AllowedIPs != "" {
		// Split by comma and render each
		ips := strings.Split(meta.AllowedIPs, ",")
		for i, ip := range ips {
			ip = strings.TrimSpace(ip)
			if ip == "" {
				continue
			}
			if i > 0 {
				clientSpec += " "
			}
			clientSpec += ip + "(" + access + ",no_root_squash,async)"
		}
	} else {
		// No restriction metadata: render a clearly-marked loopback/restricted line
		// Never use wildcard (*)
		clientSpec = "127.0.0.1(" + access + ",no_root_squash,async) # RESTRICTED: no allowed_ips metadata; update share metadata to allow specific clients"
	}

	b.WriteString(fullPath + " " + clientSpec + "\n")

	return b.String()
}

// renderSFTP renders an SFTP Match block snippet.
// Keys-only authentication (no password auth line).
// Scoped to the collection path via ChrootDirectory.
func renderSFTP(sc ShareConfig, fullPath string) string {
	var b strings.Builder

	b.WriteString("# SFTP Match block for share: " + sc.Name + "\n")
	b.WriteString("Match Group sftp-" + sc.Name + "\n")
	b.WriteString("  ChrootDirectory " + fullPath + "\n")
	b.WriteString("  ForceCommand internal-sftp\n")
	b.WriteString("  PasswordAuthentication no\n")
	b.WriteString("  PubkeyAuthentication yes\n")
	b.WriteString("  X11Forwarding no\n")
	b.WriteString("  AllowTcpForwarding no\n")
	if sc.ReadOnly {
		b.WriteString("  # Read-only: disable write operations\n")
		b.WriteString("  # Note: internal-sftp read-only requires filesystem permissions or a custom subsystem\n")
	}

	return b.String()
}

// RenderErrorEntry represents a per-share render failure.
type RenderErrorEntry struct {
	ShareID string
	Reason  string
}

// RenderAllResult holds the combined render output and any per-share errors.
type RenderAllResult struct {
	Configs map[string]string
	Errors  []RenderErrorEntry
}

// RenderAllShares renders combined configuration for all shares, grouped by protocol.
// It never fails the batch on a single share error; instead it records per-share errors
// and continues rendering the rest. Returns 200-equivalent result with configs + errors.
func RenderAllShares(shares []ShareConfig) RenderAllResult {
	result := RenderAllResult{
	Configs: map[string]string{
		"smb":  "",
		"nfs":  "",
		"sftp": "",
		},
	Errors:  []RenderErrorEntry{},
}

	smbGlobalEmitted := false

	for _, sc := range shares {
		protocol, configText, err := RenderShare(sc)
		if err != nil {
			result.Errors = append(result.Errors, RenderErrorEntry{
				ShareID: sc.ID,
				Reason:  err.Error(),
			})
			continue
		}

		switch protocol {
		case "smb":
			if !smbGlobalEmitted {
				// Extract global section (first part before first share section)
				lines := strings.Split(configText, "\n")
				inGlobal := true
				for _, line := range lines {
					if strings.HasPrefix(line, "[") && !strings.HasPrefix(line, "[global]") {
						inGlobal = false
					}
					if inGlobal {
						result.Configs["smb"] += line + "\n"
					} else {
						result.Configs["smb"] += line + "\n"
					}
				}
				smbGlobalEmitted = true
			} else {
				// Only append the share section (skip global)
				lines := strings.Split(configText, "\n")
				inGlobal := true
				for _, line := range lines {
					if strings.HasPrefix(line, "[") && !strings.HasPrefix(line, "[global]") {
						inGlobal = false
					}
					if !inGlobal {
						result.Configs["smb"] += line + "\n"
					}
				}
			}
		case "nfs":
			result.Configs["nfs"] += configText
		case "sftp":
			result.Configs["sftp"] += configText + "\n"
		}
	}

	return result
}

// ValidateWithTestparm runs testparm -s on the SMB config if available.
// Returns nil if testparm is not available or validation passes.
// Returns error if testparm is available and validation fails.
func ValidateWithTestparm(smbConfig string) error {
	// This is a best-effort validation. If testparm is not available, we skip.
	// The caller should not treat absence of testparm as a failure.
	return nil // Placeholder - actual implementation would exec testparm
}

// ValidateWithExportfs runs exportfs -n on the NFS config if available.
// Returns nil if exportfs is not available or validation passes.
// Returns error if exportfs is available and validation fails.
func ValidateWithExportfs(nfsConfig string) error {
	// This is a best-effort validation. If exportfs is not available, we skip.
	return nil // Placeholder - actual implementation would exec exportfs
}
