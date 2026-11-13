package compose

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/chatbridge"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Satu koneksi saja: :memory: modernc terpecah per koneksi (pola catalog.Open).
	db.SetMaxOpenConns(1)
	if err := catalog.Migrate(db); err != nil {
		db.Close()
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestCreateArtifact(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	spec := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":24}`)
	now := time.Unix(1700000000, 0)

	artifact, err := CreateArtifact(db, "daily-report", KindReportSchedule, spec, "test-user", now)
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}

	if artifact.ID == "" {
		t.Error("artifact ID should not be empty")
	}
	if artifact.Name != "daily-report" {
		t.Errorf("name: got %s, want daily-report", artifact.Name)
	}
	if artifact.Kind != KindReportSchedule {
		t.Errorf("kind: got %s, want %s", artifact.Kind, KindReportSchedule)
	}
	if artifact.Status != StatusPending {
		t.Errorf("status: got %s, want %s", artifact.Status, StatusPending)
	}
	if artifact.CurrentVersion != 1 {
		t.Errorf("current_version: got %d, want 1", artifact.CurrentVersion)
	}
	if string(artifact.Spec) != string(spec) {
		t.Errorf("spec mismatch: got %s, want %s", artifact.Spec, spec)
	}
	if artifact.CreatedBy != "test-user" {
		t.Errorf("created_by: got %s, want test-user", artifact.CreatedBy)
	}
	if artifact.CreatedAt != now.Unix() {
		t.Errorf("created_at: got %d, want %d", artifact.CreatedAt, now.Unix())
	}
	if artifact.UpdatedAt != now.Unix() {
		t.Errorf("updated_at: got %d, want %d", artifact.UpdatedAt, now.Unix())
	}

	// Verify version 1 was inserted
	versions, err := ListVersions(db, artifact.ID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 1 {
		t.Errorf("expected 1 version, got %d", len(versions))
	}
	if versions[0].Version != 1 {
		t.Errorf("version: got %d, want 1", versions[0].Version)
	}
	if string(versions[0].Spec) != string(spec) {
		t.Errorf("version spec mismatch")
	}
}

func TestCreateArtifactValidation(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Empty name
	_, err := CreateArtifact(db, "", KindReportSchedule, json.RawMessage(`{}`), "user", time.Now())
	if err == nil {
		t.Error("expected error for empty name")
	}

	// Empty kind
	_, err = CreateArtifact(db, "test", "", json.RawMessage(`{}`), "user", time.Now())
	if err == nil {
		t.Error("expected error for empty kind")
	}
}

func TestGetArtifact(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	spec := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":24}`)
	now := time.Unix(1700000000, 0)

	created, err := CreateArtifact(db, "test", KindReportSchedule, spec, "user", now)
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}

	// Get existing
	got, err := GetArtifact(db, created.ID)
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if got == nil {
		t.Fatal("GetArtifact returned nil for existing artifact")
	}
	if got.ID != created.ID {
		t.Errorf("ID mismatch: got %s, want %s", got.ID, created.ID)
	}

	// Get non-existent
	got, err = GetArtifact(db, "nonexistent")
	if err != nil {
		t.Fatalf("GetArtifact nonexistent: %v", err)
	}
	if got != nil {
		t.Error("GetArtifact should return nil for non-existent artifact")
	}
}

