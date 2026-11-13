package power

import (
	"testing"
	"time"

	"github.com/fauzone/kelvios/internal/storage"
)

// fakeClock is a controllable clock for tests.
type fakeClock struct {
	now time.Time
}

func (f *fakeClock) Now() time.Time          { return f.now }
func (f *fakeClock) Sleep(d time.Duration)   { f.now = f.now.Add(d) }

// TestInScheduledWindow tests the InScheduledWindow matrix.
func TestInScheduledWindow(t *testing.T) {
	tests := []struct {
		name       string
		schedules  []Schedule
		now        time.Time
		expected   bool
	}{
		// Weekday match/mismatch
		{
			name: "weekday match - Monday",
			schedules: []Schedule{{Days: "Mon", StartMin: 540, EndMin: 1020, Enabled: true}},
			now:        time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), // Monday
			expected:   true,
		},
		{
			name: "weekday mismatch - Tuesday",
			schedules: []Schedule{{Days: "Mon", StartMin: 540, EndMin: 1020, Enabled: true}},
			now:        time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC), // Tuesday
			expected:   false,
		},
		{
			name: "multiple days - Wednesday matches",
			schedules: []Schedule{{Days: "Mon,Wed,Fri", StartMin: 540, EndMin: 1020, Enabled: true}},
			now:        time.Date(2024, 1, 3, 12, 0, 0, 0, time.UTC), // Wednesday
			expected:   true,
		},
		{
			name: "multiple days - Tuesday no match",
			schedules: []Schedule{{Days: "Mon,Wed,Fri", StartMin: 540, EndMin: 1020, Enabled: true}},
			now:        time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC), // Tuesday
			expected:   false,
		},

		// Normal window inside/outside/boundary
		{
			name: "normal window - inside",
			schedules: []Schedule{{Days: "Mon", StartMin: 540, EndMin: 1020, Enabled: true}}, // 09:00-17:00
			now:        time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC), // 12:00
			expected:   true,
		},
		{
			name: "normal window - before start",
			schedules: []Schedule{{Days: "Mon", StartMin: 540, EndMin: 1020, Enabled: true}},
			now:        time.Date(2024, 1, 1, 8, 0, 0, 0, time.UTC), // 08:00
			expected:   false,
		},
		{
			name: "normal window - after end",
			schedules: []Schedule{{Days: "Mon", StartMin: 540, EndMin: 1020, Enabled: true}},
			now:        time.Date(2024, 1, 1, 18, 0, 0, 0, time.UTC), // 18:00
			expected:   false,
		},
		{
			name: "normal window - boundary start (inclusive)",
			schedules: []Schedule{{Days: "Mon", StartMin: 540, EndMin: 1020, Enabled: true}},
			now:        time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC), // 09:00
			expected:   true,
		},
		{
			name: "normal window - boundary end (exclusive)",
			schedules: []Schedule{{Days: "Mon", StartMin: 540, EndMin: 1020, Enabled: true}},
			now:        time.Date(2024, 1, 1, 17, 0, 0, 0, time.UTC), // 17:00
			expected:   false,
		},

		// Cross-midnight
		{
			name: "cross-midnight - before midnight",
			schedules: []Schedule{{Days: "Mon", StartMin: 1320, EndMin: 120, Enabled: true}}, // 22:00-02:00
			now:        time.Date(2024, 1, 1, 23, 0, 0, 0, time.UTC), // Monday 23:00
			expected:   true,
		},
		{
			name: "cross-midnight - after midnight (needs Mon,Tue)",
			schedules: []Schedule{{Days: "Mon", StartMin: 1320, EndMin: 120, Enabled: true}},
			now:        time.Date(2024, 1, 2, 1, 0, 0, 0, time.UTC), // Tuesday 01:00
			expected:   false,
		},
		{
			name: "cross-midnight - after midnight with both days",
			schedules: []Schedule{{Days: "Mon,Tue", StartMin: 1320, EndMin: 120, Enabled: true}},
			now:        time.Date(2024, 1, 2, 1, 0, 0, 0, time.UTC), // Tuesday 01:00
			expected:   true,
		},
		{
			name: "cross-midnight - outside after end",
			schedules: []Schedule{{Days: "Mon", StartMin: 1320, EndMin: 120, Enabled: true}},
			now:        time.Date(2024, 1, 2, 3, 0, 0, 0, time.UTC), // Tuesday 03:00
			expected:   false,
		},
		{
			name: "cross-midnight - outside before start",
			schedules: []Schedule{{Days: "Mon", StartMin: 1320, EndMin: 120, Enabled: true}},
			now:        time.Date(2024, 1, 1, 20, 0, 0, 0, time.UTC), // Monday 20:00
			expected:   false,
		},
		{
			name: "cross-midnight - boundary start (inclusive)",
			schedules: []Schedule{{Days: "Mon", StartMin: 1320, EndMin: 120, Enabled: true}},
			now:        time.Date(2024, 1, 1, 22, 0, 0, 0, time.UTC), // Monday 22:00
			expected:   true,
		},
		{
			name: "cross-midnight - boundary end (exclusive, needs Mon,Tue)",
			schedules: []Schedule{{Days: "Mon", StartMin: 1320, EndMin: 120, Enabled: true}},
			now:        time.Date(2024, 1, 2, 2, 0, 0, 0, time.UTC), // Tuesday 02:00
			expected:   false,
		},
		{
			name: "cross-midnight - boundary end with both days",
			schedules: []Schedule{{Days: "Mon,Tue", StartMin: 1320, EndMin: 120, Enabled: true}},
			now:        time.Date(2024, 1, 2, 2, 0, 0, 0, time.UTC), // Tuesday 02:00
			expected:   false, // boundary end is exclusive
		},

		// Disabled ignored
		{
			name: "disabled schedule ignored",
			schedules: []Schedule{{Days: "Mon", StartMin: 540, EndMin: 1020, Enabled: false}},
			now:        time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
			expected:   false,
		},
		{
			name: "mixed enabled/disabled - enabled wins",
			schedules: []Schedule{
				{Days: "Mon", StartMin: 540, EndMin: 1020, Enabled: false},
				{Days: "Mon", StartMin: 540, EndMin: 1020, Enabled: true},
			},
			now:        time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
			expected:   true,
		},

		// No schedules
		{
			name:       "no schedules",
			schedules:  []Schedule{},
			now:        time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
			expected:   false,
		},
		{
			name:       "nil schedules",
			schedules:  nil,
			now:        time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
			expected:   false,
		},

		// Zero time uses engine clock
		{
			name: "zero time uses engine clock",
			schedules: []Schedule{{Days: "Mon", StartMin: 540, EndMin: 1020, Enabled: true}},
			now:        time.Time{}, // zero time
			expected:   true, // engine clock will be set to Monday 12:00 in test setup
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &fakeClock{now: tt.now}
			e := New(clock, nil, 0)

			// Register a disk with schedules
			e.RegisterDisk("sda", "pool1")
			e.RegisterPool("pool1", storage.TierWarm, "testpool")
			e.SetSchedules("sda", tt.schedules)

			// For zero time test, set clock to a known time
			if tt.now.IsZero() {
				clock.now = time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
			}

			result := e.InScheduledWindow("sda", tt.now)
			if result != tt.expected {
				t.Errorf("InScheduledWindow = %v, want %v", result, tt.expected)
			}
		})
	}
}

