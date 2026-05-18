# go6sim Roadmap

A floating-window 6502 simulator built on top of `foxpro-go`. The
simulator hosts pluggable components (RAM, ROM, display, etc.) on a
shared bus, can run either the transistor-level netsim core or a
traditional interpretive core, and exposes a debugger UI.

Dependencies (consumed, never forked):

- `github.com/carledwards/foxpro-go` — TUI framework
- `github.com/carledwards/6502-netsim-go` — transistor-level CPU core

This document is the working item list. Check things off as they land.
Keep entries short; one line per task. Cross-cutting design lives in
`architecture.md`.

## Phase 0 — Repo skeleton ✓

- [x] `go mod init github.com/carledwards/go6sim`
- [x] Pull `foxpro-go` and `6502-netsim-go` as deps (local replace)
- [x] `cmd/6502-sim/main.go` — minimal app boots, shows a "Hello 6502" window
- [x] `Makefile` — `build`, `run`, `tidy`, `test`, `clean`
- [x] README with quickstart

## Phase 1 — Bus + component registry ✓

Single source of truth for memory-mapped IO. Replaces the hardcoded
`if addr < ramSize ... else if addr >= romBase` logic in netsim's
`motherboard.go`.

- [x] `bus.Bus` interface — `Read(addr uint16) uint8`, `Write(addr uint16, val uint8)`
- [x] `bus.Component` interface — `Base() uint16`, `Size() int`, `Read`, `Write`, `Name()`
- [x] Range-based dispatcher (overlap detection on register)
- [x] `components/ram` (read+write)
- [x] `components/rom` (read-only, `Load([]byte)`, `SetResetVector`)
- [x] Unit tests: register, overlap rejection, read/write dispatch
- [x] Integration test: real RAM+ROM through the bus

## Phase 2 — CPU backend abstraction ✓

- [x] `cpu.Backend` interface (see `architecture.md`)
- [x] `cpu.Registers` + flag bit constants
- [x] `cpu/netsim` — adapter wrapping `6502-netsim-go`
- [x] End-to-end smoke test: bus + RAM + ROM + adapter, runs a 7-instruction program
- [ ] `cpu/interp` — deferred to Phase 6 (real interpretive impl)
- [ ] Backend selection via flag / config — deferred to Phase 3 (needs UI wiring)

## Phase 3 — First windows + clock ✓

Goal: visible single-stepping of the netsim core.

- [x] CPU registers window (A/X/Y/S/PC/P, flag bits, cycles, half-cycles)
- [x] RAM hex dump window — scrollable, configurable base + length
- [x] ROM hex dump window (same provider, mapped to ROM region)
- [x] Clock window — Stop / Step / Run, 5-speed selector (1, 10, 100, 1k Hz, Max)
- [x] `App.Tick` drives Run mode (no goroutines — all on UI thread)
- [x] Menu: File → Reset CPU / Command Window / Exit; Run → Run / Stop / Step
- [x] Test program baked in: `LDA #$42 / LDX #$11 / LDY #$22 / ST*` + spin
- [ ] Load ROM from file — deferred to Phase 7

## Phase 4 — Debugger window

- [x] Disassembler (`disasm` package, 151 official 6502 opcodes, all 13 addressing modes)
- [x] Disasm window: live decode of a configurable region, current PC line highlighted, auto-scroll, manual scroll
- [x] Tests for disassembly (demo program, branch relative, every addressing mode)
- [ ] Breakpoint list (PC equals) — needs SYNC accessor in netsim to detect instruction boundaries
- [ ] Step-instruction (Step Over) — same precondition as breakpoints
- [ ] Watch list (memory addresses, refreshed each tick) — RAM window already serves; revisit if needed

## Phase 5 — Display component

- [ ] `components/display` — memory-mapped framebuffer
- [ ] Display window — renders the region as chars (and/or pixel cells)
- [ ] Configurable base addr + width/height in config dialog

## Phase 6 — Interpretive CPU core ✓

- [x] `cpu/interp` — full 151 official 6502 opcodes, 13 addressing modes
- [x] Instruction-grained execution; HalfStep emulates real cycle counts so bus reads/writes happen at the right *count* (not the right *phase* — netsim is the choice for cycle-phase accuracy)
- [x] Tests: demo program parity, ADC carry chain, JSR/RTS round-trip
- [x] `-cpu=netsim|interp` flag at startup; CPU window title reflects choice
- [ ] Runtime backend swap (mid-session) — deferred; would need bus-state translation between backends

## Phase 7 — Polish

- [ ] Save/load snapshot (RAM + CPU regs)
- [ ] ROM file loader dialog
- [ ] Per-component config windows (base addr, size)
- [ ] Settings persistence (window layout, last ROM, theme)
- [ ] Demo programs in `examples/`

## Out of scope (for now)

- Cartridge / mapper emulation
- Audio
- Cycle-exact peripheral timing beyond what each component needs
- Multi-CPU systems
