# go6sim — Backplane Architecture

> Status: **resolved direction (2026-05-18); carve in progress —
> bricks 1–3 landed, green.** This supersedes the "headless `Machine`
> facade" framing. It **is** the spec for the build plan's step 2.
> See "Carve progress" near the end. `docs/learn-path.md` (step 1) drove
> the instrument API below; steps 3+ (`cmd/6502-core-wasm`, `mcp-go6sim`,
> the web `<CodeLab>`, a future in-TUI debugger) are all *instrument
> clients* of it.

## Principle: the bus is the product

A **backplane** is the stable interface. Everything else — CPU, RAM,
ROM, display, VIA, **and the clock** — is a **card** that attaches to it.
A card may be in-process Go, out-of-process, JS-driven, or (future) real
hardware behind an adapter. The sim is *one realization* of the
backplane; physical hardware is another; hybrid is allowed. It should
feel like bus boards in a card cage you can tap off — not "JS running yet
another interpreter."

The TUI is not "the machine." It is an **instrument/console** onto the
backplane, identical whether the backplane is simulated or real.

## The card contract (layered, compositional)

> Reconciled with the code (2026-05-18): a "card" **is the existing
> `bus.Component`** — keep that name, no rename. The bus already
> implements this layered model; the carve formalizes/extends it.

Required minimum — every card satisfies `bus.Component`; the backplane
requires only this. Offsets are component-relative (the bus maps
absolute → component+offset):

```go
type Component interface {
    Name() string
    Base() uint16
    Size() int
    Read(offset uint16) uint8
    Write(offset uint16, v uint8)
}
```

Optional capabilities — discovered by type-assertion (Go idiom: small
required interface + optional capabilities, like `io.Reader` +
`io.ReaderAt`). Used when present, degraded gracefully when not.
`Ticker`/`Labeller` already existed; `Resettable`/`Snapshotter`/
`Tappable` added in carve brick 1 (`bus/capabilities.go`):

```go
type Ticker      interface { Tick(dt time.Duration) } // → cycle-based in brick 3
type Labeller    interface { Symbols() []asm.Symbol }
type Resettable  interface { Reset() }
type Snapshotter interface { Snapshot() []byte; Restore([]byte) error }
type Tappable    interface { Taps() []Tap }            // named observable signals
```

`Tick` is therefore **not** in the required set — passive cards (RAM,
ROM) don't implement it; only clocked cards (VIA) do. This is the
codebase's existing, correct design.

- "Build your own CPU/VIA" (future learner arc) = implement only the
  required interface; optionally add capabilities.
- Provided cards (CPU, RAM, ROM, VIA, LCD) implement required + the
  relevant capabilities so the instrument can show *insight* without
  per-device special-casing.
- They compose, they don't conflict. Current curriculum = general 6502
  coding against provided cards; card-authoring is a later arc the same
  model already enables — **no architectural rework later**.

## The clock is a card too

The single unification. A clock **driver** is what advances the
backplane; it is attachable like any card:

