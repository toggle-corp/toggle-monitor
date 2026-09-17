package pingcheck

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/toggle-corp/toggle-monitor/internal/probe"
)

// requireICMP skips a test on hosts where the kernel won't hand out an
// unprivileged ICMP socket (ping_group_range excludes our GID). The
// probe behaviour is still worth asserting where the socket exists —
// this is the one thing the test rig cannot fake.
func requireICMP(t *testing.T) {
	t.Helper()
	if err := Preflight(); err != nil {
		t.Skipf("no unprivileged ICMP socket here: %v", err)
	}
}

func TestProbe_loopbackSucceeds(t *testing.T) {
	requireICMP(t)

	res := Config{Host: "127.0.0.1", Timeout: 3 * time.Second}.Probe(context.Background())
	if res.Error != "" {
		t.Fatalf("loopback ping failed: %s", res.Error)
	}
	if res.FailKind != probe.FailKindNone {
		t.Errorf("FailKind: got %q, want none", res.FailKind)
	}
	// ICMP has no application-level reply code; the store writes NULL
	// for code 0, which is what the detail page expects.
	if res.Code != 0 {
		t.Errorf("Code: got %d, want 0", res.Code)
	}
	if res.Duration <= 0 {
		t.Error("Duration should carry the round-trip time")
	}
}

// Hostname targets resolve per tick, so a name-resolution failure has to
// classify as DNS — that is what feeds the self-health detector
// (ADR-0008) instead of reading as a genuine outage.
func TestProbe_resolveFailureIsDNS(t *testing.T) {
	res := Config{Host: "no-such-host.invalid", Timeout: 3 * time.Second}.Probe(context.Background())
	if res.Error == "" {
		t.Fatal("expected a failure for an unresolvable host")
	}
	if res.FailKind != probe.FailKindDNS {
		t.Errorf("FailKind: got %q, want %q (error: %s)", res.FailKind, probe.FailKindDNS, res.Error)
	}
}

func TestProbe_expiredDeadlineIsTimeout(t *testing.T) {
	requireICMP(t)

	// A literal IP skips resolution, so the expired deadline can only
	// surface on the socket itself.
	res := Config{Host: "127.0.0.1", Timeout: time.Nanosecond}.Probe(context.Background())
	if res.Error == "" {
		t.Fatal("expected a failure with an already-expired deadline")
	}
	if res.FailKind != probe.FailKindTimeout {
		t.Errorf("FailKind: got %q, want %q (error: %s)", res.FailKind, probe.FailKindTimeout, res.Error)
	}
}

func TestProbe_cancelledContextDoesNotHang(t *testing.T) {
	requireICMP(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := Config{Host: "192.0.2.1", Timeout: time.Minute}.Probe(ctx)
	if res.Error == "" {
		t.Fatal("expected a failure on a cancelled context")
	}
	if res.Duration > 5*time.Second {
		t.Errorf("probe ran for %v after cancellation", res.Duration)
	}
}

// The preflight error is the operator's only clue when a pod is missing
// the sysctl, so it has to name it.
func TestPreflight_errorNamesTheSysctl(t *testing.T) {
	err := Preflight()
	if err == nil {
		return // socket available here; nothing to assert
	}
	if !strings.Contains(err.Error(), "net.ipv4.ping_group_range") {
		t.Errorf("preflight error does not name the sysctl: %v", err)
	}
}

func TestResolve_literalIPSkipsLookup(t *testing.T) {
	addr, err := resolve(context.Background(), "192.0.2.1")
	if err != nil {
		t.Fatalf("resolve literal IP: %v", err)
	}
	if got := addr.IP.String(); got != "192.0.2.1" {
		t.Errorf("resolved to %q", got)
	}
}
