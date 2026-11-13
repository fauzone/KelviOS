package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

// fakeSourceFS is an in-memory read-only source filesystem for testing.
// It only implements List() and Open() — no write/delete methods —
// structurally proving the read-only invariant.
type fakeSourceFS struct {
	entries map[string]sourceEntry
}

type sourceEntry struct {
	content string
	isDir   bool
}

func newFakeSourceFS() *fakeSourceFS {
	return &fakeSourceFS{entries: make(map[string]sourceEntry)}
}

func (f *fakeSourceFS) addFile(path, content string) {
	f.entries[path] = sourceEntry{content: content, isDir: false}
}

func (f *fakeSourceFS) addDir(path string) {
	f.entries[path] = sourceEntry{content: "", isDir: true}
}

func (f *fakeSourceFS) List() ([]SourceEntry, error) {
	var result []SourceEntry
	for path, e := range f.entries {
		if e.isDir {
			result = append(result, SourceEntry{Path: path, Size: 0, IsDir: true})
		} else {
			result = append(result, SourceEntry{Path: path, Size: int64(len(e.content)), IsDir: false})
		}
	}
	return result, nil
}

func (f *fakeSourceFS) Open(path string) (io.ReadCloser, error) {
	e, ok := f.entries[path]
	if !ok {
		return nil, errors.New("file not found: " + path)
	}
	if e.isDir {
		return nil, errors.New("is a directory: " + path)
	}
	return io.NopCloser(strings.NewReader(e.content)), nil
}

// fakeDestFS is an in-memory destination filesystem for testing.
type fakeDestFS struct {
	files      map[string]string
	failDigest map[string]bool // paths that should return wrong digest
	failWrite  map[string]bool // paths that should fail on write
}

func newFakeDestFS() *fakeDestFS {
	return &fakeDestFS{
		files:      make(map[string]string),
		failDigest: make(map[string]bool),
		failWrite:  make(map[string]bool),
	}
}

func (f *fakeDestFS) setFailDigest(path string) {
	f.failDigest[path] = true
}

func (f *fakeDestFS) setFailWrite(path string) {
	f.failWrite[path] = true
}