| Driver | Is | Serves |
|---|---|---|
| free-run | the sim "crystal" | `Run`; TUI live; demos |
| single-step | tick N then stop | `Step(n)`; front panel; debugger `S`/`T` |
| budget-tick | tick a cycle quantum, yield | JS/RAF wasm pacing (**resolves open-Q #1**) |
| external *(future)* | a remote/hardware clock edge | hybrid / real-bus |

`Step`, `Run`, JS-pushed clock, a future front-panel, a debugger `G`/`S`,
a real hardware clock — **all the same seam: attach a clock driver.**

**Determinism invariant:** drivers are deterministic by default;
wall-clock time enters via *one explicit real-time driver only, never the
core*. (Concretely: `components/display/display.go`'s `frameAt
time.Time` must become a cycle count in the carve.)

## The instrument API (the one API all clients use)

TUI, web `<CodeLab>`, `mcp-go6sim`, and a future in-TUI debugger CLI are
all **instrument clients** of exactly this:

- attach/select **clock driver**; tick / run / stop control
- **bus transact**: peek/poke = a `Read`/`Write` on the backplane
- **state taps**: CPU registers + each card's `Taps()`
- **load** a `.bin` (origin,bytes) + **reset** (fresh reboot)
- **memory span read** (+ *future-optional* **region tap**: a bus tap so
  a consumer can mirror a watched range cheaply). There is **no display
  in the `teach-min` core** — the framebuffer is just RAM; rendering is
  always an external viewport (JS for web, tcell for TUI). The sim ships
  **bytes, never pixels.**

**Litmus test:** if the future debugger CLI (step / dump / memcpy /
peek / poke / go) is awkward to express on this API, the API is wrong.

## Interrupt observability (Q3 resolved)

Break-on-IRQ is the **control-flow twin of `RegFrame`**: one is "output
was transferred — stop and look," the other is "*control* was
transferred — stop and look." Same teaching principle. It is the
debugger-CLI litmus feature.

Curriculum (lesson 8), sequenced — fits the Layer-0 → Layer-1 thread:

- **8a observe-only (Layer-0):** a VIA **IRQ-fired count** tap. Watch it
  tick as T1 underflows. No ISR.
- **8b author-ISR (Layer-1):** learner takes over the IRQ vector (a
  go6asm *source* directive — not a sim API). Payoff: **break-on-IRQ**,
  then single-step the ISR.

Minimal deltas (small surface):

- VIA card `Tappable`: "IRQ asserted" + fired-count (IFR/IER already
  bus-readable).
- Per-source attribution ("VIA #1 vs #2") is **emergent, not built**:
  each VIA is a distinct card; the IRQ line is a backplane signal a card
  asserts, so the source is inherent to cards-on-a-bus. Data is free;
  *displaying* it is an optional UI choice.
- CPU card `Tappable`: "interrupt acknowledged / vectoring" signal (the
  cycle it pushes P/PC and loads the vector) + which vector
  (IRQ/NMI/BRK).
- Instrument API: breakpoint conditions gain **`interrupt-taken`**;
  `RunResult.stopReason` includes it (+ optional source). Break-on-IRQ
  then single-step = step-into-ISR.
- **No vector-takeover API** — the sim runs whatever vector bytes ROM
  holds; takeover is go6asm Layer-1 source; observing the current vector
  is the memory-span read we already have.

## No-second-interp invariant

The Go core is the **authoritative** backplane + cards. External
participants are **drivers, cards, or taps — never reimplementations.**
The wasm bridge *exposes* the backplane; JS pushes a clock, pushes a
`.bin`, attaches/observes cards, reads taps. **JS runs no CPU.** One
source of truth; preserves the "black box you tie into" feel.

## Presets

`Machine` = a named preset = (clock driver, CPU card, device cards).

| Preset | Cards | Role |
|---|---|---|
| `vic-demo` | CPU + RAM + ROM + VIA + **VIC** | the existing standalone ecosystem + demo corpus — **untouched** |
| `teach-min` | CPU + RAM + ROM + VIA, with a **RAM region designated as framebuffer** (no display card) | the learn machine (v1) |

**Behavioral vs presentation (the resolving line):** a card lives in the
sim only if it has *behavior* to simulate. The VIA does (counts down,
raises IRQ) — it stays. A dumb framebuffer does **not** — it is just RAM
someone looks at, so it is **memory + an external viewport, never a core
card.** The sim ships bytes; rendering is always a wrapper (JS/tcell).

Display ramp (teaching), all rendered consumer-side over a framebuffer
RAM region: **1 byte/dot** (poke addr → pixel; zero bit math) → **8
dots/byte** (the packed form *is* the shift/mask lesson — "shift left one
dot") → char text. Pixel encoding is a *per-lesson consumer choice*, not
architecture. The VIC's command+present model (double-buffer / e-ink /
tearing) is a **`vic-demo`-only device** — it has behavior+protocol, so
it's a real card there, and that is exactly why it's the *advanced* arc,
not lesson 3.

The 6522 **VIA stays** in the teaching preset: timers, IRQ, PA/PB ports
are core 6502 and the "hardware that does cool things" payoff. Ports are
exposed as taps/attach points (blink a PB LED now; drive external things
later).

## v1 scope discipline

**Build:** all-local-in-Go backplane; in-proc cards (CPU{interp|netsim},
RAM, ROM, VIA — framebuffer is a RAM-region convention, **no display
card**); clock drivers {free-run, single-step, JS-budget}; card contract
= required + capabilities; the `teach-min` preset; `vic-demo` preset
preserved as-is.

**Design-for, do NOT build in v1:** remote/out-of-proc cards, remote or
hardware clock drivers, hardware-adapter cards, the in-TUI debugger CLI,
the build-your-own-card learner arc. Interfaces must not preclude these;
transports are future.

**Untouched:** the standalone terminal TUI + demo corpus = the `vic-demo`
preset and the go6asm byte-equivalence backstop (kept, scoped).

## Where the existing code already is

The carve is less than it sounds — much exists, tcell-free:
`bus` (TraceBus.Tick), `cpu/{interp,netsim}` (Reset/Registers),
`components/{ram,rom,via,display}` (Tick/Reset/Snapshot/Load). Missing:
the unifying **Backplane** + **clock-driver** seam + **instrument API**,
the formal required/optional **card interfaces**, and removing
`display.go`'s wall-clock. The TUI becomes an instrument client; nothing
in the core imports tcell (`make wasm-check`, like go6asm).

## Carve progress

Discipline: every brick builds + all tests + backstops (`internal/demos`
equiv & analyze) green; additive where possible; the standalone TUI and
its feel are never regressed.

- **Brick 0** — recon + green baseline. ✓
- **Brick 1** — `bus/capabilities.go`: optional capability interfaces
  (`Resettable`/`Snapshotter`/`Tappable`) alongside the pre-existing
  `Ticker`/`Labeller`. Spec reconciled with code (a "card" *is*
  `bus.Component`). Additive, green. ✓
- **Brick 2** — `internal/goldentrace`: a deterministic runtime
  regression net (assemble via go6asm → run current machine → snapshot
  regs+ZP vs a committed golden; `GO6SIM_GOLDEN_UPDATE=1` to regen).
  The pre-existing backstops are assembly-level; this is the first
  *execution* net. Additive, green. ✓
- **Brick 3** — kill wall-clock in the core (option **a**: deterministic
  virtual-time, signatures unchanged). `display.Controller` is now a
  virtual-time `bus.Ticker`; `frameAt`/`lastCmdAt` are virtual
  `time.Duration`; recency accessors → `SinceLastCmd/Frame() (dur,
  seen)`; `ui/displaywin` updated to the single virtual timebase. Core
  has **zero `time.Now`/`time.Time`** (only deterministic
  `time.Duration`). Locked by `components/display` determinism test
  (identical with/without real sleeps). `wasm-check` still green. The
  standalone TUI keeps its feel — peripheral time is a *driver policy*
  (its driver still feeds periodic dt); only learn/replay drivers are
  cycle-locked. ✓
- **Brick 4** — `backplane` package: `Backplane` *embeds* `*bus.TraceBus`
  (inherits `bus.Bus` + `Tick` + the UI trace contract verbatim — zero
  reimpl), adds `Attach` (component-owned ranges; spec's `(lo,hi)`
  sketch reconciled to code, as brick 1) and a `Reset()` fan-out over
  the brick-1 `Resettable` capability, plus a `Trace()` escape hatch for
  `ui/ramwin`. Both `cmd/6502-sim` and `cmd/6502-wasm` migrated to
  compose via `backplane.New()` (`b.Attach`), behavior-identical. Unit
  test + full suite + wasm-check + backstops green. ✓
- **Brick 5** — clock-driver seam extracted. The run/step/speed logic
  (was `ui/clockwin.Provider`, tcell-coupled) moved verbatim to a core
  tcell-free, wasm-clean `clock` package (`clock.Driver`:
  Advance/StepOne/StepInstruction/Reset/Speed/SetRunning + new
  StepsDone/SpeedIndex accessors). `ui/clockwin.Provider` now embeds
  `*clock.Driver` + `foxpro.ScrollState` (no method-name collision) and
  is a thin view; type-alias re-exports keep `cmd/*` (`clockwin.Speeds/
  Provider/NewProvider/MinW/MinH`, all `clockProv.*`) untouched.
  Behaviour-identical; net = `clock/clock_test.go` (5 tests). ✓

  *Original "brick 5" was a 3-in-1 sketch — re-decomposed into tight
  green bricks:*
- **Brick 6** — `instrument` package: `Instrument` composes
  `*backplane.Backplane` + `*clock.Driver` behind one surface —
  `Reset/Step/StepCycle/SetRunning/Running/Advance/State/Peek/Poke/Mem`
  + `Driver()` hatch. `Advance(dt)` encapsulates the
  `drv.Advance`+`bp.Tick` lockstep cmd hand-duplicates. `Peek/Mem` use
  the *untraced* inner bus (inspection mustn't pollute the UI trace);
  `Poke` is traced (a real mutation). `Frame` = `Mem` over a region
  (Q2: ship bytes, no display card). Load deferred to presets (they own
  the ROM card). Additive, tcell-free, wasm-clean. Litmus net
  `instrument/instrument_test.go`: the golden program driven entirely
  through the surface (step/dump/peek/poke/go). ✓
- **Brick 7** — TUI is an instrument client. Done *minimally*, not by
  rewrite: `clockwin.NewProviderWithDriver` lets the clock window and a
  new `instrument.Instrument` **share one `*clock.Driver`**; the run
  loop's hand-duplicated `clockProv.Advance + b.Tick` became
  `inst.Advance(subPeriod)` (brick 6's `Advance` *is* that pairing).
  Behaviour-identical **by construction** (same ops/order/shared
  objects); backend-swap path unchanged (mutates the shared driver,
  instrument observes it). build + wasm-check + all nets/backstops
  green. No automated test covers interactive TUI UX → needs a manual
  smoke (`go run ./cmd/6502-sim`), but the reasoning is airtight. ✓
- **Next — Brick 8** — presets (`vic-demo` == today, byte-identical =
  the proof; `teach-min`). Brick 8 also owns `Load` (the ROM card).
- **Brick 9** — taps (VIA irq, CPU irq-ack) + `interrupt-taken`
  breakpoint.
- **Brick 8** — presets (`vic-demo` byte-identical = the proof;
  `teach-min`).
- **Brick 9** — taps (VIA irq, CPU irq-ack) + `interrupt-taken`
  breakpoint.

## Open questions (carried)

1. ~~Paced-run shape~~ — **resolved**: it's a clock driver (budget-tick).
2. ~~Display payload~~ — **resolved**: ship bytes, not pixels. No display
   card in `teach-min`; framebuffer = a RAM region + an external viewport
   (JS/tcell). v1 = consumer pulls the span per tick (~KB); region-tap is
   future-optional. Encoding (1 byte/dot vs 8 dots/byte) is a per-lesson
   consumer choice — the packed form *is* the shift/mask lesson.
3. ~~Lesson-8 interrupt depth~~ — **resolved**: both, sequenced (8a
   observe IRQ-count, Layer-0; 8b author-ISR + break-on-IRQ, Layer-1).
   See "Interrupt observability" above. Minimal: VIA irq tap, CPU irq-ack
   tap, an `interrupt-taken` breakpoint condition; no vector-takeover API.
4. Where the path/track sits in the existing `/learn` IA.
