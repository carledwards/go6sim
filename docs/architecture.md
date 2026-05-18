# Architecture

How the pieces fit. Read alongside `roadmap.md`.

## Layering

```
┌─────────────────────────────────────────────┐
│  cmd/6502-sim — wires app, windows, bus, cpu     │
├─────────────────────────────────────────────┤
│  ui/   (foxpro-go ContentProviders)         │
│   - cpuwin    - ramwin    - clockwin        │
│   - debugwin  - displaywin                  │
├─────────────────────────────────────────────┤
│  cpu.Backend  ←—  netsim adapter            │
│               ←—  interpretive (later)      │
├─────────────────────────────────────────────┤
│  bus.Bus  —  range dispatcher               │
│           —  components: ram, rom, display  │
├─────────────────────────────────────────────┤
│  foxpro-go (TUI)  +  6502-netsim-go (core)  │
└─────────────────────────────────────────────┘
```

UI layer never reads CPU internals directly — it goes through the
backend interface. CPU backends never reach into components — they
go through the bus.

## Bus

```go
package bus

type Bus interface {
    Read(addr uint16) uint8
    Write(addr uint16, val uint8)
    Register(c Component) error    // returns error on range overlap
    Components() []Component
}

type Component interface {
    Name() string
    Base() uint16
    Size() int                     // number of addresses claimed
    Read(offset uint16) uint8      // offset within the component
    Write(offset uint16, val uint8)
}
```

- Address space is 16-bit (`uint16`) — netsim core uses `int` but the
  top bits are never set; we narrow at the adapter boundary.
- Unmapped reads return `0x00`. Unmapped writes are dropped.
- ROM ignores writes internally (cleaner than failing).

## CPU backend

```go
package cpu

type Backend interface {
    Reset()
    HalfStep()                     // one half-clock (matches netsim)
    Step()                         // one full instruction (computed in adapter)
    Registers() Registers
    Cycles() uint64
}

type Registers struct {
    A, X, Y, S, P uint8
    PC            uint16
}
```

- `HalfStep` is the lowest common denominator — netsim only exposes
  half-cycle granularity. The interpretive core fakes it (each
  instruction's cycles → 2*N halfsteps internally).
- `Step` runs HalfStep until PC advances past the current instruction.
- `Cycles` is purely informational for the UI.

### netsim adapter

Wraps `motherboard.New(transDefsPath, segDefsPath)`'s logic but takes
our `bus.Bus` instead of constructing its own RAM+ROM. Adapter
implements netsim's `ReadFromBus` / `WriteToBus` callbacks by
forwarding to `bus.Read`/`bus.Write`.

### interpretive adapter (Phase 6)

Either roll our own or wrap an existing Go 6502 core. Same `Backend`
interface so swapping is invisible to the UI.

## Clock + run loop

- The UI never blocks on the CPU. The clock window owns a goroutine
  in Run mode.
- Run-mode goroutine: `for running { backend.HalfStep() }` with a
  `time.Sleep` derived from selected Hz.
- After each batch of steps, post a "redraw / refresh state" message
  via `app.Post(func() { ... })` so windows show fresh values.
- Step mode: button click → `app.Post(backend.Step)`.
- Stop just closes the run goroutine's channel.

## UI provider responsibilities

Each window holds *only* its own view state. CPU + bus state lives
once, in the simulator core. Windows pull on each `Draw`.

| Window      | Pulls from                           |
| ----------- | ------------------------------------ |
| cpuwin      | `backend.Registers()`, `Cycles()`    |
| ramwin      | `bus.Read(addr)` for visible rows    |
| romwin      | same as ramwin (read-only)           |
| displaywin  | `bus.Read` over its mapped region    |
| clockwin    | run/stop state held locally          |
| debugwin    | regs, disassembly around PC, BPs     |

## Threading rules

- The clock goroutine is the only non-UI goroutine that touches CPU
  + bus. Single writer means we don't need a mutex around the bus
  (yet — revisit if we add async peripherals).
- All UI mutations go through `app.Post`. No window provider mutates
  CPU/bus from `Draw` or `HandleKey` — they enqueue commands.
- `App.Tick(100ms, nil)` keeps the UI redrawing while Run mode
  advances state in the background.

## File layout (proposed)

```
go6sim/
├── cmd/6502-sim/main.go
├── bus/
│   ├── bus.go
│   └── bus_test.go
├── components/
│   ├── ram/
│   ├── rom/
│   └── display/
├── cpu/
│   ├── backend.go        // interface + Registers
│   ├── netsim/           // adapter
│   └── interp/           // (later)
├── ui/
│   ├── cpuwin/
│   ├── ramwin/
│   ├── clockwin/
│   ├── debugwin/
│   └── displaywin/
├── docs/
│   ├── roadmap.md
│   └── architecture.md
└── go.mod
```

## Open questions

- Should we ship netsim's `data/` files (transdefs/segdefs) inside
  this repo, or point at the netsim repo's copy via a config flag?
  Leaning: bundle via `go:embed` so the binary is self-contained.
- Disassembler: write our own (~150 lines) or import? Probably write
  it — small, no extra dep.
- Run-mode max speed throttle: hard cap or "as fast as possible"?
  Start uncapped; add cap if it pegs the UI.
