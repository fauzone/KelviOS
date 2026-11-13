// Package httpserver provides tests for the LLM registry HTTP endpoints (FR-3.5).
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/storage"
)

// newLLMTestServer creates a test server for LLM registry tests.
func newLLMTestServer(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	db, err := catalog.Open(":memory:")
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
		Database: config.DatabaseConfig{Path: ":memory:"},
	}
	disks := []storage.Disk{
		{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-LLM-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
	}
	s := New(cfg, db, "test-version")
	s.SetDriver(storage.NewSingleDriver(disks))
	s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }
	ts := httptest.NewServer(s.srv.Handler)

	// Setup admin and login
	setupAdmin(t, ts)
	st, body := login(t, ts, testAuthUser, testAuthPass)
	if st != http.StatusOK {
		t.Fatalf("login: got %d, want 200", st)
	}
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &lr); err != nil || lr.Token == "" {
		t.Fatalf("login response missing token: %v %s", err, body)
	}

	t.Cleanup(func() {
		ts.Close()
		s.Shutdown(context.Background())
		db.Close()
	})

	return s, ts, lr.Token
}

func llmDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) (int, []byte) {
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
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

// TestLLMUnauth tests that all five routes return 401 without auth.
func TestLLMUnauth(t *testing.T) {
	_, ts, _ := newLLMTestServer(t)

	routes := []struct {
		method string
		path   string
	}{
		{"GET", "/api/llm/models"},
		{"POST", "/api/llm/models"},
		{"GET", "/api/llm/models/some-id"},
		{"DELETE", "/api/llm/models/some-id"},
		{"POST", "/api/llm/models/select"},
	}

	for _, r := range routes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			st, _ := llmDoReq(t, ts, r.method, r.path, nil, "")
			if st != http.StatusUnauthorized {
				t.Errorf("%s %s: got %d, want 401", r.method, r.path, st)
			}
		})
	}
}

