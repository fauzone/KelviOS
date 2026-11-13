package backup

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestResticWrapperInitRepo tests repository initialization.
func TestResticWrapperInitRepo(t *testing.T) {
	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "repo")
	password := "test-password-123"

	// Create a stub restic script
	stubPath := createResticStub(t, tmpDir)
	w := &ResticWrapper{BinaryPath: stubPath}

	// Init repo
	err := w.InitRepo(repoPath, password)
	if err != nil {
		t.Fatalf("InitRepo failed: %v", err)
	}

	// Verify config file exists
	configPath := filepath.Join(repoPath, "config")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Error("config file not created")
	}

	// Second init should fail
	err = w.InitRepo(repoPath, password)
	if err == nil {
		t.Error("expected error on second init, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %v", err)
	}
}

// TestResticWrapperBackup tests backup and snapshot ID extraction.
func TestResticWrapperBackup(t *testing.T) {
	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "repo")
	sourceRoot := filepath.Join(tmpDir, "source")
	password := "test-password-123"

	// Create source files
	if err := os.MkdirAll(filepath.Join(sourceRoot, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "a.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "sub", "b.txt"), []byte("nested file"), 0o644); err != nil {
		t.Fatalf("write sub/b.txt: %v", err)
	}

	stubPath := createResticStub(t, tmpDir)
	w := &ResticWrapper{BinaryPath: stubPath}

	// Init repo first
	if err := w.InitRepo(repoPath, password); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	// Backup
	snapshotID, err := w.Backup(repoPath, sourceRoot, password)
	if err != nil {
		t.Fatalf("Backup failed: %v", err)
	}
	if snapshotID == "" {
		t.Error("empty snapshot ID")
	}
	if len(snapshotID) < 8 {
		t.Errorf("snapshot ID too short: %s", snapshotID)
	}
}

// TestResticWrapperListSnapshots tests listing snapshots.
func TestResticWrapperListSnapshots(t *testing.T) {
	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "repo")
	sourceRoot := filepath.Join(tmpDir, "source")
	password := "test-password-123"

	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	stubPath := createResticStub(t, tmpDir)
	w := &ResticWrapper{BinaryPath: stubPath}

	if err := w.InitRepo(repoPath, password); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	// First backup
	snap1, err := w.Backup(repoPath, sourceRoot, password)
	if err != nil {
		t.Fatalf("Backup 1: %v", err)
	}

	// Second backup (modify file)
	if err := os.WriteFile(filepath.Join(sourceRoot, "a.txt"), []byte("hello modified"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	snap2, err := w.Backup(repoPath, sourceRoot, password)
	if err != nil {
		t.Fatalf("Backup 2: %v", err)
	}

	// List snapshots
	snapshots, err := w.ListSnapshots(repoPath, password)
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snapshots) != 2 {
		t.Errorf("expected 2 snapshots, got %d", len(snapshots))
	}

	// Verify snapshot IDs match
	found1, found2 := false, false
	for _, s := range snapshots {
		if s.ID == snap1 {
			found1 = true
		}
		if s.ID == snap2 {
			found2 = true
		}
	}
	if !found1 || !found2 {
		t.Errorf("snapshot IDs not found in list: snap1=%s found=%v, snap2=%s found=%v", snap1, found1, snap2, found2)
	}
}

// TestResticWrapperRestoreFile tests restoring a single file.
func TestResticWrapperRestoreFile(t *testing.T) {
	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "repo")
	sourceRoot := filepath.Join(tmpDir, "source")
	destDir := filepath.Join(tmpDir, "restore")
	password := "test-password-123"

	if err := os.MkdirAll(filepath.Join(sourceRoot, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "a.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "sub", "b.txt"), []byte("nested file"), 0o644); err != nil {
		t.Fatalf("write sub/b.txt: %v", err)
	}

	stubPath := createResticStub(t, tmpDir)
	w := &ResticWrapper{BinaryPath: stubPath}

	if err := w.InitRepo(repoPath, password); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	snapshotID, err := w.Backup(repoPath, sourceRoot, password)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Restore a.txt
	restoredPath, err := w.RestoreFile(repoPath, snapshotID, "a.txt", destDir, password)
	if err != nil {
		t.Fatalf("RestoreFile: %v", err)
	}

	// Verify restored content
	content, err := os.ReadFile(restoredPath)
	if err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if string(content) != "hello world" {
		t.Errorf("restored content mismatch: got %q, want %q", string(content), "hello world")
	}

	// Restore nested file
	restoredPath2, err := w.RestoreFile(repoPath, snapshotID, "sub/b.txt", destDir, password)
	if err != nil {
		t.Fatalf("RestoreFile nested: %v", err)
	}
	content2, err := os.ReadFile(restoredPath2)
	if err != nil {
		t.Fatalf("read restored nested: %v", err)
	}
	if string(content2) != "nested file" {
		t.Errorf("restored nested content mismatch: got %q, want %q", string(content2), "nested file")
	}
}

