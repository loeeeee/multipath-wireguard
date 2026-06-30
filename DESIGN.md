# multipath-wireguard — DESIGN

> The "how." Architecture, packet flow in both directions, the two modes,
> config, concurrency model, and failure handling. Read INTENT.md first.

## 1. Overview

`multipath-wireguard` is a bidirectional UDP relay that sits between WireGuard and one or more
tunnels (`wstunnel` instances). It **duplicates** each WireGuard packet across
all active tunnels and relays returns back, leaning entirely on WireGuard's
anti-replay window to discard the duplicates. It never parses, dedups, or
secures payloads — it moves opaque datagrams.

One binary, two modes:

- **`client`** — faces the local WireGuard *initiator*. Fans out to a **static**
  list of routes (the local `wstunnel` listener ports). Returns go back to the
  one local WireGuard endpoint.
- **`server`** — faces the local WireGuard *responder* at the remote site. Receives
  from the tunnels, forwards to local WireGuard, and **learns** the tunnels'
  return addresses dynamically, fanning WireGuard's replies back across all of
  them.

The asymmetry is the only real subtlety: the client knows its destinations up
front (config); the server discovers its return paths from incoming traffic
(like engarde-server), because the source port a tunnel forwards from is not
something we want to hardcode.

## 2. Topology

```
LOCAL (client mode)                         REMOTE SITE (server mode)
┌───────────────────────────┐               ┌───────────────────────────┐
│ WireGuard (initiator)     │               │   WireGuard (responder)   │
│   Endpoint → 127.0.0.1:LP │               │   listens 127.0.0.1:WGP   │
│        │  ▲               │               │        ▲  │               │
│        ▼  │               │               │        │  ▼               │
│  mpwg client              │               │   mpwg server             │
│  inner: 127.0.0.1:LP      │               │   inner→ 127.0.0.1:WGP    │
│  routes (static):         │               │   outer: 0.0.0.0:SP       │
│   → 127.0.0.1:T1 ─┐       │               │      ▲ (learns sources)   │
│   → 127.0.0.1:T2 ─┼─...   │               │      │                    │
│        │          │       │               │   wstunnel servers        │
│        ▼          ▼       │               │    forward → 127.0.0.1:SP │
│  wstunnel #1, #2, …       │   N× WSS/TLS  │        ▲                  │
│  local listeners T1,T2 ───┼───────────────┼────────┘                  │
└───────────────────────────┘               └───────────────────────────┘

LP = multipath-wireguard client listen port (WireGuard's Endpoint)
T1,T2,… = local wstunnel UDP listener ports (one per tunnel) = the "routes"
SP = multipath-wireguard server listen port (where all wstunnel servers forward to)
WGP = local WireGuard UDP port at the remote site
mpwg = multipath-wireguard (shortened in the diagram only)
```

**Growth (G2):** adding tunnel #2 = stand up a second `wstunnel`, then append
`127.0.0.1:T2` to the client's routes list and restart. The server side needs
no change — it learns the new return path the first time a packet arrives over
it.

## 3. Packet flow

### 3.1 Forward (application → remote site)

1. WireGuard emits a UDP datagram to `127.0.0.1:LP` (the client inner socket).
2. `multipath-wireguard client` records WireGuard's source address as `wgPeer` (refreshed on
   every inbound packet so a WireGuard restart is picked up automatically).
3. It writes one copy of the datagram to **each** route socket (best-effort; a
   failing route never blocks the others — see §6).
4. Each `wstunnel` carries its copy over WSS to the remote site, where its server
   forwards to `127.0.0.1:SP` (the server outer socket).
5. `multipath-wireguard server` records the **source address** of that arriving packet in its
   route table (with a last-seen timestamp), then writes the datagram to its
   inner socket → `127.0.0.1:WGP`.
6. WireGuard at the remote site sees N copies, accepts the first, and **drops the
   rest via anti-replay**.

### 3.2 Return (remote site → application)

1. WireGuard at the remote site replies to the server inner socket's address (which
   WireGuard has latched as the peer endpoint).
2. `multipath-wireguard server` reads the reply on the inner socket and writes one copy to
   **every live source** in its learned route table (duplicate on the way back
   too — redundancy is symmetric).
3. Each copy travels back through its `wstunnel` to the client, arriving on the
   corresponding **route socket**.
