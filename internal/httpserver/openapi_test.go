package httpserver

// KelviOS WS45-A: OpenAPI drift-guard tests — shape, coverage, operation quality,
// and no invented paths. Tests the live /api/openapi.json endpoint.

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestOpenAPIShapeAndAuth verifies the OpenAPI document structure and public access.
func TestOpenAPIShapeAndAuth(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	defer ts.Close()

	// GET /api/openapi.json WITHOUT a token → 200, JSON, parses
	resp, err := http.Get(ts.URL + "/api/openapi.json")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "application/json" {
		t.Fatalf("expected JSON content type, got %s", ct)
	}

	var spec map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil {
		t.Fatalf("decode OpenAPI spec: %v", err)
	}

	// openapi field starts with "3.1"
	openapiVer, ok := spec["openapi"].(string)
	if !ok {
		t.Fatalf("openapi field missing or not string")
	}
	if len(openapiVer) < 3 || openapiVer[:3] != "3.1" {
		t.Fatalf("openapi version %q does not start with 3.1", openapiVer)
	}

	// info.title is "KelviOS API"
	info, ok := spec["info"].(map[string]any)
	if !ok {
		t.Fatalf("info object missing")
	}
	title, ok := info["title"].(string)
	if !ok || title != "KelviOS API" {
		t.Fatalf("info.title = %q, want \"KelviOS API\"", title)
	}

	// components.securitySchemes contains a bearer HTTP scheme
	components, ok := spec["components"].(map[string]any)
	if !ok {
		t.Fatalf("components object missing")
	}
	secSchemes, ok := components["securitySchemes"].(map[string]any)
	if !ok {
		t.Fatalf("securitySchemes object missing")
	}
	bearerAuth, ok := secSchemes["bearerAuth"].(map[string]any)
	if !ok {
		t.Fatalf("bearerAuth security scheme missing")
	}
	if bearerAuth["type"] != "http" || bearerAuth["scheme"] != "bearer" {
		t.Fatalf("bearerAuth is not HTTP bearer: %+v", bearerAuth)
	}
}

// TestOpenAPICoverage verifies the curated path list is fully present and total count ≥ 60.
func TestOpenAPICoverage(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/openapi.json")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var spec map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil {
		t.Fatalf("decode OpenAPI spec: %v", err)
	}

	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatalf("paths object missing")
	}

	// Curated list — one per section (if any missing, document has drifted)
	// Paths match the actual OpenAPI spec paths (parameterized paths, not prefix handlers)
	curated := []string{
		"/api/openapi.json",
		"/api/disks",
		"/api/pools",
		"/api/collections/{id}",
		"/api/shares/{id}",
		"/api/daemon-config",
		"/api/audit-log",
		"/api/auth/login",
		"/api/vault/status",
		"/api/power",
		"/api/maintenance/status",
		"/api/supplychain/entries",
		"/api/ups/status",
		"/api/public/share/{token}",
		"/api/heat/recompute",
		"/api/curator/recommendations",
		"/api/curator/migrations",
		"/api/backups/repos",
		"/api/shadows/promotions",
		"/api/collections/{id}/arrange/plans",
		"/api/collections/{id}/arrange/plans/{planId}/approve",
		"/api/collections/{id}/tags/run",
		"/api/collections/{id}/tags/search",
		"/api/collections/{id}/tags",
		"/api/restore-tests/status",
		"/api/power/{disk}/wake-checklist/run",
		"/",
	}

	missing := []string{}
	for _, p := range curated {
		if _, exists := paths[p]; !exists {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("curated paths missing from spec: %v", missing)
	}

	// Total path count ≥ 60 (catches silent section drops)
	if len(paths) < 60 {
		t.Fatalf("total path count %d < 60", len(paths))
	}
}

