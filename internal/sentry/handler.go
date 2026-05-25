package sentry

import (
	"context"
	"errors"
	"log/slog"

	sentrygo "github.com/getsentry/sentry-go"
)

// slogHandler forwards records at slog.LevelError (or higher) to
// Sentry. WARN/INFO/DEBUG short-circuit in Enabled so callers pay no
// per-record cost. Attrs accumulated via WithAttrs / WithGroup are
// preserved across handler clones and prepended to per-record attrs.
type slogHandler struct {
	attrs []slog.Attr
	group string
}

// Enabled gates the bridge so non-ERROR records never touch the
// Sentry path.
func (h *slogHandler) Enabled(_ context.Context, lvl slog.Level) bool {
	return lvl >= slog.LevelError
}

// Handle converts the record into a sentry.Event and ships it via a
// cloned hub (so concurrent calls don't share scope state).
//
// Two-shape rule:
//   - If any attr's value is a Go error, we build an Exception (carries
//     stacktrace + drives Sentry's exception-grouping fingerprint).
//   - Otherwise we emit a plain message-shape event.
//
// Attrs are mapped as:
//   - key == "monitor" → Sentry tag `monitor=<value>`.
//   - error-valued attrs → consumed into the Exception; not duplicated
//     into Extra.
//   - everything else → Extra (string-keyed map).
func (h *slogHandler) Handle(_ context.Context, r slog.Record) error {
	hub := sentrygo.CurrentHub().Clone()
	event := sentrygo.NewEvent()
	event.Level = sentrygo.LevelError
	event.Message = r.Message
	event.Timestamp = r.Time

	scope := hub.Scope()
	extras := map[string]interface{}{}
	apply := func(a slog.Attr) {
		switch v := a.Value.Any().(type) {
		case error:
			// Intentionally no Stacktrace: a stack captured here points
			// at the slog handler internals, not the log.Error call
			// site, which is misleading. Panic events go through
			// hub.Recover and get a real stack there.
			event.Exception = append(event.Exception, sentrygo.Exception{
				Type:  a.Key,
				Value: v.Error(),
			})
		default:
			if a.Key == "monitor" {
				scope.SetTag("monitor", a.Value.String())
				return
			}
			extras[a.Key] = v
		}
	}
	for _, a := range h.attrs {
		apply(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		apply(a)
		return true
	})

	if len(extras) > 0 {
		// sentry-go v0.46 dropped event.Extra; "slog" appears as a
		// dedicated context section in the Sentry UI.
		if event.Contexts == nil {
			event.Contexts = map[string]sentrygo.Context{}
		}
		event.Contexts["slog"] = extras
	}

	hub.CaptureEvent(event)
	return nil
}

// WithAttrs returns a new handler with the additional attrs merged
// in. Implements the standard slog.Handler contract.
func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &slogHandler{attrs: merged, group: h.group}
}

// WithGroup is required by slog.Handler. We don't currently encode
// the group anywhere — Sentry doesn't have a direct analog — but the
// method must be implemented or callers will silently drop attrs.
func (h *slogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := *h
	clone.group = name
	return &clone
}

// MultiHandler dispatches every record to each child handler. Stdlib
// slog doesn't ship a multi-handler; lifecycle uses this one to send
// records to both the JSON stdout handler and the Sentry bridge.
//
// Each handler is queried independently via Enabled, so the Sentry
// bridge can short-circuit non-ERROR records while the stdout
// handler still emits them.
type MultiHandler struct {
	handlers []slog.Handler
}

// NewMultiHandler returns a MultiHandler dispatching to the given
// children. nil entries are dropped.
func NewMultiHandler(handlers ...slog.Handler) *MultiHandler {
	out := make([]slog.Handler, 0, len(handlers))
	for _, h := range handlers {
		if h != nil {
			out = append(out, h)
		}
	}
	return &MultiHandler{handlers: out}
}

// Enabled returns true if any child is enabled at lvl.
func (m *MultiHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, lvl) {
			return true
		}
	}
	return false
}

// Handle dispatches to every child that reports Enabled. Errors are
// joined so a faulty child can't suppress the others.
func (m *MultiHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range m.handlers {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errors.Join(errs...)
}

// WithAttrs applies the attrs to every child.
func (m *MultiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	children := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		children[i] = h.WithAttrs(attrs)
	}
	return &MultiHandler{handlers: children}
}

// WithGroup applies the group to every child.
func (m *MultiHandler) WithGroup(name string) slog.Handler {
	children := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		children[i] = h.WithGroup(name)
	}
	return &MultiHandler{handlers: children}
}
