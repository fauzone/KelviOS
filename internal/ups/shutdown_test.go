package ups

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeShutdowner is a test-only Shutdowner that records calls.
type fakeShutdowner struct {
	mu    sync.Mutex
	calls []fakeShutdownCall
	err   error
	delay time.Duration
}

type fakeShutdownCall struct {
	reason string
	ctx    context.Context
}

func (f *fakeShutdowner) Shutdown(ctx context.Context, reason string) error {
	f.mu.Lock()
	f.calls = append(f.calls, fakeShutdownCall{reason: reason, ctx: ctx})
	err := f.err
	delay := f.delay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return err
}

func (f *fakeShutdowner) getCalls() []fakeShutdownCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := make([]fakeShutdownCall, len(f.calls))
	copy(calls, f.calls)
	return calls
}

func TestSystemShutdowner_EmptyCommand(t *testing.T) {
	s := &SystemShutdowner{Command: []string{}}
	err := s.Shutdown(context.Background(), "test reason")
	if err == nil {
		t.Fatal("expected error for empty command")
	}
	if !contains(err.Error(), "shutdown command not configured") {
		t.Errorf("error = %v, want contains 'shutdown command not configured'", err)
	}
}

func TestSystemShutdowner_NilCommand(t *testing.T) {
	s := &SystemShutdowner{Command: nil}
	err := s.Shutdown(context.Background(), "test reason")
	if err == nil {
		t.Fatal("expected error for nil command")
	}
	if !contains(err.Error(), "shutdown command not configured") {
		t.Errorf("error = %v, want contains 'shutdown command not configured'", err)
	}
}

func TestFakeShutdowner_RecordsCall(t *testing.T) {
	f := &fakeShutdowner{}
	ctx := context.Background()
	err := f.Shutdown(ctx, "battery critical")
	if err != nil {
		t.Fatalf("Shutdown error: %v", err)
	}
	calls := f.getCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].reason != "battery critical" {
		t.Errorf("reason = %q, want \"battery critical\"", calls[0].reason)
	}
	if calls[0].ctx != ctx {
		t.Errorf("ctx not passed through")
	}
}

func TestFakeShutdowner_ReturnsError(t *testing.T) {
	f := &fakeShutdowner{err: errors.New("simulated failure")}
	err := f.Shutdown(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error")
	}
	if !contains(err.Error(), "simulated failure") {
		t.Errorf("error = %v, want simulated failure", err)
	}
}

func TestFakeShutdowner_ContextCancellation(t *testing.T) {
	f := &fakeShutdowner{delay: 100 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	err := f.Shutdown(ctx, "test")
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if !contains(err.Error(), "context canceled") && !contains(err.Error(), "context cancelled") {
		t.Logf("context error (acceptable): %v", err)
	}
}

func TestFakeShutdowner_MultipleCalls(t *testing.T) {
	f := &fakeShutdowner{}
	ctx := context.Background()
	f.Shutdown(ctx, "first")
	f.Shutdown(ctx, "second")
	f.Shutdown(ctx, "third")

	calls := f.getCalls()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(calls))
	}
	if calls[0].reason != "first" || calls[1].reason != "second" || calls[2].reason != "third" {
		t.Errorf("reasons = %v, want [first second third]", calls)
	}
}

// contains is a simple substring check (stdlib only).
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && findSubstring(s, substr))
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
