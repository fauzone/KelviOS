package chatbridge

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/approval"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	_ "modernc.org/sqlite"
)

// TestWhatsAppName verifies the transport identifier.
func TestWhatsAppName(t *testing.T) {
	t.Parallel()

	tr, err := NewWhatsAppTransport(nil, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewWhatsAppTransport: %v", err)
	}
	defer tr.Stop()

	if tr.Name() != "whatsapp" {
		t.Errorf("Name() = %q, want 'whatsapp'", tr.Name())
	}
}

// TestWhatsAppPairingQR tests the PairingQR method under various conditions.
func TestWhatsAppPairingQR(t *testing.T) {
	t.Parallel()

	tr, err := NewWhatsAppTransport(nil, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewWhatsAppTransport: %v", err)
	}
	defer tr.Stop()

	// Case 1: No QR code set
	qr, ok := tr.PairingQR()
	if ok {
		t.Errorf("PairingQR() = (%q, true), want ('', false) when no QR set", qr)
	}

	// Case 2: QR set but paired = true
	tr.mu.Lock()
	tr.qrCode = "TEST_QR_CODE"
	tr.qrExpiry = time.Now().Add(30 * time.Second)
	tr.paired = true
	tr.mu.Unlock()

	qr, ok = tr.PairingQR()
	if ok {
		t.Errorf("PairingQR() = (%q, true), want ('', false) when paired=true", qr)
	}

	// Case 3: QR set, not paired, but expired
	tr.mu.Lock()
	tr.paired = false
	tr.qrExpiry = time.Now().Add(-10 * time.Second) // expired
	tr.mu.Unlock()

	qr, ok = tr.PairingQR()
	if ok {
		t.Errorf("PairingQR() = (%q, true), want ('', false) when QR expired", qr)
	}

	// Case 4: QR set, not paired, not expired -> should return QR
	tr.mu.Lock()
	tr.qrCode = "VALID_QR_CODE"
	tr.qrExpiry = time.Now().Add(30 * time.Second)
	tr.paired = false
	tr.mu.Unlock()

	qr, ok = tr.PairingQR()
	if !ok {
		t.Errorf("PairingQR() = (%q, false), want (qr, true) when valid", qr)
	}
	if qr != "VALID_QR_CODE" {
		t.Errorf("PairingQR() returned %q, want 'VALID_QR_CODE'", qr)
	}
}

// TestWhatsAppHandleQR tests the handleQR event handler.
func TestWhatsAppHandleQR(t *testing.T) {
	t.Parallel()

	tr, err := NewWhatsAppTransport(nil, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewWhatsAppTransport: %v", err)
	}
	defer tr.Stop()

	// Case 1: QR event with codes
	tr.handleQR(&events.QR{Codes: []string{"CODE123"}})

	tr.mu.Lock()
	qrCode := tr.qrCode
	qrExpiry := tr.qrExpiry
	tr.mu.Unlock()

	if qrCode != "CODE123" {
		t.Errorf("qrCode = %q, want 'CODE123'", qrCode)
	}
	if time.Now().After(qrExpiry) {
		t.Errorf("qrExpiry = %v, want future time", qrExpiry)
	}
	if time.Since(qrExpiry) > 50*time.Second { // should be ~45s in future
		t.Errorf("qrExpiry too far in future: %v", qrExpiry)
	}

	// Case 2: QR event with empty Codes should not change existing qrCode
	tr.handleQR(&events.QR{Codes: []string{}})

	tr.mu.Lock()
	qrCode2 := tr.qrCode
	tr.mu.Unlock()

	if qrCode2 != "CODE123" {
		t.Errorf("qrCode after empty Codes = %q, want 'CODE123' (unchanged)", qrCode2)
	}
}

// TestWhatsAppSendNotRunning verifies Send returns error when transport not started.
func TestWhatsAppSendNotRunning(t *testing.T) {
	t.Parallel()

	tr, err := NewWhatsAppTransport(nil, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewWhatsAppTransport: %v", err)
	}
	defer tr.Stop()

	// Send without Start should return error
	err = tr.Send("628123456789", "hello")
	if err == nil {
		t.Error("Send() without Start: want error, got nil")
	}
	if err.Error() != "whatsapp: transport not connected" {
		t.Errorf("Send() error = %q, want 'whatsapp: transport not connected'", err.Error())
	}
}

// TestWhatsAppRedact tests the redact function for JID sanitization.
func TestWhatsAppRedact(t *testing.T) {
	t.Parallel()

	tr, err := NewWhatsAppTransport(nil, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewWhatsAppTransport: %v", err)
	}
	defer tr.Stop()

	// Test nil input
	if tr.redact(nil) != nil {
		t.Error("redact(nil) != nil")
	}

	// Test @s.whatsapp.net redaction
	err = tr.redact(&testError{msg: "user@s.whatsapp.net"})
	if err == nil || err.Error() != "user@***" {
		t.Errorf("redact @s.whatsapp.net = %v, want 'user@***'", err)
	}

	// Test @c.us redaction
	err = tr.redact(&testError{msg: "user@c.us"})
	if err == nil || err.Error() != "user@***" {
		t.Errorf("redact @c.us = %v, want 'user@***'", err)
	}

	// Test @g.us redaction
	err = tr.redact(&testError{msg: "group@g.us"})
	if err == nil || err.Error() != "group@***" {
		t.Errorf("redact @g.us = %v, want 'group@***'", err)
	}

	// Test multiple JIDs in one error
	err = tr.redact(&testError{msg: "user1@s.whatsapp.net and user2@c.us and group@g.us"})
	if err == nil || err.Error() != "user1@*** and user2@*** and group@***" {
		t.Errorf("redact multiple = %v, want all redacted", err)
	}
}

