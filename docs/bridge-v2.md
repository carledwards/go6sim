# go6sim Bridge Protocol — DESIGN v2

**Status:** draft. Supersedes [`bridge.md`](bridge.md) (v1.0). The
v1.0 doc + implementation remain in the tree as the historical
record of what we built first; once v2 lands, v1 is removed.

> v2 is a **shape pivot**, not a feature patch. The wire (JSON over
> NDJSON) and most of the queries survive; what changes is the
> **conversational model** — from synchronous RPC + a per-session
> execution runner to a **pub/sub event bus over a single shared
> simulator pump.** This is the right contract for the way go6sim
> is actually used: one authoritative machine in the middle, many
> clients on the edges (the TUI, the foxpro controller, MCP shims,
> future browser views, future hardware), all of which need to
> observe + influence — never replace — the simulator's execution.

---

## 1. Why v1 was wrong-shaped

v1 was modeled as "lift `instrument.Instrument` over the wire."
Each session got its own goroutine running `RunUntil` slices,
emitting events back to the client. That works for one client at a
time on a fresh Instrument — and only because there was no other
driver to fight with.

The moment a *shared* Instrument enters the picture — e.g. when
`cmd/6502-sim --serve` exposes the TUI's live machine — v1's model
breaks in three concrete ways:

1. **Two clock pumps.** The TUI's `app.Tick` advances the Backend;
   the session's runner advances the Backend. Concurrent
   `HalfStep()` calls on the single-threaded `interp` tear CPU state
   → PC ends up in uninitialised RAM → BRK storm.
2. **Ownership gymnastics.** We bolted a `remote.Active()` flag
   onto the TUI to lock out its keyboard when a controller attached,
   and gated the TUI's tick on the same flag. Every concurrent
   reader / writer of the Instrument now needs to know about the
   flag. Layers of papering over.
3. **Peripherals freeze.** `RunUntil` is CPU-only by design (the
   debugger semantics it serves require it). v1's runner called it
   without separately ticking the backplane, so display.Controller,
   VIA T1, and any future card with `bus.Ticker` stopped responding
   under remote control. Patched in v1.x; root cause unchanged.

All three symptoms have the same root: **v1 has two execution
pipelines** trying to drive the same machine.

## 2. The v2 contract, in one paragraph

There is exactly one **clock pump** per Instrument. The pump is the
sole driver of CPU + peripheral execution. Every other actor —
TUI keyboard, bridge clients (foxpro controller, MCP shim, browser),
future hardware bridges — talks to the pump in **one of two
modes**:

- **Subscriber** — receives updates by topic. Pure observation,
  no influence. Many can attach; none can affect each other.
- **Commander** — sends commands ("please run," "set bp at $E005",
  "step 30 half-cycles"). Commands are *suggestions*, drained
  into the pump's input channel and acted on by the pump on its
  next tick. The commander does not wait for execution to
  complete — execution feedback comes back via the same event
  stream the subscribers see.

Synchronous queries (`cpu.state`, `mem.peek`, `taps.read`, `bp.list`)
still exist — they're naturally request/response and don't fit the
event model. The pivot is about **execution**, not inspection.

## 3. Mental model

```
   ┌─── Subscribers ───┐                ┌─── Commanders ───┐
   │  foxpro TUI       │                │  foxpro TUI      │
   │  foxpro control   │   ◄── events ──┤  foxpro control  │
   │  MCP shim         │       (by      │  MCP shim        │
   │  browser learner  │       topic)   │  keyboard, etc.  │
   └─────────┬─────────┘                └────────┬─────────┘
             │                                   │ commands
             │ topic filter                      ▼
             │                          ┌────────────────────┐
             │                          │     Hub (per       │
             ▼                          │     Instrument)    │
   ┌─── Event stream ─────────────────► │                    │
   │  clock.tick                        │  commands ──┐      │
   │  instr.executed                    │  queued     │      │
   │  region.elided                     │             ▼      │
   │  bp.hit                            │           Pump     │
   │  tap.changed                       │          (goroutine│
   │  irq.taken                         │           — the    │
   │  clock.halt                        │           sole CPU │
   │  state.snapshot                    │           driver)  │
   └──────────────────────────────────  │             ▲      │
                                        │   bp set,   │      │
                                        │   speed,    │      │
                                        │   annotations      │
                                        └────────────────────┘
                                            ▲
                                            │ in-process or
                                            │ bridge.Loader
                                            ▼
                                       Instrument
                                       (Backplane, CPU,
                                        cards, drv)
```