// TestLLMCreateList tests create 201 and list contains it.
func TestLLMCreateList(t *testing.T) {
	_, ts, token := newLLMTestServer(t)

	// Create
	req := map[string]any{
		"name":         "test-model",
		"provider":     "openai",
		"endpoint_env": "OPENAI_API_BASE",
		"model_id":     "gpt-4",
		"capabilities": map[string]any{"max_tokens": 4096},
		"secret_name":  "openai-secret",
	}
	st, body := llmDoReq(t, ts, "POST", "/api/llm/models", req, token)
	if st != http.StatusCreated {
		t.Fatalf("create: got %d, want 201: %s", st, body)
	}
	var createResp struct {
		Model struct {
			ID string `json:"id"`
		} `json:"model"`
	}
	if err := json.Unmarshal(body, &createResp); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	modelID := createResp.Model.ID
	if modelID == "" {
		t.Fatal("create response missing model ID")
	}

	// List
	st, body = llmDoReq(t, ts, "GET", "/api/llm/models", nil, token)
	if st != http.StatusOK {
		t.Fatalf("list: got %d, want 200: %s", st, body)
	}
	var listResp struct {
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	found := false
	for _, m := range listResp.Models {
		if m.ID == modelID {
			found = true
			if m.Name != "test-model" {
				t.Errorf("list model name: got %q, want %q", m.Name, "test-model")
			}
			break
		}
	}
	if !found {
		t.Error("created model not found in list")
	}
}

// TestLLMDuplicateName tests duplicate name returns 409.
func TestLLMDuplicateName(t *testing.T) {
	_, ts, token := newLLMTestServer(t)

	req := map[string]any{
		"name":     "dup-model",
		"provider": "openai",
		"model_id": "gpt-4",
	}

	// First create
	st, _ := llmDoReq(t, ts, "POST", "/api/llm/models", req, token)
	if st != http.StatusCreated {
		t.Fatalf("first create: got %d, want 201", st)
	}

	// Second create with same name
	st, body := llmDoReq(t, ts, "POST", "/api/llm/models", req, token)
	if st != http.StatusConflict {
		t.Errorf("duplicate create: got %d, want 409: %s", st, body)
	}
	var errResp map[string]string
	json.Unmarshal(body, &errResp)
	if errResp["error"] != "model name already exists" {
		t.Errorf("error message: got %q, want %q", errResp["error"], "model name already exists")
	}
}

// TestLLMSecretKeyInCapabilities tests secret key in capabilities returns 400.
func TestLLMSecretKeyInCapabilities(t *testing.T) {
	_, ts, token := newLLMTestServer(t)

	secretKeys := []string{"token", "password", "secret_key", "access_key", "app_key", "account_id", "account_key"}
	for _, key := range secretKeys {
		t.Run(key, func(t *testing.T) {
			req := map[string]any{
				"name":         "test-" + key,
				"provider":     "openai",
				"model_id":     "gpt-4",
				"capabilities": map[string]any{key: "secret-value"},
			}
			st, body := llmDoReq(t, ts, "POST", "/api/llm/models", req, token)
			if st != http.StatusBadRequest {
				t.Errorf("%s: got %d, want 400: %s", key, st, body)
			}
			var errResp map[string]string
			json.Unmarshal(body, &errResp)
			if errResp["error"] != "secret keys not allowed in request body" {
				t.Errorf("%s: error message: got %q, want %q", key, errResp["error"], "secret keys not allowed in request body")
			}
		})
	}
}

// TestLLMMissingRequiredFields tests missing name/provider/model_id returns 400.
func TestLLMMissingRequiredFields(t *testing.T) {
	_, ts, token := newLLMTestServer(t)

	testCases := []struct {
		name string
		req  map[string]any
	}{
		{"missing name", map[string]any{"provider": "openai", "model_id": "gpt-4"}},
		{"missing provider", map[string]any{"name": "test", "model_id": "gpt-4"}},
		{"missing model_id", map[string]any{"name": "test", "provider": "openai"}},
		{"all missing", map[string]any{}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			st, body := llmDoReq(t, ts, "POST", "/api/llm/models", tc.req, token)
			if st != http.StatusBadRequest {
				t.Errorf("%s: got %d, want 400: %s", tc.name, st, body)
			}
			var errResp map[string]string
			json.Unmarshal(body, &errResp)
			if errResp["error"] != "name, provider, model_id required" {
				t.Errorf("%s: error message: got %q", tc.name, errResp["error"])
			}
		})
	}
}

// TestLLMCreateWithoutCapabilities tests create without capabilities succeeds and reads back {}.
func TestLLMCreateWithoutCapabilities(t *testing.T) {
	_, ts, token := newLLMTestServer(t)

	req := map[string]any{
		"name":     "no-caps",
		"provider": "openai",
		"model_id": "gpt-4",
	}
	st, body := llmDoReq(t, ts, "POST", "/api/llm/models", req, token)
	if st != http.StatusCreated {
		t.Fatalf("create: got %d, want 201: %s", st, body)
	}
	var createResp struct {
		Model struct {
			ID           string          `json:"id"`
			Capabilities json.RawMessage `json:"capabilities"`
		} `json:"model"`
	}
	if err := json.Unmarshal(body, &createResp); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	var caps map[string]any
	if err := json.Unmarshal(createResp.Model.Capabilities, &caps); err != nil {
		t.Fatalf("unmarshal capabilities: %v", err)
	}
	if caps == nil {
		t.Error("capabilities should be {}, not null")
	}
	if len(caps) != 0 {
		t.Errorf("capabilities should be empty, got: %v", caps)
	}
}

// TestLLMSelect tests select 200.
func TestLLMSelect(t *testing.T) {
	_, ts, token := newLLMTestServer(t)

	// Create a model
	req := map[string]any{
		"name":     "select-test",
		"provider": "openai",
		"model_id": "gpt-4",
	}
	st, body := llmDoReq(t, ts, "POST", "/api/llm/models", req, token)
	if st != http.StatusCreated {
		t.Fatalf("create: got %d, want 201", st)
	}
	var createResp struct {
		Model struct {
			ID string `json:"id"`
		} `json:"model"`
	}
	json.Unmarshal(body, &createResp)
	modelID := createResp.Model.ID

	// Select it
	st, body = llmDoReq(t, ts, "POST", "/api/llm/models/select", map[string]string{"id": modelID}, token)
	if st != http.StatusOK {
		t.Fatalf("select: got %d, want 200: %s", st, body)
	}
	var selectResp map[string]string
	json.Unmarshal(body, &selectResp)
	if selectResp["status"] != "ok" {
		t.Errorf("select response: got %v, want status=ok", selectResp)
	}
}

