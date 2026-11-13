package ups

import (
	"context"
	"fmt"
	"os/exec"
)

// Shutdowner is the interface for executing system shutdown.
type Shutdowner interface {
	Shutdown(ctx context.Context, reason string) error
}

// SystemShutdowner executes a configured shutdown command.
type SystemShutdowner struct {
	// Command is the shutdown command to execute (e.g., ["shutdown", "/s", "/t", "0"] on Windows,
	// or ["shutdown", "-h", "now"] on Linux). If empty, Shutdown returns an error
	// and does NOT execute anything (safe default).
	Command []string
}

// Shutdown executes the configured command with the given context.
// Returns error if Command is empty or execution fails.
func (s *SystemShutdowner) Shutdown(ctx context.Context, reason string) error {
	if len(s.Command) == 0 {
		return fmt.Errorf("shutdown command not configured")
	}
	cmd := exec.CommandContext(ctx, s.Command[0], s.Command[1:]...)
	return cmd.Run()
}
