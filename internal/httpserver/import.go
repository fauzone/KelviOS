package httpserver

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/importer"
)

// importAllowedRoot is the root directory under which import source paths must resolve.
// In production this is "/mnt". Tests may override this to a temp directory.
var importAllowedRoot = "/mnt"

// importRequest is the JSON body for POST /api/collections/{id}/import
type importRequest struct {
	SourcePath string `json:"source_path"`
	FSHint     string `json:"fs_hint,omitempty"`
}

// handleImportCollection handles POST /api/collections/{id}/import
func (s *Server) handleImportCollection(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Resolve collection and pool
	coll, pool, ok := s.resolveCollectionAndPool(w, r, id, "import.execute")
	if !ok {
		return // already responded + audited
	}

	// Check cold tier
	if !s.checkColdTier(w, r, id, pool, "", "import.execute") {
		return // cold pool -> 409 wake_required
	}

	// Decode JSON body
	var req importRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "import.execute", "collection", id, "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Validate source_path
	if strings.TrimSpace(req.SourcePath) == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "import.execute", "collection", id, "error", "empty source_path")
		s.writeJSONError(w, http.StatusBadRequest, "source_path is required")
		return
	}

	// Resolve and validate source_path against allowed root
	resolvedSource, err := filepath.EvalSymlinks(filepath.Clean(req.SourcePath))
	if err != nil {
		// If symlink resolution fails (e.g., path doesn't exist), fall back to cleaned path
		resolvedSource = filepath.Clean(req.SourcePath)
	}

	// Check if resolved source is under allowed root (path segment boundary, not raw prefix)
	allowedRoot := filepath.Clean(importAllowedRoot)
	if !isPathUnder(resolvedSource, allowedRoot) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "import.execute", "collection", id, "error", "source_path outside allowed root: "+resolvedSource)
		s.writeJSONError(w, http.StatusBadRequest, "source_path must be under "+allowedRoot)
		return
	}

	// Resolve destination root
	mountpoint := pool.Mountpoint
	if mountpoint == "" {
		mountpoint = "/mnt/" + pool.Name
	}
	root := filepath.Join(mountpoint, coll.Path)
	resolvedDest := filepath.Clean(root)

	// Prevent self-import / recursion: source == dest, or one contains the other
	if resolvedSource == resolvedDest || isPathUnder(resolvedSource, resolvedDest) || isPathUnder(resolvedDest, resolvedSource) {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "import.execute", "collection", id, "error", "source_path overlaps with collection root")
		s.writeJSONError(w, http.StatusBadRequest, "source_path overlaps with collection root")
		return
	}

	// Build plan from local source
	src := &importer.LocalSource{Root: req.SourcePath}
	plan, err := importer.BuildPlan(src)
	if err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "import.execute", "collection", id, "error", err.Error())
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Resolve destination root (same convention as IndexCollectionFiles)
	mountpoint = pool.Mountpoint
	if mountpoint == "" {
		mountpoint = "/mnt/" + pool.Name
	}
	root = filepath.Join(mountpoint, coll.Path)
	root = filepath.Clean(root)
	dst := &importer.LocalDest{Root: root}

	// Execute import
	report := importer.Execute(r.Context(), src, dst, plan)

	// Persist import record
	fsType := string(importer.ClassifyFS(req.FSHint))
	imp := &catalog.ImportRow{
		CollectionID: id,
		SourcePath:   req.SourcePath,
		FSType:       fsType,
		TotalFiles:   plan.TotalFiles,
		TotalBytes:   plan.TotalBytes,
		Copied:       report.Copied,
		Failed:       report.Failed,
		BytesCopied:  report.BytesCopied,
	}
	if err := catalog.InsertImport(s.db, imp); err != nil {
		slog.Error("insert import failed", "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "import.execute", "collection", id, "error", "copy succeeded but catalog persist failed: "+err.Error())
		s.writeJSONError(w, http.StatusInternalServerError, "import completed but catalog persist failed")
		return
	}

	// Audit success
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "import.execute", "collection", id, "ok",
		fmt.Sprintf("source=%s fs=%s copied=%d failed=%d bytes=%d", req.SourcePath, fsType, report.Copied, report.Failed, report.BytesCopied))

	// Response
	response := map[string]any{
		"import": map[string]any{
			"id":            imp.ID,
			"collection_id": imp.CollectionID,
			"source_path":   imp.SourcePath,
			"fs_type":       imp.FSType,
			"total_files":   imp.TotalFiles,
			"total_bytes":   imp.TotalBytes,
			"copied":        imp.Copied,
			"failed":        imp.Failed,
			"bytes_copied":  imp.BytesCopied,
			"ts":            imp.TS.Unix(),
		},
		"plan": map[string]any{
			"total_files": plan.TotalFiles,
			"total_bytes": plan.TotalBytes,
		},
		"report": map[string]any{
			"files":        report.Files,
			"copied":       report.Copied,
			"failed":       report.Failed,
			"bytes_copied": report.BytesCopied,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(response)
}

// handleListCollectionImports handles GET /api/collections/{id}/imports
func (s *Server) handleListCollectionImports(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Resolve collection and pool
	_, pool, ok := s.resolveCollectionAndPool(w, r, id, "import.list")
	if !ok {
		return // already responded + audited
	}
	_ = pool // pool not used but resolved for 404 consistency

	// Parse query params (same as handleUPSEvents)
	limit := parseIntQuery(r, "limit", 100)
	offset := parseIntQuery(r, "offset", 0)

	imports, total, err := catalog.ListImports(s.db, id, limit, offset)
	if err != nil {
		slog.Error("list imports failed", "collection_id", id, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list imports failed")
		return
	}

	// Convert to response format
	result := make([]map[string]any, 0, len(imports))
	for _, imp := range imports {
		result = append(result, map[string]any{
			"id":            imp.ID,
			"collection_id": imp.CollectionID,
			"source_path":   imp.SourcePath,
			"fs_type":       imp.FSType,
			"total_files":   imp.TotalFiles,
			"total_bytes":   imp.TotalBytes,
			"copied":        imp.Copied,
			"failed":        imp.Failed,
			"bytes_copied":  imp.BytesCopied,
			"ts":            imp.TS.Unix(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"imports": result,
		"total":   total,
	})
}

// isPathUnder reports whether path is under root (path segment boundary, not raw prefix).
// Both paths must be cleaned. Returns true if path == root or path is a subdirectory of root.
func isPathUnder(path, root string) bool {
	if path == root {
		return true
	}
	// Ensure root ends with separator for prefix match on path segment boundary
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	return strings.HasPrefix(path, root)
}