// TestLLMDeleteActive tests delete of active model returns 409.
func TestLLMDeleteActive(t *testing.T) {
	_, ts, token := newLLMTestServer(t)

	// Create a model
	req := map[string]any{
		"name":     "active-del",
		"provider": "openai",
		"model_id": "gpt-4",
	}
	st, body := llmDoReq(t, ts, "POST", "/api/llm/models", req, token)
	if st != http.StatusCreated {
		t.Fatalf("create: got %d, want 201", st)
	}
	var createResp struct {
		Model struct {
			ID string `json:"id"`
		} `json:"model"`
	}
	json.Unmarshal(body, &createResp)
	modelID := createResp.Model.ID

	// Select it (make it active)
	st, _ = llmDoReq(t, ts, "POST", "/api/llm/models/select", map[string]string{"id": modelID}, token)
	if st != http.StatusOK {
		t.Fatalf("select: got %d, want 200", st)
	}

	// Try to delete active model
	st, body = llmDoReq(t, ts, "DELETE", "/api/llm/models/"+modelID, nil, token)
	if st != http.StatusConflict {
		t.Errorf("delete active: got %d, want 409: %s", st, body)
	}
	var errResp map[string]string
	json.Unmarshal(body, &errResp)
	if errResp["error"] != "model is active, cannot delete" {
		t.Errorf("error message: got %q, want %q", errResp["error"], "model is active, cannot delete")
	}
}

// TestLLMGetByID tests get-by-id 200.
func TestLLMGetByID(t *testing.T) {
	_, ts, token := newLLMTestServer(t)

	// Create a model
	req := map[string]any{
		"name":         "get-test",
		"provider":     "anthropic",
		"endpoint_env": "ANTHROPIC_API_BASE",
		"model_id":     "claude-3",
		"capabilities": map[string]any{"max_tokens": 8192},
		"secret_name":  "anthropic-secret",
	}
	st, body := llmDoReq(t, ts, "POST", "/api/llm/models", req, token)
	if st != http.StatusCreated {
		t.Fatalf("create: got %d, want 201", st)
	}
	var createResp struct {
		Model struct {
			ID string `json:"id"`
		} `json:"model"`
	}
	json.Unmarshal(body, &createResp)
	modelID := createResp.Model.ID

	// Get by ID
	st, body = llmDoReq(t, ts, "GET", "/api/llm/models/"+modelID, nil, token)
	if st != http.StatusOK {
		t.Fatalf("get by id: got %d, want 200: %s", st, body)
	}
	var getResp struct {
		Model struct {
			ID           string          `json:"id"`
			Name         string          `json:"name"`
			Provider     string          `json:"provider"`
			EndpointEnv  string          `json:"endpoint_env"`
			ModelID      string          `json:"model_id"`
			Capabilities json.RawMessage `json:"capabilities"`
			SecretName   string          `json:"secret_name"`
		} `json:"model"`
	}
	if err := json.Unmarshal(body, &getResp); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if getResp.Model.ID != modelID {
		t.Errorf("get ID mismatch: got %q, want %q", getResp.Model.ID, modelID)
	}
	if getResp.Model.Name != "get-test" {
		t.Errorf("get name: got %q, want %q", getResp.Model.Name, "get-test")
	}
	if getResp.Model.Provider != "anthropic" {
		t.Errorf("get provider: got %q, want %q", getResp.Model.Provider, "anthropic")
	}
	if getResp.Model.ModelID != "claude-3" {
		t.Errorf("get model_id: got %q, want %q", getResp.Model.ModelID, "claude-3")
	}
	if getResp.Model.SecretName != "anthropic-secret" {
		t.Errorf("get secret_name: got %q, want %q", getResp.Model.SecretName, "anthropic-secret")
	}
	var caps map[string]any
	json.Unmarshal(getResp.Model.Capabilities, &caps)
	if caps["max_tokens"] != float64(8192) {
		t.Errorf("get capabilities: got %v, want max_tokens=8192", caps)
	}
}

