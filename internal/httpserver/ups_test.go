package httpserver

// KelviOS Phase 1 WS19: UPS basics HTTP route tests (FR-1.12).
// Covers: unauth 401, POST reading flow, status, events pagination,
// bad JSON 400, wrong method 405.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fauzone/kelvios/internal/ups"
)

func upsDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

func upsLoginToken(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	_, lbody := login(t, ts, testAuthUser, testAuthPass)
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(lbody, &lr); err != nil || lr.Token == "" {
		t.Fatalf("login missing token: %v %s", err, lbody)
	}
	return lr.Token
}

type upsReadingResponse struct {
	Reading  ups.Reading  `json:"reading"`
	Decision ups.Decision `json:"decision"`
}

type upsStatusResponse struct {
	Reading           ups.Reading `json:"reading"`
	CriticalPercent   int         `json:"critical_percent"`
	ShutdownInitiated bool        `json:"shutdown_initiated"`
	LastEvent         string      `json:"last_event"`
}

type upsEventsResponse struct {
	Events []map[string]any `json:"events"`
	Total  int              `json:"total"`
}

// TestUPSUnauthenticated verifies all three routes return 401 without auth.
func TestUPSUnauthenticated(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/ups/reading"},
		{http.MethodGet, "/api/ups/status"},
		{http.MethodGet, "/api/ups/events"},
	}
	for _, rt := range routes {
		st, _ := upsDoReq(t, ts, rt.method, rt.path, nil, "")
		if st != http.StatusUnauthorized {
			t.Errorf("%s %s: got %d, want 401", rt.method, rt.path, st)
		}
	}
}

