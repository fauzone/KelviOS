package httpserver

// KelviOS Fase 1 WS18 Langkah-B (takeover Chief): supply-chain HTTP route tests.
// Production handlers + catalog CRUD landed in the original Langkah-B session;
// these tests cover the HTTP contract only: create/validation/list/get/delete,
// the verify (A1.8) decision surface, audit rows, and restart persistence.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const (
	scDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	scDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func scDoReq(t *testing.T, ts *httptest.Server, method, path string, payload any, token string) (int, []byte) {
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

func scLoginToken(t *testing.T, ts *httptest.Server) string {
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

func scValidEntry(name string) map[string]any {
	return map[string]any{
		"name":                 name,
		"publisher":            "Example Org",
		"source_repo":          "ghcr.io/example/app",
		"image_ref":            "ghcr.io/example/app:1.2.3",
		"pinned_digest":        scDigestA,
		"signature_kind":       "cosign",
		"source_kind":          "official",
		"unverified_confirmed": false,
	}
}

type scEntryResponse struct {
	SupplychainEntry struct {
		ID                string `json:"id"`
		Name              string `json:"name"`
		PinnedDigest      string `json:"pinned_digest"`
		SignatureKind     string `json:"signature_kind"`
		SourceKind        string `json:"source_kind"`
		UnverifiedConfirm bool   `json:"unverified_confirmed"`
	} `json:"supplychain_entry"`
}

func scCreate(t *testing.T, ts *httptest.Server, token, name string) (int, scEntryResponse, []byte) {
	t.Helper()
	st, body := scDoReq(t, ts, http.MethodPost, "/api/supplychain/entries", scValidEntry(name), token)
	var out scEntryResponse
	if st == http.StatusCreated {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode create response: %v %s", err, body)
		}
	}
	return st, out, body
}

// TestSupplychainCreateGetListDelete covers the CRUD happy path + auth +
// duplicate-name conflict + delete semantics.
func TestSupplychainCreateGetListDelete(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)
	var token string
	token = scLoginToken(t, ts)

	// Unauthenticated create -> 401 generic.
	st, _ := scDoReq(t, ts, http.MethodPost, "/api/supplychain/entries", scValidEntry("app-one"), "")
	if st != http.StatusUnauthorized {
		t.Fatalf("unauth create: got %d, want 401", st)
	}

	// Create 201 with all fields echoed.
	st, created, _ := scCreate(t, ts, token, "app-one")
	if st != http.StatusCreated {
		t.Fatalf("create: got %d, want 201", st)
	}
	if created.SupplychainEntry.ID == "" || created.SupplychainEntry.Name != "app-one" ||
		created.SupplychainEntry.PinnedDigest != scDigestA ||
		created.SupplychainEntry.SignatureKind != "cosign" ||
		created.SupplychainEntry.SourceKind != "official" {
		t.Fatalf("create response mismatch: %+v", created.SupplychainEntry)
	}
	id := created.SupplychainEntry.ID

	// Duplicate name -> 409.
	st, _, _ = scCreate(t, ts, token, "app-one")
	if st != http.StatusConflict {
		t.Errorf("duplicate name: got %d, want 409", st)
	}

	// Get 200 / unknown 404.
	st, body := scDoReq(t, ts, http.MethodGet, "/api/supplychain/entries/"+id, nil, token)
	if st != http.StatusOK {
		t.Fatalf("get: got %d, want 200", st)
	}
	var got scEntryResponse
	if err := json.Unmarshal(body, &got); err != nil || got.SupplychainEntry.ID != id {
		t.Fatalf("get response mismatch: %v %s", err, body)
	}
	st, _ = scDoReq(t, ts, http.MethodGet, "/api/supplychain/entries/nope-404", nil, token)
	if st != http.StatusNotFound {
		t.Errorf("get unknown: got %d, want 404", st)
	}

	// List contains exactly the one entry; unauth list -> 401.
	st, body = scDoReq(t, ts, http.MethodGet, "/api/supplychain/entries", nil, token)
	if st != http.StatusOK {
		t.Fatalf("list: got %d, want 200", st)
	}
	var listResp struct {
		Entries []map[string]any `json:"supplychain_entries"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.Entries) != 1 {
		t.Fatalf("list count: got %d, want 1", len(listResp.Entries))
	}
	st, _ = scDoReq(t, ts, http.MethodGet, "/api/supplychain/entries", nil, "")
	if st != http.StatusUnauthorized {
		t.Errorf("unauth list: got %d, want 401", st)
	}

	// Delete 204, then get 404, delete again 404.
	st, _ = scDoReq(t, ts, http.MethodDelete, "/api/supplychain/entries/"+id, nil, token)
	if st != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204", st)
	}
	st, _ = scDoReq(t, ts, http.MethodGet, "/api/supplychain/entries/"+id, nil, token)
	if st != http.StatusNotFound {
		t.Errorf("get after delete: got %d, want 404", st)
	}
	st, _ = scDoReq(t, ts, http.MethodDelete, "/api/supplychain/entries/"+id, nil, token)
	if st != http.StatusNotFound {
		t.Errorf("delete again: got %d, want 404", st)
	}
}

// TestSupplychainCreateValidationRejects covers the 400 branches driven by
// supplychain.ValidateEntry: missing field, floating/empty/uppercase digest,
// invalid source kind, invalid signature kind.
func TestSupplychainCreateValidationRejects(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)
	var token string
	token = scLoginToken(t, ts)

	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing-name", func(m map[string]any) { m["name"] = "" }},
		{"missing-publisher", func(m map[string]any) { m["publisher"] = "  " }},
		{"floating-digest", func(m map[string]any) { m["pinned_digest"] = "latest" }},
		{"empty-digest", func(m map[string]any) { m["pinned_digest"] = "" }},
		{"uppercase-digest", func(m map[string]any) { m["pinned_digest"] = strings.ToUpper(scDigestA) }},
		{"short-digest", func(m map[string]any) { m["pinned_digest"] = "sha256:abc" }},
		{"bad-source-kind", func(m map[string]any) { m["source_kind"] = "vendor" }},
		{"bad-signature-kind", func(m map[string]any) { m["signature_kind"] = "pgp" }},
	}
	for _, tc := range cases {
		body := scValidEntry("bad-" + tc.name)
		tc.mutate(body)
		st, respBody := scDoReq(t, ts, http.MethodPost, "/api/supplychain/entries", body, token)
		if st != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400 (body %s)", tc.name, st, respBody)
		}
	}

	// Nothing was persisted by the rejected creates.
	st, body := scDoReq(t, ts, http.MethodGet, "/api/supplychain/entries", nil, token)
	if st != http.StatusOK {
		t.Fatalf("list after rejects: got %d, want 200", st)
	}
	var listResp struct {
		Entries []map[string]any `json:"supplychain_entries"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listResp.Entries) != 0 {
		t.Errorf("rejected creates persisted %d entries, want 0", len(listResp.Entries))
	}
}

