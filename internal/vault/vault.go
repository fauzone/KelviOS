package vault

// KelviOS Fase 1 WS11: secrets vault (FR-1.16) — core-managed encrypted vault.
//
// AES-256-GCM. Vault key derived from a vault passphrase via PBKDF2-HMAC-SHA256
// (stdlib only). The derived key lives ONLY in process memory while unlocked;
// the server always starts LOCKED (LoadVaultConfig must run at boot).
// Secret VALUES never appear in logs, audit rows, or error messages —
// only names and short reason strings.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	vaultMinPassphraseLen = 8
	vaultPBKDF2Iterations = 210000
	vaultPBKDF2SaltLen    = 16
	vaultPBKDF2KeyLen     = 32
	vaultGCMNonceSize     = 12
	vaultConfigID         = "vault_key"
	vaultVerifierLabel    = "kelvios-vault-verifier"
)

// Sentinel errors so HTTP handlers can map to status codes without
// leaking secret material into error text.
var (
	ErrVaultExists       = errors.New("vault already exists")
	ErrVaultNotFound     = errors.New("vault does not exist")
	ErrPassphraseTooShort = errors.New("passphrase too short")
	ErrInvalidPassphrase = errors.New("invalid passphrase")
	ErrVaultLocked       = errors.New("vault is locked")
	ErrSecretNotFound    = errors.New("secret not found")
)

// Vault stores the vault state and the in-memory key.
type Vault struct {
	db       *sql.DB
	key      []byte // AES-256-GCM key, only in memory when unlocked
	keyMu    sync.RWMutex
	exists   bool
	unlocked bool
	salt     []byte // PBKDF2 salt, stored in DB (BLOB)
	verifier []byte // passphrase verifier, stored in DB (BLOB)
}

// VaultStatus is the public vault state (exists/unlocked only).
type VaultStatus struct {
	Exists   bool `json:"exists"`
	Unlocked bool `json:"unlocked"`
}

// SecretMetadata holds secret metadata WITHOUT its value.
type SecretMetadata struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// New creates a Vault bound to db. Call LoadVaultConfig after New so the
// server always boots with the correct exists flag and starts LOCKED.
func New(db *sql.DB) *Vault {
	return &Vault{db: db}
}

// Setup initializes the vault with a new passphrase. Leaves the vault
// UNLOCKED for this process. Second setup returns ErrVaultExists.
func (v *Vault) Setup(passphrase string) error {
	if len([]rune(passphrase)) < vaultMinPassphraseLen {
		return ErrPassphraseTooShort
	}

	v.keyMu.Lock()
	defer v.keyMu.Unlock()

	if v.exists {
		return ErrVaultExists
	}

	salt := make([]byte, vaultPBKDF2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generate salt: %w", err)
	}

	key := pbkdf2Key([]byte(passphrase), salt, vaultPBKDF2Iterations, vaultPBKDF2KeyLen)
	verifier := vaultVerifier(key)

	now := time.Now().Unix()
	_, err := v.db.Exec(`
		INSERT INTO vault_config (id, salt, verifier, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
	`, vaultConfigID, salt, verifier, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			v.exists = true
			v.unlocked = false
			return ErrVaultExists
		}
		return fmt.Errorf("insert vault config: %w", err)
	}

	v.key = key
	v.salt = salt
	v.verifier = verifier
	v.exists = true
	v.unlocked = true
	slog.Info("vault setup complete and unlocked")
	return nil
}

// Unlock derives the key from passphrase and unlocks the vault.
// Wrong passphrase returns ErrInvalidPassphrase (no oracle detail).
func (v *Vault) Unlock(passphrase string) error {
	v.keyMu.Lock()
	defer v.keyMu.Unlock()

	if !v.exists {
		return ErrVaultNotFound
	}

	key := pbkdf2Key([]byte(passphrase), v.salt, vaultPBKDF2Iterations, vaultPBKDF2KeyLen)
	if subtle.ConstantTimeCompare(vaultVerifier(key), v.verifier) != 1 {
		return ErrInvalidPassphrase
	}

	if !v.unlocked {
		v.key = key
		v.unlocked = true
		slog.Info("vault unlocked")
	}
	return nil
}

// Lock drops the in-memory key. Idempotent.
func (v *Vault) Lock() {
	v.keyMu.Lock()
	defer v.keyMu.Unlock()

	if v.unlocked {
		for i := range v.key {
			v.key[i] = 0
		}
		v.key = nil
		v.unlocked = false
		slog.Info("vault locked")
	}
}

