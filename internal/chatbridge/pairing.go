package chatbridge

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
)

// CreatePairingCode generates an 8-character pairing code with 10-minute expiry.
// Stores it in chat_pairing_codes table.
func (s *Service) CreatePairingCode(transport string) (*PairingCode, error) {
	// Generate 8-char code (alphanumeric)
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("random bytes: %w", err)
	}
	code := make([]byte, 8)
	for i := range code {
		code[i] = charset[int(b[i])%len(charset)]
	}
	codeStr := string(code)

	now := time.Now()
	expiresAt := now.Add(10 * time.Minute)

	_, err := s.db.Exec(`
		INSERT INTO chat_pairing_codes (code, transport, expires_at, used_at)
		VALUES (?, ?, ?, NULL)
	`, codeStr, transport, expiresAt.Unix())
	if err != nil {
		return nil, fmt.Errorf("insert pairing code: %w", err)
	}

	return &PairingCode{
		Code:      codeStr,
		Transport: transport,
		ExpiresAt: expiresAt,
		UsedAt:    nil,
	}, nil
}

// GetPairingCode retrieves a pairing code by code string.
// Returns nil if not found or expired/used.
func (s *Service) GetPairingCode(code string) (*PairingCode, error) {
	row := s.db.QueryRow(`
		SELECT code, transport, expires_at, used_at
		FROM chat_pairing_codes WHERE code = ?
	`, code)
	var pc PairingCode
	var expiresAt, usedAt sql.NullInt64
	err := row.Scan(&pc.Code, &pc.Transport, &expiresAt, &usedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan pairing code: %w", err)
	}
	pc.ExpiresAt = time.Unix(expiresAt.Int64, 0)
	if usedAt.Valid {
		t := time.Unix(usedAt.Int64, 0)
		pc.UsedAt = &t
	}
	// Check expiry
	if time.Now().After(pc.ExpiresAt) {
		return nil, nil // treat expired as not found
	}
	if pc.UsedAt != nil {
		return nil, nil // treat used as not found
	}
	return &pc, nil
}

// MarkPairingCodeUsed marks a pairing code as used.
func (s *Service) MarkPairingCodeUsed(code string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`UPDATE chat_pairing_codes SET used_at = ? WHERE code = ?`, now, code)
	return err
}

// hashPIN creates a salted SHA-256 hash of the PIN.
// Returns hex-encoded "salt:hash".
func hashPIN(pin string) (string, error) {
	salt := make([]byte, 16)
	if _, err := catalog.RandRead(salt); err != nil {
		return "", fmt.Errorf("random salt: %w", err)
	}
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(pin))
	hash := h.Sum(nil)
	return hex.EncodeToString(salt) + ":" + hex.EncodeToString(hash), nil
}

// verifyPIN verifies a PIN against a stored salt:hash.
func verifyPIN(pin, storedHash string) bool {
	parts := splitSaltHash(storedHash)
	if len(parts) != 2 {
		return false
	}
	saltHex, hashHex := parts[0], parts[1]
	salt, err := hex.DecodeString(saltHex)
	if err != nil {
		return false
	}
	expectedHash, err := hex.DecodeString(hashHex)
	if err != nil {
		return false
	}
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(pin))
	computed := h.Sum(nil)
	// Constant-time comparison
	if len(computed) != len(expectedHash) {
		return false
	}
	var diff byte
	for i := range computed {
		diff |= computed[i] ^ expectedHash[i]
	}
	return diff == 0
}

func splitSaltHash(s string) []string {
	// Simple split on ':'
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}

// CreateLink creates a new chat link (allowlist entry) with the given PIN hash.
func (s *Service) CreateLink(transport, chatID, label, pinHash string) (*ChatLink, error) {
	now := time.Now().Unix()
	id := generateLinkID()
	_, err := s.db.Exec(`
		INSERT INTO chat_links (id, transport, chat_id, label, pin_hash, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, id, transport, chatID, label, pinHash, now, now)
	if err != nil {
		return nil, fmt.Errorf("insert chat link: %w", err)
	}
	return s.GetLink(transport, chatID)
}

// GetLink retrieves a chat link by transport and chat_id.
func (s *Service) GetLink(transport, chatID string) (*ChatLink, error) {
	row := s.db.QueryRow(`
		SELECT id, transport, chat_id, label, pin_hash, created_at, last_seen_at
		FROM chat_links WHERE transport = ? AND chat_id = ?
	`, transport, chatID)
	return scanChatLink(row)
}

// GetLinkByID retrieves a chat link by ID.
func (s *Service) GetLinkByID(id string) (*ChatLink, error) {
	row := s.db.QueryRow(`
		SELECT id, transport, chat_id, label, pin_hash, created_at, last_seen_at
		FROM chat_links WHERE id = ?
	`, id)
	return scanChatLink(row)
}

// ListLinks returns all chat links.
func (s *Service) ListLinks() ([]*ChatLink, error) {
	rows, err := s.db.Query(`
		SELECT id, transport, chat_id, label, pin_hash, created_at, last_seen_at
		FROM chat_links ORDER BY created_at
	`)
	if err != nil {
		return nil, fmt.Errorf("query chat links: %w", err)
	}
	defer rows.Close()

	var links []*ChatLink
	for rows.Next() {
		link, err := scanChatLink(rows)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

// DeleteLink deletes a chat link by ID.
func (s *Service) DeleteLink(id string) error {
	res, err := s.db.Exec(`DELETE FROM chat_links WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete chat link: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateLastSeen updates the last_seen_at timestamp for a link.
func (s *Service) UpdateLastSeen(id string) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`UPDATE chat_links SET last_seen_at = ? WHERE id = ?`, now, id)
	return err
}

func scanChatLink(scanner interface {
	Scan(dest ...any) error
}) (*ChatLink, error) {
	var link ChatLink
	var createdAt, lastSeenAt int64
	var label, pinHash sql.NullString
	err := scanner.Scan(&link.ID, &link.Transport, &link.ChatID, &label, &pinHash, &createdAt, &lastSeenAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan chat link: %w", err)
	}
	if label.Valid {
		link.Label = label.String
	}
	if pinHash.Valid {
		link.PinHash = pinHash.String
	}
	link.CreatedAt = time.Unix(createdAt, 0)
	link.LastSeenAt = time.Unix(lastSeenAt, 0)
	return &link, nil
}

func generateLinkID() string {
	now := time.Now().UnixNano()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("link-%d-%08x", now, time.Now().UnixNano()&0xffffffff)
	}
	return fmt.Sprintf("link-%d-%s", now, hex.EncodeToString(b))
}