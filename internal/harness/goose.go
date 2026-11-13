// Package harness provides a replaceable interface for running an external
// agent harness (Goose headless) behind a stable contract.
package harness

import (
	"context"
	"os"
	"os/exec"
	"time"
	"io"
)

// gooseAdapter implements Harness using the Goose headless binary.
type gooseAdapter struct {
	cfg Config
}

// NewGoose creates a new Goose harness adapter with the given config.
// If cfg.Binary is empty, it defaults to the KELVIOS_GOOSE_BIN environment
// variable or "goose". If cfg.Timeout is 0, it defaults to 120 seconds.
func NewGoose(cfg Config) Harness {
	if cfg.Binary == "" {
		cfg.Binary = os.Getenv("KELVIOS_GOOSE_BIN")
		if cfg.Binary == "" {
			cfg.Binary = "goose"
		}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 120 * time.Second
	}
	return &gooseAdapter{cfg: cfg}
}

// Run executes the Goose binary with the given prompt via stdin.
// The tools slice is passed as --tool flags (if non-empty).
// Stdout is captured and returned. Stderr is included in the error only
// (never logged verbatim, as it may carry secrets).
func (a *gooseAdapter) Run(ctx context.Context, prompt string, tools []string) (string, error) {
	// Build command: goose run --stdin [--tool tool1 --tool tool2 ...]
	args := []string{"run", "--stdin"}
	for _, t := range tools {
		args = append(args, "--tool", t)
	}

	// Create a context with timeout
	ctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, a.cfg.Binary, args...)
	cmd.Stdin = nil // We'll write via stdin pipe

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}

	if err := cmd.Start(); err != nil {
		return "", err
	}

	// Write prompt to stdin in a goroutine
	go func() {
		defer stdin.Close()
		_, _ = stdin.Write([]byte(prompt))
	}()

	// Read stdout and stderr
	stdoutBytes, err := readAll(stdout)
	if err != nil {
		_ = cmd.Wait()
		return "", err
	}

	stderrBytes, err := readAll(stderr)
	if err != nil {
		_ = cmd.Wait()
		return "", err
	}

	if err := cmd.Wait(); err != nil {
		// Include stderr in the error message (caller decides whether to log)
		if len(stderrBytes) > 0 {
			return "", &RunError{Err: err, Stderr: string(stderrBytes)}
		}
		return "", err
	}

	return string(stdoutBytes), nil
}

// RunError wraps an execution error with captured stderr.
// Stderr is surfaced only through this error — never logged verbatim.
type RunError struct {
	Err    error
	Stderr string
}

func (e *RunError) Error() string {
	if e.Stderr != "" {
		return e.Err.Error() + ": " + e.Stderr
	}
	return e.Err.Error()
}

func (e *RunError) Unwrap() error {
	return e.Err
}

// readAll reads all data from a pipe until EOF.
func readAll(r io.Reader) ([]byte, error) {
	// Use a buffer to read in chunks
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}