func TestListArtifacts(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	now := time.Unix(1700000000, 0)
	spec1 := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":24}`)
	spec2 := json.RawMessage(`{"trigger":{"type":"schedule","interval_hours":1},"action":{"type":"send_report","transport":"telegram","chat_id":"123"}}`)

	_, err := CreateArtifact(db, "report-1", KindReportSchedule, spec1, "user", now)
	if err != nil {
		t.Fatalf("CreateArtifact 1: %v", err)
	}
	_, err = CreateArtifact(db, "rule-1", KindAutomationRule, spec2, "user", now.Add(time.Second))
	if err != nil {
		t.Fatalf("CreateArtifact 2: %v", err)
	}

	// List all
	all, err := ListArtifacts(db, "", "")
	if err != nil {
		t.Fatalf("ListArtifacts all: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 artifacts, got %d", len(all))
	}

	// Filter by kind
	reports, err := ListArtifacts(db, "", KindReportSchedule)
	if err != nil {
		t.Fatalf("ListArtifacts kind: %v", err)
	}
	if len(reports) != 1 || reports[0].Kind != KindReportSchedule {
		t.Errorf("kind filter failed: got %d artifacts", len(reports))
	}

	// Filter by status
	pending, err := ListArtifacts(db, StatusPending, "")
	if err != nil {
		t.Fatalf("ListArtifacts status: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("status filter failed: got %d artifacts", len(pending))
	}

	// Combined filter
	activeReports, err := ListArtifacts(db, StatusActive, KindReportSchedule)
	if err != nil {
		t.Fatalf("ListArtifacts combined: %v", err)
	}
	if len(activeReports) != 0 {
		t.Errorf("combined filter should return 0, got %d", len(activeReports))
	}
}

func TestListVersions(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	spec1 := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":24}`)
	spec2 := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":12}`)
	now := time.Unix(1700000000, 0)

	artifact, err := CreateArtifact(db, "test", KindReportSchedule, spec1, "user", now)
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}

	// Update spec to create version 2
	updated, err := UpdateSpec(db, artifact.ID, spec2, "user", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("UpdateSpec: %v", err)
	}
	if updated.CurrentVersion != 2 {
		t.Errorf("current_version after update: got %d, want 2", updated.CurrentVersion)
	}

	versions, err := ListVersions(db, artifact.ID)
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Errorf("expected 2 versions, got %d", len(versions))
	}
	if versions[0].Version != 1 || versions[1].Version != 2 {
		t.Errorf("versions not ordered: %d, %d", versions[0].Version, versions[1].Version)
	}
	if string(versions[0].Spec) != string(spec1) {
		t.Errorf("version 1 spec mismatch")
	}
	if string(versions[1].Spec) != string(spec2) {
		t.Errorf("version 2 spec mismatch")
	}
}

func TestUpdateSpec(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	spec1 := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":24}`)
	spec2 := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":12}`)
	now := time.Unix(1700000000, 0)

	artifact, err := CreateArtifact(db, "test", KindReportSchedule, spec1, "user", now)
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}

	updated, err := UpdateSpec(db, artifact.ID, spec2, "user", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("UpdateSpec: %v", err)
	}

	if updated.CurrentVersion != 2 {
		t.Errorf("current_version: got %d, want 2", updated.CurrentVersion)
	}
	if string(updated.Spec) != string(spec2) {
		t.Errorf("spec not updated")
	}
	if updated.UpdatedAt != now.Add(time.Hour).Unix() {
		t.Errorf("updated_at not updated")
	}

	// Verify version 1 still exists
	versions, _ := ListVersions(db, artifact.ID)
	if len(versions) != 2 {
		t.Errorf("expected 2 versions after update, got %d", len(versions))
	}
}

func TestUpdateSpecNotFound(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	spec := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":24}`)
	_, err := UpdateSpec(db, "nonexistent", spec, "user", time.Now())
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestSetStatus(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	spec := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":24}`)
	now := time.Unix(1700000000, 0)

	artifact, err := CreateArtifact(db, "test", KindReportSchedule, spec, "user", now)
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}

	// Activate
	updated, err := SetStatus(db, artifact.ID, StatusActive, "user", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("SetStatus activate: %v", err)
	}
	if updated.Status != StatusActive {
		t.Errorf("status: got %s, want %s", updated.Status, StatusActive)
	}
	if updated.UpdatedAt != now.Add(time.Hour).Unix() {
		t.Errorf("updated_at not updated on activate")
	}

	// Disable
	updated, err = SetStatus(db, artifact.ID, StatusDisabled, "user", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("SetStatus disable: %v", err)
	}
	if updated.Status != StatusDisabled {
		t.Errorf("status: got %s, want %s", updated.Status, StatusDisabled)
	}
}

func TestSetStatusNotFound(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	_, err := SetStatus(db, "nonexistent", StatusActive, "user", time.Now())
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestRollback(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	spec1 := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":24}`)
	spec2 := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":12}`)
	spec3 := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":6}`)
	now := time.Unix(1700000000, 0)

	artifact, err := CreateArtifact(db, "test", KindReportSchedule, spec1, "user", now)
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}

	// Update to version 2
	_, err = UpdateSpec(db, artifact.ID, spec2, "user", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("UpdateSpec v2: %v", err)
	}

	// Update to version 3
	_, err = UpdateSpec(db, artifact.ID, spec3, "user", now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("UpdateSpec v3: %v", err)
	}

	// Rollback to version 1
	rolledBack, err := Rollback(db, artifact.ID, 1, "user", now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if rolledBack.CurrentVersion != 4 {
		t.Errorf("current_version after rollback: got %d, want 4", rolledBack.CurrentVersion)
	}
	if string(rolledBack.Spec) != string(spec1) {
		t.Errorf("spec after rollback: got %s, want %s", rolledBack.Spec, spec1)
	}

	// Verify 4 versions exist
	versions, _ := ListVersions(db, artifact.ID)
	if len(versions) != 4 {
		t.Errorf("expected 4 versions after rollback, got %d", len(versions))
	}
	if versions[3].Version != 4 {
		t.Errorf("version 4 missing")
	}
	if string(versions[3].Spec) != string(spec1) {
		t.Errorf("rollback version spec mismatch")
	}
}

func TestRollbackNotFound(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Artifact not found
	_, err := Rollback(db, "nonexistent", 1, "user", time.Now())
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows for missing artifact, got %v", err)
	}

	// Version not found
	spec := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":24}`)
	artifact, _ := CreateArtifact(db, "test", KindReportSchedule, spec, "user", time.Now())
	_, err = Rollback(db, artifact.ID, 999, "user", time.Now())
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows for missing version, got %v", err)
	}
}

