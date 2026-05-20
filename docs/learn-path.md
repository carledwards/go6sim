# go6sim — Learn Path

> Status: **curriculum plan — re-grounded on what is built (2026-05-18).**
> The engine is done: `machine.TeachMin` preset, the `instrument` API,
> and `cmd/6502-core-wasm` (see
> [`architecture-backplane.md`](architecture-backplane.md) for the
> engine spec). The `<CodeLab>` component is live in thecarledwards-site
> and proven end-to-end (8-LED controller). This doc now defines the
> **lesson sequence** the site authors against — concrete, no
> speculative API.

## What this is

An ordered set of interactive lessons on **thecarledwards.com/learn**.
Each lesson is a page that embeds `<CodeLab seed="…" />`: the learner
edits 6502 assembly, **go6asm** assembles it in-browser (`target
sim-tui`, Layer-0), and **go6sim** (`teach-min`) runs it, with focused
views — **8 LEDs on VIA Port B**, CPU registers, the go6asm
disassembly-with-comments, a concise error panel (line-jump +
expandable detail), and step/breakpoint controls.

Lab seeds are authored per lesson and live with the lessons in
thecarledwards-site (passed as the `<CodeLab seed>` prop) — *not* in
go6sim. The proven demo corpus (`internal/demos/asmsrc/*.s`) stays a
standalone artifact (proof-of-system, the go6asm byte-equivalence
backstop) — independent of the curriculum.

## The machine every lesson assumes — `teach-min`

go6asm Layer-0 infers load address `$E000` and synthesizes the `$FFFA`
vector block, so early lessons are *just instructions* — no directives,
no addresses. `machine.TeachMin` is deliberately minimal: **no VIC
Controller, no graphics, no command protocol** — a dumb byte-visible
machine (resolved Q2). The protocol-bearing VIC lives only in the
standalone `vic-demo` preset, not here.

```
$00–$0F      reserved scratch (system convention)
$10–$FF      user zero page (fast indexed addressing)
$0000–$1FFF  RAM (8 KB)
$A000–$AFFF  framebuffer RAM region (raw bytes; a future pixel/char
             display renders these — v1 the rendered device is the LEDs)
$B000–$B0FF  6522 VIA #1  — Port B = the 8 LEDs; T1 = the timer
$E000–$FFFF  ROM (program here; reset vector $FFFC; IRQ/BRK $FFFE)
```

go6asm's `sim-tui` target auto-provides these (no `.import`). Only the
VIA matters for `teach-min` lessons:

| Group | Symbols |
|---|---|
| VIA port | `ViaBase $B000` — **Port B (ORB) = the 8 LEDs** (store a byte → LEDs) |
| VIA T1   | `ViaT1C_L $B004` `ViaT1C_H $B005` `ViaT1L_L $B006` `ViaT1L_H $B007` `ViaACR $B00B` |
| VIA IRQ  | `ViaIFR $B00D` `ViaIER $B00E` `ViaT1Bit $40` |

