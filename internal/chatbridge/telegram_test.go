package chatbridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// TestTelegramConstructor tests the constructor behavior.
func TestTelegramConstructor(t *testing.T) {
	t.Parallel()

	// Empty token should return error
	_, err := NewTelegramTransport("", nil, nil)
	if err == nil {
		t.Error("NewTelegramTransport with empty token: want error, got nil")
	}
	if !strings.Contains(err.Error(), "empty bot token") {
		t.Errorf("error = %q, want contains 'empty bot token'", err.Error())
	}

	// Valid token should succeed and Name() == "telegram"
	svc := &Service{}
	tr, err := NewTelegramTransport("TESTTOKEN123", svc, nil)
	if err != nil {
		t.Fatalf("NewTelegramTransport with valid token: %v", err)
	}
	if tr.Name() != "telegram" {
		t.Errorf("Name() = %q, want 'telegram'", tr.Name())
	}
}

// TestTelegramSendHappyPath tests successful Send via fake Bot API.
func TestTelegramSendHappyPath(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var capturedPath, capturedMethod string
	var capturedBody map[string]string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The body must be decoded here, inside the handler: after the
		// handler returns, the server closes the request body.
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		capturedPath = r.URL.Path
		capturedMethod = r.Method
		capturedBody = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer ts.Close()

	// Point baseURL at test server
	TelegramAPIBase = ts.URL
	defer func() { TelegramAPIBase = "https://api.telegram.org" }()

	tr, err := NewTelegramTransport("TESTTOKEN123", nil, nil)
	if err != nil {
		t.Fatalf("NewTelegramTransport: %v", err)
	}

	err = tr.Send("4242", "hello world")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	mu.Lock()
	gotPath, gotMethod, body := capturedPath, capturedMethod, capturedBody
	mu.Unlock()

	if gotPath == "" {
		t.Fatal("no request captured")
	}

	// Assert path
	expectedPath := "/botTESTTOKEN123/sendMessage"
	if gotPath != expectedPath {
		t.Errorf("request path = %q, want %q", gotPath, expectedPath)
	}

	// Assert method
	if gotMethod != http.MethodPost {
		t.Errorf("request method = %q, want POST", gotMethod)
	}

	// Assert JSON body (decoded inside the handler)
	if body["chat_id"] != "4242" {
		t.Errorf("chat_id = %q, want '4242'", body["chat_id"])
	}
	if body["text"] != "hello world" {
		t.Errorf("text = %q, want 'hello world'", body["text"])
	}
}

// TestTelegramSendAPIFailure tests Send when Bot API returns 500.
func TestTelegramSendAPIFailure(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	TelegramAPIBase = ts.URL
	defer func() { TelegramAPIBase = "https://api.telegram.org" }()

	tr, err := NewTelegramTransport("TESTTOKEN123", nil, nil)
	if err != nil {
		t.Fatalf("NewTelegramTransport: %v", err)
	}

	err = tr.Send("4242", "hello")
	if err == nil {
		t.Error("Send with 500 response: want error, got nil")
	}
	if !strings.Contains(err.Error(), "status 500") {
		t.Errorf("error = %q, want contains 'status 500'", err.Error())
	}
}

// TestTelegramSendErrorRedaction tests that token is not leaked in error messages.
func TestTelegramSendErrorRedaction(t *testing.T) {
	t.Parallel()

	// Point at a closed port (connection refused)
	TelegramAPIBase = "http://127.0.0.1:1"
	defer func() { TelegramAPIBase = "https://api.telegram.org" }()

	tr, err := NewTelegramTransport("TESTTOKEN123", nil, nil)
	if err != nil {
		t.Fatalf("NewTelegramTransport: %v", err)
	}

	err = tr.Send("4242", "hello")
	if err == nil {
		t.Error("Send to closed port: want error, got nil")
	}

	errText := err.Error()
	if strings.Contains(errText, "TESTTOKEN123") {
		t.Errorf("error text contains token: %q", errText)
	}
}

