package agents

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/fauzone/kelvios/internal/maintenance"
	"github.com/fauzone/kelvios/internal/storage"
)

// Agent is the interface every agent must implement.
type Agent interface {
	Name() string
	Describe() string
	Run(ctx context.Context, deps *Deps) (Report, error)
}

// Report is the result of an agent run.
type Report struct {
	Agent       string    `json:"agent"`
	GeneratedAt time.Time `json:"generated_at"`
	Summary     string    `json:"summary"`
	Findings    []string  `json:"findings"`
	Actions     []string  `json:"actions"`
}

// Deps holds the dependencies injected into agents.
// These shapes match what existing engines already use.
type Deps struct {
	DB        *sql.DB
	DisksFunc func() ([]storage.Disk, error)
	Now       func() time.Time
}

// registry holds all registered agents.
var registry = make(map[string]Agent)

// Register adds an agent to the registry. Case-insensitive key.
func Register(a Agent) {
	key := strings.ToLower(a.Name())
	registry[key] = a
}

// All returns all registered agents.
func All() []Agent {
	out := make([]Agent, 0, len(registry))
	for _, a := range registry {
		out = append(out, a)
	}
	return out
}

// Get retrieves an agent by name (case-insensitive).
func Get(name string) (Agent, bool) {
	a, ok := registry[strings.ToLower(name)]
	return a, ok
}

// NewDeps creates a Deps struct with the standard dependencies.
func NewDeps(db *sql.DB, disksFunc func() ([]storage.Disk, error), now func() time.Time) *Deps {
	if now == nil {
		now = time.Now
	}
	return &Deps{
		DB:        db,
		DisksFunc: disksFunc,
		Now:       now,
	}
}

// MaintenanceDeps returns maintenance.Deps from agent Deps.
func (d *Deps) MaintenanceDeps() maintenance.Deps {
	return maintenance.Deps{
		DB:        d.DB,
		DisksFunc: d.DisksFunc,
		Now:       d.Now,
	}
}

// Catalog returns the catalog package for direct queries.
func (d *Deps) Catalog() *sql.DB {
	return d.DB
}
