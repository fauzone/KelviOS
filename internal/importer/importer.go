package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"strings"
)

// FSType represents a filesystem type.
type FSType string

const (
	FSTypeExt4    FSType = "ext4"
	FSTypeXFS     FSType = "xfs"
	FSTypeBtrfs   FSType = "btrfs"
	FSTypeNTFS    FSType = "ntfs"
	FSTypeExFAT   FSType = "exfat"
	FSTypeZFS     FSType = "zfs"
	FSTypeUnknown FSType = "unknown"
)

// ClassifyFS normalizes a probe hint string (case-insensitive, trimmed)
// to a known FSType. Unrecognized values return FSTypeUnknown.
func ClassifyFS(hint string) FSType {
	normalized := strings.ToLower(strings.TrimSpace(hint))
	switch normalized {
	case "ext4":
		return FSTypeExt4
	case "xfs":
		return FSTypeXFS
	case "btrfs":
		return FSTypeBtrfs
	case "ntfs":
		return FSTypeNTFS
	case "exfat":
		return FSTypeExFAT
	case "zfs":
		return FSTypeZFS
	default:
		return FSTypeUnknown
	}
}

// SourceEntry represents a single entry in a source filesystem.
type SourceEntry struct {
	Path  string
	Size  int64
	IsDir bool
}

// SourceFS is a read-only filesystem interface for the source disk.
// It deliberately has NO write/delete methods: a source cannot be modified through this interface.
type SourceFS interface {
	List() ([]SourceEntry, error)
	Open(path string) (io.ReadCloser, error)
}

// DestFS is the destination filesystem interface.
// WriteFile writes the content from the reader and returns the SHA-256 digest
// of the bytes received, the number of bytes written, and any error.
type DestFS interface {
	WriteFile(path string, r io.Reader) (sha256Hex string, written int64, err error)
}

// Plan represents a copy plan built from a source filesystem.
type Plan struct {
	Entries    []PlanEntry
	TotalFiles int
	TotalBytes int64
}

// PlanEntry represents a file to be copied (directories are excluded from copy items but counted).
type PlanEntry struct {
	Path string
	Size int64
}

// BuildPlan walks the source filesystem once and builds a copy plan.
// Directories are excluded from copy items but counted in totals.
// An entry with negative size is a plan error (the path is named in the error).
func BuildPlan(src SourceFS) (Plan, error) {
	entries, err := src.List()
	if err != nil {
		return Plan{}, err
	}

	var planEntries []PlanEntry
	totalFiles := 0
	var totalBytes int64

	for _, e := range entries {
		if e.IsDir {
			totalFiles++
			continue
		}
		if e.Size < 0 {
			return Plan{}, errors.New("negative size for file: " + e.Path)
		}
		planEntries = append(planEntries, PlanEntry{Path: e.Path, Size: e.Size})
		totalFiles++
		totalBytes += e.Size
	}

	return Plan{
		Entries:    planEntries,
		TotalFiles: totalFiles,
		TotalBytes: totalBytes,
	}, nil
}

// FileResult represents the result of copying a single file.
type FileResult struct {
	Path   string
	Size   int64
	SHA256 string
	Status string // "ok" or "failed"
	Err    string
}

// Report represents the result of an import execution.
type Report struct {
	Files       []FileResult
	Copied      int
	Failed      int
	BytesCopied int64
}

// hashingReader wraps an io.Reader and computes SHA-256 as data is read.
type hashingReader struct {
	r   io.Reader
	h   hash.Hash
	sum []byte
}

func newHashingReader(r io.Reader) *hashingReader {
	return &hashingReader{r: r, h: sha256.New()}
}

func (hr *hashingReader) Read(p []byte) (int, error) {
	n, err := hr.r.Read(p)
	if n > 0 {
		hr.h.Write(p[:n])
	}
	return n, err
}

func (hr *hashingReader) Sum() []byte {
	if hr.sum == nil {
		hr.sum = hr.h.Sum(nil)
	}
	return hr.sum
}

func (hr *hashingReader) HexSum() string {
	return hex.EncodeToString(hr.Sum())
}

// Execute runs the import plan: streams each file from source to destination,
// verifies checksums, and returns a report.
// Context cancellation stops between files (remaining files are not attempted).
func Execute(ctx context.Context, src SourceFS, dst DestFS, plan Plan) Report {
	var report Report
	report.Files = make([]FileResult, 0, len(plan.Entries))

	for _, entry := range plan.Entries {
		select {
		case <-ctx.Done():
			// Context cancelled: stop between files, do not fabricate results for unattempted files.
			return report
		default:
		}

		// Open source file
		srcReader, err := src.Open(entry.Path)
		if err != nil {
			report.Files = append(report.Files, FileResult{
				Path:   entry.Path,
				Size:   entry.Size,
				SHA256: "",
				Status: "failed",
				Err:    err.Error(),
			})
			report.Failed++
			continue
		}

		// Hash while copying: tee source stream to hashing reader, then write to dest
		hr := newHashingReader(srcReader)
		destDigest, written, err := dst.WriteFile(entry.Path, hr)
		srcReader.Close()

		srcDigest := hr.HexSum()

		if err != nil {
			report.Files = append(report.Files, FileResult{
				Path:   entry.Path,
				Size:   entry.Size,
				SHA256: srcDigest,
				Status: "failed",
				Err:    err.Error(),
			})
			report.Failed++
			continue
		}

		// Verify: source digest (computed while streaming) vs dest-returned digest
		if srcDigest != destDigest {
			report.Files = append(report.Files, FileResult{
				Path:   entry.Path,
				Size:   written,
				SHA256: srcDigest,
				Status: "failed",
				Err:    "checksum mismatch",
			})
			report.Failed++
			continue
		}

		// Success
		report.Files = append(report.Files, FileResult{
			Path:   entry.Path,
			Size:   written,
			SHA256: srcDigest,
			Status: "ok",
			Err:    "",
		})
		report.Copied++
		report.BytesCopied += written
	}

	return report
}
