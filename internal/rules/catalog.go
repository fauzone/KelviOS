// Package rules implements the FR-3.11 Event→Action Rules Engine.
// Deterministic engine evaluating user-defined rules against observed events.
package rules

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fauzone/kelvios/internal/agents"
)

// ============================================================================
// LOCKED CATALOG — Single source of truth for validation (API + engine)
// ============================================================================

// EventType represents a known event type in the catalog.
type EventType string

const (
	EventBackupRunFailed      EventType = "backup.run_failed"
	EventBackupRunSucceeded   EventType = "backup.run_succeeded"
	EventBackupRestoreFailed  EventType = "backup.restore_failed"
	EventRestoreTestAlert     EventType = "restore_test.alert"
	EventUPSEvent             EventType = "ups.event"
	EventAutomationRunFailed  EventType = "automation.run_failed"
	EventPoolUsageAbove       EventType = "pool.usage_above"
)

// AllEventTypes returns all known event types.
func AllEventTypes() []EventType {
	return []EventType{
		EventBackupRunFailed,
		EventBackupRunSucceeded,
		EventBackupRestoreFailed,
		EventRestoreTestAlert,
		EventUPSEvent,
		EventAutomationRunFailed,
		EventPoolUsageAbove,
	}
}

// EventTypeParams describes the allowed parameters for an event type.
type EventTypeParams struct {
	Key         string   `json:"key"`
	Values      []string `json:"values,omitempty"`
	Min         *int     `json:"min,omitempty"`
	Max         *int     `json:"max,omitempty"`
	Default     any      `json:"default,omitempty"`
	Optional    bool     `json:"optional"`
}

// EventCatalog maps event types to their allowed parameters.
var EventCatalog = map[EventType][]EventTypeParams{
	EventBackupRunFailed:      {},
	EventBackupRunSucceeded:   {},
	EventBackupRestoreFailed:  {},
	EventRestoreTestAlert:     {},
	EventUPSEvent: {
		{Key: "kind", Values: []string{"on_battery", "online_restored", "critical_shutdown", "unclean_shutdown"}, Optional: true},
	},
	EventAutomationRunFailed: {},
	EventPoolUsageAbove: {
		{Key: "threshold_percent", Min: intPtr(50), Max: intPtr(99), Default: 90, Optional: true},
		{Key: "pool_id", Optional: true},
	},
}

// ActionType represents a known action type in the catalog.
type ActionType string

const (
	ActionRaiseAlert       ActionType = "raise_alert"
	ActionRunAgent         ActionType = "run_agent"
	ActionSendReport       ActionType = "send_report"
	ActionRunRestoreTest   ActionType = "run_restore_test"
)

// AllActionTypes returns all known action types.
func AllActionTypes() []ActionType {
	return []ActionType{
		ActionRaiseAlert,
		ActionRunAgent,
		ActionSendReport,
		ActionRunRestoreTest,
	}
}

// EventTypeExists checks if an event type is known.
func EventTypeExists(eventType string) bool {
	for _, et := range AllEventTypes() {
		if string(et) == eventType {
			return true
		}
	}
	return false
}

// ActionTypeExists checks if an action type is known.
func ActionTypeExists(actionType string) bool {
	for _, at := range AllActionTypes() {
		if string(at) == actionType {
			return true
		}
	}
	return false
}

// ActionTypeParams describes the allowed parameters for an action type.
type ActionTypeParams struct {
	Key      string   `json:"key"`
	Values   []string `json:"values,omitempty"`
	Min      *int     `json:"min,omitempty"`
	Max      *int     `json:"max,omitempty"`
	Default  any      `json:"default,omitempty"`
	Optional bool     `json:"optional"`
	Required bool     `json:"required,omitempty"`
}

// ActionCatalog maps action types to their allowed parameters.
// All actions also accept delay_seconds (0..86400) as a common parameter.
var ActionCatalog = map[ActionType][]ActionTypeParams{
	ActionRaiseAlert: {
		{Key: "priority", Values: []string{"normal", "high"}, Default: "normal"},
		{Key: "transport", Optional: true},
		{Key: "chat_id", Optional: true},
	},
	ActionRunAgent: {
		{Key: "agent", Values: agentNames(), Required: true},
	},
	ActionSendReport: {
		{Key: "transport", Required: true},
		{Key: "chat_id", Required: true},
	},
	ActionRunRestoreTest: {},
}

