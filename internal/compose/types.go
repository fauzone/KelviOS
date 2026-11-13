package compose

import (
	"encoding/json"
)

// Kind constants for compose artifacts.
const (
	KindReportSchedule   = "report_schedule"
	KindAutomationRule   = "automation_rule"
	KindRecipe           = "recipe"
	KindToolChain        = "tool_chain"
)

// Status constants for compose artifacts.
const (
	StatusPending  = "pending"
	StatusActive   = "active"
	StatusDisabled = "disabled"
)

// Artifact represents a compose artifact.
type Artifact struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Kind            string          `json:"kind"`
	Status          string          `json:"status"`
	CurrentVersion  int             `json:"current_version"`
	Spec            json.RawMessage `json:"spec"`
	CreatedBy       string          `json:"created_by"`
	CreatedAt       int64           `json:"created_at"`
	UpdatedAt       int64           `json:"updated_at"`
}

// Version represents a version of a compose artifact.
type Version struct {
	ArtifactID string          `json:"artifact_id"`
	Version    int             `json:"version"`
	Spec       json.RawMessage `json:"spec"`
	CreatedBy  string          `json:"created_by"`
	CreatedAt  int64           `json:"created_at"`
}

// Run represents an execution run of a compose artifact.
type Run struct {
	ID          int64  `json:"id"`
	ArtifactID  string `json:"artifact_id"`
	Version     int    `json:"version"`
	StartedAt   int64  `json:"started_at"`
	FinishedAt  int64  `json:"finished_at"`
	Result      string `json:"result"`
	Detail      string `json:"detail"`
}

// RunResult constants.
const (
	RunResultOK     = "ok"
	RunResultFailed = "failed"
)