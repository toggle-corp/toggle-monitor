---
status: accepted
date: 2026-09-17
deciders: [monitoring-team]
---

# ADR 0016 — Ping monitors probe ICMP from an unprivileged datagram socket

**Status:** Accepted
**Date:** 2026-09-17
**Relates to:** the SMTP monitoring design (the `kind` column and the
`probe.Prober` seam this reuses). ADR-0008 (self-health degraded mode) —
ping DNS failures feed the same detector.

## Context

Some things worth monitoring answer no protocol we can speak: a router,
a firewall, an appliance, a VM whose only service is behind a VPN. The
question about them is "is the host reachable at all?", and ICMP echo is
the answer.

The obstacle is not the protocol. It is that toggle-monitor runs as a
pod: distroless, `runAsNonRoot: true`, UID/GID 65532, no added
capabilities. The classic way to send an echo request is a raw socket,
which needs `CAP_NET_RAW` — a capability the restricted
PodSecurityStandard forbids and that we do not want to grant a process
whose whole job is talking to untrusted endpoints.

Linux offers a second path: `socket(AF_INET, SOCK_DGRAM, IPPROTO_ICMP)`.
The kernel builds and demultiplexes the echo header itself, rewriting
the echo ID to the socket's port, and grants the socket to any process
whose GID falls inside `net.ipv4.ping_group_range`. No capability is
involved. This is how `iputils` ping works on most modern distributions.

Whether a pod already has that GID range is a property of the container
runtime, not of Kubernetes. containerd's `enable_unprivileged_icmp`
widens every sandbox to `0 2147483647`, and it is on by default in
containerd 2.x — on a kind v0.32 cluster (containerd 2.3.1) an
unmodified pod can already ping. Where the runtime does not set it —
older containerd, CRI-O, or an explicit `enable_unprivileged_icmp =
false` — the pod's network namespace keeps the kernel default `1 0` and
the socket call fails with EACCES. Measured in the exact runtime image:

```
gcr.io/distroless/static-debian12:nonroot, -u 65532:65532
  net.ipv4.ping_group_range="1 0"          → socket: permission denied
  net.ipv4.ping_group_range="65532 65532"  → echo reply, rtt 810µs
```

`net.ipv4.ping_group_range` is on the kubelet's **safe sysctl** list: it
is namespaced per-pod, has no effect on the node or on other pods, and
needs no kubelet flag. A pod-level `securityContext.sysctls` entry is
therefore the whole requirement, and it makes the capability
deterministic instead of runtime-dependent.

## Considered Options

**A. Raw socket + `CAP_NET_RAW`.** Works everywhere, including outside
Linux-with-ping-sockets. Costs a capability on a pod that exists to make
requests to whatever the config names, and rules out the restricted PSS
for good — paid on every install, including the ones that never declare
a ping monitor.

**B. A TCP "ping" (connect to a port, discard the result).** No new
privileges at all, but it answers a different question. A host with
every port filtered is indistinguishable from a host that is down, which
is exactly the case ping monitors exist for.

**C. Unprivileged ICMP datagram socket, gated on a pod sysctl.**
Chosen. No capability, keeps the pod nonroot, leaves the restricted PSS
reachable, and the one requirement it does impose is a safe sysctl the
chart can set.

**D. Sidecar or DaemonSet that owns the raw socket.** Moves the
capability somewhere else and adds an IPC protocol, a second image and a
second failure mode for one echo request.

## Decision

**Ping monitors probe through an unprivileged ICMP datagram socket
(`golang.org/x/net/icmp` over `udp4`/`udp6`), and the deployment grants
the capability with the `net.ipv4.ping_group_range` pod sysctl.**

- `internal/pingcheck` implements `probe.Prober`, so the scheduler,
  alert state machine, coalescing, Slack rendering and metrics treat a
  ping monitor exactly like every other kind. Nothing outside the
  package knows ICMP exists.
- **One echo per tick.** Packet-loss tolerance is the scheduler's
  existing in-cycle `retries × retryBackoff`, not a second, ping-only
  loss-threshold knob. A monitor that should survive a lost packet sets
  `retries: 2`.
