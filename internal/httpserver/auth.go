package httpserver

// KelviOS Fase 1 WS9: authentication core — admin setup, login, sessions, lockout.
//
// Hard rule: secrets (passwords, tokens, password hashes) NEVER appear in
// audit detail, logs, or error bodies. Only short reason strings are recorded.
//
// NOTE: Chat Bridge lockout notification (FR-3.8) IS built — see
// internal/chatbridge/notify.go NotifyLockout. The TV admin PIN throttling
// will reuse this same tracker pattern later (note only, not implemented).

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
)

// Auth tuning constants.
const (
	authMinPasswordLen   = 12
	authMaxFailures      = 5
	authDefaultLockMin   = 15
	authSessionTTL       = 24 * time.Hour
	authBackoffBaseMs    = 100
	authBackoffCapMs     = 2000
	pbkdf2Iterations     = 210000
	pbkdf2SaltLen        = 16
	pbkdf2KeyLen         = 32
	authGenericFailBody  = "invalid username or password"
	authLockedBody       = "account locked, try again later"
)

// lockoutMinutes reads KELVIOS_LOCKOUT_MINUTES (default 15).
func lockoutMinutes() int {
	if v := os.Getenv("KELVIOS_LOCKOUT_MINUTES"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return authDefaultLockMin
}

// backoffDelayMs returns 100ms * 2^failures capped at 2000ms.
func backoffDelayMs(failures int) time.Duration {
	ms := authBackoffBaseMs
	for i := 0; i < failures && ms < authBackoffCapMs; i++ {
		ms *= 2
	}
	if ms > authBackoffCapMs {
		ms = authBackoffCapMs
	}
	return time.Duration(ms) * time.Millisecond
}

// generateAccountID returns a collision-proof account ID.
// Format: acct-<unix-nano>-<8 hex chars from crypto/rand> (WS5 pattern, stdlib only).
func generateAccountID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("acct-%d-%08x", now, counter)
	}
	return fmt.Sprintf("acct-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// pbkdf2Key implements PBKDF2-HMAC-SHA256 with stdlib only (no x/crypto).
func pbkdf2Key(password, salt []byte, iter, keyLen int) []byte {
	hLen := sha256.Size
	blocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, blocks*hLen)
	var counter [4]byte
	for i := 1; i <= blocks; i++ {
		counter[0] = byte(i >> 24)
		counter[1] = byte(i >> 16)
		counter[2] = byte(i >> 8)
		counter[3] = byte(i)
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		mac.Write(counter[:])
		u := mac.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for j := 1; j < iter; j++ {
			mac = hmac.New(sha256.New, password)
			mac.Write(u)
			u = mac.Sum(nil)
			for k := range t {
				t[k] ^= u[k]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// hashPassword encodes params + salt + derived key as "v1$<iter>$<salthex>$<keyhex>".
func hashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := pbkdf2Key([]byte(password), salt, pbkdf2Iterations, pbkdf2KeyLen)
	return fmt.Sprintf("v1$%d$%s$%s", pbkdf2Iterations, hex.EncodeToString(salt), hex.EncodeToString(key)), nil
}

// verifyPassword compares a password against an encoded hash (constant-time).
func verifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "v1" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := hex.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got := pbkdf2Key([]byte(password), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// sha256Hex returns the hex SHA-256 of s (for token storage).
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

type authCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func writeAuthError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// handleAuthSetup handles POST /api/auth/setup — one-time admin creation.
func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var cred authCredential
	if err := json.NewDecoder(r.Body).Decode(&cred); err != nil {
		writeAuthError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	cred.Username = strings.TrimSpace(cred.Username)
	if cred.Username == "" {
		writeAuthError(w, http.StatusBadRequest, "username is required")
		return
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&count); err != nil {
		slog.Error("count accounts failed", "error", err)
		writeAuthError(w, http.StatusInternalServerError, "setup failed")
		return
	}
	if count > 0 {
		writeAuthError(w, http.StatusConflict, "setup is closed")
		return
	}
	if len([]rune(cred.Password)) < authMinPasswordLen {
		writeAuthError(w, http.StatusBadRequest, "password must be at least 12 characters")
		return
	}
	hash, err := hashPassword(cred.Password)
	if err != nil {
		slog.Error("hash password failed", "error", err)
		writeAuthError(w, http.StatusInternalServerError, "setup failed")
		return
	}
	id := generateAccountID()
	now := time.Now().Unix()
	if _, err := s.db.Exec(
		`INSERT INTO accounts (id, username, password_hash, failed_count, locked_until, created_at) VALUES (?, ?, ?, 0, 0, ?)`,
		id, cred.Username, hash, now,
	); err != nil {
		// Race: a concurrent setup won — treat as closed.
		if strings.Contains(err.Error(), "UNIQUE") {
			writeAuthError(w, http.StatusConflict, "setup is closed")
			return
		}
		slog.Error("create account failed", "error", err)
		writeAuthError(w, http.StatusInternalServerError, "setup failed")
		return
	}
	catalog.WriteAuditLog(s.db, cred.Username, "auth.setup", "auth", cred.Username, "ok", "admin account created")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"username": cred.Username})
}

// accountRow mirrors the accounts table.
type accountRow struct {
	ID           string
	Username     string
	PasswordHash string
	FailedCount  int
	LockedUntil  int64
}

func getAccount(db *sql.DB, username string) (*accountRow, error) {
	var a accountRow
	err := db.QueryRow(
		`SELECT id, username, password_hash, failed_count, locked_until FROM accounts WHERE username = ?`,
		username,
	).Scan(&a.ID, &a.Username, &a.PasswordHash, &a.FailedCount, &a.LockedUntil)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// handleAuthLogin handles POST /api/auth/login.
func (s *Server) handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	var cred authCredential
	if err := json.NewDecoder(r.Body).Decode(&cred); err != nil {
		writeAuthError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	username := strings.TrimSpace(cred.Username)

	acct, err := getAccount(s.db, username)
	if err == sql.ErrNoRows || username == "" {
		// Unknown user: same generic body, fixed small delay (no oracle).
		time.Sleep(backoffDelayMs(0))
		catalog.WriteAuditLog(s.db, username, "auth.login", "auth", username, "error", "invalid credentials")
		writeAuthError(w, http.StatusUnauthorized, authGenericFailBody)
		return
	}
	if err != nil {
		slog.Error("get account failed", "error", err)
		writeAuthError(w, http.StatusInternalServerError, "login failed")
		return
	}

	now := time.Now().Unix()
	if acct.LockedUntil > now {
		catalog.WriteAuditLog(s.db, acct.Username, "auth.login", "auth", acct.Username, "error", "account locked")
		writeAuthError(w, 423, authLockedBody)
		return
	}
	// Lock expired naturally: clear stale counters before checking password.
	if acct.LockedUntil != 0 && acct.LockedUntil <= now {
		_, _ = s.db.Exec(`UPDATE accounts SET failed_count = 0, locked_until = 0 WHERE id = ?`, acct.ID)
		acct.FailedCount = 0
		acct.LockedUntil = 0
	}

	if !verifyPassword(cred.Password, acct.PasswordHash) {
		failures := acct.FailedCount + 1
		if failures >= authMaxFailures {
			lockUntil := time.Now().Unix() + int64(lockoutMinutes()*60)
			_, _ = s.db.Exec(`UPDATE accounts SET failed_count = ?, locked_until = ? WHERE id = ?`, failures, lockUntil, acct.ID)
			catalog.WriteAuditLog(s.db, acct.Username, "auth.lockout", "auth", acct.Username, "error", "account locked after 5 consecutive failures")
			// Fire-and-forget lockout notification via Chat Bridge (FR-1.15).
			if s.chatBridge != nil {
				go s.chatBridge.NotifyLockout(acct.Username, lockoutMinutes())
			}
			catalog.WriteAuditLog(s.db, acct.Username, "auth.login", "auth", acct.Username, "error", "invalid credentials")
			time.Sleep(backoffDelayMs(failures))
			writeAuthError(w, http.StatusUnauthorized, authGenericFailBody)
			return
		}
		_, _ = s.db.Exec(`UPDATE accounts SET failed_count = ? WHERE id = ?`, failures, acct.ID)
		catalog.WriteAuditLog(s.db, acct.Username, "auth.login", "auth", acct.Username, "error", "invalid credentials")
		time.Sleep(backoffDelayMs(failures))
		writeAuthError(w, http.StatusUnauthorized, authGenericFailBody)
		return
	}

	// Success: reset counter, issue session.
	_, _ = s.db.Exec(`UPDATE accounts SET failed_count = 0, locked_until = 0 WHERE id = ?`, acct.ID)
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		slog.Error("generate token failed", "error", err)
		writeAuthError(w, http.StatusInternalServerError, "login failed")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	created := time.Now()
	expires := created.Add(authSessionTTL)
	if _, err := s.db.Exec(
		`INSERT INTO sessions (token_hash, username, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		sha256Hex(token), acct.Username, created.Unix(), expires.Unix(),
	); err != nil {
		slog.Error("create session failed", "error", err)
		writeAuthError(w, http.StatusInternalServerError, "login failed")
		return
	}
	catalog.WriteAuditLog(s.db, acct.Username, "auth.login", "auth", acct.Username, "ok", "login success")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "expires_at": expires.Unix()})
}

// sessionUsername resolves a Bearer token to a username ("" = invalid/expired).
func (s *Server) sessionUsername(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" || !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if token == "" {
		return ""
	}
	var username string
	var expiresAt int64
	err := s.db.QueryRow(`SELECT username, expires_at FROM sessions WHERE token_hash = ?`, sha256Hex(token)).Scan(&username, &expiresAt)
	if err != nil {
		return ""
	}
	if expiresAt <= time.Now().Unix() {
		return ""
	}
	return username
}

// handleAuthMe handles GET /api/auth/me.
func (s *Server) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	username := s.sessionUsername(r)
	if username == "" {
		writeAuthError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"username": username})
}

// handleAuthLogout handles POST /api/auth/logout (idempotent 204).
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	auth := r.Header.Get("Authorization")
	if token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")); strings.HasPrefix(auth, "Bearer ") && token != "" {
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, sha256Hex(token))
	}
	w.WriteHeader(http.StatusNoContent)
}
