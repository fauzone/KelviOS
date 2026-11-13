// Package httpserver implements HTTP handlers for the rules engine API.
package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fauzone/kelvios/internal/rules"
)

// ============================================================================
// REQUEST/RESPONSE TYPES
// ============================================================================

type CreateRuleRequest struct {
	Name             string                 `json:"name"`
	EventType        string                 `json:"event_type"`
	EventParams      map[string]any         `json:"event_params,omitempty"`
	Actions          []rules.ActionSpec         `json:"actions"`
	Enabled          *bool                  `json:"enabled,omitempty"`
	CooldownSeconds  *int                   `json:"cooldown_seconds,omitempty"`
	MaxFiresPerDay   *int                   `json:"max_fires_per_day,omitempty"`
}

type UpdateRuleRequest struct {
	Name             *string                `json:"name,omitempty"`
	EventType        *string                `json:"event_type,omitempty"`
	EventParams      map[string]any         `json:"event_params,omitempty"`
	Actions          []rules.ActionSpec         `json:"actions,omitempty"`
	Enabled          *bool                  `json:"enabled,omitempty"`
	CooldownSeconds  *int                   `json:"cooldown_seconds,omitempty"`
	MaxFiresPerDay   *int                   `json:"max_fires_per_day,omitempty"`
}

type RuleResponse struct {
	ID               string                 `json:"id"`
	Name             string                 `json:"name"`
	EventType        string                 `json:"event_type"`
	EventParams      map[string]any         `json:"event_params"`
	Actions          []rules.ActionSpec         `json:"actions"`
	Enabled          bool                   `json:"enabled"`
	CooldownSeconds  int                    `json:"cooldown_seconds"`
	MaxFiresPerDay   int                    `json:"max_fires_per_day"`
	State            map[string]any         `json:"state"`
	LastFiredAt      int64                  `json:"last_fired_at"`
	CreatedAt        int64                  `json:"created_at"`
	UpdatedAt        int64                  `json:"updated_at"`
}

type RunResponse struct {
	ID        string `json:"id"`
	RuleID    string `json:"rule_id"`
	EventRef  string `json:"event_ref"`
	FiredAt   int64  `json:"fired_at"`
	Result    string `json:"result"`
	Detail    string `json:"detail"`
	Alert     bool   `json:"alert"`
	TestFire  bool   `json:"test_fire"`
}

type ListRunsResponse struct {
	Runs  []RunResponse `json:"runs"`
	Total int           `json:"total"`
	Limit int           `json:"limit"`
	Offset int          `json:"offset"`
}

type TestFireRequest struct {
	EventRef string         `json:"event_ref,omitempty"`
	EventData map[string]any `json:"event_data,omitempty"`
}

type CatalogResponse struct {
	EventTypes []EventTypeInfo `json:"event_types"`
	ActionTypes []ActionTypeInfo `json:"action_types"`
}

type EventTypeInfo struct {
	Type        string   `json:"type"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Params      []ParamInfo `json:"params"`
}

type ActionTypeInfo struct {
	Type        string   `json:"type"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Params      []ParamInfo `json:"params"`
}

type ParamInfo struct {
	Name        string `json:"name"`
	Type        string `json:"type"` // string, int, bool
	Required    bool   `json:"required"`
	Description string `json:"description"`
	Default     any    `json:"default,omitempty"`
	Enum        []string `json:"enum,omitempty"`
}

// ============================================================================
// HANDLERS
// ============================================================================

// RulesHandler holds dependencies for rules HTTP handlers.
type RulesHandler struct {
	Store  rules.Store
	Engine *rules.Engine
}

// RegisterRulesRoutes registers the rules API routes.
func (h *RulesHandler) RegisterRulesRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/rules/catalog", h.handleGetCatalog)
	mux.HandleFunc("GET /api/v1/rules", h.handleListRules)
	mux.HandleFunc("POST /api/v1/rules", h.handleCreateRule)
	mux.HandleFunc("GET /api/v1/rules/{id}", h.handleGetRule)
	mux.HandleFunc("PATCH /api/v1/rules/{id}", h.handleUpdateRule)
	mux.HandleFunc("DELETE /api/v1/rules/{id}", h.handleDeleteRule)
	mux.HandleFunc("POST /api/v1/rules/{id}/test-fire", h.handleTestFire)
	mux.HandleFunc("GET /api/v1/rules/{id}/runs", h.handleListRuns)
}

