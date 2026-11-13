package ups

import (
	"context"
	"time"
)

// Poller polls a VarSource at a regular interval and emits readings and decisions.
type Poller struct {
	Source          VarSource
	CriticalPercent int // <= 0 means DefaultCriticalPercent
}

func (p *Poller) criticalPercent() int {
	if p.CriticalPercent <= 0 {
		return DefaultCriticalPercent
	}
	return p.CriticalPercent
}

// PollOnce performs a single poll cycle: GetVars -> ParseVars -> Decide.
// Returns the current reading, the decision, and any error from the source.
func (p *Poller) PollOnce(prev Reading) (Reading, Decision, error) {
	vars, err := p.Source.GetVars()
	if err != nil {
		return Reading{}, Decision{}, err
	}
	cur := ParseVars(vars)
	decision := Decide(prev, cur, p.criticalPercent())
	return cur, decision, nil
}

// Run starts the polling loop.
// - ctx: cancellation context; when done, Run returns nil (no error).
// - interval: polling interval.
// - emit: called when Decision.Event != "" with the reading and decision.
// - onError: called when source returns an error; poller continues to next tick.
// Run returns only when ctx is cancelled. No goroutine leaks.
func (p *Poller) Run(ctx context.Context, interval time.Duration, emit func(Reading, Decision), onError func(error)) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var prev Reading
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cur, decision, err := p.PollOnce(prev)
			if err != nil {
				if onError != nil {
					onError(err)
				}
				continue
			}
			if decision.Event != "" {
				if emit != nil {
					emit(cur, decision)
				}
			}
			prev = cur
		}
	}
}