// TestOpenAPIOperationQuality spot-checks operation metadata on curated paths.
func TestOpenAPIOperationQuality(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/openapi.json")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var spec map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil {
		t.Fatalf("decode OpenAPI spec: %v", err)
	}

	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatalf("paths object missing")
	}

	// Curated paths to spot-check
	curated := []string{
		"/api/openapi.json",
		"/api/disks",
		"/api/pools",
		"/api/collections/{id}",
		"/api/shares/{id}",
		"/api/daemon-config",
		"/api/audit-log",
		"/api/auth/login",
		"/api/vault/status",
		"/api/power",
		"/api/maintenance/status",
		"/api/supplychain/entries",
		"/api/ups/status",
		"/api/public/share/{token}",
		"/api/heat/recompute",
		"/api/curator/recommendations",
		"/api/curator/migrations",
		"/api/backups/repos",
		"/api/shadows/promotions",
		"/api/collections/{id}/arrange/plans",
		"/api/collections/{id}/arrange/plans/{planId}/approve",
		"/api/collections/{id}/tags/run",
		"/api/collections/{id}/tags/search",
		"/api/collections/{id}/tags",
		"/api/restore-tests/status",
		"/api/power/{disk}/wake-checklist/run",
		"/",
	}

	for _, path := range curated {
		pathItem, ok := paths[path].(map[string]any)
		if !ok {
			t.Errorf("%s: path item not an object", path)
			continue
		}

		// Every path must have at least one operation with non-empty summary,
		// operationId, and a responses map
		hasValidOp := false
		for method, opRaw := range pathItem {
			// Skip non-operation keys (parameters, etc.)
			if method == "parameters" {
				continue
			}
			op, ok := opRaw.(map[string]any)
			if !ok {
				continue
			}

			summary, _ := op["summary"].(string)
			opID, _ := op["operationId"].(string)
			responses, hasResp := op["responses"].(map[string]any)

			if summary != "" && opID != "" && hasResp && len(responses) > 0 {
				hasValidOp = true
				break
			}
		}
		if !hasValidOp {
			t.Errorf("%s: no operation with non-empty summary, operationId, and responses", path)
		}
	}

	// Bearer-authed paths carry a security requirement (spot-check two)
	bearerPaths := []string{
		"/api/pools",
		"/api/collections/{id}/tags/run",
	}
	for _, path := range bearerPaths {
		pathItem, ok := paths[path].(map[string]any)
		if !ok {
			t.Errorf("%s: path item missing", path)
			continue
		}
		hasBearerSec := false
		for _, opRaw := range pathItem {
			op, ok := opRaw.(map[string]any)
			if !ok {
				continue
			}
			sec, ok := op["security"].([]any)
			if !ok || len(sec) == 0 {
				continue
			}
			for _, reqRaw := range sec {
				req, ok := reqRaw.(map[string]any)
				if !ok {
					continue
				}
				if _, hasBearer := req["bearerAuth"]; hasBearer {
					hasBearerSec = true
					break
				}
			}
			if hasBearerSec {
				break
			}
		}
		if !hasBearerSec {
			t.Errorf("%s: missing bearerAuth security requirement", path)
		}
	}

	// /api/openapi.json and / carry NO security requirement
	noAuthPaths := []string{"/api/openapi.json", "/"}
	for _, path := range noAuthPaths {
		pathItem, ok := paths[path].(map[string]any)
		if !ok {
			t.Errorf("%s: path item missing", path)
			continue
		}
		for _, opRaw := range pathItem {
			op, ok := opRaw.(map[string]any)
			if !ok {
				continue
			}
			sec, ok := op["security"].([]any)
			if !ok || len(sec) == 0 {
				// Empty or missing security = no auth required (correct)
				continue
			}
			// If security array exists, it must be [{}] (empty requirement)
			for _, reqRaw := range sec {
				req, ok := reqRaw.(map[string]any)
				if !ok || len(req) != 0 {
					t.Errorf("%s: unexpected security requirement %+v", path, req)
				}
			}
		}
	}
}

// TestOpenAPINoInventedPaths verifies known-absent paths are not present.
func TestOpenAPINoInventedPaths(t *testing.T) {
	_, ts := newAuthTestServer(t, ":memory:")
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/openapi.json")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	var spec map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil {
		t.Fatalf("decode OpenAPI spec: %v", err)
	}

	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatalf("paths object missing")
	}

	absent := []string{
		"/api/nonexistent",
		"/api/pools/{id}/teleport",
	}
	for _, p := range absent {
		if _, exists := paths[p]; exists {
			t.Errorf("known-absent path %q found in spec", p)
		}
	}
}
