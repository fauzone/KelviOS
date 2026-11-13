package httpserver

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
)

// createShareLinkRequest is the request body for creating a share link.
type createShareLinkRequest struct {
	RelPath   string `json:"rel_path"`
	ExpiresAt *int64 `json:"expires_at,omitempty"` // unix seconds
	Password  string `json:"password,omitempty"`
}

// createShareLinkResponse is the response for creating a share link.
// Token is returned ONLY ONCE here.
type createShareLinkResponse struct {
	ID          string `json:"id"`
	RelPath     string `json:"rel_path"`
	Token       string `json:"token"` // raw token, returned once
	ExpiresAt   *int64 `json:"expires_at,omitempty"`
	HasPassword bool   `json:"has_password"`
	CreatedAt   int64  `json:"created_at"`
}

// listShareLinkResponse is the response for listing share links (no token, no password hash).
type listShareLinkResponse struct {
	ID          string `json:"id"`
	RelPath     string `json:"rel_path"`
	ExpiresAt   *int64 `json:"expires_at,omitempty"`
	HasPassword bool   `json:"has_password"`
	RevokedAt   *int64 `json:"revoked_at,omitempty"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   int64  `json:"created_at"`
}

// handleCollectionShareLinks handles POST/GET /api/collections/{collectionID}/share-links
// collectionID is already validated by the router.
func (s *Server) handleCollectionShareLinks(w http.ResponseWriter, r *http.Request, collectionID string) {
	switch r.Method {
	case http.MethodPost:
		s.handleCreateShareLink(w, r, collectionID)
	case http.MethodGet:
		s.handleListShareLinks(w, r, collectionID)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// handleCreateShareLink handles POST /api/collections/{id}/share-links
func (s *Server) handleCreateShareLink(w http.ResponseWriter, r *http.Request, collectionID string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, collectionID)
	if err != nil {
		slog.Error("get collection for sharelink failed", "collection_id", collectionID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.create", "sharelink", "", "error", "get collection failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get collection failed")
		return
	}
	if coll == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.create", "sharelink", "", "error", "collection not found")
		s.writeJSONError(w, http.StatusNotFound, "collection not found")
		return
	}

	// Get pool for tier info (needed for validation, but link creation is catalog-only)
	pool, _, err := catalog.GetPool(s.db, coll.PoolID)
	if err != nil {
		slog.Error("get pool for sharelink failed", "pool_id", coll.PoolID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.create", "sharelink", "", "error", "get pool failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get pool failed")
		return
	}
	if pool == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.create", "sharelink", "", "error", "pool not found")
		s.writeJSONError(w, http.StatusNotFound, "pool not found")
		return
	}

	var req createShareLinkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.create", "sharelink", "", "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Validate rel_path (required, same validator as write operations)
	if !s.validateRelPath(w, r, collectionID, req.RelPath) {
		return
	}

	// Validate expires_at if provided
	if req.ExpiresAt != nil {
		if *req.ExpiresAt <= time.Now().Unix() {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.create", "sharelink", "", "error", "expires_at in the past")
			s.writeJSONError(w, http.StatusBadRequest, "expires_at must be in the future")
			return
		}
	}

	// Target must be a FILE in catalog (GetFile), not a directory
	fileRow, err := catalog.GetFile(s.db, collectionID, req.RelPath)
	if err != nil {
		slog.Error("get file for sharelink failed", "collection_id", collectionID, "rel_path", req.RelPath, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.create", "sharelink", "", "error", "get file failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get file failed")
		return
	}
	if fileRow == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.create", "sharelink", "", "error", "file not indexed: "+req.RelPath)
		s.writeJSONError(w, http.StatusNotFound, "file not found")
		return
	}

	// Generate token: 32 hex chars from 16 bytes crypto/rand
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		slog.Error("generate token failed", "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.create", "sharelink", "", "error", "generate token failed")
		s.writeJSONError(w, http.StatusInternalServerError, "generate token failed")
		return
	}
	token := hex.EncodeToString(tokenBytes)
	tokenHash := sha256Hex(token)

	// Hash password if provided
	var passwordHash string
	if req.Password != "" {
		hash, err := hashPassword(req.Password)
		if err != nil {
			slog.Error("hash password failed", "error", err)
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.create", "sharelink", "", "error", "hash password failed")
			s.writeJSONError(w, http.StatusInternalServerError, "hash password failed")
			return
		}
		passwordHash = hash
	}

	// Create share link in catalog
	actor := actorFromRequest(r)
	link, err := catalog.CreateShareLink(s.db, collectionID, req.RelPath, tokenHash, passwordHash, req.ExpiresAt, actor)
	if err != nil {
		// Check for unique constraint violation on token_hash (extremely unlikely but handle)
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "unique") {
			// Retry once with new token (collision is astronomically unlikely)
			tokenBytes = make([]byte, 16)
			if _, err := rand.Read(tokenBytes); err != nil {
				slog.Error("generate token retry failed", "error", err)
				catalog.WriteAuditLog(s.db, actor, "sharelink.create", "sharelink", "", "error", "generate token failed")
				s.writeJSONError(w, http.StatusInternalServerError, "generate token failed")
				return
			}
			token = hex.EncodeToString(tokenBytes)
			tokenHash = sha256Hex(token)
			link, err = catalog.CreateShareLink(s.db, collectionID, req.RelPath, tokenHash, passwordHash, req.ExpiresAt, actor)
			if err != nil {
				slog.Error("create sharelink retry failed", "error", err)
				catalog.WriteAuditLog(s.db, actor, "sharelink.create", "sharelink", "", "error", "create sharelink failed")
				s.writeJSONError(w, http.StatusInternalServerError, "create sharelink failed")
				return
			}
		} else {
			slog.Error("create sharelink failed", "error", err)
			catalog.WriteAuditLog(s.db, actor, "sharelink.create", "sharelink", "", "error", "create sharelink failed")
			s.writeJSONError(w, http.StatusInternalServerError, "create sharelink failed")
			return
		}
	}

	// Audit log success (detail = link ID + rel_path only)
	catalog.WriteAuditLog(s.db, actor, "sharelink.create", "sharelink", link.ID, "ok", link.ID+" "+link.RelPath)

	// Build response with raw token (ONLY TIME)
	var expiresAt *int64
	if link.ExpiresAt != nil {
		unix := link.ExpiresAt.Unix()
		expiresAt = &unix
	}
	resp := createShareLinkResponse{
		ID:          link.ID,
		RelPath:     link.RelPath,
		Token:       token, // returned once
		ExpiresAt:   expiresAt,
		HasPassword: link.PasswordHash != "",
		CreatedAt:   link.CreatedAt.Unix(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"share_link": resp})
}

// handleListShareLinks handles GET /api/collections/{id}/share-links
func (s *Server) handleListShareLinks(w http.ResponseWriter, r *http.Request, collectionID string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, collectionID)
	if err != nil {
		slog.Error("get collection for list sharelinks failed", "collection_id", collectionID, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get collection failed")
		return
	}
	if coll == nil {
		s.writeJSONError(w, http.StatusNotFound, "collection not found")
		return
	}

	links, err := catalog.ListShareLinks(s.db, collectionID)
	if err != nil {
		slog.Error("list sharelinks failed", "collection_id", collectionID, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list sharelinks failed")
		return
	}
	if links == nil {
		links = []*catalog.ShareLinkRow{}
	}

	result := make([]listShareLinkResponse, 0, len(links))
	for _, link := range links {
		var expiresAt, revokedAt *int64
		if link.ExpiresAt != nil {
			unix := link.ExpiresAt.Unix()
			expiresAt = &unix
		}
		if link.RevokedAt != nil {
			unix := link.RevokedAt.Unix()
			revokedAt = &unix
		}
		result = append(result, listShareLinkResponse{
			ID:          link.ID,
			RelPath:     link.RelPath,
			ExpiresAt:   expiresAt,
			HasPassword: link.PasswordHash != "",
			RevokedAt:   revokedAt,
			CreatedBy:   link.CreatedBy,
			CreatedAt:   link.CreatedAt.Unix(),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"share_links": result})
}

// handleShareLinkByID handles DELETE /api/collections/{collectionID}/share-links/{linkId}
// collectionID and linkID are already validated by the router.
func (s *Server) handleShareLinkByID(w http.ResponseWriter, r *http.Request, collectionID, linkID string) {
	// Verify collection exists
	coll, err := catalog.GetCollection(s.db, collectionID)
	if err != nil {
		slog.Error("get collection for delete sharelink failed", "collection_id", collectionID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.revoke", "sharelink", linkID, "error", "get collection failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get collection failed")
		return
	}
	if coll == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.revoke", "sharelink", linkID, "error", "collection not found")
		s.writeJSONError(w, http.StatusNotFound, "collection not found")
		return
	}

	// Verify link exists and belongs to this collection
	var linkCollectionID string
	err = s.db.QueryRow(`SELECT collection_id FROM share_links WHERE id = ?`, linkID).Scan(&linkCollectionID)
	if err == sql.ErrNoRows {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.revoke", "sharelink", linkID, "error", "share link not found")
		s.writeJSONError(w, http.StatusNotFound, "share link not found")
		return
	}
	if err != nil {
		slog.Error("get sharelink collection failed", "link_id", linkID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.revoke", "sharelink", linkID, "error", "get sharelink failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get sharelink failed")
		return
	}
	if linkCollectionID != collectionID {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.revoke", "sharelink", linkID, "error", "share link not found in collection")
		s.writeJSONError(w, http.StatusNotFound, "share link not found")
		return
	}

	// Revoke (idempotent)
	if err := catalog.RevokeShareLink(s.db, linkID); err != nil {
		slog.Error("revoke sharelink failed", "link_id", linkID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.revoke", "sharelink", linkID, "error", "revoke sharelink failed")
		s.writeJSONError(w, http.StatusInternalServerError, "revoke sharelink failed")
		return
	}

	// Audit log success (detail = link ID only)
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "sharelink.revoke", "sharelink", linkID, "ok", linkID)

	w.WriteHeader(http.StatusNoContent)
}

// handlePublicShareAccess handles GET /api/public/share/{token} (NO auth required)
func (s *Server) handlePublicShareAccess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/public/share/")
	if path == "" || strings.Contains(path, "/") {
		http.NotFound(w, r)
		return
	}
	token := path

	// Hash token to look up
	tokenHash := sha256Hex(token)

	// Look up share link
	link, err := catalog.GetShareLinkByTokenHash(s.db, tokenHash)
	if err != nil {
		slog.Error("get sharelink by token failed", "error", err)
		// Generic 404 - don't distinguish
		catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", "", "error", "unknown token")
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}
	if link == nil {
		// Generic 404 - don't distinguish
		catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", "", "error", "unknown token")
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}

	// Check revoked
	if link.RevokedAt != nil {
		catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", link.ID, "error", "revoked")
		s.writeJSONError(w, http.StatusGone, "link revoked")
		return
	}

	// Check expired
	if link.ExpiresAt != nil && link.ExpiresAt.Before(time.Now()) {
		catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", link.ID, "error", "expired")
		s.writeJSONError(w, http.StatusGone, "link expired")
		return
	}

	// Check password if required
	if link.PasswordHash != "" {
		providedPassword := r.Header.Get("X-Share-Password")
		if providedPassword == "" || !verifyPassword(providedPassword, link.PasswordHash) {
			// Generic 401 - same body for missing or wrong password
			catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", link.ID, "error", "password required")
			s.writeJSONError(w, http.StatusUnauthorized, "password required")
			return
		}
	}

	// Get collection and pool for file serving
	coll, err := catalog.GetCollection(s.db, link.CollectionID)
	if err != nil || coll == nil {
		slog.Error("get collection for public share failed", "collection_id", link.CollectionID, "error", err)
		catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", link.ID, "error", "collection not found")
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}

	pool, _, err := catalog.GetPool(s.db, coll.PoolID)
	if err != nil || pool == nil {
		slog.Error("get pool for public share failed", "pool_id", coll.PoolID, "error", err)
		catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", link.ID, "error", "pool not found")
		s.writeJSONError(w, http.StatusNotFound, "not found")
		return
	}

	// Cold tier: return 409 with wake_required (same shape as download handler)
	if pool.Tier == "cold" {
		catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", link.ID, "error", "pool tier is cold: "+link.RelPath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":         "pool tier is cold, file is offline",
			"wake_required": true,
		})
		return
	}

	// Resolve filesystem root
	root, err := catalog.ResolveCollectionRoot(pool, coll)
	if err != nil {
		slog.Error("resolve collection root failed", "collection_id", link.CollectionID, "error", err)
		catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", link.ID, "error", "resolve root failed")
		s.writeJSONError(w, http.StatusInternalServerError, "resolve root failed")
		return
	}

	// Build candidate path
	candidate := filepath.Join(root, link.RelPath)
	candidate = filepath.Clean(candidate)

	// Safety: ensure candidate is within root
	if !strings.HasPrefix(candidate, root) {
		catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", link.ID, "error", "path escape after join: "+link.RelPath)
		s.writeJSONError(w, http.StatusBadRequest, "path escape not allowed")
		return
	}

	// Verify file exists and is a regular file (not a symlink)
	fi, err := os.Stat(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", link.ID, "error", "file not on disk: "+link.RelPath)
			s.writeJSONError(w, http.StatusNotFound, "file not found")
			return
		}
		slog.Error("stat file failed", "path", candidate, "error", err)
		catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", link.ID, "error", "stat file failed")
		s.writeJSONError(w, http.StatusInternalServerError, "stat file failed")
		return
	}
	if fi.IsDir() {
		catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", link.ID, "error", "path is a directory: "+link.RelPath)
		s.writeJSONError(w, http.StatusBadRequest, "path is a directory")
		return
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", link.ID, "error", "path is a symlink: "+link.RelPath)
		s.writeJSONError(w, http.StatusBadRequest, "symlinks are not supported")
		return
	}

	// Open file
	f, err := os.Open(candidate)
	if err != nil {
		slog.Error("open file failed", "path", candidate, "error", err)
		catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", link.ID, "error", "open file failed")
		s.writeJSONError(w, http.StatusInternalServerError, "open file failed")
		return
	}
	defer f.Close()

	// Content-Type from extension
	contentType := mime.TypeByExtension(filepath.Ext(candidate))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)

	// Content-Disposition attachment
	filename := filepath.Base(candidate)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))

	// Serve file
	http.ServeContent(w, r, filename, fi.ModTime(), f)

	// Audit log success
	catalog.WriteAuditLog(s.db, "public", "sharelink.access", "sharelink", link.ID, "ok", link.RelPath)

	// Record access event for heat (FR-1.3) - best effort, never fails the response
	// Actor format for public share: "share:<token-prefix-8>"
	shareActor := "share:" + token[:8]
	catalog.RecordAccessEvent(s.db, link.CollectionID, link.RelPath, shareActor, "share")
}
