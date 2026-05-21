// Package alert holds the monitor state machine: given the previous
// status and a check result, it emits transitions and alert events.
package alert

import "time"

// Status is the persistent classification of a monitor. Only up and
// down are exercised in Issue 2; the additional statuses
// (temporary-paused, kube-paused, kube-invalid, ssl-expiring,
// ssl-skipped) land with later issues.
type Status string

const (
	StatusUp   Status = "up"
	StatusDown Status = "down"
)

// Outcome is the result of a single check tick (after in-cycle retries
// have been collapsed by the scheduler).
type Outcome string

const (
	OutcomeOK   Outcome = "ok"
	OutcomeFail Outcome = "fail"
)

// Check is the input to the state machine: the result of a single tick.
type Check struct {
	Outcome    Outcome
	At         time.Time
	StatusCode int    // 0 if not applicable (e.g., transport error / timeout)
	Error      string // human-readable summary; empty when OutcomeOK
}

// State carries everything the SM needs to know about a monitor between
// ticks. OpenedAt is the moment the current `down` incident began
// (zero when StatusUp).
type State struct {
	Status   Status
	OpenedAt time.Time
}

// EventType classifies an alert event for persistence.
type EventType string

const (
	EventOpen    EventType = "open"    // transition up → down
	EventResolve EventType = "resolve" // transition down → up
)

// Event is appended to alert_events on every state-changing tick.
type Event struct {
	Type       EventType
	At         time.Time
	StatusCode int
	Error      string
	// Downtime is the duration of the just-resolved incident. Set only
	// when Type == EventResolve.
	Downtime time.Duration
}

// Apply computes the next state from the previous state and the latest
// check. The returned *Event is non-nil only when a state-changing
// transition occurred (open or resolve).
func Apply(prev State, c Check) (State, *Event) {
	switch {
	case prev.Status == StatusUp && c.Outcome == OutcomeFail:
		return State{Status: StatusDown, OpenedAt: c.At}, &Event{
			Type:       EventOpen,
			At:         c.At,
			StatusCode: c.StatusCode,
			Error:      c.Error,
		}
	case prev.Status == StatusDown && c.Outcome == OutcomeOK:
		return State{Status: StatusUp}, &Event{
			Type:     EventResolve,
			At:       c.At,
			Downtime: c.At.Sub(prev.OpenedAt),
		}
	}
	return prev, nil
}