// testError is a helper error type for testing redact.
type testError struct {
	msg string
}

func (e *testError) Error() string { return e.msg }

// TestWhatsAppNewTransport tests the constructor behavior.
func TestWhatsAppNewTransport(t *testing.T) {
	t.Parallel()

	// Case 1: Valid temp directory - should create store file
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	tr, err := NewWhatsAppTransport(nil, dbPath)
	if err != nil {
		t.Fatalf("NewWhatsAppTransport with valid path: %v", err)
	}
	defer tr.Stop()

	// Check that _whatsapp.db file was created
	storePath := filepath.Join(tmpDir, "test_whatsapp.db")
	if _, err := os.Stat(storePath); os.IsNotExist(err) {
		t.Errorf("store file %s not created", storePath)
	}

	// Case 2: Unwritable path should return error
	// Create a directory we can't write to (by making it a file)
	badDir := t.TempDir()
	badFile := filepath.Join(badDir, "blocked")
	if err := os.WriteFile(badFile, []byte("x"), 0444); err != nil {
		t.Fatalf("create blocked file: %v", err)
	}
	// Try to use the file as a directory path (will fail)
	badDBPath := filepath.Join(badFile, "sub.db")
	_, err = NewWhatsAppTransport(nil, badDBPath)
	if err == nil {
		t.Error("NewWhatsAppTransport with unwritable path: want error, got nil")
	}
}

// TestWhatsAppStartStop tests the lifecycle methods.
func TestWhatsAppStartStop(t *testing.T) {
	// This test connects to web.whatsapp.com - allow up to 30s
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tr, err := NewWhatsAppTransport(nil, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewWhatsAppTransport: %v", err)
	}

	// Case 1: Stop without Start should be no-op (no panic)
	tr.Stop()

	// Case 2: Start twice - second should be no-op
	tr.Start(ctx)
	tr.Start(ctx) // should not panic or start duplicate

	// Case 3: Start then Stop should clean up without panic
	tr.Stop()

	// Case 4: Double Stop should be no-op
	tr.Stop()
}

// TestWhatsAppHandleMessageGroupIgnored verifies group messages are ignored.
func TestWhatsAppHandleMessageGroupIgnored(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	svc := NewService(Deps{
		DB:            db,
		ApprovalStore: approval.NewApprovalStore(db),
	})

	tr, err := NewWhatsAppTransport(svc, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewWhatsAppTransport: %v", err)
	}
	defer tr.Stop()

	// Create a group message event (Chat.Server = GroupServer)
	groupMsg := &events.Message{
		Info: types.MessageInfo{
			Chat: types.JID{
				User:   "123456789-123456@g.us",
				Server: types.GroupServer,
			},
			Sender: types.JID{
				User:   "987654321",
				Server: types.DefaultUserServer,
			},
			IsFromMe:  false,
			Timestamp: time.Now(),
		},
		Message: &waE2E.Message{
			Conversation: stringPtr("group message"),
		},
	}

	// Count audit rows before
	before := countAuditRows(t, db, "chat.conversation")

	// Call handleMessage directly (same package, unexported access)
	tr.handleMessage(groupMsg)

	// Count audit rows after
	after := countAuditRows(t, db, "chat.conversation")

	// Group messages should be ignored - no new audit row
	if after != before {
		t.Errorf("audit rows for chat.conversation: before=%d after=%d, want no change (group ignored)", before, after)
	}
}

// TestWhatsAppHandleMessageOneToOne verifies 1:1 messages are processed.
// This is a positive test to ensure the filtering logic works correctly.
func TestWhatsAppHandleMessageOneToOne(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	svc := NewService(Deps{
		DB:            db,
		ApprovalStore: approval.NewApprovalStore(db),
	})

	tr, err := NewWhatsAppTransport(svc, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("NewWhatsAppTransport: %v", err)
	}
	defer tr.Stop()

	// First, pair the chat so inbound messages are allowed
	pc, err := svc.CreatePairingCode("whatsapp")
	if err != nil {
		t.Fatalf("CreatePairingCode: %v", err)
	}
	// Simulate pairing via HandleInbound (bypassing transport Send)
	ctx := context.Background()
	if err := svc.HandleInbound(ctx, InboundMessage{
		Transport: "whatsapp",
		ChatID:    "628123456789",
		SenderID:  "628123456789",
		Text:      "/pair " + pc.Code + " 1234",
		At:        time.Now(),
	}); err != nil {
		t.Fatalf("HandleInbound pair: %v", err)
	}

	// Create a 1:1 message event (Chat.Server = DefaultUserServer)
	privateMsg := &events.Message{
		Info: types.MessageInfo{
			Chat: types.JID{
				User:   "628123456789",
				Server: types.DefaultUserServer,
			},
			Sender: types.JID{
				User:   "628987654321",
				Server: types.DefaultUserServer,
			},
			IsFromMe:  false,
			Timestamp: time.Now(),
		},
		Message: &waE2E.Message{
			Conversation: stringPtr("hello from whatsapp"),
		},
	}

	// Count audit rows before
	before := countAuditRows(t, db, "chat.conversation")

	// Call handleMessage directly
	tr.handleMessage(privateMsg)

	// Count audit rows after
	after := countAuditRows(t, db, "chat.conversation")

	// 1:1 messages should be processed - one new audit row
	if after != before+1 {
		t.Errorf("audit rows for chat.conversation: before=%d after=%d, want +1", before, after)
	}
}

// stringPtr returns a pointer to the given string.
func stringPtr(s string) *string {
	return &s
}

// countAuditRows counts audit log rows for a given action.
func countAuditRows(t *testing.T, db *sql.DB, action string) int {
	t.Helper()
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action = ?`, action).Scan(&count)
	if err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return count
}