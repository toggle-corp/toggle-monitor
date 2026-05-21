package alert

import "time"

// SSLStatus classifies a monitor's TLS-cert health. Independent from
// the uptime Status — a monitor can be up AND ssl-expiring at the
// same time.
type SSLStatus string

const (
	SSLStatusOK        SSLStatus = "ok"
	SSLStatusExpiring  SSLStatus = "ssl-expiring"
	SSLStatusSkipped   SSLStatus = "ssl-skipped" // HTTP-only static monitors
)

// SSLCheck is the per-tick input to the SSL state machine. ExpiresAt
// is zero when there's no cert (e.g. plain-HTTP probe).
type SSLCheck struct {
	At                    time.Time
	ExpiresAt             time.Time // cert NotAfter; zero → no cert observed this tick
	IsHTTPS               bool      // false → ssl-skipped (static HTTP monitors only)
	AlertThreshold        time.Duration
	EscalationThreshold   time.Duration
	ReminderInterval      time.Duration
}

// SSLState is the persisted SSL-side state.
type SSLState struct {
	Status         SSLStatus
	OpenedAt       time.Time
	LastReminderAt time.Time
}

// SSLEvent is appended to alert_events on every SSL-state-changing
// transition.
type SSLEvent struct {
	Type      EventType // EventSSLOpen, EventSSLReminder, EventSSLResolve
	At        time.Time
	ExpiresAt time.Time
}

const (
	EventSSLOpen     EventType = "ssl_open"
	EventSSLReminder EventType = "ssl_reminder"
	EventSSLResolve  EventType = "ssl_resolve"
)

// ApplySSL drives the SSL state machine. Behavior:
//
//   - !IsHTTPS                 → SSLStatusSkipped, no event.
//   - ExpiresAt zero (cert missing) → no transition (caller's job to
//     handle network-level failures via the uptime SM).
//   - TTL <= AlertThreshold and status was OK   → SSLStatusExpiring + EventSSLOpen.
//   - Already expiring: emit EventSSLReminder when the cadence elapsed.
//     Cadence = 24h once TTL <= EscalationThreshold; ReminderInterval
//     otherwise.
//   - TTL > AlertThreshold and status was Expiring → SSLStatusOK + EventSSLResolve
//     (cert renewed).
func ApplySSL(prev SSLState, c SSLCheck) (SSLState, *SSLEvent) {
	if !c.IsHTTPS {
		return SSLState{Status: SSLStatusSkipped}, nil
	}
	if c.ExpiresAt.IsZero() {
		// No cert info — couldn't observe; leave state untouched.
		return prev, nil
	}

	ttl := c.ExpiresAt.Sub(c.At)

	switch prev.Status {
	case "", SSLStatusOK, SSLStatusSkipped:
		if ttl <= c.AlertThreshold {
			return SSLState{
					Status:         SSLStatusExpiring,
					OpenedAt:       c.At,
					LastReminderAt: c.At,
				}, &SSLEvent{
					Type:      EventSSLOpen,
					At:        c.At,
					ExpiresAt: c.ExpiresAt,
				}
		}
		return SSLState{Status: SSLStatusOK}, nil

	case SSLStatusExpiring:
		if ttl > c.AlertThreshold {
			return SSLState{Status: SSLStatusOK}, &SSLEvent{
				Type:      EventSSLResolve,
				At:        c.At,
				ExpiresAt: c.ExpiresAt,
			}
		}
		// Still in the alert window — maybe time for a reminder.
		cadence := c.ReminderInterval
		if ttl <= c.EscalationThreshold {
			cadence = 24 * time.Hour
		}
		if cadence > 0 && c.At.Sub(prev.LastReminderAt) >= cadence {
			next := prev
			next.LastReminderAt = c.At
			return next, &SSLEvent{
				Type:      EventSSLReminder,
				At:        c.At,
				ExpiresAt: c.ExpiresAt,
			}
		}
		return prev, nil
	}
	return prev, nil
}
