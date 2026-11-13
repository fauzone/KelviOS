// Package power implements the KelviOS Phase 1 power engine (FR-1.6/FR-1.7).
//
// Policy + state only, NO real hardware calls. The actuator is an interface
// with a no-op implementation; a later workstream provides hdparm/sdparm.
// Browsing state never wakes a disk (PR-1.4): Snapshot/List are pure reads.
//
// State is in-memory only: after a core restart every disk returns to
// "unknown" until the engine tracks a transition it caused itself
// (consistent with the storage.PowerState comment). No DB migration.
package power

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fauzone/kelvios/internal/storage"
)

// Sentinel errors for explicit sleep/window policy refusals.
var (
	// ErrUnpooled is returned when an operation needs a pool tier but the
	// disk belongs to no pool.
	ErrUnpooled = errors.New("disk has no pool")
	// ErrHotNeverSleeps is returned when an explicit sleep targets a Hot disk.
	ErrHotNeverSleeps = errors.New("hot tier never sleeps")
	// ErrWindowOutOfRange is returned when a window value is outside 15-240.
	ErrWindowOutOfRange = errors.New("window out of range")
	// ErrPastStayAwake is returned when stay-awake targets now or the past.
	ErrPastStayAwake = errors.New("until must be in the future")
)

// Window bounds and defaults.
const (
	DefaultWindowMinutes = 60
	MinWindowMinutes     = 15
	MaxWindowMinutes     = 240
	DefaultWakeGap       = 5 * time.Second
)

// Clock is a deterministic clock: real clock in production, fake in tests.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

// SystemClock uses the real wall clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time          { return time.Now() }
func (SystemClock) Sleep(d time.Duration)   { time.Sleep(d) }

// FakeClock is a controllable clock for tests.
type FakeClock struct {
	NowTime time.Time
}

func (f *FakeClock) Now() time.Time       { return f.NowTime }
func (f *FakeClock) Sleep(d time.Duration) { f.NowTime = f.NowTime.Add(d) }

// Actuator performs the physical wake. No-op in this workstream.
type Actuator interface {
	Wake(disk string) error
}

// NoopActuator records wakes without touching hardware.
type NoopActuator struct {
	mu    sync.Mutex
	wakes []string
}

func (a *NoopActuator) Wake(disk string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.wakes = append(a.wakes, disk)
	return nil
}

// Wakes returns a copy of recorded wake requests in order.
func (a *NoopActuator) Wakes() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, len(a.wakes))
	copy(out, a.wakes)
	return out
}

// DiskEntry is the per-disk power state snapshot.
type DiskEntry struct {
	Disk         string
	Tier         *storage.Tier // nil when unpooled
	PoolID       string
	PoolName     string
	State        storage.PowerState
	LastActivity int64 // unix seconds, 0 = none
	KeepAwake    int64 // unix seconds, 0 = none
	WindowMin    int   // effective window for the owning pool (0 when unpooled)
	SMARTChecks  int
}

// diskState is internal mutable state.
type diskState struct {
	tier         *storage.Tier
	poolID       string
	poolName     string
	state        storage.PowerState
	lastActivity int64
	keepAwake    int64
	smartChecks  int
}

// Engine is the in-memory power policy engine.
type Engine struct {
	mu       sync.Mutex
	clock    Clock
	actuator Actuator
	wakeGap  time.Duration

	disks map[string]*diskState
	// poolWindowMin maps poolID -> window minutes.
	poolWindowMin map[string]int
	// poolTier maps poolID -> tier.
	poolTier map[string]storage.Tier
	// poolName maps poolID -> name (for responses).
	poolName map[string]string

	// schedules maps disk -> list of schedules (from catalog).
	// Each schedule: {days, startMin, endMin, enabled}.
	// Updated via SetSchedules (called after catalog sync).
	schedules map[string][]Schedule

	// wakeMu serializes physical wakes: one wake at a time, in request
	// order, with wakeGap between completions and next start.
	wakeMu sync.Mutex
	// lastWakeEnd is the clock-time when the last wake completed.
	lastWakeEnd time.Time
	// wakeOrder records completion order of wakes.
	wakeOrder []string
}