4. `multipath-wireguard client` reads it and writes it from the **inner** socket back to
   `wgPeer`.
5. The client-side WireGuard sees N copies, accepts the first, drops the rest.

### 3.3 Why per-route sockets on the client

The client uses **one connected UDP socket per route** (`net.DialUDP` to the
route address) rather than one shared socket. Benefits: writes are a plain
`Write` to a fixed peer; returns from that tunnel arrive *only* on that socket,
so demultiplexing is free; and a dead route surfaces as an error on its own
socket without affecting others.

## 4. Configuration

Zero external dependencies → **no YAML/TOML**. Config is command-line flags plus
an optional newline-delimited **routes file** (so "add a tunnel" is "add a
line"). Comments with `#`, blank lines ignored.

### 4.1 Client

```
multipath-wireguard client \
  -listen 127.0.0.1:51902 \      # inner: where WireGuard sends (WG Endpoint = this)
  -routes /etc/multipath-wireguard/routes.conf # one host:port per line; the tunnels
```

`routes.conf`:

```
# wstunnel #1 (pilot)
127.0.0.1:51811
# add tunnel #2 here later — one line, then restart:
# 127.0.0.1:51812
```

### 4.2 Server

```
multipath-wireguard server \
  -listen 0.0.0.0:51900 \        # outer: where all wstunnel servers forward to
  -target 127.0.0.1:51820 \      # inner: the local WireGuard UDP port (WGP)
  -client-timeout 30s            # prune learned return paths after this idle time
```

Common flags (both modes): `-read-buffer`, `-write-buffer` (socket buffer
sizes), `-log-interval` (counter dump cadence), `-v` (verbose).

## 5. Concurrency model

Goroutines are few and fixed; the packet path allocates nothing per packet.

### Client

- **inner reader** (1 goroutine): loop `ReadFromUDP(inner)` → update `wgPeer`
  (atomic pointer) → for each route socket, `Write(copy)`.
- **route reader** (1 per route): loop `Read(routeSock)` → `WriteToUDP(inner,
  wgPeer)`.
- `wgPeer` stored in an `atomic.Pointer[net.UDPAddr]`; readers load, inner reader
  stores. No mutex on the hot path.

### Server

- **outer reader** (1 goroutine): loop `ReadFromUDP(outer)` → upsert source into
  the route table (timestamped) → `Write(inner)` toward WireGuard.
- **inner reader** (1 goroutine): loop `Read(inner)` (WireGuard replies) →
  snapshot live sources → `WriteToUDP(outer, src)` for each.
- **janitor** (1 goroutine, ticker): prune table entries idle longer than
  `-client-timeout`.
- Route table is a `map[string]entry` guarded by a `sync.RWMutex`; the inner
  reader takes a brief read-lock to snapshot addresses, copies them to a local
  slice, releases, then sends. Sends never happen under the lock.

### Buffers

- Each reader owns a fixed `[]byte` of `maxPacket` (default 1500; configurable)
  and uses `ReadFromUDP`/`Read` into it directly. No per-packet allocation, no
  GC pressure on the hot path.

## 6. Failure handling

The governing rule: **best-effort duplication; a sick path must never stall a
healthy one, and must never block a read loop.**

- **Write errors are swallowed per packet.** A copy to a route that fails
  (`wstunnel` down → local port closed → `ECONNREFUSED` on the connected
  socket) is logged at low frequency and dropped. Other routes still got their
  copy. This *is* the redundancy working.
- **No blocking sends.** UDP writes are effectively non-blocking; we additionally
  never hold a lock across a write and never let one slow route serialize
  others. If we ever see blocking, set a short write deadline and treat timeout
  as drop.
- **Dead route recovery.** When `wstunnel` returns, its local port reopens and
  writes succeed again — no `multipath-wireguard` restart needed.
- **WireGuard restart.** Client re-learns `wgPeer` from the next inbound packet;
  server re-latches when WireGuard's replies resume.
- **`multipath-wireguard` restart.** Learned server state is lost and re-learned within one
  packet; WireGuard re-handshakes. Acceptable.
- **Stale return paths.** Server prunes sources idle beyond `-client-timeout` so
  a tunnel that's gone for good stops receiving duplicated replies.

## 7. Security & capabilities (G3)

- **No capabilities.** Sockets are ordinary `AF_INET/SOCK_DGRAM`. We **never**
  call `SO_BINDTODEVICE`, so `CAP_NET_RAW` is not required. All ports are high
  (>1024), so `CAP_NET_BIND_SERVICE` is not required.
- Runs fine as a `systemd` **user** service, or a system service with
  `DynamicUser=yes` and an empty capability set. Keep the whole tunnel chain in
  **one** scope (all user or all system) so `BindsTo=`/`After=` against the
  `wstunnel` units actually applies across the boundary.
- The relay sees only WireGuard ciphertext. It provides no confidentiality,
  authenticity, or integrity itself, and is not expected to — that is
  WireGuard's job. Binding the client `-listen` and server `-target` to
  `127.0.0.1` keeps the plaintext-of-our-layer (still WG-encrypted) off the wire
  except where it must traverse `wstunnel`.

## 8. Anti-replay assumptions

WireGuard's anti-replay uses a sliding window (~2000 packets in the kernel
implementation). Duplicates we emit are sent within microseconds of each other,
so even with meaningfully different per-path latency, the late copy lands far
inside the window and is dropped cleanly. The only way to defeat this is
*extreme* inter-path latency divergence (seconds) under high packet rates —
outside our operating envelope. If paths ever diverge that much, the correct
response is to drop the slow path, not to widen the window.