// ============================================================================
// CATALOG
// ============================================================================

func (h *RulesHandler) handleGetCatalog(w http.ResponseWriter, r *http.Request) {
	cat := rules.GetCatalog()

	eventTypes := make([]EventTypeInfo, 0, len(cat.Events))
	for _, et := range cat.Events {
		params := make([]ParamInfo, 0, len(et.Params))
		for _, p := range et.Params {
			params = append(params, ParamInfo{
				Name:        p.Key,
				Type:        p.Type,
				Required:    p.Required,
				Description: p.Description,
				Default:     p.Default,
				Enum:        p.Values,
			})
		}
		eventTypes = append(eventTypes, EventTypeInfo{
			Type:        et.Type,
			Label:       et.Type,
			Description: "",
			Params:      params,
		})
	}

	actionTypes := make([]ActionTypeInfo, 0, len(cat.Actions))
	for _, at := range cat.Actions {
		params := make([]ParamInfo, 0, len(at.Params))
		for _, p := range at.Params {
			params = append(params, ParamInfo{
				Name:        p.Key,
				Type:        p.Type,
				Required:    p.Required,
				Description: p.Description,
				Default:     p.Default,
				Enum:        p.Values,
			})
		}
		actionTypes = append(actionTypes, ActionTypeInfo{
			Type:        at.Type,
			Label:       at.Type,
			Description: "",
			Params:      params,
		})
	}

	writeJSON(w, http.StatusOK, CatalogResponse{
		EventTypes: eventTypes,
		ActionTypes: actionTypes,
	})
}

// ============================================================================
// RULE CRUD
// ============================================================================

func (h *RulesHandler) handleListRules(w http.ResponseWriter, r *http.Request) {
	rulesList, err := h.Store.ListRules()
	if err != nil {
		slog.Error("rules api: list rules failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list rules")
		return
	}

	resp := make([]RuleResponse, 0, len(rulesList))
	for _, rule := range rulesList {
		resp = append(resp, ruleToResponse(rule))
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *RulesHandler) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var req CreateRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Validate
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.EventType == "" {
		writeError(w, http.StatusBadRequest, "event_type is required")
		return
	}
	if len(req.Actions) == 0 {
		writeError(w, http.StatusBadRequest, "at least one action is required")
		return
	}

	// Validate event_type and actions against catalog
	if !rules.EventTypeExists(req.EventType) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown event_type: %s", req.EventType))
		return
	}
	for _, action := range req.Actions {
		if !rules.ActionTypeExists(string(action.Type)) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown action type: %s", action.Type))
			return
		}
		// Validate required params
		if err := rules.ValidateActionParams(action.Type, action.Params); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("action %s: %v", action.Type, err))
			return
		}
	}
	// Validate event params
	if err := rules.ValidateEventParams(rules.EventType(req.EventType), req.EventParams); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("event_params: %v", err))
		return
	}

	now := time.Now().Unix()
	rule := &rules.Rule{
		ID:              generateID("rule"),
		Name:            req.Name,
		EventType:       rules.EventType(req.EventType),
		EventParams:     req.EventParams,
		Actions:         req.Actions,
		Enabled:         true,
		CooldownSeconds: 3600,
		MaxFiresPerDay:  10,
		State:           make(map[string]any),
		LastFiredAt:     0,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	if req.Enabled != nil {
		rule.Enabled = *req.Enabled
	}
	if req.CooldownSeconds != nil {
		rule.CooldownSeconds = *req.CooldownSeconds
	}
	if req.MaxFiresPerDay != nil {
		rule.MaxFiresPerDay = *req.MaxFiresPerDay
	}
	if rule.EventParams == nil {
		rule.EventParams = make(map[string]any)
	}

	if err := h.Store.CreateRule(rule); err != nil {
		slog.Error("rules api: create rule failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to create rule")
		return
	}

	writeJSON(w, http.StatusCreated, ruleToResponse(rule))
}

func (h *RulesHandler) handleGetRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rule, err := h.Store.GetRule(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "rule not found")
		return
	}
	writeJSON(w, http.StatusOK, ruleToResponse(rule))
}

