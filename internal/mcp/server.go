// Package mcp implements a minimal MCP server over HTTP (JSON-RPC 2.0).
//
// The MCP server exposes the same tools with a tiered permission scheme
// (read / write / destructive-requires-approval). External agents manage
// the system through this endpoint; the approval FLOW itself is FR-3.7
// (later) — this slice enforces the gate: destructive tools are REFUSED
// and the attempt is recorded.
//
// Design (Chief-set):
// - POST /mcp on the existing server (registered exactly once, behind the
//   existing bearer auth — same token as the REST API).
// - Methods: initialize (protocolVersion "2025-03-26", serverInfo name
//   "kelvios"), notifications/initialized (accept, no response body beyond
//   202), tools/list, tools/call. Unknown method → JSON-RPC error -32601.
//   Malformed JSON → -32700.
// - NO external MCP SDK (dependency discipline: single binary, stdlib only).
// - Tool registry: each tool = {name, description, inputSchema (JSON Schema
//   object), tier ∈ read|write|destructive, handler}. Handlers call the SAME
//   internal catalog/services functions the REST handlers use — NEVER an HTTP
//   loopback call to our own API.
// - Permission gate (the heart of this slice):
//   - read + write tools: execute for any authenticated caller; every
//     tools/call writes an audit row (actor = the authenticated user, action
//     mcp.tool.<name>, result ok/error, detail carries the tool tier).
//   - destructive tools: REFUSE with a JSON-RPC error (code -32001, message
//     "approval required") AND write an audit row (action mcp.tool.<name>,
//     result "rejected", detail "destructive_requires_approval") — the
//     handler NEVER runs.
// - tools/list returns ALL tools including destructive ones (clients must see
//   the catalog; the gate is at call time).
package mcp

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/fauzone/kelvios/internal/approval"
	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/compose"
	"github.com/fauzone/kelvios/internal/maintenance"
	"github.com/fauzone/kelvios/internal/power"
	"github.com/fauzone/kelvios/internal/storage"
)

// Tier represents the permission tier of a tool.
type Tier string

const (
	// TierRead allows read-only operations.
	TierRead Tier = "read"
	// TierWrite allows state-changing operations.
	TierWrite Tier = "write"
	// TierDestructive requires approval (FR-3.7) — refused in this slice.
	TierDestructive Tier = "destructive"
)

// Tool represents an MCP tool with its metadata and handler.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Tier        Tier
	Handler     ToolHandler
}

// ToolHandler executes a tool with the given arguments.
// Returns the result (JSON-serializable) or an error.
type ToolHandler func(db *sql.DB, args map[string]any, deps *Deps, requestedBy string) (any, error)

// Deps carries the dependencies tools need (same services as REST handlers).
type Deps struct {
	DB           *sql.DB
	Power        *power.Engine
	DisksFunc    func() ([]storage.Disk, error)
	ModelIDFunc  func() string // returns active model ID or "unconfigured"
	// DestroyPoolFunc tears down a pool's storage (late-bound: the driver is
	// set on the HTTP server after construction). Nil = driver not initialized.
	DestroyPoolFunc func(poolID string) error
}

// Server implements the MCP JSON-RPC 2.0 endpoint.
type Server struct {
	db           *sql.DB
	deps         *Deps
	tools        map[string]*Tool
	approvalStore *approval.ApprovalStore
}

// New creates an MCP server with the given dependencies.
// The tool registry is built here; destructive tools have stub handlers
// that are never reached because the gate refuses first.
func New(db *sql.DB, deps *Deps) *Server {
	s := &Server{
		db:            db,
		deps:          deps,
		tools:         make(map[string]*Tool),
		approvalStore: approval.NewApprovalStore(db),
	}
	s.registerTools()
	return s
}