// TestApplyIdleLocked_ScheduledWindowSuppression tests that a disk inside a
// scheduled window stays active past its idle window.
func TestApplyIdleLocked_ScheduledWindowSuppression(t *testing.T) {
	clock := &fakeClock{now: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)} // Monday 12:00
	e := New(clock, nil, 0)

	// Register disk in Warm pool with 60 min window
	e.RegisterDisk("sda", "pool1")
	e.RegisterPool("pool1", storage.TierWarm, "testpool")
	// Schedule: Mon 09:00-17:00 (540-1020)
	e.SetSchedules("sda", []Schedule{{Days: "Mon", StartMin: 540, EndMin: 1020, Enabled: true}})

	// Mark activity at 10:00 (within schedule)
	clock.now = time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	e.MarkActivity("sda")

	// Advance to 11:00 (still within schedule, 1 hour idle but schedule covers now)
	clock.now = time.Date(2024, 1, 1, 11, 0, 0, 0, time.UTC)
	e.Evaluate()

	entry, ok := e.Snapshot("sda")
	if !ok {
		t.Fatal("disk not found")
	}
	if entry.State != storage.PowerActive {
		t.Errorf("expected PowerActive (schedule suppresses sleep), got %s", entry.State)
	}

	// Advance to 18:00 (outside schedule, 8 hours idle > 60 min window)
	clock.now = time.Date(2024, 1, 1, 18, 0, 0, 0, time.UTC)
	e.Evaluate()

	entry, ok = e.Snapshot("sda")
	if !ok {
		t.Fatal("disk not found")
	}
	if entry.State != storage.PowerStandby {
		t.Errorf("expected PowerStandby (outside schedule, idle > window), got %s", entry.State)
	}
}

