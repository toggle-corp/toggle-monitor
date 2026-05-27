// Package smtpcheck performs a single SMTP probe per monitor: it
// confirms a server is answering and speaking SMTP, optionally
// negotiates TLS (STARTTLS or implicit) and captures the certificate
// so expiry feeds the shared SSL state machine. It does NOT
// authenticate or send mail — see the SMTP monitoring design (depth
// "c").
//
// The whole conversation (connect → 220 banner → EHLO → STARTTLS/TLS
// handshake → QUIT) runs under one overall deadline derived from the
// configured timeout. The decisive reply code persisted on success is
// the EHLO 250 (proof of a working SMTP server, not just an open
// socket); transport/TLS failures surface as code 0 with the reason in
// Error.
package smtpcheck

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"time"

	"golang.org/x/net/proxy"

	"github.com/toggle-corp/toggle-monitor/internal/probe"
)

// TLS mode constants — the allowed values of an SMTP monitor's `tls`
// field. Mirrored by the config validator.
const (
	TLSStartTLS = "starttls" // plaintext connect, then upgrade via STARTTLS
	TLSImplicit = "implicit" // TLS from the first byte (SMTPS, e.g. 465)
	TLSNone     = "none"     // plaintext only; no cert captured
)

// Config describes a single SMTP probe.
type Config struct {
	Host     string
	Port     int
	TLSMode  string // TLSStartTLS | TLSImplicit | TLSNone
	EHLOName string // hostname sent in EHLO/HELO; defaults to "toggle-monitor"
	Timeout  time.Duration
	// InsecureSkipVerify disables certificate verification on the TLS
	// handshake (self-signed / private-CA relays we trust by config).
	// No effect when TLSMode is none.
	InsecureSkipVerify bool
	// ProxyDialer routes the TCP connect through an outbound proxy
	// (currently SOCKS5). nil → direct dial.
	ProxyDialer proxy.Dialer
}

const defaultEHLOName = "toggle-monitor"

// Probe implements probe.Prober. It runs the SMTP conversation and
// returns the neutral probe.Result the scheduler consumes.
func (cfg Config) Probe(ctx context.Context) probe.Result {
	start := time.Now()
	res := cfg.run(ctx)
	res.Duration = time.Since(start)
	return res
}

func (cfg Config) run(ctx context.Context) probe.Result {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)

	dialCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	conn, err := cfg.dial(dialCtx, addr)
	if err != nil {
		return probe.Result{Error: fmt.Sprintf("dial %s: %v", addr, err)}
	}
	// One overall deadline covers the entire conversation start-to-finish.
	_ = conn.SetDeadline(deadline)

	ehloName := cfg.EHLOName
	if ehloName == "" {
		ehloName = defaultEHLOName
	}

	tlsConfig := &tls.Config{
		ServerName: cfg.Host,
		//nolint:gosec // per-monitor opt-in for self-signed / private-CA relays
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}

	// Implicit TLS: wrap the socket before the SMTP greeting is read.
	if cfg.TLSMode == TLSImplicit {
		tconn := tls.Client(conn, tlsConfig)
		if err := tconn.HandshakeContext(dialCtx); err != nil {
			_ = conn.Close()
			return probe.Result{Error: fmt.Sprintf("implicit tls handshake: %v", err)}
		}
		conn = tconn
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		// NewClient reads the 220 banner; a non-2xx greeting (e.g. 421
		// service not available) surfaces here as a textproto.Error.
		_ = conn.Close()
		return failure("read banner", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Hello(ehloName); err != nil {
		return failure("ehlo", err)
	}

	if cfg.TLSMode == TLSStartTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return probe.Result{Code: 250, Error: "server does not advertise STARTTLS"}
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return failure("starttls", err)
		}
	}

	res := probe.Result{Code: 250} // EHLO 250 — decisive success code
	if state, ok := client.TLSConnectionState(); ok && len(state.PeerCertificates) > 0 {
		cert := state.PeerCertificates[0]
		res.TLS = &probe.CertInfo{
			Subject:  cert.Subject.CommonName,
			Issuer:   cert.Issuer.CommonName,
			NotAfter: cert.NotAfter,
		}
	}

	// QUIT is best-effort: the probe has already succeeded, and a server
	// that drops the connection on QUIT shouldn't flip the monitor down.
	_ = client.Quit()
	return res
}

// dial opens the TCP connection, honoring the optional proxy dialer and
// preferring a context-aware dial when available so cancellation /
// deadline propagate.
func (cfg Config) dial(ctx context.Context, addr string) (net.Conn, error) {
	if cfg.ProxyDialer != nil {
		if cd, ok := cfg.ProxyDialer.(proxy.ContextDialer); ok {
			return cd.DialContext(ctx, "tcp", addr)
		}
		return cfg.ProxyDialer.Dial("tcp", addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// failure builds a probe.Result for an error at the named SMTP step.
// SMTP reply errors (textproto.Error) carry the decisive reply code;
// transport/TLS errors leave Code at 0 with the reason in Error.
func failure(step string, err error) probe.Result {
	var pe *textproto.Error
	if errors.As(err, &pe) {
		return probe.Result{Code: pe.Code, Error: fmt.Sprintf("%s: %v", step, err)}
	}
	return probe.Result{Error: fmt.Sprintf("%s: %v", step, err)}
}
