package httpserver

// KelviOS Fase 1 WS11: secrets vault HTTP surface (FR-1.16).
//
// All routes behind requireAuth (wired in New). Secret VALUES never appear
// in any response, audit row, log, or error body — only names and short
// reason strings. There is deliberately NO endpoint returning a value.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/vault"
)

// vaultLockedStatus is 423 Locked for secret ops while the vault is locked.
const vaultLockedStatus = 423

type vaultPassphraseRequest struct {
	Passphrase string `json:"passphrase"`
}

type vaultSecretRequest struct {
	Value string `json:"value"`
}

func writeVaultError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleVaultSetup handles POST /api/vault/setup — one-time vault creation.
// Second setup → 409. Short passphrase → 400. Success leaves vault UNLOCKED.
func (s *Server) handleVaultSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req vaultPassphraseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "vault.setup", "vault", "vault", "error", "invalid JSON")
		writeVaultError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.vault.Setup(req.Passphrase); err != nil {
		switch {
		case errors.Is(err, vault.ErrVaultExists):
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "vault.setup", "vault", "vault", "error", "vault already exists")
			writeVaultError(w, http.StatusConflict, "vault already exists")
		case errors.Is(err, vault.ErrPassphraseTooShort):
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "vault.setup", "vault", "vault", "error", "passphrase too short")
			writeVaultError(w, http.StatusBadRequest, "passphrase must be at least 8 characters")
		default:
			slog.Error("vault setup failed", "error", err)
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "vault.setup", "vault", "vault", "error", "setup failed")
			writeVaultError(w, http.StatusInternalServerError, "setup failed")
		}
		return
	}
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "vault.setup", "vault", "vault", "ok", "vault created")
	st := s.vault.Status()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"exists": st.Exists, "unlocked": st.Unlocked})
}

// handleVaultUnlock handles POST /api/vault/unlock. Wrong passphrase → 401
// with the SAME generic body shape as login failures (no oracle).
func (s *Server) handleVaultUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var req vaultPassphraseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "vault.unlock", "vault", "vault", "error", "invalid JSON")
		writeVaultError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if err := s.vault.Unlock(req.Passphrase); err != nil {
		switch {
		case errors.Is(err, vault.ErrInvalidPassphrase):
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "vault.unlock", "vault", "vault", "error", "invalid passphrase")
			writeVaultError(w, http.StatusUnauthorized, authGenericFailBody)
		case errors.Is(err, vault.ErrVaultNotFound):
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "vault.unlock", "vault", "vault", "error", "vault does not exist")
			writeVaultError(w, http.StatusNotFound, "vault does not exist")
		default:
			slog.Error("vault unlock failed", "error", err)
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "vault.unlock", "vault", "vault", "error", "unlock failed")
			writeVaultError(w, http.StatusInternalServerError, "unlock failed")
		}
		return
	}
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "vault.unlock", "vault", "vault", "ok", "vault unlocked")
	st := s.vault.Status()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"exists": st.Exists, "unlocked": st.Unlocked})
}

// handleVaultLock handles POST /api/vault/lock — drops the key. 204.
func (s *Server) handleVaultLock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	s.vault.Lock()
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "vault.lock", "vault", "vault", "ok", "vault locked")
	w.WriteHeader(http.StatusNoContent)
}

// handleVaultStatus handles GET /api/vault/status — {exists, unlocked} only.
func (s *Server) handleVaultStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	st := s.vault.Status()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"exists": st.Exists, "unlocked": st.Unlocked})
}

// handleSecrets handles GET /api/secrets — list of {name, created_at,
// updated_at} ONLY. Never returns values.
func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	names, err := s.vault.ListNames()
	if err != nil {
		slog.Error("list secrets failed", "error", err)
		writeVaultError(w, http.StatusInternalServerError, "list secrets failed")
		return
	}
	result := make([]map[string]any, 0, len(names))
	for _, n := range names {
		result = append(result, map[string]any{
			"name":       n.Name,
			"created_at": n.CreatedAt.Unix(),
			"updated_at": n.UpdatedAt.Unix(),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"secrets": result})
}

// handleSecretByName handles PUT/DELETE /api/secrets/{name}.
// Locked vault → 423. Missing name on DELETE → 404.
func (s *Server) handleSecretByName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/secrets/")
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var req vaultSecretRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "secret.put", "secret", name, "error", "invalid JSON")
			writeVaultError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if err := s.vault.Put(name, req.Value); err != nil {
			if errors.Is(err, vault.ErrVaultLocked) {
				catalog.WriteAuditLog(s.db, actorFromRequest(r), "secret.put", "secret", name, "error", "vault is locked")
				writeVaultError(w, vaultLockedStatus, "vault is locked")
				return
			}
			slog.Error("secret put failed", "name", name, "error", err)
			catalog.WriteAuditLog(s.db, actorFromRequest(r), "secret.put", "secret", name, "error", "store failed")
			writeVaultError(w, http.StatusInternalServerError, "store failed")
			return
		}
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "secret.put", "secret", name, "ok", "secret stored")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"name": name})
	case http.MethodDelete:
		if err := s.vault.Delete(name); err != nil {
			switch {
			case errors.Is(err, vault.ErrVaultLocked):
				catalog.WriteAuditLog(s.db, actorFromRequest(r), "secret.delete", "secret", name, "error", "vault is locked")
				writeVaultError(w, vaultLockedStatus, "vault is locked")
			case errors.Is(err, vault.ErrSecretNotFound):
				catalog.WriteAuditLog(s.db, actorFromRequest(r), "secret.delete", "secret", name, "error", "secret not found")
				writeVaultError(w, http.StatusNotFound, "secret not found")
			default:
				slog.Error("secret delete failed", "name", name, "error", err)
				catalog.WriteAuditLog(s.db, actorFromRequest(r), "secret.delete", "secret", name, "error", "delete failed")
				writeVaultError(w, http.StatusInternalServerError, "delete failed")
			}
			return
		}
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "secret.delete", "secret", name, "ok", "secret deleted")
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}
