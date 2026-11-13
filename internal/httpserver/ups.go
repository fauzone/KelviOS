package httpserver

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/ups"
)

// ============================================================================
// WS19: UPS HTTP handlers (FR-1.12)
// ============================================================================

// upsMonitor holds in-memory UPS state for the HTTP layer.
type upsMonitor struct {
	mu                sync.Mutex
	lastReading       ups.Reading
	lastEventKind     ups.EventKind
	shutdownInitiated bool
}

// newUPSMonitor returns a fresh monitor with not-present/unknown initial state.
func newUPSMonitor() *upsMonitor {
	return &upsMonitor{
		lastReading: ups.Reading{
			Present:        false,
			State:          ups.StateUnknown,
			ChargePercent:  -1,
			RuntimeSeconds: -1,
		},
		lastEventKind:     "",
		shutdownInitiated: false,
	}
}

// applyReading applies a reading to the monitor, records events, and returns the decision.
// This is the shared logic used by both the POST handler and the poller.
// It does NOT write HTTP responses; it only updates in-memory state and persists events.
func (s *Server) applyReading(cur ups.Reading, actor string) ups.Decision {
	s.upsMon.mu.Lock()
	prev := s.upsMon.lastReading
	decision := ups.Decide(prev, cur, ups.DefaultCriticalPercent)
	s.upsMon.lastReading = cur

	// Determine if we should record an event
	shouldRecord := decision.Event != "" && (decision.Event != s.upsMon.lastEventKind || decision.Event == ups.EventCriticalShutdown)
	if shouldRecord {
		detail := "state=" + string(cur.State) + " charge=" + itoa(cur.ChargePercent)
		event := &catalog.UPSEventRow{
			Kind:          string(decision.Event),
			State:         string(cur.State),
			ChargePercent: cur.ChargePercent,
			Detail:        detail,
		}
		if err := catalog.InsertUPSEvent(s.db, event); err != nil {
			slog.Error("insert ups event failed", "error", err)
		}
		catalog.WriteAuditLog(s.db, actor, "ups."+string(decision.Event), "ups", "", "ok", detail)
		s.upsMon.lastEventKind = decision.Event
		if decision.BeginShutdown {
			s.upsMon.shutdownInitiated = true
		}
	} else if decision.Event == "" && (cur.State == ups.StateOnline || cur.State == ups.StateUnknown) {
		// Reset lastEventKind when no event and state is online/unknown
		s.upsMon.lastEventKind = ""
	}
	s.upsMon.mu.Unlock()
	return decision
}

// handleUPSReading handles POST /api/ups/reading
func (s *Server) handleUPSReading(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var vars map[string]string
	if err := json.NewDecoder(r.Body).Decode(&vars); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "ups.reading", "ups", "", "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Parse NUT vars to Reading
	cur := ups.ParseVars(vars)

	// Apply reading using shared logic
	actor := actorFromRequest(r)
	decision := s.applyReading(cur, actor)

	// Response
	resp := map[string]any{
		"reading":  cur,
		"decision": decision,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleUPSStatus handles GET /api/ups/status
func (s *Server) handleUPSStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	s.upsMon.mu.Lock()
	reading := s.upsMon.lastReading
	shutdownInitiated := s.upsMon.shutdownInitiated
	lastEvent := s.upsMon.lastEventKind
	pollerEnabled := s.upsPollerCancel != nil
	lastPollError := s.upsPollerErr
	s.upsMon.mu.Unlock()

	resp := map[string]any{
		"reading":             reading,
		"critical_percent":    ups.DefaultCriticalPercent,
		"shutdown_initiated":  shutdownInitiated,
		"last_event":          lastEvent,
		"poller_enabled":      pollerEnabled,
		"last_poll_error":     lastPollError,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// handleUPSEvents handles GET /api/ups/events
func (s *Server) handleUPSEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	limit := parseIntQuery(r, "limit", 100)
	offset := parseIntQuery(r, "offset", 0)

	events, total, err := catalog.ListUPSEvents(s.db, limit, offset)
	if err != nil {
		slog.Error("list ups events failed", "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list ups events failed")
		return
	}

	result := make([]map[string]any, 0, len(events))
	for _, e := range events {
		result = append(result, map[string]any{
			"id":             e.ID,
			"ts":             e.TS.Unix(),
			"kind":           e.Kind,
			"state":          e.State,
			"charge_percent": e.ChargePercent,
			"detail":         e.Detail,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"events": result,
		"total":  total,
	})
}

// itoa converts int to string (stdlib only).
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}
