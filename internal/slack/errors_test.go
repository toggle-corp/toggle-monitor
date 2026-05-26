package slack

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
)

func TestClassifySlackCode(t *testing.T) {
	cases := []struct {
		code string
		want ErrorKind
	}{
		{"ratelimited", KindTransient},
		{"service_unavailable", KindTransient},
		{"internal_error", KindTransient},
		{"request_timeout", KindTransient},
		{"fatal_error", KindTransient},

		{"invalid_auth", KindPersistent},
		{"token_revoked", KindPersistent},
		{"account_inactive", KindPersistent},
		{"not_in_channel", KindPersistent},
		{"channel_not_found", KindPersistent},
		{"is_archived", KindPersistent},
		{"missing_scope", KindPersistent},
		{"no_permission", KindPersistent},
		{"team_added_to_org", KindPersistent},
		{"not_authed", KindPersistent},

		{"msg_too_long", KindPermanentBug},
		{"invalid_blocks", KindPermanentBug},
		{"invalid_arguments", KindPermanentBug},
		{"invalid_attachments", KindPermanentBug},

		// chat.delete idempotency carve-out: a retried delete may see
		// message_not_found because the first attempt succeeded.
		{"message_not_found", KindPersistent},

		{"", KindUnknown},
		{"some_unrecognised_code", KindUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			if got := classifySlackCode(tc.code); got != tc.want {
				t.Errorf("classifySlackCode(%q) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

func TestClassifyHTTPStatus(t *testing.T) {
	cases := []struct {
		status int
		want   ErrorKind
	}{
		{200, KindUnknown}, // not an error
		{400, KindPermanentBug},
		{401, KindPersistent},
		{403, KindPersistent},
		{404, KindPersistent}, // method gone → operator-actionable
		{429, KindTransient},
		{500, KindTransient},
		{502, KindTransient},
		{503, KindTransient},
		{504, KindTransient},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("http_%d", tc.status), func(t *testing.T) {
			if got := classifyHTTPStatus(tc.status); got != tc.want {
				t.Errorf("classifyHTTPStatus(%d) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestClassifyTransport_safeRetryable(t *testing.T) {
	// Errors we deem "provably didn't reach Slack" — safe to retry
	// without risking double-delivery. classifyTransport must mark
	// these as transient AND safeRetryable=true.
	cases := []struct {
		name string
		err  error
	}{
		{"dns_error", &net.DNSError{Err: "no such host", Name: "slack.com", IsNotFound: true}},
		{"dial_op_error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("i/o timeout")}},
		{"econnrefused", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, code, safe := classifyTransport(tc.err)
			if kind != KindTransient {
				t.Errorf("kind: got %v, want %v", kind, KindTransient)
			}
			if !safe {
				t.Errorf("safeRetryable: got false, want true")
			}
			if code == "" {
				t.Errorf("code: got empty, want non-empty label")
			}
		})
	}
}

func TestClassifyTransport_unsafeRetryable(t *testing.T) {
	// Mid-response or read errors that may have hit Slack already.
	// classifyTransport marks them transient but safeRetryable=false
	// so the retry loop drops them rather than risk a double-post.
	cases := []struct {
		name string
		err  error
	}{
		{"read_op_error", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")}},
		{"plain_eof", errors.New("unexpected EOF")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, safe := classifyTransport(tc.err)
			if safe {
				t.Errorf("safeRetryable: got true, want false")
			}
		})
	}
}

func TestSlackError_unwrapAndFormat(t *testing.T) {
	cause := errors.New("underlying")
	se := &SlackError{
		Method: "chat.postMessage",
		Code:   "invalid_auth",
		Kind:   KindPersistent,
		Cause:  cause,
	}
	if !errors.Is(se, cause) {
		t.Errorf("errors.Is should unwrap to cause")
	}
	if se.Error() == "" {
		t.Errorf("Error() should not be empty")
	}
}
