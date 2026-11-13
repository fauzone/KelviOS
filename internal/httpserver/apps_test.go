package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/appdriver"
	"github.com/fauzone/kelvios/internal/appupdate"
	"github.com/fauzone/kelvios/internal/apps"
	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

const (
	appsTestUser = "admin"
	appsTestPass = "adminpassword123"
)

// newAppsTestServer creates a test server with admin user setup and returns
// the httptest.Server, a valid bearer token, and the database.
func newAppsTestServer(t *testing.T, dbPath string) (*httptest.Server, string, *sql.DB) {
	t.Helper()
	db, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		db.Close()
		t.Fatalf("migrate: %v", err)
	}
	// Force single connection for SQLite :memory: to share state across requests
	db.SetMaxOpenConns(1)

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

	// Set up FakeDriver for tests (no RAM limit)
	fakeDriver := appdriver.NewFakeDriver(0)
	s.SetAppDriver(fakeDriver)

	ts := httptest.NewServer(s.srv.Handler)
	t.Cleanup(func() {
		ts.Close()
		s.Shutdown(context.Background())
		db.Close()
	})

	// Setup admin user
	st, _ := postJSON(t, ts.URL+"/api/auth/setup", map[string]string{"username": appsTestUser, "password": appsTestPass}, "")
	if st != http.StatusCreated {
		t.Fatalf("setup: got %d, want 201", st)
	}
	// Login to get token
	st, body := postJSON(t, ts.URL+"/api/auth/login", map[string]string{"username": appsTestUser, "password": appsTestPass}, "")
	if st != http.StatusOK {
		t.Fatalf("login: got %d, want 200", st)
	}
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &lr); err != nil || lr.Token == "" {
		t.Fatalf("login response missing token: %v %s", err, body)
	}
	return ts, lr.Token, db
}

