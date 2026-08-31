//go:build integration

package lifecycle_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDNS is a minimal UDP DNS server used to simulate an internet
// outage end to end. While "up" it answers every A query with
// 127.0.0.1; while "down" it answers SERVFAIL, which Go's resolver
// surfaces as a real *net.DNSError — the exact error class the probers
// classify as probe.FailKindDNS. Flipping one boolean therefore
// reproduces the production failure mode (every outbound name lookup
// fails, Slack's included) without touching the machine's real
// resolver config.
type fakeDNS struct {
	conn *net.UDPConn
	// up gates the zone: true answers, false SERVFAILs.
	up      atomic.Bool
	queries atomic.Int64
}

// startFakeDNS binds a UDP listener on loopback, starts serving, and
// installs it as the process-wide resolver for the duration of the
// test. The previous net.DefaultResolver is restored on cleanup.
func startFakeDNS(t *testing.T) *fakeDNS {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("bind fake dns: %v", err)
	}
	f := &fakeDNS{conn: conn}
	f.up.Store(true)

	done := make(chan struct{})
	go f.serve(done)

	prev := net.DefaultResolver
	addr := conn.LocalAddr().String()
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", addr)
		},
	}
	t.Cleanup(func() {
		net.DefaultResolver = prev
		_ = conn.Close()
		<-done
	})
	return f
}

// setUp flips the simulated internet on (true) or off (false).
func (f *fakeDNS) setUp(up bool) { f.up.Store(up) }

func (f *fakeDNS) serve(done chan struct{}) {
	defer close(done)
	buf := make([]byte, 512)
	for {
		n, from, err := f.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		resp, ok := f.respond(buf[:n])
		if !ok {
			continue
		}
		_, _ = f.conn.WriteToUDP(resp, from)
	}
}

// respond builds the reply for one query. Returns ok=false for a
// packet it cannot parse (dropped, as a real server would).
func (f *fakeDNS) respond(q []byte) ([]byte, bool) {
	if len(q) < 12 {
		return nil, false
	}
	// Walk the QNAME labels to find the end of the question section.
	off := 12
	for off < len(q) {
		l := int(q[off])
		if l == 0 {
			off++
			break
		}
		if l&0xC0 != 0 { // compression pointer in a question: unexpected
			return nil, false
		}
		off += l + 1
	}
	if off+4 > len(q) {
		return nil, false
	}
	qtype := binary.BigEndian.Uint16(q[off : off+2])
	off += 4 // qtype + qclass
	question := q[12:off]
	f.queries.Add(1)

	// Only A is answered. An AAAA query gets NOERROR with no answers,
	// which is what makes Go's resolver settle on the IPv4 address.
	const typeA = 1
	up := f.up.Load()

	resp := make([]byte, 0, 64)
	resp = append(resp, q[0], q[1]) // ID
	flags := uint16(0x8180)         // QR|RD|RA, NOERROR
	if !up {
		flags = 0x8182 // SERVFAIL
	}
	var answers uint16
	if up && qtype == typeA {
		answers = 1
	}
	resp = binary.BigEndian.AppendUint16(resp, flags)
	resp = binary.BigEndian.AppendUint16(resp, 1) // QDCOUNT
	resp = binary.BigEndian.AppendUint16(resp, answers)
	resp = binary.BigEndian.AppendUint16(resp, 0) // NSCOUNT
	resp = binary.BigEndian.AppendUint16(resp, 0) // ARCOUNT
	resp = append(resp, question...)
	if answers == 1 {
		resp = append(resp, 0xC0, 0x0C)                   // NAME → pointer to the question
		resp = binary.BigEndian.AppendUint16(resp, typeA) // TYPE A
		resp = binary.BigEndian.AppendUint16(resp, 1)     // CLASS IN
		resp = binary.BigEndian.AppendUint32(resp, 1)     // TTL 1s — never cache across the gate flip
		resp = binary.BigEndian.AppendUint16(resp, 4)     // RDLENGTH
		resp = append(resp, 127, 0, 0, 1)
	}
	return resp, true
}

// TestFakeDNS_gateProducesRealDNSErrors is the self-check for the
// harness: with the gate open a name in the zone resolves to loopback;
// with it closed the very same lookup fails as a *net.DNSError, which
// is what the probers classify as probe.FailKindDNS.
func TestFakeDNS_gateProducesRealDNSErrors(t *testing.T) {
	dns := startFakeDNS(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupHost(ctx, "svc1.outage.test")
	if err != nil {
		t.Fatalf("lookup while up: %v", err)
	}
	if len(addrs) == 0 || addrs[0] != "127.0.0.1" {
		t.Fatalf("lookup while up: got %v, want [127.0.0.1]", addrs)
	}

	dns.setUp(false)
	_, err = net.DefaultResolver.LookupHost(ctx, "svc2.outage.test")
	if err == nil {
		t.Fatal("lookup while down: expected an error, got none")
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		t.Fatalf("lookup while down: got %T (%v), want *net.DNSError", err, err)
	}
}
