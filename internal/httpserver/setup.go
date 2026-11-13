package httpserver

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/fauzone/kelvios/internal/agentmodes"
	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/systemsettings"
)

// ===== Types for request/response =====

type setupStateResponse struct {
	HasAdmin       bool   `json:"has_admin"`
	LanguageSet    bool   `json:"language_set"`
	TimezoneSet    bool   `json:"timezone_set"`
	SetupComplete  bool   `json:"setup_complete"`
	Stage          string `json:"stage"`
}

type preferencesRequest struct {
	Language string `json:"language,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

type setupImportRequest struct {
	Format     string              `json:"format"`
	Version    int                 `json:"version"`
	ExportedAt int64               `json:"exported_at"`
	Settings   setupImportSettings `json:"settings"`
}

type setupImportSettings struct {
	Language     string `json:"language,omitempty"`
	Timezone     string `json:"timezone,omitempty"`
	AgentPreset  string `json:"agent_preset,omitempty"`
}

type systemSettingsResponse struct {
	Language string `json:"language"`
	Timezone string `json:"timezone"`
}

type exportResponse struct {
	Format     string           `json:"format"`
	Version    int              `json:"version"`
	ExportedAt int64            `json:"exported_at"`
	Settings   exportSettings   `json:"settings"`
}

type exportSettings struct {
	Language    string `json:"language"`
	Timezone    string `json:"timezone"`
	AgentPreset string `json:"agent_preset"`
}

// ===== Helpers =====

func (s *Server) computeSetupStage(hasAdmin, langSet, tzSet, complete bool) string {
	if complete {
		return "complete"
	}
	if !langSet {
		return "language"
	}
	if !tzSet {
		return "timezone"
	}
	if !hasAdmin {
		return "admin"
	}
	return "preset"
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

// checkSetupCompleteGate returns true if setup is complete (gate closed).
// If closed, writes 409 response and returns true.
func (s *Server) checkSetupCompleteGate(w http.ResponseWriter, r *http.Request) bool {
	settings, err := systemsettings.Get(s.db)
	if err != nil {
		slog.Error("setup gate: get settings failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "setup check failed")
		return true
	}
	if settings.SetupCompletedAt > 0 {
		s.writeError(w, http.StatusConflict, "setup_complete")
		return true
	}
	return false
}

// validatePreferences validates the preferences request body.
// Returns (language, timezone, error). Empty string = not provided.
func (s *Server) validatePreferences(body []byte) (string, string, error) {
	var req preferencesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", "", err
	}
	// Reject unknown fields by checking if there are extra keys
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", "", err
	}
	allowed := map[string]bool{"language": true, "timezone": true}
	for k := range raw {
		if !allowed[k] {
			return "", "", &fieldError{field: k}
		}
	}
	if req.Language != "" && !systemsettings.ValidLanguages[req.Language] {
		return "", "", &fieldError{field: "language", reason: "invalid value"}
	}
	if req.Timezone != "" {
		if _, err := time.LoadLocation(req.Timezone); err != nil {
			return "", "", &fieldError{field: "timezone", reason: "invalid timezone"}
		}
	}
	return req.Language, req.Timezone, nil
}

type fieldError struct {
	field  string
	reason string
}

func (e *fieldError) Error() string {
	if e.reason != "" {
		return e.field + ": " + e.reason
	}
	return "unknown field: " + e.field
}

// ===== Handlers =====

// handleSetupState handles GET /api/setup/state — no auth required.
func (s *Server) handleSetupState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	hasAdmin, err := systemsettings.HasAdmin(s.db)
	if err != nil {
		slog.Error("setup state: has admin failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "setup state failed")
		return
	}

	settings, err := systemsettings.Get(s.db)
	if err != nil {
		slog.Error("setup state: get settings failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "setup state failed")
		return
	}

	langSet := settings.Language != ""
	tzSet := settings.Timezone != ""
	complete := settings.SetupCompletedAt > 0

	resp := setupStateResponse{
		HasAdmin:      hasAdmin,
		LanguageSet:   langSet,
		TimezoneSet:   tzSet,
		SetupComplete: complete,
		Stage:         s.computeSetupStage(hasAdmin, langSet, tzSet, complete),
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleSetupPreferences handles POST /api/setup/preferences.
// Gate: setup_complete=409; pre-admin no auth; post-admin requireAuth.
func (s *Server) handleSetupPreferences(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Gate: setup complete
	if s.checkSetupCompleteGate(w, r) {
		return
	}

	// Auth gate: if admin exists, require auth
	hasAdmin, err := systemsettings.HasAdmin(s.db)
	if err != nil {
		slog.Error("setup preferences: has admin failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "setup failed")
		return
	}
	if hasAdmin {
		username := s.sessionUsername(r)
		if username == "" {
			s.writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}

	// Read body
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body) == 0 {
		s.writeError(w, http.StatusBadRequest, "empty body")
		return
	}

	lang, tz, err := s.validatePreferences(body)
	if err != nil {
		if fe, ok := err.(*fieldError); ok {
			if fe.reason != "" {
				s.writeError(w, http.StatusBadRequest, fe.Error())
			} else {
				s.writeError(w, http.StatusBadRequest, "unknown field: "+fe.field)
			}
		} else {
			s.writeError(w, http.StatusBadRequest, "invalid JSON")
		}
		return
	}

	// Apply changes
	actor := s.sessionUsername(r)
	if actor == "" {
		actor = "setup"
	}

	if lang != "" {
		if err := systemsettings.SetLanguage(s.db, lang); err != nil {
			slog.Error("set language failed", "error", err)
			s.writeError(w, http.StatusInternalServerError, "set language failed")
			return
		}
		catalog.WriteAuditLog(s.db, actor, "setup.preferences", "system", "language", "ok", "language set to "+lang)
	}
	if tz != "" {
		if err := systemsettings.SetTimezone(s.db, tz); err != nil {
			slog.Error("set timezone failed", "error", err)
			s.writeError(w, http.StatusInternalServerError, "set timezone failed")
			return
		}
		catalog.WriteAuditLog(s.db, actor, "setup.preferences", "system", "timezone", "ok", "timezone set to "+tz)
	}

	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleSetupImport handles POST /api/setup/import.
// Gate: setup_complete=409; pre-admin no auth; post-admin requireAuth.
// Strict validation: format=kelvios-config, version=1, no secret keys, no extra keys.
func (s *Server) handleSetupImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Gate: setup complete
	if s.checkSetupCompleteGate(w, r) {
		return
	}

	// Auth gate: if admin exists, require auth
	hasAdmin, err := systemsettings.HasAdmin(s.db)
	if err != nil {
		slog.Error("setup import: has admin failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "setup failed")
		return
	}
	if hasAdmin {
		username := s.sessionUsername(r)
		if username == "" {
			s.writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}

	// Read and parse body
	body, err := readBody(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body) == 0 {
		s.writeError(w, http.StatusBadRequest, "empty body")
		return
	}

	// Parse raw JSON for validation (secret keys, unknown fields)
	var rawReq map[string]any
	if err := json.Unmarshal(body, &rawReq); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Validate format and version from raw
	formatVal, _ := rawReq["format"].(string)
	versionVal := rawReq["version"]
	var versionInt int
	switch v := versionVal.(type) {
	case float64:
		versionInt = int(v)
	case int:
		versionInt = v
	}
	if formatVal != "kelvios-config" {
		s.writeError(w, http.StatusBadRequest, "invalid format: must be 'kelvios-config'")
		catalog.WriteAuditLog(s.db, "setup", "setup.import", "system", "import", "error", "invalid format")
		return
	}
	if versionInt != 1 {
		s.writeError(w, http.StatusBadRequest, "unsupported version: must be 1")
		catalog.WriteAuditLog(s.db, "setup", "setup.import", "system", "import", "error", "unsupported version")
		return
	}

	// Check for secret keys recursively in the entire raw request
	if hasSecretKey(rawReq) {
		s.writeError(w, http.StatusBadRequest, "import contains secret keys")
		catalog.WriteAuditLog(s.db, "setup", "setup.import", "system", "import", "error", "secret keys present")
		return
	}

	// Validate known fields only in settings
	settingsRaw, ok := rawReq["settings"].(map[string]any)
	if !ok {
		settingsRaw = map[string]any{}
	}
	allowedSettings := map[string]bool{"language": true, "timezone": true, "agent_preset": true}
	for k := range settingsRaw {
		if !allowedSettings[k] {
			s.writeError(w, http.StatusBadRequest, "unknown field in settings: "+k)
			catalog.WriteAuditLog(s.db, "setup", "setup.import", "system", "import", "error", "unknown field: "+k)
			return
		}
	}

	// Now unmarshal into structured request for further processing
	var req setupImportRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Validate language
	if req.Settings.Language != "" && !systemsettings.ValidLanguages[req.Settings.Language] {
		s.writeError(w, http.StatusBadRequest, "invalid language")
		catalog.WriteAuditLog(s.db, "setup", "setup.import", "system", "import", "error", "invalid language")
		return
	}

	// Validate timezone
	if req.Settings.Timezone != "" {
		if _, err := time.LoadLocation(req.Settings.Timezone); err != nil {
			s.writeError(w, http.StatusBadRequest, "invalid timezone")
			catalog.WriteAuditLog(s.db, "setup", "setup.import", "system", "import", "error", "invalid timezone")
			return
		}
	}

	// Validate agent_preset: only allowed if admin exists (post-auth)
	if req.Settings.AgentPreset != "" {
		if !hasAdmin {
			s.writeError(w, http.StatusBadRequest, "agent_preset requires admin")
			catalog.WriteAuditLog(s.db, "setup", "setup.import", "system", "import", "error", "agent_preset requires admin")
			return
		}
		validPresets := map[string]bool{
			agentmodes.PresetAgentInside:    true,
			agentmodes.PresetManagedOutside: true,
			agentmodes.PresetBoth:           true,
			agentmodes.PresetNoAgent:        true,
		}
		if !validPresets[req.Settings.AgentPreset] {
			s.writeError(w, http.StatusBadRequest, "invalid agent_preset")
			catalog.WriteAuditLog(s.db, "setup", "setup.import", "system", "import", "error", "invalid agent_preset")
			return
		}
	}

	// Apply atomically
	actor := s.sessionUsername(r)
	if actor == "" {
		actor = "setup"
	}
	applied := []string{}

	if req.Settings.Language != "" {
		if err := systemsettings.SetLanguage(s.db, req.Settings.Language); err != nil {
			slog.Error("import set language failed", "error", err)
			s.writeError(w, http.StatusInternalServerError, "import failed")
			return
		}
		applied = append(applied, "language")
	}
	if req.Settings.Timezone != "" {
		if err := systemsettings.SetTimezone(s.db, req.Settings.Timezone); err != nil {
			slog.Error("import set timezone failed", "error", err)
			s.writeError(w, http.StatusInternalServerError, "import failed")
			return
		}
		applied = append(applied, "timezone")
	}
	if req.Settings.AgentPreset != "" {
		harness, mcp, _ := agentmodes.SwitchesFor(req.Settings.AgentPreset)
		if err := agentmodes.SetModes(s.db, &agentmodes.Modes{
			ResidentHarness: harness,
			ExternalMCP:     mcp,
			Preset:          req.Settings.AgentPreset,
		}); err != nil {
			slog.Error("import set agent preset failed", "error", err)
			s.writeError(w, http.StatusInternalServerError, "import failed")
			return
		}
		applied = append(applied, "agent_preset")
	}

	detail := "imported: " + strings.Join(applied, ", ")
	catalog.WriteAuditLog(s.db, actor, "setup.import", "system", "import", "ok", detail)

	s.writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"applied": applied,
	})
}

// hasSecretKey checks recursively for keys containing secret-related substrings.
func hasSecretKey(v any) bool {
	secretSubstrings := []string{"password", "secret", "token", "api_key", "apikey", "credential", "private_key", "vault"}
	return hasSecretKeyRecursive(v, secretSubstrings)
}

func hasSecretKeyRecursive(v any, substrings []string) bool {
	switch val := v.(type) {
	case map[string]any:
		for k, vv := range val {
			lower := strings.ToLower(k)
			for _, sub := range substrings {
				if strings.Contains(lower, sub) {
					return true
				}
			}
			if hasSecretKeyRecursive(vv, substrings) {
				return true
			}
		}
	case []any:
		for _, vv := range val {
			if hasSecretKeyRecursive(vv, substrings) {
				return true
			}
		}
	}
	return false
}

// handleSetupComplete handles POST /api/setup/complete — requireAuth, gate setup_complete=409.
func (s *Server) handleSetupComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Require auth
	username := s.sessionUsername(r)
	if username == "" {
		s.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Gate: setup complete
	if s.checkSetupCompleteGate(w, r) {
		return
	}

	// Mark setup complete
	ts := time.Now().Unix()
	if err := systemsettings.SetSetupCompleted(s.db, ts); err != nil {
		slog.Error("setup complete failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "setup complete failed")
		return
	}

	catalog.WriteAuditLog(s.db, username, "setup.complete", "system", "setup", "ok", "first-boot wizard completed")

	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleSystemSettingsGet handles GET /api/settings/system — requireAuth.
func (s *Server) handleSystemSettingsGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	username := s.sessionUsername(r)
	if username == "" {
		s.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	settings, err := systemsettings.Get(s.db)
	if err != nil {
		slog.Error("get system settings failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "get settings failed")
		return
	}

	resp := systemSettingsResponse{
		Language: settings.Language,
		Timezone: settings.Timezone,
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handleSystemSettingsPut handles PUT /api/settings/system — requireAuth, validation same as preferences.
func (s *Server) handleSystemSettingsPut(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	username := s.sessionUsername(r)
	if username == "" {
		s.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	body, err := readBody(r)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body) == 0 {
		s.writeError(w, http.StatusBadRequest, "empty body")
		return
	}

	lang, tz, err := s.validatePreferences(body)
	if err != nil {
		if fe, ok := err.(*fieldError); ok {
			if fe.reason != "" {
				s.writeError(w, http.StatusBadRequest, fe.Error())
			} else {
				s.writeError(w, http.StatusBadRequest, "unknown field: "+fe.field)
			}
		} else {
			s.writeError(w, http.StatusBadRequest, "invalid JSON")
		}
		return
	}

	applied := []string{}
	if lang != "" {
		if err := systemsettings.SetLanguage(s.db, lang); err != nil {
			slog.Error("set language failed", "error", err)
			s.writeError(w, http.StatusInternalServerError, "set language failed")
			return
		}
		applied = append(applied, "language")
		catalog.WriteAuditLog(s.db, username, "settings.system", "system", "language", "ok", "language set to "+lang)
	}
	if tz != "" {
		if err := systemsettings.SetTimezone(s.db, tz); err != nil {
			slog.Error("set timezone failed", "error", err)
			s.writeError(w, http.StatusInternalServerError, "set timezone failed")
			return
		}
		applied = append(applied, "timezone")
		catalog.WriteAuditLog(s.db, username, "settings.system", "system", "timezone", "ok", "timezone set to "+tz)
	}

	s.writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "applied": applied})
}

// handleSettingsExport handles GET /api/settings/export — requireAuth.
func (s *Server) handleSettingsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	username := s.sessionUsername(r)
	if username == "" {
		s.writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	settings, err := systemsettings.Get(s.db)
	if err != nil {
		slog.Error("export: get settings failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "export failed")
		return
	}

	modes, err := agentmodes.GetModes(s.db)
	if err != nil {
		slog.Error("export: get agent modes failed", "error", err)
		s.writeError(w, http.StatusInternalServerError, "export failed")
		return
	}

	resp := exportResponse{
		Format:     "kelvios-config",
		Version:    1,
		ExportedAt: time.Now().Unix(),
		Settings: exportSettings{
			Language:    settings.Language,
			Timezone:    settings.Timezone,
			AgentPreset: modes.Preset,
		},
	}

	s.writeJSON(w, http.StatusOK, resp)
}

// handleSystemSettings routes GET/PUT /api/settings/system.
func (s *Server) handleSystemSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleSystemSettingsGet(w, r)
	case http.MethodPut:
		s.handleSystemSettingsPut(w, r)
	default:
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// readBody reads the request body with a size limit.
func readBody(r *http.Request) ([]byte, error) {
	const maxBodySize = 1 << 20 // 1 MB
	r.Body = http.MaxBytesReader(nil, r.Body, maxBodySize)
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}