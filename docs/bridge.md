# go6sim Bridge Protocol — DESIGN v1.0

**Status:** **superseded by [`bridge-v2.md`](bridge-v2.md)** (drafted
2026-05-20). The v1.0 contract was implemented and shipped behind
`bridge/`, `cmd/6502-sim-serve`, and `internal/bridgeclient`, but
proved to be the wrong shape once a *shared* Instrument entered the
picture (`cmd/6502-sim --serve`): the per-session execution runner
fights the TUI's own clock pump. v2 pivots to a pub/sub model with
one shared pump per Instrument; see that doc for the new contract.
This document remains as the historical record and will be removed
when v2 is implemented.

---

**Status (original):** draft — design decisions locked 2026-05-19; ready to
implement. Source-of-truth design for the network surface that
exposes `instrument.Instrument` to external clients (VS Code, MCP,
future hardware bridges, any other editor).

> This document is the *shape* contract: methods, params, returns,
> events, errors, encodings. The Go implementation lives in the new
> `go6sim/bridge` package (TBD) and the `cmd/6502-sim-serve` binary.
> Every other client surface (the TUI's in-proc instrument, the
> `cmd/6502-core-wasm` browser embed, the future MCP server, a VS Code
> extension, a U64 remote backend) is — by rule — a thin client or
> backend of *this* contract. There is only one bridge surface.

---

## 1. Goals

- **One surface, many clients.** VS Code, MCP, a future browser editor,
  a future hardware bridge — all speak the same shape so we never write
  the same method twice with subtle drift.
- **Faithful to `instrument.Instrument`.** Don't invent a new control
  model; expose the existing one (load / reset / step / run / state /
  peek / poke / mem / taps / breakpoints / run-until) over a wire.
- **Deterministic & inspectable.** Every operation that mutates the
  machine returns enough state for the client to keep its view in sync
  without polling.
- **Simple to host.** A tiny `go6sim serve --addr 127.0.0.1:NNNN` is
  the whole installation. No daemons, no auth dance for the local
  default.
- **Hard backstops carried forward.** `GOWORK=off`-buildable, no
  syscall/js leak into the bridge package, no time.Now in the
  authoritative core, byte-equivalence to in-proc usage.

## 2. Non-goals (v1)

- **Multi-session/multi-machine per connection.** v1 = exactly one
  loaded machine per WebSocket connection. Multi-session is a v2
  envelope.
- **Source-level debugging.** v1 ships addresses, not source lines.
  Layered later: clients map addresses to source via the go6asm
  `.lbl` / symbol JSON sidecars they already have.
- **Authentication beyond localhost.** v1 binds 127.0.0.1 by default
  and refuses non-loopback unless `--insecure-bind` is passed. No token
  mechanism yet. Add when there's a real remote consumer.
- **A custom assembler protocol.** Assembly stays go6asm's concern.
  Clients build images themselves (via the go6asm wasm or CLI) and
  load the bytes through this bridge.

## 3. Transport

- **Primary:** **JSON-RPC 2.0 over WebSocket**, framed by WS messages
  (one JSON object per frame). One connection = one machine session.
- **Secondary (tests / scripts):** **JSON-RPC 2.0 over stdio** of a
  `go6sim serve --stdio` invocation. Same envelopes, line-delimited
  JSON (one object per `\n`).
- **Content-Type:** `application/json`, UTF-8.
- All numeric machine values (addresses, byte values, cycle counts) are
  unsigned JSON numbers. Addresses fit in 16 bits; cycle counts use
  JSON's safe integer range (≤ 2⁵³) — see §7.

Why JSON-RPC 2.0: native notifications, native batch, well-known error
envelope, libraries exist in every language we care about (TS, Go,
Python). No bespoke envelope to maintain.

## 4. Session lifecycle

1. Client opens a WebSocket to `ws://127.0.0.1:NNNN/`.
2. Client calls `hello` (§5.1). Server replies with version +
   capabilities. No further calls accepted until `hello` succeeds.
3. Client calls `machine.load` with a preset name (e.g. `teach-min`,
   `teach-merlin`) and optionally an image to write into RAM/ROM. The
   server constructs the machine and resets it.
4. Client drives the machine via control / inspection / debug methods,
   optionally subscribing to events (§5.5).
5. Client closes the WebSocket; server destroys the session.

Failure to call `hello` before any other method → error
`-32000 NotInitialised`.

## 5. Method catalog

