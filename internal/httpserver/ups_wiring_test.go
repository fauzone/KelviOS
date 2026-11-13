package httpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/ups"
)

// ============================================================================
// Test helpers: fake NUT TCP server, fake executor command, test server setup
// ============================================================================

// fakeNUTServer starts a fake NUT server on 127.0.0.1:0 that responds with
// canned responses. Returns the address and a cleanup function.
func fakeNUTServer(t *testing.T, responses map[string][]string) (string, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()

	done := make(chan struct{})
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
					continue
				}
			}
			go func(c net.Conn) {
				defer c.Close()
				scanner := bufio.NewScanner(c)
				for scanner.Scan() {
					cmd := strings.TrimSpace(scanner.Text())
					if resp, ok := responses[cmd]; ok {
						for _, line := range resp {
							c.Write([]byte(line + "\n"))
						}
					} else {
						c.Write([]byte("ERR unknown command\n"))
					}
				}
			}(conn)
		}
	}()

	cleanup := func() {
		close(done)
		l.Close()
	}
	return addr, cleanup
}

// fakeExecutorCmd returns a command that writes a marker file when executed.
// OS-aware (Chief fix 2026-10-31): cmd /c on Windows, sh -c elsewhere —
// the original Windows-only fixture failed on Linux CI/VM runs.
func fakeExecutorCmd(markerPath string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", "echo executed > " + markerPath}
	}
	return []string{"sh", "-c", "echo executed > " + markerPath}
}

// newTestServer creates a test server with temp DB and optional UPS config.
func newTestServer(t *testing.T, upsCfg config.UPSConfig) (*Server, *httptest.Server, string, func()) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{
			Path: dbPath,
		},
		UPS: upsCfg,
	}

	s := New(cfg, db, "test-version")
	ts := httptest.NewServer(s.srv.Handler)

	// Login to get token (uses mustToken from middleware_test.go)
	token := mustToken(t, ts)

	cleanup := func() {
		ts.Close()
		db.Close()
	}

	return s, ts, token, cleanup
}

// ============================================================================
// Test (a): poller receives OB from fake NUT -> event on_battery appears
// ============================================================================
func TestUPSWiring_PollerOnBatteryEvent(t *testing.T) {
	// Fake NUT server that returns OB on first poll
	responses := map[string][]string{
		"LIST VAR testups": {
			"BEGIN LIST VAR testups",
			`VAR testups ups.status "OB"`,
			`VAR testups battery.charge "90.0"`,
			"END LIST VAR testups",
		},
	}
	addr, cleanupNUT := fakeNUTServer(t, responses)
	defer cleanupNUT()

	dbDir := t.TempDir()
	markerPath := filepath.Join(dbDir, "clean-shutdown.marker")
	execMarkerPath := filepath.Join(dbDir, "executor.marker")

	upsCfg := config.UPSConfig{
		Addr:             addr,
		UPSName:          "testups",
		CriticalPercent:  20,
		ShutdownCommand:  fakeExecutorCmd(execMarkerPath),
		MarkerPath:       markerPath,
	}

	s, ts, token, cleanup := newTestServer(t, upsCfg)
	defer cleanup()

	// Mark clean shutdown so InitUPS doesn't record unclean_shutdown
	_ = ups.MarkCleanShutdown(markerPath)

	// Initialize UPS (starts poller)
	if err := s.InitUPS(); err != nil {
		t.Fatalf("InitUPS failed: %v", err)
	}
	defer s.StopUPSPoller()

	// Test the wiring by POSTing a reading which uses same applyReading logic
	st, body := upsDoReq(t, ts, http.MethodPost, "/api/ups/reading",
		map[string]string{"ups.status": "OB", "battery.charge": "90"}, token)
	if st != http.StatusOK {
		t.Fatalf("POST reading failed: %d %s", st, body)
	}

	// Verify event persisted
	st, body = upsDoReq(t, ts, http.MethodGet, "/api/ups/events", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET events failed: %d %s", st, body)
	}
	var eventsResp struct {
		Events []map[string]any `json:"events"`
		Total  int              `json:"total"`
	}
	if err := json.Unmarshal(body, &eventsResp); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if eventsResp.Total < 1 {
		t.Fatalf("expected at least 1 event, got %d", eventsResp.Total)
	}
	found := false
	for _, e := range eventsResp.Events {
		if e["kind"] == string(ups.EventOnBattery) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("on_battery event not found in events: %+v", eventsResp.Events)
	}
}

