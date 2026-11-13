package ups

import "testing"

func TestParseVars(t *testing.T) {
	tests := []struct {
		name     string
		vars     map[string]string
		expected Reading
	}{
		{
			name: "empty map",
			vars: map[string]string{},
			expected: Reading{
				Present:        false,
				State:          StateUnknown,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
		},
		{
			name: "missing status",
			vars: map[string]string{
				"battery.charge": "85.0",
			},
			expected: Reading{
				Present:        false,
				State:          StateUnknown,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
		},
		{
			name: "blank status",
			vars: map[string]string{
				"ups.status": "",
			},
			expected: Reading{
				Present:        false,
				State:          StateUnknown,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
		},
		{
			name: "OL online",
			vars: map[string]string{
				"ups.status": "OL",
			},
			expected: Reading{
				Present:        true,
				State:          StateOnline,
				LowBattery:     false,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
		},
		{
			name: "OB on battery",
			vars: map[string]string{
				"ups.status": "OB",
			},
			expected: Reading{
				Present:        true,
				State:          StateOnBattery,
				LowBattery:     false,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
		},
		{
			name: "OB LB low battery",
			vars: map[string]string{
				"ups.status": "OB LB",
			},
			expected: Reading{
				Present:        true,
				State:          StateOnBattery,
				LowBattery:     true,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
		},
		{
			name: "FSD low battery",
			vars: map[string]string{
				"ups.status": "OB FSD",
			},
			expected: Reading{
				Present:        true,
				State:          StateOnBattery,
				LowBattery:     true,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
		},
		{
			name: "charge 85.0 -> 85",
			vars: map[string]string{
				"ups.status":     "OL",
				"battery.charge": "85.0",
			},
			expected: Reading{
				Present:        true,
				State:          StateOnline,
				LowBattery:     false,
				ChargePercent:  85,
				RuntimeSeconds: -1,
			},
		},
		{
			name: "charge garbage -> -1",
			vars: map[string]string{
				"ups.status":     "OL",
				"battery.charge": "garbage",
			},
			expected: Reading{
				Present:        true,
				State:          StateOnline,
				LowBattery:     false,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
		},
		{
			name: "charge 130 -> 100 (clamped)",
			vars: map[string]string{
				"ups.status":     "OL",
				"battery.charge": "130",
			},
			expected: Reading{
				Present:        true,
				State:          StateOnline,
				LowBattery:     false,
				ChargePercent:  100,
				RuntimeSeconds: -1,
			},
		},
		{
			name: "charge negative -> -1",
			vars: map[string]string{
				"ups.status":     "OL",
				"battery.charge": "-5",
			},
			expected: Reading{
				Present:        true,
				State:          StateOnline,
				LowBattery:     false,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
		},
		{
			name: "runtime parse valid",
			vars: map[string]string{
				"ups.status":      "OL",
				"battery.runtime": "3600",
			},
			expected: Reading{
				Present:        true,
				State:          StateOnline,
				LowBattery:     false,
				ChargePercent:  -1,
				RuntimeSeconds: 3600,
			},
		},
		{
			name: "runtime garbage -> -1",
			vars: map[string]string{
				"ups.status":      "OL",
				"battery.runtime": "garbage",
			},
			expected: Reading{
				Present:        true,
				State:          StateOnline,
				LowBattery:     false,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
		},
		{
			name: "runtime negative -> -1",
			vars: map[string]string{
				"ups.status":      "OL",
				"battery.runtime": "-100",
			},
			expected: Reading{
				Present:        true,
				State:          StateOnline,
				LowBattery:     false,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
		},
		{
			name: "multiple status tokens OB takes precedence over OL",
			vars: map[string]string{
				"ups.status": "OL OB",
			},
			expected: Reading{
				Present:        true,
				State:          StateOnBattery,
				LowBattery:     false,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
		},
		{
			name: "OL LB -> online with low battery flag",
			vars: map[string]string{
				"ups.status":     "OL LB",
				"battery.charge": "90",
			},
			expected: Reading{
				Present:        true,
				State:          StateOnline,
				LowBattery:     true,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseVars(tt.vars)
			if result != tt.expected {
				t.Errorf("ParseVars(%v) = %+v, want %+v", tt.vars, result, tt.expected)
			}
		})
	}
}

func TestDecide(t *testing.T) {
	tests := []struct {
		name            string
		prev            Reading
		cur             Reading
		criticalPercent int
		expected        Decision
	}{
		{
			name: "unknown current -> zero",
			prev: Reading{Present: true, State: StateOnline},
			cur: Reading{
				Present:        false,
				State:          StateUnknown,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
			criticalPercent: 20,
			expected:        Decision{},
		},
		{
			name: "first OB reading at 90% -> on_battery+notify",
			prev: Reading{
				Present:        true,
				State:          StateOnline,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			cur: Reading{
				Present:        true,
				State:          StateOnBattery,
				LowBattery:     false,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			criticalPercent: 20,
			expected: Decision{
				Event:  EventOnBattery,
				Notify: true,
			},
		},
		{
			name: "OB->OL -> online_restored",
			prev: Reading{
				Present:        true,
				State:          StateOnBattery,
				LowBattery:     false,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			cur: Reading{
				Present:        true,
				State:          StateOnline,
				LowBattery:     false,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			criticalPercent: 20,
			expected: Decision{
				Event:  EventOnlineRestored,
				Notify: true,
			},
		},
		{
			name: "OB at 20% with critical 20 -> critical+shutdown",
			prev: Reading{
				Present:        true,
				State:          StateOnline,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			cur: Reading{
				Present:        true,
				State:          StateOnBattery,
				LowBattery:     false,
				ChargePercent:  20,
				RuntimeSeconds: -1,
			},
			criticalPercent: 20,
			expected: Decision{
				Event:         EventCriticalShutdown,
				BeginShutdown: true,
				Notify:        true,
			},
		},
		{
			name: "OB at 21% -> no critical",
			prev: Reading{
				Present:        true,
				State:          StateOnline,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			cur: Reading{
				Present:        true,
				State:          StateOnBattery,
				LowBattery:     false,
				ChargePercent:  21,
				RuntimeSeconds: -1,
			},
			criticalPercent: 20,
			expected: Decision{
				Event:  EventOnBattery,
				Notify: true,
			},
		},
		{
			name: "OB LB at 90% -> critical (low battery flag wins)",
			prev: Reading{
				Present:        true,
				State:          StateOnline,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			cur: Reading{
				Present:        true,
				State:          StateOnBattery,
				LowBattery:     true,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			criticalPercent: 20,
			expected: Decision{
				Event:         EventCriticalShutdown,
				BeginShutdown: true,
				Notify:        true,
			},
		},
		{
			name: "steady OL->OL -> zero",
			prev: Reading{
				Present:        true,
				State:          StateOnline,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			cur: Reading{
				Present:        true,
				State:          StateOnline,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			criticalPercent: 20,
			expected:        Decision{},
		},
		{
			name: "criticalPercent 0 -> default applies (20%)",
			prev: Reading{
				Present:        true,
				State:          StateOnline,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			cur: Reading{
				Present:        true,
				State:          StateOnBattery,
				LowBattery:     false,
				ChargePercent:  20,
				RuntimeSeconds: -1,
			},
			criticalPercent: 0,
			expected: Decision{
				Event:         EventCriticalShutdown,
				BeginShutdown: true,
				Notify:        true,
			},
		},
		{
			name: "criticalPercent negative -> default applies",
			prev: Reading{
				Present:        true,
				State:          StateOnline,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			cur: Reading{
				Present:        true,
				State:          StateOnBattery,
				LowBattery:     false,
				ChargePercent:  20,
				RuntimeSeconds: -1,
			},
			criticalPercent: -5,
			expected: Decision{
				Event:         EventCriticalShutdown,
				BeginShutdown: true,
				Notify:        true,
			},
		},
		{
			name: "invalid prev (!Present) counts as StateUnknown",
			prev: Reading{
				Present:        false,
				State:          StateUnknown,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
			cur: Reading{
				Present:        true,
				State:          StateOnBattery,
				LowBattery:     false,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			criticalPercent: 20,
			expected: Decision{
				Event:  EventOnBattery,
				Notify: true,
			},
		},
		{
			name: "first reading OB at 15% (below critical) -> critical shutdown immediately",
			prev: Reading{
				Present:        false,
				State:          StateUnknown,
				ChargePercent:  -1,
				RuntimeSeconds: -1,
			},
			cur: Reading{
				Present:        true,
				State:          StateOnBattery,
				LowBattery:     false,
				ChargePercent:  15,
				RuntimeSeconds: -1,
			},
			criticalPercent: 20,
			expected: Decision{
				Event:         EventCriticalShutdown,
				BeginShutdown: true,
				Notify:        true,
			},
		},
		{
			name: "FSD flag triggers critical shutdown",
			prev: Reading{
				Present:        true,
				State:          StateOnline,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			cur: Reading{
				Present:        true,
				State:          StateOnBattery,
				LowBattery:     true,
				ChargePercent:  90,
				RuntimeSeconds: -1,
			},
			criticalPercent: 20,
			expected: Decision{
				Event:         EventCriticalShutdown,
				BeginShutdown: true,
				Notify:        true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Decide(tt.prev, tt.cur, tt.criticalPercent)
			if result != tt.expected {
				t.Errorf("Decide(prev=%+v, cur=%+v, critical=%d) = %+v, want %+v", tt.prev, tt.cur, tt.criticalPercent, result, tt.expected)
			}
		})
	}
}