// Schedule represents a recurring keep-awake window for a disk.
// days: comma-separated weekdays (Mon,Tue,Wed,Thu,Fri,Sat,Sun).
// startMin/endMin: minutes since midnight (0-1439). start != end.
// Cross-midnight windows have startMin > endMin (e.g., 22:00-02:00 = 1320, 120).
type Schedule struct {
	Days     string
	StartMin int
	EndMin   int
	Enabled  bool
}

// New creates an engine. Nil clock -> SystemClock, nil actuator -> NoopActuator.
// Non-positive gap -> DefaultWakeGap.
func New(clock Clock, actuator Actuator, wakeGap time.Duration) *Engine {
	if clock == nil {
		clock = SystemClock{}
	}
	if actuator == nil {
		actuator = &NoopActuator{}
	}
	if wakeGap <= 0 {
		wakeGap = DefaultWakeGap
	}
	return &Engine{
		clock:         clock,
		actuator:      actuator,
		wakeGap:       wakeGap,
		disks:         map[string]*diskState{},
		poolWindowMin: map[string]int{},
		poolTier:      map[string]storage.Tier{},
		poolName:      map[string]string{},
		schedules:     map[string][]Schedule{},
	}
}

// SetWakeGap updates the stagger gap (tests may inject 0/small).
func (e *Engine) SetWakeGap(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if d < 0 {
		d = 0
	}
	e.wakeGap = d
}

// RegisterPool records tier + window default for a pool.
func (e *Engine) RegisterPool(poolID string, tier storage.Tier, name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t := tier
	e.poolTier[poolID] = t
	e.poolName[poolID] = name
	if _, ok := e.poolWindowMin[poolID]; !ok {
		e.poolWindowMin[poolID] = DefaultWindowMinutes
	}
	// Refresh tier on member disks.
	for _, ds := range e.disks {
		if ds.poolID == poolID {
			tt := t
			ds.tier = &tt
			ds.poolName = name
		}
	}
}

// UnregisterPool removes pool config; member disks become unpooled
// (state preserved, policy disabled).
func (e *Engine) UnregisterPool(poolID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.poolTier, poolID)
	delete(e.poolWindowMin, poolID)
	delete(e.poolName, poolID)
	for _, ds := range e.disks {
		if ds.poolID == poolID {
			ds.tier = nil
			ds.poolID = ""
			ds.poolName = ""
		}
	}
}

// RegisterDisk ensures a disk exists in unknown state if new.
// poolID "" means unpooled.
func (e *Engine) RegisterDisk(disk, poolID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ds, ok := e.disks[disk]
	if !ok {
		ds = &diskState{state: storage.PowerUnknown}
		e.disks[disk] = ds
	}
	if poolID == "" {
		ds.tier = nil
		ds.poolID = ""
		ds.poolName = ""
		return
	}
	ds.poolID = poolID
	if t, ok := e.poolTier[poolID]; ok {
		tt := t
		ds.tier = &tt
	}
	if n, ok := e.poolName[poolID]; ok {
		ds.poolName = n
	}
}

// SetSchedules replaces the schedule list for a disk.
// Called after catalog sync to refresh schedules from the database.
func (e *Engine) SetSchedules(disk string, schedules []Schedule) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(schedules) == 0 {
		delete(e.schedules, disk)
		return
	}
	// Copy to avoid external mutation
	copied := make([]Schedule, len(schedules))
	copy(copied, schedules)
	e.schedules[disk] = copied
}

// InScheduledWindow reports whether the disk has any enabled schedule
// covering the given time. Uses the engine's clock if now is zero time.
// This is the core evaluation function for FR-1.7 scheduled windows.
func (e *Engine) InScheduledWindow(disk string, now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	scheds, ok := e.schedules[disk]
	if !ok || len(scheds) == 0 {
		return false
	}
	if now.IsZero() {
		now = e.clock.Now()
	}
	weekday := now.Weekday().String()[:3] // Mon, Tue, etc.
	nowMin := now.Hour()*60 + now.Minute()
	for _, s := range scheds {
		if !s.Enabled {
			continue
		}
		// Check weekday
		dayMatch := false
		for _, d := range strings.Split(s.Days, ",") {
			if d == weekday {
				dayMatch = true
				break
			}
		}
		if !dayMatch {
			continue
		}
		// Check time window
		if s.StartMin < s.EndMin {
			// Normal window: start < end (e.g., 09:00-17:00)
			if nowMin >= s.StartMin && nowMin < s.EndMin {
				return true
			}
		} else {
			// Cross-midnight window: start > end (e.g., 22:00-02:00)
			if nowMin >= s.StartMin || nowMin < s.EndMin {
				return true
			}
		}
	}
	return false
}

