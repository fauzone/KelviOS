package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalSourceList(t *testing.T) {
	tmpDir := t.TempDir()

	// Create test tree
	// root/
	//   a.txt (5 bytes)
	//   sub/
	//     b.txt (5 bytes)
	//     deep/
	//       c.bin (8 bytes)
	//   empty_dir/
	//   symlink_file -> a.txt (should be skipped)
	//   symlink_dir -> sub (should be skipped)

	root := filepath.Join(tmpDir, "source")
	if err := os.MkdirAll(filepath.Join(root, "sub", "deep"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty_dir"), 0o755); err != nil {
		t.Fatalf("mkdir empty_dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "deep", "c.bin"), []byte("binary\x00\x01"), 0o644); err != nil {
		t.Fatalf("write c.bin: %v", err)
	}
	// Create symlinks (on Windows this may require admin, so skip if fails)
	_ = os.Symlink("a.txt", filepath.Join(root, "symlink_file"))
	_ = os.Symlink("sub", filepath.Join(root, "symlink_dir"))

	src := &LocalSource{Root: root}
	entries, err := src.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Build map for easy lookup
	entryMap := make(map[string]SourceEntry)
	for _, e := range entries {
		entryMap[e.Path] = e
	}

	// Check expected entries
	expected := map[string]struct {
		Size  int64
		IsDir bool
	}{
		"a.txt":          {Size: 5, IsDir: false},
		"sub":            {Size: 0, IsDir: true},
		"sub/b.txt":      {Size: 5, IsDir: false},
		"sub/deep":       {Size: 0, IsDir: true},
		"sub/deep/c.bin": {Size: 8, IsDir: false},
		"empty_dir":      {Size: 0, IsDir: true},
	}

	for path, exp := range expected {
		e, ok := entryMap[path]
		if !ok {
			t.Errorf("missing entry: %s", path)
			continue
		}
		if e.Size != exp.Size {
			t.Errorf("%s: size=%d want %d", path, e.Size, exp.Size)
		}
		if e.IsDir != exp.IsDir {
			t.Errorf("%s: IsDir=%v want %v", path, e.IsDir, exp.IsDir)
		}
		if strings.Contains(path, "\\") {
			t.Errorf("%s: path should use slash separator, got backslash", path)
		}
	}

	// Symlinks should NOT be in the list
	if _, ok := entryMap["symlink_file"]; ok {
		t.Error("symlink_file should be skipped")
	}
	if _, ok := entryMap["symlink_dir"]; ok {
		t.Error("symlink_dir should be skipped")
	}

	// Total count: 6 entries (3 files + 3 dirs)
	if len(entries) != 6 {
		t.Errorf("expected 6 entries, got %d", len(entries))
	}
}

func TestLocalSourceOpen(t *testing.T) {
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "source")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}

	src := &LocalSource{Root: root}

	// Open existing file
	rc, err := src.Open("a.txt")
	if err != nil {
		t.Fatalf("Open a.txt: %v", err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("Read a.txt: %v", err)
	}
	if string(data) != "hello" {
		t.Errorf("a.txt content: got %q want %q", string(data), "hello")
	}

	// Open nested file
	rc, err = src.Open("sub/b.txt")
	if err != nil {
		t.Fatalf("Open sub/b.txt: %v", err)
	}
	data, err = io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("Read sub/b.txt: %v", err)
	}
	if string(data) != "world" {
		t.Errorf("sub/b.txt content: got %q want %q", string(data), "world")
	}

	// Open non-existent file -> error
	_, err = src.Open("nonexistent.txt")
	if err == nil {
		t.Error("Open nonexistent.txt should error")
	}

	// Open directory -> error
	_, err = src.Open("sub")
	if err == nil {
		t.Error("Open sub (directory) should error")
	}

	// Path escape attempts -> error
	escapePaths := []string{"../etc/passwd", "sub/../a.txt", "/etc/passwd", "C:\\Windows\\System32"}
	for _, p := range escapePaths {
		_, err = src.Open(p)
		if err == nil {
			t.Errorf("Open %q should error (path escape)", p)
		}
	}
}

