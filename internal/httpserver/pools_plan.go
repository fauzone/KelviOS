package httpserver

import (
	"encoding/json"
	"net/http"

	"github.com/fauzone/kelvios/internal/storage"
)

// planPoolRequest is the body of POST /api/pools/plan (WS25, FR-1.2):
// the wizard asks for the capacity consequences of a mechanism BEFORE
// anything is created, so the user confirms with the numbers visible.
type planPoolRequest struct {
	Mechanism string             `json:"mechanism"`
	Disks     []storage.PlanDisk `json:"disks"`
	Tier      string             `json:"tier"`
}

// handlePoolPlan computes a pool plan via storage.PlanPool and creates
// nothing. It is registered as the exact pattern /api/pools/plan, which
// the ServeMux prefers over the /api/pools/ subtree (handlePoolByID) —
// the wizard tests prove a plan call never lands in the pool-by-ID
// handler.
func (s *Server) handlePoolPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	var req planPoolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
		return
	}

	if req.Tier != "" {
		tier := storage.Tier(req.Tier)
		if tier != storage.TierHot && tier != storage.TierWarm && tier != storage.TierCold {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid tier: must be hot, warm, or cold"})
			return
		}
	}

	mechanism := storage.Mechanism(req.Mechanism)
	if mechanism == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "mechanism is mandatory (single, mirror, parity-scheduled, stripe, or zfs)"})
		return
	}
	if !isValidMechanism(mechanism) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid mechanism: must be one of single, mirror, parity-scheduled, stripe, zfs"})
		return
	}

	plan, err := storage.PlanPool(mechanism, req.Disks)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"plan": plan})
}