// registerTools builds the v1 tool catalog (~11 tools).
func (s *Server) registerTools() {
	// Read tools
	s.tools["list_pools"] = &Tool{
		Name:        "list_pools",
		Description: "List all storage pools with their disk names",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Tier:        TierRead,
		Handler:     handleListPools,
	}
	s.tools["list_disks"] = &Tool{
		Name:        "list_disks",
		Description: "List all disks from inventory",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Tier:        TierRead,
		Handler:     handleListDisks,
	}
	s.tools["list_collections"] = &Tool{
		Name:        "list_collections",
		Description: "List collections for a pool",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pool_id": map[string]any{"type": "string", "description": "Pool ID"},
			},
			"required": []string{"pool_id"},
		},
		Tier:    TierRead,
		Handler: handleListCollections,
	}
	s.tools["get_maintenance_status"] = &Tool{
		Name:        "get_maintenance_status",
		Description: "Get per-collection scrub status",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Tier:        TierRead,
		Handler:     handleGetMaintenanceStatus,
	}
	s.tools["list_backup_repos"] = &Tool{
		Name:        "list_backup_repos",
		Description: "List all backup repositories",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		Tier:        TierRead,
		Handler:     handleListBackupRepos,
	}
	s.tools["list_restore_test_alerts"] = &Tool{
		Name:        "list_restore_test_alerts",
		Description: "List restore test alerts",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"resolved": map[string]any{"type": "boolean", "description": "Filter by resolved status"},
			},
		},
		Tier:    TierRead,
		Handler: handleListRestoreTestAlerts,
	}

	// Write tools
	s.tools["wake_disk"] = &Tool{
		Name:        "wake_disk",
		Description: "Wake a disk from sleep",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"disk": map[string]any{"type": "string", "description": "Disk name"},
			},
			"required": []string{"disk"},
		},
		Tier:    TierWrite,
		Handler: handleWakeDisk,
	}
	s.tools["sleep_disk"] = &Tool{
		Name:        "sleep_disk",
		Description: "Put a disk to sleep",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"disk": map[string]any{"type": "string", "description": "Disk name"},
			},
			"required": []string{"disk"},
		},
		Tier:    TierWrite,
		Handler: handleSleepDisk,
	}

	// Destructive tools (handler runs ONLY after an approved FR-3.7 request)
	s.tools["delete_pool"] = &Tool{
		Name:        "delete_pool",
		Description: "Delete a pool (destructive — requires human approval via the FR-3.7 gate)",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pool_id": map[string]any{"type": "string", "description": "Pool ID"},
			},
			"required": []string{"pool_id"},
		},
		Tier: TierDestructive,
		Handler: func(db *sql.DB, args map[string]any, deps *Deps, _ string) (any, error) {
			// Mirrors REST handleDeletePool: existence check, driver
			// teardown, then catalog removal. Reached only with an
			// approved, matching approval request (gate enforces).
			poolID, _ := args["pool_id"].(string)
			if poolID == "" {
				return nil, fmt.Errorf("pool_id is required")
			}
			pool, _, err := catalog.GetPool(db, poolID)
			if err != nil {
				return nil, err
			}
			if pool == nil {
				return nil, fmt.Errorf("pool not found")
			}
			if deps.DestroyPoolFunc == nil {
				return nil, fmt.Errorf("storage driver not initialized")
			}
			if err := deps.DestroyPoolFunc(poolID); err != nil {
				return nil, err
			}
			if err := catalog.DeletePool(db, poolID); err != nil {
				return nil, err
			}
			return map[string]any{"deleted": poolID}, nil
		},
	}

	// Compose tools
	s.tools["compose_propose"] = &Tool{
		Name:        "compose_propose",
		Description: "Propose a new compose artifact (always created as pending v1; agent cannot set status)",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Artifact name"},
				"kind": map[string]any{"type": "string", "description": "Artifact kind (report_schedule, automation_rule, recipe, tool_chain)"},
				"spec": map[string]any{"type": "object", "description": "Artifact specification (validated per kind)"},
			},
			"required": []string{"name", "kind", "spec"},
		},
		Tier:    TierWrite,
		Handler: handleComposePropose,
	}
	s.tools["compose_list"] = &Tool{
		Name:        "compose_list",
		Description: "List compose artifacts with optional status and kind filters",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{"type": "string", "description": "Filter by status (pending, active, disabled)"},
				"kind":   map[string]any{"type": "string", "description": "Filter by kind (report_schedule, automation_rule, recipe, tool_chain)"},
			},
		},
		Tier:    TierRead,
		Handler: handleComposeList,
	}
	s.tools["compose_get"] = &Tool{
		Name:        "compose_get",
		Description: "Get a compose artifact by ID including all versions",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "Artifact ID"},
			},
			"required": []string{"id"},
		},
		Tier:    TierRead,
		Handler: handleComposeGet,
	}
}