func TestDeleteArtifact(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	spec := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":24}`)
	now := time.Unix(1700000000, 0)

	artifact, err := CreateArtifact(db, "test", KindReportSchedule, spec, "user", now)
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}

	// Add a run
	run := &Run{
		ArtifactID: artifact.ID,
		Version:    1,
		StartedAt:  now.Unix(),
		FinishedAt: now.Unix() + 10,
		Result:     RunResultOK,
		Detail:     "test",
	}
	if err := RecordRun(db, run); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}

	// Delete
	err = DeleteArtifact(db, artifact.ID)
	if err != nil {
		t.Fatalf("DeleteArtifact: %v", err)
	}

	// Verify artifact gone
	got, err := GetArtifact(db, artifact.ID)
	if err != nil {
		t.Fatalf("GetArtifact after delete: %v", err)
	}
	if got != nil {
		t.Error("artifact should be deleted")
	}

	// Verify versions gone
	versions, err := ListVersions(db, artifact.ID)
	if err != nil {
		t.Fatalf("ListVersions after delete: %v", err)
	}
	if len(versions) != 0 {
		t.Errorf("versions should be deleted, got %d", len(versions))
	}

	// Verify runs gone
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM compose_runs WHERE artifact_id = ?`, artifact.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 0 {
		t.Errorf("runs should be deleted, got %d", count)
	}
}

func TestDeleteArtifactNotFound(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	err := DeleteArtifact(db, "nonexistent")
	if err != sql.ErrNoRows {
		t.Errorf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestRecordRun(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	spec := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":24}`)
	now := time.Unix(1700000000, 0)

	artifact, err := CreateArtifact(db, "test", KindReportSchedule, spec, "user", now)
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}

	run := &Run{
		ArtifactID: artifact.ID,
		Version:    1,
		StartedAt:  now.Unix(),
		FinishedAt: now.Unix() + 10,
		Result:     RunResultOK,
		Detail:     "sent",
	}
	if err := RecordRun(db, run); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}

	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM compose_runs WHERE artifact_id = ?`, artifact.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 run, got %d", count)
	}
}

func TestLastFinishedRun(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	spec := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":24}`)
	now := time.Unix(1700000000, 0)

	artifact, err := CreateArtifact(db, "test", KindReportSchedule, spec, "user", now)
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}

	// No runs yet
	finished, has := LastFinishedRun(db, artifact.ID)
	if has {
		t.Errorf("expected no runs, got finished=%d", finished)
	}

	// Add finished run
	run := &Run{
		ArtifactID: artifact.ID,
		Version:    1,
		StartedAt:  now.Unix(),
		FinishedAt: now.Unix() + 10,
		Result:     RunResultOK,
		Detail:     "sent",
	}
	if err := RecordRun(db, run); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}

	finished, has = LastFinishedRun(db, artifact.ID)
	if !has {
		t.Error("expected finished run")
	}
	if finished != now.Unix()+10 {
		t.Errorf("finished_at: got %d, want %d", finished, now.Unix()+10)
	}

	// Add another run
	run2 := &Run{
		ArtifactID: artifact.ID,
		Version:    1,
		StartedAt:  now.Unix() + 100,
		FinishedAt: now.Unix() + 110,
		Result:     RunResultOK,
		Detail:     "sent again",
	}
	if err := RecordRun(db, run2); err != nil {
		t.Fatalf("RecordRun 2: %v", err)
	}

	finished, has = LastFinishedRun(db, artifact.ID)
	if !has || finished != now.Unix()+110 {
		t.Errorf("expected latest finished run: got %d, want %d", finished, now.Unix()+110)
	}
}

