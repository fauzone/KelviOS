package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ResticSnapshot represents a restic snapshot.
type ResticSnapshot struct {
	ID        string    `json:"id"`
	Time      time.Time `json:"time"`
	Paths     []string  `json:"paths"`
	Hostname  string    `json:"hostname"`
	Username  string    `json:"username"`
	Tags      []string  `json:"tags"`
	ShortID   string    `json:"short_id"`
}

// ResticWrapper wraps restic CLI operations.
type ResticWrapper struct {
	BinaryPath string
}

// NewResticWrapper creates a new ResticWrapper.
// Binary path comes from KELVIOS_RESTIC_BIN env var, defaulting to "restic".
func NewResticWrapper() *ResticWrapper {
	bin := os.Getenv("KELVIOS_RESTIC_BIN")
	if bin == "" {
		bin = "restic"
	}
	return &ResticWrapper{BinaryPath: bin}
}

// InitRepo initializes a restic repository at repoPath with the given password.
// Returns error if repo already exists or initialization fails.
func (w *ResticWrapper) InitRepo(repoPath, password string) error {
	return w.InitRepoWithEnv(repoPath, password, nil)
}

// InitRepoWithEnv initializes a restic repository with extra environment variables.
func (w *ResticWrapper) InitRepoWithEnv(repoPath, password string, extraEnv map[string]string) error {
	if _, err := os.Stat(repoPath); err == nil {
		return fmt.Errorf("repository already exists at %s", repoPath)
	}

	cmd := exec.Command(w.BinaryPath, "init", "--repo", repoPath)
	env := append(os.Environ(), "RESTIC_PASSWORD="+password)
	if extraEnv != nil {
		for k, v := range extraEnv {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restic init failed: %w: %s", err, string(output))
	}
	return nil
}

// Backup backs up sourceRoot to the repository at repoPath.
// Returns the snapshot ID on success.
func (w *ResticWrapper) Backup(repoPath, sourceRoot, password string) (string, error) {
	return w.BackupWithEnv(repoPath, sourceRoot, password, nil)
}

// BackupWithEnv backs up sourceRoot with extra environment variables.
func (w *ResticWrapper) BackupWithEnv(repoPath, sourceRoot, password string, extraEnv map[string]string) (string, error) {
	cmd := exec.Command(w.BinaryPath, "backup", sourceRoot, "--repo", repoPath, "--json")
	env := append(os.Environ(), "RESTIC_PASSWORD="+password)
	if extraEnv != nil {
		for k, v := range extraEnv {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("restic backup failed: %w: %s", err, string(output))
	}

	// Parse JSON output to get snapshot ID
	var result struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		// Fallback: try to extract from last line
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.Contains(lines[i], "snapshot_id") {
				var r struct {
					SnapshotID string `json:"snapshot_id"`
				}
				if json.Unmarshal([]byte(lines[i]), &r) == nil && r.SnapshotID != "" {
					return r.SnapshotID, nil
				}
			}
		}
		return "", fmt.Errorf("failed to parse snapshot ID from restic output: %s", string(output))
	}
	if result.SnapshotID == "" {
		return "", fmt.Errorf("empty snapshot ID from restic backup")
	}
	return result.SnapshotID, nil
}

// ListSnapshots returns all snapshots in the repository.
func (w *ResticWrapper) ListSnapshots(repoPath, password string) ([]ResticSnapshot, error) {
	return w.ListSnapshotsWithEnv(repoPath, password, nil)
}

