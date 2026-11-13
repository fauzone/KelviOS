package chatbridge

import (
	"context"
	"database/sql"
	"time"

	"github.com/fauzone/kelvios/internal/approval"
)

// Transport is the interface that concrete chat transports (WhatsApp, Telegram, etc.) must implement.
type Transport interface {
	// Name returns the transport identifier (e.g., "whatsapp", "telegram").
	Name() string
	// Send sends a text message to the given chat ID.
	// Returns error if the message could not be sent.
	Send(chatID, text string) error
}

// LifecycleTransport extends Transport with Start/Stop lifecycle methods.
// Transports that implement this interface will be started/stopped by Service.Start/Stop.
type LifecycleTransport interface {
	Transport
	// Start begins the transport's background work (e.g., long-polling loop).
	// It runs until Stop() is called or context is cancelled.
	Start(ctx context.Context)
	// Stop stops the transport's background work and waits for it to exit.
	Stop()
}

// InboundMessage represents a message received from a transport.
type InboundMessage struct {
	Transport string
	ChatID    string
	SenderID  string
	Text      string
	At        time.Time
}

// ChatLink represents a paired chat (allowlist entry).
type ChatLink struct {
	ID          string
	Transport   string
	ChatID      string
	Label       string
	PinHash     string // salt:hash (hex-encoded)
	CreatedAt   time.Time
	LastSeenAt  time.Time
}

// PairingCode represents a one-time pairing code.
type PairingCode struct {
	Code      string
	Transport string
	ExpiresAt time.Time
	UsedAt    *time.Time
}

// Deps holds the dependencies for the chatbridge Service.
// Mirrors the mcp.Deps style.
type Deps struct {
	DB            *sql.DB
	ApprovalStore *approval.ApprovalStore
	HarnessRun    func(ctx context.Context, prompt string, tools []string) (string, error)
	ReportBuilder func(db *sql.DB, now time.Time) (string, error)
	LinkStore     LinkStore
}

// LinkStore defines the interface for chat link persistence.
// Implemented by the Service itself (internal methods), but exposed for testing.
type LinkStore interface {
	CreateLink(transport, chatID, label, pinHash string) (*ChatLink, error)
	GetLink(transport, chatID string) (*ChatLink, error)
	GetLinkByID(id string) (*ChatLink, error)
	ListLinks() ([]*ChatLink, error)
	DeleteLink(id string) error
	UpdateLastSeen(id string) error
	CreatePairingCode(transport string) (*PairingCode, error)
	GetPairingCode(code string) (*PairingCode, error)
	MarkPairingCodeUsed(code string) error
}

// PINFailureTracker tracks consecutive PIN failures per link for lockout.
type PINFailureTracker struct {
	failures map[string]int       // linkID -> count
	locked   map[string]time.Time // linkID -> unlock time
}

// NewPINFailureTracker creates a new tracker.
func NewPINFailureTracker() *PINFailureTracker {
	return &PINFailureTracker{
		failures: make(map[string]int),
		locked:   make(map[string]time.Time),
	}
}

// RecordFailure increments the failure count for a link.
// Returns true if the link is now locked (3 consecutive failures).
func (t *PINFailureTracker) RecordFailure(linkID string) bool {
	t.failures[linkID]++
	if t.failures[linkID] >= 3 {
		t.locked[linkID] = time.Now().Add(15 * time.Minute)
		return true
	}
	return false
}

// IsLocked returns true if the link is currently locked.
func (t *PINFailureTracker) IsLocked(linkID string) bool {
	unlockTime, ok := t.locked[linkID]
	if !ok {
		return false
	}
	if time.Now().After(unlockTime) {
		delete(t.locked, linkID)
		delete(t.failures, linkID)
		return false
	}
	return true
}

// Reset clears failures for a link (on successful PIN verification).
func (t *PINFailureTracker) Reset(linkID string) {
	delete(t.failures, linkID)
	delete(t.locked, linkID)
}
