// Package probe defines the neutral result + prober seam shared by
// every probe kind (HTTP, SMTP, …). The scheduler drives a Prober and
// feeds the resulting Result into the uptime + SSL state machines
// without knowing which protocol produced it. Each concrete probe
// package (httpcheck, smtpcheck) implements Prober and adapts its own
// richer result into this shape.
package probe

import (
	"context"
	"time"
)

// CertInfo is the slim TLS-cert info the SSL state machine consumes.
// Nil on a Result means no certificate was observed this tick (plain
// probe, or a failure before the handshake completed).
type CertInfo struct {
	Subject  string
	Issuer   string
	NotAfter time.Time
}

// Result is the normalized outcome of one probe tick. Outcome is
// derived by the caller from Error: empty Error == ok, non-empty ==
// fail. Code is the decisive protocol code (HTTP status, SMTP reply
// code), or 0 for a transport/TLS-level failure with the reason in
// Error.
type Result struct {
	Code     int
	Error    string
	Duration time.Duration
	TLS      *CertInfo
}

// Prober runs a single probe according to its own captured config and
// returns the normalized Result. Implementations must be safe to call
// repeatedly (the scheduler retries in-cycle).
type Prober interface {
	Probe(ctx context.Context) Result
}
