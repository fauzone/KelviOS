package chatbridge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// WhatsAppTransport implements the Transport interface for WhatsApp native protocol.
type WhatsAppTransport struct {
	client     *whatsmeow.Client
	svc        *Service
	storePath  string
	db         *sql.DB
	container  *sqlstore.Container

	mu           sync.Mutex
	running      bool
	stopCh       chan struct{}
	disconnectCh chan struct{}
	wg           sync.WaitGroup
	qrCode       string
	qrExpiry     time.Time
	paired       bool
}

// NewWhatsAppTransport creates a new WhatsApp transport.
// svc is the chatbridge service for inbound message handling.
// dbPath is the catalog database path; the device store is placed alongside
// it (<name>_whatsapp.db). Pass "" to use the plain default name.
// Returns an error if the store cannot be initialized.
func NewWhatsAppTransport(svc *Service, dbPath string) (*WhatsAppTransport, error) {
	storePath := os.Getenv("KELVIOS_WHATSAPP_STORE_PATH")
	if storePath == "" {
		if dbPath == "" {
			dbPath = "kelvios.db"
		}
		storePath = strings.TrimSuffix(dbPath, ".db") + "_whatsapp.db"
	}

	// Open SQLite database for whatsmeow store.
	// Pragmas are set explicitly (catalog.go pattern): the mattn-style DSN
	// parameters (?_foreign_keys=on) do NOT take effect under modernc.org/sqlite,
	// and sqlstore.Upgrade fails when foreign_keys is off.
	db, err := sql.Open("sqlite", storePath)
	if err != nil {
		return nil, fmt.Errorf("whatsapp: open store db: %w", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("whatsapp: set journal_mode: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("whatsapp: set busy_timeout: %w", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		db.Close()
		return nil, fmt.Errorf("whatsapp: set foreign_keys: %w", err)
	}
	var fkEnabled int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&fkEnabled); err != nil || fkEnabled != 1 {
		db.Close()
		return nil, fmt.Errorf("whatsapp: foreign_keys not enabled (got %d, err %v)", fkEnabled, err)
	}

	// Create whatsmeow SQL store container
	container := sqlstore.NewWithDB(db, "sqlite", waLog.Stdout("whatsapp-store", "INFO", false))
	if err := container.Upgrade(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("whatsapp: upgrade store: %w", err)
	}

	// Get or create device
	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("whatsapp: get device: %w", err)
	}

	client := whatsmeow.NewClient(device, waLog.Stdout("whatsapp-client", "INFO", false))

	t := &WhatsAppTransport{
		client:    client,
		svc:       svc,
		storePath: storePath,
		db:        db,
		container: container,
		stopCh:    make(chan struct{}),
	}

	// Register event handler for inbound messages
	client.AddEventHandler(t.handleEvent)

	return t, nil
}

// redact strips sensitive data from errors before logging.
// whatsmeow errors may contain JIDs or other identifiers.
func (t *WhatsAppTransport) redact(err error) error {
	if err == nil {
		return nil
	}
	// whatsmeow doesn't embed tokens in URLs like Telegram, but JIDs
	// could be considered sensitive. We'll redact common patterns.
	msg := err.Error()
	// Redact JIDs (user@s.whatsapp.net or similar)
	msg = strings.ReplaceAll(msg, "@s.whatsapp.net", "@***")
	msg = strings.ReplaceAll(msg, "@c.us", "@***")
	msg = strings.ReplaceAll(msg, "@g.us", "@***")
	return errors.New(msg)
}

// Name returns the transport identifier.
func (t *WhatsAppTransport) Name() string {
	return "whatsapp"
}

// Send sends a text message to the given chat ID via WhatsApp.
// chatID is the user part of the JID (e.g., "628123456789").
// Returns error if not connected or send fails.
func (t *WhatsAppTransport) Send(chatID, text string) error {
	t.mu.Lock()
	running := t.running
	client := t.client
	t.mu.Unlock()

	if !running || client == nil {
		return errors.New("whatsapp: transport not connected")
	}

	// Build JID from user part
	jid := types.NewJID(chatID, types.DefaultUserServer)
	if jid.User == "" {
		return fmt.Errorf("whatsapp: invalid chat ID: %s", chatID)
	}

	msg := &waE2E.Message{
		Conversation: &text,
	}

	_, err := client.SendMessage(context.Background(), jid, msg)
	if err != nil {
		return fmt.Errorf("whatsapp: send message: %w", t.redact(err))
	}
	return nil
}

// Start begins the WhatsApp connection and event processing.
// It runs until Stop() is called or context is cancelled.
func (t *WhatsAppTransport) Start(ctx context.Context) {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return
	}
	t.running = true
	t.stopCh = make(chan struct{})
	t.mu.Unlock()

	t.wg.Add(1)
	go t.connectLoop(ctx)
}

// Stop stops the WhatsApp connection and waits for it to exit.
func (t *WhatsAppTransport) Stop() {
	t.mu.Lock()
	if !t.running {
		t.mu.Unlock()
		return
	}
	close(t.stopCh)
	t.running = false
	client := t.client
	t.mu.Unlock()

	if client != nil {
		client.Disconnect()
	}
	t.wg.Wait()

	// Close store
	if t.db != nil {
		t.db.Close()
	}
}

