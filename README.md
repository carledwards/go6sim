# go6sim

A floating-window 6502 microcomputer simulator with two interchangeable
CPU cores, a memory-mapped VIC video chip, a real **6522 VIA** peripheral
ticking on its own crystal, and a small library of demo programs. Built
on top of [`foxpro-go`](https://github.com/carledwards/foxpro-go)
(FoxPro-for-DOS-style TUI framework) and
[`6502-netsim-go`](https://github.com/carledwards/6502-netsim-go)
(transistor-level Visual6502 port). Each component plugs into a shared
bus at hardware-realistic chip-select boundaries; each gets its own
draggable window.

The point is to make a 6502 system you can *see*: every memory access,
every register, every framebuffer cell, every timer underflow, in real
time. Long-term goal: a teaching tool where demos written here transfer
to real silicon unmodified.

## Screenshots

**TUI Logic Analyzer** — bus + control-line trace in a real terminal,
captured at 20 Hz with single-step granularity.

![TUI Logic Analyzer](docs/images/tui_logic_analyzer.gif)

**Wasm Logic Analyzer** — same widget rendered to a browser canvas
via the pixel-overlay path; ~8× the sample density per cell.

![Wasm Logic Analyzer](docs/images/wasm_logic_analyzer.gif)

**Wasm Emulation** — the full simulator running in a browser tab:
CPU window, memory viewer, Monitor REPL, and the VIC graphics demo
all driven by the same backend that runs in the terminal.

![Wasm Emulation](docs/images/wasm_emu.gif)

## Quickstart

```bash
make tidy
make run
```

`go.mod` pins
[`foxpro-go`](https://github.com/carledwards/foxpro-go) (TUI framework)
and [`6502-netsim-go`](https://github.com/carledwards/6502-netsim-go)
(transistor-level CPU) to tagged versions, so a clean clone fetches
everything from the module proxy with no sibling-checkout setup.

If you want to iterate on either dependency locally, add a temporary
`replace` directive in `go.mod` pointing at your sibling checkout —
remove it before pushing.

Defaults are tuned for "open it, see something happening": the TUI
boots on the interpretive CPU at Max speed with batch auto-tuned to
fit the per-tick budget, the marquee demo is loaded, and the clock
is running. Esc or Ctrl+Q to quit.

## Browser build

Same code, same demos, same dual-CPU backend, same VIA — running in
the browser via WebAssembly through
[`foxpro-go/wasm`](https://github.com/carledwards/foxpro-go). The
simulator's `tcell.Screen` is swapped for a `tcell.SimulationScreen`
(pure-Go cell buffer) and the JS side renders cells to a canvas.
Graphics-mode pixels are layered onto the cell grid via a sentinel-
rune trick — windows, drop shadows, and z-order all work over the
bitmap.

```bash
make wasm           # build web/sim.wasm + copy wasm_exec.js
make wasm-serve     # python3 -m http.server on port 8765 (override with PORT=)
```

Then open `http://localhost:8765/`.

The wasm build defaults to:

- **interp** CPU (netsim is slow under wasm; swap via the CPU menu if
  you want to watch transistors crawl)
- auto-start running so visitors see motion immediately
- BouncingBalls graphics demo as the boot program
- Esc / Ctrl+Q disabled (would terminate the wasm runtime and brick
  the page); close the tab instead

Bundle size: ~5.3 MB raw, ~1.4 MB gzipped. Standard static-host MIME
config (`application/wasm`) is enough — no cross-origin headers
required.

## Bridge protocol + Monitor REPL

Beyond the on-screen widgets, the simulator exposes a debugger
protocol — JSON-RPC 2.0 over NDJSON/TCP for remote drivers, or as
direct Go calls in-process. Both share one Go interface
(`bridge.Target`), so the same Monitor REPL drives every edge:

| Edge | Command | How it talks to the sim |
|---|---|---|
| Built-in Monitor (terminal) | `cmd/6502-sim` (Window → Toggle Monitor) | in-process via `bridge.HubDirect` |
| Built-in Monitor (browser) | `cmd/6502-wasm` (Monitor visible by default) | in-process via `bridge.HubDirect` |
| Headless bridge server | `cmd/6502-sim-serve` | listens on `:6502`, NDJSON/TCP |
| Shared-bridge TUI | `cmd/6502-sim --serve` | TUI runs locally + accepts remote bridge clients |
| Remote controller TUI | `cmd/6502-control` | dials a bridge server, healing reconnect |
| **Future** | MCP server, VS Code extension, etc. | implement `bridge.Target` |

The Monitor itself lives in `internal/monitor`. Its command set:

| Group | Verbs |
|---|---|
| CPU & run | `r` regs · `g [addr]` run · `s [n]` step · `.` stop · `reset` · `stack [r]` |
| Memory | `m [addr] [n]` hex · `d [addr] [n]` disasm · `: <addr> <b>…` poke · `f <s> <e> <b>` fill · `t <s> <d> <n>` transfer · `h <s> <e> <b>…` hunt |
| Breakpoints | `bp <addr>` · `bc [id\|all]` · `bl` |
| Interrupts | `irq` · `nmi` |
| Hardware | `hw` / `info` · `via <sub>` (list/dump/set) |
| Monitor | `cls` · `help [cmd]` · `help window` · `reconnect` · `q` |

Addresses are hex (`$E000` / `0xE000` / `E000`) or **symbolic**:
`pc`, `sp`, `reset`, `irq`, `nmi`. The symbolic forms read live —
`d pc` disassembles wherever you currently are; `m irq` dumps the
IRQ handler the CPU would jump to right now.

The remote controller auto-reconnects with backoff if the sim
restarts. CPU state preservation across reconnect depends on which
loader: `cmd/6502-sim --serve` keeps the live Hub across reconnects;
`cmd/6502-sim-serve`'s per-session Hub is fresh on each reconnect.

The protocol contract lives in [`docs/bridge-v2.md`](docs/bridge-v2.md).

### CLI flags (terminal build only)

| Flag           | Default   | Notes                                                 |
|----------------|-----------|-------------------------------------------------------|
| `-cpu`         | `interp`  | CPU backend: `interp` or `netsim` (transistor)        |
| `-run`         | `true`    | Start the clock running immediately                   |
| `-speed`       | `max`     | Initial clock target: `1`, `10`, `20`, `100`, `1k`, `max` |
| `-batch`       | `0`       | Max half-cycles per UI tick (0 = auto-tune at startup)|
| `-cpuprofile`  | (off)     | Write CPU pprof to file                               |
| `-memprofile`  | (off)     | Write heap pprof at exit                              |

The wasm build doesn't take flags; it boots with the same defaults.
User-facing controls live in the menus and keyboard shortcuts.

## Memory map

Hardware-realistic address decoding — components claim their ranges
exactly the way a 74HC138 chip-select decoder would on a real board.
A two-stage decoder (A13–A15 → 8 KB regions; A8–A11 within the I/O
region → 256 B sub-regions) gives every peripheral its own CS line
with no chip-select collisions.

| Range              | Component                  | Size    |
|--------------------|----------------------------|---------|
| `$0000`–`$1FFF`    | RAM                        | 8 KB    |
| `$A000`–`$A3FF`    | VIC color plane            | 1 KB CS (520 B used) |
| `$A400`–`$A7FF`    | VIC char plane             | 1 KB CS (520 B used) |
| `$A800`–`$ABFF`    | VIC controller             | 1 KB CS (16 B used)  |
| `$B000`–`$B0FF`    | 6522 VIA #1                | 256 B CS (regs mirror ×16) |
| `$B100`–`$BFFF`    | peripheral slots (15 ×)    | 256 B CS each |
| `$C000`–`$DFFF`    | VIC graphics plane         | 8 KB (160 × 100 @ 4bpp) |
| `$E000`–`$FFFF`    | ROM (reset vector at `$FFFC`) | 8 KB |

VIC controller registers (offsets within `$A800`):

| Off  | Reg          | Behavior                                          |
|------|--------------|---------------------------------------------------|
| `+0` | Cmd          | Write triggers an op (Clear, Shift\*, Rot\*, Invert, Rect\*, Gfx\*) |
| `+1` | Pause        | `1` = UI shows snapshot; `0` = UI shows live memory |
| `+2` | Frame        | Any write captures a new snapshot (use while paused) |
| `+3` | RectX        | Rect parameters consumed by `CmdRect*` and `CmdGfx*` |
| `+4` | RectY        | opcodes — clamped to display bounds                |
| `+5` | RectW        |                                                   |
| `+6` | RectH        |                                                   |
| `+7` | GfxColor     | Current draw color for `CmdGfx*` (palette idx 0–15) |
| `+8` | Mode         | `0` = char (default), `1` = graphics              |

VIA #1 — Phase 1 implements **Timer 1** in free-running and one-shot
modes plus IFR/IER semantics; ports, T2, SR, and PCR are stubbed and
read/write a backing byte without side effects yet. The chip is
clocked from its own 1 MHz oscillator (independent of the CPU), so
demos that pace off T1 keep ticking even while the CPU is single-
stepping or paused — same as a real 65C22S board with a separate
timer crystal. Pacing pattern (canonical W65C22):

```asm
; Set up T1 free-run with latch = $C350 (~50 ms @ 1 MHz)
LDA #$50  : STA $B006   ; T1L-L
LDA #$C3  : STA $B005   ; T1C-H — copies latch→counter, starts T1
LDA #$40  : STA $B00B   ; ACR bit 6 = T1 free-run

; Poll for underflow
WAIT: LDA $B00D         ; IFR
      AND #$40          ; T1 flag
      BEQ WAIT
      LDA $B004         ; T1C-L read clears IFR T1
```

## CPU backends

| Backend  | Speed       | What it is                                            |
|----------|-------------|-------------------------------------------------------|
| `interp` | several MHz | Conventional 151-opcode interpretive 6502 (default)   |
| `netsim` | ~26 kHz     | Transistor-level Visual6502 port — every cycle simulates ~3500 transistors |

The `Backend` interface (`cpu/backend.go`) lets you swap at runtime
via the **CPU** menu. Both expose the same address/data bus state
plus IRQ/NMI for the simulator's introspection windows.

## Windows

Every component gets its own floating, draggable window. Click in the
title bar to drag, click the corner to resize.

- **CPU** — A/X/Y/S/PC, P flags, half-cycle counter, live address bus,
  data bus, R/W direction, IRQ/NMI line states.
- **RAM** / **ROM** (Memory views) — hex view + ASCII column with
  editable base address (click the `$XXXX:` button, type 4 hex
  digits). Trace tinting: yellow = write that *changed* the byte,
  brown = write that left it unchanged, green = read. `v` cycles
  Hex / Disasm / Labels — Labels shows declared symbols within the
  current region (or a per-byte fallback view for regions without
  symbols). The disasm column substitutes operand addresses with
  symbol names where known and appends per-instruction comments.
- **VIC / Video** — 40 × 13 framebuffer with 16-color palette, plus a
  160 × 100 graphics plane (when in graphics mode). Right column has
  buttons for every controller command. Below the framebuffer, a
  scrollable hex strip shows the VIC's controller region.
- **VIA 1** — live snapshot of the chip's state: ports + DDRs at the
  top, then Timer 1 (counter / latch / mode / armed flag), then ACR
  decoded + IFR + IER bit dots (● set, . clear). The chip's base
  address + crystal speed live in the title bar. The counter ticks
  down even when the CPU is paused or stepping, because the VIA's
  crystal runs independently.
- **Monitor** — the shared REPL described above. Toggleable from
  the Window menu in the terminal build; visible on startup in the
  browser build. Common pane across all three apps.
- **Logic Analyzer** (scope) — hidden by default; toggleable. 256
  cycles of bus-trace history with auto-tuned sampling stride.

Run/Stop/Step + speed controls live on the menu bar's right-side
tray (clickable). The Clock window was removed when the bridge
landed — every action is in the menu, keyboard hotkeys, or the
Monitor's command line.

## Demos

Selectable from the **Demo** menu, in three sections:

| Demo                | What it does                                            |
|---------------------|---------------------------------------------------------|
| Marquee             | Scrolling "HELLO 6502 SIM"; paces via VIA T1 (default boot demo for TUI) |
| Bouncer             | Single `*` bouncing across row 6                       |
| Scroller            | Diagonal gradient scrolling up the display              |
| Snow (LFSR)         | 8-bit Galois LFSR fills + clears the framebuffer       |
| Scroller (framed)   | Same as Scroller but Pause + Frame for clean snapshots |
| Blitter (RAM→VIC)   | Copies byte patterns out of RAM into the VIC planes    |
| Quadrants           | 4 independent rect rotations using `CmdRect*`          |
| Bouncing Balls      | Four colored balls in graphics mode, paced via VIA T1 (wasm only — TUI has no graphics plane) |

All demos are built via the in-tree `asm` package — a small fluent
6502 assembler that emits bytes plus per-instruction comments and
named memory symbols, surfaced by the Memory window's Labels and
Disasm views.

## Menu shortcuts

| Key       | Action                              |
|-----------|-------------------------------------|
| `Z`       | Reset machine (does NOT stop the clock — like a hardware reset button) |
| `F2`      | Toggle command window               |
| `R`       | Run                                 |
| `.`       | Stop                                |
| `S`       | Step one instruction (until PC changes) |
| `T`       | Step one half-cycle ("tick")        |
| `Esc`     | Quit (terminal) · close menu (browser, where Quit is disabled) |

In the Memory window:

| Key       | Action                              |
|-----------|-------------------------------------|
| `g`       | Edit base address                   |
| `v`       | Cycle view: Hex / Disasm / Labels   |
| `i`       | Toggle disassembly info panel       |

In the VIC window's hex strip:

| Key / mouse        | Action                      |
|--------------------|-----------------------------|
| Mouse wheel        | Scroll memBase by 1 row     |
| `[` / `]`          | Scroll by 1 row (16 bytes)  |
| `{` / `}`          | Scroll by 1 page (112 bytes) |
| Click `▲` / `▼`    | ±1 row                      |
| Click track        | Page up/down                |
| Drag `◆`           | Jump to position            |

## Architecture

The execution model has two goroutines: the **foxpro UI thread**
(handles input, draws windows) and the **Hub Pump** (drives the CPU
+ peripherals in slices, synchronously). They serialise through a
single mutex — concurrent reads from the UI side take a query lock;
the Pump holds it during each slice. Same model native + wasm.
In wasm, the Pump yields to the JS event loop every ~10 ms in Max
mode so the browser tab can render.

The Pump slices each "tick" into 200-half-cycle chunks and pairs
each `RunUntil` with a `bus.Tick(virtualDt)` for peripherals — so
polling-based demos (those that LDA/AND/BEQ a peripheral flag in a
tight wait loop) observe timer underflows multiple times per app
frame instead of just once. The driver auto-tunes per-tick batch
size at startup to fit the host's tick budget.

Components self-describe their register layouts via the optional
`bus.Labeller` interface (`Symbols() []asm.Symbol`), so the Memory
window's Labels view annotates the VIC and VIA register regions
automatically — no hand-maintained mapping. Time-driven peripherals
implement `bus.Ticker` and get fanned out automatically.

The **bridge protocol** is a separate layer above the Hub. Clients
(remote `cmd/6502-control`, in-process Monitor in `cmd/6502-sim` /
`cmd/6502-wasm`, future MCP / VS Code) implement / consume the
`bridge.Target` interface; the transport (TCP NDJSON or direct Go
calls via `bridge.HubDirect`) is the only thing that differs.

Read `docs/architecture.md` for layering + component contracts,
`docs/bridge-v2.md` for the protocol surface, and `docs/roadmap.md`
for remaining work.

## Project layout

```
cmd/6502-sim/         terminal entry — main wiring, flags, profiling
cmd/6502-sim-serve/   headless bridge server (no UI, listens on :6502)
cmd/6502-control/     remote controller TUI — bridge client over NDJSON/TCP
cmd/6502-wasm/        browser entry — wasm-tagged, uses foxpro-go's wasm bridge
asm/                  fluent 6502 assembler used by demos
backplane/            machine bus + interrupt aggregation + reset capability
bridge/               protocol layer — Hub + Pump + Target interface + HubDirect
clock/                Driver, Speeds, halfStep accumulator
cpu/                  Backend interface + PCSetter capability
cpu/netsim/           netsim adapter
cpu/interp/           interpretive 151-opcode 6502 (with IRQ/NMI service paths)
components/           ram, rom, display, via
disasm/               151-opcode disassembler with cycle counts and effects
instrument/           Instrument facade — wraps backplane + driver
internal/bridgeclient/ Go wire client for the bridge protocol
internal/monitor/     shared Monitor REPL — drives any bridge.Target
internal/demos/       shared demo programs (text + graphics)
ui/                   cpuwin, ramwin, displaywin, viawin, scopewin, clockwin
web/                  static frontend served by the wasm build (built artifacts)
docs/                 architecture, bridge protocol, roadmap, systems
```

## Status

Working on both terminal and browser. The transistor-level core hits
~26 kHz on a recent Mac; the interpretive core is several MHz. Both
pass the same demos.

The simulator is set up so each peripheral lives on its own
chip-select region with realistic mirroring (the 6522's 16 registers
mirror through a 256-byte CS block, exactly as on a real board with
only RS0–RS3 hooked up). Demos written here should run on real
silicon without modification.

## License

MIT — see [LICENSE](LICENSE).
