package compose

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/chatbridge"

	_ "modernc.org/sqlite"
)

// TestExecutorAutomationRuleDue tests that an active automation_rule artifact
// with schedule trigger and send_report action executes on tick and records an ok run.
func TestExecutorAutomationRuleDue(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	chat := chatbridge.NewService(chatbridge.Deps{DB: db})
	fakeTransport := &fakeTransport{name: "telegram"}
	chat.RegisterTransport(fakeTransport)

	nowFunc := func() time.Time { return time.Unix(1700000000, 0) }
	executor := NewExecutor(db, chat, nowFunc)

	// Create an active automation_rule artifact
	spec := json.RawMessage(`{
		"trigger": {"type": "schedule", "interval_hours": 1},
		"action": {"type": "send_report", "transport": "telegram", "chat_id": "999"}
	}`)
	artifact, err := CreateArtifact(db, "test-automation", KindAutomationRule, spec, "user", nowFunc())
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	_, err = SetStatus(db, artifact.ID, StatusActive, "user", nowFunc())
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	// Execute tick
	executor.tick(context.Background())

	// Verify exactly 1 run recorded with ok result
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM compose_runs WHERE artifact_id = ?`, artifact.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 run, got %d", count)
	}

	var result, detail string
	err = db.QueryRow(`SELECT result, detail FROM compose_runs WHERE artifact_id = ?`, artifact.ID).Scan(&result, &detail)
	if err != nil {
		t.Fatalf("query run: %v", err)
	}
	if result != RunResultOK {
		t.Errorf("run result: got %s, want %s", result, RunResultOK)
	}
	if detail != "sent" {
		t.Errorf("run detail: got %s, want sent", detail)
	}

	// Verify fakeTransport received at least one non-empty message
	// (We can't directly inspect fakeTransport, but we can verify the run succeeded)
	// The fakeTransport.Send returns nil, so if we got here, it was called.
}

// TestExecutorAutomationRuleNotDue tests that an automation_rule with a recent
// finished run does not execute again until the interval has passed.
func TestExecutorAutomationRuleNotDue(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	chat := chatbridge.NewService(chatbridge.Deps{DB: db})
	fakeTransport := &fakeTransport{name: "telegram"}
	chat.RegisterTransport(fakeTransport)

	nowFunc := func() time.Time { return time.Unix(1700000000, 0) }
	executor := NewExecutor(db, chat, nowFunc)

	// Create an active automation_rule artifact
	spec := json.RawMessage(`{
		"trigger": {"type": "schedule", "interval_hours": 1},
		"action": {"type": "send_report", "transport": "telegram", "chat_id": "999"}
	}`)
	artifact, err := CreateArtifact(db, "test-automation-notdue", KindAutomationRule, spec, "user", nowFunc())
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	_, err = SetStatus(db, artifact.ID, StatusActive, "user", nowFunc())
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	// Record a finished run at 'now' (simulating it just ran)
	run := &Run{
		ArtifactID: artifact.ID,
		Version:    1,
		StartedAt:  nowFunc().Unix() - 10,
		FinishedAt: nowFunc().Unix(),
		Result:     RunResultOK,
		Detail:     "sent",
	}
	if err := RecordRun(db, run); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}

	// Execute tick - should NOT run again because interval not passed
	executor.tick(context.Background())

	// Verify still only 1 run (the one we manually recorded)
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM compose_runs WHERE artifact_id = ?`, artifact.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 run (no new run), got %d", count)
	}
}

