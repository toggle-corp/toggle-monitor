// Package pingcheck performs a single ICMP echo probe per monitor: it
// resolves the target, sends one echo request and waits for the
// matching reply. It answers exactly one question — "does this host
// answer ICMP?" — so there is no port, no TLS and no application-level
// reply code.
//
// The socket is an *unprivileged* ICMP datagram socket
// (SOCK_DGRAM/IPPROTO_ICMP), not a raw socket: the process needs no
// CAP_NET_RAW, only a GID inside the kernel's
// `net.ipv4.ping_group_range`. That sysctl is a Kubernetes "safe"
// sysctl, so the deployment sets it per-pod and the container keeps
// running as nonroot. Whether a pod can ping *without* it depends on
// the container runtime: where the runtime leaves the range at the
// kernel default `1 0`, the socket call fails with EACCES — see
// Preflight and ADR-0016.
//
// One echo per probe tick is deliberate: packet loss is absorbed by
// the scheduler's existing in-cycle retries (retries × retryBackoff),
// the same mechanism the HTTP and SMTP probes rely on.
package pingcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/toggle-corp/toggle-monitor/internal/probe"
)

// Config describes a single ICMP probe.
type Config struct {
	// Host is a hostname or literal IP. Hostnames resolve per tick, so
	// a DNS outage surfaces as probe.FailKindDNS and feeds the
	// self-health detector (ADR-0008) like every other probe kind.
	Host string
	// Timeout bounds the whole probe (resolve + send + wait for reply).
	Timeout time.Duration
}

const defaultTimeout = 5 * time.Second

// payloadMagic identifies our own echo replies on a shared socket.
// The kernel rewrites the echo ID on a datagram ICMP socket, so the
// payload and sequence — not the ID — are what a reply is matched on.
var payloadMagic = []byte("toggle-monitor")

// Probe implements probe.Prober.
func (cfg Config) Probe(ctx context.Context) probe.Result {
	start := time.Now()
	res := cfg.run(ctx)
	if res.Duration == 0 {
		res.Duration = time.Since(start)
	}
	return res
}

func (cfg Config) run(ctx context.Context) probe.Result {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	addr, err := resolve(ctx, cfg.Host)
	if err != nil {
		return probe.Result{Error: fmt.Sprintf("resolve %s: %v", cfg.Host, err), FailKind: classify(err)}
	}

	conn, err := listen(addr.IP)
	if err != nil {
		return probe.Result{Error: fmt.Sprintf("icmp socket: %v", err), FailKind: probe.FailKindDial}
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)

	// The socket deadline bounds the probe, but only ctx carries a
	// SIGTERM mid-check. Expiring the deadline unblocks the pending read
	// immediately so shutdown isn't held for the rest of the timeout.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-stop:
		}
	}()

	seq := int(time.Now().UnixNano()) & 0xffff
	msg := icmp.Message{
		Type: echoType(addr.IP),
		Body: &icmp.Echo{
			// The kernel overwrites ID with the socket's port on a
			// datagram ICMP socket; it is set only so the wire message
			// is well-formed.
			ID:   os.Getpid() & 0xffff,
			Seq:  seq,
			Data: payloadMagic,
		},
	}
	wire, err := msg.Marshal(nil)
	if err != nil {
		return probe.Result{Error: fmt.Sprintf("marshal echo: %v", err), FailKind: probe.FailKindDial}
	}

	sentAt := time.Now()
	if _, err := conn.WriteTo(wire, &net.UDPAddr{IP: addr.IP, Zone: addr.Zone}); err != nil {
		return probe.Result{Error: fmt.Sprintf("send echo to %s: %v", addr.IP, err), FailKind: classify(err)}
	}

	rtt, err := awaitReply(conn, addr.IP, seq, sentAt)
	if err != nil {
		return probe.Result{Error: fmt.Sprintf("echo %s: %v", addr.IP, err), FailKind: classify(err)}
	}
	// Duration is the round-trip time of the echo, not the wall time of
	// the whole probe — resolution and socket setup are not latency of
	// the target.
	return probe.Result{Duration: rtt}
}

