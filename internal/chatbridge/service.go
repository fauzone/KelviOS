package chatbridge

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fauzone/kelvios/internal/approval"
	"github.com/fauzone/kelvios/internal/catalog"
)

// Service is the core chat bridge service.
// It holds all dependencies and manages transports, links, pairing, commands, and scheduled reports.
type Service struct {
	db            *sql.DB
	approvalStore *approval.ApprovalStore
	harnessRun    func(ctx context.Context, prompt string, tools []string) (string, error)
	transports    map[string]Transport // registered transports by name
	transportsMu  sync.RWMutex
	pinTracker    *PINFailureTracker
	reportTicker  *time.Ticker
	reportStop    chan struct{}
	reportWg      sync.WaitGroup
	started       bool                 // guards Start/Stop idempotence
	startedMu     sync.Mutex
}

func NewService(deps Deps) *Service {
	if deps.HarnessRun == nil {
		// Default: agent not configured
		deps.HarnessRun = func(ctx context.Context, prompt string, tools []string) (string, error) {
			return "Agent not configured. Available commands: /help, /status, /health, /backups, /approvals, /approve, /reject, /report", nil
		}
	}
	return &Service{
		db:            deps.DB,
		approvalStore: deps.ApprovalStore,
		harnessRun:    deps.HarnessRun,
		transports:    make(map[string]Transport),
		pinTracker:    NewPINFailureTracker(),
		reportStop:    make(chan struct{}),
	}
}

// RegisterTransport registers a transport by name.
// Called by concrete transport implementations (WhatsApp, Telegram, etc.) at startup.
func (s *Service) RegisterTransport(t Transport) {
	s.transportsMu.Lock()
	defer s.transportsMu.Unlock()
	s.transports[t.Name()] = t
	slog.Info("chatbridge: transport registered", "name", t.Name())
}

// GetTransport returns a registered transport by name.
func (s *Service) GetTransport(name string) (Transport, bool) {
	s.transportsMu.RLock()
	defer s.transportsMu.RUnlock()
	t, ok := s.transports[name]
	return t, ok
}

// HandleInbound is the entry point for inbound messages from transports.
// Transports call this method when they receive a message.
// It handles pairing, allowlist enforcement, command routing, and conversation passthrough.
// Every handled message writes an audit row.
func (s *Service) HandleInbound(ctx context.Context, msg InboundMessage) error {
	// Find or create link
	link, err := s.GetLink(msg.Transport, msg.ChatID)
	if err != nil {
		return fmt.Errorf("get link: %w", err)
	}

	actor := fmt.Sprintf("chat:%s:%s", msg.Transport, msg.ChatID)
	targetType := "chat_link"
	var targetID string
	if link != nil {
		targetID = link.ID
	}

	// Update last_seen for known links
	if link != nil {
		_ = s.UpdateLastSeen(link.ID)
	}

	text := strings.TrimSpace(msg.Text)
	isCommand := strings.HasPrefix(text, "/")

	// Handle pairing command (allowed for unlinked chats)
	if isCommand && strings.HasPrefix(text, "/pair") {
		return s.handlePair(ctx, msg, actor, targetType, targetID, text)
	}

	// If no link, only /pair and /help are allowed
	if link == nil {
		if isCommand && strings.HasPrefix(text, "/help") {
			reply, _, _, _ := s.handleCommand(ctx, nil, text)
			_ = s.sendReply(msg.Transport, msg.ChatID, reply)
			catalog.WriteAuditLog(s.db, actor, "chat.command.help", targetType, "", "ok", "help from unpaired chat")
			return nil
		}
		// Refuse all other messages from unpaired chats
		refuseMsg := "This chat is not paired. Use /pair <code> <pin> to pair, or /help for help."
		_ = s.sendReply(msg.Transport, msg.ChatID, refuseMsg)
		catalog.WriteAuditLog(s.db, actor, "chat.command.refused", targetType, "", "refused", "unpaired")
		return nil
	}

	// Linked chat: handle commands or conversation
	if isCommand {
		reply, action, detail, err := s.handleCommand(ctx, link, text)
		_ = s.sendReply(msg.Transport, msg.ChatID, reply)
		result := "ok"
		if err != nil {
			result = "error"
		}
		catalog.WriteAuditLog(s.db, actor, action, targetType, targetID, result, detail)
		return err
	}

	// Conversation passthrough
	reply, err := s.harnessRun(ctx, text, nil)
	if err != nil {
		reply = fmt.Sprintf("Agent error: %v", err)
	}
	_ = s.sendReply(msg.Transport, msg.ChatID, reply)
	catalog.WriteAuditLog(s.db, actor, "chat.conversation", targetType, targetID, "ok", "conversation passthrough")
	return nil
}

