// Package llmregistry provides tests for the LLM model registry.
package llmregistry

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/fauzone/kelvios/internal/catalog"
)

// TestCreateListGetDeleteHappyPath tests the basic CRUD operations.
func TestCreateListGetDeleteHappyPath(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()

	// Create
	req := CreateModelRequest{
		Name:         "test-model",
		Provider:     "openai",
		EndpointEnv:  "OPENAI_API_BASE",
		ModelID:      "gpt-4",
		Capabilities: json.RawMessage(`{"max_tokens": 4096}`),
		SecretName:   "openai-secret",
	}
	model, err := Create(db, req)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if model == nil {
		t.Fatal("Create returned nil model")
	}
	if model.Name != "test-model" {
		t.Errorf("model name: got %q, want %q", model.Name, "test-model")
	}
	if model.Provider != "openai" {
		t.Errorf("model provider: got %q, want %q", model.Provider, "openai")
	}
	if model.ModelID != "gpt-4" {
		t.Errorf("model model_id: got %q, want %q", model.ModelID, "gpt-4")
	}
	if model.SecretName != "openai-secret" {
		t.Errorf("model secret_name: got %q, want %q", model.SecretName, "openai-secret")
	}
	if len(model.Capabilities) == 0 {
		t.Error("model capabilities should not be empty")
	}
	if model.ID == "" {
		t.Error("model ID should not be empty")
	}

	// List
	models, err := List(db)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(models) != 1 {
		t.Errorf("List count: got %d, want 1", len(models))
	}
	if models[0].ID != model.ID {
		t.Errorf("List model ID mismatch: got %q, want %q", models[0].ID, model.ID)
	}

	// Get
	got, err := Get(db, model.ID)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.ID != model.ID {
		t.Errorf("Get ID mismatch: got %q, want %q", got.ID, model.ID)
	}
	if got.Name != model.Name {
		t.Errorf("Get name mismatch: got %q, want %q", got.Name, model.Name)
	}

	// GetByName
	gotByName, err := GetByName(db, model.Name)
	if err != nil {
		t.Fatalf("GetByName failed: %v", err)
	}
	if gotByName == nil {
		t.Fatal("GetByName returned nil")
	}
	if gotByName.ID != model.ID {
		t.Errorf("GetByName ID mismatch: got %q, want %q", gotByName.ID, model.ID)
	}

	// Delete
	err = Delete(db, model.ID)
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify deleted
	models, err = List(db)
	if err != nil {
		t.Fatalf("List after delete failed: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("List after delete: got %d, want 0", len(models))
	}
	got, err = Get(db, model.ID)
	if err != nil {
		t.Fatalf("Get after delete failed: %v", err)
	}
	if got != nil {
		t.Errorf("Get after delete should return nil, got %v", got)
	}
}

// TestCreateDuplicateName tests that duplicate name returns ErrModelNameExists.
func TestCreateDuplicateName(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()

	req := CreateModelRequest{
		Name:     "duplicate",
		Provider: "openai",
		ModelID:  "gpt-4",
	}
	_, err = Create(db, req)
	if err != nil {
		t.Fatalf("first Create failed: %v", err)
	}

	_, err = Create(db, req)
	if err == nil {
		t.Fatal("second Create should have failed")
	}
	if !errors.Is(err, ErrModelNameExists) {
		t.Errorf("expected ErrModelNameExists, got: %v", err)
	}
}

// TestCreateSecretKeyInCapabilities tests that secret keys in capabilities returns ErrSecretKeyInBody.
func TestCreateSecretKeyInCapabilities(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()

	secretKeys := []string{"token", "password", "secret_key", "access_key", "app_key", "account_id", "account_key"}
	for _, key := range secretKeys {
		t.Run(key, func(t *testing.T) {
			caps := map[string]any{key: "secret-value"}
			capsJSON, _ := json.Marshal(caps)
			req := CreateModelRequest{
				Name:         "test-" + key,
				Provider:     "openai",
				ModelID:      "gpt-4",
				Capabilities: json.RawMessage(capsJSON),
			}
			_, err := Create(db, req)
			if err == nil {
				t.Fatalf("Create with %s should have failed", key)
			}
			if !errors.Is(err, ErrSecretKeyInBody) {
				t.Errorf("expected ErrSecretKeyInBody for %s, got: %v", key, err)
			}
		})
	}
}

// TestCreateMissingRequiredFields tests that missing name/provider/model_id returns ErrInvalidRequest.
func TestCreateMissingRequiredFields(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()

	testCases := []struct {
		name     string
		req      CreateModelRequest
		wantErr  error
	}{
		{"missing name", CreateModelRequest{Provider: "openai", ModelID: "gpt-4"}, ErrInvalidRequest},
		{"missing provider", CreateModelRequest{Name: "test", ModelID: "gpt-4"}, ErrInvalidRequest},
		{"missing model_id", CreateModelRequest{Name: "test", Provider: "openai"}, ErrInvalidRequest},
		{"all missing", CreateModelRequest{}, ErrInvalidRequest},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Create(db, tc.req)
			if err == nil {
				t.Fatal("Create should have failed")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("expected %v, got: %v", tc.wantErr, err)
			}
		})
	}
}