// PairingQR returns the current QR code payload for pairing.
// Returns (qrCode, true) if a QR code is available and not yet paired.
// Returns ("", false) if already paired or no QR code generated yet.
func (t *WhatsAppTransport) PairingQR() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.paired {
		return "", false
	}
	if t.qrCode == "" {
		return "", false
	}
	if time.Now().After(t.qrExpiry) {
		return "", false
	}
	return t.qrCode, true
}

// waitDisconnect blocks until the client disconnects (true) or the transport
// is told to stop / the context ends (false).
func (t *WhatsAppTransport) waitDisconnect(ctx context.Context) bool {
	ch := make(chan struct{})
	t.mu.Lock()
	t.disconnectCh = ch
	t.mu.Unlock()

	select {
	case <-ctx.Done():
		return false
	case <-t.stopCh:
		return false
	case <-ch:
		return true
	}
}

// sleepOrStop sleeps for d unless the transport is told to stop or the
// context ends. It reports false when told to stop.
func (t *WhatsAppTransport) sleepOrStop(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-t.stopCh:
		return false
	case <-time.After(d):
		return true
	}
}

func (t *WhatsAppTransport) connectLoop(ctx context.Context) {
	defer t.wg.Done()

	backoff := time.Second
	maxBackoff := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.stopCh:
			return
		default:
		}

		// Check if already connected (whatsmeow auto-reconnects by default)
		if t.client.IsConnected() {
			t.mu.Lock()
			t.paired = true
			t.qrCode = ""
			t.mu.Unlock()
			slog.Info("whatsapp: connected")
			if !t.waitDisconnect(ctx) {
				return
			}
			t.mu.Lock()
			t.paired = false
			t.mu.Unlock()
			slog.Info("whatsapp: disconnected")
			continue
		}

		// Try to connect
		err := t.client.Connect()
		if err != nil {
			slog.Warn("whatsapp: connect failed, backing off", "error", t.redact(err), "backoff", backoff)
			if !t.sleepOrStop(ctx, backoff) {
				return
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		// Reset backoff on successful connection
		backoff = time.Second

		// Wait for disconnect or stop
		if !t.waitDisconnect(ctx) {
			return
		}
	}
}

func (t *WhatsAppTransport) handleEvent(evt interface{}) {
	switch v := evt.(type) {
	case *events.Message:
		t.handleMessage(v)
	case *events.QR:
		t.handleQR(v)
	case *events.PairSuccess:
		t.handlePairSuccess(v)
	case *events.LoggedOut:
		t.handleLoggedOut(v)
	case *events.Disconnected:
		t.mu.Lock()
		if t.disconnectCh != nil {
			close(t.disconnectCh)
			t.disconnectCh = nil
		}
		t.mu.Unlock()
	}
}

func (t *WhatsAppTransport) handleMessage(evt *events.Message) {
	// v1: 1:1 chats only; ignore group messages
	if evt.Info.Chat.Server != types.DefaultUserServer {
		return
	}

	// Ignore messages from self
	if evt.Info.IsFromMe {
		return
	}

	// Extract text content
	text := evt.Message.GetConversation()
	if text == "" {
		// Try extended text message
		if ext := evt.Message.GetExtendedTextMessage(); ext != nil {
			text = ext.GetText()
		}
	}
	if text == "" {
		return
	}

	// ChatID is the user part of the sender JID
	chatID := evt.Info.Chat.User
	senderID := evt.Info.Sender.User
	at := evt.Info.Timestamp

	inbound := InboundMessage{
		Transport: "whatsapp",
		ChatID:    chatID,
		SenderID:  senderID,
		Text:      text,
		At:        at,
	}

	ctx := context.Background()
	if err := t.svc.HandleInbound(ctx, inbound); err != nil {
		slog.Error("whatsapp: handle inbound failed", "error", err, "chat_id", chatID)
	}
}

func (t *WhatsAppTransport) handleQR(evt *events.QR) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(evt.Codes) == 0 {
		return
	}
	t.qrCode = evt.Codes[0]
	// QR codes typically expire in ~20-30 seconds; use 45s for safety
	t.qrExpiry = time.Now().Add(45 * time.Second)
	slog.Info("whatsapp: QR code generated")
}

func (t *WhatsAppTransport) handlePairSuccess(evt *events.PairSuccess) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.paired = true
	t.qrCode = ""
	slog.Info("whatsapp: pairing successful")
}

func (t *WhatsAppTransport) handleLoggedOut(evt *events.LoggedOut) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.paired = false
	t.qrCode = ""
	slog.Info("whatsapp: logged out", "reason", evt.Reason)
}

// IsPaired returns whether the WhatsApp transport is currently paired.
func (t *WhatsAppTransport) IsPaired() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.paired
}

// QRExpiry returns the expiry time of the current QR code.
// Returns zero time if no QR code is set.
func (t *WhatsAppTransport) QRExpiry() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.qrExpiry
}