func (s *Service) handlePair(ctx context.Context, msg InboundMessage, actor, targetType, targetID, text string) error {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		reply := "Usage: /pair <code> <pin>"
		_ = s.sendReply(msg.Transport, msg.ChatID, reply)
		catalog.WriteAuditLog(s.db, actor, "chat.command.pair", targetType, targetID, "error", "missing args")
		return nil
	}

	code := parts[1]
	if len(parts) < 3 {
		reply := "Usage: /pair <code> <pin> (PIN required: 4-8 digits)"
		_ = s.sendReply(msg.Transport, msg.ChatID, reply)
		catalog.WriteAuditLog(s.db, actor, "chat.command.pair", targetType, targetID, "error", "missing PIN")
		return nil
	}
	pin := parts[2]

	// Validate PIN format: 4-8 digits
	if len(pin) < 4 || len(pin) > 8 {
		reply := "PIN must be 4-8 digits."
		_ = s.sendReply(msg.Transport, msg.ChatID, reply)
		catalog.WriteAuditLog(s.db, actor, "chat.command.pair", targetType, targetID, "error", "invalid PIN format")
		return nil
	}
	for _, c := range pin {
		if c < '0' || c > '9' {
			reply := "PIN must be 4-8 digits."
			_ = s.sendReply(msg.Transport, msg.ChatID, reply)
			catalog.WriteAuditLog(s.db, actor, "chat.command.pair", targetType, targetID, "error", "invalid PIN format")
			return nil
		}
	}

	// Look up pairing code
	pc, err := s.GetPairingCode(code)
	if err != nil {
		reply := "Pairing failed."
		_ = s.sendReply(msg.Transport, msg.ChatID, reply)
		catalog.WriteAuditLog(s.db, actor, "chat.command.pair", targetType, targetID, "error", "lookup failed")
		return err
	}
	if pc == nil {
		reply := "Invalid or expired pairing code."
		_ = s.sendReply(msg.Transport, msg.ChatID, reply)
		catalog.WriteAuditLog(s.db, actor, "chat.command.pair", targetType, targetID, "refused", "invalid/expired code")
		return nil
	}
	if pc.Transport != msg.Transport {
		reply := "Pairing code is for a different transport."
		_ = s.sendReply(msg.Transport, msg.ChatID, reply)
		catalog.WriteAuditLog(s.db, actor, "chat.command.pair", targetType, targetID, "refused", "transport mismatch")
		return nil
	}

	// Hash the PIN
	pinHash, err := hashPIN(pin)
	if err != nil {
		reply := "Pairing failed."
		_ = s.sendReply(msg.Transport, msg.ChatID, reply)
		catalog.WriteAuditLog(s.db, actor, "chat.command.pair", targetType, targetID, "error", "hash failed")
		return err
	}

	// Create the link
	label := fmt.Sprintf("%s:%s", msg.Transport, msg.ChatID)
	link, err := s.CreateLink(msg.Transport, msg.ChatID, label, pinHash)
	if err != nil {
		// Check for unique constraint violation (already linked)
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "unique") {
			reply := "This chat is already paired."
			_ = s.sendReply(msg.Transport, msg.ChatID, reply)
			catalog.WriteAuditLog(s.db, actor, "chat.command.pair", targetType, targetID, "refused", "already paired")
			return nil
		}
		reply := "Pairing failed."
		_ = s.sendReply(msg.Transport, msg.ChatID, reply)
		catalog.WriteAuditLog(s.db, actor, "chat.command.pair", targetType, targetID, "error", "create link failed")
		return err
	}

	// Mark pairing code as used
	_ = s.MarkPairingCodeUsed(code)

	reply := fmt.Sprintf("Paired successfully! Your chat is now linked as '%s'. Use /help for commands.", link.Label)
	_ = s.sendReply(msg.Transport, msg.ChatID, reply)
	catalog.WriteAuditLog(s.db, actor, "chat.command.pair", "chat_link", link.ID, "ok", "paired")
	return nil
}