// TestApplyIdleLocked_NoSchedule_GoesStandby tests that without a schedule,
// a Warm disk goes standby after idle window.
func TestApplyIdleLocked_NoSchedule_GoesStandby(t *testing.T) {
	clock := &fakeClock{now: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)}
	e := New(clock, nil, 0)

	e.RegisterDisk("sda", "pool1")
	e.RegisterPool("pool1", storage.TierWarm, "testpool")
	// No schedules

	// Mark activity
	e.MarkActivity("sda")

	// Advance 30 min (within 60 min window)
	clock.now = time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC)
	e.Evaluate()

	entry, _ := e.Snapshot("sda")
	if entry.State != storage.PowerActive {
		t.Errorf("expected PowerActive at 30 min, got %s", entry.State)
	}

	// Advance 90 min (past 60 min window)
	clock.now = time.Date(2024, 1, 1, 11, 30, 0, 0, time.UTC)
	e.Evaluate()

	entry, _ = e.Snapshot("sda")
	if entry.State != storage.PowerStandby {
		t.Errorf("expected PowerStandby at 90 min, got %s", entry.State)
	}
}

// TestApplyIdleLocked_ColdGoesOff tests that Cold tier goes to Off (not Standby).
func TestApplyIdleLocked_ColdGoesOff(t *testing.T) {
	clock := &fakeClock{now: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)}
	e := New(clock, nil, 0)

	e.RegisterDisk("sda", "pool1")
	e.RegisterPool("pool1", storage.TierCold, "testpool")

	e.MarkActivity("sda")

	// Advance past window
	clock.now = time.Date(2024, 1, 1, 11, 30, 0, 0, time.UTC)
	e.Evaluate()

	entry, _ := e.Snapshot("sda")
	if entry.State != storage.PowerOff {
		t.Errorf("expected PowerOff for Cold tier, got %s", entry.State)
	}
}

// TestApplyIdleLocked_HotNeverSleeps tests that Hot tier never sleeps regardless of schedule.
func TestApplyIdleLocked_HotNeverSleeps(t *testing.T) {
	clock := &fakeClock{now: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)}
	e := New(clock, nil, 0)

	e.RegisterDisk("sda", "pool1")
	e.RegisterPool("pool1", storage.TierHot, "testpool")
	// Even with a schedule that doesn't cover now
	e.SetSchedules("sda", []Schedule{{Days: "Tue", StartMin: 540, EndMin: 1020, Enabled: true}})

	e.MarkActivity("sda")

	// Advance way past window, on a day not covered by schedule
	clock.now = time.Date(2024, 1, 3, 10, 0, 0, 0, time.UTC) // Wednesday
	e.Evaluate()

	entry, _ := e.Snapshot("sda")
	if entry.State != storage.PowerActive {
		t.Errorf("expected PowerActive for Hot tier, got %s", entry.State)
	}
}

// TestApplyIdleLocked_UnpooledNeverSleeps tests that unpooled disks never sleep.
func TestApplyIdleLocked_UnpooledNeverSleeps(t *testing.T) {
	clock := &fakeClock{now: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)}
	e := New(clock, nil, 0)

	e.RegisterDisk("sda", "") // unpooled

	e.MarkActivity("sda")

	// Advance way past any window
	clock.now = time.Date(2024, 1, 3, 10, 0, 0, 0, time.UTC)
	e.Evaluate()

	entry, _ := e.Snapshot("sda")
	if entry.State != storage.PowerActive {
		t.Errorf("expected PowerActive for unpooled disk, got %s", entry.State)
	}
}

