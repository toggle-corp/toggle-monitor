package slack

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"time"
)

// ErrorKind classifies a Slack failure for retry + logging routing.
type ErrorKind int

const (
	// KindUnknown is the zero value: not classified, or not an error.
	KindUnknown ErrorKind = iota
	// KindTransient is retryable. Eventual exhaustion → WARN, no Sentry.
	KindTransient
	// KindPersistent is operator-actionable (auth/config). No retry. ERROR → Sentry.
	KindPersistent
	// KindPermanentBug is our bug (malformed payload). No retry. ERROR → Sentry, tagged.
	KindPermanentBug
)

// String is for human-readable logs and test output.
func (k ErrorKind) String() string {
	switch k {
	case KindTransient:
		return "transient"
	case KindPersistent:
		return "persistent"
	case KindPermanentBug:
		return "permanent_bug"
	default:
		return "unknown"
	}
}

// SlackError is the typed error returned by every Client method. It
// carries everything callers + the slog/Sentry bridge need to route
// and surface the failure without re-parsing strings.
type SlackError struct {
	// Method is the Slack API method (e.g. "chat.postMessage").
	Method string
	// Code is the Slack error string (e.g. "invalid_auth") or a transport
	// label (e.g. "dial", "dns", "http_500"). Empty only for unknown.
	Code string
	// Kind is the routing bucket.
	Kind ErrorKind
	// HTTPStatus is the response status code, or 0 for transport errors.
	HTTPStatus int
	// RetryAfter is the parsed Retry-After header, or 0 if absent.
	RetryAfter time.Duration
	// Attempts is the number of attempts made before giving up (≥1).
	Attempts int
	// Elapsed is the wall time spent including all retries.
	Elapsed time.Duration
	// ExhaustedRetries is true iff the retry loop tried at least once
	// after the initial attempt and still gave up.
	ExhaustedRetries bool
	// Cause is the wrapped underlying error.
	Cause error

	// transportSafeRetry is the unexported "request provably didn't
	// reach Slack" marker. The retry loop consults it; callers never
	// need to see it.
	transportSafeRetry bool
}

// Error renders a human-readable form that includes the slack code so
// log lines and existing string-matching tests stay legible.
func (e *SlackError) Error() string {
	if e == nil {
		return ""
	}
	prefix := "slack"
	if e.Method != "" {
		prefix = "slack " + e.Method
	}
	if e.Code != "" {
		if e.Cause != nil {
			return fmt.Sprintf("%s: %s: %v", prefix, e.Code, e.Cause)
		}
		return fmt.Sprintf("%s: %s", prefix, e.Code)
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", prefix, e.Cause)
	}
	return prefix + ": unknown error"
}

// Unwrap exposes the cause so errors.Is / errors.As can reach into
// the underlying transport error.
func (e *SlackError) Unwrap() error { return e.Cause }

// Retryable reports whether this error is in a class the client may
// retry. Persistent and permanent-bug errors are never retried.
func (e *SlackError) Retryable() bool { return e != nil && e.Kind == KindTransient }

// classifySlackCode buckets a Slack-level "error" string. Empty/unknown
// codes map to KindUnknown so the caller decides how to handle them
// (typically: treat unknown ok=false as persistent so we don't spin on
// retries against an error we don't recognise).
func classifySlackCode(code string) ErrorKind {
	switch code {
	// Retryable: server-side hiccups, rate limits, transient server errors.
	case "ratelimited",
		"service_unavailable",
		"internal_error",
		"request_timeout",
		"fatal_error":
		return KindTransient

	// Operator-actionable: bot identity, channel membership, scope.
	case "invalid_auth",
		"token_revoked",
		"token_expired",
		"account_inactive",
		"not_authed",
		"not_in_channel",
		"channel_not_found",
		"is_archived",
		"missing_scope",
		"no_permission",
		"team_added_to_org",
		"user_not_found",
		"user_not_visible",
		// For chat.delete: a retried delete after an uncertain success
		// may see message_not_found. We treat it as persistent — the
		// classifier doesn't know which method called it, so the
		// chat.delete carve-out is applied at the call site.
		"message_not_found",
		"cant_delete_message",
		"compliance_exports_prevent_deletion":
		return KindPersistent

	// Our payload bug: malformed Block Kit, oversized message, bad args.
	case "msg_too_long",
		"invalid_blocks",
		"invalid_blocks_format",
		"invalid_arguments",
		"invalid_argument_name",
		"invalid_attachments",
		"too_many_attachments",
		"too_many_contexts":
		return KindPermanentBug
	}
	return KindUnknown
}

// classifyHTTPStatus buckets a non-2xx HTTP status. 2xx returns
// KindUnknown (the call succeeded transport-wise; classify the slack
// payload instead).
func classifyHTTPStatus(status int) ErrorKind {
	switch {
	case status >= 200 && status < 300:
		return KindUnknown
	case status == 400:
		return KindPermanentBug
	case status == 401 || status == 403 || status == 404:
		return KindPersistent
	case status == 429:
		return KindTransient
	case status >= 500:
		return KindTransient
	}
	return KindUnknown
}

// classifyTransport buckets a transport-level error from
// http.Client.Do. Returns (kind, code-label, safeRetryable) where
// safeRetryable is true only for errors the request provably didn't
// reach Slack — DNS lookup, dial timeout, connection refused. Errors
// that may have hit Slack mid-flight (read/EOF after request was
// sent) are marked transient but unsafe so the retry loop can drop
// them to avoid double-delivery.
func classifyTransport(err error) (kind ErrorKind, code string, safeRetryable bool) {
	if err == nil {
		return KindUnknown, "", false
	}

	// DNS lookup failed — the request never reached the network.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return KindTransient, "dns", true
	}

	// net.OpError: distinguish dial-time (safe) from read-time (unsafe).
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		switch opErr.Op {
		case "dial":
			// Includes TCP connect timeouts, connection refused, no
			// route to host. None of these reached Slack.
			return KindTransient, "dial", true
		case "read", "write":
			// May have sent the request already. Mark transient but
			// not safely retryable.
			return KindTransient, opErr.Op, false
		}
	}

	// syscall-level connection refused / no-route, in case it didn't
	// come through an OpError wrap.
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EHOSTUNREACH) {
		return KindTransient, "dial", true
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return KindTransient, "reset", false
	}

	// Generic unwrap on io.EOF / "unexpected EOF" — uncertain delivery.
	if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "EOF") {
		return KindTransient, "eof", false
	}

	// Unknown transport error: treat as transient-unsafe so we surface
	// it but don't retry blindly.
	return KindTransient, "transport", false
}
