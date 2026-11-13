package httpserver

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/config"
	"github.com/fauzone/kelvios/internal/power"
	"github.com/fauzone/kelvios/internal/storage"
	_ "modernc.org/sqlite"
)

// testServer creates a test server with in-memory database and fake clock.
func testServer(t *testing.T) (*Server, *sql.DB, *power.FakeClock, string) {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	err = catalog.Migrate(db)
	if err != nil {
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
		UPS:      config.UPSConfig{},
	}

	s := New(cfg, db, "test")
	fakeClock := &power.FakeClock{NowTime: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)}
	s.SetPowerEngine(power.New(fakeClock, nil, 0))

	// Stub disk inventory to return our test disk
	s.SetDisksFunc(func() ([]storage.Disk, error) {
		return []storage.Disk{{Name: "sda", Path: "/dev/sda", SizeBytes: 1000000000}}, nil
	})

	// Create a test pool and disk for power operations
	pool := &catalog.PoolRow{
		ID:         "pool1",
		Name:       "testpool",
		NodeID:     "local",
		Tier:       "warm",
		Mechanism:  "single",
		Driver:     "single",
		Mountpoint: "/mnt/testpool",
		SizeBytes:  1000000000,
		UsedBytes:  0,
		Encrypted:  false,
		Status:     "active",
	}
	err = catalog.CreatePool(db, pool, []string{"sda"})
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}

	// Register with power engine
	s.power.RegisterPool("pool1", storage.TierWarm, "testpool")
	s.power.RegisterDisk("sda", "pool1")

	// Create test user and session for authentication
	token := "test-token-12345"
	tokenHash := sha256Hex(token)
	now := time.Now().Unix()
	_, err = db.Exec(`INSERT INTO accounts (id, username, password_hash, failed_count, locked_until, created_at) VALUES (?, ?, ?, 0, 0, ?)`,
		"acct-test", "testuser", "dummy-hash", now)
	if err != nil {
		t.Fatalf("create test account: %v", err)
	}
	_, err = db.Exec(`INSERT INTO sessions (token_hash, username, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, "testuser", now, now+3600)
	if err != nil {
		t.Fatalf("create test session: %v", err)
	}

	return s, db, fakeClock, token
}

// authRequest adds Bearer token authentication.
func authRequest(req *http.Request) {
	req.Header.Set("Authorization", "Bearer test-token-12345")
}

// TestPowerSchedulesCreate tests POST /api/power/{disk}/schedules.
func TestPowerSchedulesCreate(t *testing.T) {
	s, db, _, _ := testServer(t)
	defer db.Close()

	tests := []struct {
		name           string
		body           string
		expectedStatus int
		checkResponse  func(t *testing.T, body map[string]any)
	}{
		{
			name: "valid create returns 201",
			body: `{"days":"Mon,Tue,Wed","start_min":540,"end_min":1020}`,
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body map[string]any) {
				sch := body["schedule"].(map[string]any)
				if sch["disk"] != "sda" {
					t.Errorf("expected disk sda, got %v", sch["disk"])
				}
				if sch["days"] != "Mon,Tue,Wed" {
					t.Errorf("expected days Mon,Tue,Wed, got %v", sch["days"])
				}
				if sch["start_min"] != float64(540) {
					t.Errorf("expected start_min 540, got %v", sch["start_min"])
				}
				if sch["end_min"] != float64(1020) {
					t.Errorf("expected end_min 1020, got %v", sch["end_min"])
				}
				if sch["enabled"] != true {
					t.Error("expected enabled=true")
				}
				if sch["id"] == nil || sch["id"] == "" {
					t.Error("expected schedule ID")
				}
			},
		},
		{
			name:           "duplicate returns 409",
			body:           `{"days":"Mon,Tue,Wed","start_min":540,"end_min":1020}`,
			expectedStatus: http.StatusConflict,
		},
		{
			name:           "empty days returns 400",
			body:           `{"days":"","start_min":540,"end_min":1020}`,
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "start equals end returns 400",
			body:           `{"days":"Mon","start_min":540,"end_min":540}`,
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "invalid weekday returns 400",
			body:           `{"days":"Mon,Invalid","start_min":540,"end_min":1020}`,
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "start out of range returns 400",
			body:           `{"days":"Mon","start_min":-1,"end_min":1020}`,
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "end out of range returns 400",
			body:           `{"days":"Mon","start_min":540,"end_min":1440}`,
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "unknown disk returns 404",
			body:           `{"days":"Mon","start_min":540,"end_min":1020}`,
			expectedStatus: http.StatusNotFound,
			// Use different disk
		},
		{
			name:           "unauthenticated returns 401",
			body:           `{"days":"Mon","start_min":540,"end_min":1020}`,
			expectedStatus: http.StatusUnauthorized,
			// No auth cookie
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			if tt.name == "unknown disk returns 404" {
				req = httptest.NewRequest(http.MethodPost, "/api/power/sdb/schedules", strings.NewReader(tt.body))
			} else {
				req = httptest.NewRequest(http.MethodPost, "/api/power/sda/schedules", strings.NewReader(tt.body))
			}
			req.Header.Set("Content-Type", "application/json")

			if tt.name != "unauthenticated returns 401" {
				authRequest(req)
			}

			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d: %s", tt.expectedStatus, w.Code, w.Body.String())
				return
			}

			if tt.checkResponse != nil {
				var body map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("unmarshal response: %v", err)
				}
				tt.checkResponse(t, body)
			}
		})
	}
}

// TestPowerSchedulesList tests GET /api/power/{disk}/schedules.
func TestPowerSchedulesList(t *testing.T) {
	s, db, _, _ := testServer(t)
	defer db.Close()

	// Create a schedule first
	_, err := catalog.CreatePowerSchedule(db, "sda", "Mon,Tue,Wed", 540, 1020)
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/power/sda/schedules", nil)
	authRequest(req)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
		return
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	schedules := body["schedules"].([]any)
	if len(schedules) != 1 {
		t.Errorf("expected 1 schedule, got %d", len(schedules))
	}
	sch := schedules[0].(map[string]any)
	if sch["disk"] != "sda" {
		t.Errorf("expected disk sda, got %v", sch["disk"])
	}
	if sch["days"] != "Mon,Tue,Wed" {
		t.Errorf("expected days Mon,Tue,Wed, got %v", sch["days"])
	}
}

// TestPowerSchedulesListEmpty tests list returns empty array for disk with no schedules.
func TestPowerSchedulesListEmpty(t *testing.T) {
	s, db, _, _ := testServer(t)
	defer db.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/power/sda/schedules", nil)
	authRequest(req)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
		return
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	schedules := body["schedules"].([]any)
	if len(schedules) != 0 {
		t.Errorf("expected 0 schedules, got %d", len(schedules))
	}
}

// TestPowerSchedulesListUnknownDisk tests list returns 404 for unknown disk.
func TestPowerSchedulesListUnknownDisk(t *testing.T) {
	s, db, _, _ := testServer(t)
	defer db.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/power/unknown/schedules", nil)
	authRequest(req)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPowerSchedulesPatch tests PATCH /api/power/{disk}/schedules/{id}.
func TestPowerSchedulesPatch(t *testing.T) {
	s, db, _, _ := testServer(t)
	defer db.Close()

	// Create a schedule
	sch, err := catalog.CreatePowerSchedule(db, "sda", "Mon", 540, 1020)
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	tests := []struct {
		name           string
		body           string
		expectedStatus int
		checkResponse  func(t *testing.T, body map[string]any)
	}{
		{
			name:           "disable returns 200",
			body:           `{"enabled":false}`,
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body map[string]any) {
				sch := body["schedule"].(map[string]any)
				if sch["enabled"] != false {
					t.Error("expected enabled=false")
				}
			},
		},
		{
			name:           "enable returns 200",
			body:           `{"enabled":true}`,
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body map[string]any) {
				sch := body["schedule"].(map[string]any)
				if sch["enabled"] != true {
					t.Error("expected enabled=true")
				}
			},
		},
		{
			name:           "invalid JSON returns 400",
			body:           `not json`,
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "unknown schedule returns 404",
			body:           `{"enabled":true}`,
			expectedStatus: http.StatusNotFound,
			// Use different schedule ID
		},
		{
			name:           "unknown disk returns 404",
			body:           `{"enabled":true}`,
			expectedStatus: http.StatusNotFound,
			// Use different disk
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *http.Request
			url := "/api/power/sda/schedules/" + sch.ID
			if tt.name == "unknown schedule returns 404" {
				url = "/api/power/sda/schedules/nonexistent"
			} else if tt.name == "unknown disk returns 404" {
				url = "/api/power/sdb/schedules/" + sch.ID
			}
			req = httptest.NewRequest(http.MethodPatch, url, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			authRequest(req)

			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d: %s", tt.expectedStatus, w.Code, w.Body.String())
				return
			}

			if tt.checkResponse != nil {
				var body map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				tt.checkResponse(t, body)
			}
		})
	}
}

// TestPowerSchedulesDelete tests DELETE /api/power/{disk}/schedules/{id}.
func TestPowerSchedulesDelete(t *testing.T) {
	s, db, _, _ := testServer(t)
	defer db.Close()

	// Create a schedule
	sch, err := catalog.CreatePowerSchedule(db, "sda", "Mon", 540, 1020)
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}

	// Delete it
	req := httptest.NewRequest(http.MethodDelete, "/api/power/sda/schedules/"+sch.ID, nil)
	authRequest(req)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", w.Code, w.Body.String())
	}

	// Verify it's gone from list
	req = httptest.NewRequest(http.MethodGet, "/api/power/sda/schedules", nil)
	authRequest(req)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	schedules := body["schedules"].([]any)
	if len(schedules) != 0 {
		t.Errorf("expected 0 schedules after delete, got %d", len(schedules))
	}

	// Delete missing returns 404
	req = httptest.NewRequest(http.MethodDelete, "/api/power/sda/schedules/"+sch.ID, nil)
	authRequest(req)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for delete missing, got %d", w.Code)
	}
}

// TestPowerSchedulesAuditLog tests that mutations write audit rows.
func TestPowerSchedulesAuditLog(t *testing.T) {
	s, db, _, _ := testServer(t)
	defer db.Close()

	// Create schedule
	req := httptest.NewRequest(http.MethodPost, "/api/power/sda/schedules", strings.NewReader(`{"days":"Mon","start_min":540,"end_min":1020}`))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", w.Code, w.Body.String())
	}

	var createBody map[string]any
	json.Unmarshal(w.Body.Bytes(), &createBody)
	schID := createBody["schedule"].(map[string]any)["id"].(string)

	// Check audit log for create
	entries, _, err := catalog.ListAuditLog(db, 10, 0, "power.schedule.create", "", "")
	if err != nil {
		t.Fatalf("list audit log: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected audit log entry for create")
	}
	if entries[0].Result != "ok" {
		t.Errorf("expected audit result ok, got %s", entries[0].Result)
	}
	if !strings.Contains(entries[0].Detail, schID) {
		t.Errorf("audit detail should contain schedule ID: %s", entries[0].Detail)
	}

	// Patch schedule
	req = httptest.NewRequest(http.MethodPatch, "/api/power/sda/schedules/"+schID, strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("patch failed: %d %s", w.Code, w.Body.String())
	}

	// Check audit log for patch
	entries, _, err = catalog.ListAuditLog(db, 10, 0, "power.schedule.update", "", "")
	if err != nil {
		t.Fatalf("list audit log: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected audit log entry for patch")
	}
	if entries[0].Result != "ok" {
		t.Errorf("expected audit result ok, got %s", entries[0].Result)
	}

	// Delete schedule
	req = httptest.NewRequest(http.MethodDelete, "/api/power/sda/schedules/"+schID, nil)
	authRequest(req)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("delete failed: %d %s", w.Code, w.Body.String())
	}

	// Check audit log for delete
	entries, _, err = catalog.ListAuditLog(db, 10, 0, "power.schedule.delete", "", "")
	if err != nil {
		t.Fatalf("list audit log: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected audit log entry for delete")
	}
	if entries[0].Result != "ok" {
		t.Errorf("expected audit result ok, got %s", entries[0].Result)
	}
}

// TestPowerSchedulesCrossMidnight tests that cross-midnight schedules are accepted.
func TestPowerSchedulesCrossMidnight(t *testing.T) {
	s, db, _, _ := testServer(t)
	defer db.Close()

	// 22:00 (1320) to 02:00 (120)
	req := httptest.NewRequest(http.MethodPost, "/api/power/sda/schedules", strings.NewReader(`{"days":"Mon,Tue,Wed,Thu,Fri","start_min":1320,"end_min":120}`))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201 for cross-midnight, got %d: %s", w.Code, w.Body.String())
	}

	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	sch := body["schedule"].(map[string]any)
	if sch["start_min"] != float64(1320) || sch["end_min"] != float64(120) {
		t.Errorf("expected start=1320 end=120, got start=%v end=%v", sch["start_min"], sch["end_min"])
	}
}

// TestPowerSchedulesDifferentCaseDays tests that days are normalized.
func TestPowerSchedulesDifferentCaseDays(t *testing.T) {
	s, db, _, _ := testServer(t)
	defer db.Close()

	// Create with unsorted days
	req := httptest.NewRequest(http.MethodPost, "/api/power/sda/schedules", strings.NewReader(`{"days":"Wed,Mon,Mon,Tue","start_min":540,"end_min":1020}`))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	sch := body["schedule"].(map[string]any)
	if sch["days"] != "Mon,Tue,Wed" {
		t.Errorf("expected normalized days Mon,Tue,Wed, got %v", sch["days"])
	}

	// Creating with same days in different order should be duplicate (409)
	req = httptest.NewRequest(http.MethodPost, "/api/power/sda/schedules", strings.NewReader(`{"days":"Tue,Wed,Mon","start_min":540,"end_min":1020}`))
	req.Header.Set("Content-Type", "application/json")
	authRequest(req)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for duplicate, got %d: %s", w.Code, w.Body.String())
	}
}

// TestPowerWake tests POST /api/power/{disk}/wake.
func TestPowerWake(t *testing.T) {
	s, db, _, _ := testServer(t)
	defer db.Close()

	tests := []struct {
		name           string
		disk           string
		auth           bool
		expectedStatus int
		checkAudit     bool
		auditAction    string
		auditResult    string
	} {
		{
			name:           "success returns 200 and audit ok",
			disk:           "sda",
			auth:           true,
			expectedStatus: http.StatusOK,
			checkAudit:     true,
			auditAction:    "power.wake",
			auditResult:    "ok",
		},
		{
			name:           "unknown disk returns 404 and audit error",
			disk:           "sdb",
			auth:           true,
			expectedStatus: http.StatusNotFound,
			checkAudit:     true,
			auditAction:    "power.wake",
			auditResult:    "error",
		},
		{
			name:           "unauthenticated returns 401",
			disk:           "sda",
			auth:           false,
			expectedStatus: http.StatusUnauthorized,
			checkAudit:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/power/"+tt.disk+"/wake", nil)
			if tt.auth {
				authRequest(req)
			}

			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d: %s", tt.expectedStatus, w.Code, w.Body.String())
				return
			}

			if tt.checkAudit {
				entries, _, err := catalog.ListAuditLog(db, 10, 0, tt.auditAction, "", "")
				if err != nil {
					t.Fatalf("list audit log: %v", err)
				}
				if len(entries) == 0 {
					t.Errorf("expected audit log entry for %s", tt.auditAction)
					return
				}
				if entries[0].Result != tt.auditResult {
					t.Errorf("expected audit result %s, got %s", tt.auditResult, entries[0].Result)
				}
				if entries[0].Actor != "testuser" && tt.auth {
					t.Errorf("expected actor testuser, got %s", entries[0].Actor)
				}
				if entries[0].TargetID != tt.disk {
					t.Errorf("expected target %s, got %s", tt.disk, entries[0].TargetID)
				}
			}
		})
	}
}

// TestPowerSleep tests POST /api/power/{disk}/sleep.
func TestPowerSleep(t *testing.T) {
	s, db, _, _ := testServer(t)
	defer db.Close()

	tests := []struct {
		name           string
		disk           string
		auth           bool
		expectedStatus int
		checkAudit     bool
		auditAction    string
		auditResult    string
	} {
		{
			name:           "success warm disk returns 200 standby and audit ok",
			disk:           "sda", // warm tier via testServer helper
			auth:           true,
			expectedStatus: http.StatusOK,
			checkAudit:     true,
			auditAction:    "power.sleep",
			auditResult:    "ok",
		},
		{
			name:           "unknown disk returns 404 and audit error",
			disk:           "sdb",
			auth:           true,
			expectedStatus: http.StatusNotFound,
			checkAudit:     true,
			auditAction:    "power.sleep",
			auditResult:    "error",
		},
		{
			name:           "unauthenticated returns 401",
			disk:           "sda",
			auth:           false,
			expectedStatus: http.StatusUnauthorized,
			checkAudit:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/power/"+tt.disk+"/sleep", nil)
			if tt.auth {
				authRequest(req)
			}

			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d: %s", tt.expectedStatus, w.Code, w.Body.String())
				return
			}

			if tt.checkAudit {
				entries, _, err := catalog.ListAuditLog(db, 10, 0, tt.auditAction, "", "")
				if err != nil {
					t.Fatalf("list audit log: %v", err)
				}
				if len(entries) == 0 {
					t.Errorf("expected audit log entry for %s", tt.auditAction)
					return
				}
				if entries[0].Result != tt.auditResult {
					t.Errorf("expected audit result %s, got %s", tt.auditResult, entries[0].Result)
				}
				if entries[0].Actor != "testuser" && tt.auth {
					t.Errorf("expected actor testuser, got %s", entries[0].Actor)
				}
				if entries[0].TargetID != tt.disk {
					t.Errorf("expected target %s, got %s", tt.disk, entries[0].TargetID)
				}
			}
		})
	}

	// Note: Hot tier (409 ErrHotNeverSleeps) and unpooled disk (409 ErrUnpooled)
	// cannot be tested via testServer helper which only seeds a warm-tier pooled disk.
	// These require separate test setup with hot-tier pool or unpooled disk registration.
}