// TestApplyIdleLocked_ManualStayAwakeWorks tests that manual stay-awake still works.
func TestApplyIdleLocked_ManualStayAwakeWorks(t *testing.T) {
	clock := &fakeClock{now: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)}
	e := New(clock, nil, 0)

	e.RegisterDisk("sda", "pool1")
	e.RegisterPool("pool1", storage.TierWarm, "testpool")

	e.MarkActivity("sda")

	// Set stay-awake until 12:00 (2 hours from now)
	stayUntil := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC).Unix()
	_, _, err := e.StayAwake("sda", stayUntil)
	if err != nil {
		t.Fatalf("StayAwake: %v", err)
	}

	// Advance to 11:30 (within stay-awake, past idle window)
	clock.now = time.Date(2024, 1, 1, 11, 30, 0, 0, time.UTC)
	e.Evaluate()

	entry, _ := e.Snapshot("sda")
	if entry.State != storage.PowerActive {
		t.Errorf("expected PowerActive (manual stay-awake), got %s", entry.State)
	}

	// Advance to 12:30 (past stay-awake)
	clock.now = time.Date(2024, 1, 1, 12, 30, 0, 0, time.UTC)
	e.Evaluate()

	entry, _ = e.Snapshot("sda")
	if entry.State != storage.PowerStandby {
		t.Errorf("expected PowerStandby (past stay-awake), got %s", entry.State)
	}
}

// TestApplyIdleLocked_ScheduleAndStayAwakeCombined tests that both schedule and stay-awake suppress sleep.
func TestApplyIdleLocked_ScheduleAndStayAwakeCombined(t *testing.T) {
	clock := &fakeClock{now: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)} // Monday 10:00
	e := New(clock, nil, 0)

	e.RegisterDisk("sda", "pool1")
	e.RegisterPool("pool1", storage.TierWarm, "testpool")
	// Schedule: Mon 09:00-11:00 (540-660)
	e.SetSchedules("sda", []Schedule{{Days: "Mon", StartMin: 540, EndMin: 660, Enabled: true}})

	e.MarkActivity("sda")

	// At 10:30 - within schedule, should stay active
	clock.now = time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC)
	e.Evaluate()
	entry, _ := e.Snapshot("sda")
	if entry.State != storage.PowerActive {
		t.Errorf("expected PowerActive at 10:30 (in schedule), got %s", entry.State)
	}

	// At 11:30 - past schedule, but set stay-awake until 13:00
	clock.now = time.Date(2024, 1, 1, 11, 30, 0, 0, time.UTC)
	stayUntil := time.Date(2024, 1, 1, 13, 0, 0, 0, time.UTC).Unix()
	_, _, err := e.StayAwake("sda", stayUntil)
	if err != nil {
		t.Fatalf("StayAwake: %v", err)
	}
	e.Evaluate()
	entry, _ = e.Snapshot("sda")
	if entry.State != storage.PowerActive {
		t.Errorf("expected PowerActive at 11:30 (stay-awake), got %s", entry.State)
	}

	// At 13:30 - past both schedule and stay-awake
	clock.now = time.Date(2024, 1, 1, 13, 30, 0, 0, time.UTC)
	e.Evaluate()
	entry, _ = e.Snapshot("sda")
	if entry.State != storage.PowerStandby {
		t.Errorf("expected PowerStandby at 13:30 (past both), got %s", entry.State)
	}
}

// TestEvaluate_AllDisks tests that Evaluate applies to all tracked disks.
func TestEvaluate_AllDisks(t *testing.T) {
	clock := &fakeClock{now: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)}
	e := New(clock, nil, 0)

	e.RegisterDisk("sda", "pool1")
	e.RegisterDisk("sdb", "pool1")
	e.RegisterPool("pool1", storage.TierWarm, "testpool")

	e.MarkActivity("sda")
	e.MarkActivity("sdb")

	// Advance past window
	clock.now = time.Date(2024, 1, 1, 11, 30, 0, 0, time.UTC)
	e.Evaluate()

	entryA, _ := e.Snapshot("sda")
	entryB, _ := e.Snapshot("sdb")
	if entryA.State != storage.PowerStandby {
		t.Errorf("sda expected PowerStandby, got %s", entryA.State)
	}
	if entryB.State != storage.PowerStandby {
		t.Errorf("sdb expected PowerStandby, got %s", entryB.State)
	}
}