// TestSupplychainVerifyDecisions covers the A1.8 gate over HTTP: allow,
// digest_mismatch, invalid_served_digest, signature_failed, signature_missing,
// custom unverified_unconfirmed, plus request validation and audit rows.
func TestSupplychainVerifyDecisions(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	setupAdmin(t, ts)
	var token string
	token = scLoginToken(t, ts)

	_, official, _ := scCreate(t, ts, token, "official-app")
	offID := official.SupplychainEntry.ID

	verify := func(id string, payload map[string]any) (int, map[string]any) {
		st, body := scDoReq(t, ts, http.MethodPost, "/api/supplychain/entries/"+id+"/verify", payload, token)
		var out map[string]any
		if st == http.StatusOK {
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatalf("decode verify response: %v %s", err, body)
			}
		}
		return st, out
	}

	// Happy: digest matches + signature verified -> allowed.
	st, out := verify(offID, map[string]any{"served_digest": scDigestA, "signature_status": "verified"})
	if st != http.StatusOK || out["allowed"] != true {
		t.Fatalf("verify happy: got %d %v, want 200 allowed=true", st, out)
	}

	// Digest mismatch (A1.8 core case).
	st, out = verify(offID, map[string]any{"served_digest": scDigestB, "signature_status": "verified"})
	if st != http.StatusOK || out["allowed"] != false || out["reason"] != "digest_mismatch" {
		t.Fatalf("verify mismatch: got %d %v, want allowed=false reason=digest_mismatch", st, out)
	}

	// Served digest not a digest at all.
	st, out = verify(offID, map[string]any{"served_digest": "latest", "signature_status": "verified"})
	if st != http.StatusOK || out["allowed"] != false || out["reason"] != "invalid_served_digest" {
		t.Fatalf("verify invalid served: got %d %v, want reason=invalid_served_digest", st, out)
	}

	// Signature failed is never overridden.
	st, out = verify(offID, map[string]any{"served_digest": scDigestA, "signature_status": "failed"})
	if st != http.StatusOK || out["allowed"] != false || out["reason"] != "signature_failed" {
		t.Fatalf("verify sig failed: got %d %v, want reason=signature_failed", st, out)
	}

	// Signature required (cosign) but not provided.
	st, out = verify(offID, map[string]any{"served_digest": scDigestA, "signature_status": "not_provided"})
	if st != http.StatusOK || out["allowed"] != false || out["reason"] != "signature_missing" {
		t.Fatalf("verify sig missing: got %d %v, want reason=signature_missing", st, out)
	}

	// Invalid signature_status value -> 400.
	st, _ = scDoReq(t, ts, http.MethodPost, "/api/supplychain/entries/"+offID+"/verify",
		map[string]any{"served_digest": scDigestA, "signature_status": "maybe"}, token)
	if st != http.StatusBadRequest {
		t.Errorf("verify bad signature_status: got %d, want 400", st)
	}

	// Verify on unknown entry -> 404; unauth verify -> 401.
	st, _ = scDoReq(t, ts, http.MethodPost, "/api/supplychain/entries/nope-404/verify",
		map[string]any{"served_digest": scDigestA, "signature_status": "verified"}, token)
	if st != http.StatusNotFound {
		t.Errorf("verify unknown entry: got %d, want 404", st)
	}
	st, _ = scDoReq(t, ts, http.MethodPost, "/api/supplychain/entries/"+offID+"/verify",
		map[string]any{"served_digest": scDigestA, "signature_status": "verified"}, "")
	if st != http.StatusUnauthorized {
		t.Errorf("verify unauth: got %d, want 401", st)
	}

	// Custom entry without unverified confirmation -> unverified_unconfirmed;
	// the same entry shape WITH confirmation is creatable and verifies allowed.
	customBody := scValidEntry("custom-app")
	customBody["source_kind"] = "custom"
	customBody["signature_kind"] = "none"
	customBody["unverified_confirmed"] = false
	st, body := scDoReq(t, ts, http.MethodPost, "/api/supplychain/entries", customBody, token)
	if st != http.StatusCreated {
		t.Fatalf("create custom: got %d, want 201 (%s)", st, body)
	}
	var customCreated scEntryResponse
	if err := json.Unmarshal(body, &customCreated); err != nil {
		t.Fatalf("decode custom create: %v", err)
	}
	st, out = verify(customCreated.SupplychainEntry.ID, map[string]any{"served_digest": scDigestA, "signature_status": "not_provided"})
	if st != http.StatusOK || out["allowed"] != false || out["reason"] != "unverified_unconfirmed" {
		t.Fatalf("verify custom unconfirmed: got %d %v, want reason=unverified_unconfirmed", st, out)
	}

	confirmedBody := scValidEntry("custom-confirmed")
	confirmedBody["source_kind"] = "custom"
	confirmedBody["signature_kind"] = "none"
	confirmedBody["unverified_confirmed"] = true
	st, body = scDoReq(t, ts, http.MethodPost, "/api/supplychain/entries", confirmedBody, token)
	if st != http.StatusCreated {
		t.Fatalf("create custom confirmed: got %d, want 201 (%s)", st, body)
	}
	var confirmedCreated scEntryResponse
	if err := json.Unmarshal(body, &confirmedCreated); err != nil {
		t.Fatalf("decode confirmed create: %v", err)
	}
	st, out = verify(confirmedCreated.SupplychainEntry.ID, map[string]any{"served_digest": scDigestA, "signature_status": "not_provided"})
	if st != http.StatusOK || out["allowed"] != true {
		t.Fatalf("verify custom confirmed: got %d %v, want allowed=true", st, out)
	}

	// Audit: refused verifies are recorded as supplychain.verify error rows
	// (mismatch, invalid served, failed, missing, unconfirmed = at least 5).
	st, body = getWithToken(t, ts.URL+"/api/audit-log?action=supplychain.verify&result=error", token)
	if st != http.StatusOK {
		t.Fatalf("audit-log verify errors: got %d, want 200", st)
	}
	var al struct {
		Entries []struct {
			Actor string `json:"actor"`
		} `json:"entries"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(body, &al); err != nil {
		t.Fatalf("decode audit-log: %v", err)
	}
	if al.Total < 5 {
		t.Errorf("supplychain.verify error audit rows: got %d, want >=5", al.Total)
	}
	for _, e := range al.Entries {
		if e.Actor != testAuthUser {
			t.Errorf("verify audit actor: got %q, want %q", e.Actor, testAuthUser)
		}
	}
}

// TestSupplychainRestartPersistence: an entry created on a file DB is still
// readable after the server restarts on the same file.
func TestSupplychainRestartPersistence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kelvios-sc.db")

	_, ts1 := newAuthTestServer(t, dbPath)
	setupAdmin(t, ts1)
	token1 := scLoginToken(t, ts1)
	st, created, _ := scCreate(t, ts1, token1, "persisted-app")
	if st != http.StatusCreated {
		t.Fatalf("create on server1: got %d, want 201", st)
	}
	id := created.SupplychainEntry.ID

	_, ts2 := newAuthTestServer(t, dbPath)
	token2 := scLoginToken(t, ts2)
	st, body := scDoReq(t, ts2, http.MethodGet, "/api/supplychain/entries/"+id, nil, token2)
	if st != http.StatusOK {
		t.Fatalf("get after restart: got %d, want 200 (%s)", st, body)
	}
	var got scEntryResponse
	if err := json.Unmarshal(body, &got); err != nil || got.SupplychainEntry.Name != "persisted-app" ||
		got.SupplychainEntry.PinnedDigest != scDigestA {
		t.Fatalf("restarted entry mismatch: %v %s", err, body)
	}
}