// TestResticWrapperValidatePassword tests password validation.
func TestResticWrapperValidatePassword(t *testing.T) {
	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "repo")
	password := "test-password-123"
	wrongPassword := "wrong-password"

	stubPath := createResticStub(t, tmpDir)
	w := &ResticWrapper{BinaryPath: stubPath}

	if err := w.InitRepo(repoPath, password); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	// Correct password should work
	if err := w.ValidatePassword(repoPath, password); err != nil {
		t.Errorf("ValidatePassword with correct password failed: %v", err)
	}

	// Wrong password should fail
	err := w.ValidatePassword(repoPath, wrongPassword)
	if err == nil {
		t.Error("expected error with wrong password, got nil")
	}
}

// TestResticWrapperContextCancellation tests context cancellation.
func TestResticWrapperContextCancellation(t *testing.T) {
	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "repo")
	sourceRoot := filepath.Join(tmpDir, "source")
	password := "test-password-123"

	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	stubPath := createResticStub(t, tmpDir)
	w := &ResticWrapper{BinaryPath: stubPath}

	if err := w.InitRepo(repoPath, password); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	// Test BackupWithContext with cancelled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately
	_, err := w.BackupWithContext(ctx, repoPath, sourceRoot, password)
	if err == nil {
		t.Error("expected error from cancelled context, got nil")
	}

	// Test ListSnapshotsWithContext with cancelled context
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_, err = w.ListSnapshotsWithContext(ctx2, repoPath, password)
	if err == nil {
		t.Error("expected error from cancelled context, got nil")
	}

	// Test RestoreFileWithContext with cancelled context
	ctx3, cancel3 := context.WithCancel(context.Background())
	cancel3()
	_, err = w.RestoreFileWithContext(ctx3, repoPath, "snap1", "a.txt", tmpDir, password)
	if err == nil {
		t.Error("expected error from cancelled context, got nil")
	}
}

// TestResticWrapperRepoExists tests RepoExists.
func TestResticWrapperRepoExists(t *testing.T) {
	tmpDir := t.TempDir()
	repoPath := filepath.Join(tmpDir, "repo")
	password := "test-password-123"

	stubPath := createResticStub(t, tmpDir)
	w := &ResticWrapper{BinaryPath: stubPath}

	// Before init
	if w.RepoExists(repoPath) {
		t.Error("RepoExists should be false before init")
	}

	if err := w.InitRepo(repoPath, password); err != nil {
		t.Fatalf("InitRepo: %v", err)
	}

	// After init
	if !w.RepoExists(repoPath) {
		t.Error("RepoExists should be true after init")
	}
}