// TestSetSchedules_EmptyClears tests that SetSchedules with empty slice clears schedules.
func TestSetSchedules_EmptyClears(t *testing.T) {
	clock := &fakeClock{now: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)}
	e := New(clock, nil, 0)

	e.RegisterDisk("sda", "pool1")
	e.RegisterPool("pool1", storage.TierWarm, "testpool")
	e.SetSchedules("sda", []Schedule{{Days: "Mon", StartMin: 540, EndMin: 1020, Enabled: true}})

	if !e.InScheduledWindow("sda", clock.now) {
		t.Error("expected schedule to match initially")
	}

	e.SetSchedules("sda", []Schedule{})

	if e.InScheduledWindow("sda", clock.now) {
		t.Error("expected no schedule match after clearing")
	}
}

// TestInScheduledWindow_UnknownDisk tests that unknown disk returns false.
func TestInScheduledWindow_UnknownDisk(t *testing.T) {
	clock := &fakeClock{now: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)}
	e := New(clock, nil, 0)

	result := e.InScheduledWindow("unknown", clock.now)
	if result {
		t.Error("expected false for unknown disk")
	}
}

// TestRecordSMART_Passivity tests that RecordSMART on sleeping/off/unknown
// disks is a no-op: no state change, no actuator wake. On active disks it
// only increments the passive SMART counter.
func TestRecordSMART_Passivity(t *testing.T) {
	tests := []struct {
		name           string
		initialState   storage.PowerState
		expectWake     bool
		expectCounter  int
		expectState    storage.PowerState
	} {
		{
			name:          "unknown disk - no state change, no wake, no counter",
			initialState:  storage.PowerUnknown,
			expectWake:    false,
			expectCounter: 0,
			expectState:   storage.PowerUnknown,
		},
		{
			name:          "standby disk - no state change, no wake, no counter",
			initialState:  storage.PowerStandby,
			expectWake:    false,
			expectCounter: 0,
			expectState:   storage.PowerStandby,
		},
		{
			name:          "off disk - no state change, no wake, no counter",
			initialState:  storage.PowerOff,
			expectWake:    false,
			expectCounter: 0,
			expectState:   storage.PowerOff,
		},
		{
			name:          "active disk - counter increments, no wake, state stays active",
			initialState:  storage.PowerActive,
			expectWake:    false,
			expectCounter: 1,
			expectState:   storage.PowerActive,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &fakeClock{now: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)}
			actuator := &NoopActuator{}
			e := New(clock, actuator, 0)

			e.RegisterDisk("sda", "pool1")
			e.RegisterPool("pool1", storage.TierWarm, "testpool")

			// Set initial state directly via internal access (test helper pattern)
			e.mu.Lock()
			ds := e.disks["sda"]
			ds.state = tt.initialState
			if tt.initialState == storage.PowerActive {
				ds.lastActivity = clock.Now().Unix()
			}
			e.mu.Unlock()

			// Call RecordSMART
			entry, ok := e.RecordSMART("sda")
			if !ok {
				t.Fatal("RecordSMART returned false for known disk")
			}

			// Check actuator wakes
			wakes := actuator.Wakes()
			if tt.expectWake {
				if len(wakes) == 0 {
					t.Error("expected actuator wake, got none")
				} else {
					t.Errorf("expected no actuator wake, got %v", wakes)
				}
			}

			// Check SMART counter
			if entry.SMARTChecks != tt.expectCounter {
				t.Errorf("expected SMARTChecks=%d, got %d", tt.expectCounter, entry.SMARTChecks)
			}

			// Check state unchanged
			if entry.State != tt.expectState {
				t.Errorf("expected state %s, got %s", tt.expectState, entry.State)
			}
		})
	}
}