// JSON-RPC 2.0 request/response types.

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      any         `json:"id,omitempty"`
	Result  any         `json:"result,omitempty"`
	Error   *jsonRPCError `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// JSON-RPC error codes.
const (
	errParseError     = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternalError  = -32603
	// errApprovalRequired is the custom code for destructive tool refusal.
	errApprovalRequired = -32001
)

// HandleMCP handles POST /mcp requests.
// The caller (httpserver) is responsible for auth; actor is passed explicitly.
func (s *Server) HandleMCP(w http.ResponseWriter, r *http.Request, actor string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req jsonRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeError(w, nil, errParseError, "parse error", nil)
		return
	}

	// Validate JSON-RPC version.
	if req.JSONRPC != "2.0" {
		s.writeError(w, req.ID, errInvalidRequest, "invalid request: jsonrpc must be 2.0", nil)
		return
	}

	switch req.Method {
	case "initialize":
		s.handleInitialize(w, req.ID)
	case "notifications/initialized":
		// Accept: no response body beyond 202.
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		s.handleToolsList(w, req.ID)
	case "tools/call":
		s.handleToolsCall(w, r, req.ID, req.Params, actor)
	default:
		s.writeError(w, req.ID, errMethodNotFound, "method not found", nil)
	}
}

func (s *Server) handleInitialize(w http.ResponseWriter, id any) {
	result := map[string]any{
		"protocolVersion": "2025-03-26",
		"serverInfo": map[string]any{
			"name":    "kelvios",
			"version": "1.0.0",
		},
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
	}
	s.writeResult(w, id, result)
}

func (s *Server) handleToolsList(w http.ResponseWriter, id any) {
	// Return ALL tools including destructive ones (clients must see the catalog;
	// the gate is at call time).
	tools := make([]map[string]any, 0, len(s.tools))
	for _, t := range s.tools {
		tools = append(tools, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	s.writeResult(w, id, map[string]any{"tools": tools})
}

type toolsCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

func (s *Server) handleToolsCall(w http.ResponseWriter, r *http.Request, id any, rawParams json.RawMessage, actor string) {
	var params toolsCallParams
	if len(rawParams) > 0 {
		if err := json.Unmarshal(rawParams, &params); err != nil {
			s.writeError(w, id, errInvalidParams, "invalid params", nil)
			return
		}
	}
	if params.Name == "" {
		s.writeError(w, id, errInvalidParams, "invalid params: name is required", nil)
		return
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}

	tool, ok := s.tools[params.Name]
	if !ok {
		s.writeError(w, id, errMethodNotFound, fmt.Sprintf("tool not found: %s", params.Name), nil)
		return
	}

	action := "mcp.tool." + tool.Name

	// Get model ID for audit logging (active LLM registry model or "unconfigured")
	modelID := "unconfigured"
	if s.deps != nil && s.deps.ModelIDFunc != nil {
		modelID = s.deps.ModelIDFunc()
	}
	requestedBy := actor
	if modelID != "unconfigured" {
		requestedBy = fmt.Sprintf("%s:%s", actor, modelID)
	}

	// Permission gate: destructive tools require approval.
	if tool.Tier == TierDestructive {
		// Check if request includes an approval_id
		approvalID := ""
		if v, ok := params.Arguments["approval_id"].(string); ok {
			approvalID = v
			// approval_id is a gate field, not a tool argument: remove it so
			// argument matching compares pure tool arguments (the stored
			// request never contains it) and the handler never sees it.
			delete(params.Arguments, "approval_id")
		}

		if approvalID == "" {
			// No approval_id: create a pending approval request
			req, err := s.approvalStore.Create(tool.Name, params.Arguments, requestedBy)
			if err != nil {
				slog.Error("create approval request failed", "tool", tool.Name, "error", err)
				s.writeError(w, id, errInternalError, "approval request failed", nil)
				return
			}
			// Write audit row for the approval request creation
			catalog.WriteAuditLog(s.db, actor, action, "mcp_approval", req.ID, "pending", fmt.Sprintf("tool=%s requested_by=%s", tool.Name, requestedBy))
			// Return -32001 with approval_id and status pending
			s.writeError(w, id, errApprovalRequired, "approval required", map[string]any{
				"approval_id": req.ID,
				"status":      "pending",
			})
			return
		}

		// approval_id provided: validate it
		req, err := s.approvalStore.Get(approvalID)
		if err != nil {
			slog.Error("get approval request failed", "approval_id", approvalID, "error", err)
			s.writeError(w, id, errInternalError, "approval validation failed", nil)
			return
		}
		if req == nil {
			s.writeError(w, id, errApprovalRequired, "approval required", map[string]any{
				"approval_id": approvalID,
				"status":      "not_found",
				"reason":      "approval request not found",
			})
			return
		}
		if req.Status != approval.ApprovalStatusApproved {
			reason := "approval not approved"
			if req.Status == approval.ApprovalStatusRejected {
				reason = "approval rejected"
			} else if req.Status == approval.ApprovalStatusExpired {
				reason = "approval expired"
			}
			s.writeError(w, id, errApprovalRequired, "approval required", map[string]any{
				"approval_id": approvalID,
				"status":      string(req.Status),
				"reason":      reason,
			})
			return
		}
		if req.ConsumedAt != 0 {
			s.writeError(w, id, errApprovalRequired, "approval required", map[string]any{
				"approval_id": approvalID,
				"status":      "consumed",
				"reason":      "approval already consumed",
			})
			return
		}
		if !req.MatchesToolAndArgs(tool.Name, params.Arguments) {
			s.writeError(w, id, errApprovalRequired, "approval required", map[string]any{
				"approval_id": approvalID,
				"status":      "mismatch",
				"reason":      "tool or arguments do not match approval request",
			})
			return
		}

		// Approved and matches: execute the handler
		result, err := tool.Handler(s.db, params.Arguments, s.deps, requestedBy)
		if err != nil {
			catalog.WriteAuditLog(s.db, requestedBy, action, "mcp_tool", tool.Name, "error", fmt.Sprintf("%s (approved:%s)", string(tool.Tier), approvalID))
			slog.Error("mcp tool call failed", "tool", tool.Name, "error", err)
			s.writeError(w, id, errInternalError, err.Error(), nil)
			return
		}

		// Mark approval as consumed
		if err := s.approvalStore.Consume(approvalID); err != nil {
			slog.Error("mark approval consumed failed", "approval_id", approvalID, "error", err)
		}

		// Write audit row with agent + model identity
		catalog.WriteAuditLog(s.db, requestedBy, action, "mcp_tool", tool.Name, "ok", fmt.Sprintf("%s (approved:%s)", string(tool.Tier), approvalID))
		s.writeResult(w, id, map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": toJSONString(result)},
			},
		})
		return
	}

	// Read + write tools: execute for any authenticated caller.
	result, err := tool.Handler(s.db, params.Arguments, s.deps, requestedBy)
	if err != nil {
		catalog.WriteAuditLog(s.db, requestedBy, action, "mcp_tool", tool.Name, "error", string(tool.Tier))
		slog.Error("mcp tool call failed", "tool", tool.Name, "error", err)
		s.writeError(w, id, errInternalError, err.Error(), nil)
		return
	}

	catalog.WriteAuditLog(s.db, requestedBy, action, "mcp_tool", tool.Name, "ok", string(tool.Tier))
	s.writeResult(w, id, map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": toJSONString(result)},
		},
	})
}

func (s *Server) writeResult(w http.ResponseWriter, id any, result any) {
	w.Header().Set("Content-Type", "application/json")
	resp := jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: result}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) writeError(w http.ResponseWriter, id any, code int, message string, data any) {
	w.Header().Set("Content-Type", "application/json")
	resp := jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: message, Data: data},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func toJSONString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// --- Tool handlers (call the SAME internal services as REST handlers) ---

func handleListPools(db *sql.DB, args map[string]any, deps *Deps, _ string) (any, error) {
	pools, err := catalog.ListPools(db)
	if err != nil {
		return nil, fmt.Errorf("list pools: %w", err)
	}
	if pools == nil {
		pools = []*catalog.PoolRow{}
	}
	return map[string]any{"pools": pools}, nil
}

func handleListDisks(db *sql.DB, args map[string]any, deps *Deps, _ string) (any, error) {
	if deps == nil || deps.DisksFunc == nil {
		return nil, fmt.Errorf("disk inventory not available")
	}
	disks, err := deps.DisksFunc()
	if err != nil {
		return nil, fmt.Errorf("disk inventory: %w", err)
	}
	if disks == nil {
		disks = []storage.Disk{}
	}
	return map[string]any{"disks": disks}, nil
}

func handleListCollections(db *sql.DB, args map[string]any, deps *Deps, _ string) (any, error) {
	poolID, _ := args["pool_id"].(string)
	if poolID == "" {
		return nil, fmt.Errorf("pool_id is required")
	}
	// Verify pool exists (same as REST handler).
	pool, _, err := catalog.GetPool(db, poolID)
	if err != nil {
		return nil, fmt.Errorf("get pool: %w", err)
	}
	if pool == nil {
		return nil, fmt.Errorf("pool not found")
	}
	collections, err := catalog.ListCollections(db, poolID)
	if err != nil {
		return nil, fmt.Errorf("list collections: %w", err)
	}
	if collections == nil {
		collections = []*catalog.CollectionRow{}
	}
	return map[string]any{"collections": collections}, nil
}

func handleGetMaintenanceStatus(db *sql.DB, args map[string]any, deps *Deps, _ string) (any, error) {
	statuses, err := maintenance.Status(db)
	if err != nil {
		return nil, fmt.Errorf("maintenance status: %w", err)
	}
	if statuses == nil {
		statuses = []maintenance.MaintenanceStatus{}
	}
	return map[string]any{"statuses": statuses}, nil
}

func handleListBackupRepos(db *sql.DB, args map[string]any, deps *Deps, _ string) (any, error) {
	repos, err := catalog.ListBackupRepos(db)
	if err != nil {
		return nil, fmt.Errorf("list backup repos: %w", err)
	}
	if repos == nil {
		repos = []*catalog.BackupRepoRow{}
	}
	return map[string]any{"repos": repos}, nil
}

func handleListRestoreTestAlerts(db *sql.DB, args map[string]any, deps *Deps, _ string) (any, error) {
	resolvedFilter := -1
	if v, ok := args["resolved"]; ok {
		if b, ok := v.(bool); ok {
			if b {
				resolvedFilter = 1
			} else {
				resolvedFilter = 0
			}
		}
	}

	query := `SELECT id, run_id, repo_id, snapshot_id, rel_path, reason, created_at, resolved FROM restore_test_alerts`
	queryArgs := []any{}
	if resolvedFilter >= 0 {
		query += ` WHERE resolved = ?`
		queryArgs = append(queryArgs, resolvedFilter)
	}
	query += ` ORDER BY created_at DESC, rowid DESC LIMIT 500`

	rows, err := db.Query(query, queryArgs...)
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	defer rows.Close()

	type alertRow struct {
		ID         string `json:"id"`
		RunID      string `json:"run_id"`
		RepoID     string `json:"repo_id"`
		SnapshotID string `json:"snapshot_id"`
		RelPath    string `json:"rel_path"`
		Reason     string `json:"reason"`
		CreatedAt  int64  `json:"created_at"`
		Resolved   int    `json:"resolved"`
	}
	var alerts []alertRow
	for rows.Next() {
		var a alertRow
		if err := rows.Scan(&a.ID, &a.RunID, &a.RepoID, &a.SnapshotID, &a.RelPath, &a.Reason, &a.CreatedAt, &a.Resolved); err != nil {
			continue
		}
		alerts = append(alerts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan alerts: %w", err)
	}
	if alerts == nil {
		alerts = []alertRow{}
	}
	return map[string]any{"alerts": alerts}, nil
}

func handleWakeDisk(db *sql.DB, args map[string]any, deps *Deps, _ string) (any, error) {
	disk, _ := args["disk"].(string)
	if disk == "" {
		return nil, fmt.Errorf("disk is required")
	}
	if deps == nil || deps.Power == nil {
		return nil, fmt.Errorf("power engine not available")
	}
	entry, ok := deps.Power.Wake(disk)
	if !ok {
		return nil, fmt.Errorf("disk not found: %s", disk)
	}
	return map[string]any{"disk": entry}, nil
}

func handleSleepDisk(db *sql.DB, args map[string]any, deps *Deps, _ string) (any, error) {
	disk, _ := args["disk"].(string)
	if disk == "" {
		return nil, fmt.Errorf("disk is required")
	}
	if deps == nil || deps.Power == nil {
		return nil, fmt.Errorf("power engine not available")
	}
	entry, ok, err := deps.Power.Sleep(disk)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("disk not found: %s", disk)
	}
	return map[string]any{"disk": entry}, nil
}

// --- Compose tool handlers ---

func handleComposePropose(db *sql.DB, args map[string]any, deps *Deps, requestedBy string) (any, error) {
	name, _ := args["name"].(string)
	kind, _ := args["kind"].(string)
	specRaw, ok := args["spec"]
	if !ok {
		return nil, fmt.Errorf("spec is required")
	}

	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	if kind == "" {
		return nil, fmt.Errorf("kind is required")
	}

	specJSON, err := json.Marshal(specRaw)
	if err != nil {
		return nil, fmt.Errorf("marshal spec: %w", err)
	}

	// Validate spec before creating artifact
	if err := compose.ValidateSpec(kind, specJSON); err != nil {
		return nil, fmt.Errorf("invalid spec: %w", err)
	}

	artifact, err := compose.CreateArtifact(db, name, kind, specJSON, requestedBy, time.Now())
	if err != nil {
		return nil, fmt.Errorf("create artifact: %w", err)
	}

	// Audit log
	catalog.WriteAuditLog(db, requestedBy, "compose.create", "compose", artifact.ID, "ok", fmt.Sprintf("kind=%s name=%s", kind, name))

	return map[string]any{
		"id":               artifact.ID,
		"name":             artifact.Name,
		"kind":             artifact.Kind,
		"status":           "pending",
		"current_version":  artifact.CurrentVersion,
	}, nil
}

func handleComposeList(db *sql.DB, args map[string]any, deps *Deps, _ string) (any, error) {
	status, _ := args["status"].(string)
	kind, _ := args["kind"].(string)

	artifacts, err := compose.ListArtifacts(db, status, kind)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}

	result := make([]map[string]any, 0, len(artifacts))
	for _, a := range artifacts {
		var specObj any
		if len(a.Spec) > 0 {
			if err := json.Unmarshal(a.Spec, &specObj); err != nil {
				specObj = string(a.Spec)
			}
		}
		result = append(result, map[string]any{
			"id":               a.ID,
			"name":             a.Name,
			"kind":             a.Kind,
			"status":           a.Status,
			"current_version":  a.CurrentVersion,
			"spec":             specObj,
			"created_by":       a.CreatedBy,
			"created_at":       a.CreatedAt,
			"updated_at":       a.UpdatedAt,
		})
	}

	return map[string]any{"artifacts": result}, nil
}

func handleComposeGet(db *sql.DB, args map[string]any, deps *Deps, _ string) (any, error) {
	id, _ := args["id"].(string)
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}

	artifact, err := compose.GetArtifact(db, id)
	if err != nil {
		return nil, fmt.Errorf("get artifact: %w", err)
	}
	if artifact == nil {
		return nil, fmt.Errorf("artifact not found")
	}

	versions, err := compose.ListVersions(db, id)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w", err)
	}

	var specObj any
	if len(artifact.Spec) > 0 {
		if err := json.Unmarshal(artifact.Spec, &specObj); err != nil {
			specObj = string(artifact.Spec)
		}
	}

	versionList := make([]map[string]any, 0, len(versions))
	for _, v := range versions {
		var vSpecObj any
		if len(v.Spec) > 0 {
			if err := json.Unmarshal(v.Spec, &vSpecObj); err != nil {
				vSpecObj = string(v.Spec)
			}
		}
		versionList = append(versionList, map[string]any{
			"artifact_id": v.ArtifactID,
			"version":     v.Version,
			"spec":        vSpecObj,
			"created_by":  v.CreatedBy,
			"created_at":  v.CreatedAt,
		})
	}

	return map[string]any{
		"artifact": map[string]any{
			"id":               artifact.ID,
			"name":             artifact.Name,
			"kind":             artifact.Kind,
			"status":           artifact.Status,
			"current_version":  artifact.CurrentVersion,
			"spec":             specObj,
			"created_by":       artifact.CreatedBy,
			"created_at":       artifact.CreatedAt,
			"updated_at":       artifact.UpdatedAt,
			"versions":         versionList,
		},
	}, nil
}


