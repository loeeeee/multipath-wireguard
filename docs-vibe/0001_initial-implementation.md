# 0001_initial-implementation

- **Date:** 2026-06-30
- **Status:** done

## Intent
Implement the full multipath-wireguard UDP relay from scratch per DESIGN.md and INTENT.md specs. The project had only documentation and a flake.nix -- no Go source code, no go.mod, no flake.lock.

## Implementation
All core Go source files written from scratch against the DESIGN.md architecture:

- `go.mod` -- module `multipath-wireguard`, Go 1.26 (pinned by flake), no `require` block (stdlib only per rule 1).
- `relay.go` -- shared `counter` type with atomic fields (`rxPackets`, `rxBytes`, `txPackets`, `txBytes`, `lastSeen`), `rateLimiter` for hot-path error logging, `maxPacket` constant (1500).
- `config.go` -- `parseRoutes(path)` reads newline-delimited routes file, strips `#` comments and blank lines, validates every line as `host:port`. Fails on first invalid line or empty file.
- `server.go` -- server struct with `outer` (listening), `target` (connected to WireGuard), `routeTable` (map guarded by `sync.RWMutex`). Goroutines: `outerReader` (ReadFromUDP, upsert source, Write to target; target failure cancels context), `innerReader` (Read from target, snapshot route table under RLock, WriteToUDP to each source outside lock), `janitor` (ticker-based prune of idle entries), `counterLogger` (periodic structured log).
- `client.go` -- client struct with `inner` (listening) and per-route connected sockets (`DialUDP`). `wgPeer` stored in `atomic.Pointer[net.UDPAddr]`. Goroutines: `innerReader` (ReadFromUDP, store wgPeer, Write to each route), per-route `routeReader` (Read from connected socket, load wgPeer, WriteToUDP to inner), `counterLogger`.
- `main.go` -- flag parsing for both modes, `signal.NotifyContext` for SIGINT/SIGTERM, fail-fast validation of all flags and routes at startup before entering packet loops.
- `relay_test.go` -- unit tests for route parsing (valid lines, comments, blanks, error cases), counter atomic operations (single + concurrent), rate limiter, route table upsert/snapshot/prune.
- `e2e_test.go` -- three loopback end-to-end tests: `TestE2EDuplication` (fake echo, server, 2 forwarders, client; verify echo receives duplicates, all replies return), `TestE2EFailover` (kill one forwarder, verify traffic continues over survivor), `TestE2ERouteTablePrune` (verify learned routes pruned after client timeout).

Key design decisions:
- Snapshot route table entries into a pre-allocated slice under RLock, then send outside lock (rule 4 compliance).
- Target write failures in server cancel context to shut down (plan note: target is the only consumer; no target = can't do job).
- Rate-limited logging with simple timestamp comparison (not perfectly accurate under contention but good enough).
- Pre-allocated `[]*routeEntry` slice reused via `entries = entries[:0]` to minimize per-packet allocation.
- No `-read-buffer`/`-write-buffer` flags implemented yet (noted as follow-up).

## Validation results
- `nix flake check` -- passes, built successfully
- `nix build` -- produces statically-linked binary at `./result/bin/multipath-wireguard`
- `go test ./... -race` -- all tests pass (7.145s), race detector clean
- `gofmt -l .` -- clean
- `go vet ./...` -- clean
- `go.mod` -- no `require` block, stdlib only

## Usage
```bash
nix develop
go run . client -listen 127.0.0.1:51902 -routes ./routes.conf
go run . server -listen 0.0.0.0:51900 -target 127.0.0.1:51820 -client-timeout 30s
go test ./... -race
```

## Status & follow-ups
- [x] `nix flake check` / `nix build` -- passes
- [x] `go test ./... -race` -- passes
- [ ] `-read-buffer`/`-write-buffer` flags -- specified in DESIGN but not yet wired to socket buffer calls
- [ ] Hot path allocation audit with `-benchmem` -- should verify no per-packet `make`/`append`/`fmt.Sprintf`
- [ ] `flake.nix` `src = ./.` restored (was briefly changed to `lib.cleanSource` during troubleshooting; reverted)

## Review fixes (2026-06-30)

10 issues from local code review, all fixed:

- **server.go:** route-table map key changed from `string` to `netip.AddrPort` (eliminates per-packet `addr.String()` allocation). Added `maxRouteEntries` cap (10,000) to prevent unbounded memory growth. `snapshot()` now reuses a pre-allocated `scratch` slice (no per-packet `make`). `server.run()` returns `error`; target-write failure propagates via buffered channel, causing `os.Exit(1)` in `runServer()`. `serverOuterReader` uses `ReadFromUDPAddrPort` (zero-allocation). Removed unused `logInterval` parameter from `serverJanitor`.
- **client.go:** `clientInnerReader` uses `ReadFromUDPAddrPort`; `*net.UDPAddr` only allocated when peer actually changes (via `wgPeer` comparison).
- **main.go:** Duration flags (`-client-timeout`, `-log-interval`) validated for positivity before use; panics on non-positive values replaced with clean `os.Exit(2)` messages.
- **e2e_test.go:** Extracted shared `startE2ETopology` helper (eliminates triplicate setup). `runEcho` made nil-safe for `recvCount` (dead stores removed from `TestE2EFailover`, `TestE2ERouteTablePrune`).
