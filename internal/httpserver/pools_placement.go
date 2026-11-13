// Package httpserver handles the placement plan endpoint (FR-1.5).
// GET /api/pools/{id}/placement?collection=<collID>&operation=read|write
// Returns the planned wake disks without actually waking anything.
package httpserver

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/fauzone/kelvios/internal/catalog"
	"github.com/fauzone/kelvios/internal/placement"
)

type placementPlanResponse struct {
	PoolID       string   `json:"pool_id"`
	CollectionID string   `json:"collection_id"`
	Operation    string   `json:"operation"`
	WakeDisks    []string `json:"wake_disks"`
}

// handlePoolPlacement handles GET /api/pools/{id}/placement
// Query params: collection (required), operation (required: read|write)
// Returns 404 for unknown pool/collection, 400 for unknown operation or
// missing collection disk_name, 401 for unauthenticated.
func (s *Server) handlePoolPlacement(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Path is /api/pools/{id}/placement
	path := strings.TrimPrefix(r.URL.Path, "/api/pools/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	// Extract pool ID (everything before /placement)
	const suffix = "/placement"
	if !strings.HasSuffix(path, suffix) {
		http.NotFound(w, r)
		return
	}
	poolID := strings.TrimSuffix(path, suffix)
	if poolID == "" || strings.Contains(poolID, "/") {
		http.NotFound(w, r)
		return
	}

	// Query params
	collectionID := r.URL.Query().Get("collection")
	operationStr := r.URL.Query().Get("operation")

	if collectionID == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection query parameter is required"})
		return
	}
	if operationStr == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "operation query parameter is required (read|write)"})
		return
	}

	// Validate operation
	var op placement.Operation
	switch operationStr {
	case "read":
		op = placement.OpRead
	case "write":
		op = placement.OpWrite
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid operation: must be read or write"})
		return
	}

	// Get pool from catalog
	poolRow, diskNames, err := catalog.GetPool(s.db, poolID)
	if err != nil {
		slog.Error("get pool for placement failed", "pool_id", poolID, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get pool failed"})
		return
	}
	if poolRow == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "pool not found"})
		return
	}

	// Get collection from catalog
	coll, err := catalog.GetCollection(s.db, collectionID)
	if err != nil {
		slog.Error("get collection for placement failed", "collection_id", collectionID, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "get collection failed"})
		return
	}
	if coll == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection not found"})
		return
	}

	// Verify collection belongs to this pool
	if coll.PoolID != poolID {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "collection does not belong to this pool"})
		return
	}

	// Build placement.Pool from catalog data
	poolDisks := make([]placement.PoolDisk, 0, len(diskNames))
	for _, dn := range diskNames {
		// We need disk sizes. For now, we can't get sizes from catalog alone
		// without inventory. But the brief says to get disk pool from catalog
		// (pool_disks) + DiskName from collection. The parity disk rule needs
		// sizes. We'll need to get sizes from inventory or storage.
		// For the endpoint, we'll use storage.Inventory to get sizes.
		poolDisks = append(poolDisks, placement.PoolDisk{Name: dn, SizeBytes: 0})
	}

	// Try to enrich with sizes from inventory
	disks, err := s.disksFunc()
	if err == nil {
		sizeMap := make(map[string]int64)
		for _, d := range disks {
			sizeMap[d.Name] = d.SizeBytes
		}
		for i := range poolDisks {
			if sz, ok := sizeMap[poolDisks[i].Name]; ok {
				poolDisks[i].SizeBytes = sz
			}
		}
	}

	p := placement.Pool{
		ID:        poolRow.ID,
		Tier:      poolRow.Tier,
		Mechanism: poolRow.Mechanism,
		Disks:     poolDisks,
	}

	c := placement.Collection{DiskName: coll.DiskName}

	wakeDisks, err := placement.Decide(p, c, op)
	if err != nil {
		// placement unknown (empty DiskName) -> 400
		if err.Error() == "placement unknown: collection has no disk assigned" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "placement unknown"})
			return
		}
		// Other errors (unknown op, no disks for parity) -> 400
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	resp := placementPlanResponse{
		PoolID:       poolID,
		CollectionID: collectionID,
		Operation:    operationStr,
		WakeDisks:    wakeDisks,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