// TestTelegramGetUpdatesOffset tests getUpdates offset handling.
func TestTelegramGetUpdatesOffset(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var requestCount int
	var lastQuery string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		lastQuery = r.URL.RawQuery
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if requestCount == 1 {
			// First call: return two updates
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"result": []map[string]any{
					{"update_id": 10, "message": map[string]any{"message_id": 1, "from": map[string]any{"id": 1, "is_bot": false}, "chat": map[string]any{"id": 4242}, "date": 1000, "text": "hi"}},
					{"update_id": 11, "message": map[string]any{"message_id": 2, "from": map[string]any{"id": 1, "is_bot": false}, "chat": map[string]any{"id": 4242}, "date": 1001, "text": "hello"}},
				},
			})
		} else {
			// Subsequent calls: empty result
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{}})
		}
	}))
	defer ts.Close()

	TelegramAPIBase = ts.URL
	defer func() { TelegramAPIBase = "https://api.telegram.org" }()

	tr, err := NewTelegramTransport("TESTTOKEN123", nil, nil)
	if err != nil {
		t.Fatalf("NewTelegramTransport: %v", err)
	}

	// Call getUpdates directly (unexported but same package)
	updates, err := tr.getUpdates()
	if err != nil {
		t.Fatalf("getUpdates first call: %v", err)
	}
	if len(updates) != 2 {
		t.Errorf("first getUpdates: got %d updates, want 2", len(updates))
	}
	if updates[0].UpdateID != 10 || updates[1].UpdateID != 11 {
		t.Errorf("update_ids = %v, want [10, 11]", []int64{updates[0].UpdateID, updates[1].UpdateID})
	}

	// Check offset was updated to 12
	tr.mu.Lock()
	offset := tr.offset
	tr.mu.Unlock()
	if offset != 12 {
		t.Errorf("offset after first call = %d, want 12", offset)
	}

	// Second call should send offset=12 and timeout=30
	_, err = tr.getUpdates()
	if err != nil {
		t.Fatalf("getUpdates second call: %v", err)
	}

	mu.Lock()
	query := lastQuery
	mu.Unlock()

	if !strings.Contains(query, "offset=12") {
		t.Errorf("second request query = %q, want offset=12", query)
	}
	if !strings.Contains(query, "timeout=30") {
		t.Errorf("second request query = %q, want timeout=30", query)
	}
}

// TestTelegramInboundPollLoop tests the full inbound flow through the poll loop.
func TestTelegramInboundPollLoop(t *testing.T) {
	// This test uses the real Service and test DB, with the Telegram transport
	// pointing at a fake Bot API server.

	db := setupTestDB(t)
	svc, _ := newTestService(t, db)

	// Pre-pair a link for chat 4242 with PIN 1234
	// We use the fake transport "telegram" for pairing, then register the real transport
	code := pair(t, svc, svc.transports["telegram"].(*fakeTransport), "4242", "1234")
	_ = code // used for pairing

	var mu sync.Mutex
	var sendMessageCalls []map[string]any
	var getUpdatesCallCount int

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			getUpdatesCallCount++
			w.Header().Set("Content-Type", "application/json")
			if getUpdatesCallCount == 1 {
				// Return one update: /status from chat 4242
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": true,
					"result": []map[string]any{
						{
							"update_id": 100,
							"message": map[string]any{
								"message_id": 1,
								"from":       map[string]any{"id": 999, "is_bot": false, "first_name": "Test"},
								"chat":       map[string]any{"id": 4242, "type": "private"},
								"date":       time.Now().Unix(),
								"text":       "/status",
							},
						},
					},
				})
			} else {
				// Subsequent calls: empty (will be stopped before this matters)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{}})
			}
			return
		}

		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			sendMessageCalls = append(sendMessageCalls, body)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	TelegramAPIBase = ts.URL
	defer func() { TelegramAPIBase = "https://api.telegram.org" }()

	// Create the real Telegram transport and register it (replacing the fake)
	tr, err := NewTelegramTransport("TESTTOKEN123", svc, nil)
	if err != nil {
		t.Fatalf("NewTelegramTransport: %v", err)
	}
	svc.RegisterTransport(tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr.Start(ctx)

	// Wait for the sendMessage to be captured (poll loop processes update, calls HandleInbound, which calls Send)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(sendMessageCalls)
		mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	tr.Stop()

	mu.Lock()
	calls := sendMessageCalls
	mu.Unlock()

	if len(calls) == 0 {
		t.Fatal("no sendMessage captured; poll loop did not process inbound")
	}

	// Assert the reply contains "No pools configured." (empty state /status response)
	found := false
	for _, call := range calls {
		text, _ := call["text"].(string)
		if strings.Contains(text, "No pools configured.") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("sendMessage text does not contain 'No pools configured.'; calls = %+v", calls)
	}
}