Same actor can be both subscriber and commander on one connection.
They're independent permissions / interests, not exclusive roles.

## 4. Components

### Hub (one per Instrument)

The thing every actor talks to. Owns:

- The shared `Instrument` (CPU + cards + driver).
- A **command channel** — every commander writes to it; the pump
  drains it.
- A **subscription registry** — who's listening to which topics.
- A **broadcast mechanism** — pump emits an event; hub fans out to
  matching subscribers; backpressure is per-subscriber (slow
  consumers drop events for their topics, never block the pump).

### Pump (one goroutine per Hub)

The only mutator of the Instrument. Loop body:

1. Drain command channel into local state (bp set, speed, running
   flag, step budgets, etc.).
2. If "running" or "step budget > 0", advance one slice (CPU
   `HalfStep` + peripheral `bp.Tick`, in lockstep).
3. Emit events from this slice's observations to the hub:
   - `clock.tick` per half-cycle (high volume; topic that's
     usually unsubscribed).
   - `instr.executed` on SYNC boundaries (per opcode).
   - `region.elided` when an annotated region's exit is detected.
   - `bp.hit` when an armed breakpoint matches.
   - `irq.taken` when the CPU vectors.
   - `tap.changed` when a card's `Taps()` value diffs from prior
     slice (coalesced ≤ 60 Hz per topic per channel).
   - `clock.halt` when the pump stops (limit / bp / vector / client
     / parked / error).
   - `state.snapshot` on configurable cadence.
4. Sleep (or yield) to pace toward configured `SpeedHz`. Pacing
   logic lives here, where it has the full picture (cycles consumed
   this slice, wall time so far, target rate).

### Session (one per bridge connection)

A thin record: which topics this session subscribed to; a writer
that serialises frame emission to the connection; the queries it can
issue. **No execution goroutine.** No mutex around an Instrument it
"owns." Sessions are pipes, not engines.

### TUI keyboard (in-process commander)

The existing keyboard handler becomes a thin emitter of commands
into the same Hub command channel a remote session would use. Run /
Stop / Step / Reset all become commands. The pump processes them
identically to a remote commander's. **The `remote.Active()`
lockout is deleted** — no longer needed; there's only one pump.

## 5. Transport — unchanged from v1

