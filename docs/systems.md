# go6sim — Systems Map

**Status:** drafted 2026-05-20. Front-door doc for the project. Read this
first; then drop into `architecture-backplane.md` if you're touching
the in-process layer, or `bridge.md` if you're writing a client.

> This document does not introduce new design — it consolidates how the
> existing pieces fit together so a new contributor (or future-you)
> can land in the right doc the first time.

---

## One seam, many wrappers

There is exactly **one machine surface**, and everything else wraps it:

```
                         what the world talks to
                                  │
                                  ▼
                      ┌───────────────────────┐
                      │ instrument.Instrument │  load / reset / step / run /
                      │                       │  state / peek / poke / mem /
                      └───────────┬───────────┘  taps / breakpoints / runUntil
                                  │
                           what it owns
                                  ▼
            ┌─────────────────────┴───────────────────────┐
            │  Backplane (the bus)                        │
            │   ├─ CPU (interp; netsim optional)          │
            │   ├─ RAM, ROM                               │
            │   ├─ VIA(s)  — one or two, per preset       │
            │   └─ framebuffer RAM (teach-min only)       │
            │  Clock.Driver  (run / step / speed)         │
            └─────────────────────────────────────────────┘
```

The `instrument.Instrument` type is the **north star**. Every UI, every
adapter, every client process eventually drives this surface — either
in-process (TUI, WASM) or over a wire (TCP bridge). When you find
yourself adding a new way to talk to the simulator, the first question
is: *how does this reach an Instrument?*

For the in-process detail (Backplane, cards, clock driver), see
`architecture-backplane.md`. For the over-the-wire detail (method
catalog, params, errors, events), see `bridge.md`.

## Three deployment shapes that exist today

```
1) TUI                         2) Browser (WASM)              3) TCP bridge
   no comms                       no comms                      real wire

   ┌───────────────┐             ┌───────────────┐             ┌─────────┐
   │ cmd/6502-sim  │             │ <CodeLab>     │             │ client  │
   │  tcell UI     │             │   JS / DOM    │             │ (you,   │
   │      │        │             │      │        │             │  nc,    │
   │      ▼        │             │   syscall/js  │             │  MCP,   │
   │  Instrument   │             │      │        │             │  VS Cd) │
   └───────────────┘             │      ▼        │             └────┬────┘
   one process                   │  cmd/6502-    │                  │
                                 │  core-wasm    │       NDJSON / TCP
                                 │  Instrument   │                  │
                                 └───────────────┘             ┌────▼─────────────┐
                                 one tab                       │ 6502-sim-serve   │
                                                               │  ndjsonConn      │
                                                               │  bridge.Server   │
                                                               │  presetLoader    │
                                                               │  Instrument      │
                                                               │  ── one per ──   │
                                                               │   connection     │
                                                               └──────────────────┘
                                                               one process,
                                                               one sim per conn
```

The **only** shape with a wire is #3. The other two share an address
space with the Instrument — tcell drawing on a terminal, or JS calling
Go functions through `syscall/js`. Same Go code under all three; the
process boundary is the only thing that moves.

## What flows, what stays

|                                       | TUI            | Browser (WASM)        | TCP bridge                              |
|---                                    |---             |---                    |---                                      |
| Where the 6502 state lives            | in-process     | in-process (WASM heap)| in the **server** process               |
| Where the program bytes come from     | loaded in-process | go6asm.wasm in-page | sent over the wire (base64 in `machine.load`) |
| How "step / run / stop" is requested  | keyboard       | JS function call      | JSON-RPC request frame                  |
| How the UI sees state                 | direct call    | direct call           | `cpu.state` / `mem.peek` requests, or subscribed `state.snapshot` notifications |
| Per-message data sizes                | n/a            | n/a                   | ~80 B (regs), ~150 B (regs+taps), ~11 KB max (whole-RAM read) |

The bridge is **not** streaming pixels or executing instructions over
the wire. The wire carries **control + selective observation**. The
simulator stays in the server; the client drives it and asks
questions.

## Work distribution

| Always on the server                 | Always on the client                |
|---                                   |---                                  |
| Executing CPU cycles                 | Composing JSON requests             |
| Diffing taps per slice               | Decoding JSON responses             |
| Checking breakpoints at SYNC         | Routing responses by id             |
| Buffering subscription state         | Routing notifications by method     |
| Marshalling responses + notifications| Anything visual / UI / interpretation |