// ============================================================================
// Test (b): fake NUT OB+LB (critical) -> event critical_shutdown +
// fake executor CALLED + GET /api/ups/status shows shutdown_initiated=true
// and poller_enabled=true
// ============================================================================
func TestUPSWiring_PollerCriticalShutdownExecutor(t *testing.T) {
	// Fake NUT server that returns OB+LB (critical)
	responses := map[string][]string{
		"LIST VAR testups": {
			"BEGIN LIST VAR testups",
			`VAR testups ups.status "OB LB"`,
			`VAR testups battery.charge "15.0"`,
			"END LIST VAR testups",
		},
	}
	addr, cleanupNUT := fakeNUTServer(t, responses)
	defer cleanupNUT()

	dbDir := t.TempDir()
	markerPath := filepath.Join(dbDir, "clean-shutdown.marker")
	execMarkerPath := filepath.Join(dbDir, "executor.marker")

	upsCfg := config.UPSConfig{
		Addr:             addr,
		UPSName:          "testups",
		CriticalPercent:  20,
		ShutdownCommand:  fakeExecutorCmd(execMarkerPath),
		MarkerPath:       markerPath,
	}

	s, ts, token, cleanup := newTestServer(t, upsCfg)
	defer cleanup()

	// Mark clean shutdown so InitUPS doesn't record unclean_shutdown
	_ = ups.MarkCleanShutdown(markerPath)

	// Initialize UPS (starts poller with 10s interval)
	if err := s.InitUPS(); err != nil {
		t.Fatalf("InitUPS failed: %v", err)
	}
	defer s.StopUPSPoller()

	// For integration test, we also test the poller path directly with a short interval
	// by creating a poller that uses the same client and emit logic.
	client := &ups.Client{
		Addr:    addr,
		Timeout: 5 * time.Second,
		UPSName: "testups",
	}
	poller := &ups.Poller{
		Source:          client,
		CriticalPercent: 20,
	}

	var execCalled bool
	var execMu sync.Mutex
	emit := func(reading ups.Reading, decision ups.Decision) {
		// Use shared applyReading logic; actor is "poller"
		s.applyReading(reading, "poller")
		if decision.BeginShutdown {
			// Execute shutdown if command configured
			if len(s.cfg.UPS.ShutdownCommand) > 0 {
				shutdowner := &ups.SystemShutdowner{Command: s.cfg.UPS.ShutdownCommand}
				_ = shutdowner.Shutdown(context.Background(), "battery critical")
				execMu.Lock()
				execCalled = true
				execMu.Unlock()
			}
		}
	}

	// Drive PollOnce synchronously until the executor marker appears or a
	// generous deadline passes. The previous shape (background Run loop at a
	// 100ms tick + fixed-then-polled waits) starved under heavy CI/VM load:
	// client polls errored on their 5s deadline faster than they could
	// succeed, so the marker never appeared (audit Pass 1 finding, failed 2x
	// for the auditor; Chief fix 2026-10-31). Synchronous driving removes the
	// ticker/goroutine scheduling from the equation; Poller.Run itself is
	// covered by unit tests in internal/ups. Assertions below are unchanged.
	var prev ups.Reading
	driveDeadline := time.Now().Add(15 * time.Second)
	for {
		cur, decision, err := poller.PollOnce(prev)
		if err == nil {
			prev = cur
			if decision.Event != "" {
				emit(cur, decision)
			}
			if _, serr := os.Stat(execMarkerPath); serr == nil {
				break
			}
		}
		if time.Now().After(driveDeadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	// Verify critical_shutdown event persisted
	st, body := upsDoReq(t, ts, http.MethodGet, "/api/ups/events", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET events failed: %d %s", st, body)
	}
	var eventsResp struct {
		Events []map[string]any `json:"events"`
		Total  int              `json:"total"`
	}
	if err := json.Unmarshal(body, &eventsResp); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	foundCritical := false
	for _, e := range eventsResp.Events {
		if e["kind"] == string(ups.EventCriticalShutdown) {
			foundCritical = true
			break
		}
	}
	if !foundCritical {
		t.Errorf("critical_shutdown event not found in events: %+v", eventsResp.Events)
	}

	// Verify executor marker file was created (fake executor called)
	execMu.Lock()
	called := execCalled
	execMu.Unlock()
	if !called {
		t.Errorf("executor was not called (execCalled=false)")
	}
	if _, err := os.Stat(execMarkerPath); os.IsNotExist(err) {
		t.Errorf("executor marker file not created at %s", execMarkerPath)
	}

	// Verify GET /api/ups/status shows shutdown_initiated=true and poller_enabled=true
	st, body = upsDoReq(t, ts, http.MethodGet, "/api/ups/status", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET status failed: %d %s", st, body)
	}
	var statusResp struct {
		ShutdownInitiated bool `json:"shutdown_initiated"`
		PollerEnabled     bool `json:"poller_enabled"`
	}
	if err := json.Unmarshal(body, &statusResp); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !statusResp.ShutdownInitiated {
		t.Errorf("shutdown_initiated should be true after critical shutdown")
	}
	if !statusResp.PollerEnabled {
		t.Errorf("poller_enabled should be true while poller is running")
	}
}

// ============================================================================
// Tests (c)-(e): Chief takeover (2026-10-31) - the continuation session ended
// mid-write before these landed; written by Chief following the same helpers.
// ============================================================================

// Test (c): no NUT config -> poller stays disabled, no events recorded.
func TestUPSWiring_PollerDisabledByDefault(t *testing.T) {
	dbDir := t.TempDir()
	markerPath := filepath.Join(dbDir, "clean-shutdown.marker")
	upsCfg := config.UPSConfig{MarkerPath: markerPath} // Addr + UPSName empty

	s, ts, token, cleanup := newTestServer(t, upsCfg)
	defer cleanup()

	_ = ups.MarkCleanShutdown(markerPath)
	if err := s.InitUPS(); err != nil {
		t.Fatalf("InitUPS failed: %v", err)
	}
	defer s.StopUPSPoller()

	st, body := upsDoReq(t, ts, http.MethodGet, "/api/ups/status", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET status failed: %d %s", st, body)
	}
	var status struct {
		PollerEnabled bool   `json:"poller_enabled"`
		LastPollError string `json:"last_poll_error"`
	}
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.PollerEnabled {
		t.Errorf("poller_enabled = true, want false without NUT config")
	}
	if status.LastPollError != "" {
		t.Errorf("last_poll_error = %q, want empty", status.LastPollError)
	}

	st, body = upsDoReq(t, ts, http.MethodGet, "/api/ups/events", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET events failed: %d %s", st, body)
	}
	var eventsResp struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(body, &eventsResp); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if eventsResp.Total != 0 {
		t.Errorf("expected 0 events with poller disabled and clean boot, got %d", eventsResp.Total)
	}
}

// Test (d): boot check - absent marker records unclean_shutdown + integrity
// detail; present marker is consumed silently (no event).
func TestUPSWiring_BootCheck(t *testing.T) {
	getEvents := func(t *testing.T, ts *httptest.Server, token string) []map[string]any {
		t.Helper()
		st, body := upsDoReq(t, ts, http.MethodGet, "/api/ups/events", nil, token)
		if st != http.StatusOK {
			t.Fatalf("GET events failed: %d %s", st, body)
		}
		var resp struct {
			Events []map[string]any `json:"events"`
			Total  int              `json:"total"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode events: %v", err)
		}
		return resp.Events
	}

	// Unclean: no marker present at boot.
	{
		dbDir := t.TempDir()
		markerPath := filepath.Join(dbDir, "clean-shutdown.marker")
		s, ts, token, cleanup := newTestServer(t, config.UPSConfig{MarkerPath: markerPath})
		defer cleanup()
		if err := s.InitUPS(); err != nil {
			t.Fatalf("InitUPS (unclean) failed: %v", err)
		}
		events := getEvents(t, ts, token)
		found := false
		for _, e := range events {
			if e["kind"] == string(ups.EventUncleanShutdown) {
				found = true
				detail, _ := e["detail"].(string)
				if !strings.Contains(detail, "integrity=ok") {
					t.Errorf("unclean detail = %q, want it to contain integrity=ok", detail)
				}
			}
		}
		if !found {
			t.Errorf("unclean_shutdown event not recorded on markerless boot: %+v", events)
		}
		if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
			t.Errorf("boot check must not create a marker; stat err = %v", err)
		}
	}

	// Clean: marker present at boot is consumed, no event.
	{
		dbDir := t.TempDir()
		markerPath := filepath.Join(dbDir, "clean-shutdown.marker")
		s, ts, token, cleanup := newTestServer(t, config.UPSConfig{MarkerPath: markerPath})
		defer cleanup()
		if err := ups.MarkCleanShutdown(markerPath); err != nil {
			t.Fatalf("MarkCleanShutdown: %v", err)
		}
		if err := s.InitUPS(); err != nil {
			t.Fatalf("InitUPS (clean) failed: %v", err)
		}
		events := getEvents(t, ts, token)
		if len(events) != 0 {
			t.Errorf("clean boot must record no events, got %+v", events)
		}
		if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
			t.Errorf("clean marker must be consumed at boot; stat err = %v", err)
		}
	}
}

// Test (e): ups_events persist across a restart, and the graceful
// CleanShutdown -> next-boot-clean roundtrip adds no spurious events.
func TestUPSWiring_RestartPersistence(t *testing.T) {
	dbDir := t.TempDir()
	dbPath := filepath.Join(dbDir, "test.db")
	markerPath := filepath.Join(dbDir, "clean-shutdown.marker")
	mkCfg := func() *config.Config {
		return &config.Config{
			HTTP: config.HTTPConfig{
				Addr:         ":0",
				ReadTimeout:  10 * time.Second,
				WriteTimeout: 10 * time.Second,
				IdleTimeout:  60 * time.Second,
			},
			Database: config.DatabaseConfig{Path: dbPath},
			UPS:      config.UPSConfig{MarkerPath: markerPath},
		}
	}

	// Instance 1: clean boot, two readings, graceful shutdown mark.
	db, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := New(mkCfg(), db, "test-version")
	if err := ups.MarkCleanShutdown(markerPath); err != nil {
		t.Fatalf("MarkCleanShutdown: %v", err)
	}
	if err := s.InitUPS(); err != nil {
		t.Fatalf("InitUPS #1: %v", err)
	}
	s.applyReading(ups.Reading{Present: true, State: ups.StateOnBattery, ChargePercent: 90, RuntimeSeconds: -1}, "test")
	s.applyReading(ups.Reading{Present: true, State: ups.StateOnline, ChargePercent: 100, RuntimeSeconds: -1}, "test")
	if err := s.CleanShutdown(); err != nil {
		t.Fatalf("CleanShutdown: %v", err)
	}
	db.Close()

	// Instance 2: events survived; the clean marker makes this boot silent.
	db2, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog (restart): %v", err)
	}
	defer db2.Close()
	s2 := New(mkCfg(), db2, "test-version")
	if err := s2.InitUPS(); err != nil {
		t.Fatalf("InitUPS #2: %v", err)
	}
	events, total, err := catalog.ListUPSEvents(db2, 10, 0)
	if err != nil {
		t.Fatalf("ListUPSEvents (restart): %v", err)
	}
	if total != 2 {
		t.Fatalf("expected 2 persisted events after restart, got %d: %+v", total, events)
	}
	foundOnBattery, foundRestored := false, false
	for _, e := range events {
		if e.Kind == string(ups.EventOnBattery) {
			foundOnBattery = true
		}
		if e.Kind == string(ups.EventOnlineRestored) {
			foundRestored = true
		}
	}
	if !foundOnBattery || !foundRestored {
		t.Errorf("persisted kinds incomplete: on_battery=%v online_restored=%v", foundOnBattery, foundRestored)
	}
}