(`ColorBase $A000` / `CharBase $A400` resolve as plain framebuffer RAM
in `teach-min` — reserved for a later pixel/char display arc; lessons
don't use them in v1.)

## The runtime surface lessons use (shipped — `<CodeLab>` → core-wasm)

Concrete, done — the instrument API exposed by `cmd/6502-core-wasm`:
`load · reset · step(n) · setRunning · advance(ms)` (RAF run) `· state ·
peek · poke · mem(lo,hi) · taps · setSpeedHz · setBreakpoint ·
clearBreakpoint · breakOnVector · runUntil`. Each lesson exercises a
subset; the column below names it.

## The lesson sequence

Seed = the starter program shipped in `<CodeLab seed>`. *Sees* = what
the learner observes/uses in the lab.

| # | Lesson | Seed sketch | Sees |
|---|--------|-------------|------|
| 0 | **Orientation.** The machine = CPU + RAM + a 6522 VIA. The toolchain: you write asm → go6asm assembles → go6sim runs. Run one instruction. | `LDA #$2A` | load, step(1), state |
| 1 | **Registers & immediates.** `LDA/LDX/LDY #imm`, `TAX/TAY`; watch A/X/Y. | `LDA #$01` / `LDX #$10` / `TAX` | step(n), state |
| 2 | **Memory & zero page.** `STA`/`LDA` abs + zp; write RAM, read it back. | `LDA #$42` / `STA $10` / `LDA $10` | mem/peek, step |
| 3 | **The 8 LEDs are one byte** (the "aha"). Store a byte to `ViaBase` → the LEDs light. Pure memory-mapped I/O — *the same store runs on the real lcm-32 board*. | `LDA #%01011010` / `STA ViaBase` | the LED view, peek($B000) |
| 4 | **Loops & branches.** A binary counter on the LEDs (the CodeLab default). `INX`, `CPX`, `BNE`. | the 8-LED counter | run, breakpoint, cycles |
| 5 | **Shift & mask.** A *walking* lit LED — `ASL`/`LSR`/`ROL` ("shift left one dot"). The bit-manipulation lesson. | `LDA #$01` / `lp: STA ViaBase` / `ASL` / `BNE lp` / restart | step, the LED view |
| 6 | **Indexed addressing & tables.** Drive the LEDs from a pattern table. | `ldx#0` / `c: lda patt,x` / `sta ViaBase` / `inx` / `cpx#N` / `bne c` / `patt: .byte …` | mem span, step into loop |
| 7 | **Timing with the VIA T1.** Free-run T1, poll `ViaIFR & ViaT1Bit` to pace the counter so it visibly steps. | counter + `set ACR/T1L*`; `w: BIT ViaIFR` / `BVC w` | long run, taps (`via1.t1`/`irq`) |
| 8 | **Interrupts (real).** Take over the IRQ vector (Layer-1), VIA T1 fires → your ISR advances the LEDs → `RTI`. **Break-on-IRQ**, then single-step the ISR. (Delivered on interp *and* netsim — brick 9b.) | `.segment`/vector + ISR; `CLI` | break-on-vector, step-into-ISR, taps |
| 9 | **Reading the machine.** A planted bug (jump into data / write to ROM / bad vector). Use the disassembly-with-comments + the error panel + breakpoints/`runUntil` to find it. | a deliberately broken counter | disasm, errors, runUntil, step |

Progressive disclosure: **lessons 0–7 are Layer-0** (just instructions).
**Lesson 8** is the first directive — *taking over a vector* — which
motivates go6asm §11 layering exactly when it's needed.

Deferred (NOT v1, NOT `teach-min`): a pixel/char display arc (the
framebuffer-RAM region rendered) and the VIC's command/double-buffer/
tearing model — those belong to a future present-capable display or the
`vic-demo` machine, sequenced *after* these basics (resolved Q2).

## Engine spec / requirements trail

The instrument API and all engine decisions (paced run, determinism,
dual CPU, hardware IRQ, breakpoints) are **resolved and built** — see
[`architecture-backplane.md`](architecture-backplane.md). This doc no
longer carries a speculative `Machine` facade.

## Scope / non-goals (v1)

- Teaches *this machine* by doing — not a full 6502 ISA course.
- `teach-min` only: VIA-PB LEDs are the rendered device; framebuffer-RAM
  pixel/char display + VIC command model are a later arc.
- interp by default; netsim is an optional "watch it at the transistor
  level" reveal (it now also takes the VIA IRQ — brick 9b).
- No save/share of learner programs in v1.
- 6502 only (no 65C02/65816 until go6asm ships those ISAs).

## Open question

- **Q4 — content-org (not architecture):** where the lesson track sits
  in thecarledwards-site `/learn` IA (its `LessonLayout` already has
  prev/next). Decide as the first lessons are authored.