// TestCreateWithoutCapabilities tests that create without capabilities succeeds and reads back {}.
func TestCreateWithoutCapabilities(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()

	req := CreateModelRequest{
		Name:     "no-caps",
		Provider: "openai",
		ModelID:  "gpt-4",
	}
	model, err := Create(db, req)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if model == nil {
		t.Fatal("Create returned nil")
	}
	// Capabilities should be empty object, not null
	var caps map[string]any
	if err := json.Unmarshal(model.Capabilities, &caps); err != nil {
		t.Fatalf("unmarshal capabilities: %v", err)
	}
	if caps == nil {
		t.Error("capabilities should be {}, not null")
	}
	if len(caps) != 0 {
		t.Errorf("capabilities should be empty, got: %v", caps)
	}
}

// TestSetActiveModelUnknown tests that SetActiveModel on unknown id returns ErrModelNotFound.
func TestSetActiveModelUnknown(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()

	err = SetActiveModel(db, "llm-nonexistent")
	if err == nil {
		t.Fatal("SetActiveModel should have failed")
	}
	if !errors.Is(err, ErrModelNotFound) {
		t.Errorf("expected ErrModelNotFound, got: %v", err)
	}
}

// TestSetActiveModelRoundTrip tests SetActiveModel then GetActiveModel round-trips.
func TestSetActiveModelRoundTrip(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()

	// Create a model
	req := CreateModelRequest{
		Name:     "active-test",
		Provider: "openai",
		ModelID:  "gpt-4",
	}
	model, err := Create(db, req)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Set as active
	err = SetActiveModel(db, model.ID)
	if err != nil {
		t.Fatalf("SetActiveModel failed: %v", err)
	}

	// Get active model
	active, err := GetActiveModel(db)
	if err != nil {
		t.Fatalf("GetActiveModel failed: %v", err)
	}
	if active == nil {
		t.Fatal("GetActiveModel returned nil")
	}
	if active.ID != model.ID {
		t.Errorf("active model ID: got %q, want %q", active.ID, model.ID)
	}
	if active.Name != model.Name {
		t.Errorf("active model name: got %q, want %q", active.Name, model.Name)
	}

	// GetActiveModelID should also return the same ID
	activeID, err := GetActiveModelID(db)
	if err != nil {
		t.Fatalf("GetActiveModelID failed: %v", err)
	}
	if activeID != model.ID {
		t.Errorf("GetActiveModelID: got %q, want %q", activeID, model.ID)
	}
}

// TestDeleteActiveModel tests that delete of the ACTIVE model returns ErrModelActive.
func TestDeleteActiveModel(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()

	// Create a model
	req := CreateModelRequest{
		Name:     "to-delete",
		Provider: "openai",
		ModelID:  "gpt-4",
	}
	model, err := Create(db, req)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Set as active
	err = SetActiveModel(db, model.ID)
	if err != nil {
		t.Fatalf("SetActiveModel failed: %v", err)
	}

	// Try to delete active model
	err = Delete(db, model.ID)
	if err == nil {
		t.Fatal("Delete of active model should have failed")
	}
	if !errors.Is(err, ErrModelActive) {
		t.Errorf("expected ErrModelActive, got: %v", err)
	}

	// Verify model still exists
	got, err := Get(db, model.ID)
	if err != nil {
		t.Fatalf("Get after failed delete: %v", err)
	}
	if got == nil {
		t.Error("model should still exist after failed delete")
	}
}

// TestDeleteNonActiveModel tests that delete of non-active model succeeds.
func TestDeleteNonActiveModel(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()

	// Create two models
	req1 := CreateModelRequest{Name: "model1", Provider: "openai", ModelID: "gpt-4"}
	req2 := CreateModelRequest{Name: "model2", Provider: "anthropic", ModelID: "claude-3"}
	model1, err := Create(db, req1)
	if err != nil {
		t.Fatalf("Create model1 failed: %v", err)
	}
	model2, err := Create(db, req2)
	if err != nil {
		t.Fatalf("Create model2 failed: %v", err)
	}

	// Set model1 as active
	err = SetActiveModel(db, model1.ID)
	if err != nil {
		t.Fatalf("SetActiveModel failed: %v", err)
	}

	// Delete model2 (non-active) should succeed
	err = Delete(db, model2.ID)
	if err != nil {
		t.Fatalf("Delete non-active model failed: %v", err)
	}

	// Verify model2 is gone
	got, err := Get(db, model2.ID)
	if err != nil {
		t.Fatalf("Get model2 after delete: %v", err)
	}
	if got != nil {
		t.Error("model2 should be deleted")
	}

	// Verify model1 still exists
	got, err = Get(db, model1.ID)
	if err != nil {
		t.Fatalf("Get model1 after delete model2: %v", err)
	}
	if got == nil {
		t.Error("model1 should still exist")
	}
}

