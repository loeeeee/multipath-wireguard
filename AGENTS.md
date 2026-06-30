# AGENTS.md

Instructions for any coding agent (or human) working in this repository. Read
`INTENT.md` for *why* and `DESIGN.md` for *how* before changing code. If a
change would contradict either doc, update the doc in the same change or stop
and ask.

## What this project is

`multipath-wireguard` is a tiny, dependency-free Go UDP relay that duplicates WireGuard
packets across multiple `wstunnel` paths for redundancy and relays returns,
relying on WireGuard's own anti-replay window to discard duplicates. It is a
load-bearing network component: correctness and predictability matter more than
features or cleverness.

## Golden rules (do not violate without explicit sign-off)

1. **Standard library only.** No third-party modules. `go.mod` must have no
   `require` block. If you think you need a dependency, stop and ask — the
   answer is almost always "write the few lines instead."
2. **Never add de-duplication, parsing, decryption, or inspection of payloads.**
   The relay moves opaque datagrams. WireGuard dedups. This is load-bearing in
   the design, not an omission.
3. **Never bind to a network interface** (`SO_BINDTODEVICE`) or do anything that
   requires a Linux capability. No `CAP_NET_RAW`, no `CAP_NET_BIND_SERVICE`,
   high ports only. Staying unprivileged is a hard requirement (INTENT G3).
4. **Never block a read loop or hold a lock across a socket write.** A sick or
   dead path must never stall a healthy one. Sends are best-effort; per-packet
   write errors are dropped (rate-limited log), not propagated.
5. **No per-packet allocation on the hot path.** Read into reusable buffers.
   Don't introduce `make`/`append`/`fmt.Sprintf` inside the forwarding loops.
6. **Keep it small and readable.** If the binary grows past a few hundred lines
   of core logic, that's a smell — push back. One person should be able to read
   the whole packet path and reason about its failure modes.
7. **Fail fast on startup and on broken invariants; never on an expected runtime
   condition.** Validate everything reachable at startup — flags, listen/target
   addresses, every route in the routes file — and **refuse to start** with a
   clear, non-zero exit if anything is wrong, malformed, or ambiguous. Never
   launch in a degraded or half-configured state and "hope." If an internal
   invariant is violated at runtime (a state that should be impossible), treat
   it as fatal rather than limping on with corrupt state. **The carve-out is the
   packet hot path:** a route refusing packets, a peer not yet learned, or a
   transient send error are *expected, handled* conditions — they are the
   redundancy working, not failures, and must never exit the process or stall
   other paths (see rule 4). In short: fail fast when we *cannot possibly be
   doing the right thing*; keep going when *one path is down but others work*.
8. **No emoji. Anywhere.** Not in code, comments, log output, commit messages,
   PR/issue text, docs, or `docs-vibe/` entries. No decorative or symbolic
   glyphs used as ornament either. Standard typographic marks already in use are
   fine (box-drawing in diagrams, arrows like `→`, the section sign `§`); the
   ban is on emoji and emoji-like decoration, not on legitimate punctuation.
9. **Use the Nix toolchain. Don't assume a system Go.** The project is a Nix
   flake; `flake.nix` + `flake.lock` are the source of truth for the Go version
   and dev tools, and `flake.lock` is committed. Develop inside `nix develop`,
   build with `nix build`, validate with `nix flake check`. Do not introduce a
   build path that depends on tools installed outside the flake (no global
   `go`, no `Makefile` calling host binaries). The "standard library only" rule
   (rule 1) is about *Go* modules; flake *inputs* are separate, but keep them
   minimal and pinned, and never let the Go package build need a `vendorHash`
   (it stays `null` precisely because there are no Go dependencies).

## Toolchain (Nix)

Everything runs through the flake. The Go commands below are the same ones you'd
run by hand, but always inside the dev shell so the toolchain is the pinned one.

```bash
nix develop            # enter the dev shell (pinned Go + gopls + tools)
nix build              # build the package -> ./result/bin/multipath-wireguard
nix run . -- client …  # build and run in one step
nix flake check        # run the checks (build + tests); CI runs this
```

Inside `nix develop` (or via `nix develop -c <cmd>` for one-offs):

```bash
gofmt -l .            # must print nothing (formatting is enforced)
go vet ./...          # must pass clean
go build ./...        # must build
go test ./... -race   # unit + e2e, race detector on; must pass
```

Run locally inside the dev shell:

```bash
# client mode (fan-out to a static routes file)
go run . client -listen 127.0.0.1:51902 -routes ./routes.conf

# server mode (learns return paths, forwards to local WireGuard)
go run . server -listen 0.0.0.0:51900 -target 127.0.0.1:51820 -client-timeout 30s
```

There is a loopback end-to-end test (`e2e_test.go`) that stands up fake
WireGuard + fake tunnels entirely on localhost — no root, no real `wstunnel`,
no real WireGuard needed. **Run it after every change to the packet path.**

When you bump the Go version or a flake input, do it deliberately (`nix flake
update` or a targeted `nix flake lock --update-input <name>`), commit the
updated `flake.lock`, and confirm `nix flake check` still passes.

## Definition of done for any change