func TestLocalDestWriteFile(t *testing.T) {
	tmpDir := t.TempDir()
	destRoot := filepath.Join(tmpDir, "dest")

	dst := &LocalDest{Root: destRoot}

	// Write a file
	content := []byte("hello world")
	r := strings.NewReader(string(content))
	digest, written, err := dst.WriteFile("a.txt", r)
	if err != nil {
		t.Fatalf("WriteFile a.txt: %v", err)
	}
	if written != int64(len(content)) {
		t.Errorf("written=%d want %d", written, len(content))
	}
	sum := sha256.Sum256(content)
	expectedDigest := hex.EncodeToString(sum[:])
	if digest != expectedDigest {
		t.Errorf("digest=%s want %s", digest, expectedDigest)
	}

	// Verify file exists with correct content
	data, err := os.ReadFile(filepath.Join(destRoot, "a.txt"))
	if err != nil {
		t.Fatalf("ReadFile a.txt: %v", err)
	}
	if string(data) != string(content) {
		t.Errorf("file content mismatch: got %q want %q", string(data), string(content))
	}

	// Write nested file (parent dirs auto-created)
	nestedContent := []byte("nested content")
	r = strings.NewReader(string(nestedContent))
	digest, written, err = dst.WriteFile("sub/deep/nested.txt", r)
	if err != nil {
		t.Fatalf("WriteFile nested: %v", err)
	}
	if written != int64(len(nestedContent)) {
		t.Errorf("nested written=%d want %d", written, len(nestedContent))
	}
	sum = sha256.Sum256(nestedContent)
	expectedDigest = hex.EncodeToString(sum[:])
	if digest != expectedDigest {
		t.Errorf("nested digest=%s want %s", digest, expectedDigest)
	}

	// Verify nested file
	data, err = os.ReadFile(filepath.Join(destRoot, "sub", "deep", "nested.txt"))
	if err != nil {
		t.Fatalf("ReadFile nested: %v", err)
	}
	if string(data) != string(nestedContent) {
		t.Errorf("nested file content mismatch")
	}

	// Path escape attempts -> error
	escapePaths := []string{"../escape.txt", "sub/../escape.txt", "/absolute.txt", "C:\\Windows\\temp.txt"}
	for _, p := range escapePaths {
		r = strings.NewReader("x")
		_, _, err = dst.WriteFile(p, r)
		if err == nil {
			t.Errorf("WriteFile %q should error (path escape)", p)
		}
	}

	// Error during write -> partial file removed
	// We can't easily simulate a write error after partial write without a custom reader,
	// but we can verify the error path by passing a reader that errors.
	// For now, verify that the file doesn't exist after a failed write to a non-writable location.
	// (On Windows, creating a file in a non-writable dir may not work as expected in tests.)
}

func TestLocalSourceDestRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	sourceRoot := filepath.Join(tmpDir, "source")
	destRoot := filepath.Join(tmpDir, "dest")

	// Create source tree
	if err := os.MkdirAll(filepath.Join(sourceRoot, "sub", "deep"), 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	files := map[string][]byte{
		"a.txt":          []byte("hello"),
		"sub/b.txt":      []byte("world"),
		"sub/deep/c.bin": []byte("binary\x00\x01\x02"),
	}
	for path, content := range files {
		fullPath := filepath.Join(sourceRoot, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", path, err)
		}
		if err := os.WriteFile(fullPath, content, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	src := &LocalSource{Root: sourceRoot}
	dst := &LocalDest{Root: destRoot}

	// Build plan
	plan, err := BuildPlan(src)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	// 3 files + 2 dirs (sub, sub/deep) = 5 total entries
	if plan.TotalFiles != 5 {
		t.Errorf("plan.TotalFiles=%d want 5", plan.TotalFiles)
	}
	// c.bin is "binary\x00\x01\x02" = 9 bytes
	if plan.TotalBytes != 5+5+9 {
		t.Errorf("plan.TotalBytes=%d want 19", plan.TotalBytes)
	}

	// Execute
	report := Execute(context.Background(), src, dst, plan)
	if report.Copied != 3 {
		t.Errorf("report.Copied=%d want 3", report.Copied)
	}
	if report.Failed != 0 {
		t.Errorf("report.Failed=%d want 0", report.Failed)
	}
	if report.BytesCopied != 19 {
		t.Errorf("report.BytesCopied=%d want 19", report.BytesCopied)
	}
	if len(report.Files) != 3 {
		t.Errorf("report.Files len=%d want 3", len(report.Files))
	}

	// Verify each file byte-identical
	for _, fr := range report.Files {
		if fr.Status != "ok" {
			t.Errorf("%s: status=%s want ok", fr.Path, fr.Status)
			continue
		}
		expectedContent := files[fr.Path]
		actualContent, err := os.ReadFile(filepath.Join(destRoot, fr.Path))
		if err != nil {
			t.Errorf("%s: read dest: %v", fr.Path, err)
			continue
		}
		if string(actualContent) != string(expectedContent) {
			t.Errorf("%s: content mismatch", fr.Path)
		}
		// Verify SHA-256 matches
		sum := sha256.Sum256(expectedContent)
		expectedDigest := hex.EncodeToString(sum[:])
		if fr.SHA256 != expectedDigest {
			t.Errorf("%s: digest mismatch: got %s want %s", fr.Path, fr.SHA256, expectedDigest)
		}
	}

	// Verify source tree is byte-identical (read-only in practice)
	for path, expectedContent := range files {
		actualContent, err := os.ReadFile(filepath.Join(sourceRoot, path))
		if err != nil {
			t.Errorf("source %s: read: %v", path, err)
			continue
		}
		if string(actualContent) != string(expectedContent) {
			t.Errorf("source %s: content changed!", path)
		}
	}
}

func TestLocalSourceEmptyRoot(t *testing.T) {
	tmpDir := t.TempDir()
	root := filepath.Join(tmpDir, "empty")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	src := &LocalSource{Root: root}
	entries, err := src.List()
	if err != nil {
		t.Fatalf("List empty: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(entries))
	}
}

func TestLocalSourceNonExistentRoot(t *testing.T) {
	src := &LocalSource{Root: "/nonexistent/path/that/does/not/exist"}
	_, err := src.List()
	if err == nil {
		t.Error("List non-existent root should error")
	}
}

func TestLocalSourceNotADir(t *testing.T) {
	tmpDir := t.TempDir()
	file := filepath.Join(tmpDir, "notadir.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	src := &LocalSource{Root: file}
	_, err := src.List()
	if err == nil {
		t.Error("List on file (not dir) should error")
	}
}
