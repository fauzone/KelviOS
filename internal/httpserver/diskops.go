package httpserver

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/diskops"
)

// diskReplacePlanRequest is the JSON body for POST /api/pools/{id}/disk-replace/plan
type diskReplacePlanRequest struct {
	FailingDisk     string `json:"failing_disk"`
	ReplacementDisk string `json:"replacement_disk"`
}

// diskExpandPlanRequest is the JSON body for POST /api/pools/{id}/disk-expand/plan
type diskExpandPlanRequest struct {
	NewDisk string `json:"new_disk"`
}

// handleDiskReplacePlan handles POST /api/pools/{id}/disk-replace/plan
// Body: {failing_disk, replacement_disk}
// Reads pool disks from catalog; calls diskops.BuildPlan -> 200 {plan}
// Plan error -> 400 + audit error row; operation recorded with status "planned"
func (s *Server) handleDiskReplacePlan(w http.ResponseWriter, r *http.Request, poolID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Resolve pool
	pool, poolDisks, err := catalog.GetPool(s.db, poolID)
	if err != nil {
		slog.Error("get pool for disk replace plan failed", "pool_id", poolID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "error", "get pool failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get pool failed")
		return
	}
	if pool == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "error", "pool not found")
		s.writeJSONError(w, http.StatusNotFound, "pool not found")
		return
	}

	// Decode JSON body
	var req diskReplacePlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Validate required fields
	if req.FailingDisk == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "error", "failing_disk is required")
		s.writeJSONError(w, http.StatusBadRequest, "failing_disk is required")
		return
	}
	if req.ReplacementDisk == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "error", "replacement_disk is required")
		s.writeJSONError(w, http.StatusBadRequest, "replacement_disk is required")
		return
	}

	// Convert pool disks to diskops.DiskRef
	diskRefs := make([]diskops.DiskRef, len(poolDisks))
	for i, name := range poolDisks {
		// We need to get the size for each disk from the catalog
		// For now, we'll query the disk size from the devices table or use a placeholder
		// In a real implementation, we'd fetch actual disk sizes
		size := int64(0)
		err := s.db.QueryRow(`SELECT size_bytes FROM devices WHERE name = ?`, name).Scan(&size)
		if err != nil {
			slog.Warn("disk size not found in catalog, using 0", "disk", name, "error", err)
		}
		diskRefs[i] = diskops.DiskRef{Name: name, SizeBytes: size}
	}

	// Build plan using diskops package
	plan, err := diskops.BuildPlan(poolID, diskRefs,
		diskops.DiskRef{Name: req.FailingDisk, SizeBytes: 0}, // Size will be validated against pool disk
		diskops.DiskRef{Name: req.ReplacementDisk, SizeBytes: 0},
		pool.Mechanism)

	if err != nil {
		// Audit error with error detail (no disk names/paths in detail per audit policy)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "error", err.Error())
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Persist operation with status "planned"
	detailJSON := buildDiskOpDetail(plan)
	diskOp := &catalog.DiskOpRow{
		PoolID:          poolID,
		Kind:            "replace",
		Strategy:        string(plan.Strategy),
		FailingDisk:     req.FailingDisk,
		ReplacementDisk: req.ReplacementDisk,
		Status:          "planned",
		Detail:          detailJSON,
	}
	if err := catalog.InsertDiskOp(s.db, diskOp); err != nil {
		slog.Error("insert disk operation failed", "pool_id", poolID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "error", "catalog persist failed")
		s.writeJSONError(w, http.StatusInternalServerError, "catalog persist failed")
		return
	}

	// Audit success (counts/strategy only, no disk names/paths)
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "ok",
		fmt.Sprintf("kind=replace strategy=%s steps=%d delta_bytes=%d", plan.Strategy, len(plan.Steps), plan.Preview.DeltaBytes))

	// Response
	response := map[string]any{
		"plan": map[string]any{
			"pool_id":          plan.PoolID,
			"pool_mechanism":   plan.PoolMechanism,
			"failing_disk":     plan.FailingDisk,
			"replacement_disk": plan.ReplacementDisk,
			"strategy":         plan.Strategy,
			"steps":            plan.Steps,
			"preview":          plan.Preview,
		},
		"operation": map[string]any{
			"id":               diskOp.ID,
			"pool_id":          diskOp.PoolID,
			"kind":             diskOp.Kind,
			"strategy":         diskOp.Strategy,
			"failing_disk":     diskOp.FailingDisk,
			"replacement_disk": diskOp.ReplacementDisk,
			"status":           diskOp.Status,
			"detail":           diskOp.Detail,
			"created_at":       diskOp.CreatedAt.Unix(),
		},
	}
	s.writeJSON(w, http.StatusOK, response)
}