- [ ] `nix flake check` passes (build + `gofmt`/`vet`/`go test ./... -race`,
      including the e2e duplication/dedup test), and `nix build` produces the
      binary.
- [ ] No new Go module dependency (`go.mod` unchanged or only Go-version bumped;
      package `vendorHash` stays `null`). Any flake input change has the updated
      `flake.lock` committed.
- [ ] Hot path still allocation-free (if you touched it, say how you verified —
      e.g. a quick `-benchmem` benchmark or reasoning).
- [ ] If behavior or interfaces changed, `DESIGN.md` (and `INTENT.md` if scope
      changed) updated in the same commit.
- [ ] New behavior has a test. Bug fixes come with a regression test.
- [ ] A session development doc was written under `docs-vibe/` (see "Session
      development log" below).

## Session development log (required)

At the **end of every work session**, write a short development doc capturing
what happened, so the project's history is legible to the next person (or agent)
without re-reading the diff.

- **Location & name:** `docs-vibe/####_short-name.md`
  - `####` is a zero-padded, monotonically increasing sequence number. Use the
    next number after the highest existing file in `docs-vibe/` (e.g. if `0007_…`
    exists, the next is `0008_`). Never reuse or renumber existing files.
  - `short-name` is a brief kebab-case slug for the session's focus, e.g.
    `0008_server-route-table.md`, `0009_e2e-failover-test.md`.
- **One file per session.** Don't append to a previous session's file; create a
  new numbered one. These are an append-only log, not living docs.
- **This is separate from INTENT/DESIGN.** Those describe the current intended
  state and are *edited in place*; the `docs-vibe/` log is a chronological record
  and is *never* edited after the session that wrote it.

Use this template:

```markdown
# ####_short-name

- **Date:** YYYY-MM-DD
- **Status:** done | in-progress | blocked

## Intent
What the session set out to do, in one or two sentences (the user's ask).

## Implementation
What actually changed and why. Key decisions and trade-offs. Anything that
deviated from the original plan and the reason.

## Usage
How to exercise the new/changed behavior (commands, config, flags). Omit if
nothing user-facing changed.

## Status & follow-ups
What's done, what's left, known issues, and anything the next session should
pick up. Note any INTENT/DESIGN edits made this session.
```

Keep it short and factual — a few paragraphs, not an essay. If a session
produced no meaningful change (e.g. investigation only), still record it with a
note on what was learned, so dead ends aren't rediscovered later.

## Code conventions

- Standard `gofmt`; no custom formatting. Group imports stdlib-only.
- Errors: wrap with `fmt.Errorf("...: %w", err)` at setup boundaries (socket
  creation, config load). In the packet loops, **do not** return on transient
  write errors — count and drop.
- Logging: use `log/slog`. Structured key/values. The packet path logs only
  rate-limited or periodic summaries — never one line per packet.
- Concurrency: prefer the fixed goroutine layout in DESIGN §5. Shared state is
  `wgPeer` (atomic pointer, client) and the learned route table (`sync.RWMutex`,
  server). Snapshot under lock, send outside it.
- Fail-fast in practice (rule 7): do **all** validation at startup — parse and
  check every route, resolve and bind every socket — and exit non-zero with a
  clear message before entering the packet loops if anything is wrong. No
  `panic` in steady state; a violated impossible-state invariant may `panic`,
  but an expected runtime condition (dead route, unlearned peer, transient send
  error) is counted and dropped, never fatal.
- Keep `client.go` and `server.go` mode-specific; shared mechanics live in
  `relay.go`. Don't merge the two modes into one branchy function.

## Repository map

| File | Responsibility |
|------|----------------|
| `main.go` | flag parsing, mode dispatch, socket setup, signal handling, shutdown |
| `client.go` | client mode: inner reader, per-route sockets, `wgPeer` |
| `server.go` | server mode: outer reader, learned route table, janitor, inner reader |
| `relay.go` | shared buffers, counters, copy helpers |
| `config.go` | routes-file parser + flag wiring |
| `*_test.go` | unit tests + loopback e2e |
| `flake.nix` / `flake.lock` | Nix toolchain + package + checks; pinned, committed |
| `docs-vibe/####_short-name.md` | append-only per-session development log (see above) |

## Things that look like improvements but are not

- "Let's dedup to save the server some work." No — that's WireGuard's job and
  adds state we don't want (rule 2).
- "Let's bind each route to its interface for true path isolation." No — that
  reintroduces `CAP_NET_RAW` and the engarde problem we left behind (rule 3).
- "Let's add a YAML/TOML config." No — pulls in a dependency (rule 1). The
  newline routes file is intentional and makes "add a tunnel = add a line."
- "Let's pool/channel packets between goroutines for throughput." Measure first;
  the simple per-socket reader is usually faster and is definitely simpler. Don't
  add a channel hop to the hot path without a benchmark proving it helps.

## When you're unsure

Prefer the smallest change that satisfies INTENT's success criteria. If a
request conflicts with a golden rule or with INTENT/DESIGN, surface the conflict
explicitly rather than quietly working around it. Document decisions in the
relevant `.md`, not just in code comments.