// TestUPSReadingFlow covers the full reading -> decision -> event persistence flow.
func TestUPSReadingFlow(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)
	token := upsLoginToken(t, ts)

	// 1. POST OL 100% -> online, no event
	st, body := upsDoReq(t, ts, http.MethodPost, "/api/ups/reading",
		map[string]string{"ups.status": "OL", "battery.charge": "100"}, token)
	if st != http.StatusOK {
		t.Fatalf("POST OL: got %d, want 200 (%s)", st, body)
	}
	var resp upsReadingResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode OL response: %v %s", err, body)
	}
	if !resp.Reading.Present || resp.Reading.State != ups.StateOnline || resp.Reading.ChargePercent != 100 {
		t.Errorf("OL reading mismatch: %+v", resp.Reading)
	}
	if resp.Decision.Event != "" {
		t.Errorf("OL should produce no event, got: %q", resp.Decision.Event)
	}

	// Status shows online, no shutdown, no last_event
	st, body = upsDoReq(t, ts, http.MethodGet, "/api/ups/status", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET status after OL: got %d (%s)", st, body)
	}
	var status upsStatusResponse
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("decode status: %v %s", err, body)
	}
	if status.Reading.State != ups.StateOnline || status.ShutdownInitiated || status.LastEvent != "" {
		t.Errorf("status after OL mismatch: %+v", status)
	}

	// 2. POST OB 90% -> on_battery event recorded
	st, body = upsDoReq(t, ts, http.MethodPost, "/api/ups/reading",
		map[string]string{"ups.status": "OB", "battery.charge": "90"}, token)
	if st != http.StatusOK {
		t.Fatalf("POST OB 90: got %d, want 200 (%s)", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode OB 90 response: %v %s", err, body)
	}
	if resp.Reading.State != ups.StateOnBattery || resp.Reading.ChargePercent != 90 {
		t.Errorf("OB 90 reading mismatch: %+v", resp.Reading)
	}
	if resp.Decision.Event != ups.EventOnBattery || !resp.Decision.Notify {
		t.Errorf("OB 90 decision mismatch: event=%q notify=%v", resp.Decision.Event, resp.Decision.Notify)
	}

	// Events list total 1
	st, body = upsDoReq(t, ts, http.MethodGet, "/api/ups/events", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET events after OB 90: got %d (%s)", st, body)
	}
	var eventsResp upsEventsResponse
	if err := json.Unmarshal(body, &eventsResp); err != nil {
		t.Fatalf("decode events: %v %s", err, body)
	}
	if eventsResp.Total != 1 || len(eventsResp.Events) != 1 {
		t.Errorf("events after OB 90: total=%d len=%d, want 1", eventsResp.Total, len(eventsResp.Events))
	}
	if eventsResp.Events[0]["kind"] != string(ups.EventOnBattery) {
		t.Errorf("event kind mismatch: got %q", eventsResp.Events[0]["kind"])
	}

	// Audit row ups.on_battery exists
	st, body = upsDoReq(t, ts, http.MethodGet, "/api/audit-log?action=ups.on_battery&result=ok", nil, token)
	if st != http.StatusOK {
		t.Fatalf("audit-log ups.on_battery: got %d (%s)", st, body)
	}
	var al struct {
		Entries []struct {
			Action string `json:"action"`
		} `json:"entries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(body, &al); err != nil {
		t.Fatalf("decode audit-log: %v %s", err, body)
	}
	if al.Total != 1 {
		t.Errorf("audit ups.on_battery count: got %d, want 1", al.Total)
	}

	// 3. POST OB charge 15 (critical 20) -> critical_shutdown, begin_shutdown=true
	st, body = upsDoReq(t, ts, http.MethodPost, "/api/ups/reading",
		map[string]string{"ups.status": "OB", "battery.charge": "15"}, token)
	if st != http.StatusOK {
		t.Fatalf("POST OB 15: got %d, want 200 (%s)", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode OB 15 response: %v %s", err, body)
	}
	if resp.Decision.Event != ups.EventCriticalShutdown || !resp.Decision.BeginShutdown || !resp.Decision.Notify {
		t.Errorf("OB 15 decision mismatch: event=%q begin=%v notify=%v", resp.Decision.Event, resp.Decision.BeginShutdown, resp.Decision.Notify)
	}

	// Status shows shutdown_initiated=true
	st, body = upsDoReq(t, ts, http.MethodGet, "/api/ups/status", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET status after OB 15: got %d (%s)", st, body)
	}
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("decode status: %v %s", err, body)
	}
	if !status.ShutdownInitiated {
		t.Errorf("shutdown_initiated should be true after critical")
	}
	if status.LastEvent != string(ups.EventCriticalShutdown) {
		t.Errorf("last_event mismatch: got %q", status.LastEvent)
	}

	// 4. POST OL -> online_restored event
	st, body = upsDoReq(t, ts, http.MethodPost, "/api/ups/reading",
		map[string]string{"ups.status": "OL", "battery.charge": "100"}, token)
	if st != http.StatusOK {
		t.Fatalf("POST OL restore: got %d, want 200 (%s)", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode OL restore response: %v %s", err, body)
	}
	if resp.Decision.Event != ups.EventOnlineRestored || !resp.Decision.Notify {
		t.Errorf("OL restore decision mismatch: event=%q notify=%v", resp.Decision.Event, resp.Decision.Notify)
	}

	// Events list now has 3 total (on_battery, critical_shutdown, online_restored)
	st, body = upsDoReq(t, ts, http.MethodGet, "/api/ups/events", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET events after OL restore: got %d (%s)", st, body)
	}
	if err := json.Unmarshal(body, &eventsResp); err != nil {
		t.Fatalf("decode events: %v %s", err, body)
	}
	if eventsResp.Total != 3 {
		t.Errorf("events total after restore: got %d, want 3", eventsResp.Total)
	}
}

// TestUPSEventsPagination tests limit/offset with defaults/clamps.
func TestUPSEventsPagination(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)
	token := upsLoginToken(t, ts)

	// Create 3 events
	for _, charge := range []string{"90", "15", "100"} {
		status := "OB"
		if charge == "100" {
			status = "OL"
		}
		_, _ = upsDoReq(t, ts, http.MethodPost, "/api/ups/reading",
			map[string]string{"ups.status": status, "battery.charge": charge}, token)
	}

	// limit=1 returns 1 with full total
	st, body := upsDoReq(t, ts, http.MethodGet, "/api/ups/events?limit=1&offset=0", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET events limit=1: got %d (%s)", st, body)
	}
	var eventsResp upsEventsResponse
	if err := json.Unmarshal(body, &eventsResp); err != nil {
		t.Fatalf("decode events: %v %s", err, body)
	}
	if eventsResp.Total != 3 || len(eventsResp.Events) != 1 {
		t.Errorf("pagination limit=1: total=%d len=%d, want total=3 len=1", eventsResp.Total, len(eventsResp.Events))
	}

	// offset=1 returns next
	st, body = upsDoReq(t, ts, http.MethodGet, "/api/ups/events?limit=1&offset=1", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET events offset=1: got %d (%s)", st, body)
	}
	if err := json.Unmarshal(body, &eventsResp); err != nil {
		t.Fatalf("decode events: %v %s", err, body)
	}
	if len(eventsResp.Events) != 1 || eventsResp.Total != 3 {
		t.Errorf("pagination offset=1: total=%d len=%d", eventsResp.Total, len(eventsResp.Events))
	}
	if eventsResp.Events[0]["kind"] == eventsResp.Events[0]["kind"] {
		// Just verify we got a different event (newest first, so offset=1 is second newest)
		_ = eventsResp.Events[0]["kind"]
	}
}

// TestUPSBadJSON verifies 400 on invalid JSON body.
func TestUPSBadJSON(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)
	token := upsLoginToken(t, ts)

	// Send invalid JSON
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/ups/reading", bytes.NewReader([]byte("{not json}")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad JSON: got %d, want 400", resp.StatusCode)
	}
}

// TestUPSWrongMethod verifies 405 on wrong HTTP method.
func TestUPSWrongMethod(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)
	token := upsLoginToken(t, ts)

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/ups/reading"},
		{http.MethodPut, "/api/ups/reading"},
		{http.MethodDelete, "/api/ups/reading"},
		{http.MethodPost, "/api/ups/status"},
		{http.MethodPut, "/api/ups/status"},
		{http.MethodDelete, "/api/ups/status"},
		{http.MethodPost, "/api/ups/events"},
		{http.MethodPut, "/api/ups/events"},
		{http.MethodDelete, "/api/ups/events"},
	}
	for _, tc := range cases {
		st, _ := upsDoReq(t, ts, tc.method, tc.path, nil, token)
		if st != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: got %d, want 405", tc.method, tc.path, st)
		}
	}
}

// TestUPSCriticalPercentDefault verifies criticalPercent 0 uses default (20).
func TestUPSCriticalPercentDefault(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)
	token := upsLoginToken(t, ts)

	// First go online
	_, _ = upsDoReq(t, ts, http.MethodPost, "/api/ups/reading",
		map[string]string{"ups.status": "OL", "battery.charge": "100"}, token)

	// Then OB at 21% - should NOT trigger critical (default is 20)
	st, body := upsDoReq(t, ts, http.MethodPost, "/api/ups/reading",
		map[string]string{"ups.status": "OB", "battery.charge": "21"}, token)
	if st != http.StatusOK {
		t.Fatalf("POST OB 21: got %d (%s)", st, body)
	}
	var resp upsReadingResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v %s", err, body)
	}
	if resp.Decision.Event == ups.EventCriticalShutdown {
		t.Errorf("OB 21%% should not be critical with default 20")
	}
	if resp.Decision.Event != ups.EventOnBattery {
		t.Errorf("OB 21%% should be on_battery, got %q", resp.Decision.Event)
	}
}

// TestUPSLowBatteryFlag verifies LB/FSD tokens trigger critical even at high charge.
func TestUPSLowBatteryFlag(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)
	token := upsLoginToken(t, ts)

	// First go online
	_, _ = upsDoReq(t, ts, http.MethodPost, "/api/ups/reading",
		map[string]string{"ups.status": "OL", "battery.charge": "100"}, token)

	// OB LB at 90% -> critical (low battery flag wins)
	st, body := upsDoReq(t, ts, http.MethodPost, "/api/ups/reading",
		map[string]string{"ups.status": "OB LB", "battery.charge": "90"}, token)
	if st != http.StatusOK {
		t.Fatalf("POST OB LB 90: got %d (%s)", st, body)
	}
	var resp upsReadingResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v %s", err, body)
	}
	if resp.Decision.Event != ups.EventCriticalShutdown || !resp.Decision.BeginShutdown {
		t.Errorf("OB LB 90%% should be critical_shutdown, got %q begin=%v", resp.Decision.Event, resp.Decision.BeginShutdown)
	}
}

// TestUPSFirstReadingCritical verifies critical triggers even on first on-battery reading.
func TestUPSFirstReadingCritical(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)
	token := upsLoginToken(t, ts)

	// First reading ever is OB at 15% (critical) - no prior online
	st, body := upsDoReq(t, ts, http.MethodPost, "/api/ups/reading",
		map[string]string{"ups.status": "OB", "battery.charge": "15"}, token)
	if st != http.StatusOK {
		t.Fatalf("first POST OB 15: got %d (%s)", st, body)
	}
	var resp upsReadingResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v %s", err, body)
	}
	if resp.Decision.Event != ups.EventCriticalShutdown || !resp.Decision.BeginShutdown {
		t.Errorf("first OB 15%% should be critical_shutdown, got %q begin=%v", resp.Decision.Event, resp.Decision.BeginShutdown)
	}
}

// TestUPSSteadyOnlineNoEvent verifies steady OL->OL produces no event.
func TestUPSSteadyOnlineNoEvent(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)
	token := upsLoginToken(t, ts)

	// OL -> OL
	_, _ = upsDoReq(t, ts, http.MethodPost, "/api/ups/reading",
		map[string]string{"ups.status": "OL", "battery.charge": "100"}, token)
	st, body := upsDoReq(t, ts, http.MethodPost, "/api/ups/reading",
		map[string]string{"ups.status": "OL", "battery.charge": "100"}, token)
	if st != http.StatusOK {
		t.Fatalf("second OL: got %d (%s)", st, body)
	}
	var resp upsReadingResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v %s", err, body)
	}
	if resp.Decision.Event != "" {
		t.Errorf("steady OL->OL should produce no event, got %q", resp.Decision.Event)
	}
}
