// Package harness provides tests for the Goose adapter.
package harness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// TestGooseRunSuccess tests that Run returns stdout on success.
func TestGooseRunSuccess(t *testing.T) {
	stubPath := createEchoStub(t)
	defer os.Remove(stubPath)

	cfg := Config{Binary: stubPath, Timeout: 5 * time.Second}
	h := NewGoose(cfg)

	ctx := context.Background()
	out, err := h.Run(ctx, "hello world", nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if out != "hello world" {
		t.Errorf("expected stdout %q, got %q", "hello world", out)
	}
}

// TestGooseRunNonZeroExit tests that non-zero exit surfaces RunError with stderr.
func TestGooseRunNonZeroExit(t *testing.T) {
	stubPath := createFailStub(t)
	defer os.Remove(stubPath)

	cfg := Config{Binary: stubPath, Timeout: 5 * time.Second}
	h := NewGoose(cfg)

	ctx := context.Background()
	_, err := h.Run(ctx, "test prompt", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var runErr *RunError
	if !errors.As(err, &runErr) {
		t.Fatalf("expected *RunError, got %T: %v", err, err)
	}
	if runErr.Stderr == "" {
		t.Error("RunError.Stderr should contain stderr output")
	}
	if runErr.Err == nil {
		t.Error("RunError.Err should wrap the underlying error")
	}
}

// TestGooseRunTimeout tests that timeout cancels the command.
func TestGooseRunTimeout(t *testing.T) {
	stubPath := createSleepStub(t, 2*time.Second)
	defer os.Remove(stubPath)

	cfg := Config{Binary: stubPath, Timeout: 1 * time.Second}
	h := NewGoose(cfg)

	ctx := context.Background()
	_, err := h.Run(ctx, "test prompt", nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	// On Windows, context cancellation may surface as a different error type.
	// Accept either context deadline exceeded or a process kill error.
	if !isContextDeadlineExceeded(err) && !isProcessKilled(err) {
		t.Errorf("expected timeout-related error, got: %v", err)
	}
}

// TestGooseRunWithTools tests that tools are passed as --tool flags.
func TestGooseRunWithTools(t *testing.T) {
	stubPath := createArgsEchoStub(t)
	defer os.Remove(stubPath)

	cfg := Config{Binary: stubPath, Timeout: 5 * time.Second}
	h := NewGoose(cfg)

	ctx := context.Background()
	out, err := h.Run(ctx, "prompt", []string{"tool1", "tool2"})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if !contains(out, "--tool") || !contains(out, "tool1") || !contains(out, "tool2") {
		t.Errorf("expected tools in output, got: %s", out)
	}
}

// TestGooseDefaultConfig tests default binary and timeout.
func TestGooseDefaultConfig(t *testing.T) {
	old := os.Getenv("KELVIOS_GOOSE_BIN")
	os.Unsetenv("KELVIOS_GOOSE_BIN")
	defer func() {
		if old != "" {
			os.Setenv("KELVIOS_GOOSE_BIN", old)
		}
	}()

	cfg := DefaultConfig()
	if cfg.Binary != "goose" {
		t.Errorf("default binary: got %q, want %q", cfg.Binary, "goose")
	}
	if cfg.Timeout != 120*time.Second {
		t.Errorf("default timeout: got %v, want 120s", cfg.Timeout)
	}
}

// TestGooseEnvConfig tests KELVIOS_GOOSE_BIN environment variable.
func TestGooseEnvConfig(t *testing.T) {
	stubPath := createEchoStub(t)
	defer os.Remove(stubPath)

	old := os.Getenv("KELVIOS_GOOSE_BIN")
	os.Setenv("KELVIOS_GOOSE_BIN", stubPath)
	defer func() {
		if old != "" {
			os.Setenv("KELVIOS_GOOSE_BIN", old)
		} else {
			os.Unsetenv("KELVIOS_GOOSE_BIN")
		}
	}()

	cfg := Config{}
	h := NewGoose(cfg)

	ctx := context.Background()
	out, err := h.Run(ctx, "env test", nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if out != "env test" {
		t.Errorf("expected stdout %q, got %q", "env test", out)
	}
}

// TestGooseExplicitConfigOverridesEnv tests that explicit config overrides env.
func TestGooseExplicitConfigOverridesEnv(t *testing.T) {
	echoStub := createEchoStub(t)
	failStub := createFailStub(t)
	defer os.Remove(echoStub)
	defer os.Remove(failStub)

	old := os.Getenv("KELVIOS_GOOSE_BIN")
	os.Setenv("KELVIOS_GOOSE_BIN", failStub)
	defer func() {
		if old != "" {
			os.Setenv("KELVIOS_GOOSE_BIN", old)
		} else {
			os.Unsetenv("KELVIOS_GOOSE_BIN")
		}
	}()

	cfg := Config{Binary: echoStub, Timeout: 5 * time.Second}
	h := NewGoose(cfg)

	ctx := context.Background()
	out, err := h.Run(ctx, "explicit", nil)
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if out != "explicit" {
		t.Errorf("expected stdout %q, got %q", "explicit", out)
	}
}

// TestRunErrorUnwrap tests that RunError unwraps correctly.
func TestRunErrorUnwrap(t *testing.T) {
	baseErr := &testError{msg: "base error"}
	runErr := &RunError{Err: baseErr, Stderr: "stderr output"}

	if runErr.Unwrap() != baseErr {
		t.Error("Unwrap should return the wrapped error")
	}
	if runErr.Error() != "base error: stderr output" {
		t.Errorf("Error() format: got %q", runErr.Error())
	}
}

// TestRunErrorNoStderr tests RunError without stderr.
func TestRunErrorNoStderr(t *testing.T) {
	baseErr := &testError{msg: "base error"}
	runErr := &RunError{Err: baseErr, Stderr: ""}

	if runErr.Error() != "base error" {
		t.Errorf("Error() without stderr: got %q", runErr.Error())
	}
}

// Helpers - create Go stub binaries that work cross-platform

func createEchoStub(t *testing.T) string {
	t.Helper()
	return createGoStub(t, "echo", `
package main
import (
	"io"
	"os"
)
func main() {
	io.Copy(os.Stdout, os.Stdin)
}
`)
}

func createFailStub(t *testing.T) string {
	t.Helper()
	return createGoStub(t, "fail", `
package main
import (
	"os"
)
func main() {
	os.Stderr.WriteString("something went wrong\n")
	os.Exit(1)
}
`)
}

func createSleepStub(t *testing.T, duration time.Duration) string {
	t.Helper()
	ms := int(duration.Milliseconds())
	return createGoStub(t, "sleep", `
package main
import (
	"time"
	"os"
)
func main() {
	time.Sleep(time.Duration(`+strconv.Itoa(ms)+` * int(time.Millisecond)))
	os.Exit(0)
}
`)
}

func createArgsEchoStub(t *testing.T) string {
	t.Helper()
	return createGoStub(t, "argsecho", `
package main
import (
	"fmt"
	"os"
)
func main() {
	for i, a := range os.Args {
		if i > 0 {
			fmt.Print(" ")
		}
		fmt.Print(a)
	}
	fmt.Println()
}
`)
}

func createGoStub(t *testing.T, name, source string) string {
	t.Helper()
	dir := t.TempDir()
	srcPath := filepath.Join(dir, name+".go")
	if err := os.WriteFile(srcPath, []byte(source), 0o644); err != nil {
		t.Fatalf("write stub source: %v", err)
	}
	binPath := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	// Build the stub
	cmd := exec.Command("go", "build", "-o", binPath, srcPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stub %s: %v\n%s", name, err, out)
	}
	return binPath
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func isContextDeadlineExceeded(err error) bool {
	for err != nil {
		if err.Error() == "context deadline exceeded" {
			return true
		}
		if unwrapper, ok := err.(interface{ Unwrap() error }); ok {
			err = unwrapper.Unwrap()
			continue
		}
		break
	}
	return false
}

func isProcessKilled(err error) bool {
	for err != nil {
		s := err.Error()
		// Windows: "exit status 1" when process is killed by context timeout
		// Unix: "signal: killed" or similar
		if contains(s, "killed") || contains(s, "terminated") || contains(s, "signal") || contains(s, "exit status") {
			return true
		}
		if unwrapper, ok := err.(interface{ Unwrap() error }); ok {
			err = unwrapper.Unwrap()
			continue
		}
		break
	}
	return false
}

type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}