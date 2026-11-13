package httpserver

// KelviOS Fase 1 WS11 Langkah-B: vault + secrets route tests.
// Secret VALUES never appear in assertions — only names, statuses, and
// audit rows (which must never contain the value either).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testVaultPass  = "vault-passphrase-1"
	testVaultWrong = "wrong-passphrase-1"
	testSecretName = "api-key"
	testSecretVal  = "s3cr3t-value-xyz"
)

func vaultDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) (int, []byte) {
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

// TestVaultRoutes covers: setup 201/409, unlock 401 generik, lock 204,
// status, PUT 200, list nama-saja, PUT terkunci 423, DELETE tak-ada 404,
// dan baris audit vault tercatat via GET /api/audit-log.
func TestVaultRoutes(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)
	_, lbody := login(t, ts, testAuthUser, testAuthPass)
	var lr struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(lbody, &lr); err != nil || lr.Token == "" {
		t.Fatalf("login missing token: %v %s", err, lbody)
	}
	token := lr.Token

	// 1. status before setup: {false false}
	st, body := getWithToken(t, ts.URL+"/api/vault/status", token)
	if st != http.StatusOK {
		t.Fatalf("status: got %d, want 200", st)
	}
	var vs struct {
		Exists   bool `json:"exists"`
		Unlocked bool `json:"unlocked"`
	}
	if err := json.Unmarshal(body, &vs); err != nil || vs.Exists || vs.Unlocked {
		t.Fatalf("status before setup: %v %s", err, body)
	}

	// 2. setup 201
	st, _ = postJSON(t, ts.URL+"/api/vault/setup",
		map[string]string{"passphrase": testVaultPass}, token)
	if st != http.StatusCreated {
		t.Fatalf("setup: got %d, want 201", st)
	}

	// 3. second setup 409
	st, _ = postJSON(t, ts.URL+"/api/vault/setup",
		map[string]string{"passphrase": testVaultPass}, token)
	if st != http.StatusConflict {
		t.Errorf("second setup: got %d, want 409", st)
	}

	// 4. PUT secret 200
	st, _ = vaultDoReq(t, ts, http.MethodPut, "/api/secrets/"+testSecretName,
		map[string]string{"value": testSecretVal}, token)
	if st != http.StatusOK {
		t.Fatalf("put secret: got %d, want 200", st)
	}

	// 5. GET list: nama-saja, TANPA nilai
	st, body = getWithToken(t, ts.URL+"/api/secrets", token)
	if st != http.StatusOK {
		t.Fatalf("list secrets: got %d, want 200", st)
	}
	if strings.Contains(string(body), testSecretVal) {
		t.Errorf("list response leaks secret value")
	}
	var listResp struct {
		Secrets []map[string]any `json:"secrets"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.Secrets) != 1 {
		t.Fatalf("list count: got %d, want 1", len(listResp.Secrets))
	}
	if listResp.Secrets[0]["name"] != testSecretName {
		t.Errorf("list name: got %v, want %q", listResp.Secrets[0]["name"], testSecretName)
	}
	if _, ok := listResp.Secrets[0]["value"]; ok {
		t.Errorf("list entry contains value field")
	}

	// 6. lock 204
	st, _ = vaultDoReq(t, ts, http.MethodPost, "/api/vault/lock", nil, token)
	if st != http.StatusNoContent {
		t.Fatalf("lock: got %d, want 204", st)
	}

	// 7. PUT while locked 423
	st, _ = vaultDoReq(t, ts, http.MethodPut, "/api/secrets/"+testSecretName,
		map[string]string{"value": "other"}, token)
	if st != 423 {
		t.Errorf("put locked: got %d, want 423", st)
	}

	// 8. unlock wrong passphrase 401 generik (sama dengan login gagal)
	st, wbody := postJSON(t, ts.URL+"/api/vault/unlock",
		map[string]string{"passphrase": testVaultWrong}, token)
	if st != http.StatusUnauthorized {
		t.Errorf("unlock wrong: got %d, want 401", st)
	}
	if !strings.Contains(string(wbody), authGenericFailBody) {
		t.Errorf("unlock wrong body not generic: %s", wbody)
	}

	// 9. unlock correct 200
	st, _ = postJSON(t, ts.URL+"/api/vault/unlock",
		map[string]string{"passphrase": testVaultPass}, token)
	if st != http.StatusOK {
		t.Fatalf("unlock: got %d, want 200", st)
	}

	// 10. DELETE missing 404
	st, _ = vaultDoReq(t, ts, http.MethodDelete, "/api/secrets/no-such", nil, token)
	if st != http.StatusNotFound {
		t.Errorf("delete missing: got %d, want 404", st)
	}

	// 11. DELETE ok 204
	st, _ = vaultDoReq(t, ts, http.MethodDelete, "/api/secrets/"+testSecretName, nil, token)
	if st != http.StatusNoContent {
		t.Errorf("delete: got %d, want 204", st)
	}

	// 12. audit rows vault tercatat, tanpa nilai secret di detail mana pun
	st, abody := getWithToken(t, ts.URL+"/api/audit-log?limit=1000", token)
	if st != http.StatusOK {
		t.Fatalf("audit-log list: got %d, want 200", st)
	}
	var al struct {
		Entries []struct {
			Action string `json:"action"`
			Actor  string `json:"actor"`
			Result string `json:"result"`
			Detail string `json:"detail"`
		} `json:"entries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(abody, &al); err != nil {
		t.Fatalf("decode audit-log: %v", err)
	}
	want := map[string]bool{
		"vault.setup":   false,
		"vault.unlock":  false,
		"vault.lock":    false,
		"secret.put":    false,
		"secret.delete": false,
	}
	for _, e := range al.Entries {
		if _, ok := want[e.Action]; ok {
			want[e.Action] = true
			if e.Actor != testAuthUser {
				t.Errorf("audit %s actor: got %q, want %q", e.Action, e.Actor, testAuthUser)
			}
		}
		if strings.Contains(e.Detail, testSecretVal) {
			t.Errorf("audit detail leaks secret value: action=%s detail=%q", e.Action, e.Detail)
		}
	}
	for action, seen := range want {
		if !seen {
			t.Errorf("missing audit row: %s", action)
		}
	}
}
