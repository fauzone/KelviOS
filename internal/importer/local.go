package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LocalSource implements SourceFS over a local directory tree.
// The source is read-only: no write/delete methods are provided.
type LocalSource struct {
	Root string
}

// List walks the Root recursively and returns all entries.
// Entries use slash-separated paths relative to Root.
// Directories have IsDir=true, Size=0.
// Files carry their size.
// Symlinks are SKIPPED entirely: dir symlinks not descended, file symlinks not listed.
func (ls *LocalSource) List() ([]SourceEntry, error) {
	if ls.Root == "" {
		return nil, errors.New("LocalSource.Root is empty")
	}

	root := filepath.Clean(ls.Root)
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("LocalSource.Root is not a directory")
	}

	var entries []SourceEntry
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Skip directories we can't read
			return filepath.SkipDir
		}

		// Skip symlinks entirely
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		// Get relative path from root
		relPath, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		// Skip the root directory itself
		if relPath == "." {
			return nil
		}
		// Safety: reject paths escaping the root
		if strings.HasPrefix(relPath, "..") {
			return nil
		}
		// Normalize to slash-separated
		relPath = filepath.ToSlash(relPath)

		if d.IsDir() {
			entries = append(entries, SourceEntry{
				Path:  relPath,
				Size:  0,
				IsDir: true,
			})
			return nil
		}

		// Regular file
		info, err := d.Info()
		if err != nil {
			return nil // skip unreadable files
		}
		// Skip symlinks (defense in depth)
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		entries = append(entries, SourceEntry{
			Path:  relPath,
			Size:  info.Size(),
			IsDir: false,
		})
		return nil
	})

	if err != nil {
		return nil, err
	}

	return entries, nil
}

// Open opens a file for reading from the source.
// path is slash-separated relative to Root.
// Rejects absolute paths (Unix-style starting with /, Windows-style with drive letter) and any path escaping Root (..).
// Returns read-only file handle.
func (ls *LocalSource) Open(path string) (io.ReadCloser, error) {
	if ls.Root == "" {
		return nil, errors.New("LocalSource.Root is empty")
	}
	if isAbsolutePath(path) || strings.HasPrefix(path, "..") || strings.Contains(path, "../") {
		return nil, errors.New("path escape not allowed")
	}

	root := filepath.Clean(ls.Root)
	fullPath := filepath.Join(root, filepath.FromSlash(path))
	fullPath = filepath.Clean(fullPath)

	// Safety: ensure fullPath is within root
	if !strings.HasPrefix(fullPath, root) {
		return nil, errors.New("path escape not allowed")
	}

	// Verify it's a regular file (not a directory, not a symlink)
	info, err := os.Stat(fullPath)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, errors.New("path is a directory")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("symlinks are not supported")
	}

	return os.Open(fullPath)
}

// LocalDest implements DestFS over a local directory tree.
// WriteFile creates/truncates the file, copies content while hashing SHA-256,
// and returns the hex digest and bytes written.
// On any error, the partial file is removed before returning the error.
type LocalDest struct {
	Root string
}

// WriteFile writes the content from the reader to the destination.
// path is slash-separated relative to Root.
// Creates parent directories as needed (0o755).
// Creates/truncates the file (0o644).
// Returns SHA-256 hex digest of bytes written, bytes written, and any error.
// On error, removes the partial file before returning.
func (ld *LocalDest) WriteFile(path string, r io.Reader) (sha256Hex string, written int64, err error) {
	if ld.Root == "" {
		return "", 0, errors.New("LocalDest.Root is empty")
	}
	if isAbsolutePath(path) || strings.HasPrefix(path, "..") || strings.Contains(path, "../") {
		return "", 0, errors.New("path escape not allowed")
	}

	root := filepath.Clean(ld.Root)
	fullPath := filepath.Join(root, filepath.FromSlash(path))
	fullPath = filepath.Clean(fullPath)

	// Safety: ensure fullPath is within root
	if !strings.HasPrefix(fullPath, root) {
		return "", 0, errors.New("path escape not allowed")
	}

	// Create parent directory
	parentDir := filepath.Dir(fullPath)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return "", 0, err
	}

	// Create/truncate file
	f, err := os.OpenFile(fullPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, err
	}

	// Hash while writing
	h := sha256.New()
	mw := io.MultiWriter(f, h)

	written, err = io.Copy(mw, r)
	closeErr := f.Close()

	if err != nil {
		// Remove partial file on error
		_ = os.Remove(fullPath)
		return "", written, err
	}
	if closeErr != nil {
		_ = os.Remove(fullPath)
		return "", written, closeErr
	}

	digest := hex.EncodeToString(h.Sum(nil))
	return digest, written, nil
}

// isAbsolutePath checks if a slash-separated path is absolute.
// Handles Unix-style (starting with /) and Windows-style (drive letter + colon, e.g., C:/...).
func isAbsolutePath(path string) bool {
	if strings.HasPrefix(path, "/") {
		return true
	}
	// Windows drive letter: X:/... or X:\... (but we normalize to slash)
	if len(path) >= 3 && path[1] == ':' && (path[2] == '/' || path[2] == '\\') {
		return true
	}
	return false
}
