// Package httpserver tests for WS47 Agents HTTP endpoints (FR-3.6).
// Pattern: New(cfg, db, "test-version"), auth setup+login admin/adminpassword123.

package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

const (
	testAgentsUser = "admin"
	testAgentsPass = "adminpassword123"
)

func newAgentsTestServer(t *testing.T, dbPath string) (*Server, *httptest.Server, *sql.DB) {
	t.Helper()
	db, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		db.Close()
		t.Fatalf("migrate: %v", err)
	}
	cfg := &config.Config{
		HTTP: config.HTTPConfig{
			Addr:         ":0",
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Database: config.DatabaseConfig{Path: dbPath},
	}
	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	s := New(cfg, db, "test-version")
	s.SetDriver(storage.NewSingleDriver(disks))
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	ts := httptest.NewServer(s.srv.Handler)
	t.Cleanup(func() {
		ts.Close()
		s.Shutdown(context.Background())
		db.Close()
	})
	return s, ts, db
}



func setupAndLogin(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	st, _ := postJSON(t, ts.URL+"/api/auth/setup",
		map[string]string{"username": testAgentsUser, "password": testAgentsPass}, "")
	if st != http.StatusCreated {
		t.Fatalf("setup: got %d, want 201", st)
	}
	st, body := postJSON(t, ts.URL+"/api/auth/login",
		map[string]string{"username": testAgentsUser, "password": testAgentsPass}, "")
	if st != http.StatusOK {
		t.Fatalf("login: got %d, want 200", st)
	}
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &lr); err != nil || lr.Token == "" {
		t.Fatalf("login response missing token: %v %s", err, body)
	}
	return lr.Token
}

// TestAgentsUnauth401: unauthenticated requests to both routes return 401.
func TestAgentsUnauth401(t *testing.T) {
	_, ts, _ := newAgentsTestServer(t, ":memory:")

	// GET /api/agents without token
	st, _ := getWithToken(t, ts.URL+"/api/agents", "")
	if st != http.StatusUnauthorized {
		t.Errorf("GET /api/agents unauth: got %d, want 401", st)
	}

	// POST /api/agents/health_guardian/run without token
	st, _ = postJSON(t, ts.URL+"/api/agents/health_guardian/run", nil, "")
	if st != http.StatusUnauthorized {
		t.Errorf("POST /api/agents/health_guardian/run unauth: got %d, want 401", st)
	}
}

// TestAgentsListReturnsFourNames: authenticated list returns the 4 agent names.
func TestAgentsListReturnsFourNames(t *testing.T) {
	_, ts, _ := newAgentsTestServer(t, ":memory:")
	token := setupAndLogin(t, ts)

	st, body := getWithToken(t, ts.URL+"/api/agents", token)
	if st != http.StatusOK {
		t.Fatalf("GET /api/agents: got %d, want 200", st)
	}
	var resp struct {
		Agents []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Agents) != 4 {
		t.Fatalf("expected 4 agents, got %d: %v", len(resp.Agents), resp.Agents)
	}
	names := make(map[string]bool)
	for _, a := range resp.Agents {
		names[a.Name] = true
	}
	expected := []string{"health_guardian", "backup_steward", "tier_curator", "capacity_planner"}
	for _, exp := range expected {
		if !names[exp] {
			t.Errorf("missing agent in list: %s", exp)
		}
	}
}

// TestAgentsRunEachReturns200WithMatchingAgentField: run each agent returns 200 with matching agent field.
func TestAgentsRunEachReturns200WithMatchingAgentField(t *testing.T) {
	_, ts, _ := newAgentsTestServer(t, ":memory:")
	token := setupAndLogin(t, ts)

	agentNames := []string{"health_guardian", "backup_steward", "tier_curator", "capacity_planner"}
	for _, name := range agentNames {
		st, body := postJSON(t, ts.URL+"/api/agents/"+name+"/run", nil, token)
		if st != http.StatusOK {
			t.Errorf("POST /api/agents/%s/run: got %d, want 200, body: %s", name, st, string(body))
			continue
		}
		var report struct {
			Agent string `json:"agent"`
		}
		if err := json.Unmarshal(body, &report); err != nil {
			t.Errorf("decode report for %s: %v", name, err)
			continue
		}
		if report.Agent != name {
			t.Errorf("agent field mismatch for %s: got %q, want %q", name, report.Agent, name)
		}
	}
}