// appsDoReq makes an HTTP request with the given method, path, payload, and token.
func appsDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) (int, []byte) {
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
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

// TestAppsAuthGate tests that /api/apps requires a valid Bearer token.
func TestAppsAuthGate(t *testing.T) {
	ts, token, _ := newAppsTestServer(t, ":memory:")

	// GET without token → 401
	st, _ := appsDoReq(t, ts, http.MethodGet, "/api/apps", nil, "")
	if st != http.StatusUnauthorized {
		t.Fatalf("GET no token: got %d, want 401", st)
	}

	// POST action without token → 401
	st, _ = appsDoReq(t, ts, http.MethodPost, "/api/apps/adguard-home/install", nil, "")
	if st != http.StatusUnauthorized {
		t.Fatalf("POST action no token: got %d, want 401", st)
	}

	// GET with valid token → 200
	st, body := appsDoReq(t, ts, http.MethodGet, "/api/apps", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET with token: got %d, want 200; body: %s", st, body)
	}
	var resp struct {
		Apps []struct {
			ID             string   `json:"id"`
			Name           string   `json:"name"`
			Category       string   `json:"category"`
			Description    string   `json:"description"`
			Wave           int      `json:"wave"`
			Optional       bool     `json:"optional"`
			Group          string   `json:"group"`
			Image          string   `json:"image"`
			RAMMB          int      `json:"ram_mb"`
			Ports          []int    `json:"ports"`
			Notes          string   `json:"notes"`
			Status         string   `json:"status"`
			PortCollisions []int    `json:"port_collisions"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if len(resp.Apps) != 26 {
		t.Errorf("apps list length = %d, want 26", len(resp.Apps))
	}
	// All should be available by default
	for _, a := range resp.Apps {
		if a.Status != "available" {
			t.Errorf("app %s status = %q, want available", a.ID, a.Status)
		}
		if a.PortCollisions == nil {
			t.Errorf("app %s port_collisions = nil, want []", a.ID)
		}
	}
}

// TestAppsFilterWave tests filtering by wave.
func TestAppsFilterWave(t *testing.T) {
	ts, token, _ := newAppsTestServer(t, ":memory:")

	// Wave 1
	st, body := appsDoReq(t, ts, http.MethodGet, "/api/apps?wave=1", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET wave=1: got %d, want 200; body: %s", st, body)
	}
	var resp struct {
		Apps []struct {
			ID   string `json:"id"`
			Wave int    `json:"wave"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Apps) != 9 {
		t.Errorf("wave 1 count = %d, want 9", len(resp.Apps))
	}
	for _, a := range resp.Apps {
		if a.Wave != 1 {
			t.Errorf("app %s wave = %d, want 1", a.ID, a.Wave)
		}
	}

	// Wave 2
	st, body = appsDoReq(t, ts, http.MethodGet, "/api/apps?wave=2", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET wave=2: got %d, want 200; body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Apps) != 17 {
		t.Errorf("wave 2 count = %d, want 17", len(resp.Apps))
	}
	for _, a := range resp.Apps {
		if a.Wave != 2 {
			t.Errorf("app %s wave = %d, want 2", a.ID, a.Wave)
		}
	}
}

// TestAppsFilterCategory tests filtering by category.
func TestAppsFilterCategory(t *testing.T) {
	ts, token, _ := newAppsTestServer(t, ":memory:")

	// Category "iot"
	st, body := appsDoReq(t, ts, http.MethodGet, "/api/apps?category=iot", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET category=iot: got %d, want 200; body: %s", st, body)
	}
	var resp struct {
		Apps []struct {
			ID       string `json:"id"`
			Category string `json:"category"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Count iot apps in catalog
	iotCount := 0
	for _, a := range apps.Catalog() {
		if a.Category == "iot" {
			iotCount++
		}
	}
	if len(resp.Apps) != iotCount {
		t.Errorf("category iot count = %d, want %d", len(resp.Apps), iotCount)
	}
	for _, a := range resp.Apps {
		if a.Category != "iot" {
			t.Errorf("app %s category = %q, want iot", a.ID, a.Category)
		}
	}
}

// TestAppsFilterWaveAndCategory tests combined filter.
func TestAppsFilterWaveAndCategory(t *testing.T) {
	ts, token, _ := newAppsTestServer(t, ":memory:")

	// Wave 1 + category iot
	st, body := appsDoReq(t, ts, http.MethodGet, "/api/apps?wave=1&category=iot", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET wave=1&category=iot: got %d, want 200; body: %s", st, body)
	}
	var resp struct {
		Apps []struct {
			ID       string `json:"id"`
			Wave     int    `json:"wave"`
			Category string `json:"category"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Count wave 1 iot apps in catalog
	expected := 0
	for _, a := range apps.Catalog() {
		if a.Wave == 1 && a.Category == "iot" {
			expected++
		}
	}
	if len(resp.Apps) != expected {
		t.Errorf("wave 1 + iot count = %d, want %d", len(resp.Apps), expected)
	}
	for _, a := range resp.Apps {
		if a.Wave != 1 || a.Category != "iot" {
			t.Errorf("app %s wave=%d category=%q, want wave=1 category=iot", a.ID, a.Wave, a.Category)
		}
	}
}

// TestAppsGetNotFound tests GET /api/apps/{id} with unknown id.
func TestAppsGetNotFound(t *testing.T) {
	ts, token, _ := newAppsTestServer(t, ":memory:")

	st, body := appsDoReq(t, ts, http.MethodGet, "/api/apps/nonexistent", nil, token)
	if st != http.StatusNotFound {
		t.Fatalf("GET unknown: got %d, want 404; body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "app_not_found" {
		t.Errorf("error = %q, want app_not_found", errResp["error"])
	}
}

// TestAppsLifecycle tests install→activate→deactivate→uninstall on a real catalog app.
func TestAppsLifecycle(t *testing.T) {
	ts, token, _ := newAppsTestServer(t, ":memory:")

	// Pick a real catalog app
	appID := "adguard-home"

	// 1. Initial state: available
	st, body := appsDoReq(t, ts, http.MethodGet, "/api/apps/"+appID, nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET initial: got %d, want 200; body: %s", st, body)
	}
	var resp struct {
		App struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"app"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.App.Status != "available" {
		t.Errorf("initial status = %q, want available", resp.App.Status)
	}

	// 2. Install → installed
	st, body = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("POST install: got %d, want 200; body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode install: %v", err)
	}
	if resp.App.Status != "installed" {
		t.Errorf("after install status = %q, want installed", resp.App.Status)
	}

	// 3. Activate → active
	st, body = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/activate", nil, token)
	if st != http.StatusOK {
		t.Fatalf("POST activate: got %d, want 200; body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode activate: %v", err)
	}
	if resp.App.Status != "active" {
		t.Errorf("after activate status = %q, want active", resp.App.Status)
	}

	// 4. Deactivate → installed
	st, body = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/deactivate", nil, token)
	if st != http.StatusOK {
		t.Fatalf("POST deactivate: got %d, want 200; body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode deactivate: %v", err)
	}
	if resp.App.Status != "installed" {
		t.Errorf("after deactivate status = %q, want installed", resp.App.Status)
	}

	// 5. Uninstall → available
	st, body = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/uninstall", nil, token)
	if st != http.StatusOK {
		t.Fatalf("POST uninstall: got %d, want 200; body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode uninstall: %v", err)
	}
	if resp.App.Status != "available" {
		t.Errorf("after uninstall status = %q, want available", resp.App.Status)
	}
}

// TestAppsInvalidTransitions tests illegal transitions return 409.
func TestAppsInvalidTransitions(t *testing.T) {
	ts, token, _ := newAppsTestServer(t, ":memory:")

	appID := "adguard-home"

	// 1. Activate from available (illegal: available→active not allowed)
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/activate", nil, token)
	if st != http.StatusConflict {
		t.Fatalf("activate from available: got %d, want 409; body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "invalid_transition" {
		t.Errorf("error = %q, want invalid_transition", errResp["error"])
	}

	// Verify status unchanged (still available)
	st, body = appsDoReq(t, ts, http.MethodGet, "/api/apps/"+appID, nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET after illegal: got %d, want 200; body: %s", st, body)
	}
	var resp struct {
		App struct {
			Status string `json:"status"`
		} `json:"app"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.App.Status != "available" {
		t.Errorf("status after illegal activate = %q, want available (unchanged)", resp.App.Status)
	}

	// 2. Install first, then uninstall from active (illegal: active→available not allowed)
	// Install
	st, _ = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}
	// Activate
	st, _ = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/activate", nil, token)
	if st != http.StatusOK {
		t.Fatalf("activate: got %d, want 200", st)
	}
	// Now try uninstall from active (illegal)
	st, body = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/uninstall", nil, token)
	if st != http.StatusConflict {
		t.Fatalf("uninstall from active: got %d, want 409; body: %s", st, body)
	}
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "invalid_transition" {
		t.Errorf("error = %q, want invalid_transition", errResp["error"])
	}

	// Verify status unchanged (still active)
	st, body = appsDoReq(t, ts, http.MethodGet, "/api/apps/"+appID, nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET after illegal uninstall: got %d, want 200; body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.App.Status != "active" {
		t.Errorf("status after illegal uninstall = %q, want active (unchanged)", resp.App.Status)
	}
}

// TestAppsActionNotFound tests action on unknown app ID.
func TestAppsActionNotFound(t *testing.T) {
	ts, token, _ := newAppsTestServer(t, ":memory:")

	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/nonexistent/install", nil, token)
	if st != http.StatusNotFound {
		t.Fatalf("POST unknown: got %d, want 404; body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "app_not_found" {
		t.Errorf("error = %q, want app_not_found", errResp["error"])
	}
}

// TestAppsPortCollisions tests port_collisions reporting.
func TestAppsPortCollisions(t *testing.T) {
	ts, token, db := newAppsTestServer(t, ":memory:")

	// Find two apps that share a port
	collisions := apps.PortCollisions()
	var sharedPort int
	var appA, appB string
	for port, ids := range collisions {
		if len(ids) >= 2 {
			sharedPort = port
			appA = ids[0]
			appB = ids[1]
			break
		}
	}
	if sharedPort == 0 {
		t.Fatal("no port collisions found in catalog for test")
	}

	// Seed both apps as installed directly in the status store.
	// This test covers port_collisions *reporting* (WS53-B), not driver
	// execution: with a driver attached, installing the second colliding
	// app is correctly rejected with 409 port_conflict (covered by
	// TestAppsFakeDriverPortConflict), so REST install can no longer
	// set up this scenario. (Chief micro-fix, WS54-FIX continuation,
	// 8 Nov 2026 — replacing Saka's removal of FakeDriver port checks.)
	for _, id := range []string{appA, appB} {
		if err := apps.SetStatus(db, id, apps.StatusInstalled); err != nil {
			t.Fatalf("seed status %s: %v", id, err)
		}
	}

	// Check both report the collision
	for _, id := range []string{appA, appB} {
		st, body := appsDoReq(t, ts, http.MethodGet, "/api/apps/"+id, nil, token)
		if st != http.StatusOK {
			t.Fatalf("GET %s: got %d, want 200; body: %s", id, st, body)
		}
		var resp struct {
			App struct {
				PortCollisions []int `json:"port_collisions"`
			} `json:"app"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode %s: %v", id, err)
		}
		found := false
		for _, p := range resp.App.PortCollisions {
			if p == sharedPort {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("app %s port_collisions = %v, want to contain %d", id, resp.App.PortCollisions, sharedPort)
		}
	}

	// Check a third app that doesn't share the port reports empty
	// Find an app not in the collision
	var appC string
	for _, a := range apps.Catalog() {
		if a.ID != appA && a.ID != appB {
			appC = a.ID
			break
		}
	}
	if appC == "" {
		t.Fatal("no third app found")
	}
	st, body := appsDoReq(t, ts, http.MethodGet, "/api/apps/"+appC, nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200; body: %s", appC, st, body)
	}
	var resp struct {
		App struct {
			PortCollisions []int `json:"port_collisions"`
		} `json:"app"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode %s: %v", appC, err)
	}
	if len(resp.App.PortCollisions) != 0 {
		t.Errorf("app %s port_collisions = %v, want []", appC, resp.App.PortCollisions)
	}
}

// TestAppsMethodNotAllowed tests wrong methods return 405.
func TestAppsMethodNotAllowed(t *testing.T) {
	ts, token, _ := newAppsTestServer(t, ":memory:")

	// PUT on /api/apps
	st, _ := appsDoReq(t, ts, http.MethodPut, "/api/apps", nil, token)
	if st != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/apps: got %d, want 405", st)
	}

	// DELETE on /api/apps
	st, _ = appsDoReq(t, ts, http.MethodDelete, "/api/apps", nil, token)
	if st != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /api/apps: got %d, want 405", st)
	}

	// GET on action route
	st, _ = appsDoReq(t, ts, http.MethodGet, "/api/apps/adguard-home/install", nil, token)
	if st != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/apps/.../install: got %d, want 405", st)
	}

	// PUT on action route
	st, _ = appsDoReq(t, ts, http.MethodPut, "/api/apps/adguard-home/install", nil, token)
	if st != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/apps/.../install: got %d, want 405", st)
	}
}

// TestAppsInvalidWave tests invalid wave parameter.
func TestAppsInvalidWave(t *testing.T) {
	ts, token, _ := newAppsTestServer(t, ":memory:")

	st, _ := appsDoReq(t, ts, http.MethodGet, "/api/apps?wave=3", nil, token)
	if st != http.StatusBadRequest {
		t.Errorf("wave=3: got %d, want 400", st)
	}

	st, _ = appsDoReq(t, ts, http.MethodGet, "/api/apps?wave=abc", nil, token)
	if st != http.StatusBadRequest {
		t.Errorf("wave=abc: got %d, want 400", st)
	}
}

// TestAppsActionUnknown tests unknown action returns 404.
func TestAppsActionUnknown(t *testing.T) {
	ts, token, _ := newAppsTestServer(t, ":memory:")

	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/adguard-home/unknown", nil, token)
	if st != http.StatusNotFound {
		t.Fatalf("unknown action: got %d, want 404; body: %s", st, body)
	}
}

// newAppsTestServerWithFakeDriver creates a test server with FakeDriver.
func newAppsTestServerWithFakeDriver(t *testing.T, dbPath string, ramLimitMB int64) (*httptest.Server, string, *sql.DB, *appdriver.FakeDriver) {
	t.Helper()
	db, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		db.Close()
		t.Fatalf("migrate: %v", err)
	}
	db.SetMaxOpenConns(1)

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

	// Set up FakeDriver
	fakeDriver := appdriver.NewFakeDriver(ramLimitMB)
	s.SetAppDriver(fakeDriver)

	// Set up update source (static feed v1)
	s.SetUpdateSource(appupdate.NewStaticFeed())

	ts := httptest.NewServer(s.srv.Handler)
	t.Cleanup(func() {
		ts.Close()
		s.Shutdown(context.Background())
		db.Close()
	})

	// Setup admin user
	st, _ := postJSON(t, ts.URL+"/api/auth/setup", map[string]string{"username": appsTestUser, "password": appsTestPass}, "")
	if st != http.StatusCreated {
		t.Fatalf("setup: got %d, want 201", st)
	}
	// Login to get token
	st, body := postJSON(t, ts.URL+"/api/auth/login", map[string]string{"username": appsTestUser, "password": appsTestPass}, "")
	if st != http.StatusOK {
		t.Fatalf("login: got %d, want 200", st)
	}
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &lr); err != nil || lr.Token == "" {
		t.Fatalf("login response missing token: %v %s", err, body)
	}
	return ts, lr.Token, db, fakeDriver
}

// TestAppsFakeDriverLifecycle tests install→activate→deactivate→uninstall with FakeDriver.
func TestAppsFakeDriverLifecycle(t *testing.T) {
	ts, token, _, fakeDriver := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	// Pick a real catalog app
	appID := "adguard-home"

	// 1. Initial state: available
	st, body := appsDoReq(t, ts, http.MethodGet, "/api/apps/"+appID, nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET initial: got %d, want 200; body: %s", st, body)
	}
	var resp struct {
		App struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"app"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.App.Status != "available" {
		t.Errorf("initial status = %q, want available", resp.App.Status)
	}

	// 2. Install → installed
	st, body = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("POST install: got %d, want 200; body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode install: %v", err)
	}
	if resp.App.Status != "installed" {
		t.Errorf("after install status = %q, want installed", resp.App.Status)
	}
	// Verify FakeDriver has container
	if _, ok := fakeDriver.GetContainer(appID); !ok {
		t.Errorf("FakeDriver: container not found after install")
	}

	// 3. Activate → active (health gate will pass since FakeDriver returns healthy=true)
	st, body = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/activate", nil, token)
	if st != http.StatusOK {
		t.Fatalf("POST activate: got %d, want 200; body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode activate: %v", err)
	}
	if resp.App.Status != "active" {
		t.Errorf("after activate status = %q, want active", resp.App.Status)
	}

	// 4. Deactivate → installed
	st, body = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/deactivate", nil, token)
	if st != http.StatusOK {
		t.Fatalf("POST deactivate: got %d, want 200; body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode deactivate: %v", err)
	}
	if resp.App.Status != "installed" {
		t.Errorf("after deactivate status = %q, want installed", resp.App.Status)
	}
	// Verify RAM released on stop (T6)
	if fakeDriver.GetRAMUsed() != 0 {
		t.Errorf("FakeDriver: ramUsed = %d after stop, want 0", fakeDriver.GetRAMUsed())
	}

	// 5. Uninstall → available
	st, body = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/uninstall", nil, token)
	if st != http.StatusOK {
		t.Fatalf("POST uninstall: got %d, want 200; body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode uninstall: %v", err)
	}
	if resp.App.Status != "available" {
		t.Errorf("after uninstall status = %q, want available", resp.App.Status)
	}
	// Verify container removed
	if _, ok := fakeDriver.GetContainer(appID); ok {
		t.Errorf("FakeDriver: container still exists after uninstall")
	}
}

// TestAppsFakeDriverHealthGateFailure tests health gate failure on activate.
func TestAppsFakeDriverHealthGateFailure(t *testing.T) {
	ts, token, _, fakeDriver := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"

	// Install first
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}

	// Make container unhealthy
	fakeDriver.SetHealthy(appID, false)

	// Activate should fail health gate and return 502
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/activate", nil, token)
	if st != http.StatusBadGateway {
		t.Fatalf("POST activate with unhealthy: got %d, want 502; body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "health_check_failed" {
		t.Errorf("error = %q, want health_check_failed", errResp["error"])
	}

	// Status should remain installed (not active)
	st, body = appsDoReq(t, ts, http.MethodGet, "/api/apps/"+appID, nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET after failed activate: got %d, want 200; body: %s", st, body)
	}
	var resp struct {
		App struct {
			Status string `json:"status"`
		} `json:"app"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.App.Status != "installed" {
		t.Errorf("status after failed activate = %q, want installed", resp.App.Status)
	}
}

// TestAppsFakeDriverRAMExhausted tests RAM limit enforcement.
func TestAppsFakeDriverRAMExhausted(t *testing.T) {
	// RAM limit: 100 MB
	ts, token, _, _ := newAppsTestServerWithFakeDriver(t, ":memory:", 100)

	// Pick an app with RAMMB > 100 (e.g., adguard-home has 50 MB, need larger)
	// Find an app with RAMMB > 100
	var largeApp string
	for _, a := range apps.Catalog() {
		if a.RAMMB > 100 {
			largeApp = a.ID
			break
		}
	}
	if largeApp == "" {
		t.Fatal("no app with RAMMB > 100 found in catalog")
	}

	// Install should fail with resource exhausted
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+largeApp+"/install", nil, token)
	if st != http.StatusInsufficientStorage {
		t.Fatalf("POST install with RAM limit: got %d, want 507; body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "resource_exhausted" {
		t.Errorf("error = %q, want resource_exhausted", errResp["error"])
	}
}

// TestAppsFakeDriverPortConflict tests port conflict detection.
func TestAppsFakeDriverPortConflict(t *testing.T) {
	ts, token, _, _ := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	// Find two apps that share a port
	collisions := apps.PortCollisions()
	var sharedPort int
	var appA, appB string
	for port, ids := range collisions {
		if len(ids) >= 2 {
			sharedPort = port
			appA = ids[0]
			appB = ids[1]
			break
		}
	}
	if sharedPort == 0 {
		t.Fatal("no port collisions found in catalog for test")
	}

	// Install first app
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appA+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install %s: got %d, want 200", appA, st)
	}

	// Install second app with same port should fail
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appB+"/install", nil, token)
	if st != http.StatusConflict {
		t.Fatalf("install %s (port conflict): got %d, want 409; body: %s", appB, st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "port_conflict" {
		t.Errorf("error = %q, want port_conflict", errResp["error"])
	}
}

// TestAppsFakeDriverKill tests Kill method releases resources.
func TestAppsFakeDriverKill(t *testing.T) {
	ts, token, _, fakeDriver := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"

	// Install
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}

	// Activate
	st, _ = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/activate", nil, token)
	if st != http.StatusOK {
		t.Fatalf("activate: got %d, want 200", st)
	}

	// Kill via driver directly
	err := fakeDriver.Kill(appID)
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// Verify resources released
	if fakeDriver.GetRAMUsed() != 0 {
		t.Errorf("ramUsed = %d after Kill, want 0", fakeDriver.GetRAMUsed())
	}
	if _, ok := fakeDriver.GetContainer(appID); ok {
		t.Errorf("container still exists after Kill")
	}
	// Verify ports released
	for _, p := range apps.Catalog() {
		if p.ID == appID {
			for _, port := range p.Ports {
				if owner, ok := fakeDriver.GetPortOwner(port); ok && owner == appID {
					t.Errorf("port %d still owned by %s after Kill", port, owner)
				}
			}
		}
	}
}
// newAppsTestServerNoDriver creates a test server WITHOUT an app driver
// (appDriver == nil), mirroring the production binary when the Docker
// socket is unavailable. (Chief, WS54-FIX continuation, 8 Nov 2026.)
func newAppsTestServerNoDriver(t *testing.T, dbPath string) (*httptest.Server, string, *sql.DB) {
	t.Helper()
	db, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		db.Close()
		t.Fatalf("migrate: %v", err)
	}
	db.SetMaxOpenConns(1)

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
	// Deliberately no SetAppDriver: appDriver stays nil.

	ts := httptest.NewServer(s.srv.Handler)
	t.Cleanup(func() {
		ts.Close()
		s.Shutdown(context.Background())
		db.Close()
	})

	st, _ := postJSON(t, ts.URL+"/api/auth/setup", map[string]string{"username": appsTestUser, "password": appsTestPass}, "")
	if st != http.StatusCreated {
		t.Fatalf("setup: got %d, want 201", st)
	}
	st, body := postJSON(t, ts.URL+"/api/auth/login", map[string]string{"username": appsTestUser, "password": appsTestPass}, "")
	if st != http.StatusOK {
		t.Fatalf("login: got %d, want 200", st)
	}
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &lr); err != nil || lr.Token == "" {
		t.Fatalf("login response missing token: %v %s", err, body)
	}
	return ts, lr.Token, db
}

// TestAppsNoDriverUnavailable tests that app actions without a driver
// return 503 driver_unavailable instead of pretending success
// (WS54-FIX continuation: nil-driver contract from server.go seat delta).
func TestAppsNoDriverUnavailable(t *testing.T) {
	ts, token, _ := newAppsTestServerNoDriver(t, ":memory:")

	appID := "adguard-home"
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusServiceUnavailable {
		t.Fatalf("POST install without driver: got %d, want 503; body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "driver_unavailable" {
		t.Errorf("error = %q, want driver_unavailable", errResp["error"])
	}

	// Status must remain available (no fake success)
	st, body = appsDoReq(t, ts, http.MethodGet, "/api/apps/"+appID, nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET after failed install: got %d, want 200; body: %s", st, body)
	}
	var resp struct {
		App struct {
			Status string `json:"status"`
		} `json:"app"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.App.Status != "available" {
		t.Errorf("status after failed install = %q, want available", resp.App.Status)
	}
}

// TestAppsUpdateSuccess tests successful update with health gate pass.
func TestAppsUpdateSuccess(t *testing.T) {
	ts, token, db, fakeDriver := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"

	// Install first
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}

	// Activate to make it active (update allowed from active)
	st, _ = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/activate", nil, token)
	if st != http.StatusOK {
		t.Fatalf("activate: got %d, want 200", st)
	}

	// Update - should succeed (static feed has adguard-home update)
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/update", nil, token)
	if st != http.StatusOK {
		t.Fatalf("POST update: got %d, want 200; body: %s", st, body)
	}

	var resp struct {
		App struct {
			ID             string `json:"id"`
			Status         string `json:"status"`
			InstalledImage string `json:"installed_image"`
			UpdateAvailable bool   `json:"update_available"`
			Update         struct {
				Version       string `json:"version"`
				ImageRef      string `json:"image_ref"`
				ReleaseNotes  string `json:"release_notes"`
				PermissionChanges struct {
					PortsAdded          []int `json:"ports_added"`
					PortsRemoved        []int `json:"ports_removed"`
					RAMMBFrom           int   `json:"rammb_from"`
					RAMMBTo             int   `json:"rammb_to"`
					ImageSourceChanged  bool  `json:"image_source_changed"`
					DigestChanged       bool  `json:"digest_changed"`
				} `json:"permission_changes"`
			} `json:"update"`
		} `json:"app"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.App.Status != "active" {
		t.Errorf("status after update = %q, want active", resp.App.Status)
	}
	if resp.App.InstalledImage != "adguard/adguardhome:v0.108.0" {
		t.Errorf("installed_image = %q, want adguard/adguardhome:v0.108.0", resp.App.InstalledImage)
	}
	if resp.App.UpdateAvailable {
		t.Errorf("update_available = %v, want false after successful update", resp.App.UpdateAvailable)
	}
	// After update, no update should be available (same version)
	if resp.App.Update.Version != "" {
		t.Errorf("update.version should be empty after update, got %q", resp.App.Update.Version)
	}

	// Debug: query database directly to verify installed info
	var dbImageRef, dbVersionLabel, dbDigest, dbPrevImageRef string
	err := db.QueryRow(`
		SELECT image_ref, version_label, digest, previous_image_ref
		FROM app_installs WHERE app_id = ?
	`, appID).Scan(&dbImageRef, &dbVersionLabel, &dbDigest, &dbPrevImageRef)
	if err != nil {
		t.Fatalf("query app_installs: %v", err)
	}
	t.Logf("DB after update: image_ref=%q, version_label=%q, digest=%q, previous_image_ref=%q", dbImageRef, dbVersionLabel, dbDigest, dbPrevImageRef)
	if dbImageRef != "adguard/adguardhome:v0.108.0" {
		t.Errorf("DB image_ref = %q, want adguard/adguardhome:v0.108.0", dbImageRef)
	}
	if dbVersionLabel != "0.108.0" {
		t.Errorf("DB version_label = %q, want 0.108.0", dbVersionLabel)
	}
	if dbDigest != "sha256:a1b2c3d4e5f6789012345678901234567890abcdef1234567890abcdef123456" {
		t.Errorf("DB digest = %q, want feed digest", dbDigest)
	}
	if dbPrevImageRef != "adguard/adguardhome:latest" {
		t.Errorf("DB previous_image_ref = %q, want adguard/adguardhome:latest", dbPrevImageRef)
	}

	// Verify driver has new image
	img, ok := fakeDriver.GetContainerImage(appID)
	if !ok {
		t.Fatal("container not found after update")
	}
	if img != "adguard/adguardhome:v0.108.0" {
		t.Errorf("driver image = %q, want v0.108.0", img)
	}
	prevImg, ok := fakeDriver.GetContainerPreviousImage(appID)
	if !ok {
		t.Fatal("container not found after update")
	}
	// Catalog image for adguard-home is "adguard/adguardhome:latest" (not v0.107.0)
	if prevImg != "adguard/adguardhome:latest" {
		t.Errorf("driver previous_image = %q, want catalog image (latest)", prevImg)
	}
}

// TestAppsUpdateFromInstalled tests update from installed (not active) status.
func TestAppsUpdateFromInstalled(t *testing.T) {
	ts, token, _, fakeDriver := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"

	// Install only (not activate)
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}

	// Update from installed status
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/update", nil, token)
	if st != http.StatusOK {
		t.Fatalf("POST update from installed: got %d, want 200; body: %s", st, body)
	}

	var resp struct {
		App struct {
			Status         string `json:"status"`
			InstalledImage string `json:"installed_image"`
		} `json:"app"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.App.Status != "installed" {
		t.Errorf("status after update = %q, want installed", resp.App.Status)
	}
	if resp.App.InstalledImage != "adguard/adguardhome:v0.108.0" {
		t.Errorf("installed_image = %q, want adguard/adguardhome:v0.108.0", resp.App.InstalledImage)
	}

	// Verify driver has new image
	img, ok := fakeDriver.GetContainerImage(appID)
	if !ok {
		t.Fatal("container not found after update")
	}
	if img != "adguard/adguardhome:v0.108.0" {
		t.Errorf("driver image = %q, want v0.108.0", img)
	}
}

// TestAppsUpdateNotInstalled tests update on available app returns 409.
func TestAppsUpdateNotInstalled(t *testing.T) {
	ts, token, _, _ := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"

	// App is available (not installed)
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/update", nil, token)
	if st != http.StatusConflict {
		t.Fatalf("POST update on available: got %d, want 409; body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "not_installed" {
		t.Errorf("error = %q, want not_installed", errResp["error"])
	}

	// Status must remain available
	st, body = appsDoReq(t, ts, http.MethodGet, "/api/apps/"+appID, nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET after failed update: got %d, want 200; body: %s", st, body)
	}
	var resp struct {
		App struct {
			Status string `json:"status"`
		} `json:"app"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.App.Status != "available" {
		t.Errorf("status after failed update = %q, want available", resp.App.Status)
	}
}

// TestAppsUpdateNoUpdateAvailable tests update when feed has no entry.
func TestAppsUpdateNoUpdateAvailable(t *testing.T) {
	ts, token, _, _ := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	// esphome has update in static feed, but let's use an app without update
	// Find an app not in static feed
	var noUpdateApp string
	for _, a := range apps.Catalog() {
		if a.ID != "adguard-home" && a.ID != "esphome" {
			noUpdateApp = a.ID
			break
		}
	}
	if noUpdateApp == "" {
		// All catalog apps have updates, skip test
		t.Skip("all catalog apps have updates in static feed")
	}

	// Install the app
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+noUpdateApp+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}

	// Update should fail with no_update_available
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+noUpdateApp+"/update", nil, token)
	if st != http.StatusConflict {
		t.Fatalf("POST update no feed entry: got %d, want 409; body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "no_update_available" {
		t.Errorf("error = %q, want no_update_available", errResp["error"])
	}
}

// TestAppsUpdateSameImage tests update when target image same as installed.
func TestAppsUpdateSameImage(t *testing.T) {
	ts, token, db, fakeDriver := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"

	// Install
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}

	// Manually set installed info to match feed image (simulate already updated)
	// Use the database directly to set the installed info
	if err := apps.SetInstalledInfo(db, appID, apps.StatusInstalled, "adguard/adguardhome:v0.108.0", "0.108.0", "sha256:a1b2c3d4e5f6789012345678901234567890abcdef1234567890abcdef123456", ""); err != nil {
		t.Fatalf("set installed info: %v", err)
	}

	// Update should fail with no_update_available
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/update", nil, token)
	if st != http.StatusConflict {
		t.Fatalf("POST update same image: got %d, want 409; body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "no_update_available" {
		t.Errorf("error = %q, want no_update_available", errResp["error"])
	}

	_ = fakeDriver
}

// TestAppsUpdateHealthGateFailure tests health gate failure triggers rollback.
func TestAppsUpdateHealthGateFailure(t *testing.T) {
	ts, token, _, fakeDriver := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"

	// Install and activate
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}
	st, _ = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/activate", nil, token)
	if st != http.StatusOK {
		t.Fatalf("activate: got %d, want 200", st)
	}

	// Make container unhealthy after update (simulate health gate failure)
	// We need to intercept the update - instead, make the container unhealthy
	// before the health check polls. Since we can't hook the update,
	// we test by making the container unhealthy and then calling update.
	// The update will succeed in driver but health check will fail.
	// Actually, the health check polls AFTER update. So we need to make
	// the container unhealthy after update. Since we can't hook,
	// we test the rollback path by making the container unhealthy
	// and then calling update - but the update will set healthy=true.
	// Better approach: test with a custom fake that fails health.
	// Rollback is now tested via TestAppsRollbackSuccess (HTTP) and
	// TestFakeDriverRollbackUpdate / TestFakeDriverRollbackUpdateClearsPrevious (driver).
	_ = fakeDriver
}

// TestAppsUpdateDriverUnavailable tests update without driver returns 503.
func TestAppsUpdateDriverUnavailable(t *testing.T) {
	// Use a file-based database so we can share it between servers
	dbPath := "/tmp/kelvios_test_update_nodriver.db"
	defer os.Remove(dbPath)

	// Create server WITH driver, install app
	ts1, token, db, _ := newAppsTestServerWithFakeDriver(t, dbPath, 0)
	appID := "adguard-home"
	st, _ := appsDoReq(t, ts1, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}
	ts1.Close()
	db.Close()

	// Create server WITHOUT driver, using same database - login instead of setup
	db2, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db2); err != nil {
		db2.Close()
		t.Fatalf("migrate: %v", err)
	}
	db2.SetMaxOpenConns(1)

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
	s := New(cfg, db2, "test-version")
	s.SetDriver(storage.NewSingleDriver(disks))
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	// Deliberately no SetAppDriver: appDriver stays nil.
	s.SetUpdateSource(appupdate.NewStaticFeed())

	ts2 := httptest.NewServer(s.srv.Handler)
	t.Cleanup(func() {
		ts2.Close()
		s.Shutdown(context.Background())
		db2.Close()
	})

	// Login with existing credentials (vault already exists from first server)
	st, body := postJSON(t, ts2.URL+"/api/auth/login", map[string]string{"username": appsTestUser, "password": appsTestPass}, "")
	if st != http.StatusOK {
		t.Fatalf("login: got %d, want 200", st)
	}
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &lr); err != nil || lr.Token == "" {
		t.Fatalf("login response missing token: %v %s", err, body)
	}
	token2 := lr.Token

	st, body = appsDoReq(t, ts2, http.MethodPost, "/api/apps/"+appID+"/update", nil, token2)
	if st != http.StatusServiceUnavailable {
		t.Fatalf("POST update without driver: got %d, want 503; body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "driver_unavailable" {
		t.Errorf("error = %q, want driver_unavailable", errResp["error"])
	}
}

// TestAppsUpdatePortConflict tests update with port conflict returns 409.
func TestAppsUpdatePortConflict(t *testing.T) {
	ts, token, _, _ := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	// Find two apps that share a port
	collisions := apps.PortCollisions()
	var sharedPort int
	var appA, appB string
	for port, ids := range collisions {
		if len(ids) >= 2 {
			sharedPort = port
			appA = ids[0]
			appB = ids[1]
			break
		}
	}
	if sharedPort == 0 {
		t.Fatal("no port collisions found in catalog for test")
	}

	// Install first app
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appA+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install %s: got %d, want 200", appA, st)
	}

	// Install second app (should fail due to port conflict)
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appB+"/install", nil, token)
	if st != http.StatusConflict {
		t.Fatalf("install %s (port conflict): got %d, want 409; body: %s", appB, st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "port_conflict" {
		t.Errorf("error = %q, want port_conflict", errResp["error"])
	}

	// Now test update port conflict: install appA, then try to update appA to have appB's ports
	// This is harder to test via API since update uses feed ports.
	// The static feed for adguard-home adds port 8443 which doesn't conflict.
	// We'll test driver-level port conflict in TestFakeDriverUpdatePortConflict.
}

// TestAppsUpdateRAMExhausted tests update exceeding RAM limit returns 507.
func TestAppsUpdateRAMExhausted(t *testing.T) {
	// RAM limit: 150 MB
	ts, token, _, _ := newAppsTestServerWithFakeDriver(t, ":memory:", 150)

	// adguard-home has RAMMB 128 in catalog, feed has 256
	// Install first (128 MB)
	appID := "adguard-home"
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}

	// Update should fail (256 MB > 150 MB limit)
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/update", nil, token)
	if st != http.StatusInsufficientStorage {
		t.Fatalf("POST update RAM exhausted: got %d, want 507; body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "resource_exhausted" {
		t.Errorf("error = %q, want resource_exhausted", errResp["error"])
	}
}

// TestAppsUpdateEsphomeNoPermissionChange tests update without permission changes.
func TestAppsUpdateEsphomeNoPermissionChange(t *testing.T) {
	ts, token, db, fakeDriver := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "esphome"

	// Install
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}

	// Update - esphome has no permission changes (same ports, same RAM)
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/update", nil, token)
	if st != http.StatusOK {
		t.Fatalf("POST update esphome: got %d, want 200; body: %s", st, body)
	}

	var resp struct {
		App struct {
			Status         string `json:"status"`
			InstalledImage string `json:"installed_image"`
			UpdateAvailable bool   `json:"update_available"`
			Update          struct {
				Version       string `json:"version"`
				ImageRef      string `json:"image_ref"`
				ReleaseNotes  string `json:"release_notes"`
				PermissionChanges struct {
					PortsAdded         []int `json:"ports_added"`
					PortsRemoved       []int `json:"ports_removed"`
					RAMMBFrom          int   `json:"rammb_from"`
					RAMMBTo            int   `json:"rammb_to"`
					ImageSourceChanged bool  `json:"image_source_changed"`
					DigestChanged      bool  `json:"digest_changed"`
				} `json:"permission_changes"`
			} `json:"update"`
		} `json:"app"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	t.Logf("Response body: %s", string(body))
	if resp.App.InstalledImage != "ghcr.io/esphome/esphome:2024.12.0" {
		t.Errorf("installed_image = %q, want ghcr.io/esphome/esphome:2024.12.0", resp.App.InstalledImage)
	}
	if resp.App.UpdateAvailable {
		t.Errorf("update_available = %v, want false after successful update", resp.App.UpdateAvailable)
	}
	// After update, no update should be available (same version in feed)
	if resp.App.Update.Version != "" {
		t.Errorf("update.version should be empty after update, got %q", resp.App.Update.Version)
	}
	// update should be null since no further update available
	if resp.App.Update.ImageRef != "" {
		t.Errorf("update.image_ref should be empty after update, got %q", resp.App.Update.ImageRef)
	}

	// Debug: query database directly
	var dbImageRef, dbVersionLabel, dbDigest, dbPrevImageRef string
	err := db.QueryRow(`
		SELECT image_ref, version_label, digest, previous_image_ref
		FROM app_installs WHERE app_id = ?
	`, appID).Scan(&dbImageRef, &dbVersionLabel, &dbDigest, &dbPrevImageRef)
	if err != nil {
		t.Fatalf("query app_installs: %v", err)
	}
	t.Logf("DB after update: image_ref=%q, version_label=%q, digest=%q, previous_image_ref=%q", dbImageRef, dbVersionLabel, dbDigest, dbPrevImageRef)

	// Verify driver
	img, ok := fakeDriver.GetContainerImage(appID)
	if !ok {
		t.Fatal("container not found after update")
	}
	if img != "ghcr.io/esphome/esphome:2024.12.0" {
		t.Errorf("driver image = %q, want 2024.12.0", img)
	}
}

// TestAppsUpdateRetentionOneGen tests that previous_image_ref is cleared after second update.
func TestAppsUpdateRetentionOneGen(t *testing.T) {
	ts, token, _, fakeDriver := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"

	// Install
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}

	// First update
	st, _ = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/update", nil, token)
	if st != http.StatusOK {
		t.Fatalf("first update: got %d, want 200", st)
	}

	// Verify previous_image is set
	prevImg, ok := fakeDriver.GetContainerPreviousImage(appID)
	if !ok {
		t.Fatal("container not found after first update")
	}
	if prevImg == "" {
		t.Error("previous_image should be set after first update")
	}

	// Second update - we can't easily do this via API since static feed only has one version
	// But we can test driver-level retention in TestFakeDriverRollbackUpdateClearsPrevious (now implemented)
}

// TestAppsUpdateListDetailFields tests that list/detail include update fields.
func TestAppsUpdateListDetailFields(t *testing.T) {
	ts, token, _, _ := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"

	// Install
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}

	// GET detail before update
	st, body := appsDoReq(t, ts, http.MethodGet, "/api/apps/"+appID, nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET detail: got %d, want 200; body: %s", st, body)
	}

	var resp struct {
		App struct {
			InstalledImage  string `json:"installed_image"`
			UpdateAvailable bool   `json:"update_available"`
			Update          *struct {
				Version       string `json:"version"`
				ImageRef      string `json:"image_ref"`
				ReleaseNotes  string `json:"release_notes"`
				PermissionChanges struct {
					PortsAdded         []int `json:"ports_added"`
					PortsRemoved       []int `json:"ports_removed"`
					RAMMBFrom          int   `json:"rammb_from"`
					RAMMBTo            int   `json:"rammb_to"`
					ImageSourceChanged bool  `json:"image_source_changed"`
					DigestChanged      bool  `json:"digest_changed"`
				} `json:"permission_changes"`
			} `json:"update"`
		} `json:"app"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	t.Logf("GET detail before update (BEFORE): %s", string(body))
	// Before update, installed_image should be catalog image (catalog has "latest" tag)
	if resp.App.InstalledImage != "adguard/adguardhome:latest" {
		t.Errorf("installed_image before update = %q, want catalog image (latest)", resp.App.InstalledImage)
	}
	// Update should be available
	if !resp.App.UpdateAvailable {
		t.Error("update_available should be true before update")
	}
	if resp.App.Update == nil {
		t.Fatal("update should not be nil before update")
	}
	if resp.App.Update.Version != "0.108.0" {
		t.Errorf("update.version = %q, want 0.108.0", resp.App.Update.Version)
	}
	if resp.App.Update.ImageRef != "adguard/adguardhome:v0.108.0" {
		t.Errorf("update.image_ref = %q, want adguard/adguardhome:v0.108.0", resp.App.Update.ImageRef)
	}
	// Check permission changes - port 8443 added, RAM 128->256
	found8443 := false
	for _, p := range resp.App.Update.PermissionChanges.PortsAdded {
		if p == 8443 {
			found8443 = true
			break
		}
	}
	if !found8443 {
		t.Errorf("permission_changes.ports_added missing 8443: %v", resp.App.Update.PermissionChanges.PortsAdded)
	}
	if resp.App.Update.PermissionChanges.RAMMBFrom != 128 {
		t.Errorf("permission_changes.rammb_from = %d, want 128", resp.App.Update.PermissionChanges.RAMMBFrom)
	}
	if resp.App.Update.PermissionChanges.RAMMBTo != 256 {
		t.Errorf("permission_changes.rammb_to = %d, want 256", resp.App.Update.PermissionChanges.RAMMBTo)
	}

	// Update
	st, _ = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/update", nil, token)
	if st != http.StatusOK {
		t.Fatalf("update: got %d, want 200", st)
	}

	// GET detail after update
	st, body = appsDoReq(t, ts, http.MethodGet, "/api/apps/"+appID, nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET detail after update: got %d, want 200; body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	t.Logf("GET detail after update (AFTER): %s", string(body))
	t.Logf("Raw body bytes: %v", []byte(string(body)))
	if resp.App.Update != nil {
		t.Logf("Immediately after unmarshal: UpdateAvailable=%v, Update.Version=%q, Update.ImageRef=%q, Update=%+v", resp.App.UpdateAvailable, resp.App.Update.Version, resp.App.Update.ImageRef, resp.App.Update)
	} else {
		t.Logf("Immediately after unmarshal: UpdateAvailable=%v, Update=nil", resp.App.UpdateAvailable)
	}
	if resp.App.InstalledImage != "adguard/adguardhome:v0.108.0" {
		t.Errorf("installed_image after update = %q, want feed image", resp.App.InstalledImage)
	}
	// After update, no update should be available
	if resp.App.UpdateAvailable {
		t.Error("update_available should be false after update")
	}
	if resp.App.Update != nil && resp.App.Update.Version != "" {
		t.Errorf("update.version should be empty after update, got %q", resp.App.Update.Version)
	}
	if resp.App.Update != nil {
		t.Errorf("update should be nil after update, got %+v", resp.App.Update)
	}

	// List should also have these fields
	st, body = appsDoReq(t, ts, http.MethodGet, "/api/apps", nil, token)
	if st != http.StatusOK {
		t.Fatalf("GET list: got %d, want 200; body: %s", st, body)
	}
	var listResp struct {
		Apps []struct {
			ID             string `json:"id"`
			InstalledImage string `json:"installed_image"`
			UpdateAvailable bool   `json:"update_available"`
			Update          *struct {
				Version string `json:"version"`
			} `json:"update"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, a := range listResp.Apps {
		if a.ID == appID {
			if a.InstalledImage != "adguard/adguardhome:v0.108.0" {
				t.Errorf("list installed_image = %q, want feed image", a.InstalledImage)
			}
			if a.UpdateAvailable {
				t.Error("list update_available should be false after update")
			}
			break
		}
	}
}

// TestAppsUpdateMethodNotAllowed tests that GET /update returns 405.
func TestAppsUpdateMethodNotAllowed(t *testing.T) {
	ts, token, _, _ := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"
	st, _ := appsDoReq(t, ts, http.MethodGet, "/api/apps/"+appID+"/update", nil, token)
	if st != http.StatusMethodNotAllowed {
		t.Errorf("GET /update: got %d, want 405", st)
	}
	st, _ = appsDoReq(t, ts, http.MethodPut, "/api/apps/"+appID+"/update", nil, token)
	if st != http.StatusMethodNotAllowed {
		t.Errorf("PUT /update: got %d, want 405", st)
	}
	st, _ = appsDoReq(t, ts, http.MethodDelete, "/api/apps/"+appID+"/update", nil, token)
	if st != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /update: got %d, want 405", st)
	}
}

// TestAppsUpdateUnknownApp tests update on unknown app returns 404.
func TestAppsUpdateUnknownApp(t *testing.T) {
	ts, token, _, _ := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/unknown-app/update", nil, token)
	if st != http.StatusNotFound {
		t.Fatalf("POST update unknown app: got %d, want 404; body: %s", st, body)
	}
}

// TestAppsRollbackSuccess tests successful rollback with health gate pass.
func TestAppsRollbackSuccess(t *testing.T) {
	ts, token, db, fakeDriver := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"

	// Install first
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}

	// Activate to make it active (rollback allowed from active)
	st, _ = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/activate", nil, token)
	if st != http.StatusOK {
		t.Fatalf("activate: got %d, want 200", st)
	}

	// Update first to have a previous image
	st, _ = appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/update", nil, token)
	if st != http.StatusOK {
		t.Fatalf("update: got %d, want 200", st)
	}

	// Verify we have a previous image
	prevImg, ok := fakeDriver.GetContainerPreviousImage(appID)
	if !ok || prevImg == "" {
		t.Fatal("previous_image should be set after update")
	}

	// Rollback - should succeed
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/rollback", nil, token)
	if st != http.StatusOK {
		t.Fatalf("POST rollback: got %d, want 200; body: %s", st, body)
	}

	var resp struct {
		App struct {
			ID             string `json:"id"`
			Status         string `json:"status"`
			InstalledImage string `json:"installed_image"`
			UpdateAvailable bool   `json:"update_available"`
			Update         *struct {
				Version       string `json:"version"`
				ImageRef      string `json:"image_ref"`
				ReleaseNotes  string `json:"release_notes"`
				PermissionChanges struct {
					PortsAdded          []int `json:"ports_added"`
					PortsRemoved        []int `json:"ports_removed"`
					RAMMBFrom           int   `json:"rammb_from"`
					RAMMBTo             int   `json:"rammb_to"`
					ImageSourceChanged  bool  `json:"image_source_changed"`
					DigestChanged       bool  `json:"digest_changed"`
				} `json:"permission_changes"`
			} `json:"update"`
		} `json:"app"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.App.Status != "active" {
		t.Errorf("status after rollback = %q, want active", resp.App.Status)
	}
	// After rollback, installed_image should be back to catalog image (latest)
	if resp.App.InstalledImage != "adguard/adguardhome:latest" {
		t.Errorf("installed_image = %q, want adguard/adguardhome:latest", resp.App.InstalledImage)
	}
	// After rollback, update_available should be true (feed has v0.108.0 > catalog latest)
	if !resp.App.UpdateAvailable {
		t.Errorf("update_available = %v, want true after rollback (feed has newer version)", resp.App.UpdateAvailable)
	}
	if resp.App.Update == nil || resp.App.Update.Version != "0.108.0" {
		t.Errorf("update.version = %q, want 0.108.0", resp.App.Update.Version)
	}

	// Debug: query database directly to verify installed info
	var dbImageRef, dbVersionLabel, dbDigest, dbPrevImageRef string
	err := db.QueryRow(`
		SELECT image_ref, version_label, digest, previous_image_ref
		FROM app_installs WHERE app_id = ?
	`, appID).Scan(&dbImageRef, &dbVersionLabel, &dbDigest, &dbPrevImageRef)
	if err != nil {
		t.Fatalf("query app_installs: %v", err)
	}
	t.Logf("DB after rollback: image_ref=%q, version_label=%q, digest=%q, previous_image_ref=%q", dbImageRef, dbVersionLabel, dbDigest, dbPrevImageRef)
	if dbImageRef != "adguard/adguardhome:latest" {
		t.Errorf("DB image_ref = %q, want adguard/adguardhome:latest", dbImageRef)
	}
	if dbPrevImageRef != "" {
		t.Errorf("DB previous_image_ref = %q, want empty (cleared after rollback)", dbPrevImageRef)
	}

	// Verify driver has rolled back image
	img, ok := fakeDriver.GetContainerImage(appID)
	if !ok {
		t.Fatal("container not found after rollback")
	}
	if img != "adguard/adguardhome:latest" {
		t.Errorf("driver image = %q, want catalog image (latest)", img)
	}
	prevImg, ok = fakeDriver.GetContainerPreviousImage(appID)
	if !ok {
		t.Fatal("container not found after rollback")
	}
	if prevImg != "" {
		t.Errorf("driver previous_image = %q, want empty (cleared after rollback)", prevImg)
	}
}

// TestAppsRollbackNotInstalled tests rollback on not-installed app returns 409.
func TestAppsRollbackNotInstalled(t *testing.T) {
	ts, token, _, _ := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"

	// Try rollback without installing
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/rollback", nil, token)
	if st != http.StatusConflict {
		t.Fatalf("POST rollback not installed: got %d, want 409; body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "not_installed" {
		t.Errorf("error = %q, want not_installed", errResp["error"])
	}
}

// TestAppsRollbackNoPrevious tests rollback without previous image returns 409.
func TestAppsRollbackNoPrevious(t *testing.T) {
	ts, token, _, _ := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"

	// Install but don't update (no previous image)
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}

	// Try rollback without previous image
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/rollback", nil, token)
	if st != http.StatusConflict {
		t.Fatalf("POST rollback no previous: got %d, want 409; body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "no_previous_image" {
		t.Errorf("error = %q, want no_previous_image", errResp["error"])
	}
}

// TestAppsRollbackDriverUnavailable tests rollback without driver returns 503.
func TestAppsRollbackDriverUnavailable(t *testing.T) {
	// Create server WITH driver first to set up app state
	dbPath := "/tmp/kelvios_test_rollback_nodriver.db"
	defer os.Remove(dbPath)

	ts1, token, db1, fakeDriver := newAppsTestServerWithFakeDriver(t, dbPath, 0)
	appID := "adguard-home"
	st, _ := appsDoReq(t, ts1, http.MethodPost, "/api/apps/"+appID+"/install", nil, token)
	if st != http.StatusOK {
		t.Fatalf("install: got %d, want 200", st)
	}
	st, _ = appsDoReq(t, ts1, http.MethodPost, "/api/apps/"+appID+"/update", nil, token)
	if st != http.StatusOK {
		t.Fatalf("update: got %d, want 200", st)
	}
	ts1.Close()
	db1.Close()
	_ = fakeDriver

	// Now create server WITHOUT driver using the SAME database
	db, err := catalog.Open(dbPath)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	db.SetMaxOpenConns(1)

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

	// Set up update source (static feed v1)
	s.SetUpdateSource(appupdate.NewStaticFeed())
	// NO driver set - s.appDriver will be nil

	ts := httptest.NewServer(s.srv.Handler)
	t.Cleanup(func() {
		ts.Close()
		s.Shutdown(context.Background())
		db.Close()
	})

	// Use the same token (auth is in the shared database)
	// Now test rollback on server without driver
	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/rollback", nil, token)
	if st != http.StatusServiceUnavailable {
		t.Fatalf("POST rollback no driver: got %d, want 503; body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if errResp["error"] != "driver_unavailable" {
		t.Errorf("error = %q, want driver_unavailable", errResp["error"])
	}
}

// TestAppsRollbackUnknownApp tests rollback on unknown app returns 404.
func TestAppsRollbackUnknownApp(t *testing.T) {
	ts, token, _, _ := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	st, body := appsDoReq(t, ts, http.MethodPost, "/api/apps/unknown-app/rollback", nil, token)
	if st != http.StatusNotFound {
		t.Fatalf("POST rollback unknown app: got %d, want 404; body: %s", st, body)
	}
}

// TestAppsRollbackMethodNotAllowed tests that GET /rollback returns 405.
func TestAppsRollbackMethodNotAllowed(t *testing.T) {
	ts, token, _, _ := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"
	st, _ := appsDoReq(t, ts, http.MethodGet, "/api/apps/"+appID+"/rollback", nil, token)
	if st != http.StatusMethodNotAllowed {
		t.Errorf("GET /rollback: got %d, want 405", st)
	}
	st, _ = appsDoReq(t, ts, http.MethodPut, "/api/apps/"+appID+"/rollback", nil, token)
	if st != http.StatusMethodNotAllowed {
		t.Errorf("PUT /rollback: got %d, want 405", st)
	}
	st, _ = appsDoReq(t, ts, http.MethodDelete, "/api/apps/"+appID+"/rollback", nil, token)
	if st != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /rollback: got %d, want 405", st)
	}
}

// TestAppsRollbackUnauthorized tests rollback without token returns 401.
func TestAppsRollbackUnauthorized(t *testing.T) {
	ts, _, _, _ := newAppsTestServerWithFakeDriver(t, ":memory:", 0)

	appID := "adguard-home"
	st, _ := appsDoReq(t, ts, http.MethodPost, "/api/apps/"+appID+"/rollback", nil, "")
	if st != http.StatusUnauthorized {
		t.Errorf("POST rollback no token: got %d, want 401", st)
	}
}