// TestLLMSelectUnknown tests select unknown model returns 404.
func TestLLMSelectUnknown(t *testing.T) {
	_, ts, token := newLLMTestServer(t)

	st, body := llmDoReq(t, ts, "POST", "/api/llm/models/select", map[string]string{"id": "llm-unknown"}, token)
	if st != http.StatusNotFound {
		t.Errorf("select unknown: got %d, want 404: %s", st, body)
	}
	var errResp map[string]string
	json.Unmarshal(body, &errResp)
	if errResp["error"] != "model not found" {
		t.Errorf("error message: got %q, want %q", errResp["error"], "model not found")
	}
}

// TestLLMDeleteNonActive tests delete non-active model returns 204 and get-after-delete 404.
func TestLLMDeleteNonActive(t *testing.T) {
	_, ts, token := newLLMTestServer(t)

	// Create two models
	req1 := map[string]any{"name": "model1", "provider": "openai", "model_id": "gpt-4"}
	req2 := map[string]any{"name": "model2", "provider": "anthropic", "model_id": "claude-3"}

	st, body := llmDoReq(t, ts, "POST", "/api/llm/models", req1, token)
	if st != http.StatusCreated {
		t.Fatalf("create model1: got %d", st)
	}
	var r1 struct{ Model struct{ ID string `json:"id"` } `json:"model"` }
	json.Unmarshal(body, &r1)

	st, body = llmDoReq(t, ts, "POST", "/api/llm/models", req2, token)
	if st != http.StatusCreated {
		t.Fatalf("create model2: got %d", st)
	}
	var r2 struct{ Model struct{ ID string `json:"id"` } `json:"model"` }
	json.Unmarshal(body, &r2)

	// Select model1 (make it active)
	st, _ = llmDoReq(t, ts, "POST", "/api/llm/models/select", map[string]string{"id": r1.Model.ID}, token)
	if st != http.StatusOK {
		t.Fatalf("select model1: got %d", st)
	}

	// Delete model2 (non-active)
	st, _ = llmDoReq(t, ts, "DELETE", "/api/llm/models/"+r2.Model.ID, nil, token)
	if st != http.StatusNoContent {
		t.Errorf("delete non-active: got %d, want 204", st)
	}

	// Get model2 after delete should be 404
	st, body = llmDoReq(t, ts, "GET", "/api/llm/models/"+r2.Model.ID, nil, token)
	if st != http.StatusNotFound {
		t.Errorf("get after delete: got %d, want 404: %s", st, body)
	}

	// Model1 should still exist
	st, _ = llmDoReq(t, ts, "GET", "/api/llm/models/"+r1.Model.ID, nil, token)
	if st != http.StatusOK {
		t.Errorf("model1 after delete model2: got %d, want 200", st)
	}
}

// TestLLMUnknownRouteMethod tests unknown route method returns 405.
func TestLLMUnknownRouteMethod(t *testing.T) {
	_, ts, token := newLLMTestServer(t)

	// PUT on /api/llm/models (not allowed)
	st, _ := llmDoReq(t, ts, "PUT", "/api/llm/models", nil, token)
	if st != http.StatusMethodNotAllowed {
		t.Errorf("PUT /api/llm/models: got %d, want 405", st)
	}

	// PATCH on /api/llm/models/{id} (not allowed)
	st, _ = llmDoReq(t, ts, "PATCH", "/api/llm/models/some-id", nil, token)
	if st != http.StatusMethodNotAllowed {
		t.Errorf("PATCH /api/llm/models/id: got %d, want 405", st)
	}

	// GET on /api/llm/models/select (not allowed)
	st, _ = llmDoReq(t, ts, "GET", "/api/llm/models/select", nil, token)
	if st != http.StatusMethodNotAllowed {
		t.Errorf("GET /api/llm/models/select: got %d, want 405", st)
	}
}