All methods are JSON-RPC 2.0 requests. Notifications (server → client)
have no `id` and use the dotted names listed in §5.5.

### 5.1 Lifecycle

| Method | Params | Returns | Notes |
|---|---|---|---|
| `hello` | `{ clientName, clientVersion, protocols?: ["1.0"] }` | `{ serverVersion, protocol: "1.0", capabilities: string[] }` | First call, mandatory. `capabilities` advertises optional features (e.g. `"netsim-cpu"`, `"breakpoints"`, `"taps"`, `"frame"`). |
| `machine.list` | `{}` | `{ presets: [{ name, summary }] }` | The presets the server can construct (e.g. `teach-min`, `teach-merlin`, `vic-demo`). |
| `machine.load` | `{ preset, image?: { bytes_b64, origin } }` | `{ session, preset, regions: Region[] }` | Builds the named preset, resets it, optionally loads an image (else the preset's ROM stays as authored). `regions` describes the live memory map (see §7). |
| `machine.reset` | `{}` | `{ state: CPUState }` | Pulse reset; CPU returns to its reset-vector PC. |
| `machine.unload` | `{}` | `{}` | Tear down the current session machine. Subsequent calls (other than `hello`/`machine.load`) → `NoMachine`. |

### 5.2 Control (the clock)

| Method | Params | Returns | Notes |
|---|---|---|---|
| `clock.step` | `{ n?: int = 1 }` | `{ state: CPUState }` | Step `n` instructions. Synchronous; small. |
| `clock.stepCycle` | `{ halves?: int = 1 }` | `{ state: CPUState }` | Step `halves` half-cycles. Sub-instruction granularity for tap/IRQ work. |
| `clock.run` | `{ speedHz?: int }` | `{}` | Start the run loop in the background. Server pushes `clock.halt` (§5.5) when it stops (parked/break/error/explicit stop). `speedHz` updates the clock driver target if given. |
| `clock.stop` | `{}` | `{ state: CPUState, reason: string }` | Halt the run loop now. `reason` = `"client"`. |
| `clock.setSpeedHz` | `{ hz: int }` | `{ hz: int }` | Set the clock target. `0` = "as fast as host can run." |
| `clock.advance` | `{ durationUs: int }` | `{ halfCycles: int, state: CPUState }` | Advance virtual time by `durationUs` microseconds. Synchronous. Mirrors `Instrument.Advance` (the WASM/RAF semantics). |
| `clock.running` | `{}` | `{ running: bool, speedHz: int }` | Read clock state without mutating. |

### 5.3 Inspection

| Method | Params | Returns | Notes |
|---|---|---|---|
| `cpu.state` | `{}` | `CPUState` | The CPU registers + cycle counter. See §7. |
| `mem.peek` | `{ addr: u16, n?: int = 1 }` | `{ bytes_b64 }` | Read `n` bytes starting at `addr`. **Untraced** (won't fire taps). |
| `mem.poke` | `{ addr: u16, bytes_b64 }` | `{ written: int }` | Write `bytes_b64` starting at `addr`. **Traced** (does fire taps / device side-effects). |
| `mem.read` | `{ region: string }` | `{ bytes_b64, addr: u16, length: int }` | Read a *named* region from the live memory map (e.g. `"framebuffer"`, `"zero-page"`, `"stack"`). Region names come from `machine.load`'s `regions`. |
| `mem.frame` | `{}` | `{ bytes_b64, addr: u16, length: int }` | Convenience for the `"framebuffer"` region (whatever the active preset designates). |
| `taps.list` | `{}` | `{ taps: { name: string, summary: string }[] }` | All taps the loaded machine exposes (aggregated `<card>.<tap>` + CPU taps), per the backplane §B9a contract. |
| `taps.read` | `{ names?: string[] }` | `{ values: { [name: string]: any } }` | Read tap values now. Empty `names` = read all. |

### 5.4 Debugging

| Method | Params | Returns | Notes |
|---|---|---|---|
| `bp.set` | `{ addr: u16, kind?: "addr" = "addr" }` | `{ id: string, addr: u16 }` | Set an address breakpoint (SYNC-boundary). Returns a stable id for later removal. |
| `bp.setVector` | `{ vector: "reset" \| "nmi" \| "irq" }` | `{ id: string, vector }` | Break-on-vector (§B9a tap-driven). Stops the run when the CPU vectors. |
| `bp.clear` | `{ id?: string }` | `{ cleared: int }` | Clear one (`id`) or all (no `id`) breakpoints. |
| `bp.list` | `{}` | `{ breakpoints: Breakpoint[] }` | All active breakpoints. |
| `clock.runUntil` | `{ maxHalfCycles: int }` | `RunResult` | Run synchronously up to `maxHalfCycles` half-cycles, stopping on any active breakpoint, vector break, or limit. The deterministic per-half-step engine from `Instrument.RunUntil`. |

`RunResult` (returned by `clock.runUntil` and also emitted as the body
of the `clock.halt` notification when an asynchronous run stops):

```json
{
  "halfCycles": 12340,
  "reason": "breakpoint" | "vector" | "limit" | "parked" | "client" | "error",
  "addr": 57344,
  "bpId": "bp_1",
  "state": { ...CPUState... }
}
```

### 5.5 Server-pushed events (notifications)

Notifications have no `id` and are sent unsolicited when the client has
subscribed.

| Method | Body | When |
|---|---|---|
| `clock.halt` | `RunResult` | Async run loop stopped. |
| `bp.hit` | `{ id, addr, state }` | A breakpoint was hit during an async run. (Also encoded in the `clock.halt` `RunResult.reason`/`bpId` — `bp.hit` fires first, then `clock.halt`.) |
| `tap.changed` | `{ name, value, halfCycles }` | A subscribed tap changed value. Subject to throttling (§7). |
| `state.snapshot` | `{ state, taps?: {...} }` | Periodic state push when subscribed via `state.subscribe`. |

Subscription methods:

| Method | Params | Returns |
|---|---|---|
| `events.subscribe` | `{ channels: string[] }` | `{ subscribed: string[] }` |
| `events.unsubscribe` | `{ channels?: string[] }` | `{ unsubscribed: string[] }` |

Channels:
- `clock.halt` — async-run stop reasons.
- `bp.hit` — breakpoint fires.
- `tap.<name>` — a specific tap (e.g. `tap.via.t1`, `tap.cpu.irq`).
  `tap.*` subscribes to all.
- `state` — periodic snapshots (every N half-cycles or every M ms;
  configured via `events.subscribe`'s `state.intervalMs` / `state.everyHalves`).

## 6. Error model

JSON-RPC 2.0 errors. Reserved codes:

| Code | Name | When |
|---|---|---|
| `-32700` | ParseError | (JSON-RPC standard) |
| `-32600` | InvalidRequest | (JSON-RPC standard) |
| `-32601` | MethodNotFound | (JSON-RPC standard) |
| `-32602` | InvalidParams | (JSON-RPC standard) |
| `-32603` | InternalError | Unexpected server error. |
| `-32000` | NotInitialised | A method other than `hello` called before `hello`. |
| `-32001` | NoMachine | Machine-bound method called with no `machine.load`. |
| `-32002` | UnknownPreset | `machine.load` named a preset the server doesn't know. |
| `-32003` | BusError | Bad addr (e.g. region not present). |
| `-32004` | ImageReject | `image.bytes_b64` too large / overlaps protected region. |
| `-32005` | UnknownBreakpoint | `bp.clear { id }` for unknown id. |
| `-32006` | CapabilityMissing | Method requires a capability the server didn't advertise. |
| `-32007` | NotInRun / NotIdle | State-machine misuse (e.g. `clock.run` when already running). |

All error bodies include `{ code, message, data? }` per JSON-RPC. `data`
carries a structured detail when relevant (e.g. the rejected addr).

## 7. Encoding conventions

- **Addresses & byte values:** plain JSON unsigned numbers. `addr` is
  16-bit; `byte` is 8-bit. The server validates and rejects with
  `InvalidParams`.
- **Byte arrays:** **base64** (`"bytes_b64"`). Not hex (large frames /
  ROMs would bloat). Decoded length must match `n`/`length` where the
  call specifies one.
- **Cycle counts:** unsigned JSON number, safe to 2⁵³ — enough for
  ~100 years of 1 MHz cycles. No bigint hack needed.
- **`CPUState`** (canonical form for every method that returns it):

  ```json
  {
    "a": 0, "x": 0, "y": 0, "sp": 253, "p": 36,
    "pc": 57344,
    "halfCycles": 0,
    "running": false,
    "interrupts": { "irqAsserted": false }
  }
  ```

- **`Region`** (returned by `machine.load`):

  ```json
  { "name": "ram", "lo": 0, "hi": 8191, "readOnly": false }
  ```

  Special names recognised by `mem.read` / `mem.frame`:
  `"zero-page"`, `"stack"`, `"ram"`, `"framebuffer"`, `"rom"`. Presets
  may name additional regions.
- **Tap throttling:** the server coalesces `tap.changed` notifications
  per channel to at most `tapMaxHz` per second (default 60), to keep a
  busy bus from flooding the wire. Clients that need every transition
  use `clock.runUntil` + explicit polling instead.

## 8. Versioning & capabilities

- The wire protocol carries a version string (`"1.0"`). The server
  advertises its supported versions in `hello`; the client picks one or
  closes.
- Methods that need an optional feature gate on a capability name
  (`"taps"`, `"breakpoints"`, `"netsim-cpu"`, `"frame"`, …). Calling a
  capability-gated method without the cap returns `CapabilityMissing`.
- Adding a method = bump *minor* version. Removing or breaking the
  shape of an existing method = bump *major*. The bridge is committed
  to keeping a major version's surface stable.

## 9. Security

- Default bind: `127.0.0.1` (loopback only). Refuses other binds
  unless `--insecure-bind` is passed.
- No auth in v1. The local-only assumption is the protection.
- v2 will add a `--token` option and a `hello { token }` field
  validated server-side. Notes are out of scope for this doc.

## 10. Resolved decisions (locked 2026-05-19)

These were the open forks heading into implementation. All eight
resolved — listed below for the record so anyone reading later sees
what was considered and why we chose what we chose.

1. **Streaming run output vs. polled state.** Right now `clock.run` is
   "go background, push `clock.halt` on stop." Should `state.snapshot`
   also auto-fire during a run (configurable cadence)?
   **Resolved: yes — only when the client has subscribed to the
   `state` channel.** Default cadence is configurable on
   `events.subscribe` (`state.intervalMs` or `state.everyHalves`); no
   push if no subscription.
2. **Memory writes during run.** Should `mem.poke` work while the
   clock is running?
   **Resolved: yes.** Atomic per-call, traced (devices see it),
   matches debugger expectations.
3. **`mem.peek` over an active run loop.**
   **Resolved: yes.** Untraced read; safe to interleave.
4. **Single-machine restriction.**
   **Resolved: v1 = one machine per connection.** Multi-session is a
   v2 envelope (sessions identified by a `session` field in every
   request). Not now.
5. **Symbol/label upload.** Push the go6asm-emitted symbol table so
   notifications can decorate `"addr": 0xE000` with `"label": "main"`?
   **Resolved: yes, optional.** Method `machine.symbols { table }`;
   server stores and decorates events. Gated by capability
   `"symbols"`.
6. **Image format.**
   **Resolved: raw bytes at an `origin`, not `.o6`.** Linking stays
   go6asm's job — the client links and ships the raw image. Keeps
   the bridge package dep-free of go6asm (one of the carve
   invariants).
7. **Headless vs. TUI in one process.**
   **Resolved: both.** A dedicated `cmd/6502-sim-serve` *and* a
   `--serve <addr>` flag on `cmd/6502-sim` (TUI), both delegating to
   the same `bridge` package. Same `Instrument` shared by the TUI and
   any connected client when run alongside.
8. **Authentication for non-loopback.**
   **Resolved: deferred to v2.** v1 binds `127.0.0.1`, refuses other
   binds unless `--insecure-bind`. No token mechanism shipped yet;
   add when a real remote consumer arrives.

## 11. Implementation roadmap (sketch, not part of the contract)

- **Step 1.** `go6sim/bridge` package — typed Go interfaces matching
  this catalog, JSON-RPC handler over `io.ReadWriter`, zero net
  dependency itself.
- **Step 2.** `cmd/6502-sim-serve` — thin binary, opens a WebSocket
  listener and a `bridge.Handler` per connection, instantiates an
  `instrument.Instrument` per session.
- **Step 3.** `cmd/6502-sim --serve <addr>` flag — same bridge handler
  attached alongside the TUI, sharing one Instrument.
- **Step 4.** Conformance tests against an in-proc bridge client
  (`bridge/bridge_test.go`) — every method, every error code,
  notification ordering for breakpoints + halts.
- **Step 5.** Publish `bridge/schema.json` (generated from the Go
  types) so non-Go clients (VS Code TS, MCP) consume the same shape
  without redefining it.

---

*Companion docs:* see `docs/architecture-backplane.md` for the
`instrument.Instrument` carve this bridge wraps, and `docs/learn-path.md`
for the curriculum the bridge will eventually serve.