// SyncFromCatalog reconciles engine membership with catalog pools.
// pools maps poolID -> {tier, name, disks}. Disks in inventory but in no
// pool are registered as unpooled. Engine never invents disk names:
// only names present in pools or inventory are tracked. Unknown disks
// previously tracked but absent from both are dropped.
func (e *Engine) SyncFromCatalog(pools map[string]PoolInfo, inventory []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Refresh pool configs.
	for id, p := range pools {
		t := p.Tier
		e.poolTier[id] = t
		e.poolName[id] = p.Name
		if _, ok := e.poolWindowMin[id]; !ok {
			e.poolWindowMin[id] = DefaultWindowMinutes
		}
	}
	// Remove pools that vanished.
	for id := range e.poolTier {
		if _, ok := pools[id]; !ok {
			delete(e.poolTier, id)
			delete(e.poolWindowMin, id)
			delete(e.poolName, id)
		}
	}
	// Build desired membership: disk -> poolID.
	want := map[string]string{}
	for id, p := range pools {
		for _, d := range p.Disks {
			want[d] = id
		}
	}
	for _, d := range inventory {
		if _, ok := want[d]; !ok {
			// Mark inventory-only disks as unpooled candidates.
			if _, ok := want[d]; !ok {
				// only set if not already pool-assigned
				if _, exists := want[d]; !exists {
					// keep absent -> registered unpooled below
				}
			}
		}
	}
	// Ensure all wanted + inventory disks exist.
	for d, pid := range want {
		ds, ok := e.disks[d]
		if !ok {
			ds = &diskState{state: storage.PowerUnknown}
			e.disks[d] = ds
		}
		ds.poolID = pid
		if t, ok := e.poolTier[pid]; ok {
			tt := t
			ds.tier = &tt
		} else {
			ds.tier = nil
		}
		ds.poolName = e.poolName[pid]
	}
	for _, d := range inventory {
		if _, ok := want[d]; ok {
			continue
		}
		ds, ok := e.disks[d]
		if !ok {
			ds = &diskState{state: storage.PowerUnknown}
			e.disks[d] = ds
		}
		ds.tier = nil
		ds.poolID = ""
		ds.poolName = ""
	}
	// Drop disks neither in pools nor inventory.
	keep := map[string]bool{}
	for d := range want {
		keep[d] = true
	}
	for _, d := range inventory {
		keep[d] = true
	}
	for d := range e.disks {
		if !keep[d] {
			delete(e.disks, d)
		}
	}
}

// PoolInfo describes one pool for SyncFromCatalog.
type PoolInfo struct {
	Tier  storage.Tier
	Name  string
	Disks []string
}

// Exists reports whether disk is tracked.
func (e *Engine) Exists(disk string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.disks[disk]
	return ok
}

// Snapshot returns a copy of one disk entry. Pure read: never mutates.
func (e *Engine) Snapshot(disk string) (DiskEntry, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ds, ok := e.disks[disk]
	if !ok {
		return DiskEntry{}, false
	}
	return e.entryLocked(disk, ds), true
}

// List returns all entries sorted by disk name. Pure read.
func (e *Engine) List() []DiskEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	names := make([]string, 0, len(e.disks))
	for n := range e.disks {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]DiskEntry, 0, len(names))
	for _, n := range names {
		out = append(out, e.entryLocked(n, e.disks[n]))
	}
	return out
}

func (e *Engine) entryLocked(disk string, ds *diskState) DiskEntry {
	en := DiskEntry{
		Disk:         disk,
		State:        ds.state,
		LastActivity: ds.lastActivity,
		KeepAwake:    ds.keepAwake,
		SMARTChecks:  ds.smartChecks,
	}
	if ds.tier != nil {
		t := *ds.tier
		en.Tier = &t
		en.PoolID = ds.poolID
		en.PoolName = ds.poolName
		if w, ok := e.poolWindowMin[ds.poolID]; ok {
			en.WindowMin = w
		} else {
			en.WindowMin = DefaultWindowMinutes
		}
	}
	return en
}

