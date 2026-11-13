package chatbridge

import (
	"context"
	"log/slog"
	"time"
)

// NotifyLockout sends a lockout notification to all linked chats.
// It is best-effort and non-blocking: failures are logged but do not
// propagate as errors. If no linked chats exist, it logs at debug level
// and returns without error.
func (s *Service) NotifyLockout(username string, lockoutMinutes int) {
	links, err := s.ListLinks()
	if err != nil {
		slog.Error("chatbridge: notify lockout list links failed", "error", err)
		return
	}
	slog.Debug("chatbridge: notify lockout triggered", "username", username, "links", len(links))
	if len(links) == 0 {
		slog.Debug("chatbridge: notify lockout skipped — no linked chats", "username", username)
		return
	}

	// Format time in WIB (UTC+7)
	now := time.Now()
	wib := now.Add(7 * time.Hour)
	timeStr := wib.Format("15:04")

	msg := "KelviOS: akun " + username + " terkunci " + itoa(lockoutMinutes) + " menit setelah 5x gagal login (" + timeStr + " WIB). Bila bukan Anda, periksa Activity log."

	// Fire-and-forget: send to each linked chat via its transport
	for _, link := range links {
		go func(l *ChatLink) {
			if err := s.sendReply(l.Transport, l.ChatID, msg); err != nil {
				slog.Error("chatbridge: notify lockout send failed", "transport", l.Transport, "chat_id", l.ChatID, "error", err)
			}
		}(link)
	}
}

// itoa is a small helper to avoid importing strconv just for this.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// NotifyLockoutSync is a synchronous variant for testing.
// It returns the number of messages sent and any error from ListLinks.
func (s *Service) NotifyLockoutSync(ctx context.Context, username string, lockoutMinutes int) (int, error) {
	links, err := s.ListLinks()
	if err != nil {
		return 0, err
	}
	if len(links) == 0 {
		slog.Debug("chatbridge: notify lockout skipped — no linked chats", "username", username)
		return 0, nil
	}

	now := time.Now()
	wib := now.Add(7 * time.Hour)
	timeStr := wib.Format("15:04")

	msg := "KelviOS: akun " + username + " terkunci " + itoa(lockoutMinutes) + " menit setelah 5x gagal login (" + timeStr + " WIB). Bila bukan Anda, periksa Activity log."

	sent := 0
	for _, link := range links {
		if err := s.sendReply(link.Transport, link.ChatID, msg); err != nil {
			slog.Error("chatbridge: notify lockout send failed", "transport", link.Transport, "chat_id", link.ChatID, "error", err)
			continue
		}
		sent++
	}
	return sent, nil
}