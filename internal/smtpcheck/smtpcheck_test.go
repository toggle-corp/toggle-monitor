package smtpcheck_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/probe"
	"github.com/toggle-corp/toggle-monitor/internal/smtpcheck"
)

// fakeSMTP starts a one-shot TCP server on localhost that runs handle
// against the first accepted connection, and returns its host/port.
func fakeSMTP(t *testing.T, handle func(c net.Conn)) (string, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		handle(conn)
	}()
	addr := ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

func TestProbe_none_happyPath(t *testing.T) {
	host, port := fakeSMTP(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		_, _ = io.WriteString(c, "220 mail.example.test ESMTP\r\n")
		_, _ = br.ReadString('\n') // EHLO
		_, _ = io.WriteString(c, "250 mail.example.test\r\n")
		_, _ = br.ReadString('\n') // QUIT
		_, _ = io.WriteString(c, "221 Bye\r\n")
	})

	res := smtpcheck.Config{
		Host: host, Port: port, TLSMode: smtpcheck.TLSNone, Timeout: 2 * time.Second,
	}.Probe(context.Background())

	if res.Error != "" {
		t.Fatalf("unexpected error: %q", res.Error)
	}
	if res.Code != 250 {
		t.Errorf("code: got %d, want 250 (decisive EHLO code)", res.Code)
	}
	if res.TLS != nil {
		t.Errorf("plaintext probe should capture no cert, got %+v", res.TLS)
	}
}

func TestProbe_bannerFailure(t *testing.T) {
	host, port := fakeSMTP(t, func(c net.Conn) {
		// 421: service not available — a non-2xx greeting must fail the probe.
		_, _ = io.WriteString(c, "421 mail.example.test service not available\r\n")
	})

	res := smtpcheck.Config{
		Host: host, Port: port, TLSMode: smtpcheck.TLSNone, Timeout: 2 * time.Second,
	}.Probe(context.Background())

	if res.Error == "" {
		t.Fatal("expected a failure on a 421 banner")
	}
	if res.Code != 421 {
		t.Errorf("code: got %d, want 421 (decisive reply code preserved)", res.Code)
	}
}

func TestProbe_starttlsNotAdvertised(t *testing.T) {
	host, port := fakeSMTP(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		_, _ = io.WriteString(c, "220 mail.example.test ESMTP\r\n")
		_, _ = br.ReadString('\n') // EHLO
		// EHLO succeeds (250) but does NOT advertise STARTTLS.
		_, _ = io.WriteString(c, "250-mail.example.test\r\n250 PIPELINING\r\n")
		_, _ = br.ReadString('\n') // drain
	})

	res := smtpcheck.Config{
		Host: host, Port: port, TLSMode: smtpcheck.TLSStartTLS, Timeout: 2 * time.Second,
	}.Probe(context.Background())

	if res.Error == "" {
		t.Fatal("expected failure when STARTTLS requested but not advertised")
	}
	if res.Code != 250 {
		t.Errorf("code: got %d, want 250 (EHLO succeeded before STARTTLS check)", res.Code)
	}
}

func TestProbe_dialFailure(t *testing.T) {
	// Port 1 on localhost: connection refused, surfaced as a transport
	// failure with code 0.
	res := smtpcheck.Config{
		Host: "127.0.0.1", Port: 1, TLSMode: smtpcheck.TLSNone, Timeout: time.Second,
	}.Probe(context.Background())

	if res.Error == "" {
		t.Fatal("expected dial failure")
	}
	if res.Code != 0 {
		t.Errorf("code: got %d, want 0 for a transport failure", res.Code)
	}
	if res.FailKind != probe.FailKindDial {
		t.Errorf("FailKind: got %q, want %q", res.FailKind, probe.FailKindDial)
	}
}

// TestProbe_dnsFailure confirms an unresolvable host is classified
// probe.FailKindDNS from the real *net.DNSError chain — the signal the
// self-health detector (ADR-0008) keys off. .invalid never resolves.
func TestProbe_dnsFailure(t *testing.T) {
	res := smtpcheck.Config{
		Host: "nonexistent.invalid", Port: 25, TLSMode: smtpcheck.TLSNone, Timeout: 2 * time.Second,
	}.Probe(context.Background())

	if res.Error == "" {
		t.Fatal("expected a DNS resolution failure")
	}
	if res.FailKind != probe.FailKindDNS {
		t.Errorf("FailKind: got %q, want %q", res.FailKind, probe.FailKindDNS)
	}
}

// TestProbe_replyCodeFailureIsHTTP confirms a decisive SMTP reply-code
// failure (421 banner) is probe.FailKindHTTP — an application-level
// answer, never DNS — so it does not trip degraded mode.
func TestProbe_replyCodeFailureIsHTTP(t *testing.T) {
	host, port := fakeSMTP(t, func(c net.Conn) {
		_, _ = io.WriteString(c, "421 mail.example.test service not available\r\n")
	})

	res := smtpcheck.Config{
		Host: host, Port: port, TLSMode: smtpcheck.TLSNone, Timeout: 2 * time.Second,
	}.Probe(context.Background())

	if res.FailKind != probe.FailKindHTTP {
		t.Errorf("FailKind: got %q, want %q", res.FailKind, probe.FailKindHTTP)
	}
}

// TestProbe_none_happyPathHasNoFailKind confirms a successful probe
// carries probe.FailKindNone.
func TestProbe_none_happyPathHasNoFailKind(t *testing.T) {
	host, port := fakeSMTP(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		_, _ = io.WriteString(c, "220 mail.example.test ESMTP\r\n")
		_, _ = br.ReadString('\n') // EHLO
		_, _ = io.WriteString(c, "250 mail.example.test\r\n")
		_, _ = br.ReadString('\n') // QUIT
		_, _ = io.WriteString(c, "221 bye\r\n")
	})

	res := smtpcheck.Config{
		Host: host, Port: port, TLSMode: smtpcheck.TLSNone, Timeout: 2 * time.Second,
	}.Probe(context.Background())

	if res.Error != "" {
		t.Fatalf("expected success, got error: %q", res.Error)
	}
	if res.FailKind != probe.FailKindNone {
		t.Errorf("FailKind: got %q, want none", res.FailKind)
	}
}
