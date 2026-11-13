package chatbridge

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// failingTransport is a transport that always fails to send.
type failingTransport struct {
	name string
	sent map[string][]string
}

func (f *failingTransport) Name() string { return f.name }

func (f *failingTransport) Send(chatID, text string) error {
	return sql.ErrConnDone // simulate transport error
}

func seedLink(t *testing.T, db *sql.DB, transport, chatID, label string) {
	t.Helper()
	now := time.Now().Unix()
	mustExec(t, db, `
		INSERT INTO chat_links (id, transport, chat_id, label, pin_hash, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "link-"+chatID, transport, chatID, label, "salt:hash", now, now)
}

func TestNotifyLockout_NoLinkedChats(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	// No linked chats - should be a no-op with debug log only
	sent, err := svc.NotifyLockoutSync(context.Background(), "admin", 15)
	if err != nil {
		t.Fatalf("NotifyLockoutSync error: %v", err)
	}
	if sent != 0 {
		t.Errorf("sent = %d, want 0 (no linked chats)", sent)
	}
	if fake.count("chat-1") != 0 {
		t.Errorf("fake transport should have 0 messages, got %d", fake.count("chat-1"))
	}
}

func TestNotifyLockout_WithLinkedChats(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	// Seed two linked chats
	seedLink(t, db, "telegram", "chat-1", "telegram:chat-1")
	seedLink(t, db, "telegram", "chat-2", "telegram:chat-2")

	sent, err := svc.NotifyLockoutSync(context.Background(), "admin", 15)
	if err != nil {
		t.Fatalf("NotifyLockoutSync error: %v", err)
	}
	if sent != 2 {
		t.Errorf("sent = %d, want 2 (two linked chats)", sent)
	}

	// Verify message content
	msg1 := fake.last("chat-1")
	msg2 := fake.last("chat-2")
	if msg1 == "" || msg2 == "" {
		t.Fatal("expected messages to both chats")
	}

	// Check message format
	wantSubstrings := []string{
		"KelviOS: akun admin terkunci 15 menit setelah 5x gagal login",
		"WIB). Bila bukan Anda, periksa Activity log.",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(msg1, want) {
			t.Errorf("msg1 missing %q: got %q", want, msg1)
		}
		if !strings.Contains(msg2, want) {
			t.Errorf("msg2 missing %q: got %q", want, msg2)
		}
	}

	// Ensure no secrets in message
	forbidden := []string{"password", "token", "hash", "1234", "secret"}
	for _, f := range forbidden {
		if strings.Contains(strings.ToLower(msg1), f) {
			t.Errorf("msg1 contains forbidden substring %q", f)
		}
		if strings.Contains(strings.ToLower(msg2), f) {
			t.Errorf("msg2 contains forbidden substring %q", f)
		}
	}
}

func TestNotifyLockout_MultipleTransports(t *testing.T) {
	db := setupTestDB(t)
	svc, fakeTG := newTestService(t, db)

	// Add a second fake transport (WhatsApp)
	fakeWA := &fakeTransport{name: "whatsapp", sent: map[string][]string{}}
	svc.RegisterTransport(fakeWA)

	// Seed links for both transports
	seedLink(t, db, "telegram", "chat-1", "telegram:chat-1")
	seedLink(t, db, "whatsapp", "wa-1", "whatsapp:wa-1")

	sent, err := svc.NotifyLockoutSync(context.Background(), "admin", 15)
	if err != nil {
		t.Fatalf("NotifyLockoutSync error: %v", err)
	}
	if sent != 2 {
		t.Errorf("sent = %d, want 2 (one per transport)", sent)
	}

	if fakeTG.count("chat-1") != 1 {
		t.Errorf("telegram transport should have 1 message, got %d", fakeTG.count("chat-1"))
	}
	if fakeWA.count("wa-1") != 1 {
		t.Errorf("whatsapp transport should have 1 message, got %d", fakeWA.count("wa-1"))
	}
}

func TestNotifyLockout_TransportSendError(t *testing.T) {
	db := setupTestDB(t)
	svc, _ := newTestService(t, db)

	// Seed a link
	seedLink(t, db, "telegram", "chat-1", "telegram:chat-1")

	// Register a failing transport
	fakeFail := &failingTransport{name: "telegram", sent: map[string][]string{}}
	svc.transportsMu.Lock()
	svc.transports["telegram"] = fakeFail
	svc.transportsMu.Unlock()

	sent, err := svc.NotifyLockoutSync(context.Background(), "admin", 15)
	if err != nil {
		t.Fatalf("NotifyLockoutSync error: %v", err)
	}
	// Should still return sent=0 (error logged but not propagated)
	if sent != 0 {
		t.Errorf("sent = %d, want 0 (transport error)", sent)
	}
}

func TestNotifyLockout_MessageFormat(t *testing.T) {
	db := setupTestDB(t)
	svc, fake := newTestService(t, db)

	seedLink(t, db, "telegram", "chat-1", "telegram:chat-1")

	sent, err := svc.NotifyLockoutSync(context.Background(), "testuser", 15)
	if err != nil {
		t.Fatalf("NotifyLockoutSync error: %v", err)
	}
	if sent != 1 {
		t.Fatalf("sent = %d, want 1", sent)
	}

	msg := fake.last("chat-1")
	// Verify exact format: "KelviOS: akun <username> terkunci <minutes> menit setelah 5x gagal login (<HH:MM WIB>). Bila bukan Anda, periksa Activity log."
	if !strings.HasPrefix(msg, "KelviOS: akun testuser terkunci 15 menit setelah 5x gagal login (") {
		t.Errorf("message prefix mismatch: %q", msg)
	}
	if !strings.HasSuffix(msg, "WIB). Bila bukan Anda, periksa Activity log.") {
		t.Errorf("message suffix mismatch: %q", msg)
	}
	// Check time format HH:MM
	timePart := strings.TrimPrefix(msg, "KelviOS: akun testuser terkunci 15 menit setelah 5x gagal login (")
	timePart = strings.TrimSuffix(timePart, " WIB). Bila bukan Anda, periksa Activity log.")
	if len(timePart) != 5 || timePart[2] != ':' {
		t.Errorf("time format invalid: %q", timePart)
	}
}