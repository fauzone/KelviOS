package ups

import "strconv"

const DefaultCriticalPercent = 20

type PowerState string

const (
	StateUnknown   PowerState = "unknown"
	StateOnline    PowerState = "online"
	StateOnBattery PowerState = "on_battery"
)

type Reading struct {
	Present        bool       `json:"present"`
	State          PowerState `json:"state"`
	LowBattery     bool       `json:"low_battery"`
	ChargePercent  int        `json:"charge_percent"`  // -1 = unknown
	RuntimeSeconds int        `json:"runtime_seconds"` // -1 = unknown
}

func ParseVars(vars map[string]string) Reading {
	if len(vars) == 0 {
		return Reading{
			Present:        false,
			State:          StateUnknown,
			ChargePercent:  -1,
			RuntimeSeconds: -1,
		}
	}

	status, ok := vars["ups.status"]
	if !ok || status == "" {
		return Reading{
			Present:        false,
			State:          StateUnknown,
			ChargePercent:  -1,
			RuntimeSeconds: -1,
		}
	}

	reading := Reading{
		Present:        true,
		State:          StateUnknown,
		LowBattery:     false,
		ChargePercent:  -1,
		RuntimeSeconds: -1,
	}

	tokens := splitStatus(status)
	for _, token := range tokens {
		switch token {
		case "OB":
			reading.State = StateOnBattery
		case "OL":
			if reading.State != StateOnBattery {
				reading.State = StateOnline
			}
		case "LB", "FSD":
			reading.LowBattery = true
		}
	}

	if chargeStr, ok := vars["battery.charge"]; ok {
		reading.ChargePercent = parseCharge(chargeStr)
	}

	if runtimeStr, ok := vars["battery.runtime"]; ok {
		reading.RuntimeSeconds = parseRuntime(runtimeStr)
	}

	return reading
}

func splitStatus(status string) []string {
	var tokens []string
	current := ""
	for _, r := range status {
		if r == ' ' || r == '\t' {
			if current != "" {
				tokens = append(tokens, current)
				current = ""
			}
		} else {
			current += string(r)
		}
	}
	if current != "" {
		tokens = append(tokens, current)
	}
	return tokens
}

func parseCharge(s string) int {
	var f float64
	n, err := parseFloat(s, &f)
	if err != nil || n == 0 || f < 0 {
		return -1
	}
	if f > 100 {
		return 100
	}
	return int(f)
}

func parseRuntime(s string) int {
	var f float64
	n, err := parseFloat(s, &f)
	if err != nil || n == 0 || f < 0 {
		return -1
	}
	return int(f)
}

func parseFloat(s string, out *float64) (int, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	*out = f
	return 1, nil
}

type EventKind string

const (
	EventOnBattery        EventKind = "on_battery"
	EventOnlineRestored   EventKind = "online_restored"
	EventCriticalShutdown EventKind = "critical_shutdown"
	EventUncleanShutdown  EventKind = "unclean_shutdown"
)

type Decision struct {
	Event         EventKind `json:"event"` // "" = no event
	BeginShutdown bool      `json:"begin_shutdown"`
	Notify        bool      `json:"notify"`
}

func Decide(prev, cur Reading, criticalPercent int) Decision {
	if criticalPercent <= 0 {
		criticalPercent = DefaultCriticalPercent
	}

	if !cur.Present || cur.State == StateUnknown {
		return Decision{}
	}

	prevState := prev.State
	if !prev.Present {
		prevState = StateUnknown
	}

	if cur.State == StateOnBattery && (cur.LowBattery || (cur.ChargePercent >= 0 && cur.ChargePercent <= criticalPercent)) {
		return Decision{
			Event:         EventCriticalShutdown,
			BeginShutdown: true,
			Notify:        true,
		}
	}

	if prevState != StateOnBattery && cur.State == StateOnBattery {
		return Decision{
			Event:  EventOnBattery,
			Notify: true,
		}
	}

	if prevState == StateOnBattery && cur.State == StateOnline {
		return Decision{
			Event:  EventOnlineRestored,
			Notify: true,
		}
	}

	return Decision{}
}
