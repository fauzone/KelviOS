// Package llmregistry provides a catalog of LLM models the core can call
// without knowing which model sits behind each entry. Secrets live in the
// vault under secret_name only — never in this catalog.
package llmregistry

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

const (
	// ActiveModelMetaKey is the meta table key storing the active model ID.
	ActiveModelMetaKey = "llm_active_model"
)

var idFallbackCounter uint64

// generateLLMModelID returns a collision-proof LLM model ID.
// Format: llm-<unix-nano>-<8 hex chars from crypto/rand>
// Uses only standard library (crypto/rand, encoding/hex).
func generateLLMModelID() string {
	now := time.Now().UnixNano()
	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		counter := atomic.AddUint64(&idFallbackCounter, 1)
		return fmt.Sprintf("llm-%d-%08x", now, counter)
	}
	return fmt.Sprintf("llm-%d-%s", now, hex.EncodeToString(randBytes[:]))
}

// Model represents an LLM model entry in the registry.
type Model struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Provider     string          `json:"provider"`
	EndpointEnv  string          `json:"endpoint_env"`
	ModelID      string          `json:"model_id"`
	Capabilities json.RawMessage `json:"capabilities,omitempty"` // JSON object
	SecretName   string          `json:"secret_name,omitempty"`  // vault reference only
	CreatedAt    time.Time       `json:"created_at"`
}

// CreateModelRequest is the request body for creating a model.
// Secret keys are rejected — secrets live in the vault under secret_name only.
type CreateModelRequest struct {
	Name         string          `json:"name"`
	Provider     string          `json:"provider"`
	EndpointEnv  string          `json:"endpoint_env"`
	ModelID      string          `json:"model_id"`
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	SecretName   string          `json:"secret_name,omitempty"`
}

// Sentinel errors for HTTP handlers to map to status codes.
var (
	ErrModelNameExists   = errors.New("model name already exists")
	ErrModelNotFound     = errors.New("model not found")
	ErrModelActive       = errors.New("model is active, cannot delete")
	ErrInvalidRequest    = errors.New("invalid request")
	ErrSecretKeyInBody   = errors.New("secret keys not allowed in request body")
)

// secretKeys is the set of lowercase keys that are rejected in request bodies
// (same set as WS43 backup registry).
var secretKeys = map[string]bool{
	"access_key":  true,
	"secret_key":  true,
	"app_key":     true,
	"password":    true,
	"token":       true,
	"account_id":  true,
	"account_key": true,
}

// ValidateCreateRequest validates a CreateModelRequest.
// Returns ErrInvalidRequest if required fields are missing,
// ErrSecretKeyInBody if any secret key is present.
func ValidateCreateRequest(req CreateModelRequest) error {
	if req.Name == "" || req.Provider == "" || req.ModelID == "" {
		return ErrInvalidRequest
	}
	// Check for secret keys in capabilities JSON (if present)
	if len(req.Capabilities) > 0 {
		var caps map[string]any
		if err := json.Unmarshal(req.Capabilities, &caps); err == nil {
			for k := range caps {
				if secretKeys[k] {
					return ErrSecretKeyInBody
				}
			}
		}
	}
	return nil
}