// ListSnapshotsWithEnv returns all snapshots with extra environment variables.
func (w *ResticWrapper) ListSnapshotsWithEnv(repoPath, password string, extraEnv map[string]string) ([]ResticSnapshot, error) {
	cmd := exec.Command(w.BinaryPath, "snapshots", "--repo", repoPath, "--json")
	env := append(os.Environ(), "RESTIC_PASSWORD="+password)
	if extraEnv != nil {
		for k, v := range extraEnv {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("restic snapshots failed: %w: %s", err, string(output))
	}

	var snapshots []ResticSnapshot
	if err := json.Unmarshal(output, &snapshots); err != nil {
		return nil, fmt.Errorf("failed to parse snapshots: %w: %s", err, string(output))
	}
	return snapshots, nil
}

// RestoreFile restores a single file from a snapshot to destDir.
// relPath is the relative path within the snapshot.
// Returns the absolute path of the restored file.
func (w *ResticWrapper) RestoreFile(repoPath, snapshotID, relPath, destDir, password string) (string, error) {
	return w.RestoreFileWithEnv(repoPath, snapshotID, relPath, destDir, password, nil)
}

// RestoreFileWithEnv restores a single file with extra environment variables.
func (w *ResticWrapper) RestoreFileWithEnv(repoPath, snapshotID, relPath, destDir, password string, extraEnv map[string]string) (string, error) {
	// Ensure destDir exists
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir dest: %w", err)
	}

	// restic restore --target destDir --include relPath snapshotID
	cmd := exec.Command(w.BinaryPath, "restore", snapshotID, "--repo", repoPath, "--target", destDir, "--include", relPath)
	env := append(os.Environ(), "RESTIC_PASSWORD="+password)
	if extraEnv != nil {
		for k, v := range extraEnv {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("restic restore failed: %w: %s", err, string(output))
	}

	// The restored file will be at destDir/relPath
	restoredPath := filepath.Join(destDir, relPath)
	if _, err := os.Stat(restoredPath); err != nil {
		return "", fmt.Errorf("restored file not found at %s: %w", restoredPath, err)
	}
	return restoredPath, nil
}

// RepoExists checks if a restic repository exists at the given path.
func (w *ResticWrapper) RepoExists(repoPath string) bool {
	_, err := os.Stat(filepath.Join(repoPath, "config"))
	return err == nil
}

// ValidatePassword checks if the password can open the repository.
func (w *ResticWrapper) ValidatePassword(repoPath, password string) error {
	return w.ValidatePasswordWithEnv(repoPath, password, nil)
}

// ValidatePasswordWithEnv checks password with extra environment variables.
func (w *ResticWrapper) ValidatePasswordWithEnv(repoPath, password string, extraEnv map[string]string) error {
	cmd := exec.Command(w.BinaryPath, "snapshots", "--repo", repoPath)
	env := append(os.Environ(), "RESTIC_PASSWORD="+password)
	if extraEnv != nil {
		for k, v := range extraEnv {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("invalid password or repository: %s", string(output))
	}
	return nil
}

// Context-aware versions for tests that need cancellation.

// BackupWithContext backs up sourceRoot to the repository with context cancellation.
func (w *ResticWrapper) BackupWithContext(ctx context.Context, repoPath, sourceRoot, password string) (string, error) {
	return w.BackupWithContextAndEnv(ctx, repoPath, sourceRoot, password, nil)
}

// BackupWithContextAndEnv backs up with context and extra environment variables.
func (w *ResticWrapper) BackupWithContextAndEnv(ctx context.Context, repoPath, sourceRoot, password string, extraEnv map[string]string) (string, error) {
	cmd := exec.CommandContext(ctx, w.BinaryPath, "backup", sourceRoot, "--repo", repoPath, "--json")
	env := append(os.Environ(), "RESTIC_PASSWORD="+password)
	if extraEnv != nil {
		for k, v := range extraEnv {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("restic backup failed: %w: %s", err, string(output))
	}

	var result struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		lines := strings.Split(strings.TrimSpace(string(output)), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.Contains(lines[i], "snapshot_id") {
				var r struct {
					SnapshotID string `json:"snapshot_id"`
				}
				if json.Unmarshal([]byte(lines[i]), &r) == nil && r.SnapshotID != "" {
					return r.SnapshotID, nil
				}
			}
		}
		return "", fmt.Errorf("failed to parse snapshot ID from restic output: %s", string(output))
	}
	if result.SnapshotID == "" {
		return "", fmt.Errorf("empty snapshot ID from restic backup")
	}
	return result.SnapshotID, nil
}

// ListSnapshotsWithContext returns all snapshots with context cancellation.
func (w *ResticWrapper) ListSnapshotsWithContext(ctx context.Context, repoPath, password string) ([]ResticSnapshot, error) {
	return w.ListSnapshotsWithContextAndEnv(ctx, repoPath, password, nil)
}

// ListSnapshotsWithContextAndEnv returns all snapshots with context and extra env.
func (w *ResticWrapper) ListSnapshotsWithContextAndEnv(ctx context.Context, repoPath, password string, extraEnv map[string]string) ([]ResticSnapshot, error) {
	cmd := exec.CommandContext(ctx, w.BinaryPath, "snapshots", "--repo", repoPath, "--json")
	env := append(os.Environ(), "RESTIC_PASSWORD="+password)
	if extraEnv != nil {
		for k, v := range extraEnv {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("restic snapshots failed: %w: %s", err, string(output))
	}

	var snapshots []ResticSnapshot
	if err := json.Unmarshal(output, &snapshots); err != nil {
		return nil, fmt.Errorf("failed to parse snapshots: %w: %s", err, string(output))
	}
	return snapshots, nil
}

// RestoreFileWithContext restores a single file with context cancellation.
func (w *ResticWrapper) RestoreFileWithContext(ctx context.Context, repoPath, snapshotID, relPath, destDir, password string) (string, error) {
	return w.RestoreFileWithContextAndEnv(ctx, repoPath, snapshotID, relPath, destDir, password, nil)
}

// RestoreFileWithContextAndEnv restores with context and extra environment variables.
func (w *ResticWrapper) RestoreFileWithContextAndEnv(ctx context.Context, repoPath, snapshotID, relPath, destDir, password string, extraEnv map[string]string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir dest: %w", err)
	}

	cmd := exec.CommandContext(ctx, w.BinaryPath, "restore", snapshotID, "--repo", repoPath, "--target", destDir, "--include", relPath)
	env := append(os.Environ(), "RESTIC_PASSWORD="+password)
	if extraEnv != nil {
		for k, v := range extraEnv {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("restic restore failed: %w: %s", err, string(output))
	}

	restoredPath := filepath.Join(destDir, relPath)
	if _, err := os.Stat(restoredPath); err != nil {
		return "", fmt.Errorf("restored file not found at %s: %w", restoredPath, err)
	}
	return restoredPath, nil
}