func TestValidateSpec(t *testing.T) {
	tests := []struct {
		name      string
		kind      string
		spec      string
		wantError bool
	}{
		{
			name:      "valid report_schedule",
			kind:      KindReportSchedule,
			spec:      `{"transport":"telegram","chat_id":"123","interval_hours":24}`,
			wantError: false,
		},
		{
			name:      "valid report_schedule whatsapp",
			kind:      KindReportSchedule,
			spec:      `{"transport":"whatsapp","chat_id":"456","interval_hours":1}`,
			wantError: false,
		},
		{
			name:      "report_schedule missing transport",
			kind:      KindReportSchedule,
			spec:      `{"chat_id":"123","interval_hours":24}`,
			wantError: true,
		},
		{
			name:      "report_schedule invalid transport",
			kind:      KindReportSchedule,
			spec:      `{"transport":"email","chat_id":"123","interval_hours":24}`,
			wantError: true,
		},
		{
			name:      "report_schedule missing chat_id",
			kind:      KindReportSchedule,
			spec:      `{"transport":"telegram","interval_hours":24}`,
			wantError: true,
		},
		{
			name:      "report_schedule empty chat_id",
			kind:      KindReportSchedule,
			spec:      `{"transport":"telegram","chat_id":"","interval_hours":24}`,
			wantError: true,
		},
		{
			name:      "report_schedule missing interval_hours",
			kind:      KindReportSchedule,
			spec:      `{"transport":"telegram","chat_id":"123"}`,
			wantError: true,
		},
		{
			name:      "report_schedule interval_hours too low",
			kind:      KindReportSchedule,
			spec:      `{"transport":"telegram","chat_id":"123","interval_hours":0}`,
			wantError: true,
		},
		{
			name:      "report_schedule interval_hours too high",
			kind:      KindReportSchedule,
			spec:      `{"transport":"telegram","chat_id":"123","interval_hours":169}`,
			wantError: true,
		},
		{
			name:      "valid automation_rule",
			kind:      KindAutomationRule,
			spec:      `{"trigger":{"type":"schedule","interval_hours":1},"action":{"type":"send_report","transport":"telegram","chat_id":"123"}}`,
			wantError: false,
		},
		{
			name:      "automation_rule missing trigger",
			kind:      KindAutomationRule,
			spec:      `{"action":{"type":"send_report","transport":"telegram","chat_id":"123"}}`,
			wantError: true,
		},
		{
			name:      "automation_rule invalid trigger type",
			kind:      KindAutomationRule,
			spec:      `{"trigger":{"type":"webhook","interval_hours":1},"action":{"type":"send_report","transport":"telegram","chat_id":"123"}}`,
			wantError: true,
		},
		{
			name:      "automation_rule missing action",
			kind:      KindAutomationRule,
			spec:      `{"trigger":{"type":"schedule","interval_hours":1}}`,
			wantError: true,
		},
		{
			name:      "automation_rule invalid action type",
			kind:      KindAutomationRule,
			spec:      `{"trigger":{"type":"schedule","interval_hours":1},"action":{"type":"delete_file","transport":"telegram","chat_id":"123"}}`,
			wantError: true,
		},
		{
			name:      "valid recipe",
			kind:      KindRecipe,
			spec:      `{"steps":[{"tool":"tool1","args":{"param":"value"}}]}`,
			wantError: false,
		},
		{
			name:      "recipe missing steps",
			kind:      KindRecipe,
			spec:      `{}`,
			wantError: true,
		},
		{
			name:      "recipe empty steps",
			kind:      KindRecipe,
			spec:      `{"steps":[]}`,
			wantError: true,
		},
		{
			name:      "recipe step missing tool",
			kind:      KindRecipe,
			spec:      `{"steps":[{"args":{}}]}`,
			wantError: true,
		},
		{
			name:      "recipe step empty tool",
			kind:      KindRecipe,
			spec:      `{"steps":[{"tool":"","args":{}}]}`,
			wantError: true,
		},
		{
			name:      "recipe step missing args",
			kind:      KindRecipe,
			spec:      `{"steps":[{"tool":"tool1"}]}`,
			wantError: true,
		},
		{
			name:      "recipe step args not object",
			kind:      KindRecipe,
			spec:      `{"steps":[{"tool":"tool1","args":"not-object"}]}`,
			wantError: true,
		},
		{
			name:      "valid tool_chain",
			kind:      KindToolChain,
			spec:      `{"chain":["tool1","tool2"]}`,
			wantError: false,
		},
		{
			name:      "tool_chain missing chain",
			kind:      KindToolChain,
			spec:      `{}`,
			wantError: true,
		},
		{
			name:      "tool_chain empty chain",
			kind:      KindToolChain,
			spec:      `{"chain":[]}`,
			wantError: true,
		},
		{
			name:      "tool_chain empty element",
			kind:      KindToolChain,
			spec:      `{"chain":["tool1",""]}`,
			wantError: true,
		},
		{
			name:      "unknown kind",
			kind:      "unknown_kind",
			spec:      `{}`,
			wantError: true,
		},
		{
			name:      "empty spec",
			kind:      KindReportSchedule,
			spec:      ``,
			wantError: true,
		},
		{
			name:      "invalid JSON",
			kind:      KindReportSchedule,
			spec:      `{invalid}`,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSpec(tt.kind, json.RawMessage(tt.spec))
			if tt.wantError && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestExecutorStartStop(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Create a mock chatbridge service (nil is fine for start/stop test)
	var chat *chatbridge.Service
	nowFunc := func() time.Time { return time.Unix(1700000000, 0) }

	executor := NewExecutor(db, chat, nowFunc)

	// Start should be idempotent
	executor.Start(context.Background())
	executor.Start(context.Background()) // Second start should not panic

	// Stop should be idempotent
	executor.Stop()
	executor.Stop() // Second stop should not panic
}

func TestExecutorTickNoActiveArtifacts(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	var chat *chatbridge.Service
	nowFunc := func() time.Time { return time.Unix(1700000000, 0) }

	executor := NewExecutor(db, chat, nowFunc)
	executor.Start(context.Background())
	defer executor.Stop()

	// Tick with no active artifacts should not error
	// We can't directly call tick, but we can verify no panic by starting/stopping
	// The executor runs tick internally
	time.Sleep(10 * time.Millisecond)
}

func TestExecutorExecuteIfDue(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Create a mock chatbridge with a registered transport
	chat := chatbridge.NewService(chatbridge.Deps{
		DB: db,
	})
	// Register a fake transport
	fakeTransport := &fakeTransport{name: "telegram"}
	chat.RegisterTransport(fakeTransport)

	nowFunc := func() time.Time { return time.Unix(1700000000, 0) }
	executor := NewExecutor(db, chat, nowFunc)

	// Create an active report_schedule artifact
	spec := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":24}`)
	artifact, err := CreateArtifact(db, "test-report", KindReportSchedule, spec, "user", nowFunc())
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	// Activate it
	_, err = SetStatus(db, artifact.ID, StatusActive, "user", nowFunc())
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	// Execute tick - should try to send report
	executor.Start(context.Background())
	defer executor.Stop()

	// Wait for tick to run (it runs immediately on start)
	time.Sleep(50 * time.Millisecond)

	// Verify a run was recorded (even if send failed due to fake transport)
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM compose_runs WHERE artifact_id = ?`, artifact.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count == 0 {
		t.Error("expected at least one run recorded")
	}
}

// TestExecutorDeadlockRegression — regresi deadlock tick (temuan Chief 7 Nov 2026,
// rekomendasi audit Aria WS52-A): tick menahan rows terbuka sambil executeIfDue
// query lagi = deadlock dengan satu koneksi. Dengan artifact aktif + DB satu
// koneksi, tick HARUS selesai (bukan menggantung) dan mencatat satu run.
func TestExecutorDeadlockRegression(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	chat := chatbridge.NewService(chatbridge.Deps{DB: db})
	chat.RegisterTransport(&fakeTransport{name: "telegram"})

	nowFunc := func() time.Time { return time.Unix(1700000000, 0) }
	executor := NewExecutor(db, chat, nowFunc)

	spec := json.RawMessage(`{"transport":"telegram","chat_id":"123","interval_hours":24}`)
	artifact, err := CreateArtifact(db, "test-deadlock", KindReportSchedule, spec, "user", nowFunc())
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	if _, err := SetStatus(db, artifact.ID, StatusActive, "user", nowFunc()); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		executor.tick(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tick tidak selesai dalam 5 detik — deadlock tick terulang")
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM compose_runs WHERE artifact_id = ?`, artifact.ID).Scan(&count); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count == 0 {
		t.Error("expected at least one run recorded")
	}
}

type fakeTransport struct {
	name string
}

func (f *fakeTransport) Name() string { return f.name }
func (f *fakeTransport) Send(chatID, text string) error {
	return nil // Always succeed for test
}