// Status returns the public vault state.
func (v *Vault) Status() VaultStatus {
	v.keyMu.RLock()
	defer v.keyMu.RUnlock()
	return VaultStatus{Exists: v.exists, Unlocked: v.unlocked}
}

// Put creates or replaces a secret. Requires unlocked vault.
func (v *Vault) Put(name, value string) error {
	v.keyMu.RLock()
	defer v.keyMu.RUnlock()

	if !v.unlocked {
		return ErrVaultLocked
	}

	block, err := aes.NewCipher(v.key)
	if err != nil {
		return fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("create GCM: %w", err)
	}
	nonce := make([]byte, vaultGCMNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, []byte(value), nil)

	now := time.Now().Unix()
	_, err = v.db.Exec(`
		INSERT INTO secrets (name, ciphertext, nonce, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET ciphertext = ?, nonce = ?, updated_at = ?
	`, name, ciphertext, nonce, now, now, ciphertext, nonce, now)
	if err != nil {
		return fmt.Errorf("store secret: %w", err)
	}
	slog.Info("secret stored", "name", name)
	return nil
}

// Get decrypts and returns a secret. Internal use only — values must never
// leave the process via API, log, audit, or error. Requires unlocked vault.
func (v *Vault) Get(name string) (string, error) {
	v.keyMu.RLock()
	defer v.keyMu.RUnlock()

	if !v.unlocked {
		return "", ErrVaultLocked
	}

	var ciphertext, nonce []byte
	err := v.db.QueryRow(
		`SELECT ciphertext, nonce FROM secrets WHERE name = ?`, name,
	).Scan(&ciphertext, &nonce)
	if err == sql.ErrNoRows {
		return "", ErrSecretNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get secret: %w", err)
	}

	block, err := aes.NewCipher(v.key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt secret: %w", err)
	}
	return string(plaintext), nil
}

// Delete removes a secret. Requires unlocked vault.
func (v *Vault) Delete(name string) error {
	v.keyMu.RLock()
	defer v.keyMu.RUnlock()

	if !v.unlocked {
		return ErrVaultLocked
	}

	res, err := v.db.Exec(`DELETE FROM secrets WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrSecretNotFound
	}
	slog.Info("secret deleted", "name", name)
	return nil
}

// ListNames returns secret metadata WITHOUT values. Works while locked
// (no key needed) — callers must still never expose values.
func (v *Vault) ListNames() ([]SecretMetadata, error) {
	rows, err := v.db.Query(`SELECT name, created_at, updated_at FROM secrets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list secret names: %w", err)
	}
	defer rows.Close()

	secrets := []SecretMetadata{}
	for rows.Next() {
		var s SecretMetadata
		var createdAt, updatedAt int64
		if err := rows.Scan(&s.Name, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan secret metadata: %w", err)
		}
		s.CreatedAt = time.Unix(createdAt, 0)
		s.UpdatedAt = time.Unix(updatedAt, 0)
		secrets = append(secrets, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate secret names: %w", err)
	}
	return secrets, nil
}

// LoadVaultConfig loads salt/verifier from DB at boot. The server ALWAYS
// starts LOCKED when a vault exists; missing row means no vault yet.
func (v *Vault) LoadVaultConfig() error {
	v.keyMu.Lock()
	defer v.keyMu.Unlock()

	var salt, verifier []byte
	err := v.db.QueryRow(
		`SELECT salt, verifier FROM vault_config WHERE id = ?`, vaultConfigID,
	).Scan(&salt, &verifier)

	if err == sql.ErrNoRows {
		v.exists = false
		v.unlocked = false
		slog.Info("vault does not exist, awaiting setup")
		return nil
	}
	if err != nil {
		return fmt.Errorf("load vault config: %w", err)
	}

	v.salt = salt
	v.verifier = verifier
	v.exists = true
	v.unlocked = false // always start locked
	slog.Info("vault config loaded, currently locked")
	return nil
}

// vaultVerifier derives the stored verifier from a key (HMAC-SHA256 of a
// fixed label). Wrong passphrases are detectable without decrypting.
func vaultVerifier(key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(vaultVerifierLabel))
	return mac.Sum(nil)
}

// pbkdf2Key implements PBKDF2-HMAC-SHA256 with stdlib only (no x/crypto).
// Mirrors internal/httpserver/auth.go parameters.
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