## 9. MTU

We stack WireGuard inside `wstunnel` (WebSocket + TLS framing) inside the path
MTU. Set the **WireGuard** interface MTU low enough that an encapsulated packet
fits the path without IP fragmentation (start conservative, e.g. 1280, and tune
up only if verified). Fragmentation here looks like intermittent, size-dependent
breakage that mimics a multipath bug but isn't. `multipath-wireguard` itself adds **no**
header and no overhead per path; `maxPacket` only needs to cover the largest
datagram WireGuard hands us.

## 10. Observability

- Per-route / per-source atomic counters: packets in/out, bytes in/out, last-seen.
- A ticker (`-log-interval`) emits one structured log line per route summarizing
  the counters; a route whose inbound counter stops advancing while others move
  is your failed path.
- Keep it to logging for the pilot. A small HTTP `/status` JSON endpoint is a
  candidate for later (mirrors engarde's web list) but is explicitly out of
  scope now.

## 11. Repository layout

```
.
├── INTENT.md
├── DESIGN.md
├── AGENTS.md
├── README.md
├── flake.nix              # Nix toolchain, package (buildGoModule), checks
├── flake.lock             # pinned inputs; committed
├── go.mod                 # module def; no require block (stdlib only)
├── main.go                # flag parsing, mode dispatch, socket setup, signals
├── client.go              # client-mode readers + wgPeer handling
├── server.go              # server-mode readers + learned route table + janitor
├── relay.go               # shared: buffers, counters, copy loop helpers
├── config.go              # routes-file parser, flag wiring
├── relay_test.go          # unit tests (parser, table prune, counter math)
├── e2e_test.go            # loopback end-to-end duplication/dedup test (§12)
└── docs-vibe/             # append-only per-session dev log: ####_short-name.md (AGENTS.md)
```

## 12. Testing

- **Unit:** routes-file parsing (comments/blanks/bad lines), route-table upsert
  and prune, counter accounting.
- **End-to-end (loopback, no real WireGuard or wstunnel):**
  1. Stand up a fake "WireGuard" UDP echo on a local port.
  2. Start `multipath-wireguard server` pointing `-target` at it.
  3. Start two trivial UDP forwarders to stand in for two `wstunnel` paths,
     both delivering to the server's `-listen`.
  4. Start `multipath-wireguard client` with both forwarders as routes.
  5. Send a numbered packet into the client inner socket; assert the echo comes
     back exactly once *to the sender* even though two copies traversed the
     mesh (proves fan-out + that returns reach `wgPeer`); assert the server
     received two copies (proves duplication actually happened).
  6. Kill one forwarder mid-stream; assert traffic continues over the survivor
     (proves G1/success-criterion 3).
- **Race:** run the e2e and unit tests under `go test -race`.

## 13. Future work (not in pilot)

- SIGHUP hot reload of `routes.conf` (add a path with no restart).
- `/status` JSON endpoint + Prometheus-style counters.
- IPv6 routes.
- Optional per-route send pacing / write deadlines if a pathological tunnel ever
  blocks.