func (f *fakeDestFS) WriteFile(path string, r io.Reader) (string, int64, error) {
	if f.failWrite[path] {
		return "", 0, errors.New("write failed: " + path)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	f.files[path] = string(data)
	h := sha256.Sum256(data)
	digest := hex.EncodeToString(h[:])
	// Corrupt digest if configured
	if f.failDigest[path] {
		// Flip first byte to guarantee mismatch
		corrupted := []byte(digest)
		corrupted[0] ^= 0xFF
		digest = string(corrupted)
	}
	return digest, int64(len(data)), nil
}

// TestClassifyFS tests the filesystem classification table.
func TestClassifyFS(t *testing.T) {
	tests := []struct {
		input    string
		expected FSType
	}{
		{"ext4", FSTypeExt4},
		{"EXT4", FSTypeExt4},
		{" Ext4 ", FSTypeExt4},
		{" EXT4 ", FSTypeExt4},
		{"xfs", FSTypeXFS},
		{"XFS", FSTypeXFS},
		{" btrfs ", FSTypeBtrfs},
		{"ntfs", FSTypeNTFS},
		{"NTFS", FSTypeNTFS},
		{"exfat", FSTypeExFAT},
		{"EXFAT", FSTypeExFAT},
		{"zfs", FSTypeZFS},
		{"ZFS", FSTypeZFS},
		{"weirdfs", FSTypeUnknown},
		{"", FSTypeUnknown},
		{"   ", FSTypeUnknown},
		{"unknown", FSTypeUnknown},
	}

	for _, tc := range tests {
		result := ClassifyFS(tc.input)
		if result != tc.expected {
			t.Errorf("ClassifyFS(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

// TestBuildPlan tests plan building: totals, dir exclusion, negative-size error.
func TestBuildPlan(t *testing.T) {
	t.Run("happy path with dirs and files", func(t *testing.T) {
		src := newFakeSourceFS()
		src.addDir("/dir1")
		src.addDir("/dir1/subdir")
		src.addFile("/file1.txt", "hello")
		src.addFile("/dir1/file2.txt", "world")
		src.addFile("/dir1/subdir/file3.txt", "!")

		plan, err := BuildPlan(src)
		if err != nil {
			t.Fatalf("BuildPlan failed: %v", err)
		}

		if plan.TotalFiles != 5 { // 2 dirs + 3 files
			t.Errorf("TotalFiles = %d, want 5", plan.TotalFiles)
		}
		if plan.TotalBytes != 11 { // 5 + 5 + 1
			t.Errorf("TotalBytes = %d, want 11", plan.TotalBytes)
		}
		if len(plan.Entries) != 3 {
			t.Errorf("Entries count = %d, want 3", len(plan.Entries))
		}
		// Verify entries are files only (no dirs)
		for _, e := range plan.Entries {
			if e.Path == "/dir1" || e.Path == "/dir1/subdir" {
				t.Errorf("Directory %q should not be in plan entries", e.Path)
			}
		}
	})

	t.Run("negative size error", func(t *testing.T) {
		srcNeg := &fakeSourceFSWithNegativeSize{}

		_, err := BuildPlan(srcNeg)
		if err == nil {
			t.Error("BuildPlan should error on negative size")
		}
		if !strings.Contains(err.Error(), "negative size") {
			t.Errorf("Error should mention negative size, got: %v", err)
		}
		if !strings.Contains(err.Error(), "/badfile") {
			t.Errorf("Error should name the path, got: %v", err)
		}
	})

	t.Run("empty source", func(t *testing.T) {
		src := newFakeSourceFS()
		plan, err := BuildPlan(src)
		if err != nil {
			t.Fatalf("BuildPlan failed: %v", err)
		}
		if plan.TotalFiles != 0 {
			t.Errorf("TotalFiles = %d, want 0", plan.TotalFiles)
		}
		if plan.TotalBytes != 0 {
			t.Errorf("TotalBytes = %d, want 0", plan.TotalBytes)
		}
		if len(plan.Entries) != 0 {
			t.Errorf("Entries count = %d, want 0", len(plan.Entries))
		}
	})
}

// fakeSourceFSWithNegativeSize returns a negative size for one file.
type fakeSourceFSWithNegativeSize struct{}

func (f *fakeSourceFSWithNegativeSize) List() ([]SourceEntry, error) {
	return []SourceEntry{
		{Path: "/good.txt", Size: 10, IsDir: false},
		{Path: "/badfile", Size: -5, IsDir: false},
	}, nil
}

func (f *fakeSourceFSWithNegativeSize) Open(path string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

// fakeSourceFSWithMissingFile includes a file in List() but fails on Open().
type fakeSourceFSWithMissingFile struct{}

func (f *fakeSourceFSWithMissingFile) List() ([]SourceEntry, error) {
	return []SourceEntry{
		{Path: "/good1.txt", Size: 6, IsDir: false},
		{Path: "/missing.txt", Size: 100, IsDir: false},
		{Path: "/good2.txt", Size: 6, IsDir: false},
	}, nil
}

func (f *fakeSourceFSWithMissingFile) Open(path string) (io.ReadCloser, error) {
	if path == "/missing.txt" {
		return nil, errors.New("file not found: " + path)
	}
	return io.NopCloser(strings.NewReader("data 1")), nil
}

// TestExecute tests the copy engine with various scenarios.
func TestExecute(t *testing.T) {
	t.Run("happy path: 3 files, digests match", func(t *testing.T) {
		src := newFakeSourceFS()
		src.addFile("/a.txt", "content A")
		src.addFile("/b.txt", "content B")
		src.addFile("/c.txt", "content C")

		plan, err := BuildPlan(src)
		if err != nil {
			t.Fatalf("BuildPlan failed: %v", err)
		}

		dst := newFakeDestFS()
		report := Execute(context.Background(), src, dst, plan)

		if report.Copied != 3 {
			t.Errorf("Copied = %d, want 3", report.Copied)
		}
		if report.Failed != 0 {
			t.Errorf("Failed = %d, want 0", report.Failed)
		}
		expectedBytes := int64(len("content A") + len("content B") + len("content C"))
		if report.BytesCopied != expectedBytes {
			t.Errorf("BytesCopied = %d, want %d", report.BytesCopied, expectedBytes)
		}
		if len(report.Files) != 3 {
			t.Errorf("Files count = %d, want 3", len(report.Files))
		}
		for _, fr := range report.Files {
			if fr.Status != "ok" {
				t.Errorf("File %s status = %s, want ok", fr.Path, fr.Status)
			}
			if fr.Err != "" {
				t.Errorf("File %s Err = %q, want empty", fr.Path, fr.Err)
			}
			// Verify SHA256 matches
			h := sha256.Sum256([]byte(src.entries[fr.Path].content))
			expectedDigest := hex.EncodeToString(h[:])
			if fr.SHA256 != expectedDigest {
				t.Errorf("File %s SHA256 = %s, want %s", fr.Path, fr.SHA256, expectedDigest)
			}
		}
	})

	t.Run("dest corrupts one file: wrong digest returned", func(t *testing.T) {
		src := newFakeSourceFS()
		src.addFile("/good1.txt", "data 1")
		src.addFile("/bad.txt", "data 2")
		src.addFile("/good2.txt", "data 3")

		plan, err := BuildPlan(src)
		if err != nil {
			t.Fatalf("BuildPlan failed: %v", err)
		}

		dst := newFakeDestFS()
		dst.setFailDigest("/bad.txt") // This file will have corrupted digest

		report := Execute(context.Background(), src, dst, plan)

		if report.Copied != 2 {
			t.Errorf("Copied = %d, want 2", report.Copied)
		}
		if report.Failed != 1 {
			t.Errorf("Failed = %d, want 1", report.Failed)
		}
		if len(report.Files) != 3 {
			t.Errorf("Files count = %d, want 3", len(report.Files))
		}
		for _, fr := range report.Files {
			if fr.Path == "/bad.txt" {
				if fr.Status != "failed" {
					t.Errorf("bad.txt status = %s, want failed", fr.Status)
				}
				if fr.Err != "checksum mismatch" {
					t.Errorf("bad.txt Err = %q, want 'checksum mismatch'", fr.Err)
				}
			} else {
				if fr.Status != "ok" {
					t.Errorf("%s status = %s, want ok", fr.Path, fr.Status)
				}
			}
		}
	})

	t.Run("source Open error on one file: failed + continue", func(t *testing.T) {
		src := &fakeSourceFSWithMissingFile{}

		plan, err := BuildPlan(src)
		if err != nil {
			t.Fatalf("BuildPlan failed: %v", err)
		}

		dst := newFakeDestFS()
		report := Execute(context.Background(), src, dst, plan)

		if report.Copied != 2 {
			t.Errorf("Copied = %d, want 2", report.Copied)
		}
		if report.Failed != 1 {
			t.Errorf("Failed = %d, want 1", report.Failed)
		}
		if len(report.Files) != 3 {
			t.Errorf("Files count = %d, want 3", len(report.Files))
		}
		for _, fr := range report.Files {
			if fr.Path == "/missing.txt" {
				if fr.Status != "failed" {
					t.Errorf("missing.txt status = %s, want failed", fr.Status)
				}
				if !strings.Contains(fr.Err, "file not found") {
					t.Errorf("missing.txt Err = %q, want 'file not found'", fr.Err)
				}
			} else {
				if fr.Status != "ok" {
					t.Errorf("%s status = %s, want ok", fr.Path, fr.Status)
				}
			}
		}
	})

	t.Run("context cancelled before start: zero copied, empty report", func(t *testing.T) {
		src := newFakeSourceFS()
		src.addFile("/a.txt", "data")

		plan, err := BuildPlan(src)
		if err != nil {
			t.Fatalf("BuildPlan failed: %v", err)
		}

		dst := newFakeDestFS()
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		report := Execute(ctx, src, dst, plan)

		if report.Copied != 0 {
			t.Errorf("Copied = %d, want 0", report.Copied)
		}
		if report.Failed != 0 {
			t.Errorf("Failed = %d, want 0", report.Failed)
		}
		if report.BytesCopied != 0 {
			t.Errorf("BytesCopied = %d, want 0", report.BytesCopied)
		}
		if len(report.Files) != 0 {
			t.Errorf("Files count = %d, want 0", len(report.Files))
		}
	})

	t.Run("context cancelled mid-run: stops between files", func(t *testing.T) {
		src := newFakeSourceFS()
		src.addFile("/a.txt", "data 1")
		src.addFile("/b.txt", "data 2")
		src.addFile("/c.txt", "data 3")

		plan, err := BuildPlan(src)
		if err != nil {
			t.Fatalf("BuildPlan failed: %v", err)
		}

		ctx, cancel := context.WithCancel(context.Background())

		// Cancel after first file by using a custom dest that cancels on second write
		cancelOnSecond := &cancellingDestFS{
			fakeDestFS: newFakeDestFS(),
			cancel:     cancel,
			callCount:  0,
		}

		report := Execute(ctx, src, cancelOnSecond, plan)

		// Should have copied 1 file (first), then cancelled before second
		if report.Copied != 1 {
			t.Errorf("Copied = %d, want 1", report.Copied)
		}
		if report.Failed != 0 {
			t.Errorf("Failed = %d, want 0", report.Failed)
		}
		if len(report.Files) != 1 {
			t.Errorf("Files count = %d, want 1 (stopped after first)", len(report.Files))
		}
	})
}

// cancellingDestFS cancels context after first WriteFile call.
type cancellingDestFS struct {
	*fakeDestFS
	cancel    context.CancelFunc
	callCount int
}

func (c *cancellingDestFS) WriteFile(path string, r io.Reader) (string, int64, error) {
	c.callCount++
	if c.callCount == 1 {
		c.cancel()
	}
	return c.fakeDestFS.WriteFile(path, r)
}
