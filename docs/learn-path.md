# go6sim — Learn Path

> Status: **design / outline**. No lesson content written yet. This
> document defines the *sequence* of interactive lessons. It originally
> derived a `Machine` facade API; that has been **superseded by
> [`architecture-backplane.md`](architecture-backplane.md)** (the
> backplane / cards / clock-driver / instrument model) — read that for
> the step-2 spec. The lesson table below still uses VIC registers; its
> **device specifics need a re-grounding pass on the `teach-min` preset
> (a simple memory-mapped char LCD + 6522 VIA, not the VIC)** before
> content is authored.

## What this is

An ordered set of interactive lessons on thecarledwards.com `/learn`. Each
lesson: prose + a `<CodeLab>` where the learner edits 6502 assembly, go6asm
assembles it in-browser (`target sim-tui`, Layer-0), and **go6sim runs it**
with focused views — VIC output, registers, memory, the go6asm
disassembly-with-comments, and the static analyzer.

**Lab programs are authored per lesson** and owned by the learn layer —
small, purpose-built `.s` that teach one idea. The proven demo corpus
(`internal/demos/asmsrc/*.s`) is **not** the curriculum spine: it stays a
standalone artifact (proof-of-system, the go6asm byte-equivalence
backstop, future CLI example set) and is at most an optional *"here's a
real standalone program — go run it in the TUI/CLI"* reference. Curriculum
and corpus are independent.

## The machine (what every lesson assumes)

The `sim-tui` go6asm target. go6asm Layer-0 infers load address `$E000`
and synthesizes the `$FFFA` vector block, so early lessons are *just
instructions* — no directives, no addresses.

```
$00–$0F      reserved scratch (system convention)
$10–$FF      user zero page (fast indexed addressing)
$0000–$1FFF  RAM (8 KB)
$A000–$A3FF  VIC color plane   (40×13 cells)
$A400–$A7FF  VIC char  plane   (40×13 cells)
$A800–$A80F  VIC controller registers
$B000–$B0FF  6522 VIA #1 (timer pacing)
$C000–$DFFF  VIC graphics plane (160×100 @ 4bpp, graphics mode)
$E000–$FFFF  ROM (program here; reset vector $FFFC; block $FFFA)
```

go6asm's `sim-tui` target auto-provides these names (no `.import`):

| Group | Symbols |
|---|---|
| VIC ctrl | `RegCmd $A800` `RegPause $A801` `RegFrame $A802` `RegRectX/Y/W/H $A803–$A806` `RegGfxColor $A807` `RegMode $A808` |
| VIC cmds | `CmdClear $01` `CmdShiftUp $02` `CmdRotLeft $07` `CmdGfxClear $20` `CmdGfxRectFill $23` `CmdGfxFillCircle $26` |
| Planes | `ColorBase $A000` `CharBase $A400` |
| VIA #1 | `ViaBase $B000` `ViaT1C_L $B004` `ViaT1C_H $B005` `ViaT1L_L $B006` `ViaT1L_H $B007` `ViaACR $B00B` `ViaIFR $B00D` `ViaIER $B00E` `ViaT1Bit $40` |

## The lesson sequence

Each row: the concept, a starter sketch (illustrative — Layer-0 unless
noted), the demo it builds toward, and **the facade capabilities it
exercises** (this column is the API spec — see next section).

| # | Lesson | Starter sketch | Builds toward | Facade caps |
|---|--------|----------------|---------------|-------------|
| 0 | **Orientation.** The machine = CPU + RAM + VIC + VIA. The toolchain: write asm → go6asm assembles → go6sim runs. Run a 1-instruction program. | `LDA #$2A` | — | `Load`, `Reset`, `Step(1)`, `State` |
| 1 | **Registers & immediates.** `LDA/LDX/LDY #imm`; watch A/X/Y change. | `LDA #$01` `LDX #$10` `TAX` | — | `Step(n)`, `State` (A,X,Y,P,PC,cycles) |
| 2 | **Memory.** `STA abs` then `LDA abs`; write RAM, read it back. | `LDA #$42` `STA $0200` `LDA $0200` | — | `Mem(lo,hi)` read; watch one cell |
| 3 | **The screen is memory.** Poke `CharBase`/`ColorBase` → a glyph appears. The "aha". | `LDA #'A'` `STA CharBase` `LDA #5` `STA ColorBase` | hello.s | `Frame()` (text planes) reflects `Mem` writes |
| 4 | **Loops & branches.** Fill a row of the char plane with a counted loop. | `LDX #0` `lp: LDA #'*'` `STA CharBase,X` `INX` `CPX #40` `BNE lp` | hello.s | `Run(untilBreakpoint\|maxCycles)`, breakpoint, cycle count |
| 5 | **Indexed addressing.** Copy a string/table into the char plane. | `ldx #0` `c: lda msg,x` `beq d` `sta CharBase,x` `inx` `bne c` `d: ...` `msg: .byte "HELLO",0` *(Layer-0 still ok)* | hello.s, marquee.s | `Mem` span window; `Step` into the loop |
| 6 | **The VIC controller.** Structured ops vs raw pokes: `RegCmd`=`CmdClear`, `CmdShiftUp`. | `LDA #CmdClear` `STA RegCmd` | scroller.s | `Frame()` after a command (side-effects visible) |
| 7 | **Timing with the VIA.** Free-run T1; poll `ViaIFR & ViaT1Bit` to pace work (no IRQ yet). | set `ViaT1L_*`, `ViaACR`; `w: BIT ViaIFR` `BVC w` | bouncer.s (pacing) | long `Run`, cycle/time, watch VIA regs |
| 8 | **Interrupts.** The synthesized vectors (Layer-0) vs *taking them over* (Layer-1). VIA T1 IRQ, `ViaIER`, `RTI`. | `.target sim-tui` / `.segment` to own the IRQ vector + ISR | scroller_framed.s | vector introspection, IRQ modeling, break-on-vector, step-into-ISR |
| 9 | **Animation.** move → erase → redraw, paced by the VIA. Then run the *real* bouncer and modify it. | (bouncer.s itself) | **bouncer.s** | continuous paced `Run`, `Frame()` streaming, `Reset` |
| 10 | **Tearing & double-buffer.** `RegPause`+`RegFrame`; scroller vs scroller_framed side by side. | (scroller.s vs scroller_framed.s) | **blitter.s** | frame timing, `Pause`/commit semantics |
| 11 | **The graphics plane.** `RegMode` graphics; `CmdGfxRectFill`/`FillCircle` on $C000–$DFFF. | `LDA #1` `STA RegMode` `… RegRect* …` `LDA #CmdGfxRectFill` `STA RegCmd` | **quad.s, balls.s** | `Frame()` graphics mode + mode switch |
| 12 | **Reading the machine.** Use go6asm disassembly-with-comments + the analyzer to find a planted bug (jump into data / write to ROM / bad vector). | a deliberately broken variant | (any) | analyzer surface, breakpoint, step, mem/disasm views |