// awaitReply reads until the echo reply matching seq arrives or the
// connection deadline expires. Replies belonging to other conversations
// (a stray echo, another sequence) are skipped rather than failing the
// probe.
func awaitReply(conn *icmp.PacketConn, dst net.IP, seq int, sentAt time.Time) (time.Duration, error) {
	proto := protoNumber(dst)
	buf := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFrom(buf)
		if err != nil {
			return 0, err
		}
		rtt := time.Since(sentAt)
		if !sameHost(peer, dst) {
			continue
		}
		reply, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil {
			continue
		}
		if reply.Type != echoReplyType(dst) {
			continue
		}
		body, ok := reply.Body.(*icmp.Echo)
		if !ok || body.Seq != seq || !bytes.Equal(body.Data, payloadMagic) {
			continue
		}
		return rtt, nil
	}
}

// Preflight opens and closes one ICMP datagram socket to prove the
// process may ping at all. It is called once at startup when the config
// declares ping monitors, so a pod missing the
// `net.ipv4.ping_group_range` sysctl fails loudly at boot instead of
// reporting every ping target as down.
func Preflight() error {
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		return fmt.Errorf("open unprivileged ICMP socket: %w — the process GID must be inside "+
			"net.ipv4.ping_group_range (in Kubernetes, set it as a pod sysctl; see docs/operations.md)", err)
	}
	return conn.Close()
}

// resolve turns Host into a single target address, preferring IPv4 —
// ICMP and ICMPv6 are separate protocols and one address has to win.
// A literal IP short-circuits the lookup.
func resolve(ctx context.Context, host string) (*net.IPAddr, error) {
	if ip := net.ParseIP(host); ip != nil {
		return &net.IPAddr{IP: ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if a.IP.To4() != nil {
			addr := a
			return &addr, nil
		}
	}
	if len(addrs) > 0 {
		addr := addrs[0]
		return &addr, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// listen opens the unprivileged ICMP datagram socket for the address
// family of dst. "udp4"/"udp6" select SOCK_DGRAM here; they are not UDP
// on the wire.
func listen(dst net.IP) (*icmp.PacketConn, error) {
	if dst.To4() != nil {
		return icmp.ListenPacket("udp4", "0.0.0.0")
	}
	return icmp.ListenPacket("udp6", "::")
}

func echoType(dst net.IP) icmp.Type {
	if dst.To4() != nil {
		return ipv4.ICMPTypeEcho
	}
	return ipv6.ICMPTypeEchoRequest
}

func echoReplyType(dst net.IP) icmp.Type {
	if dst.To4() != nil {
		return ipv4.ICMPTypeEchoReply
	}
	return ipv6.ICMPTypeEchoReply
}

// protoNumber is the IANA protocol number ParseMessage needs to pick
// the ICMP vs ICMPv6 message set.
func protoNumber(dst net.IP) int {
	if dst.To4() != nil {
		return 1 // ICMP
	}
	return 58 // ICMPv6
}

// sameHost reports whether a received packet came from the target. The
// peer of a datagram ICMP socket is a *net.UDPAddr with a zero port.
func sameHost(peer net.Addr, dst net.IP) bool {
	switch p := peer.(type) {
	case *net.UDPAddr:
		return p.IP.Equal(dst)
	case *net.IPAddr:
		return p.IP.Equal(dst)
	default:
		return false
	}
}

// classify maps a transport-level error into a probe.FailKind by
// unwrapping the real error chain (errors.As), never by string-matching.
// DNS runs first because a timed-out lookup is wrapped in a
// *net.DNSError. See ADR-0008.
func classify(err error) probe.FailKind {
	if err == nil {
		return probe.FailKindNone
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return probe.FailKindDNS
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return probe.FailKindTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return probe.FailKindTimeout
	}
	return probe.FailKindDial
}
