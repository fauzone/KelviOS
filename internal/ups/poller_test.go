package ups

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVarSource is a test implementation of VarSource.
type fakeVarSource struct {
	mu      sync.Mutex
	varsSeq []map[string]string
	errSeq  []error
	callIdx int
}

func (f *fakeVarSource) GetVars() (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.callIdx >= len(f.varsSeq) && f.callIdx >= len(f.errSeq) {
		// Return a default OL reading instead of erroring, so poller can continue
		return map[string]string{"ups.status": "OL", "battery.charge": "100"}, nil
	}
	var vars map[string]string
	var err error
	if f.callIdx < len(f.varsSeq) {
		vars = f.varsSeq[f.callIdx]
	}
	if f.callIdx < len(f.errSeq) {
		err = f.errSeq[f.callIdx]
	}
	f.callIdx++
	return vars, err
}

func TestPoller_PollOnce(t *testing.T) {
	src := &fakeVarSource{
		varsSeq: []map[string]string{
			{"ups.status": "OL", "battery.charge": "100"},
		},
	}
	p := &Poller{Source: src, CriticalPercent: 20}

	prev := Reading{}
	cur, decision, err := p.PollOnce(prev)
	if err != nil {
		t.Fatalf("PollOnce error: %v", err)
	}
	if !cur.Present || cur.State != StateOnline || cur.ChargePercent != 100 {
		t.Errorf("cur = %+v, want Present=true State=online Charge=100", cur)
	}
	if decision.Event != "" {
		t.Errorf("decision.Event = %q, want empty", decision.Event)
	}
}

func TestPoller_PollOnce_Error(t *testing.T) {
	src := &fakeVarSource{
		errSeq: []error{errors.New("connection refused")},
	}
	p := &Poller{Source: src, CriticalPercent: 20}

	_, _, err := p.PollOnce(Reading{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error = %v, want connection refused", err)
	}
}

func TestPoller_Run_Sequence(t *testing.T) {
	// Sequence: OL -> OB (on_battery) -> OB+LB (critical_shutdown) -> OL (online_restored)
	src := &fakeVarSource{
		varsSeq: []map[string]string{
			{"ups.status": "OL", "battery.charge": "100"},   // tick 1: OL
			{"ups.status": "OB", "battery.charge": "90"},    // tick 2: OB 90%
			{"ups.status": "OB LB", "battery.charge": "15"}, // tick 3: OB LB 15% (critical)
			{"ups.status": "OL", "battery.charge": "95"},    // tick 4: OL restored
		},
	}
	p := &Poller{Source: src, CriticalPercent: 20}

	var events []Decision
	var readings []Reading
	var mu sync.Mutex
	emit := func(r Reading, d Decision) {
		mu.Lock()
		readings = append(readings, r)
		events = append(events, d)
		mu.Unlock()
	}
	onError := func(err error) {
		t.Errorf("unexpected error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Run for a short time, then cancel
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	p.Run(ctx, 50*time.Millisecond, emit, onError)

	mu.Lock()
	defer mu.Unlock()

	// Should have 3 events: on_battery, critical_shutdown, online_restored
	if len(events) != 3 {
		t.Errorf("got %d events, want 3: %+v", len(events), events)
	}
	if len(events) >= 1 && events[0].Event != EventOnBattery {
		t.Errorf("event 0 = %v, want on_battery", events[0].Event)
	}
	if len(events) >= 2 && events[1].Event != EventCriticalShutdown {
		t.Errorf("event 1 = %v, want critical_shutdown", events[1].Event)
	}
	if len(events) >= 2 && !events[1].BeginShutdown {
		t.Errorf("event 1 BeginShutdown = false, want true")
	}
	if len(events) >= 3 && events[2].Event != EventOnlineRestored {
		t.Errorf("event 2 = %v, want online_restored", events[2].Event)
	}
}

func TestPoller_Run_ErrorContinues(t *testing.T) {
	// First call errors, second succeeds
	src := &fakeVarSource{
		errSeq:  []error{errors.New("transient error")},
		varsSeq: []map[string]string{{"ups.status": "OL", "battery.charge": "100"}},
	}
	p := &Poller{Source: src, CriticalPercent: 20}

	var errorCount int
	var mu sync.Mutex
	onError := func(err error) {
		mu.Lock()
		errorCount++
		mu.Unlock()
	}
	emit := func(r Reading, d Decision) {}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	p.Run(ctx, 50*time.Millisecond, emit, onError)

	mu.Lock()
	if errorCount != 1 {
		t.Errorf("errorCount = %d, want 1", errorCount)
	}
	mu.Unlock()
}

func TestPoller_Run_CancelReturns(t *testing.T) {
	src := &fakeVarSource{
		varsSeq: []map[string]string{
			{"ups.status": "OL", "battery.charge": "100"},
		},
	}
	p := &Poller{Source: src, CriticalPercent: 20}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	done := make(chan struct{})
	go func() {
		p.Run(ctx, 10*time.Millisecond, func(Reading, Decision) {}, func(error) {})
		close(done)
	}()

	select {
	case <-done:
		// Good, returned promptly
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestPoller_CriticalPercentDefault(t *testing.T) {
	src := &fakeVarSource{
		varsSeq: []map[string]string{
			{"ups.status": "OB", "battery.charge": "20"}, // exactly at default 20%
		},
	}
	p := &Poller{Source: src, CriticalPercent: 0} // should use default 20

	_, decision, err := p.PollOnce(Reading{Present: true, State: StateOnline, ChargePercent: 90})
	if err != nil {
		t.Fatalf("PollOnce error: %v", err)
	}
	if decision.Event != EventCriticalShutdown {
		t.Errorf("decision.Event = %v, want critical_shutdown (charge 20 <= default 20)", decision.Event)
	}
	if !decision.BeginShutdown {
		t.Errorf("BeginShutdown = false, want true")
	}
}

func TestPoller_CriticalPercentNegative(t *testing.T) {
	src := &fakeVarSource{
		varsSeq: []map[string]string{
			{"ups.status": "OB", "battery.charge": "20"},
		},
	}
	p := &Poller{Source: src, CriticalPercent: -5} // should use default 20

	_, decision, err := p.PollOnce(Reading{Present: true, State: StateOnline, ChargePercent: 90})
	if err != nil {
		t.Fatalf("PollOnce error: %v", err)
	}
	if decision.Event != EventCriticalShutdown {
		t.Errorf("decision.Event = %v, want critical_shutdown", decision.Event)
	}
}