// handleDiskExpandPlan handles POST /api/pools/{id}/disk-expand/plan
// Body: {new_disk}
// Calls diskops.BuildExpansionPlan -> 200 {plan}
// Error -> 400 + audit error row; operation recorded with status "planned"
func (s *Server) handleDiskExpandPlan(w http.ResponseWriter, r *http.Request, poolID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Resolve pool
	pool, poolDisks, err := catalog.GetPool(s.db, poolID)
	if err != nil {
		slog.Error("get pool for disk expand plan failed", "pool_id", poolID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "error", "get pool failed")
		s.writeJSONError(w, http.StatusInternalServerError, "get pool failed")
		return
	}
	if pool == nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "error", "pool not found")
		s.writeJSONError(w, http.StatusNotFound, "pool not found")
		return
	}

	// Decode JSON body
	var req diskExpandPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "error", "invalid JSON")
		s.writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	// Validate required fields
	if req.NewDisk == "" {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "error", "new_disk is required")
		s.writeJSONError(w, http.StatusBadRequest, "new_disk is required")
		return
	}

	// Convert pool disks to diskops.DiskRef
	diskRefs := make([]diskops.DiskRef, len(poolDisks))
	for i, name := range poolDisks {
		size := int64(0)
		err := s.db.QueryRow(`SELECT size_bytes FROM devices WHERE name = ?`, name).Scan(&size)
		if err != nil {
			slog.Warn("disk size not found in catalog, using 0", "disk", name, "error", err)
		}
		diskRefs[i] = diskops.DiskRef{Name: name, SizeBytes: size}
	}

	// Build plan using diskops package
	plan, err := diskops.BuildExpansionPlan(poolID, diskRefs,
		diskops.DiskRef{Name: req.NewDisk, SizeBytes: 0},
		pool.Mechanism)

	if err != nil {
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "error", err.Error())
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Persist operation with status "planned"
	detailJSON := buildDiskOpDetail(plan)
	diskOp := &catalog.DiskOpRow{
		PoolID:          poolID,
		Kind:            "expand",
		Strategy:        string(plan.Strategy),
		ReplacementDisk: req.NewDisk, // For expand, we store new_disk in replacement_disk field
		Status:          "planned",
		Detail:          detailJSON,
	}
	if err := catalog.InsertDiskOp(s.db, diskOp); err != nil {
		slog.Error("insert disk operation failed", "pool_id", poolID, "error", err)
		catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "error", "catalog persist failed")
		s.writeJSONError(w, http.StatusInternalServerError, "catalog persist failed")
		return
	}

	// Audit success (counts/strategy only, no disk names/paths)
	catalog.WriteAuditLog(s.db, actorFromRequest(r), "diskops.plan", "pool", poolID, "ok",
		fmt.Sprintf("kind=expand strategy=%s steps=%d delta_bytes=%d", plan.Strategy, len(plan.Steps), plan.Preview.DeltaBytes))

	// Response
	response := map[string]any{
		"plan": map[string]any{
			"pool_id":        plan.PoolID,
			"pool_mechanism": plan.PoolMechanism,
			"new_disk":       plan.NewDisk,
			"strategy":       plan.Strategy,
			"steps":          plan.Steps,
			"preview":        plan.Preview,
		},
		"operation": map[string]any{
			"id":               diskOp.ID,
			"pool_id":          diskOp.PoolID,
			"kind":             diskOp.Kind,
			"strategy":         diskOp.Strategy,
			"replacement_disk": diskOp.ReplacementDisk,
			"status":           diskOp.Status,
			"detail":           diskOp.Detail,
			"created_at":       diskOp.CreatedAt.Unix(),
		},
	}
	s.writeJSON(w, http.StatusOK, response)
}

// handleListDiskOperations handles GET /api/pools/{id}/disk-operations
// Returns {operations, total}
func (s *Server) handleListDiskOperations(w http.ResponseWriter, r *http.Request, poolID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Verify pool exists
	pool, _, err := catalog.GetPool(s.db, poolID)
	if err != nil {
		slog.Error("get pool for list disk operations failed", "pool_id", poolID, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "get pool failed")
		return
	}
	if pool == nil {
		s.writeJSONError(w, http.StatusNotFound, "pool not found")
		return
	}

	// Parse query params
	limit := parseIntQuery(r, "limit", 100)
	offset := parseIntQuery(r, "offset", 0)

	ops, total, err := catalog.ListDiskOps(s.db, poolID, limit, offset)
	if err != nil {
		slog.Error("list disk operations failed", "pool_id", poolID, "error", err)
		s.writeJSONError(w, http.StatusInternalServerError, "list disk operations failed")
		return
	}

	// Convert to response format
	result := make([]map[string]any, 0, len(ops))
	for _, op := range ops {
		result = append(result, map[string]any{
			"id":               op.ID,
			"pool_id":          op.PoolID,
			"kind":             op.Kind,
			"strategy":         op.Strategy,
			"failing_disk":     op.FailingDisk,
			"replacement_disk": op.ReplacementDisk,
			"status":           op.Status,
			"detail":           op.Detail,
			"created_at":       op.CreatedAt.Unix(),
		})
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"operations": result,
		"total":      total,
	})
}

// buildDiskOpDetail creates a JSON summary for audit (counts/strategy only, no disk names/paths)
func buildDiskOpDetail(plan diskops.ReplacePlan) string {
	detail := map[string]any{
		"strategy":    plan.Strategy,
		"steps_count": len(plan.Steps),
		"old_usable":  plan.Preview.OldUsableBytes,
		"new_usable":  plan.Preview.NewUsableBytes,
		"delta_bytes": plan.Preview.DeltaBytes,
	}
	data, _ := json.Marshal(detail)
	return string(data)
}
