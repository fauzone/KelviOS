// Package harness provides a replaceable interface for running an external
// agent harness (Goose headless) behind a stable contract. The core calls
// this interface without knowing which harness binary sits behind it.
package harness

import (
	"context"
	"time"
)

// Harness is the interface for executing a prompt via an external harness.
type Harness interface {
	// Run executes the harness with the given prompt and optional tool allowlist.
	// Returns the captured stdout on success, or an error containing stderr.
	Run(ctx context.Context, prompt string, tools []string) (string, error)
}

// Config holds configuration for the Goose harness adapter.
type Config struct {
	// Binary is the path or name of the Goose binary.
	// Defaults to "goose" or the KELVIOS_GOOSE_BIN environment variable.
	Binary string
	// Timeout is the maximum execution time for a single Run call.
	// Defaults to 120 seconds.
	Timeout time.Duration
}

// DefaultConfig returns a Config with default values.
func DefaultConfig() Config {
	return Config{
		Binary:  "goose",
		Timeout: 120 * time.Second,
	}
}