func (h *RulesHandler) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rule, err := h.Store.GetRule(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "rule not found")
		return
	}

	var req UpdateRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Validate event_type if provided
	if req.EventType != nil {
		if !rules.EventTypeExists(*req.EventType) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown event_type: %s", *req.EventType))
			return
		}
		rule.EventType = rules.EventType(*req.EventType)
	}

	// Validate actions if provided
	if req.Actions != nil {
		for _, action := range req.Actions {
			if !rules.ActionTypeExists(string(action.Type)) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown action type: %s", action.Type))
				return
			}
			if err := rules.ValidateActionParams(action.Type, action.Params); err != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("action %s: %v", action.Type, err))
				return
			}
		}
		rule.Actions = req.Actions
	}

	// Validate event_params if provided
	if req.EventParams != nil {
		if err := rules.ValidateEventParams(rule.EventType, req.EventParams); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("event_params: %v", err))
			return
		}
		rule.EventParams = req.EventParams
	}

	if req.Name != nil {
		rule.Name = *req.Name
	}
	if req.Enabled != nil {
		rule.Enabled = *req.Enabled
	}
	if req.CooldownSeconds != nil {
		rule.CooldownSeconds = *req.CooldownSeconds
	}
	if req.MaxFiresPerDay != nil {
		rule.MaxFiresPerDay = *req.MaxFiresPerDay
	}

	rule.UpdatedAt = time.Now().Unix()

	if err := h.Store.UpdateRule(rule); err != nil {
		slog.Error("rules api: update rule failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to update rule")
		return
	}

	writeJSON(w, http.StatusOK, ruleToResponse(rule))
}

func (h *RulesHandler) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.Store.DeleteRule(id); err != nil {
		slog.Error("rules api: delete rule failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to delete rule")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// ============================================================================
// TEST FIRE
// ============================================================================

func (h *RulesHandler) handleTestFire(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rule, err := h.Store.GetRule(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "rule not found")
		return
	}

	var req TestFireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Create a test run
	now := time.Now().Unix()
	eventRef := req.EventRef
	if eventRef == "" {
		eventRef = fmt.Sprintf("test:%d", now)
	}

	run := &rules.Run{
		RuleID:    rule.ID,
		EventRef:  eventRef,
		FiredAt:   now,
		Result:    rules.RunResultPending,
		Detail:    "",
		Alert:     false,
		TestFire:  true,
	}

	// Execute actions (same as engine but with test_fire=true)
	results := h.executeActionsForTest(r.Context(), rule, run)
	run.Detail = results.Detail
	run.Result = results.Result
	run.Alert = results.Alert

	if err := h.Store.CreateRun(run); err != nil {
		slog.Error("rules api: test fire create run failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to record test fire")
		return
	}
	// Refresh the auto-assigned run ID for the response.
	if saved, err := h.Store.GetRun(rule.ID, eventRef); err == nil {
		run = saved
	}

	writeJSON(w, http.StatusOK, RunResponse{
		ID:       strconv.FormatInt(run.ID, 10),
		RuleID:   run.RuleID,
		EventRef: run.EventRef,
		FiredAt:  run.FiredAt,
		Result:   run.Result,
		Detail:   run.Detail,
		Alert:    run.Alert,
		TestFire: run.TestFire,
	})
}

func (h *RulesHandler) executeActionsForTest(ctx context.Context, rule *rules.Rule, run *rules.Run) rules.ActionResults {
	// We need to replicate the engine's executeActions logic
	// For now, create a minimal implementation
	var details []string
	allOK := true
	anyAlert := false

	for _, action := range rule.Actions {
		var detail string
		var err error

		switch action.Type {
		case rules.ActionRaiseAlert:
			detail, err = h.executeRaiseAlertForTest(ctx, action.Params)
			if err == nil {
				anyAlert = true
			}
		case rules.ActionRunAgent:
			detail, err = h.executeRunAgentForTest(ctx, action.Params)
		case rules.ActionSendReport:
			detail, err = h.executeSendReportForTest(ctx, action.Params)
		case rules.ActionRunRestoreTest:
			detail, err = h.executeRunRestoreTestForTest(ctx)
		default:
			detail = "unknown_action"
			err = fmt.Errorf("unknown action type: %s", action.Type)
		}

		if err != nil {
			allOK = false
			details = append(details, fmt.Sprintf("%s: failed (%v)", action.Type, err))
		} else {
			details = append(details, fmt.Sprintf("%s: ok (%s)", action.Type, detail))
		}
	}

	result := rules.RunResultOK
	if !allOK {
		hasOK := len(rule.Actions) > 0
		if hasOK {
			result = rules.RunResultPartial
		} else {
			result = rules.RunResultFailed
		}
	}

	return rules.ActionResults{
		Detail: strings.Join(details, "; "),
		Result: result,
		Alert:  anyAlert,
	}
}