// TestAgentsRunUnknownNameReturns404: unknown agent name returns 404.
func TestAgentsRunUnknownNameReturns404(t *testing.T) {
	_, ts, _ := newAgentsTestServer(t, ":memory:")
	token := setupAndLogin(t, ts)

	st, body := postJSON(t, ts.URL+"/api/agents/nonexistent/run", nil, token)
	if st != http.StatusNotFound {
		t.Errorf("POST /api/agents/nonexistent/run: got %d, want 404, body: %s", st, string(body))
	}
	var errResp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &errResp); err != nil || !strings.Contains(strings.ToLower(errResp.Error), "not found") {
		t.Errorf("expected 'not found' error, got: %s", string(body))
	}
}

// TestAgentsRunWrongMethodReturns405: GET on run endpoint returns 405.
func TestAgentsRunWrongMethodReturns405(t *testing.T) {
	_, ts, _ := newAgentsTestServer(t, ":memory:")
	token := setupAndLogin(t, ts)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/agents/health_guardian/run", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/agents/health_guardian/run: got %d, want 405", resp.StatusCode)
	}
}

// TestAgentsRunWritesAuditRow: run writes an audit row with action agent.run.<name>.
func TestAgentsRunWritesAuditRow(t *testing.T) {
	s, ts, db := newAgentsTestServer(t, ":memory:")
	token := setupAndLogin(t, ts)

	agentNames := []string{"health_guardian", "backup_steward", "tier_curator", "capacity_planner"}
	for _, name := range agentNames {
		st, _ := postJSON(t, ts.URL+"/api/agents/"+name+"/run", nil, token)
		if st != http.StatusOK {
			t.Errorf("POST /api/agents/%s/run: got %d, want 200", name, st)
			continue
		}

		// Read audit log directly from test DB
		var count int
		err := db.QueryRow(`
			SELECT COUNT(*) FROM audit_log
			WHERE action = ? AND target_type = 'agent' AND target_id = ? AND result = 'ok'
		`, "agent.run."+name, name).Scan(&count)
		if err != nil {
			t.Fatalf("query audit_log for %s: %v", name, err)
		}
		if count != 1 {
			t.Errorf("expected exactly 1 audit row for agent.run.%s, got %d", name, count)
		}

		// Also verify detail contains findings count
		var detail string
		err = db.QueryRow(`
			SELECT detail FROM audit_log
			WHERE action = ? AND target_type = 'agent' AND target_id = ? AND result = 'ok'
		`, "agent.run."+name, name).Scan(&detail)
		if err != nil {
			t.Fatalf("query audit_log detail for %s: %v", name, err)
		}
		if !strings.Contains(detail, "findings=") {
			t.Errorf("audit detail for %s missing findings count: %s", name, detail)
		}
	}

	// Call list endpoint to generate agent.list audit row
	_, _ = getWithToken(t, ts.URL+"/api/agents", token)

	// Verify error audit row for unknown agent
	st, _ := postJSON(t, ts.URL+"/api/agents/nonexistent/run", nil, token)
	if st != http.StatusNotFound {
		t.Errorf("POST /api/agents/nonexistent/run: got %d, want 404", st)
	}
	var errCount int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM audit_log
		WHERE action = 'agent.run.nonexistent' AND result = 'error'
	`).Scan(&errCount)
	if err != nil {
		t.Fatalf("query audit_log for unknown agent: %v", err)
	}
	if errCount != 1 {
		t.Errorf("expected 1 error audit row for unknown agent, got %d", errCount)
	}

	// Verify list action audit row
	var listCount int
	err = db.QueryRow(`
		SELECT COUNT(*) FROM audit_log
		WHERE action = 'agent.list' AND result = 'ok'
	`).Scan(&listCount)
	if err != nil {
		t.Fatalf("query audit_log for agent.list: %v", err)
	}
	if listCount < 1 {
		t.Errorf("expected at least 1 agent.list audit row, got %d", listCount)
	}

	_ = s // suppress unused warning
}