- **The target is resolved every tick**, so a lookup failure classifies
  as `probe.FailKindDNS` and feeds the self-health detector (ADR-0008)
  on the same terms as an HTTP or SMTP probe. IPv4 wins when a name has
  both address families: ICMP and ICMPv6 are separate protocols and one
  address has to be picked.
- **Replies are matched on sequence and payload, never on the echo ID** —
  the kernel overwrites the ID on a datagram socket.
- **Startup preflight.** When the config declares any `pingMonitors`,
  `RunServe` opens and closes one ICMP socket before doing anything
  else and refuses to start if it fails, naming the sysctl. Without this
  a pod missing the sysctl starts happily and reports every ping target
  as down — a local misconfiguration wearing the costume of an estate-wide
  outage.
- **Config shape.** A static-only `pingMonitors:` block (kube discovery
  never produces ICMP monitors), `kind: ping` in the existing `monitors`
  table, `url` synthesized as `ping://host`. No port, no TLS, no SSL
  thresholds; `TLSBearing` is false so the SSL state machine is skipped.
  `host` must be a bare hostname or IP.
- **No `proxy:` field.** SOCKS5 carries TCP only. Declaring a proxy on a
  ping monitor is a validation error rather than a silently ignored key.
- **The chart ships the sysctl behind `ping.enabled`, default false.** A
  deployment that never pings gains no sysctl, and a deployment that
  does gets the capability whatever its runtime defaults to.

No migration is required: `kind` is a plain `TEXT` column with no CHECK
constraint, and `host` already exists for SMTP.

## Consequences

**A ping monitor has no status code.** ICMP carries no application-level
reply code, so the check persists code `0`, which the store writes as
NULL and the UI renders as no code. The error string is the only
discriminator between failure modes.

**`NetworkPolicy` cannot allow ICMP.** The API selects TCP, UDP and SCTP
only. In a namespace with default-deny egress, ping monitors stay down
until a CNI-specific rule (`CiliumNetworkPolicy` `icmps:`, or Calico)
lets echo requests out. This is a property of the cluster, not something
the chart can fix, and is documented in `docs/operations.md` §ICMP.

**A forgotten `ping.enabled` is a CrashLoopBackOff**, by choice. The
alternative — starting and paging for every ping target — costs an
on-call hour to diagnose instead of one look at the pod's logs.

**Targets that drop ICMP look down.** Plenty of hosts and cloud load
balancers answer HTTP but never answer a ping. Ping monitors are for
hosts known to answer; the operator confirms that before declaring one.

**Host reachability becomes a usable `dependsOn` parent.** An HTTP
monitor that depends on a ping monitor for the same host rolls into the
host's incident when the host itself is unreachable, instead of paging
separately.

## Verification

Confirmed on a local kind cluster (v0.32, containerd 2.3.1, Kubernetes
v1.36), running the real image as UID/GID 65532:

- With `ping.enabled: true` the pod carries the sysctl and probes for
  real: a monitor targeting a pod IP reports `up`, a monitor targeting an
  unrouted address (`203.0.113.9`) reports `down` with `echo …: i/o
  timeout`. Both persist a NULL status code, as intended.
- A pod pinned to `net.ipv4.ping_group_range: "1 0"` — a runtime without
  unprivileged ICMP — exits at once with the preflight error naming the
  sysctl, instead of starting and paging for every target.
- Under a namespace enforcing `restricted`, a pod carrying the sysctl is
  admitted; the sysctl draws no violation. The chart's own pod spec is
  *not* restricted-compliant today for unrelated reasons
  (`allowPrivilegeEscalation`, `capabilities.drop`, `seccompProfile` are
  unset at container level) — a preexisting gap this ADR does not close.

## References

- `internal/pingcheck` — the prober and the startup preflight.
- `internal/config` — the `pingMonitors:` block and its validation.
- `deploy/helm/toggle-monitor` — `ping.enabled` and the rendered sysctl.
- [docs/config-schema.md §5d](../config-schema.md) — field reference.
- [docs/operations.md §ICMP](../operations.md) — enabling it, and the
  failure modes.
- Kubernetes documentation, "Using sysctls in a Kubernetes cluster" —
  the safe-sysctl list, which `net.ipv4.ping_group_range` has been on
  since v1.18.