// createResticStub creates a Go-based stub restic binary for testing.
// The stub simulates restic init, backup, snapshots, and restore commands.
// It enforces password validation: snapshots/backup/restore commands fail if
// RESTIC_PASSWORD doesn't match the password used during init.
func createResticStub(t *testing.T, tmpDir string) string {
	stubSrc := filepath.Join(tmpDir, "restic_stub.go")
	stubBin := filepath.Join(tmpDir, "restic-stub")
	if runtime.GOOS == "windows" {
		stubBin += ".exe"
	}

	// Go source for the stub - using a regular string to avoid backtick issues
	// Backup output uses "snapshot_id", snapshots output uses "id" (per ResticSnapshot)
	src := "package main\n\nimport (\n\t\"encoding/json\"\n\t\"fmt\"\n\t\"os\"\n\t\"path/filepath\"\n\t\"strings\"\n\t\"time\"\n)\n\n// BackupSnapshot is the output format for backup command (uses snapshot_id)\n// BackupSnapshot is the output format for backup command (uses snapshot_id)\ntype BackupSnapshot struct {\n\tSnapshotID string   `json:\"snapshot_id\"`\n\tTime       string   `json:\"time\"`\n\tPaths      []string `json:\"paths\"`\n\tHostname   string   `json:\"hostname\"`\n\tUsername   string   `json:\"username\"`\n\tTags       []string `json:\"tags\"`\n\tShortID    string   `json:\"short_id\"`\n}\n\n// ListSnapshot is the output format for snapshots command (uses id)\ntype ListSnapshot struct {\n\tID        string   `json:\"id\"`\n\tTime      string   `json:\"time\"`\n\tPaths     []string `json:\"paths\"`\n\tHostname  string   `json:\"hostname\"`\n\tUsername  string   `json:\"username\"`\n\tTags      []string `json:\"tags\"`\n\tShortID   string   `json:\"short_id\"`\n}\n\nfunc main() {\n\tif len(os.Args) < 2 {\n\t\tfmt.Fprintln(os.Stderr, \"ERROR: no command provided\")\n\t\tos.Exit(1)\n\t}\n\n\tcommand := os.Args[1]\n\targs := os.Args[2:]\n\trepoPath := \"\"\n\tpassword := os.Getenv(\"RESTIC_PASSWORD\")\n\n\t// Parse --repo flag\n\tfor i := 0; i < len(args); i++ {\n\t\tif args[i] == \"--repo\" && i+1 < len(args) {\n\t\t\trepoPath = args[i+1]\n\t\t\tbreak\n\t\t}\n\t}\n\n\tswitch command {\n\tcase \"init\":\n\t\tif repoPath == \"\" {\n\t\t\tfmt.Fprintln(os.Stderr, \"ERROR: --repo required for init\")\n\t\t\tos.Exit(1)\n\t\t}\n\t\tconfigPath := filepath.Join(repoPath, \"config\")\n\t\tif _, err := os.Stat(configPath); err == nil {\n\t\t\tfmt.Fprintf(os.Stderr, \"ERROR: repository already exists at %s\\n\", repoPath)\n\t\t\tos.Exit(1)\n\t\t}\n\t\tif err := os.MkdirAll(repoPath, 0o755); err != nil {\n\t\t\tfmt.Fprintf(os.Stderr, \"ERROR: mkdir failed: %v\\n\", err)\n\t\t\tos.Exit(1)\n\t\t}\n\t\tconfig := \"{\\\"version\\\":1,\\\"id\\\":\\\"test-repo-id\\\",\\\"chunker_polynomial\\\":\\\"...\\\"}\"\n\t\tif err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {\n\t\t\tfmt.Fprintf(os.Stderr, \"ERROR: write config failed: %v\\n\", err)\n\t\t\tos.Exit(1)\n\t\t}\n\t\t// Store password for validation\n\t\tpasswordPath := filepath.Join(repoPath, \".password\")\n\t\tif err := os.WriteFile(passwordPath, []byte(password), 0o600); err != nil {\n\t\t\tfmt.Fprintf(os.Stderr, \"ERROR: write password failed: %v\\n\", err)\n\t\t\tos.Exit(1)\n\t\t}\n\n\tcase \"backup\":\n\t\tif repoPath == \"\" {\n\t\t\tfmt.Fprintln(os.Stderr, \"ERROR: --repo required for backup\")\n\t\t\tos.Exit(1)\n\t\t}\n\t\t// Validate password\n\t\tif !validatePassword(repoPath, password) {\n\t\t\tfmt.Fprintln(os.Stderr, \"ERROR: invalid password\")\n\t\t\tos.Exit(1)\n\t\t}\n\t\t// Find source path (first non-flag arg)\n\t\tsourcePath := \".\"\n\t\tfor _, arg := range args {\n\t\t\tif arg != \"--repo\" && arg != \"--json\" && !strings.HasPrefix(arg, \"--\") {\n\t\t\t\tsourcePath = arg\n\t\t\t\tbreak\n\t\t\t}\n\t\t}\n\t\tsnapID := fmt.Sprintf(\"snap-%d-%d\", time.Now().UnixNano(), os.Getpid())\n\t\tsnapshot := BackupSnapshot{\n\t\t\tSnapshotID: snapID,\n\t\t\tTime:       \"2024-01-01T00:00:00Z\",\n\t\t\tPaths:      []string{sourcePath},\n\t\t\tHostname:   \"test\",\n\t\t\tUsername:   \"test\",\n\t\t\tTags:       []string{},\n\t\t\tShortID:    snapID[:8],\n\t\t}\n\t\toutput, _ := json.Marshal(snapshot)\n\t\tfmt.Println(string(output))\n\t\t// Store snapshot metadata\n\t\tsnapshotsDir := filepath.Join(repoPath, \"snapshots\")\n\t\tif err := os.MkdirAll(snapshotsDir, 0o755); err != nil {\n\t\t\tfmt.Fprintf(os.Stderr, \"ERROR: mkdir snapshots failed: %v\\n\", err)\n\t\t\tos.Exit(1)\n\t\t}\n\t\tsnapMetaPath := filepath.Join(snapshotsDir, snapID+\".txt\")\n\t\tif err := os.WriteFile(snapMetaPath, []byte(sourcePath), 0o644); err != nil {\n\t\t\tfmt.Fprintf(os.Stderr, \"ERROR: write snapshot meta failed: %v\\n\", err)\n\t\t\tos.Exit(1)\n\t\t}\n\n\tcase \"snapshots\":\n\t\tif repoPath == \"\" {\n\t\t\tfmt.Fprintln(os.Stderr, \"ERROR: --repo required for snapshots\")\n\t\t\tos.Exit(1)\n\t\t}\n\t\t// Validate password\n\t\tif !validatePassword(repoPath, password) {\n\t\t\tfmt.Fprintln(os.Stderr, \"ERROR: invalid password\")\n\t\t\tos.Exit(1)\n\t\t}\n\t\tsnapshotsDir := filepath.Join(repoPath, \"snapshots\")\n\t\tentries, err := os.ReadDir(snapshotsDir)\n\t\tif err != nil {\n\t\t\t// No snapshots dir = empty list\n\t\t\tfmt.Println(\"[]\")\n\t\t\treturn\n\t\t}\n\t\tvar snapshots []ListSnapshot\n\t\tfor _, entry := range entries {\n\t\t\tif strings.HasSuffix(entry.Name(), \".txt\") {\n\t\t\t\tsnapID := strings.TrimSuffix(entry.Name(), \".txt\")\n\t\t\t\tmetaPath := filepath.Join(snapshotsDir, entry.Name())\n\t\t\t\tsourcePath, _ := os.ReadFile(metaPath)\n\t\t\t\tsnapshots = append(snapshots, ListSnapshot{\n\t\t\t\t\tID:        snapID,\n\t\t\t\t\tTime:      \"2024-01-01T00:00:00Z\",\n\t\t\t\t\tPaths:     []string{string(sourcePath)},\n\t\t\t\t\tHostname:  \"test\",\n\t\t\t\t\tUsername:  \"test\",\n\t\t\t\t\tTags:      []string{},\n\t\t\t\t\tShortID:   snapID[:8],\n\t\t\t\t})\n\t\t\t}\n\t\t}\n\t\toutput, _ := json.Marshal(snapshots)\n\t\tfmt.Println(string(output))\n\n\tcase \"restore\":\n\t\tif len(args) < 1 {\n\t\t\tfmt.Fprintln(os.Stderr, \"ERROR: snapshot ID required for restore\")\n\t\t\tos.Exit(1)\n\t\t}\n\t\tsnapshotID := args[0]\n\t\targs = args[1:]\n\n\t\ttargetPath := \"\"\n\t\tincludePath := \"\"\n\t\tfor i := 0; i < len(args); i++ {\n\t\t\tswitch args[i] {\n\t\t\tcase \"--repo\":\n\t\t\t\tif i+1 < len(args) {\n\t\t\t\t\trepoPath = args[i+1]\n\t\t\t\t\ti++\n\t\t\t\t}\n\t\t\tcase \"--target\":\n\t\t\t\tif i+1 < len(args) {\n\t\t\t\t\ttargetPath = args[i+1]\n\t\t\t\t\ti++\n\t\t\t\t}\n\t\t\tcase \"--include\":\n\t\t\t\tif i+1 < len(args) {\n\t\t\t\t\tincludePath = args[i+1]\n\t\t\t\t\ti++\n\t\t\t\t}\n\t\t\t}\n\t\t}\n\t\tif repoPath == \"\" || targetPath == \"\" || includePath == \"\" {\n\t\t\tfmt.Fprintln(os.Stderr, \"ERROR: --repo, --target, --include required for restore\")\n\t\t\tos.Exit(1)\n\t\t}\n\t\t// Validate password\n\t\tif !validatePassword(repoPath, password) {\n\t\t\tfmt.Fprintln(os.Stderr, \"ERROR: invalid password\")\n\t\t\tos.Exit(1)\n\t\t}\n\t\tsnapMetaPath := filepath.Join(repoPath, \"snapshots\", snapshotID+\".txt\")\n\t\tsourcePathBytes, err := os.ReadFile(snapMetaPath)\n\t\tif err != nil {\n\t\t\tfmt.Fprintln(os.Stderr, \"ERROR: snapshot not found\")\n\t\t\tos.Exit(1)\n\t\t}\n\t\tsourcePath := string(sourcePathBytes)\n\t\tsrcFile := filepath.Join(sourcePath, includePath)\n\t\tdstFile := filepath.Join(targetPath, includePath)\n\t\tif err := os.MkdirAll(filepath.Dir(dstFile), 0o755); err != nil {\n\t\t\tfmt.Fprintf(os.Stderr, \"ERROR: mkdir target failed: %v\\n\", err)\n\t\t\tos.Exit(1)\n\t\t}\n\t\tcontent, err := os.ReadFile(srcFile)\n\t\tif err != nil {\n\t\t\tfmt.Fprintf(os.Stderr, \"ERROR: read source file failed: %v\\n\", err)\n\t\t\tos.Exit(1)\n\t\t}\n\t\tif err := os.WriteFile(dstFile, content, 0o644); err != nil {\n\t\t\tfmt.Fprintf(os.Stderr, \"ERROR: write target file failed: %v\\n\", err)\n\t\t\tos.Exit(1)\n\t\t}\n\n\tdefault:\n\t\tfmt.Fprintf(os.Stderr, \"ERROR: unknown command %s\\n\", command)\n\t\tos.Exit(1)\n\t}\n}\n\nfunc validatePassword(repoPath, password string) bool {\n\tpasswordPath := filepath.Join(repoPath, \".password\")\n\tstoredPassword, err := os.ReadFile(passwordPath)\n\tif err != nil {\n\t\treturn false\n\t}\n\treturn strings.TrimSpace(string(storedPassword)) == password\n}\n"

	if err := os.WriteFile(stubSrc, []byte(src), 0o644); err != nil {
		t.Fatalf("write stub source: %v", err)
	}

	// Build the stub - use go binary from GOROOT
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		goBin += ".exe"
	}
	cmd := &exec.Cmd{
		Path: goBin,
		Args: []string{"go", "build", "-o", stubBin, stubSrc},
		Dir:  tmpDir,
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stub failed: %v\n%s", err, string(output))
	}

	return stubBin
}