// windowLocked returns effective window duration for a disk state.
func (e *Engine) windowLocked(ds *diskState) time.Duration {
	if ds.poolID == "" {
		return time.Duration(DefaultWindowMinutes) * time.Minute
	}
	if w, ok := e.poolWindowMin[ds.poolID]; ok {
		return time.Duration(w) * time.Minute
	}
	return time.Duration(DefaultWindowMinutes) * time.Minute
}

// applyIdleLocked transitions active -> standby/off when the idle window
// elapsed and no keep-awake covers now. Hot and unpooled never sleep.
// Scheduled windows (FR-1.7) also suppress sleep while the disk is awake.
func (e *Engine) applyIdleLocked(ds *diskState, now time.Time) {
	if ds.state != storage.PowerActive {
		return
	}
	if ds.tier == nil {
		return
	}
	if *ds.tier == storage.TierHot {
		return
	}
	if ds.lastActivity == 0 {
		return
	}
	if ds.keepAwake != 0 && now.Unix() < ds.keepAwake {
		return
	}
	// Scheduled window check: if any enabled schedule covers now, suppress sleep.
	// We need the disk name to look up schedules. Since diskState doesn't
	// store the disk name, we check all schedules for a match on this disk's
	// pool/disk identity. For simplicity, we iterate all schedule entries
	// and check if any matches a disk with this poolID.
	// Note: This is called with e.mu held, so schedules map is safe to read.
	for diskName, scheds := range e.schedules {
		// Find the diskState for this diskName to check poolID match
		if ds2, ok := e.disks[diskName]; ok && ds2 == ds {
			for _, s := range scheds {
				if !s.Enabled {
					continue
				}
				weekday := now.Weekday().String()[:3]
				dayMatch := false
				for _, d := range strings.Split(s.Days, ",") {
					if d == weekday {
						dayMatch = true
						break
					}
				}
				if !dayMatch {
					continue
				}
				nowMin := now.Hour()*60 + now.Minute()
				if s.StartMin < s.EndMin {
					if nowMin >= s.StartMin && nowMin < s.EndMin {
						return // scheduled window covers now -> suppress sleep
					}
				} else {
					if nowMin >= s.StartMin || nowMin < s.EndMin {
						return // cross-midnight window covers now
					}
				}
			}
			break // found the disk, no need to check other schedule entries
		}
	}
	idle := now.Sub(time.Unix(ds.lastActivity, 0))
	if idle < e.windowLocked(ds) {
		return
	}
	if *ds.tier == storage.TierWarm {
		ds.state = storage.PowerStandby
	} else {
		ds.state = storage.PowerOff
	}
}

// Evaluate applies idle policy to all disks at the current clock time.
// Called by mutating operations; never by reads.
func (e *Engine) Evaluate() {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock.Now()
	for _, ds := range e.disks {
		e.applyIdleLocked(ds, now)
	}
}

// MarkActivity records activity (streaming counts): resets idle timer.
// If the disk was sleeping/off/unknown, it becomes active (policy wake,
// not a staggered hardware wake).
func (e *Engine) MarkActivity(disk string) (DiskEntry, bool) {
	e.mu.Lock()
	ds, ok := e.disks[disk]
	if !ok {
		e.mu.Unlock()
		return DiskEntry{}, false
	}
	now := e.clock.Now()
	e.applyIdleLocked(ds, now)
	ds.lastActivity = now.Unix()
	if ds.state != storage.PowerActive {
		ds.state = storage.PowerActive
	}
	en := e.entryLocked(disk, ds)
	e.mu.Unlock()
	return en, true
}

// StayAwake sets keep_awake_until. Past values are rejected.
func (e *Engine) StayAwake(disk string, until int64) (DiskEntry, bool, error) {
	e.mu.Lock()
	ds, ok := e.disks[disk]
	if !ok {
		e.mu.Unlock()
		return DiskEntry{}, false, nil
	}
	now := e.clock.Now().Unix()
	if until <= now {
		e.mu.Unlock()
		return DiskEntry{}, true, ErrPastStayAwake
	}
	// Set keepAwake FIRST so applyIdleLocked sees it and suppresses sleep.
	ds.keepAwake = until
	e.applyIdleLocked(ds, e.clock.Now())
	en := e.entryLocked(disk, ds)
	e.mu.Unlock()
	return en, true, nil
}