**The simulator never decides anything strategic.** It does what it's
told: step N, run until you say stop, write these bytes here. The
client owns intent — which preset, what program, when to start, where
to put breakpoints, what to display.

## A typical session, top to bottom

A full conversation over the TCP bridge, exercising the surface that
matters in practice. (The same vocabulary, used differently, runs the
TUI and the WASM embed.)

```
client                                    server
TCP connect ────────────────────────►     accept; sess = {initialised=0, inst=nil, sub={}}

← hello {protocols:["1.0"]}
                                          sess.initialised = 1
                                  ◄────── result {serverVersion, protocol, capabilities}

← machine.load {preset:"teach-merlin", image_b64}
                                          presetLoader → machine.TeachMerlin()
                                          inst.Load(image_bytes)  (ROM + reset vector)
                                          sess.inst / sess.regions / sess.bps={} set
                                  ◄────── result {preset, regions:[ram, via1, via2, rom]}

← events.subscribe {channels:["clock.halt","bp.hit","state"]}
                                          subClockHalt / subBPHit / subState → true
                                  ◄────── result {subscribed:[...]}

← bp.set {addr:0xE100}
                                          inst.SetBreakpoint($E100)
                                          sess.bps["bp_1"] = …
                                  ◄────── result {id:"bp_1"}

← clock.run
                                          inst.SetRunning(true);  go sess.runner()
                                  ◄────── result {}

                                          runner goroutine begins:
                                            loop:
                                              slice = inst.RunUntil(200)
                                              tap-diff → emit (throttled, if subbed)
                                              every 100ms → emit state.snapshot (if subbed)
                                              slice halted?
                                                → emit bp.hit + clock.halt
                                                → exit

                                  ◄────── notification state.snapshot {...}
                                  ◄────── notification state.snapshot {...}
                                  ◄────── notification bp.hit {id:"bp_1",addr:$E100,state:{...}}
                                  ◄────── notification clock.halt {reason:"breakpoint",...}

← clock.step {n:1}                        runActive=false; back to dispatch
                                          inst.Step(1)
                                  ◄────── result {state:{...}}

close TCP ──────────────────────────►     EOF; teardown(): cancel runner, conn.Close
```

Anything any future client does is a permutation of those moves.

## "More pieces" in this picture

| Piece                                | What it is                                                       | What changes on the server      | Effort                    |
|---                                   |---                                                               |---                              |---                        |
| **MCP shim** (`mcp-go6sim`)          | Another client. Translates MCP tool calls → JSON-RPC.            | Nothing                         | Small (~200 lines)        |
| **VS Code desktop extension**        | Another client. `net.Socket` + the same JSON-RPC.                | Nothing                         | Small (~500 lines, mostly UI glue) |
| **WebSocket transport**              | Second `Conn` adapter alongside `ndjsonConn`.                    | Adds a binary or a `--ws-addr` flag + one dep | ~80 lines + 1 dep         |
| **`--serve` on TUI**                 | TUI sharing its **same** Instrument with bridge clients.         | TUI handlers must take `sess.mu`; clock-ownership rule needed | Bigger — design first     |
| **Merlin app** (`merlin-6502`)       | A program (assembly + assets) that runs on `teach-merlin`.       | Nothing                         | Application-sized work    |
| **Custom hardware backend**          | A second `Loader` (or bridge backend) that drives a real chip.   | New backend; bridge surface unchanged | Hardware-paced            |

Notice the right column: **everything except `--serve` and the Merlin
application itself is small client-side work**. The hard architectural
work is done — the bridge plus the in-process carve cover the surface
every future shape needs.

## Where to read next

| If you're touching…                                       | Read                                  |
|---                                                        |---                                    |
| The CPU, RAM, VIA, Backplane, clock driver, Instrument    | `architecture-backplane.md`           |
| The JSON-RPC wire — methods, params, returns, events, errors | `bridge.md`                          |
| The 6502 curriculum and which presets serve which lessons | `learn-path.md`                       |
| A new client (VS Code, MCP, browser, custom hardware)     | `bridge.md` §5 (methods) + this doc's "More pieces" table |
