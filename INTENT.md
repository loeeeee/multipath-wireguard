# multipath-wireguard — INTENT

> The "why." What problem this solves, what success looks like, and what is
> deliberately out of scope. Read this before DESIGN.md.

## Problem

We tunnel a WireGuard link to a remote site through
`wstunnel` (WireGuard-over-WebSocket/TLS) so the traffic survives restrictive
networks. Today that is a **single path**: one `wstunnel` connection. If that
path degrades or drops, the WireGuard link drops with it.

We want **path redundancy**: the ability to run the same WireGuard link over
several independent tunnels at once, so that losing any one tunnel is invisible
to WireGuard and to everything above it.

## Why a custom duplication layer

WireGuard already does the hard part. Every packet it receives is
authenticated and run through an anti-replay window, so **duplicate copies of a
packet are silently discarded** by WireGuard itself. That means a redundancy
layer does not need to be clever: it only needs to

1. **fan out** — copy each WireGuard packet to every active tunnel, and
2. **fan in** — relay packets coming back from any tunnel to WireGuard,

and let WireGuard throw away the duplicates. This is a "send every copy, first
one wins" model (redundancy), **not** bandwidth aggregation.

The existing tools were each rejected for a concrete reason:

- **engarde** — mature, but fans out *per network interface*, not per
  destination. Adding a loopback tunnel later requires manufacturing a virtual
  interface per tunnel and granting `CAP_NET_RAW` for `SO_BINDTODEVICE`. The
  growth model fights our design.
- **glorytun / MLVPN / ubond** — aggregation-oriented and not actively
  maintained; reordering/failover behavior is the wrong fit and the
  maintenance risk is unacceptable for a load-bearing component.

Since the job is genuinely small and we want to own a component this critical,
we build a tight, dependency-free Go implementation we fully understand.

## Goals

- **G1 — Redundant delivery.** Each WireGuard packet is duplicated across all
  active tunnels; loss of any single tunnel does not interrupt the link.
- **G2 — Grow by configuration.** Start with one tunnel. Adding tunnel #2 and
  #3 later is a one-line config change (append a destination), not a code or
  architecture change.
- **G3 — Unprivileged.** Runs as an ordinary user with no Linux capabilities:
  high ports only, no interface binding, no `CAP_NET_RAW`, no `CAP_NET_BIND_SERVICE`.
- **G4 — Self-contained.** Single static Go binary, standard library only, easy
  to pin in a Nix closure and run as a systemd service.
- **G5 — Comprehensible.** Small enough (~a few hundred lines) that one person
  can read the whole thing and reason about its failure modes.

## Non-goals

- **No bandwidth aggregation.** We do not split traffic to sum throughput. Every
  packet goes down every path on purpose.
- **No de-duplication in this layer.** WireGuard's anti-replay window is the
  de-duplicator. We never inspect, parse, or dedup payloads.
- **No encryption, auth, or integrity of our own.** WireGuard owns all of that.
  This layer is a dumb relay carrying already-encrypted ciphertext.
- **No tunnel management.** We do not start, stop, or health-check `wstunnel`.
  We send to local UDP ports; whether a tunnel is healthy is WireGuard's
  problem to notice (via duplicates still arriving on other paths).
- **No IPv6 / no DNS-heavy config (initially).** Routes are literal `IP:port`.
  IPv6 may come later; it is not required for the pilot.
- **No cross-platform support.** Linux/NixOS only.

## Success criteria

The pilot is successful when:

1. With a **single** configured route, the WireGuard link to the remote site is
   established and stable through the duplication layer on both ends.
2. Adding a **second** route (a second `wstunnel`) is a one-line config edit +
   service restart, with **no** code change.
3. With two routes active, **killing either `wstunnel` mid-session** does not
   drop the WireGuard link — traffic continues over the survivor, and WireGuard
   shows no handshake loss to the application above it.
4. The process runs as an unprivileged systemd service with **no capabilities
   granted**.
5. Idle/steady-state CPU is negligible on the target host.

## Constraints & assumptions

- Target: Linux (NixOS), deployed via Home Manager / NixOS modules + systemd.
- WireGuard does the dedup; we depend on its anti-replay window. Duplicates are
  emitted near-simultaneously, so inter-path latency divergence stays well
  inside the window. (See DESIGN.md "Anti-replay assumptions.")
- The remote side runs the same binary in server mode. The remote
  is in scope for this project even though the wstunnel server there predates it.
- MTU must be set so WireGuard-inside-wstunnel does not fragment. (See DESIGN.md.)

## Open questions

- Do we want hot config reload (SIGHUP) for adding routes without dropping the
  process, or is a restart acceptable for the pilot? (Leaning: restart now,
  reload later.)
- Do we need a minimal metrics/status surface for the pilot, or is structured
  logging of per-route counters enough? (Leaning: logging now.)