// SetWindow sets the owning pool window. Range 15-240 enforced here too.
func (e *Engine) SetWindow(disk string, minutes int) (DiskEntry, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ds, ok := e.disks[disk]
	if !ok {
		return DiskEntry{}, false, nil
	}
	if ds.poolID == "" || ds.tier == nil {
		return DiskEntry{}, true, ErrUnpooled
	}
	if minutes < MinWindowMinutes || minutes > MaxWindowMinutes {
		return DiskEntry{}, true, ErrWindowOutOfRange
	}
	e.poolWindowMin[ds.poolID] = minutes
	return e.entryLocked(disk, ds), true, nil
}

// Sleep performs an explicit sleep request.
func (e *Engine) Sleep(disk string) (DiskEntry, bool, error) {
	e.mu.Lock()
	ds, ok := e.disks[disk]
	if !ok {
		e.mu.Unlock()
		return DiskEntry{}, false, nil
	}
	e.applyIdleLocked(ds, e.clock.Now())
	if ds.tier == nil || ds.poolID == "" {
		en := e.entryLocked(disk, ds)
		e.mu.Unlock()
		return en, true, ErrUnpooled
	}
	if *ds.tier == storage.TierHot {
		en := e.entryLocked(disk, ds)
		e.mu.Unlock()
		return en, true, ErrHotNeverSleeps
	}
	if *ds.tier == storage.TierWarm {
		ds.state = storage.PowerStandby
	} else {
		ds.state = storage.PowerOff
	}
	en := e.entryLocked(disk, ds)
	e.mu.Unlock()
	return en, true, nil
}

// Wake performs an explicit wake request through the serial staggered queue.
// Already-active disk is a no-op success (no queue, no actuator call).
func (e *Engine) Wake(disk string) (DiskEntry, bool) {
	e.mu.Lock()
	ds, ok := e.disks[disk]
	if !ok {
		e.mu.Unlock()
		return DiskEntry{}, false
	}
	e.applyIdleLocked(ds, e.clock.Now())
	if ds.state == storage.PowerActive {
		en := e.entryLocked(disk, ds)
		e.mu.Unlock()
		return en, true
	}
	e.mu.Unlock()

	// Serial queue: one wake at a time, in request order, gap between wakes.
	e.wakeMu.Lock()
	defer e.wakeMu.Unlock()

	// Enforce gap since last wake completion (clock-based, fake-clock safe).
	e.mu.Lock()
	gap := e.wakeGap
	lastEnd := e.lastWakeEnd
	e.mu.Unlock()
	if !lastEnd.IsZero() && gap > 0 {
		now := e.clock.Now()
		if elapsed := now.Sub(lastEnd); elapsed < gap {
			e.clock.Sleep(gap - elapsed)
		}
	}

	_ = e.actuator.Wake(disk)

	e.mu.Lock()
	now := e.clock.Now()
	if ds2, ok := e.disks[disk]; ok {
		ds2.state = storage.PowerActive
		ds2.lastActivity = now.Unix()
		en := e.entryLocked(disk, ds2)
		e.wakeOrder = append(e.wakeOrder, disk)
		e.lastWakeEnd = now
		e.mu.Unlock()
		return en, true
	}
	e.mu.Unlock()
	return DiskEntry{}, false
}

// RecordSMART records a passive SMART check only when already active.
// On sleeping/unknown disks it is a recorded no-op: no state change, no wake.
func (e *Engine) RecordSMART(disk string) (DiskEntry, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ds, ok := e.disks[disk]
	if !ok {
		return DiskEntry{}, false
	}
	if ds.state == storage.PowerActive {
		ds.smartChecks++
	}
	return e.entryLocked(disk, ds), true
}

// WakeOrder returns completion order of staggered wakes.
func (e *Engine) WakeOrder() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.wakeOrder))
	copy(out, e.wakeOrder)
	return out
}

// ActuatorWakes returns recorded actuator wakes when using NoopActuator.
func (e *Engine) ActuatorWakes() []string {
	if n, ok := e.actuator.(*NoopActuator); ok {
		return n.Wakes()
	}
	return nil
}

// GetSchedules returns the schedules for a disk.
func (e *Engine) GetSchedules(disk string) []Schedule {
	e.mu.Lock()
	defer e.mu.Unlock()
	scheds, ok := e.schedules[disk]
	if !ok {
		return nil
	}
	// Return a copy to avoid external mutation
	copied := make([]Schedule, len(scheds))
	copy(copied, scheds)
	return copied
}
