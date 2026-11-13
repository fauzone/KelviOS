package httpserver

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestChatbridgePairingCode(t *testing.T) {
	ts, token, _ := newMCPTestServer(t, ":memory:")

	// Happy path: 201 with an 8-char code.
	status, body := postJSON(t, ts.URL+"/api/chatbridge/pairing-code", map[string]any{"transport": "telegram"}, token)
	if status != http.StatusCreated {
		t.Fatalf("pairing-code status = %d, want 201 (body %s)", status, body)
	}
	// postJSON returns raw bytes; re-parse for the code field.
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal pairing-code response: %v", err)
	}
	code, _ := parsed["code"].(string)
	if len(code) != 8 {
		t.Errorf("code = %q, want 8 chars", code)
	}
	if parsed["transport"] != "telegram" {
		t.Errorf("transport = %v, want telegram", parsed["transport"])
	}
	if _, ok := parsed["expires_at"]; !ok {
		t.Error("response missing expires_at")
	}

	// Missing transport: 400.
	status, _ = postJSON(t, ts.URL+"/api/chatbridge/pairing-code", map[string]any{}, token)
	if status != http.StatusBadRequest {
		t.Errorf("missing transport status = %d, want 400", status)
	}

	// No auth: 401.
	status, _ = postJSON(t, ts.URL+"/api/chatbridge/pairing-code", map[string]any{"transport": "telegram"}, "")
	if status != http.StatusUnauthorized {
		t.Errorf("unauth status = %d, want 401", status)
	}
}

func TestChatbridgeLinks(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")

	// No auth: 401.
	status, _ := tagDoReq(t, ts, http.MethodGet, "/api/chatbridge/links", nil, "")
	if status != http.StatusUnauthorized {
		t.Errorf("unauth GET links status = %d, want 401", status)
	}

	// Empty list.
	status, body := tagDoReq(t, ts, http.MethodGet, "/api/chatbridge/links", nil, token)
	if status != http.StatusOK {
		t.Fatalf("GET links status = %d, want 200", status)
	}
	links, _ := body["links"].([]any)
	if len(links) != 0 {
		t.Errorf("links = %v, want empty", links)
	}

	// Seed a link directly (pin_hash is a credential; the endpoint must never echo it).
	const hashValue = "deadbeef0123456789abcdef0123456789abcdef0123456789abcdef01234567:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if _, err := db.Exec(`INSERT INTO chat_links (id, transport, chat_id, label, pin_hash, created_at, last_seen_at)
		VALUES ('link-1','telegram','chat-1','telegram:chat-1',?,1700000000,1700000000)`, hashValue); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	status, body = tagDoReq(t, ts, http.MethodGet, "/api/chatbridge/links", nil, token)
	if status != http.StatusOK {
		t.Fatalf("GET links status = %d, want 200", status)
	}
	links, _ = body["links"].([]any)
	if len(links) != 1 {
		t.Fatalf("links = %v, want 1 entry", links)
	}
	entry, _ := links[0].(map[string]any)
	if entry["id"] != "link-1" || entry["transport"] != "telegram" || entry["chat_id"] != "chat-1" {
		t.Errorf("link entry = %v, want id/transport/chat_id", entry)
	}
	if _, ok := entry["pin_hash"]; ok {
		t.Error("response contains pin_hash")
	}
	// The hash value itself must not appear anywhere in the response.
	rawBytes, _ := json.Marshal(body)
	raw := string(rawBytes)
	if strings.Contains(raw, hashValue) || strings.Contains(raw, "deadbeef") {
		t.Error("response leaks the stored pin_hash value")
	}
}

func TestChatbridgeLinkDelete(t *testing.T) {
	ts, token, db := newMCPTestServer(t, ":memory:")

	if _, err := db.Exec(`INSERT INTO chat_links (id, transport, chat_id, label, pin_hash, created_at, last_seen_at)
		VALUES ('link-1','telegram','chat-1','telegram:chat-1','salt:hash',1700000000,1700000000)`); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	// Unknown id: 404.
	status, _ := tagDoReq(t, ts, http.MethodDelete, "/api/chatbridge/links/nope", nil, token)
	if status != http.StatusNotFound {
		t.Errorf("delete unknown status = %d, want 404", status)
	}

	// No auth: 401.
	status, _ = tagDoReq(t, ts, http.MethodDelete, "/api/chatbridge/links/link-1", nil, "")
	if status != http.StatusUnauthorized {
		t.Errorf("unauth delete status = %d, want 401", status)
	}

	// Existing link: 200 and gone.
	status, body := tagDoReq(t, ts, http.MethodDelete, "/api/chatbridge/links/link-1", nil, token)
	if status != http.StatusOK {
		t.Fatalf("delete status = %d, want 200", status)
	}
	if body["deleted"] != "link-1" {
		t.Errorf("deleted = %v, want link-1", body["deleted"])
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chat_links WHERE id = 'link-1'`).Scan(&count); err != nil || count != 0 {
		t.Errorf("link-1 count = %d (err %v), want 0 after delete", count, err)
	}

	// Deleting again: 404.
	status, _ = tagDoReq(t, ts, http.MethodDelete, "/api/chatbridge/links/link-1", nil, token)
	if status != http.StatusNotFound {
		t.Errorf("delete twice status = %d, want 404", status)
	}
}