// TestResticSnapshotJSONParsing tests that ResticSnapshot JSON parsing works.
func TestResticSnapshotJSONParsing(t *testing.T) {
	jsonData := `[
		{
			"id": "abc123",
			"time": "2024-01-01T12:00:00Z",
			"paths": ["/data"],
			"hostname": "test-host",
			"username": "test-user",
			"tags": ["tag1", "tag2"],
			"short_id": "abc123"
		}
	]`

	var snapshots []ResticSnapshot
	if err := json.Unmarshal([]byte(jsonData), &snapshots); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if len(snapshots) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snapshots))
	}

	s := snapshots[0]
	if s.ID != "abc123" {
		t.Errorf("ID: got %s, want abc123", s.ID)
	}
	expectedTime, _ := time.Parse(time.RFC3339, "2024-01-01T12:00:00Z")
	if !s.Time.Equal(expectedTime) {
		t.Errorf("Time: got %v, want %v", s.Time, expectedTime)
	}
	if len(s.Paths) != 1 || s.Paths[0] != "/data" {
		t.Errorf("Paths: got %v, want [/data]", s.Paths)
	}
	if s.Hostname != "test-host" {
		t.Errorf("Hostname: got %s, want test-host", s.Hostname)
	}
	if s.Username != "test-user" {
		t.Errorf("Username: got %s, want test-user", s.Username)
	}
	if len(s.Tags) != 2 || s.Tags[0] != "tag1" || s.Tags[1] != "tag2" {
		t.Errorf("Tags: got %v, want [tag1 tag2]", s.Tags)
	}
	if s.ShortID != "abc123" {
		t.Errorf("ShortID: got %s, want abc123", s.ShortID)
	}
}