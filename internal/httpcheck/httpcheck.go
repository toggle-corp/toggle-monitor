// Package httpcheck performs a single HTTP probe per monitor
// configuration and returns a typed result.
package httpcheck

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/proxy"

	"github.com/toggle-corp/toggle-monitor/internal/probe"
)

// Config describes a single check probe.
type Config struct {
	URL                 string
	Method              string
	AcceptedStatusCodes []int
	Timeout             time.Duration
	FollowRedirects     bool
	// TLSInsecureSkipVerify disables certificate verification on
	// HTTPS probes (self-signed / private-CA endpoints we trust by
	// configuration). Has no effect on plain HTTP URLs.
	TLSInsecureSkipVerify bool
	// ProxyDialer routes the probe through an outbound proxy
	// (currently SOCKS5). nil → direct dial.
	ProxyDialer proxy.Dialer
	UserAgent   string
}

// Result is the outcome of one probe.
type Result struct {
	StatusCode   int
	Duration     time.Duration
	Error        string // empty iff the request returned a status code in AcceptedStatusCodes
	ResponseBody []byte

	// FailKind classifies a failure by network layer, derived from the
	// real error chain (never string-matched). probe.FailKindNone on
	// success. See ADR-0008.
	FailKind probe.FailKind

	// TLS holds certificate info captured from the response's
	// TLS handshake. Nil for plain HTTP probes and for probes that
	// failed before the handshake completed.
	TLS *TLSInfo
}

// TLSInfo is the slim cert info the SSL state machine consumes.
type TLSInfo struct {
	Subject  string
	Issuer   string
	NotAfter time.Time
}

// Probe implements probe.Prober: it runs Check and adapts the
// HTTP-specific Result into the neutral probe.Result the scheduler
// consumes. Carrying the method on Config lets a Config value be stored
// directly as a Plan's prober.
func (cfg Config) Probe(ctx context.Context) probe.Result {
	res := Check(ctx, cfg)
	out := probe.Result{
		Code:     res.StatusCode,
		Error:    res.Error,
		FailKind: res.FailKind,
		Duration: res.Duration,
	}
	if res.TLS != nil {
		out.TLS = &probe.CertInfo{
			Subject:  res.TLS.Subject,
			Issuer:   res.TLS.Issuer,
			NotAfter: res.TLS.NotAfter,
		}
	}
	return out
}

// Check performs one HTTP probe according to cfg. The caller is
// responsible for in-cycle retries (the scheduler collapses retries
// into a single tick before handing the final Result to the alert
// state machine).
func Check(ctx context.Context, cfg Config) Result {
	client := &http.Client{Timeout: cfg.Timeout}
	if cfg.TLSInsecureSkipVerify || cfg.ProxyDialer != nil {
		// Clone the default transport so connection pooling and
		// timeouts stay at Go's defaults; only override what we need.
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if cfg.TLSInsecureSkipVerify {
			//nolint:gosec // per-monitor opt-in for self-signed certs
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
		if cfg.ProxyDialer != nil {
			// Prefer the context-aware Dial when the underlying
			// proxy.Dialer implements it (SOCKS5 from x/net/proxy
			// does). Falls back to the legacy Dial otherwise — that
			// loses cancel propagation but keeps the probe working.
			if cd, ok := cfg.ProxyDialer.(proxy.ContextDialer); ok {
				tr.DialContext = cd.DialContext
			} else {
				dialer := cfg.ProxyDialer
				tr.DialContext = func(_ context.Context, network, addr string) (net.Conn, error) {
					return dialer.Dial(network, addr)
				}
			}
		}
		client.Transport = tr
	}
	if !cfg.FollowRedirects {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	req, err := http.NewRequestWithContext(ctx, cfg.Method, cfg.URL, nil)
	if err != nil {
		return Result{Error: fmt.Sprintf("invalid request: %v", err)}
	}
	if cfg.UserAgent != "" {
		req.Header.Set("User-Agent", cfg.UserAgent)
	}

	start := time.Now()
	resp, err := client.Do(req)
	dur := time.Since(start)
	if err != nil {
		return Result{Error: err.Error(), FailKind: classify(err), Duration: dur}
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	res := Result{
		StatusCode:   resp.StatusCode,
		Duration:     dur,
		ResponseBody: body,
	}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		cert := resp.TLS.PeerCertificates[0]
		res.TLS = &TLSInfo{
			Subject:  cert.Subject.CommonName,
			Issuer:   cert.Issuer.CommonName,
			NotAfter: cert.NotAfter,
		}
	}
	if !accepted(resp.StatusCode, cfg.AcceptedStatusCodes) {
		res.Error = fmt.Sprintf("unexpected status %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		// A decisive status code was received — an application-level
		// failure, never a transport/DNS one (ADR-0008).
		res.FailKind = probe.FailKindHTTP
	}
	return res
}

// classify maps a transport-level error from client.Do into a
// probe.FailKind by unwrapping the real error chain (errors.As on
// *net.DNSError / *tls.*), never by string-matching. Order matters:
// a DNS lookup that timed out is wrapped in a *net.DNSError, so the
// DNS check runs before the generic timeout check. Anything after a
// completed resolution that is a connect/timeout is FailKindDial or
// FailKindTimeout; a TLS-handshake failure is FailKindTLS.
func classify(err error) probe.FailKind {
	if err == nil {
		return probe.FailKindNone
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return probe.FailKindDNS
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return probe.FailKindTLS
	}
	var recordErr *tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return probe.FailKindTLS
	}
	var alertErr tls.AlertError
	if errors.As(err, &alertErr) {
		return probe.FailKindTLS
	}
	// context deadline / i/o timeout after resolution succeeded.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return probe.FailKindTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return probe.FailKindTimeout
	}
	// Anything left — a connect-level failure (connection refused /
	// reset) after resolution, or an unrecognized transport error — is
	// dial-class. This is the safe default for ADR-0008: only FailKindDNS
	// trips degraded mode, so a misclassification here never over-trips.
	return probe.FailKindDial
}

func accepted(code int, list []int) bool {
	for _, c := range list {
		if c == code {
			return true
		}
	}
	return false
}