// TestWakeOrder_StaggeredWake tests that WakeOrder respects request order
// and WakeGap spacing using NoopActuator and FakeClock. No real sleeps.
func TestWakeOrder_StaggeredWake(t *testing.T) {
	clock := &FakeClock{NowTime: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)}
	actuator := &NoopActuator{}
	gap := 100 * time.Millisecond
	e := New(clock, actuator, gap)

	// Register three disks in standby
	for _, d := range []string{"sda", "sdb", "sdc"} {
		e.RegisterDisk(d, "pool1")
	}
	e.RegisterPool("pool1", storage.TierWarm, "testpool")

	// Set all to standby
	e.mu.Lock()
	for _, d := range []string{"sda", "sdb", "sdc"} {
		ds := e.disks[d]
		ds.state = storage.PowerStandby
		ds.lastActivity = clock.Now().Unix() - 3600 // idle for 1 hour
	}
	e.mu.Unlock()

	// Wake in order: sda, sdb, sdc
	e.Wake("sda")
	e.Wake("sdb")
	e.Wake("sdc")

	// Check actuator wakes recorded in order
	wakes := actuator.Wakes()
	if len(wakes) != 3 {
		t.Fatalf("expected 3 actuator wakes, got %d: %v", len(wakes), wakes)
	}
	if wakes[0] != "sda" || wakes[1] != "sdb" || wakes[2] != "sdc" {
		t.Errorf("actuator wakes order mismatch: got %v, want [sda sdb sdc]", wakes)
	}

	// Check WakeOrder matches
	order := e.WakeOrder()
	if len(order) != 3 {
		t.Fatalf("expected 3 wake order entries, got %d: %v", len(order), order)
	}
	if order[0] != "sda" || order[1] != "sdb" || order[2] != "sdc" {
		t.Errorf("WakeOrder mismatch: got %v, want [sda sdb sdc]", order)
	}

	// Check clock advanced by gap between each wake (2 gaps = 200ms total)
	expectedTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC).Add(2 * gap)
	if !clock.Now().Equal(expectedTime) {
		t.Errorf("clock time mismatch: got %v, want %v", clock.Now(), expectedTime)
	}

	// Verify all disks are now active
	for _, d := range []string{"sda", "sdb", "sdc"} {
		entry, _ := e.Snapshot(d)
		if entry.State != storage.PowerActive {
			t.Errorf("%s: expected PowerActive, got %s", d, entry.State)
		}
	}
}

// TestWakeOrder_AlreadyActive_NoQueue tests that waking an already-active
// disk is a no-op success: no queue, no actuator call, no gap wait.
func TestWakeOrder_AlreadyActive_NoQueue(t *testing.T) {
	clock := &FakeClock{NowTime: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)}
	actuator := &NoopActuator{}
	gap := 100 * time.Millisecond
	e := New(clock, actuator, gap)

	e.RegisterDisk("sda", "pool1")
	e.RegisterPool("pool1", storage.TierWarm, "testpool")

	// Set to active
	e.mu.Lock()
	ds := e.disks["sda"]
	ds.state = storage.PowerActive
	ds.lastActivity = clock.Now().Unix()
	e.mu.Unlock()

	// Wake should be instant no-op
	entry, ok := e.Wake("sda")
	if !ok {
		t.Fatal("Wake returned false for known disk")
	}
	if entry.State != storage.PowerActive {
		t.Errorf("expected PowerActive, got %s", entry.State)
	}

	// No actuator wake recorded
	wakes := actuator.Wakes()
	if len(wakes) != 0 {
		t.Errorf("expected 0 actuator wakes for already-active disk, got %v", wakes)
	}

	// WakeOrder unchanged
	order := e.WakeOrder()
	if len(order) != 0 {
		t.Errorf("expected empty WakeOrder, got %v", order)
	}

	// Clock should not have advanced
	if !clock.Now().Equal(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("clock should not advance for no-op wake, got %v", clock.Now())
	}
}

// TestWakeOrder_GapRespected tests that WakeGap is enforced between
// consecutive wakes even when called rapidly.
func TestWakeOrder_GapRespected(t *testing.T) {
	clock := &FakeClock{NowTime: time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)}
	actuator := &NoopActuator{}
	gap := 50 * time.Millisecond
	e := New(clock, actuator, gap)

	for _, d := range []string{"sda", "sdb"} {
		e.RegisterDisk(d, "pool1")
	}
	e.RegisterPool("pool1", storage.TierWarm, "testpool")

	e.mu.Lock()
	for _, d := range []string{"sda", "sdb"} {
		ds := e.disks[d]
		ds.state = storage.PowerStandby
		ds.lastActivity = clock.Now().Unix() - 3600
	}
	e.mu.Unlock()

	start := clock.Now()
	e.Wake("sda")
	afterFirst := clock.Now()
	e.Wake("sdb")
	afterSecond := clock.Now()

	// First wake should be immediate (no previous wake)
	if !afterFirst.Equal(start) {
		t.Errorf("first wake should not wait, clock advanced to %v", afterFirst)
	}

	// Second wake should have waited for gap
	expectedAfterSecond := start.Add(gap)
	if !afterSecond.Equal(expectedAfterSecond) {
		t.Errorf("second wake should wait for gap: got %v, want %v", afterSecond, expectedAfterSecond)
	}
}