func TestChatbridgeSchemaVersion(t *testing.T) {
	_, _, db := newMCPTestServer(t, ":memory:")
	var version string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&version); err != nil {
		t.Fatalf("schema_version: %v", err)
	}
	if version != "41" {
		t.Errorf("schema_version = %q, want 41 (WS62: event_rules; WS59: ota_updates; 39 was WS55: app_installs columns + v39)", version)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('chat_links','chat_pairing_codes','app_installs')`).Scan(&count); err != nil || count != 3 {
		t.Errorf("chat tables count = %d (err %v), want 3", count, err)
	}
}

func TestChatbridgeWhatsAppQR(t *testing.T) {
	// Test without WhatsApp enabled (default)
	ts, token, _ := newMCPTestServer(t, ":memory:")

	// No auth: 401
	status, _ := tagDoReq(t, ts, http.MethodGet, "/api/chatbridge/whatsapp/qr", nil, "")
	if status != http.StatusUnauthorized {
		t.Errorf("unauth GET whatsapp/qr status = %d, want 401", status)
	}

	// With auth but WhatsApp not enabled: 404
	status, body := tagDoReq(t, ts, http.MethodGet, "/api/chatbridge/whatsapp/qr", nil, token)
	if status != http.StatusNotFound {
		t.Errorf("whatsapp not enabled status = %d, want 404, body=%v", status, body)
	}
	if body["error"] != "whatsapp_not_enabled" {
		t.Errorf("error = %v, want whatsapp_not_enabled", body["error"])
	}

	// Test with WhatsApp enabled
	// We need to create a new server with WhatsApp enabled
	// For now, we test the 404 case which is the default
}

func TestChatbridgeWhatsAppStatus(t *testing.T) {
	// Test without WhatsApp enabled (default)
	ts, token, _ := newMCPTestServer(t, ":memory:")

	// No auth: 401
	status, _ := tagDoReq(t, ts, http.MethodGet, "/api/chatbridge/whatsapp/status", nil, "")
	if status != http.StatusUnauthorized {
		t.Errorf("unauth GET whatsapp/status status = %d, want 401", status)
	}

	// With auth but WhatsApp not enabled: 200 with registered=false
	status, body := tagDoReq(t, ts, http.MethodGet, "/api/chatbridge/whatsapp/status", nil, token)
	if status != http.StatusOK {
		t.Errorf("whatsapp not enabled status = %d, want 200, body=%v", status, body)
	}
	if body["registered"] != false {
		t.Errorf("registered = %v, want false", body["registered"])
	}
	if body["paired"] != false {
		t.Errorf("paired = %v, want false", body["paired"])
	}
	if body["qr_available"] != false {
		t.Errorf("qr_available = %v, want false", body["qr_available"])
	}
}

func TestChatbridgeWhatsAppQREnabled(t *testing.T) {
	// Set env BEFORE constructing server so WhatsApp transport registers
	t.Setenv("KELVIOS_WHATSAPP_ENABLED", "1")
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "test_whatsapp.db")
	t.Setenv("KELVIOS_WHATSAPP_STORE_PATH", storePath)

	ts, token, _ := newMCPTestServer(t, ":memory:")

	// No auth: 401
	status, _ := tagDoReq(t, ts, http.MethodGet, "/api/chatbridge/whatsapp/qr", nil, "")
	if status != http.StatusUnauthorized {
		t.Errorf("unauth GET whatsapp/qr status = %d, want 401", status)
	}

	// With auth and WhatsApp enabled but not started: 200 paired=false, qr_available=false
	status, body := tagDoReq(t, ts, http.MethodGet, "/api/chatbridge/whatsapp/qr", nil, token)
	if status != http.StatusOK {
		t.Errorf("whatsapp enabled status = %d, want 200, body=%v", status, body)
	}
	if body["paired"] != false {
		t.Errorf("paired = %v, want false", body["paired"])
	}
	if body["qr_available"] != false {
		t.Errorf("qr_available = %v, want false", body["qr_available"])
	}
	// qr field should not be present when qr_available=false
	if _, ok := body["qr"]; ok {
		t.Errorf("qr field present when qr_available=false: %v", body["qr"])
	}
}

func TestChatbridgeWhatsAppStatusEnabled(t *testing.T) {
	// Set env BEFORE constructing server so WhatsApp transport registers
	t.Setenv("KELVIOS_WHATSAPP_ENABLED", "1")
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "test_whatsapp.db")
	t.Setenv("KELVIOS_WHATSAPP_STORE_PATH", storePath)

	ts, token, _ := newMCPTestServer(t, ":memory:")

	// No auth: 401
	status, _ := tagDoReq(t, ts, http.MethodGet, "/api/chatbridge/whatsapp/status", nil, "")
	if status != http.StatusUnauthorized {
		t.Errorf("unauth GET whatsapp/status status = %d, want 401", status)
	}

	// With auth and WhatsApp enabled: 200 registered=true, paired=false, qr_available=false
	status, body := tagDoReq(t, ts, http.MethodGet, "/api/chatbridge/whatsapp/status", nil, token)
	if status != http.StatusOK {
		t.Errorf("whatsapp enabled status = %d, want 200, body=%v", status, body)
	}
	if body["registered"] != true {
		t.Errorf("registered = %v, want true", body["registered"])
	}
	if body["paired"] != false {
		t.Errorf("paired = %v, want false", body["paired"])
	}
	if body["qr_available"] != false {
		t.Errorf("qr_available = %v, want false", body["qr_available"])
	}
}

func TestChatbridgeWhatsAppMethodNotAllowed(t *testing.T) {
	// Set env BEFORE constructing server so WhatsApp transport registers
	t.Setenv("KELVIOS_WHATSAPP_ENABLED", "1")
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "test_whatsapp.db")
	t.Setenv("KELVIOS_WHATSAPP_STORE_PATH", storePath)

	ts, token, _ := newMCPTestServer(t, ":memory:")

	// POST /api/chatbridge/whatsapp/qr -> 405
	status, _ := tagDoReq(t, ts, http.MethodPost, "/api/chatbridge/whatsapp/qr", nil, token)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("POST whatsapp/qr status = %d, want 405", status)
	}

	// POST /api/chatbridge/whatsapp/status -> 405
	status, _ = tagDoReq(t, ts, http.MethodPost, "/api/chatbridge/whatsapp/status", nil, token)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("POST whatsapp/status status = %d, want 405", status)
	}
}