// agentNames returns the list of registered agent names from the agents registry.
func agentNames() []string {
	var names []string
	for _, a := range agents.All() {
		names = append(names, a.Name())
	}
	return names
}

// CommonActionParams returns the common parameters accepted by all actions.
func CommonActionParams() []ActionTypeParams {
	return []ActionTypeParams{
		{Key: "delay_seconds", Min: intPtr(0), Max: intPtr(86400), Default: 0, Optional: true},
	}
}

// intPtr returns a pointer to an int.
func intPtr(v int) *int {
	return &v
}

// ============================================================================
// VALIDATION HELPERS
// ============================================================================

// ValidateEventParams validates event_params against the catalog for a given event_type.
func ValidateEventParams(eventType EventType, params map[string]any) error {
	allowed, ok := EventCatalog[eventType]
	if !ok {
		return fmt.Errorf("unknown event_type: %s", eventType)
	}
	// Check for unknown keys
	for k := range params {
		found := false
		for _, p := range allowed {
			if p.Key == k {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown event_param key: %s (event_type=%s)", k, eventType)
		}
	}
	// Validate known keys
	for _, p := range allowed {
		val, exists := params[p.Key]
		if !exists {
			if !p.Optional {
				return fmt.Errorf("missing required event_param: %s", p.Key)
			}
			continue
		}
		if p.Values != nil {
			strVal, ok := val.(string)
			if !ok {
				return fmt.Errorf("event_param %s must be string", p.Key)
			}
			valid := false
			for _, v := range p.Values {
				if v == strVal {
					valid = true
					break
				}
			}
			if !valid {
				return fmt.Errorf("event_param %s invalid value: %s (allowed: %v)", p.Key, strVal, p.Values)
			}
		}
		if p.Min != nil || p.Max != nil {
			var intVal int
			switch v := val.(type) {
			case float64:
				intVal = int(v)
			case int:
				intVal = v
			default:
				return fmt.Errorf("event_param %s must be integer", p.Key)
			}
			if p.Min != nil && intVal < *p.Min {
				return fmt.Errorf("event_param %s below minimum: %d < %d", p.Key, intVal, *p.Min)
			}
			if p.Max != nil && intVal > *p.Max {
				return fmt.Errorf("event_param %s above maximum: %d > %d", p.Key, intVal, *p.Max)
			}
		}
	}
	return nil
}

// ValidateActionParams validates a single action's parameters against the catalog.
func ValidateActionParams(actionType ActionType, params map[string]any) error {
	allowed, ok := ActionCatalog[actionType]
	if !ok {
		return fmt.Errorf("unknown action_type: %s", actionType)
	}
	// Also check common params
	allAllowed := append(allowed, CommonActionParams()...)

	// Check for unknown keys
	for k := range params {
		found := false
		for _, p := range allAllowed {
			if p.Key == k {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown action_param key: %s (action_type=%s)", k, actionType)
		}
	}
	// Validate known keys
	for _, p := range allAllowed {
		val, exists := params[p.Key]
		if !exists {
			if p.Required {
				return fmt.Errorf("missing required action_param: %s", p.Key)
			}
			continue
		}
		if p.Values != nil {
			strVal, ok := val.(string)
			if !ok {
				return fmt.Errorf("action_param %s must be string", p.Key)
			}
			valid := false
			for _, v := range p.Values {
				if v == strVal {
					valid = true
					break
				}
			}
			if !valid {
				return fmt.Errorf("action_param %s invalid value: %s (allowed: %v)", p.Key, strVal, p.Values)
			}
		}
		if p.Min != nil || p.Max != nil {
			var intVal int
			switch v := val.(type) {
			case float64:
				intVal = int(v)
			case int:
				intVal = v
			default:
				return fmt.Errorf("action_param %s must be integer", p.Key)
			}
			if p.Min != nil && intVal < *p.Min {
				return fmt.Errorf("action_param %s below minimum: %d < %d", p.Key, intVal, *p.Min)
			}
			if p.Max != nil && intVal > *p.Max {
				return fmt.Errorf("action_param %s above maximum: %d > %d", p.Key, intVal, *p.Max)
			}
		}
	}
	// Special validation: raise_alert transport+chat_id must be both present or both absent
	if actionType == ActionRaiseAlert {
		hasTransport := params["transport"] != nil
		hasChatID := params["chat_id"] != nil
		if hasTransport != hasChatID {
			return fmt.Errorf("raise_alert: transport and chat_id must be both present or both absent")
		}
	}
	return nil
}

// ValidateRule validates a complete rule (event + actions) against the catalog.
func ValidateRule(eventType EventType, eventParams map[string]any, actions []ActionSpec) error {
	if err := ValidateEventParams(eventType, eventParams); err != nil {
		return fmt.Errorf("event validation: %w", err)
	}
	if len(actions) < 1 || len(actions) > 3 {
		return fmt.Errorf("actions count must be 1..3, got %d", len(actions))
	}
	for i, a := range actions {
		if err := ValidateActionParams(a.Type, a.Params); err != nil {
			return fmt.Errorf("action[%d] validation: %w", i, err)
		}
	}
	return nil
}

// ActionSpec represents a single action in a rule.
type ActionSpec struct {
	Type   ActionType       `json:"type"`
	Params map[string]any   `json:"params"`
}

// Rule represents a user-defined event→action rule.
type Rule struct {
	ID                string         `json:"id"`
	Name              string         `json:"name"`
	EventType         EventType      `json:"event_type"`
	EventParams       map[string]any `json:"event_params"`
	Actions           []ActionSpec   `json:"actions"`
	Enabled           bool           `json:"enabled"`
	CooldownSeconds   int            `json:"cooldown_seconds"`
	MaxFiresPerDay    int            `json:"max_fires_per_day"`
	State             map[string]any `json:"state"`
	LastFiredAt       int64          `json:"last_fired_at"`
	CreatedAt         int64          `json:"created_at"`
	UpdatedAt         int64          `json:"updated_at"`
}

// Run represents a rule execution record.
type Run struct {
	ID         int64  `json:"id"`
	RuleID     string `json:"rule_id"`
	EventRef   string `json:"event_ref"`
	FiredAt    int64  `json:"fired_at"`
	Result     string `json:"result"`     // "ok" | "failed" | "partial" | "pending"
	Detail     string `json:"detail"`
	Alert      bool   `json:"alert"`
	TestFire   bool   `json:"test_fire"`
}

// RunResult constants.
const (
	RunResultOK      = "ok"
	RunResultFailed  = "failed"
	RunResultPartial = "partial"
	RunResultPending = "pending"
)

// ============================================================================
// CATALOG EXPORT FOR API
// ============================================================================

// CatalogResponse is the JSON response for GET /api/rules/catalog.
type CatalogResponse struct {
	Events []CatalogEvent `json:"events"`
	Actions []CatalogAction `json:"actions"`
}

type CatalogEvent struct {
	Type   string             `json:"type"`
	Params []CatalogParamSpec `json:"params"`
}

type CatalogAction struct {
	Type   string             `json:"type"`
	Params []CatalogParamSpec `json:"params"`
}

type CatalogParamSpec struct {
	Key         string   `json:"key"`
	Type        string   `json:"type,omitempty"`
	Description string   `json:"description,omitempty"`
	Values      []string `json:"values,omitempty"`
	Min         *int     `json:"min,omitempty"`
	Max         *int     `json:"max,omitempty"`
	Default     any      `json:"default,omitempty"`
	Optional    bool     `json:"optional"`
	Required    bool     `json:"required,omitempty"`
}

// inferParamType derives the editor type for a catalog parameter from its
// default value (int range bounds imply int); string otherwise.
func inferParamType(def any, min *int) string {
	switch def.(type) {
	case int, int64, float64:
		return "int"
	case bool:
		return "bool"
	}
	if min != nil {
		return "int"
	}
	return "string"
}

// GetCatalog returns the catalog for the API editor.
func GetCatalog() CatalogResponse {
	var events []CatalogEvent
	for _, et := range AllEventTypes() {
		params := EventCatalog[et]
		var cp []CatalogParamSpec
		for _, p := range params {
			cp = append(cp, CatalogParamSpec{
				Key:      p.Key,
				Type:     inferParamType(p.Default, p.Min),
				Values:   p.Values,
				Min:      p.Min,
				Max:      p.Max,
				Default:  p.Default,
				Optional: p.Optional,
			})
		}
		events = append(events, CatalogEvent{Type: string(et), Params: cp})
	}

	var actions []CatalogAction
	for _, at := range AllActionTypes() {
		params := ActionCatalog[at]
		allParams := append(params, CommonActionParams()...)
		var cp []CatalogParamSpec
		for _, p := range allParams {
			cp = append(cp, CatalogParamSpec{
				Key:      p.Key,
				Type:     inferParamType(p.Default, p.Min),
				Values:   p.Values,
				Min:      p.Min,
				Max:      p.Max,
				Default:  p.Default,
				Optional: p.Optional,
				Required: p.Required,
			})
		}
		actions = append(actions, CatalogAction{Type: string(at), Params: cp})
	}

	return CatalogResponse{Events: events, Actions: actions}
}

// ============================================================================
// STORE INTERFACE (for engine dependency injection)
// ============================================================================

// Store defines the persistence interface for rules.
type Store interface {
	CreateRule(rule *Rule) error
	GetRule(id string) (*Rule, error)
	ListRules() ([]*Rule, error)
	UpdateRule(rule *Rule) error
	DeleteRule(id string) error
	CreateRun(run *Run) error
	GetRun(ruleID string, eventRef string) (*Run, error)
	ListRuns(ruleID string, limit, offset int) ([]*Run, int, error)
	CountRunsSince(ruleID string, since int64) (int, error)
	UpdateRuleState(ruleID string, state map[string]any, lastFiredAt int64) error
}

// SQLStore implements Store using SQLite.
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore creates a new SQLStore.
func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{db: db}
}

func (s *SQLStore) CreateRule(rule *Rule) error {
	eventParamsJSON, _ := json.Marshal(rule.EventParams)
	actionsJSON, _ := json.Marshal(rule.Actions)
	stateJSON, _ := json.Marshal(rule.State)
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		INSERT INTO event_rules (id, name, event_type, event_params, actions, enabled, cooldown_seconds, max_fires_per_day, state, last_fired_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, rule.ID, rule.Name, rule.EventType, string(eventParamsJSON), string(actionsJSON), boolToInt(rule.Enabled), rule.CooldownSeconds, rule.MaxFiresPerDay, string(stateJSON), rule.LastFiredAt, now, now)
	return err
}

func (s *SQLStore) GetRule(id string) (*Rule, error) {
	row := s.db.QueryRow(`
		SELECT id, name, event_type, event_params, actions, enabled, cooldown_seconds, max_fires_per_day, state, last_fired_at, created_at, updated_at
		FROM event_rules WHERE id = ?
	`, id)
	var r Rule
	var eventParamsJSON, actionsJSON, stateJSON string
	var enabled int
	err := row.Scan(&r.ID, &r.Name, &r.EventType, &eventParamsJSON, &actionsJSON, &enabled, &r.CooldownSeconds, &r.MaxFiresPerDay, &stateJSON, &r.LastFiredAt, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Enabled = enabled != 0
	json.Unmarshal([]byte(eventParamsJSON), &r.EventParams)
	json.Unmarshal([]byte(actionsJSON), &r.Actions)
	json.Unmarshal([]byte(stateJSON), &r.State)
	if r.EventParams == nil {
		r.EventParams = map[string]any{}
	}
	if r.Actions == nil {
		r.Actions = []ActionSpec{}
	}
	if r.State == nil {
		r.State = map[string]any{}
	}
	return &r, nil
}

func (s *SQLStore) ListRules() ([]*Rule, error) {
	rows, err := s.db.Query(`
		SELECT id, name, event_type, event_params, actions, enabled, cooldown_seconds, max_fires_per_day, state, last_fired_at, created_at, updated_at
		FROM event_rules ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []*Rule
	for rows.Next() {
		var r Rule
		var eventParamsJSON, actionsJSON, stateJSON string
		var enabled int
		if err := rows.Scan(&r.ID, &r.Name, &r.EventType, &eventParamsJSON, &actionsJSON, &enabled, &r.CooldownSeconds, &r.MaxFiresPerDay, &stateJSON, &r.LastFiredAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		json.Unmarshal([]byte(eventParamsJSON), &r.EventParams)
		json.Unmarshal([]byte(actionsJSON), &r.Actions)
		json.Unmarshal([]byte(stateJSON), &r.State)
		if r.EventParams == nil {
			r.EventParams = map[string]any{}
		}
		if r.Actions == nil {
			r.Actions = []ActionSpec{}
		}
		if r.State == nil {
			r.State = map[string]any{}
		}
		rules = append(rules, &r)
	}
	return rules, nil
}

func (s *SQLStore) UpdateRule(rule *Rule) error {
	eventParamsJSON, _ := json.Marshal(rule.EventParams)
	actionsJSON, _ := json.Marshal(rule.Actions)
	stateJSON, _ := json.Marshal(rule.State)
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		UPDATE event_rules SET name = ?, event_type = ?, event_params = ?, actions = ?, enabled = ?, cooldown_seconds = ?, max_fires_per_day = ?, state = ?, last_fired_at = ?, updated_at = ?
		WHERE id = ?
	`, rule.Name, rule.EventType, string(eventParamsJSON), string(actionsJSON), boolToInt(rule.Enabled), rule.CooldownSeconds, rule.MaxFiresPerDay, string(stateJSON), rule.LastFiredAt, now, rule.ID)
	return err
}

func (s *SQLStore) DeleteRule(id string) error {
	_, err := s.db.Exec(`DELETE FROM event_rules WHERE id = ?`, id)
	return err
}

func (s *SQLStore) CreateRun(run *Run) error {
	_, err := s.db.Exec(`
		INSERT INTO event_rule_runs (rule_id, event_ref, fired_at, result, detail, alert, test_fire)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, run.RuleID, run.EventRef, run.FiredAt, run.Result, run.Detail, boolToInt(run.Alert), boolToInt(run.TestFire))
	return err
}

func (s *SQLStore) GetRun(ruleID, eventRef string) (*Run, error) {
	row := s.db.QueryRow(`
		SELECT id, rule_id, event_ref, fired_at, result, detail, alert, test_fire
		FROM event_rule_runs WHERE rule_id = ? AND event_ref = ?
	`, ruleID, eventRef)
	var r Run
	var alert, testFire int
	err := row.Scan(&r.ID, &r.RuleID, &r.EventRef, &r.FiredAt, &r.Result, &r.Detail, &alert, &testFire)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Alert = alert != 0
	r.TestFire = testFire != 0
	return &r, nil
}

func (s *SQLStore) ListRuns(ruleID string, limit, offset int) ([]*Run, int, error) {
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event_rule_runs WHERE rule_id = ?`, ruleID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`
		SELECT id, rule_id, event_ref, fired_at, result, detail, alert, test_fire
		FROM event_rule_runs WHERE rule_id = ? ORDER BY fired_at DESC LIMIT ? OFFSET ?
	`, ruleID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		var r Run
		var alert, testFire int
		if err := rows.Scan(&r.ID, &r.RuleID, &r.EventRef, &r.FiredAt, &r.Result, &r.Detail, &alert, &testFire); err != nil {
			return nil, 0, err
		}
		r.Alert = alert != 0
		r.TestFire = testFire != 0
		runs = append(runs, &r)
	}
	return runs, total, nil
}

func (s *SQLStore) CountRunsSince(ruleID string, since int64) (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM event_rule_runs WHERE rule_id = ? AND fired_at >= ?
	`, ruleID, since).Scan(&count)
	return count, err
}

func (s *SQLStore) UpdateRuleState(ruleID string, state map[string]any, lastFiredAt int64) error {
	stateJSON, _ := json.Marshal(state)
	now := time.Now().Unix()
	_, err := s.db.Exec(`
		UPDATE event_rules SET state = ?, last_fired_at = ?, updated_at = ? WHERE id = ?
	`, string(stateJSON), lastFiredAt, now, ruleID)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}