// TestEvaluate_NeverWakes tests that Evaluate() only produces sleep-direction
// transitions (active -> standby/off) and ZERO actuator wakes.
func TestEvaluate_NeverWakes(t *testing.T) {
	clock := &FakeClock{NowTime: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)}
	actuator := &NoopActuator{}
	e := New(clock, actuator, 0)

	// Register disks of different tiers
	e.RegisterDisk("sda", "pool-warm")
	e.RegisterDisk("sdb", "pool-cold")
	e.RegisterDisk("sdc", "pool-hot")
	e.RegisterDisk("sdd", "") // unpooled

	e.RegisterPool("pool-warm", storage.TierWarm, "warm")
	e.RegisterPool("pool-cold", storage.TierCold, "cold")
	e.RegisterPool("pool-hot", storage.TierHot, "hot")

	// Mark all as active with old activity time
	for _, d := range []string{"sda", "sdb", "sdc", "sdd"} {
		e.mu.Lock()
		ds := e.disks[d]
		ds.state = storage.PowerActive
		ds.lastActivity = clock.Now().Unix() - 7200 // 2 hours ago
		e.mu.Unlock()
	}

	// Run Evaluate
	e.Evaluate()

	// Check actuator: ZERO wakes
	wakes := actuator.Wakes()
	if len(wakes) != 0 {
		t.Errorf("Evaluate must not trigger actuator wakes, got %v", wakes)
	}

	// Check state transitions: only sleep-direction
	entryA, _ := e.Snapshot("sda") // Warm -> Standby
	if entryA.State != storage.PowerStandby {
		t.Errorf("sda (Warm): expected PowerStandby, got %s", entryA.State)
	}

	entryB, _ := e.Snapshot("sdb") // Cold -> Off
	if entryB.State != storage.PowerOff {
		t.Errorf("sdb (Cold): expected PowerOff, got %s", entryB.State)
	}

	entryC, _ := e.Snapshot("sdc") // Hot -> stays Active
	if entryC.State != storage.PowerActive {
		t.Errorf("sdc (Hot): expected PowerActive, got %s", entryC.State)
	}

	entryD, _ := e.Snapshot("sdd") // Unpooled -> stays Active
	if entryD.State != storage.PowerActive {
		t.Errorf("sdd (Unpooled): expected PowerActive, got %s", entryD.State)
	}

	// Verify no wake order recorded
	order := e.WakeOrder()
	if len(order) != 0 {
		t.Errorf("Evaluate must not record wake order, got %v", order)
	}
}

// TestEvaluate_AlreadySleeping_NoChange tests that Evaluate on already
// sleeping/off/unknown disks does nothing (no state change, no wakes).
func TestEvaluate_AlreadySleeping_NoChange(t *testing.T) {
	clock := &FakeClock{NowTime: time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)}
	actuator := &NoopActuator{}
	e := New(clock, actuator, 0)

	e.RegisterDisk("sda", "pool1")
	e.RegisterPool("pool1", storage.TierWarm, "testpool")

	// Set to standby (already sleeping)
	e.mu.Lock()
	ds := e.disks["sda"]
	ds.state = storage.PowerStandby
	ds.lastActivity = clock.Now().Unix() - 7200
	e.mu.Unlock()

	e.Evaluate()

	// State unchanged
	entry, _ := e.Snapshot("sda")
	if entry.State != storage.PowerStandby {
		t.Errorf("expected PowerStandby unchanged, got %s", entry.State)
	}

	// No actuator wakes
	wakes := actuator.Wakes()
	if len(wakes) != 0 {
		t.Errorf("expected 0 actuator wakes, got %v", wakes)
	}
}
