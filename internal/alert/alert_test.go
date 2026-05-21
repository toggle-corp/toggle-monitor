package alert_test

import (
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/alert"
)

var t0 = time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)

func TestApply_upAndOk_staysUpNoEvent(t *testing.T) {
	prev := alert.State{Status: alert.StatusUp}
	next, ev := alert.Apply(prev, alert.Check{Outcome: alert.OutcomeOK, At: t0})

	if next.Status != alert.StatusUp {
		t.Errorf("status: got %q, want %q", next.Status, alert.StatusUp)
	}
	if ev != nil {
		t.Errorf("expected no event, got %+v", ev)
	}
}

func TestApply_downAndOk_transitionsToUpWithResolveEvent(t *testing.T) {
	prev := alert.State{Status: alert.StatusDown, OpenedAt: t0}
	resolveAt := t0.Add(45 * time.Minute)

	next, ev := alert.Apply(prev, alert.Check{Outcome: alert.OutcomeOK, At: resolveAt})

	if next.Status != alert.StatusUp {
		t.Errorf("status: got %q, want %q", next.Status, alert.StatusUp)
	}
	if !next.OpenedAt.IsZero() {
		t.Errorf("OpenedAt: got %v, want zero", next.OpenedAt)
	}
	if ev == nil {
		t.Fatal("expected resolve event, got nil")
	}
	if ev.Type != alert.EventResolve {
		t.Errorf("event type: got %q, want %q", ev.Type, alert.EventResolve)
	}
	if want := 45 * time.Minute; ev.Downtime != want {
		t.Errorf("downtime: got %v, want %v", ev.Downtime, want)
	}
	if !ev.At.Equal(resolveAt) {
		t.Errorf("event At: got %v, want %v", ev.At, resolveAt)
	}
}

func TestApply_downAndFail_emitsReminderAfterInterval(t *testing.T) {
	prev := alert.State{Status: alert.StatusDown, OpenedAt: t0, LastReminderAt: t0}
	tickAt := t0.Add(3*24*time.Hour + time.Second)

	next, ev := alert.Apply(prev, alert.Check{
		Outcome:          alert.OutcomeFail,
		At:               tickAt,
		StatusCode:       503,
		Error:            "still down",
		ReminderInterval: 3 * 24 * time.Hour,
	})

	if next.Status != alert.StatusDown {
		t.Errorf("status: got %q, want %q", next.Status, alert.StatusDown)
	}
	if !next.OpenedAt.Equal(t0) {
		t.Errorf("OpenedAt: got %v, want unchanged %v", next.OpenedAt, t0)
	}
	if !next.LastReminderAt.Equal(tickAt) {
		t.Errorf("LastReminderAt: got %v, want %v", next.LastReminderAt, tickAt)
	}
	if ev == nil {
		t.Fatal("expected a reminder event, got nil")
	}
	if ev.Type != alert.EventReminder {
		t.Errorf("event type: got %q, want %q", ev.Type, alert.EventReminder)
	}
}

func TestApply_downAndFail_noReminderBeforeInterval(t *testing.T) {
	prev := alert.State{Status: alert.StatusDown, OpenedAt: t0, LastReminderAt: t0}
	tickAt := t0.Add(2 * 24 * time.Hour) // one day shy of 3d

	next, ev := alert.Apply(prev, alert.Check{
		Outcome:          alert.OutcomeFail,
		At:               tickAt,
		ReminderInterval: 3 * 24 * time.Hour,
	})

	if ev != nil {
		t.Errorf("expected no event before reminder interval, got %+v", ev)
	}
	if !next.LastReminderAt.Equal(t0) {
		t.Errorf("LastReminderAt: got %v, want unchanged %v", next.LastReminderAt, t0)
	}
}

func TestApply_upAndFail_initializesLastReminderAt(t *testing.T) {
	prev := alert.State{Status: alert.StatusUp}
	next, _ := alert.Apply(prev, alert.Check{
		Outcome:          alert.OutcomeFail,
		At:               t0,
		ReminderInterval: 3 * 24 * time.Hour,
	})
	if !next.LastReminderAt.Equal(t0) {
		t.Errorf("LastReminderAt: got %v, want %v (initialized to OpenedAt)", next.LastReminderAt, t0)
	}
}

func TestApply_downAndFail_staysDownNoEvent(t *testing.T) {
	prev := alert.State{Status: alert.StatusDown, OpenedAt: t0}
	later := t0.Add(2 * time.Minute)

	next, ev := alert.Apply(prev, alert.Check{
		Outcome:    alert.OutcomeFail,
		At:         later,
		StatusCode: 503,
		Error:      "Service Unavailable",
	})

	if next.Status != alert.StatusDown {
		t.Errorf("status: got %q, want %q", next.Status, alert.StatusDown)
	}
	if !next.OpenedAt.Equal(t0) {
		t.Errorf("OpenedAt: got %v, want unchanged %v", next.OpenedAt, t0)
	}
	if ev != nil {
		t.Errorf("expected no event on down→down, got %+v", ev)
	}
}

func TestApply_upAndFail_transitionsToDownWithOpenEvent(t *testing.T) {
	prev := alert.State{Status: alert.StatusUp}
	check := alert.Check{
		Outcome:    alert.OutcomeFail,
		At:         t0,
		StatusCode: 500,
		Error:      "Internal Server Error",
	}

	next, ev := alert.Apply(prev, check)

	if next.Status != alert.StatusDown {
		t.Errorf("status: got %q, want %q", next.Status, alert.StatusDown)
	}
	if !next.OpenedAt.Equal(t0) {
		t.Errorf("OpenedAt: got %v, want %v", next.OpenedAt, t0)
	}
	if ev == nil {
		t.Fatal("expected an open event, got nil")
	}
	if ev.Type != alert.EventOpen {
		t.Errorf("event type: got %q, want %q", ev.Type, alert.EventOpen)
	}
	if ev.StatusCode != 500 {
		t.Errorf("event status code: got %d, want 500", ev.StatusCode)
	}
	if ev.Error != "Internal Server Error" {
		t.Errorf("event error: got %q, want %q", ev.Error, "Internal Server Error")
	}
	if !ev.At.Equal(t0) {
		t.Errorf("event At: got %v, want %v", ev.At, t0)
	}
}
