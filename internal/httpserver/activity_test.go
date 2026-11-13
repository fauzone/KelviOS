package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/activity"
	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/compose"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/power"
	"github.com/fauzone/kelvios/internal/storage"

	_ "modernc.org/sqlite"
)

type activityFixture struct {
	s      *Server
	ts     *httptest.Server
	db     *sql.DB
	token  string
	agg    *activity.Aggregator
}

func newActivityFixture(t *testing.T) *activityFixture {
	t.Helper()
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		db.Close()
		t.Fatalf("migrate: %v", err)
	}

	// Seed compose artifacts for testing
	now := time.Now().Unix()
	spec := []byte(`{"interval_hours": 24}`)
	_, err = db.Exec(`
		INSERT INTO compose_artifacts (id, name, kind, status, spec, current_version, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "test-artifact-1", "Daily Report", compose.KindReportSchedule, compose.StatusActive, spec, 1, "testuser", now, now)
	if err != nil {
		t.Fatalf("insert artifact: %v", err)
	}

	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{Path: ":memory:"},
	}
	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	s := New(cfg, db, "test-version")
	s.SetDriver(storage.NewSingleDriver(disks))
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }

	// Create power engine with schedules for power source
	powerEngine := power.New(nil, nil, 0)
	powerEngine.RegisterPool("pool1", storage.TierWarm, "Test Pool")
	powerEngine.RegisterDisk("disk1", "pool1")
	powerEngine.SetSchedules("disk1", []power.Schedule{
		{Days: "Mon,Tue,Wed,Thu,Fri", StartMin: 540, EndMin: 1020, Enabled: true},
	})

	// Create activity aggregator
	agg := activity.NewAggregator(db, s.composeExecutor, powerEngine, s.disksFunc, time.Now, nil)
	s.SetActivityAggregator(agg)

	ts := httptest.NewServer(s.srv.Handler)
	f := &activityFixture{s: s, ts: ts, db: db, agg: agg}
	f.token = mustToken(t, ts)
	return f
}

func (f *activityFixture) close() {
	f.ts.Close()
	_ = f.s.Shutdown(context.Background())
	_ = f.db.Close()
}

func (f *activityFixture) getJSON(t *testing.T, path string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.ts.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

func (f *activityFixture) postJSON(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, f.ts.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody
}

// TestActivityUnknownCompose tests GET /api/activity/compose:unknown returns 404 item_not_found
func TestActivityUnknownCompose(t *testing.T) {
	f := newActivityFixture(t)
	defer f.close()

	status, body := f.getJSON(t, "/api/activity/compose:nonexistent")
	if status != http.StatusNotFound {
		t.Errorf("GET unknown compose: status %d, want %d", status, http.StatusNotFound)
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["error"] != "item_not_found" {
		t.Errorf("error key: %q, want 'item_not_found'", resp["error"])
	}
}

// TestActivityUnknownAgent tests GET /api/activity/agent:unknown returns 404 item_not_found
func TestActivityUnknownAgent(t *testing.T) {
	f := newActivityFixture(t)
	defer f.close()

	status, body := f.getJSON(t, "/api/activity/agent:nonexistent")
	if status != http.StatusNotFound {
		t.Errorf("GET unknown agent: status %d, want %d", status, http.StatusNotFound)
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["error"] != "item_not_found" {
		t.Errorf("error key: %q, want 'item_not_found'", resp["error"])
	}
}

// TestActivityActionUnknownID tests POST /api/activity/unknown/action returns 404 item_not_found
func TestActivityActionUnknownID(t *testing.T) {
	f := newActivityFixture(t)
	defer f.close()

	status, body := f.postJSON(t, "/api/activity/compose:nonexistent/pause", nil)
	if status != http.StatusNotFound {
		t.Errorf("POST action on unknown id: status %d, want %d", status, http.StatusNotFound)
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["error"] != "item_not_found" {
		t.Errorf("error key: %q, want 'item_not_found'", resp["error"])
	}
}

// TestActivityPauseAgentStill409 tests POST /api/activity/agent:tier_curator/pause returns 409 control_not_supported
func TestActivityPauseAgentStill409(t *testing.T) {
	f := newActivityFixture(t)
	defer f.close()

	status, body := f.postJSON(t, "/api/activity/agent:tier_curator/pause", nil)
	if status != http.StatusConflict {
		t.Errorf("POST pause on agent: status %d, want %d", status, http.StatusConflict)
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["error"] != "control_not_supported" {
		t.Errorf("error key: %q, want 'control_not_supported'", resp["error"])
	}
}

// TestActivityListStill200 tests GET /api/activity returns 200 with items
func TestActivityListStill200(t *testing.T) {
	f := newActivityFixture(t)
	defer f.close()

	status, body := f.getJSON(t, "/api/activity")
	if status != http.StatusOK {
		t.Errorf("GET list: status %d, want %d", status, http.StatusOK)
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	items, ok := resp["items"].([]any)
	if !ok {
		t.Fatal("items field missing or not array")
	}
	if len(items) == 0 {
		t.Error("expected at least one item in list")
	}
}