func (s *Service) sendReply(transportName, chatID, text string) error {
	s.transportsMu.RLock()
	t, ok := s.transports[transportName]
	s.transportsMu.RUnlock()
	if !ok {
		slog.Warn("chatbridge: transport not registered for reply", "transport", transportName)
		return fmt.Errorf("transport %s not registered", transportName)
	}
	return t.Send(chatID, text)
}

// Start begins the scheduled report goroutine if enabled and starts all
// registered lifecycle transports.
// Interval comes from env KELVIOS_CHATBRIDGE_REPORT_INTERVAL_HOURS.
// <=0 or unset = disabled (no report goroutine started), but transports are still started.
// Idempotent: a second call is a no-op.
func (s *Service) Start(ctx context.Context) {
	s.startedMu.Lock()
	if s.started {
		s.startedMu.Unlock()
		return
	}
	s.started = true
	s.startedMu.Unlock()

	// Start lifecycle transports first (independent of report interval)
	s.transportsMu.RLock()
	transports := make([]LifecycleTransport, 0, len(s.transports))
	for _, t := range s.transports {
		if lt, ok := t.(LifecycleTransport); ok {
			transports = append(transports, lt)
		}
	}
	s.transportsMu.RUnlock()

	for _, lt := range transports {
		slog.Info("chatbridge: starting transport", "name", lt.Name())
		lt.Start(ctx)
	}

	// Then start scheduled reports if enabled
	intervalHours := 0
	if v := os.Getenv("KELVIOS_CHATBRIDGE_REPORT_INTERVAL_HOURS"); v != "" {
		fmt.Sscanf(v, "%d", &intervalHours)
	}
	if intervalHours <= 0 {
		slog.Info("chatbridge: scheduled reports disabled (KELVIOS_CHATBRIDGE_REPORT_INTERVAL_HOURS not set or <=0)")
		return
	}

	interval := time.Duration(intervalHours) * time.Hour
	s.reportTicker = time.NewTicker(interval)
	s.reportStop = make(chan struct{})
	s.reportWg.Add(1)
	go func() {
		defer s.reportWg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.reportStop:
				return
			case <-s.reportTicker.C:
				s.sendScheduledReports()
			}
		}
	}()
	slog.Info("chatbridge: scheduled reports started", "interval_hours", intervalHours)
}

// Stop stops all lifecycle transports and the scheduled report goroutine.
// Safe to call multiple times; safe when Start was never called.
func (s *Service) Stop() {
	s.startedMu.Lock()
	if !s.started {
		s.startedMu.Unlock()
		return
	}
	s.started = false
	s.startedMu.Unlock()

	// Stop lifecycle transports first
	s.transportsMu.RLock()
	transports := make([]LifecycleTransport, 0, len(s.transports))
	for _, t := range s.transports {
		if lt, ok := t.(LifecycleTransport); ok {
			transports = append(transports, lt)
		}
	}
	s.transportsMu.RUnlock()

	for _, lt := range transports {
		slog.Info("chatbridge: stopping transport", "name", lt.Name())
		lt.Stop()
	}

	// Then stop scheduled reports
	if s.reportTicker != nil {
		s.reportTicker.Stop()
		if s.reportStop != nil {
			close(s.reportStop)
		}
		s.reportWg.Wait()
		slog.Info("chatbridge: scheduled reports stopped")
	}
}

func (s *Service) sendScheduledReports() {
	report, err := BuildReport(s.db, time.Now())
	if err != nil {
		slog.Error("chatbridge: build scheduled report failed", "error", err)
		return
	}

	links, err := s.ListLinks()
	if err != nil {
		slog.Error("chatbridge: list links for scheduled report failed", "error", err)
		return
	}

	s.transportsMu.RLock()
	transports := make(map[string]Transport, len(s.transports))
	for k, v := range s.transports {
		transports[k] = v
	}
	s.transportsMu.RUnlock()

	for _, link := range links {
		t, ok := transports[link.Transport]
		if !ok {
			slog.Warn("chatbridge: transport not registered for scheduled report", "transport", link.Transport, "chat_id", link.ChatID)
			continue
		}
		if err := t.Send(link.ChatID, report); err != nil {
			slog.Error("chatbridge: send scheduled report failed", "transport", link.Transport, "chat_id", link.ChatID, "error", err)
		}
	}
}