// TestTelegramBotEmptyFiltering tests that bot messages, empty text, and non-message updates are filtered.
func TestTelegramBotEmptyFiltering(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	svc, _ := newTestService(t, db)

	// Pair a chat so inbound would be allowed
	pair(t, svc, svc.transports["telegram"].(*fakeTransport), "4242", "1234")

	var mu sync.Mutex
	var sendMessageCount int
	var getUpdatesCallCount int

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if strings.HasSuffix(r.URL.Path, "/getUpdates") {
			getUpdatesCallCount++
			w.Header().Set("Content-Type", "application/json")
			if getUpdatesCallCount == 1 {
				// Return a page with:
				// (a) message from a bot user
				// (b) message with empty text
				// (c) update with no message
				// (d) a valid message (should be processed)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": true,
					"result": []map[string]any{
						{
							"update_id": 10,
							"message": map[string]any{
								"message_id": 1,
								"from":       map[string]any{"id": 111, "is_bot": true, "first_name": "Bot"},
								"chat":       map[string]any{"id": 4242, "type": "private"},
								"date":       time.Now().Unix(),
								"text":       "from bot",
							},
						},
						{
							"update_id": 11,
							"message": map[string]any{
								"message_id": 2,
								"from":       map[string]any{"id": 222, "is_bot": false, "first_name": "User"},
								"chat":       map[string]any{"id": 4242, "type": "private"},
								"date":       time.Now().Unix(),
								"text":       "",
							},
						},
						{
							"update_id": 12,
							// no message field
						},
						{
							"update_id": 13,
							"message": map[string]any{
								"message_id": 3,
								"from":       map[string]any{"id": 333, "is_bot": false, "first_name": "User"},
								"chat":       map[string]any{"id": 4242, "type": "private"},
								"date":       time.Now().Unix(),
								"text":       "/status",
							},
						},
					},
				})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": []any{}})
			}
			return
		}

		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			sendMessageCount++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	TelegramAPIBase = ts.URL
	defer func() { TelegramAPIBase = "https://api.telegram.org" }()

	tr, err := NewTelegramTransport("TESTTOKEN123", svc, nil)
	if err != nil {
		t.Fatalf("NewTelegramTransport: %v", err)
	}
	svc.RegisterTransport(tr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr.Start(ctx)

	// Wait for one poll cycle (should process the valid message)
	time.Sleep(500 * time.Millisecond)

	tr.Stop()

	mu.Lock()
	count := sendMessageCount
	mu.Unlock()

	// Only the valid message (update_id 13) should trigger a sendMessage
	// The bot message, empty text, and no-message updates should be filtered
	if count != 1 {
		t.Errorf("sendMessage count = %d, want 1 (only the valid non-bot, non-empty message)", count)
	}
}

// TestTelegram401StopsLoop tests that 401 from getUpdates sets badToken and stops polling.
func TestTelegram401StopsLoop(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var getUpdatesCallCount int

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		getUpdatesCallCount++
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "Unauthorized"})
	}))
	defer ts.Close()

	TelegramAPIBase = ts.URL
	defer func() { TelegramAPIBase = "https://api.telegram.org" }()

	tr, err := NewTelegramTransport("TESTTOKEN123", nil, nil)
	if err != nil {
		t.Fatalf("NewTelegramTransport: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tr.Start(ctx)

	// Wait ~1.5s for the loop to hit 401 and stop
	time.Sleep(1500 * time.Millisecond)

	tr.Stop()

	mu.Lock()
	count := getUpdatesCallCount
	mu.Unlock()

	// Should not spin: count should be small (≤ 3)
	if count > 3 {
		t.Errorf("getUpdates call count = %d after 1.5s with 401, want ≤ 3 (no spin)", count)
	}

	// badToken flag should be set
	tr.mu.Lock()
	badToken := tr.badToken
	tr.mu.Unlock()
	if !badToken {
		t.Error("badToken flag not set after 401")
	}
}
