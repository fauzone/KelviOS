package chatbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TelegramAPIBase is the base URL for the Telegram Bot API.
// Overridable for tests.
var TelegramAPIBase = "https://api.telegram.org"

// TelegramTransport implements the Transport interface for Telegram.
type TelegramTransport struct {
	token      string
	svc        *Service
	httpClient *http.Client
	baseURL    string

	// Long-polling state
	mu       sync.Mutex
	offset   int64
	stopCh   chan struct{}
	wg       sync.WaitGroup
	running  bool
	badToken bool
}

// NewTelegramTransport creates a new Telegram transport.
// token is the bot token from KELVIOS_TELEGRAM_BOT_TOKEN.
// svc is the chatbridge service for inbound message handling.
// httpClient is optional; nil defaults to http.DefaultClient.
// Returns an error if token is empty.
func NewTelegramTransport(token string, svc *Service, httpClient *http.Client) (*TelegramTransport, error) {
	if token == "" {
		return nil, errors.New("telegram: empty bot token")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &TelegramTransport{
		token:      token,
		svc:        svc,
		httpClient: httpClient,
		baseURL:    TelegramAPIBase,
		stopCh:     make(chan struct{}),
	}, nil
}

// redact strips the bot token from an error's text. http.Client.Do returns
// *url.Error values whose message embeds the full request URL — including
// the bot token — so any transport error must pass through here before it
// can reach a log or a caller.
func (t *TelegramTransport) redact(err error) error {
	if err == nil {
		return nil
	}
	if t.token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), t.token, "***"))
}

// Name returns the transport identifier.
func (t *TelegramTransport) Name() string {
	return "telegram"
}

// Send sends a text message to the given chat ID via Telegram Bot API.
func (t *TelegramTransport) Send(chatID, text string) error {
	url := fmt.Sprintf("%s/bot%s/sendMessage", t.baseURL, t.token)
	payload := map[string]string{
		"chat_id": chatID,
		"text":    text,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("telegram: marshal payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: send request: %w", t.redact(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: send failed with status %d", resp.StatusCode)
	}
	return nil
}

// Start begins the long-polling loop for inbound messages.
// It runs until Stop() is called or context is cancelled.
func (t *TelegramTransport) Start(ctx context.Context) {
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		return
	}
	t.running = true
	t.stopCh = make(chan struct{})
	t.mu.Unlock()

	t.wg.Add(1)
	go t.pollLoop(ctx)
}

// Stop stops the long-polling loop and waits for it to exit.
func (t *TelegramTransport) Stop() {
	t.mu.Lock()
	if !t.running {
		t.mu.Unlock()
		return
	}
	close(t.stopCh)
	t.running = false
	t.mu.Unlock()
	t.wg.Wait()
}

func (t *TelegramTransport) pollLoop(ctx context.Context) {
	defer t.wg.Done()

	backoff := time.Second
	maxBackoff := 30 * time.Second
	badTokenLogged := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.stopCh:
			return
		default:
		}

		updates, err := t.getUpdates()
		if err != nil {
			// Check for 401 (bad token)
			var httpErr *httpError
			if errors.As(err, &httpErr) && httpErr.statusCode == http.StatusUnauthorized {
				if !badTokenLogged {
					slog.Error("telegram: invalid bot token, stopping poll loop")
					badTokenLogged = true
				}
				t.mu.Lock()
				t.badToken = true
				t.mu.Unlock()
				return
			}

			slog.Warn("telegram: getUpdates failed, backing off", "error", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-t.stopCh:
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		// Reset backoff on success
		backoff = time.Second

		for _, update := range updates {
			if update.Message == nil {
				continue
			}
			msg := update.Message
			if msg.From == nil || msg.From.IsBot {
				continue
			}
			text := msg.Text
			if text == "" {
				continue
			}

			chatID := strconv.FormatInt(msg.Chat.ID, 10)
			senderID := strconv.FormatInt(msg.From.ID, 10)
			at := time.Unix(msg.Date, 0)

			inbound := InboundMessage{
				Transport: "telegram",
				ChatID:    chatID,
				SenderID:  senderID,
				Text:      text,
				At:        at,
			}

			if err := t.svc.HandleInbound(ctx, inbound); err != nil {
				slog.Error("telegram: handle inbound failed", "error", err, "chat_id", chatID)
			}
		}
	}
}

func (t *TelegramTransport) getUpdates() ([]telegramUpdate, error) {
	t.mu.Lock()
	offset := t.offset
	t.mu.Unlock()

	url := fmt.Sprintf("%s/bot%s/getUpdates", t.baseURL, t.token)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("telegram: create getUpdates request: %w", err)
	}

	q := req.URL.Query()
	q.Set("timeout", "30")
	if offset > 0 {
		q.Set("offset", strconv.FormatInt(offset, 10))
	}
	req.URL.RawQuery = q.Encode()

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram: getUpdates request: %w", t.redact(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &httpError{statusCode: resp.StatusCode}
	}

	var result telegramGetUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("telegram: decode getUpdates response: %w", err)
	}

	if !result.OK {
		return nil, fmt.Errorf("telegram: getUpdates API error: %s", result.Description)
	}

	// Update offset to the last processed update_id + 1
	if len(result.Result) > 0 {
		lastUpdate := result.Result[len(result.Result)-1]
		t.mu.Lock()
		t.offset = lastUpdate.UpdateID + 1
		t.mu.Unlock()
	}

	return result.Result, nil
}

// httpError wraps an HTTP status code for error checking.
type httpError struct {
	statusCode int
}

func (e *httpError) Error() string {
	return fmt.Sprintf("http error: %d", e.statusCode)
}

// Telegram API types

type telegramGetUpdatesResponse struct {
	OK          bool             `json:"ok"`
	Result      []telegramUpdate `json:"result"`
	Description string           `json:"description"`
}

type telegramUpdate struct {
	UpdateID int64            `json:"update_id"`
	Message  *telegramMessage `json:"message"`
}

type telegramMessage struct {
	MessageID int64         `json:"message_id"`
	From      *telegramUser `json:"from"`
	Chat      telegramChat  `json:"chat"`
	Date      int64         `json:"date"`
	Text      string        `json:"text"`
}

type telegramUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type telegramChat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
}