func (h *RulesHandler) executeRaiseAlertForTest(ctx context.Context, params map[string]any) (string, error) {
	priority := "normal"
	if p, ok := params["priority"].(string); ok {
		priority = p
	}
	return fmt.Sprintf("test alert logged (priority=%s)", priority), nil
}

func (h *RulesHandler) executeRunAgentForTest(ctx context.Context, params map[string]any) (string, error) {
	agentName, ok := params["agent"].(string)
	if !ok || agentName == "" {
		return "", fmt.Errorf("agent name required")
	}
	return fmt.Sprintf("test: would run agent %s", agentName), nil
}

func (h *RulesHandler) executeSendReportForTest(ctx context.Context, params map[string]any) (string, error) {
	transport, ok := params["transport"].(string)
	if !ok || transport == "" {
		return "", fmt.Errorf("transport required")
	}
	chatID, ok := params["chat_id"].(string)
	if !ok || chatID == "" {
		return "", fmt.Errorf("chat_id required")
	}
	return fmt.Sprintf("test: would send report via %s to %s", transport, chatID), nil
}

func (h *RulesHandler) executeRunRestoreTestForTest(ctx context.Context) (string, error) {
	return "test: would run restore test", nil
}

// ============================================================================
// RUNS
// ============================================================================

func (h *RulesHandler) handleListRuns(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	
	// Verify rule exists
	_, err := h.Store.GetRule(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "rule not found")
		return
	}

	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 200 {
			limit = v
		}
	}
	offset := 0
	if o := r.URL.Query().Get("offset"); o != "" {
		if v, err := strconv.Atoi(o); err == nil && v >= 0 {
			offset = v
		}
	}

	runs, total, err := h.Store.ListRuns(id, limit, offset)
	if err != nil {
		slog.Error("rules api: list runs failed", "error", err)
		writeError(w, http.StatusInternalServerError, "failed to list runs")
		return
	}

	resp := make([]RunResponse, 0, len(runs))
	for _, run := range runs {
		resp = append(resp, RunResponse{
				ID:       strconv.FormatInt(run.ID, 10),
			RuleID:   run.RuleID,
			EventRef: run.EventRef,
			FiredAt:  run.FiredAt,
			Result:   run.Result,
			Detail:   run.Detail,
			Alert:    run.Alert,
			TestFire: run.TestFire,
		})
	}

	writeJSON(w, http.StatusOK, ListRunsResponse{
		Runs:   resp,
		Total:  total,
		Limit:  limit,
		Offset: offset,
	})
}

// ============================================================================
// HELPERS
// ============================================================================

func ruleToResponse(rule *rules.Rule) RuleResponse {
	return RuleResponse{
		ID:              rule.ID,
		Name:            rule.Name,
		EventType:       string(rule.EventType),
		EventParams:     rule.EventParams,
		Actions:         rule.Actions,
		Enabled:         rule.Enabled,
		CooldownSeconds: rule.CooldownSeconds,
		MaxFiresPerDay:  rule.MaxFiresPerDay,
		State:           rule.State,
		LastFiredAt:     rule.LastFiredAt,
		CreatedAt:       rule.CreatedAt,
		UpdatedAt:       rule.UpdatedAt,
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func generateID(prefix string) string {
	return fmt.Sprintf("%s_%d_%s", prefix, time.Now().UnixMilli(), randomString(8))
}

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[time.Now().UnixNano()%int64(len(letters))]
		time.Sleep(time.Nanosecond)
	}
	return string(b)
}