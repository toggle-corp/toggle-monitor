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

// FailKind classifies a probe failure by the layer of the network
// stack it occurred at, derived by the prober from the real error
// chain (errors.As on *net.DNSError and friends) — NOT by
// string-matching Result.Error. The self-health detector (ADR-0008)
// keys off FailKindDNS to distinguish "the monitor went blind" from
// "the target is genuinely down." A successful probe is FailKindNone.
type FailKind string

const (
	// FailKindNone is the zero value: no failure (the probe succeeded).
	FailKindNone FailKind = ""
	// FailKindDNS is a name-resolution failure (*net.DNSError). The
	// self-health detector treats a burst of these as "blind."
	FailKindDNS FailKind = "dns"
	// FailKindDial is a connect-level failure after resolution
	// succeeded (connection refused / dial timeout).
	FailKindDial FailKind = "dial"
	// FailKindTLS is a TLS-handshake failure (cert / protocol).
	FailKindTLS FailKind = "tls"
	// FailKindTimeout is a context deadline / i/o timeout that is not
	// classifiable as a more specific kind.
	FailKindTimeout FailKind = "timeout"
	// FailKindHTTP is an application-level failure: a decisive protocol
	// code was received (Code != 0) but it was not accepted. A real
	// service returning 5xx is FailKindHTTP, never FailKindDNS.
	FailKindHTTP FailKind = "http"
)

// Result is the normalized outcome of one probe tick. Outcome is
// derived by the caller from Error: empty Error == ok, non-empty ==
// fail. Code is the decisive protocol code (HTTP status, SMTP reply
// code), or 0 for a transport/TLS-level failure with the reason in
// Error. FailKind classifies a failure by network layer (see FailKind);
// it is FailKindNone on success.
type Result struct {
	Code     int
	Error    string
	FailKind FailKind
	Duration time.Duration
	TLS      *CertInfo
}

// Prober runs a single probe according to its own captured config and
// returns the normalized Result. Implementations must be safe to call
// repeatedly (the scheduler retries in-cycle).
type Prober interface {
	Probe(ctx context.Context) Result
}
