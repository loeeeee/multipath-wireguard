# multipath-wireguard

A tiny, dependency-free UDP relay that runs a single WireGuard link over several
tunnels at once for redundancy. It duplicates every WireGuard packet across all
active paths and relies on WireGuard's built-in anti-replay window to discard
the duplicates. Send every copy, first one wins.

> Status: pilot. Linux only. Standard-library Go, no external dependencies.

## Why

We carry WireGuard through `wstunnel` (WireGuard-over-WebSocket/TLS) to survive
restrictive networks. With one tunnel that is a single point of failure.
`multipath-wireguard` lets the same WireGuard link ride several tunnels
simultaneously, so losing any one tunnel is invisible to WireGuard and to
everything above it.

This is **redundancy, not aggregation**: traffic is duplicated, not split. It is
the right model for a loss-sensitive link where you care about a path *failing*,
not about summing bandwidth. WireGuard already authenticates every packet and
drops replays, so the relay stays deliberately dumb: it never parses, dedups,
encrypts, or inspects payloads.

See [`INTENT.md`](INTENT.md) for the goals and the reasons existing tools
(engarde, glorytun, MLVPN) were not a fit, and [`DESIGN.md`](DESIGN.md) for the
full architecture.

## How it works

The relay sits between WireGuard and the tunnels on both ends:

- **client** mode faces the local WireGuard initiator. It fans each packet out
  to a static list of routes (your local `wstunnel` listener ports) and relays
  returns back to WireGuard.
- **server** mode faces the WireGuard responder at the far site. It receives
  from the tunnels, forwards to local WireGuard, learns each tunnel's return
  address, and fans WireGuard's replies back across all of them.

Duplicates converge back onto one WireGuard socket on each end, where WireGuard
discards all but the first copy.

## Build

This is a Nix flake; the toolchain is pinned in `flake.nix` / `flake.lock`.

```bash
nix build                       # -> ./result/bin/multipath-wireguard
nix run . -- client -listen 127.0.0.1:51902 -routes ./routes.conf
nix develop                     # dev shell with the pinned Go + tools
nix flake check                 # build + tests
```

Inside `nix develop` the usual Go commands work (`go build ./...`,
`go test ./...`). There are no dependencies to fetch; it produces a single
static binary.

## Usage

Client side (local machine), fanning out to your tunnels:

```bash
multipath-wireguard client \
  -listen 127.0.0.1:51902 \           # WireGuard's Endpoint points here
  -routes ./routes.conf               # one host:port per line (the tunnels)
```

`routes.conf`:

```
# tunnel #1 (pilot)
127.0.0.1:51811
```

Server side (far end), forwarding to local WireGuard:

```bash
multipath-wireguard server \
  -listen 0.0.0.0:51900 \             # where the wstunnel servers forward to
  -target 127.0.0.1:51820 \           # the local WireGuard UDP port
  -client-timeout 30s                 # prune return paths idle this long
```

Point WireGuard's `Endpoint` at the client's `-listen` address, and make sure
WireGuard's MTU is low enough to avoid fragmentation inside the `wstunnel`
encapsulation.

## Growing from one path to many

Adding a second or third tunnel is a one-line change: stand up another
`wstunnel`, append its local listener to `routes.conf`, and restart the client.

```
# tunnel #1
127.0.0.1:51811
# tunnel #2 (added later)
127.0.0.1:51812
```

The server side needs no change; it learns the new return path the first time a
packet arrives over it.

## Privileges

None required. The relay uses ordinary UDP sockets on high ports, never binds to
an interface, and needs no Linux capabilities (`CAP_NET_RAW` / `CAP_NET_BIND_SERVICE`).
It runs cleanly as an unprivileged systemd user or system service.

## Documentation

- [`INTENT.md`](INTENT.md) — goals, non-goals, success criteria.
- [`DESIGN.md`](DESIGN.md) — architecture, packet flow, concurrency, failure
  handling, testing.
- [`AGENTS.md`](AGENTS.md) — contributor and coding-agent rules.
- `docs-vibe/` — append-only per-session development log.