// TestDeleteUnknownModel tests that delete of unknown model returns ErrModelNotFound.
func TestDeleteUnknownModel(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()

	err = Delete(db, "llm-unknown")
	if err == nil {
		t.Fatal("Delete unknown should have failed")
	}
	if !errors.Is(err, ErrModelNotFound) {
		t.Errorf("expected ErrModelNotFound, got: %v", err)
	}
}

// TestValidateCreateRequest tests the validation function directly.
func TestValidateCreateRequest(t *testing.T) {
	testCases := []struct {
		name    string
		req     CreateModelRequest
		wantErr error
	}{
		{"valid", CreateModelRequest{Name: "test", Provider: "openai", ModelID: "gpt-4"}, nil},
		{"missing name", CreateModelRequest{Provider: "openai", ModelID: "gpt-4"}, ErrInvalidRequest},
		{"missing provider", CreateModelRequest{Name: "test", ModelID: "gpt-4"}, ErrInvalidRequest},
		{"missing model_id", CreateModelRequest{Name: "test", Provider: "openai"}, ErrInvalidRequest},
		{"secret token in caps", CreateModelRequest{Name: "test", Provider: "openai", ModelID: "gpt-4", Capabilities: json.RawMessage(`{"token": "secret"}`)}, ErrSecretKeyInBody},
		{"secret password in caps", CreateModelRequest{Name: "test", Provider: "openai", ModelID: "gpt-4", Capabilities: json.RawMessage(`{"password": "secret"}`)}, ErrSecretKeyInBody},
		{"secret secret_key in caps", CreateModelRequest{Name: "test", Provider: "openai", ModelID: "gpt-4", Capabilities: json.RawMessage(`{"secret_key": "secret"}`)}, ErrSecretKeyInBody},
		{"valid caps no secrets", CreateModelRequest{Name: "test", Provider: "openai", ModelID: "gpt-4", Capabilities: json.RawMessage(`{"max_tokens": 4096}`)}, nil},
		{"empty caps", CreateModelRequest{Name: "test", Provider: "openai", ModelID: "gpt-4", Capabilities: json.RawMessage(`{}`)}, nil},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCreateRequest(tc.req)
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("expected %v, got: %v", tc.wantErr, err)
				}
			}
		})
	}
}

// TestRegistryWrapper tests the Registry struct wrapper methods.
func TestRegistryWrapper(t *testing.T) {
	db, err := catalog.Open(":memory:")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	if err := catalog.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer db.Close()

	r := NewRegistry(db)

	// CreateModel
	req := CreateModelRequest{Name: "wrapper-test", Provider: "openai", ModelID: "gpt-4"}
	model, err := r.CreateModel(req)
	if err != nil {
		t.Fatalf("CreateModel failed: %v", err)
	}

	// ListModels
	models, err := r.ListModels()
	if err != nil {
		t.Fatalf("ListModels failed: %v", err)
	}
	if len(models) != 1 {
		t.Errorf("ListModels count: got %d, want 1", len(models))
	}

	// GetModel
	got, err := r.GetModel(model.ID)
	if err != nil {
		t.Fatalf("GetModel failed: %v", err)
	}
	if got.ID != model.ID {
		t.Errorf("GetModel ID mismatch")
	}

	// GetActiveModel (none set)
	active, err := r.GetActiveModel()
	if err != nil {
		t.Fatalf("GetActiveModel failed: %v", err)
	}
	if active != nil {
		t.Error("GetActiveModel should return nil when none set")
	}

	// SetActiveModel
	err = r.SetActiveModel(model.ID)
	if err != nil {
		t.Fatalf("SetActiveModel failed: %v", err)
	}

	// GetActiveModel (now set)
	active, err = r.GetActiveModel()
	if err != nil {
		t.Fatalf("GetActiveModel after set failed: %v", err)
	}
	if active == nil || active.ID != model.ID {
		t.Error("GetActiveModel should return the set model")
	}

	// DeleteModel should fail for active model
	err = r.DeleteModel(model.ID)
	if err == nil {
		t.Fatal("DeleteModel of active model should have failed")
	}
	if !errors.Is(err, ErrModelActive) {
		t.Errorf("expected ErrModelActive, got: %v", err)
	}
}