// Create inserts a new model into the registry.
// Returns ErrModelNameExists if a model with the same name already exists.
func Create(db *sql.DB, req CreateModelRequest) (*Model, error) {
	if err := ValidateCreateRequest(req); err != nil {
		return nil, err
	}

	// Empty capabilities must not insert NULL (column is NOT NULL).
	caps := req.Capabilities
	if len(caps) == 0 {
		caps = json.RawMessage("{}")
	}

	now := time.Now().Unix()
	id := generateLLMModelID()

	_, err := db.Exec(`
		INSERT INTO llm_models (id, name, provider, endpoint_env, model_id, capabilities, secret_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, id, req.Name, req.Provider, req.EndpointEnv, req.ModelID, caps, req.SecretName, now)
	if err != nil {
		if isUniqueConstraintError(err) {
			return nil, ErrModelNameExists
		}
		return nil, fmt.Errorf("insert model: %w", err)
	}

	return Get(db, id)
}

// List returns all models in the registry.
func List(db *sql.DB) ([]*Model, error) {
	rows, err := db.Query(`
		SELECT id, name, provider, endpoint_env, model_id, capabilities, secret_name, created_at
		FROM llm_models ORDER BY created_at
	`)
	if err != nil {
		return nil, fmt.Errorf("query models: %w", err)
	}
	defer rows.Close()

	var models []*Model
	for rows.Next() {
		var m Model
		var createdAt int64
		var caps, secret sql.NullString
		if err := rows.Scan(&m.ID, &m.Name, &m.Provider, &m.EndpointEnv, &m.ModelID, &caps, &secret, &createdAt); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		if caps.Valid {
			m.Capabilities = json.RawMessage(caps.String)
		}
		if secret.Valid {
			m.SecretName = secret.String
		}
		m.CreatedAt = time.Unix(createdAt, 0)
		models = append(models, &m)
	}
	if models == nil {
		models = []*Model{}
	}
	return models, nil
}

// Get retrieves a model by ID. Returns nil if not found.
func Get(db *sql.DB, id string) (*Model, error) {
	row := db.QueryRow(`
		SELECT id, name, provider, endpoint_env, model_id, capabilities, secret_name, created_at
		FROM llm_models WHERE id = ?
	`, id)
	var m Model
	var createdAt int64
	var caps, secret sql.NullString
	if err := row.Scan(&m.ID, &m.Name, &m.Provider, &m.EndpointEnv, &m.ModelID, &caps, &secret, &createdAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan model: %w", err)
	}
	if caps.Valid {
		m.Capabilities = json.RawMessage(caps.String)
	}
	if secret.Valid {
		m.SecretName = secret.String
	}
	m.CreatedAt = time.Unix(createdAt, 0)
	return &m, nil
}

// GetByName retrieves a model by name. Returns nil if not found.
func GetByName(db *sql.DB, name string) (*Model, error) {
	row := db.QueryRow(`
		SELECT id, name, provider, endpoint_env, model_id, capabilities, secret_name, created_at
		FROM llm_models WHERE name = ?
	`, name)
	var m Model
	var createdAt int64
	var caps, secret sql.NullString
	if err := row.Scan(&m.ID, &m.Name, &m.Provider, &m.EndpointEnv, &m.ModelID, &caps, &secret, &createdAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan model: %w", err)
	}
	if caps.Valid {
		m.Capabilities = json.RawMessage(caps.String)
	}
	if secret.Valid {
		m.SecretName = secret.String
	}
	m.CreatedAt = time.Unix(createdAt, 0)
	return &m, nil
}

// Delete removes a model by ID.
// Returns ErrModelNotFound if the model doesn't exist.
// Returns ErrModelActive if the model is the currently active one.
func Delete(db *sql.DB, id string) error {
	// Check if model exists
	model, err := Get(db, id)
	if err != nil {
		return err
	}
	if model == nil {
		return ErrModelNotFound
	}

	// Check if this is the active model
	activeID, err := GetActiveModelID(db)
	if err != nil {
		return fmt.Errorf("check active model: %w", err)
	}
	if activeID == id {
		return ErrModelActive
	}

	res, err := db.Exec(`DELETE FROM llm_models WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete model: %w", err)
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return ErrModelNotFound
	}
	return nil
}

// GetActiveModelID returns the currently active model ID from meta.
// Returns empty string if none is set.
func GetActiveModelID(db *sql.DB) (string, error) {
	var value sql.NullString
	err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, ActiveModelMetaKey).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("query active model: %w", err)
	}
	if value.Valid {
		return value.String, nil
	}
	return "", nil
}

// GetActiveModel returns the currently active model, or nil if none set.
func GetActiveModel(db *sql.DB) (*Model, error) {
	id, err := GetActiveModelID(db)
	if err != nil {
		return nil, err
	}
	if id == "" {
		return nil, nil
	}
	return Get(db, id)
}

// SetActiveModel sets the active model pointer in meta.
// Returns ErrModelNotFound if the model doesn't exist.
func SetActiveModel(db *sql.DB, id string) error {
	// Verify model exists
	model, err := Get(db, id)
	if err != nil {
		return err
	}
	if model == nil {
		return ErrModelNotFound
	}

	_, err = db.Exec(`
		INSERT OR REPLACE INTO meta (key, value)
		VALUES (?, ?)
	`, ActiveModelMetaKey, id)
	if err != nil {
		return fmt.Errorf("set active model: %w", err)
	}
	return nil
}

// isUniqueConstraintError checks if an error is a SQLite UNIQUE constraint violation.
func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return contains(s, "UNIQUE") || contains(s, "unique")
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// Registry wraps the database and provides LLM model registry operations.
// This is the main entry point for the HTTP handlers.
type Registry struct {
	db *sql.DB
}

// NewRegistry creates a new Registry bound to the given database.
func NewRegistry(db *sql.DB) *Registry {
	return &Registry{db: db}
}

// CreateModel creates a new model entry.
func (r *Registry) CreateModel(req CreateModelRequest) (*Model, error) {
	return Create(r.db, req)
}

// ListModels returns all models.
func (r *Registry) ListModels() ([]*Model, error) {
	return List(r.db)
}

// GetModel retrieves a model by ID.
func (r *Registry) GetModel(id string) (*Model, error) {
	return Get(r.db, id)
}

// DeleteModel removes a model by ID.
func (r *Registry) DeleteModel(id string) error {
	return Delete(r.db, id)
}

// GetActiveModel returns the currently active model.
func (r *Registry) GetActiveModel() (*Model, error) {
	return GetActiveModel(r.db)
}

// SetActiveModel sets the active model.
func (r *Registry) SetActiveModel(id string) error {
	return SetActiveModel(r.db, id)
}