Progressive-disclosure thread: lessons 0–7 are Layer-0 (just
instructions). Lesson 8 is where the learner first writes a directive — to
*take over* a vector — which motivates go6asm §11 layering naturally.

## Facade capabilities (derived from the table — now folded into the backplane spec)

> **Superseded.** This list is the *evidence* (each capability traced to
> ≥1 lesson) that fed the **instrument API** in
> [`architecture-backplane.md`](architecture-backplane.md). The
> authoritative step-2 spec is that doc; the API below is no longer a
> standalone `Machine` facade but the instrument client surface over the
> backplane. Kept here as the requirements trail.

```
New(cfg) *Machine          // cfg = sim-tui map (RAM/VIC/VIA/ROM, vectors)
Load(origin uint16, b []byte)   // bytes from go6asm output
Reset()

Step(n int) StepResult     // L0,1,2,5,8 — single/few instruction stepping
Run(opts RunOpts) RunResult// L4,7,9,10,11 — maxCycles | untilBreakpoint | untilHalt | paced
State() CPUState           // L0,1,4,7 — A X Y SP PC P(flags) + cycle count
Mem(lo, hi uint16) []byte  // L2,3,5 — span read (MemWrite: optional, lab "pokes")
Frame() Framebuffer        // L3,6,9,10,11 — color+char planes (40×13),
                           //   graphics plane (160×100 4bpp), current RegMode
SetBreakpoint(addr uint16) / ClearBreakpoint(addr)   // L4,8,12
                           // Run stops with RunResult.Reason + addr
Vectors() VectorInfo       // L8 — reset/NMI/IRQ targets; pending IRQ/NMI;
                           //   support "break when an interrupt is taken"
```

Cross-cutting requirements the lessons impose:

- **Paced run for the browser.** L9–11 animate. The facade must let the
  wasm bridge drive cadence — either `Run` returns after a frame/quantum
  and the RAF loop re-calls it, or a step-budget per tick. Decide in
  step 2; do **not** bake a wall-clock sleep into the core (kills
  determinism + wasm).
- **Determinism.** `Step`/`Run` reproducible given the same program and
  budget → golden-trace tests (the core becomes testable the moment it's
  behind this facade — that *is* the deep cleanup).
- **Two CPU cores.** go6sim already has interp + netsim adapters; the
  facade picks one via `cfg` (default interp; netsim as a lesson
  "now watch it at the transistor level" reveal — optional, post-v1).
- **Snapshot/restore** (optional, post-v1): lesson "reset to this
  checkpoint" without re-running from $E000. Not required for v1.

The **analyzer/disassembly** (lesson 12) is go6asm-side, not the facade —
the `<CodeLab>` already has go6asm's output (disasm + diagnostics + source
comments). The facade only needs `Mem`/`Vectors`/breakpoints so the lab
can correlate a finding with runtime state.

## Lab mechanics & delivery (informs steps 4–5)

- Starter `.s` for lessons live in this repo so they stay in lockstep with
  the machine: `docs/learn/labs/NN-*.s`. Capstones reference
  `internal/demos/asmsrc/*.s` directly (already the regression corpus).
- `<CodeLab>` assembles via the existing go6asm `asm-worker.js`
  (`target: "sim-tui"`, Layer-0), feeds bytes to the `cmd/6502-core-wasm`
  bridge over the facade, renders `Frame()` to a canvas + a register strip
  + a memory peek + go6asm's disasm/analyzer panels.
- Site Pages workflow builds both wasms from tagged repos into
  `public/sim/` (`go6asm@vX`, `go6sim@vY`) — no committed binaries,
  versioned, same pattern as go6asm's `pages.yml`. (go6sim's *first* tag
  is cut when the site first pins it — not before.)

## Scope / non-goals (v1)

- Not a course on the whole 6502 ISA; it teaches *this machine* by doing.
- No save/share of learner programs in v1 (URL-encode later if wanted).
- No netsim CPU in the default path (perf); offered as an optional reveal.
- 6502 only. No 65C02/65816 lessons until go6asm ships those ISAs.

## Open questions

Authoritative list lives in
[`architecture-backplane.md`](architecture-backplane.md). Status:

- Q1 paced-run, Q2 display payload, Q3 lesson-8 interrupt depth —
  **resolved** (see that doc).
- **Q4 — open, content-org (not architecture):** where the path
  index/track sits in the existing `/learn` IA (LessonLayout prev/next
  already exists). Decide when content is authored.
