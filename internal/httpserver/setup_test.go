package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

func newSetupTestServer(t *testing.T, dbPath string) (*Server, *httptest.Server) {
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
	return s, ts
}

func putJSON(t *testing.T, url string, payload any, token string) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(http.MethodPut, url, body)
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

func setupAdminWithToken(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	setupAdmin(t, ts)
	st, body := login(t, ts, testAuthUser, testAuthPass)
	if st != http.StatusOK {
		t.Fatalf("login: got %d, want 200, body: %s", st, body)
	}
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &lr); err != nil || lr.Token == "" {
		t.Fatalf("login response missing token: %v %s", err, body)
	}
	return lr.Token
}

// TestSetupStateTransitions tests the stage progression through the wizard.
func TestSetupStateTransitions(t *testing.T) {
	_, ts := newSetupTestServer(t, ":memory:")

	// Fresh DB: stage = language
	st, body := getWithToken(t, ts.URL+"/api/setup/state", "")
	if st != http.StatusOK {
		t.Fatalf("state fresh: got %d, want 200, body: %s", st, body)
	}
	var resp struct {
		HasAdmin      bool   `json:"has_admin"`
		LanguageSet   bool   `json:"language_set"`
		TimezoneSet   bool   `json:"timezone_set"`
		SetupComplete bool   `json:"setup_complete"`
		Stage         string `json:"stage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if resp.HasAdmin {
		t.Errorf("fresh: has_admin=true, want false")
	}
	if resp.LanguageSet {
		t.Errorf("fresh: language_set=true, want false")
	}
	if resp.TimezoneSet {
		t.Errorf("fresh: timezone_set=true, want false")
	}
	if resp.SetupComplete {
		t.Errorf("fresh: setup_complete=true, want false")
	}
	if resp.Stage != "language" {
		t.Errorf("fresh: stage=%q, want 'language'", resp.Stage)
	}

	// After language preference: stage = timezone
	st, body = postJSON(t, ts.URL+"/api/setup/preferences",
		map[string]string{"language": "id"}, "")
	if st != http.StatusOK {
		t.Fatalf("preferences language: got %d, want 200, body: %s", st, body)
	}
	st, body = getWithToken(t, ts.URL+"/api/setup/state", "")
	if st != http.StatusOK {
		t.Fatalf("state after lang: got %d, want 200, body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if resp.Stage != "timezone" {
		t.Errorf("after language: stage=%q, want 'timezone'", resp.Stage)
	}
	if !resp.LanguageSet {
		t.Errorf("after language: language_set=false, want true")
	}

	// After timezone preference: stage = admin
	st, body = postJSON(t, ts.URL+"/api/setup/preferences",
		map[string]string{"timezone": "Asia/Jakarta"}, "")
	if st != http.StatusOK {
		t.Fatalf("preferences timezone: got %d, want 200, body: %s", st, body)
	}
	st, body = getWithToken(t, ts.URL+"/api/setup/state", "")
	if st != http.StatusOK {
		t.Fatalf("state after tz: got %d, want 200, body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if resp.Stage != "admin" {
		t.Errorf("after timezone: stage=%q, want 'admin'", resp.Stage)
	}
	if !resp.TimezoneSet {
		t.Errorf("after timezone: timezone_set=false, want true")
	}

	// After admin setup: stage = preset
	token := setupAdminWithToken(t, ts)

	st, body = getWithToken(t, ts.URL+"/api/setup/state", token)
	if st != http.StatusOK {
		t.Fatalf("state after admin: got %d, want 200, body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if resp.Stage != "preset" {
		t.Errorf("after admin: stage=%q, want 'preset'", resp.Stage)
	}
	if !resp.HasAdmin {
		t.Errorf("after admin: has_admin=false, want true")
	}

	// After complete: setup_complete=true, stage=complete
	st, body = postJSON(t, ts.URL+"/api/setup/complete", nil, token)
	if st != http.StatusOK {
		t.Fatalf("complete: got %d, want 200, body: %s", st, body)
	}
	st, body = getWithToken(t, ts.URL+"/api/setup/state", token)
	if st != http.StatusOK {
		t.Fatalf("state after complete: got %d, want 200, body: %s", st, body)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if !resp.SetupComplete {
		t.Errorf("after complete: setup_complete=false, want true")
	}
	if resp.Stage != "complete" {
		t.Errorf("after complete: stage=%q, want 'complete'", resp.Stage)
	}
}

// TestSetupPreferencesAuthGates tests the auth gates for preferences.
func TestSetupPreferencesAuthGates(t *testing.T) {
	// Pre-admin: no auth required
	t.Run("pre_admin_no_auth", func(t *testing.T) {
		_, ts := newSetupTestServer(t, ":memory:")
		st, body := postJSON(t, ts.URL+"/api/setup/preferences",
			map[string]string{"language": "id"}, "")
		if st != http.StatusOK {
			t.Errorf("pre-admin no auth: got %d, want 200, body: %s", st, body)
		}
	})

	// Post-admin: without auth = 401
	t.Run("post_admin_no_auth_401", func(t *testing.T) {
		_, ts := newSetupTestServer(t, ":memory:")
		setupAdmin(t, ts)
		st, body := postJSON(t, ts.URL+"/api/setup/preferences",
			map[string]string{"language": "en"}, "")
		if st != http.StatusUnauthorized {
			t.Errorf("post-admin no auth: got %d, want 401, body: %s", st, body)
		}
	})

	// Post-admin: with auth = 200
	t.Run("post_admin_with_auth", func(t *testing.T) {
		_, ts := newSetupTestServer(t, ":memory:")
		token := setupAdminWithToken(t, ts)
		st, body := postJSON(t, ts.URL+"/api/setup/preferences",
			map[string]string{"language": "en"}, token)
		if st != http.StatusOK {
			t.Errorf("post-admin with auth: got %d, want 200, body: %s", st, body)
		}
	})
}

// TestSetupCompleteGate tests that all setup endpoints return 409 after complete.
func TestSetupCompleteGate(t *testing.T) {
	_, ts := newSetupTestServer(t, ":memory:")
	token := setupAdminWithToken(t, ts)

	// Complete setup
	st, body := postJSON(t, ts.URL+"/api/setup/complete", nil, token)
	if st != http.StatusOK {
		t.Fatalf("complete: got %d, want 200, body: %s", st, body)
	}

	// Preferences after complete = 409
	st, body = postJSON(t, ts.URL+"/api/setup/preferences",
		map[string]string{"language": "en"}, token)
	if st != http.StatusConflict {
		t.Errorf("preferences after complete: got %d, want 409, body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil || errResp["error"] != "setup_complete" {
		t.Errorf("preferences after complete error: %s", body)
	}

	// Import after complete = 409
	st, body = postJSON(t, ts.URL+"/api/setup/import",
		map[string]any{
			"format": "kelvios-config", "version": 1, "exported_at": time.Now().Unix(),
			"settings": map[string]string{"language": "en"},
		}, token)
	if st != http.StatusConflict {
		t.Errorf("import after complete: got %d, want 409, body: %s", st, body)
	}

	// Complete again = 409
	st, body = postJSON(t, ts.URL+"/api/setup/complete", nil, token)
	if st != http.StatusConflict {
		t.Errorf("complete again: got %d, want 409, body: %s", st, body)
	}
}

// TestSetupPreferencesValidation tests validation of preferences.
func TestSetupPreferencesValidation(t *testing.T) {
	_, ts := newSetupTestServer(t, ":memory:")

	// Invalid language
	st, body := postJSON(t, ts.URL+"/api/setup/preferences",
		map[string]string{"language": "fr"}, "")
	if st != http.StatusBadRequest {
		t.Errorf("invalid language: got %d, want 400, body: %s", st, body)
	}

	// Invalid timezone
	st, body = postJSON(t, ts.URL+"/api/setup/preferences",
		map[string]string{"timezone": "Invalid/Zone"}, "")
	if st != http.StatusBadRequest {
		t.Errorf("invalid timezone: got %d, want 400, body: %s", st, body)
	}

	// Unknown field
	st, body = postJSON(t, ts.URL+"/api/setup/preferences",
		map[string]string{"language": "id", "unknown": "value"}, "")
	if st != http.StatusBadRequest {
		t.Errorf("unknown field: got %d, want 400, body: %s", st, body)
	}

	// Empty body
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/setup/preferences", bytes.NewReader([]byte{}))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty body: got %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestSetupImportValidation tests strict validation of import.
func TestSetupImportValidation(t *testing.T) {
	_, ts := newSetupTestServer(t, ":memory:")
	token := setupAdminWithToken(t, ts)

	// Invalid format
	st, body := postJSON(t, ts.URL+"/api/setup/import",
		map[string]any{
			"format": "wrong", "version": 1, "exported_at": time.Now().Unix(),
			"settings": map[string]string{},
		}, token)
	if st != http.StatusBadRequest {
		t.Errorf("invalid format: got %d, want 400, body: %s", st, body)
	}

	// Invalid version
	st, body = postJSON(t, ts.URL+"/api/setup/import",
		map[string]any{
			"format": "kelvios-config", "version": 2, "exported_at": time.Now().Unix(),
			"settings": map[string]string{},
		}, token)
	if st != http.StatusBadRequest {
		t.Errorf("invalid version: got %d, want 400, body: %s", st, body)
	}

	// Secret key at top level
	st, body = postJSON(t, ts.URL+"/api/setup/import",
		map[string]any{
			"format": "kelvios-config", "version": 1, "exported_at": time.Now().Unix(),
			"settings": map[string]string{"password": "secret"},
		}, token)
	if st != http.StatusBadRequest {
		t.Errorf("secret key top level: got %d, want 400, body: %s", st, body)
	}

	// Secret key nested (api_key)
	st, body = postJSON(t, ts.URL+"/api/setup/import",
		map[string]any{
			"format": "kelvios-config", "version": 1, "exported_at": time.Now().Unix(),
			"settings": map[string]any{"language": "id", "nested": map[string]string{"api_key": "secret"}},
		}, token)
	if st != http.StatusBadRequest {
		t.Errorf("secret key nested: got %d, want 400, body: %s", st, body)
	}

	// Unknown field in settings
	st, body = postJSON(t, ts.URL+"/api/setup/import",
		map[string]any{
			"format": "kelvios-config", "version": 1, "exported_at": time.Now().Unix(),
			"settings": map[string]string{"language": "id", "unknown_field": "value"},
		}, token)
	if st != http.StatusBadRequest {
		t.Errorf("unknown field in settings: got %d, want 400, body: %s", st, body)
	}

	// agent_preset without admin (pre-auth) = 400
	_, ts2 := newSetupTestServer(t, ":memory:")
	st, body = postJSON(t, ts2.URL+"/api/setup/import",
		map[string]any{
			"format": "kelvios-config", "version": 1, "exported_at": time.Now().Unix(),
			"settings": map[string]string{"agent_preset": "agent_inside"},
		}, "")
	if st != http.StatusBadRequest {
		t.Errorf("agent_preset pre-admin: got %d, want 400, body: %s", st, body)
	}
	var errResp map[string]string
	if err := json.Unmarshal(body, &errResp); err != nil || !strings.Contains(errResp["error"], "agent_preset requires admin") {
		t.Errorf("agent_preset pre-admin error: %s", body)
	}
}

// TestSetupImportAppliesAtomically tests that valid import applies language+timezone.
func TestSetupImportAppliesAtomically(t *testing.T) {
	_, ts := newSetupTestServer(t, ":memory:")
	token := setupAdminWithToken(t, ts)

	st, body := postJSON(t, ts.URL+"/api/setup/import",
		map[string]any{
			"format": "kelvios-config", "version": 1, "exported_at": time.Now().Unix(),
			"settings": map[string]string{"language": "id", "timezone": "Asia/Jakarta"},
		}, token)
	if st != http.StatusOK {
		t.Fatalf("import valid: got %d, want 200, body: %s", st, body)
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode import response: %v", err)
	}
	applied, ok := resp["applied"].([]any)
	if !ok || len(applied) != 2 {
		t.Errorf("import applied: got %v, want [language timezone]", resp["applied"])
	}

	// Verify via state
	st, body = getWithToken(t, ts.URL+"/api/setup/state", token)
	if st != http.StatusOK {
		t.Fatalf("state after import: got %d, want 200", st)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if resp["language_set"] != true {
		t.Errorf("language_set after import: %v", resp["language_set"])
	}
	if resp["timezone_set"] != true {
		t.Errorf("timezone_set after import: %v", resp["timezone_set"])
	}
}

// TestSetupImportAgentPreset tests agent_preset import post-auth.
func TestSetupImportAgentPreset(t *testing.T) {
	_, ts := newSetupTestServer(t, ":memory:")
	token := setupAdminWithToken(t, ts)

	st, body := postJSON(t, ts.URL+"/api/setup/import",
		map[string]any{
			"format": "kelvios-config", "version": 1, "exported_at": time.Now().Unix(),
			"settings": map[string]string{"agent_preset": "both"},
		}, token)
	if st != http.StatusOK {
		t.Fatalf("import agent_preset: got %d, want 200, body: %s", st, body)
	}

	// Verify via agent-modes endpoint
	st, body = getWithToken(t, ts.URL+"/api/agent-modes", token)
	if st != http.StatusOK {
		t.Fatalf("get agent-modes: got %d, want 200", st)
	}
	var modes struct {
		Preset string `json:"preset"`
	}
	if err := json.Unmarshal(body, &modes); err != nil {
		t.Fatalf("decode agent-modes: %v", err)
	}
	if modes.Preset != "both" {
		t.Errorf("agent preset after import: got %q, want 'both'", modes.Preset)
	}
}

// TestExportImportRoundTrip tests export -> import round-trip.
func TestExportImportRoundTrip(t *testing.T) {
	_, ts := newSetupTestServer(t, ":memory:")
	token := setupAdminWithToken(t, ts)

	// Set some settings first
	postJSON(t, ts.URL+"/api/setup/preferences",
		map[string]string{"language": "id", "timezone": "Asia/Jakarta"}, token)

	// Export
	st, body := getWithToken(t, ts.URL+"/api/settings/export", token)
	if st != http.StatusOK {
		t.Fatalf("export: got %d, want 200, body: %s", st, body)
	}
	var exportResp map[string]any
	if err := json.Unmarshal(body, &exportResp); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if exportResp["format"] != "kelvios-config" || exportResp["version"] != float64(1) {
		t.Errorf("export format/version: %v", exportResp)
	}

	// Import into fresh DB
	_, ts2 := newSetupTestServer(t, ":memory:")
	token2 := setupAdminWithToken(t, ts2)
	st, body = postJSON(t, ts2.URL+"/api/setup/import", exportResp, token2)
	if st != http.StatusOK {
		t.Fatalf("import round-trip: got %d, want 200, body: %s", st, body)
	}

	// Verify settings match
	st, body = getWithToken(t, ts2.URL+"/api/settings/system", token2)
	if st != http.StatusOK {
		t.Fatalf("get system after import: got %d, want 200", st)
	}
	var sysResp map[string]string
	if err := json.Unmarshal(body, &sysResp); err != nil {
		t.Fatalf("decode system: %v", err)
	}
	if sysResp["language"] != "id" {
		t.Errorf("language after round-trip: got %q, want 'id'", sysResp["language"])
	}
	if sysResp["timezone"] != "Asia/Jakarta" {
		t.Errorf("timezone after round-trip: got %q, want 'Asia/Jakarta'", sysResp["timezone"])
	}
}

// TestSetupStateNoSecrets tests that /api/setup/state returns no secrets.
func TestSetupStateNoSecrets(t *testing.T) {
	_, ts := newSetupTestServer(t, ":memory:")
	token := setupAdminWithToken(t, ts)

	st, body := getWithToken(t, ts.URL+"/api/setup/state", token)
	if st != http.StatusOK {
		t.Fatalf("state: got %d, want 200", st)
	}
	bodyStr := string(body)
	// Should not contain username, token, password, hash (secret VALUES).
	// "admin" is NOT forbidden — it appears in the legitimate field name "has_admin"
	// which is a required boolean per Scope 1 contract.
	forbidden := []string{"username", "token", "password", "hash"}
	for _, f := range forbidden {
		if strings.Contains(strings.ToLower(bodyStr), f) {
			t.Errorf("state response contains forbidden term %q: %s", f, bodyStr)
		}
	}
}

// TestSystemSettingsEndpoints tests GET/PUT /api/settings/system.
func TestSystemSettingsEndpoints(t *testing.T) {
	_, ts := newSetupTestServer(t, ":memory:")
	token := setupAdminWithToken(t, ts)

	// GET before setting
	st, body := getWithToken(t, ts.URL+"/api/settings/system", token)
	if st != http.StatusOK {
		t.Fatalf("get system: got %d, want 200, body: %s", st, body)
	}
	var resp map[string]string
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode system: %v", err)
	}
	if resp["language"] != "" || resp["timezone"] != "" {
		t.Errorf("system before set: got %v, want empty", resp)
	}

	// PUT language
	st, body = putJSON(t, ts.URL+"/api/settings/system",
		map[string]string{"language": "id"}, token)
	if st != http.StatusOK {
		t.Fatalf("put language: got %d, want 200, body: %s", st, body)
	}

	// GET after
	st, body = getWithToken(t, ts.URL+"/api/settings/system", token)
	if st != http.StatusOK {
		t.Fatalf("get system after: got %d, want 200", st)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode system after: %v", err)
	}
	if resp["language"] != "id" {
		t.Errorf("language after put: got %q, want 'id'", resp["language"])
	}

	// PUT timezone
	st, body = putJSON(t, ts.URL+"/api/settings/system",
		map[string]string{"timezone": "UTC"}, token)
	if st != http.StatusOK {
		t.Fatalf("put timezone: got %d, want 200, body: %s", st, body)
	}

	// GET after
	st, body = getWithToken(t, ts.URL+"/api/settings/system", token)
	if st != http.StatusOK {
		t.Fatalf("get system after tz: got %d, want 200", st)
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode system after tz: %v", err)
	}
	if resp["timezone"] != "UTC" {
		t.Errorf("timezone after put: got %q, want 'UTC'", resp["timezone"])
	}

	// Validation: invalid language
	st, body = putJSON(t, ts.URL+"/api/settings/system",
		map[string]string{"language": "fr"}, token)
	if st != http.StatusBadRequest {
		t.Errorf("put invalid language: got %d, want 400, body: %s", st, body)
	}

	// Validation: invalid timezone
	st, body = putJSON(t, ts.URL+"/api/settings/system",
		map[string]string{"timezone": "Invalid/Zone"}, token)
	if st != http.StatusBadRequest {
		t.Errorf("put invalid timezone: got %d, want 400, body: %s", st, body)
	}

	// Unknown field
	st, body = putJSON(t, ts.URL+"/api/settings/system",
		map[string]string{"language": "en", "unknown": "value"}, token)
	if st != http.StatusBadRequest {
		t.Errorf("put unknown field: got %d, want 400, body: %s", st, body)
	}

	// Without auth = 401
	st, body = getWithToken(t, ts.URL+"/api/settings/system", "")
	if st != http.StatusUnauthorized {
		t.Errorf("get system no auth: got %d, want 401", st)
	}
	st, body = putJSON(t, ts.URL+"/api/settings/system",
		map[string]string{"language": "en"}, "")
	if st != http.StatusUnauthorized {
		t.Errorf("put system no auth: got %d, want 401", st)
	}
}

// TestSetupPreferencesPreAdminNoAuth tests that preferences work without auth before admin exists.
func TestSetupPreferencesPreAdminNoAuth(t *testing.T) {
	_, ts := newSetupTestServer(t, ":memory:")

	// Should work without auth
	st, body := postJSON(t, ts.URL+"/api/setup/preferences",
		map[string]string{"language": "id"}, "")
	if st != http.StatusOK {
		t.Errorf("preferences pre-admin no auth: got %d, want 200, body: %s", st, body)
	}

	// Verify it was set
	st, body = getWithToken(t, ts.URL+"/api/setup/state", "")
	if st != http.StatusOK {
		t.Fatalf("state: got %d, want 200", st)
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if resp["language_set"] != true {
		t.Errorf("language_set not true: %v", resp)
	}
}