- **Primary:** JSON-RPC 2.0 over WebSocket (and NDJSON-over-TCP for
  the local case, as v1's `cmd/6502-sim-serve` already does).
- **Secondary:** stdio for tests / scripts.
- All numeric machine values plain JSON; byte arrays base64;
  cycle counts ≤ 2⁵³.

Wire envelope is identical to v1 — JSON-RPC `{jsonrpc, id, method,
params}` for requests, `{result | error}` for responses, no-id
for notifications.

## 5a. Go interface — `bridge.Target`

For in-process consumers (the TUI's built-in Monitor, integration
tests, the future MCP shim) the protocol is also expressed as a Go
interface so the same consumer code drives both transports:

```go
// package bridge
type Target interface {
    // Queries
    CPUState() (CPUState, *Error)
    MemPeek(addr uint16, n int) ([]byte, *Error)
    BPList() (BPListResult, *Error)

    // Commands (synchronous ack)
    MemPoke(addr uint16, b []byte) (int, *Error)
    Step(n int) (ClockStepResult, *Error)
    Run(speedHz int) *Error
    Stop() (ClockStopResult, *Error)
    Reset() (CPUState, *Error)
    SetPC(pc uint16) (CPUState, *Error)
    BPSet(addr uint16) (BPSetResult, *Error)
    BPClear(id string) (BPClearResult, *Error)
    IRQ() *Error
    NMI() *Error

    // Escape hatch
    Call(method string, params any) (json.RawMessage, *Error)

    // Events
    Notifications() <-chan Notification
}
```

Two implementations ship:

| Implementation | Transport | Use case |
|---|---|---|
| `*bridgeclient.Client` | NDJSON over TCP | remote drivers (`cmd/6502-control`, MCP later, VS Code later) |
| `*bridge.HubDirect` | direct Go calls | in-process Monitor (`cmd/6502-sim`, `cmd/6502-wasm`) |

`HubDirect` wraps a `*bridge.Hub` and subscribes to the Hub's event
topics, marshaling payloads to `json.RawMessage` so the
`Notification` shape matches the wire. Consumers cast the params
once and don't know which transport they're on.

Compile-time `var _ Target = (*Foo)(nil)` assertions in each
implementation file ensure new methods added to `Target` break the
build until each consumer grows the matching method — that's the
discipline this interface exists to enforce.

## 6. Lifecycle

```
client                                    hub
TCP / WS connect ─────────────────────► accept; session created

← hello { protocols: ["2.0"] }
                                          mark initialised
                                  ◄────── result { serverVersion, protocol: "2.0", capabilities }

← image.describe   (optional)
                                          (the hub's bound preset + regions)
                                  ◄────── result { preset, label, summary, regions[] }

← image.load { bytes_b64, annotations? }   (optional — only if client wants to ship a fresh program)
                                          pump receives "new image"; ROM rewrite + reset;
                                          annotations stored
                                  ◄────── result { ok: true }

← events.subscribe { topics: ["instr.executed", "clock.halt", "region.elided"] }
                                          register topic interest
                                  ◄────── result { subscribed: [...] }

← bp.set { addr: 0xE100 }
                                          queued command → pump arms bp
                                  ◄────── result { id: "bp_1" }

← clock.run                          ←── ─── command (fire-and-forget; just an ack)
                                  ◄────── result {}

                                          pump goroutine begins ticking:
                                  ◄────── instr.executed (per-opcode events for subscribed clients)
                                  ◄────── region.elided  (when entering @nodebug region; one event for whole block)
                                  ◄────── tap.changed    (when a tap diffs, coalesced)
                                  ◄────── ...

                                          eventually: bp at $E100 fires
                                  ◄────── bp.hit { id: "bp_1", addr: $E100, state }
                                  ◄────── clock.halt { reason: "breakpoint", ... }

← cpu.state                              (synchronous query — request/response)
                                  ◄────── result { CPUState }

close ──────────────────────────────►   session disposed; pump unaffected,
                                        other subscribers continue.
```

The notable shift from v1: **`clock.run` returns instantly**. The
execution feedback comes back through the topic stream the client
subscribed to. The same conversation works whether the pump is also
serving the TUI keyboard, two other clients, or nothing at all.

## 7. Topics catalog (events flowing sim → client)

| Topic | Body | Volume | When emitted | Status |
|---|---|---|---|---|
| `state.snapshot` | `{ state, taps? }` | ~10 Hz while running + edges | periodic during run; edge on CmdRun/Step; on CmdSetPC; on CmdIRQ/NMI | **live** |
| `bp.hit` | `{ id, addr, state }` | rare | armed bp matches at SYNC | **live** |
| `clock.halt` | `{ reason, addr, state, bpId? }` | once per stop | pump stops (any reason — bp/vector/client/step/limit/reset) | **live** |
| `clock.tick` | `{ halfCycles }` | very high | per half-cycle; usually unsubscribed | phase-B |
| `instr.executed` | `{ pc, op, halfCycles, state? }` | medium | per opcode completion (SYNC boundary) | phase-B |
| `region.elided` | `{ name, ticks, exitPC }` | rare | exit of an `@nodebug`-annotated region | phase-E |
| `irq.taken` | `{ vector: "irq"\|"nmi"\|"reset", state }` | rare | CPU vectoring | phase-B |
| `tap.changed` | `{ name, value, halfCycles }` | coalesced ≤ 60 Hz/channel | tap value differs from prior slice | phase-B (subscription accepted; emission pending) |
| `image.amended` | `{ source }` | rare | a commander pushed new bytes / annotations | phase-E |
| `bus.error` | `{ addr, kind }` | rare | a write to read-only ROM / undefined region | phase-B |

`state.snapshot` is the "I don't want to subscribe to a firehose;
give me a periodic full picture" topic. Subscription params include
its cadence (`intervalMs` or `everyHalves`).

`region.elided` is the keystone of the teaching story (§10).

## 8. Commands catalog (client → sim, fire-and-forget)

All commands return a small `{ok: true}` ack synchronously; their
**effect** is observed via topics. Some commands also accept
parameters that affect future behavior (set bp, set speed).

| Method | Params | Effect |
|---|---|---|
| `clock.run` | `{ speedHz?, until_halfcycles? }` | pump starts; halts when limit / bp / vector hit |
| `clock.stop` | `{}` | pump stops at next slice boundary (PC aligned to next SYNC); `clock.halt { reason: "client" }` emitted |
| `clock.step` | `{ kind?: "instruction"\|"halfcycle" = "instruction", n?: int = 1 }` | pump runs N of the given granularity; `clock.halt { reason: "step" }` emitted |
| `clock.setSpeed` | `{ hz: int }` | pump's pacing target; applies on next slice |
| `cpu.setPC` | `{ pc }` | overwrite PC; backend must implement `cpu.PCSetter` (interp does, netsim doesn't) — errors otherwise; emits a `state.snapshot` |
| `cpu.irq` | `{}` | pulse the host-driven IRQ line; auto-deasserts at next slice / step. CPU services on next SYNC if I=0 (correct 6502 mask behaviour); silently ignored if masked |
| `cpu.nmi` | `{}` | pulse the host-driven NMI line; edge-triggered; ignores I flag. **interp only** — netsim CPU has no `SetNMI` upstream |
| `bp.set` | `{ addr, kind?: "addr" = "addr" }` | arms an addr breakpoint (returns id in the ack) |
| `bp.setVector` | `{ vector: "reset"\|"nmi"\|"irq" }` | arms vector-take break (returns id) |
| `bp.clear` | `{ id? }` | clears one or all bps |
| `machine.reset` | `{}` | shallow reset — re-fetch reset vector; halts CPU; emits `clock.halt { reason: "reset" }` |
| `image.load` | `{ bytes_b64, origin?, annotations? }` | pump halts, ROM written, reset; `image.amended` emitted |
| `image.amend` | `{ annotations }` | replace just the annotation metadata; pump consults on next entry |
| `mem.poke` | `{ addr, bytes_b64 }` | traced write; valid at any time |
| `events.subscribe` | `{ topics: [...], stateIntervalMs?, tapMaxHz? }` | session-scoped subscription update |
| `events.unsubscribe` | `{ topics?: [...] }` | remove some/all subscriptions |

Note no `clock.runUntil` and no `clock.advance` in v2's commands —
both fold into `clock.run { until_halfcycles }` and disappear as
separate concepts.

## 9. Queries (client → sim, request/response, unchanged spirit from v1)

Synchronous, returning the requested data. Always safe — never
mutate the pump or session state.

| Method | Returns |
|---|---|
| `hello` | `{ serverVersion, protocol, capabilities }` |
| `image.describe` | `{ preset, label, summary, regions }` |
| `cpu.state` | `{ state }` |
| `mem.peek` | `{ bytes_b64 }` |
| `mem.read` | `{ bytes_b64, addr, length }` (named region) |
| `mem.frame` | `{ bytes_b64, addr, length }` (sugar for framebuffer region) |
| `taps.list` | `{ taps[] }` |
| `taps.read` | `{ values }` |
| `bp.list` | `{ breakpoints[] }` |
| `pump.status` | `{ running, speedHz, slice, lastHaltReason, subscribers }` |

`pump.status` is new — a one-shot snapshot of "what is the pump
doing right now," useful for clients connecting to a live machine
that's already running.

## 10. Region elision — the `@nodebug` hook

This is the v2-specific affordance the rest of the design serves:
**the simulator can elide cycle-burning regions and emit a single
summary event in their place.**

### The author's interface

In go6asm source:

```
        LDX #$FF
@delay: DEX           ; @nodebug delay-x   <─ region tag, lexed but not assembled
        BNE @delay
        ; @endnodebug
        ...
```

(Syntax TBD with go6asm — the spec here just claims a contract.)
go6asm emits, alongside the image, an annotations table:

```json
{
  "annotations": [
    { "kind": "nodebug", "name": "delay-x", "lo": 0xE003, "hi": 0xE008 }
  ]
}
```

### The simulator's behavior

The pump consults the annotations table on each instruction step.
When PC enters a `nodebug` region:

1. Suppress `clock.tick` and `instr.executed` topic emission inside
   the region (other topics — `bp.hit`, `irq.taken`, `tap.changed`
   — continue to fire; the region still *executes*, it just doesn't
   narrate).
2. Continue stepping until PC exits the region.
3. Emit one `region.elided { name: "delay-x", ticks: 1280, exitPC: ... }`.
4. Resume normal emission.

A subscriber to `instr.executed + region.elided` watches the
program "skip" the delay — clean narrative.

A subscriber that also wants the cycle-level reality just opts into
`clock.tick`; nothing is hidden, the events are still there in the
firehose.

### Why this lives on the sim side

- **Authority** — the simulator has the program, the annotations,
  and the live PC. No client could do this without a back-channel.
- **Once written, every client benefits** — VS Code, foxpro
  controller, MCP shim all see `region.elided` without
  re-implementing the elision logic.
- **The author controls the narrative** — `@nodebug` is a source
  annotation, not a client preference. The teaching intent is
  baked into the program.

### What v2 ships vs what comes later

- v2 **defines** `region.elided` as a topic + `image.amend` /
  `image.load` carrying an `annotations` array.
- v2 **implements** the pump-side elision logic.
- The **go6asm `@nodebug` syntax** is a separate piece of work in
  go6asm; the bridge just consumes whatever annotations metadata
  the client supplies. A client can compose annotations manually
  for testing before go6asm support lands.

## 11. Encoding — unchanged

Same as v1 §7: base64 for byte arrays, plain JSON numbers for
addresses/values, JSON-safe integers for cycle counts, `CPUState`
canonical shape, region map carried by `image.describe`.

## 12. Capability negotiation

`hello` returns a capabilities array. v2 advertises feature names
both clients and server can check. New v2 caps:

- `"events.topics"` — pub/sub topic surface (the heart of v2)
- `"region.elided"` — pump implements elision (server-side
  promise; clients can check before subscribing)
- `"step.granularity"` — `clock.step` honours `kind: halfcycle`
- `"pump.status"` — the live-status query is implemented

v1 caps that survive: `"breakpoints"`, `"taps"`, `"frame"`.

## 13. Error model — same codes as v1 §6

Two new codes for the v2 model:

| Code | Name | When |
|---|---|---|
| `-32008` | `TopicUnknown` | subscribe/unsubscribe named a topic the server doesn't advertise |
| `-32009` | `AnnotationsReject` | `image.amend`/`load` annotations malformed or out-of-range |

`CodeNotInRun` from v1 mostly disappears — `clock.run` is
fire-and-forget; calling it while running just returns ok (it's a
no-op + the existing pump keeps going). `clock.stop` while idle is
similarly a no-op (returns ok, no halt event emitted).

## 14. Security — unchanged

Loopback default; `--insecure-bind` to escape it; no auth in this
revision; token auth deferred.

## 15. Resolved decisions

These are the v2-specific forks, resolved 2026-05-20:

1. **One pump per Instrument.** Hard rule. No per-session execution
   goroutine. ✓
2. **Commands are fire-and-forget**, observed via the event stream.
   Synchronous queries kept for inspection (cpu.state, mem.*,
   bp.list, taps.*). ✓
3. **Topics are first-class.** Subscription is per-topic with
   optional per-topic params (cadence, max Hz). ✓
4. **`region.elided` is a topic.** The simulator decides what to
   elide based on annotations, not the client. ✓
5. **TUI keyboard is a commander.** No special path; routes
   through the same Hub. ✓
6. **Multiple simultaneous bridge clients allowed.** Each is its
   own session with its own subscriptions; the Hub fans out. ✓
7. **`clock.tick` exists but is rarely subscribed.** Producing it
   is cheap; the cost is the wire emission, controlled by
   subscription. ✓
8. **No `clock.runUntil` / `clock.advance`** commands. Both fold
   into `clock.run { until_halfcycles }`. ✓

## 16. Implementation roadmap

Phased so each step is testable independently:

| Phase | What | Survives unchanged |
|---|---|---|
| **A** | Hub + Pump skeleton in `bridge/`. One pump goroutine, command channel, broadcast registry. No topics yet — just `clock.halt`. Headless `cmd/6502-sim-serve` migrates to use it. | wire, lifecycle, queries, error model |
| **B** | Topic surface — `instr.executed`, `bp.hit`, `irq.taken`, `tap.changed`, `state.snapshot`. Per-topic subscription on `events.subscribe`. | A |
| **C** | TUI integration — `cmd/6502-sim --serve` retires its own tick-drive in the pump path. Keyboard becomes a commander. `remote.Active()` deleted. | A, B |
| **D** | Controller refactor — `cmd/6502-control` becomes a subscriber + commander. Buttons fire commands; UI mirrors update from `state.snapshot` + `instr.executed`. | A, B, C |
| **E** | `region.elided` — pump consults annotations, emits region events. `image.amend` accepts annotations. | A–D |
| **F** | go6asm `@nodebug` syntax — assembler-side, not this protocol. | unrelated |

Between A and B, the bridge tests are updated to the new shape.
Between C and D, the user's two-foxpro-TUIs vision actually works
without races, because there's one pump and the controller
genuinely is just a subscriber + commander.

---

*Companion docs:* see [`architecture-backplane.md`](architecture-backplane.md)
for the in-process Backplane/Instrument carve this bridge wraps,
[`systems.md`](systems.md) for the systems map (the v2 architecture
will need a refresh once Phase A lands), and the v1 doc
[`bridge.md`](bridge.md) which v2 supersedes.
