package storage

import (
	"context"
	"os/exec"
	"sync"
)

// CommandRunner is the abstraction for running external commands.
// All external commands (mergerfs, snapraid, fusermount/umount) MUST go through this.
type CommandRunner interface {
	// Run executes a command with the given context and arguments.
	// Returns stdout+stderr combined output and error.
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// LookPath searches for an executable binary in PATH.
	// Returns the absolute path if found, empty string if not found.
	LookPath(name string) (string, error)
}

// ToolProbe is the abstraction for probing tool availability.
// Used by the registry to determine driver availability without hardcoding PATH checks.
type ToolProbe interface {
	// LookPath searches for an executable binary in PATH.
	// Returns the absolute path if found, empty string if not found.
	LookPath(name string) (string, error)
}

// OSCommandRunner is the production implementation using os/exec.
type OSCommandRunner struct{}

// Run executes a command using os/exec.
func (r *OSCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

// LookPath uses exec.LookPath to find a binary in PATH.
func (r *OSCommandRunner) LookPath(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", nil // not found, return empty string (not error) for probe semantics
	}
	return path, nil
}

// OSToolProbe is the production implementation of ToolProbe using exec.LookPath.
type OSToolProbe struct{}

// LookPath uses exec.LookPath to find a binary in PATH.
func (p *OSToolProbe) LookPath(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", nil // not found, return empty string (not error) for probe semantics
	}
	return path, nil
}

// FakeRunner is a test implementation of CommandRunner that records calls
// and returns configurable responses.
type FakeRunner struct {
	mu       sync.Mutex
	Calls    []FakeCall
	Responses map[string]FakeResponse // key: "name|arg1|arg2|..."
	DefaultResponse FakeResponse
}

// FakeCall records a single command invocation.
type FakeCall struct {
	Name string
	Args []string
	Ctx  context.Context
}

// FakeResponse defines what a fake command should return.
type FakeResponse struct {
	Output []byte
	Err    error
}

// NewFakeRunner creates a new fake runner for testing.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{
		Calls:    []FakeCall{},
		Responses: make(map[string]FakeResponse),
	}
}

// SetResponse sets a specific response for a command pattern.
// Pattern is "name|arg1|arg2|..." (use "|" as separator).
func (f *FakeRunner) SetResponse(pattern string, resp FakeResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Responses[pattern] = resp
}

// SetDefaultResponse sets the default response for unmatched commands.
func (f *FakeRunner) SetDefaultResponse(resp FakeResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.DefaultResponse = resp
}

// Run implements CommandRunner for testing.
func (f *FakeRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, FakeCall{Name: name, Args: args, Ctx: ctx})
	
	// Build pattern key
	key := name
	for _, a := range args {
		key += "|" + a
	}
	
	resp, ok := f.Responses[key]
	if !ok {
		resp = f.DefaultResponse
	}
	f.mu.Unlock()
	
	return resp.Output, resp.Err
}

// LookPath implements CommandRunner for testing.
func (f *FakeRunner) LookPath(name string) (string, error) {
	// For fake, we can set a specific response for LookPath
	key := "LookPath|" + name
	f.mu.Lock()
	resp, ok := f.Responses[key]
	f.mu.Unlock()
	
	if ok {
		if resp.Err != nil {
			return "", resp.Err
		}
		return string(resp.Output), nil
	}
	return "", nil // not found by default
}

// FakeProbe is a test implementation of ToolProbe.
type FakeProbe struct {
	mu       sync.Mutex
	Paths    map[string]string // name -> path (empty = not found)
}

// NewFakeProbe creates a new fake probe for testing.
func NewFakeProbe() *FakeProbe {
	return &FakeProbe{
		Paths: make(map[string]string),
	}
}

// SetPath sets the path for a binary (empty = not found).
func (f *FakeProbe) SetPath(name, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Paths[name] = path
}

// LookPath implements ToolProbe for testing.
func (f *FakeProbe) LookPath(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Paths[name], nil
}
