package httpserver

import (
	"database/sql"
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

func TestDiskOpsEndpoints(t *testing.T) {
	// Each subtest gets its own fresh DB to avoid cross-test pollution
	testCases := []struct {
		name string
		run  func(t *testing.T, db *sql.DB, ts *httptest.Server, token, poolID string)
	}{
		{
			name: "plan_replace_single_success",
			run: func(t *testing.T, db *sql.DB, ts *httptest.Server, token, poolID string) {
				resp := doReq(t, ts, token, "POST", "/api/pools/"+poolID+"/disk-replace/plan", map[string]any{
					"failing_disk":     "sda",
					"replacement_disk": "sdc",
				})
				if resp.StatusCode != http.StatusOK {
					b, _ := io.ReadAll(resp.Body)
					t.Fatalf("POST disk-replace/plan: expected 200, got %d: %s", resp.StatusCode, string(b))
				}
				var body struct {
					Plan map[string]any `json:"plan"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				resp.Body.Close()
				if body.Plan["strategy"] != "copy-out" {
					t.Errorf("strategy: got %v, want copy-out", body.Plan["strategy"])
				}
			},
		},
		{
			name: "plan_expand_success",
			run: func(t *testing.T, db *sql.DB, ts *httptest.Server, token, poolID string) {
				resp := doReq(t, ts, token, "POST", "/api/pools/"+poolID+"/disk-expand/plan", map[string]any{
					"new_disk": "sdc",
				})
				if resp.StatusCode != http.StatusOK {
					b, _ := io.ReadAll(resp.Body)
					t.Fatalf("POST disk-expand/plan: expected 200, got %d: %s", resp.StatusCode, string(b))
				}
				var body struct {
					Plan map[string]any `json:"plan"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				resp.Body.Close()
				if body.Plan["strategy"] != "copy-out" {
					t.Errorf("strategy: got %v, want copy-out", body.Plan["strategy"])
				}
			},
		},
		{
			name: "plan_replace_failing_disk_not_in_pool_400",
			run: func(t *testing.T, db *sql.DB, ts *httptest.Server, token, poolID string) {
				resp := doReq(t, ts, token, "POST", "/api/pools/"+poolID+"/disk-replace/plan", map[string]any{
					"failing_disk":     "sdx",
					"replacement_disk": "sdc",
				})
				if resp.StatusCode != http.StatusBadRequest {
					b, _ := io.ReadAll(resp.Body)
					t.Fatalf("expected 400 for failing disk not in pool, got %d: %s", resp.StatusCode, string(b))
				}
				resp.Body.Close()
			},
		},
		{
			name: "plan_replace_replacement_already_in_pool_400",
			run: func(t *testing.T, db *sql.DB, ts *httptest.Server, token, poolID string) {
				resp := doReq(t, ts, token, "POST", "/api/pools/"+poolID+"/disk-replace/plan", map[string]any{
					"failing_disk":     "sda",
					"replacement_disk": "sda",
				})
				if resp.StatusCode != http.StatusBadRequest {
					b, _ := io.ReadAll(resp.Body)
					t.Fatalf("expected 400 for replacement already in pool, got %d: %s", resp.StatusCode, string(b))
				}
				resp.Body.Close()
			},
		},
		{
			name: "plan_expand_new_disk_already_in_pool_400",
			run: func(t *testing.T, db *sql.DB, ts *httptest.Server, token, poolID string) {
				resp := doReq(t, ts, token, "POST", "/api/pools/"+poolID+"/disk-expand/plan", map[string]any{
					"new_disk": "sda",
				})
				if resp.StatusCode != http.StatusBadRequest {
					b, _ := io.ReadAll(resp.Body)
					t.Fatalf("expected 400 for new disk already in pool, got %d: %s", resp.StatusCode, string(b))
				}
				resp.Body.Close()
			},
		},
		{
			name: "unauthenticated_401",
			run: func(t *testing.T, db *sql.DB, ts *httptest.Server, token, poolID string) {
				req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/pools/"+poolID+"/disk-replace/plan", nil)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("request failed: %v", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("expected 401, got %d", resp.StatusCode)
				}
			},
		},
		{
			name: "pool_not_found_404",
			run: func(t *testing.T, db *sql.DB, ts *httptest.Server, token, poolID string) {
				resp := doReq(t, ts, token, "POST", "/api/pools/nonexistent/disk-replace/plan", map[string]any{
					"failing_disk":     "sda",
					"replacement_disk": "sdc",
				})
				if resp.StatusCode != http.StatusNotFound {
					b, _ := io.ReadAll(resp.Body)
					t.Fatalf("expected 404 for unknown pool, got %d: %s", resp.StatusCode, string(b))
				}
				resp.Body.Close()
			},
		},
		{
			name: "method_not_allowed_405",
			run: func(t *testing.T, db *sql.DB, ts *httptest.Server, token, poolID string) {
				req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/pools/"+poolID+"/disk-replace/plan", nil)
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("request failed: %v", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusMethodNotAllowed {
					t.Fatalf("expected 405, got %d", resp.StatusCode)
				}
			},
		},
		{
			name: "list_disk_operations_empty",
			run: func(t *testing.T, db *sql.DB, ts *httptest.Server, token, poolID string) {
				resp := doReq(t, ts, token, "GET", "/api/pools/"+poolID+"/disk-operations", nil)
				if resp.StatusCode != http.StatusOK {
					b, _ := io.ReadAll(resp.Body)
					t.Fatalf("GET disk-operations: expected 200, got %d: %s", resp.StatusCode, string(b))
				}
				var body struct {
					Operations []map[string]any `json:"operations"`
					Total      int              `json:"total"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				resp.Body.Close()
				if body.Total != 0 {
					t.Errorf("total: got %d, want 0", body.Total)
				}
			},
		},
		{
			name: "list_disk_operations_after_plan",
			run: func(t *testing.T, db *sql.DB, ts *httptest.Server, token, poolID string) {
				// Create a plan first
				resp := doReq(t, ts, token, "POST", "/api/pools/"+poolID+"/disk-replace/plan", map[string]any{
					"failing_disk":     "sda",
					"replacement_disk": "sdc",
				})
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("setup plan: expected 200, got %d", resp.StatusCode)
				}
				resp.Body.Close()

				// List operations
				resp = doReq(t, ts, token, "GET", "/api/pools/"+poolID+"/disk-operations", nil)
				if resp.StatusCode != http.StatusOK {
					b, _ := io.ReadAll(resp.Body)
					t.Fatalf("GET disk-operations: expected 200, got %d: %s", resp.StatusCode, string(b))
				}
				var body struct {
					Operations []map[string]any `json:"operations"`
					Total      int              `json:"total"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				resp.Body.Close()
				if body.Total < 1 {
					t.Errorf("total: got %d, want >=1", body.Total)
				}
				if len(body.Operations) == 0 {
					t.Error("operations: expected non-empty list")
				}
				// Check status is "planned"
				if body.Operations[0]["status"] != "planned" {
					t.Errorf("status: got %v, want planned", body.Operations[0]["status"])
				}
			},
		},
		{
			name: "audit_does_not_leak_disk_content_on_ok",
			run: func(t *testing.T, db *sql.DB, ts *httptest.Server, token, poolID string) {
				// Create a plan
				resp := doReq(t, ts, token, "POST", "/api/pools/"+poolID+"/disk-replace/plan", map[string]any{
					"failing_disk":     "sda",
					"replacement_disk": "sdc",
				})
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("setup plan: expected 200, got %d", resp.StatusCode)
				}
				resp.Body.Close()

				// Check audit log - OK entries should NOT contain disk names or paths
				req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/audit-log?action=diskops.plan&result=ok", nil)
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatalf("request failed: %v", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("GET audit-log: expected 200, got %d", resp.StatusCode)
				}
				var al struct {
					Entries []map[string]any `json:"entries"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&al); err != nil {
					t.Fatalf("decode audit log: %v", err)
				}
				for _, e := range al.Entries {
					detail, _ := e["detail"].(string)
					if strings.Contains(detail, "sda") || strings.Contains(detail, "sdc") || strings.Contains(detail, "sdb") {
						t.Errorf("audit detail leaks disk name: %q", detail)
					}
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := catalog.Open(":memory:")
			if err != nil {
				t.Fatalf("open catalog: %v", err)
			}
			defer db.Close()

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
					Path: ":memory:",
				},
			}

			disks := []storage.Disk{
				{Name: "sda", Path: "/dev/sda", Model: "WDC WD40EFRX", Serial: "WD-TEST", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
				{Name: "sdb", Path: "/dev/sdb", Model: "WDC WD40EFRX", Serial: "WD-TEST2", SizeBytes: 4000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
				{Name: "sdc", Path: "/dev/sdc", Model: "WDC WD40EFRX", Serial: "WD-TEST3", SizeBytes: 6000787030016, Rotational: true, Mounted: false, PowerState: storage.PowerUnknown, NodeID: storage.DefaultNodeID},
			}
			driver := storage.NewSingleDriver(disks)

			s := New(cfg, db, "test-version")
			s.SetDriver(driver)
			s.disksFunc = func() ([]storage.Disk, error) { return disks, nil }

			ts := httptest.NewServer(s.srv.Handler)
			token := mustToken(t, ts)
			defer ts.Close()

			// Create a pool (single mechanism, 1 disk)
			resp := doReq(t, ts, token, "POST", "/api/pools", map[string]any{
				"name":       "media",
				"tier":       "warm",
				"mechanism":  "single",
				"disks":      []string{"sda"},
				"mountpoint": "/mnt/media",
			})
			if resp.StatusCode != http.StatusCreated {
				b, _ := io.ReadAll(resp.Body)
				t.Fatalf("POST pool: expected 201, got %d: %s", resp.StatusCode, string(b))
			}
			var poolResp struct {
				Pool struct {
					ID string `json:"id"`
				} `json:"pool"`
			}
			json.NewDecoder(resp.Body).Decode(&poolResp)
			resp.Body.Close()
			poolID := poolResp.Pool.ID

			tc.run(t, db, ts, token, poolID)
		})
	}
}

func doReq(t *testing.T, ts *httptest.Server, token, method, path string, payload any) *http.Response {
	t.Helper()
	var body io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		body = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, ts.URL+path, body)
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
	return resp
}