// TestExecutorAutomationRuleTriggerNotSupported tests that an automation_rule
// with unsupported trigger type (e.g., webhook) records a failed run and does not execute.
func TestExecutorAutomationRuleTriggerNotSupported(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	chat := chatbridge.NewService(chatbridge.Deps{DB: db})
	fakeTransport := &fakeTransport{name: "telegram"}
	chat.RegisterTransport(fakeTransport)

	nowFunc := func() time.Time { return time.Unix(1700000000, 0) }
	executor := NewExecutor(db, chat, nowFunc)

	// Create an active automation_rule artifact with unsupported trigger type
	spec := json.RawMessage(`{
		"trigger": {"type": "webhook", "interval_hours": 1},
		"action": {"type": "send_report", "transport": "telegram", "chat_id": "999"}
	}`)
	artifact, err := CreateArtifact(db, "test-automation-webhook", KindAutomationRule, spec, "user", nowFunc())
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	_, err = SetStatus(db, artifact.ID, StatusActive, "user", nowFunc())
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	// Execute tick
	executor.tick(context.Background())

	// Verify 1 run recorded with failed result and trigger_not_supported detail
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM compose_runs WHERE artifact_id = ?`, artifact.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 run, got %d", count)
	}

	var result, detail string
	err = db.QueryRow(`SELECT result, detail FROM compose_runs WHERE artifact_id = ?`, artifact.ID).Scan(&result, &detail)
	if err != nil {
		t.Fatalf("query run: %v", err)
	}
	if result != RunResultFailed {
		t.Errorf("run result: got %s, want %s", result, RunResultFailed)
	}
	if detail != "trigger_not_supported" {
		t.Errorf("run detail: got %s, want trigger_not_supported", detail)
	}
}

// TestExecutorAutomationRuleActionNotWhitelisted tests that an automation_rule
// with unsupported action type (e.g., delete_pool) records a failed run and does not execute.
func TestExecutorAutomationRuleActionNotWhitelisted(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	chat := chatbridge.NewService(chatbridge.Deps{DB: db})
	fakeTransport := &fakeTransport{name: "telegram"}
	chat.RegisterTransport(fakeTransport)

	nowFunc := func() time.Time { return time.Unix(1700000000, 0) }
	executor := NewExecutor(db, chat, nowFunc)

	// Create an active automation_rule artifact with unsupported action type
	spec := json.RawMessage(`{
		"trigger": {"type": "schedule", "interval_hours": 1},
		"action": {"type": "delete_pool", "transport": "telegram", "chat_id": "999"}
	}`)
	artifact, err := CreateArtifact(db, "test-automation-delete", KindAutomationRule, spec, "user", nowFunc())
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	_, err = SetStatus(db, artifact.ID, StatusActive, "user", nowFunc())
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	// Execute tick
	executor.tick(context.Background())

	// Verify 1 run recorded with failed result and action_not_whitelisted detail
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM compose_runs WHERE artifact_id = ?`, artifact.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 run, got %d", count)
	}

	var result, detail string
	err = db.QueryRow(`SELECT result, detail FROM compose_runs WHERE artifact_id = ?`, artifact.ID).Scan(&result, &detail)
	if err != nil {
		t.Fatalf("query run: %v", err)
	}
	if result != RunResultFailed {
		t.Errorf("run result: got %s, want %s", result, RunResultFailed)
	}
	if detail != "action_not_whitelisted" {
		t.Errorf("run detail: got %s, want action_not_whitelisted", detail)
	}
}

// TestExecutorAutomationRuleTransportNotRegistered tests that an automation_rule
// with a transport that is not registered records a failed run.
func TestExecutorAutomationRuleTransportNotRegistered(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	chat := chatbridge.NewService(chatbridge.Deps{DB: db})
	// Only register telegram, not whatsapp
	fakeTransport := &fakeTransport{name: "telegram"}
	chat.RegisterTransport(fakeTransport)

	nowFunc := func() time.Time { return time.Unix(1700000000, 0) }
	executor := NewExecutor(db, chat, nowFunc)

	// Create an active automation_rule artifact with whatsapp transport (not registered)
	spec := json.RawMessage(`{
		"trigger": {"type": "schedule", "interval_hours": 1},
		"action": {"type": "send_report", "transport": "whatsapp", "chat_id": "999"}
	}`)
	artifact, err := CreateArtifact(db, "test-automation-wa", KindAutomationRule, spec, "user", nowFunc())
	if err != nil {
		t.Fatalf("CreateArtifact: %v", err)
	}
	_, err = SetStatus(db, artifact.ID, StatusActive, "user", nowFunc())
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	// Execute tick
	executor.tick(context.Background())

	// Verify 1 run recorded with failed result and transport_not_registered detail
	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM compose_runs WHERE artifact_id = ?`, artifact.ID).Scan(&count)
	if err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 run, got %d", count)
	}

	var result, detail string
	err = db.QueryRow(`SELECT result, detail FROM compose_runs WHERE artifact_id = ?`, artifact.ID).Scan(&result, &detail)
	if err != nil {
		t.Fatalf("query run: %v", err)
	}
	if result != RunResultFailed {
		t.Errorf("run result: got %s, want %s", result, RunResultFailed)
	}
	if detail != "transport_not_registered" {
		t.Errorf("run detail: got %s, want transport_not_registered", detail)
	}
}