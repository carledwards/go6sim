// Package instrument is the single control/observe surface over a
// go6sim machine — the one API every edge (the TUI, the web <CodeLab>,
// mcp-go6sim, a future debugger CLI) talks to. It composes a
// *backplane.Backplane (the cards + bus) and a *clock.Driver (run/step/
// speed) so callers never hand-wire the CPU loop.
//
// Litmus (docs/architecture-backplane.md): a debugger CLI — step / dump
// / peek / poke / go — must be expressible on this surface. It is:
// Step/StepCycle = s/t, Mem = dump, Peek/Poke = peek/poke, SetRunning =
// g/.
//
// Brick 6 of the backplane carve: additive, no existing code changed.
// Program loading is deferred to the preset brick (presets own the ROM
// card); this brick is pure control + observation.
package instrument

import (
	"time"

	"github.com/carledwards/go6sim/backplane"
	"github.com/carledwards/go6sim/bus"
	"github.com/carledwards/go6sim/clock"
)

// State is a snapshot of architectural + clock state for observers.
type State struct {
	A, X, Y, S, P uint8
	PC            uint16
	HalfCycles    uint64
	Running       bool
	SpeedHz       int
}

// Instrument wraps a machine for control and observation.
type Instrument struct {
	bp       *backplane.Backplane
	drv      *clock.Driver
	bps      map[uint16]bool
	brkOnVec bool
}

// New builds an instrument over an already-composed backplane and a
// clock driver (the driver carries the cpu.Backend).
func New(bp *backplane.Backplane, drv *clock.Driver) *Instrument {
	return &Instrument{bp: bp, drv: drv, bps: map[uint16]bool{}}
}

// RunResult reports why RunUntil stopped.
//
//	Reason: "breakpoint" | "vector" | "budget"
//	Addr:   PC at the stop
type RunResult struct {
	HalfCycles uint64
	Reason     string
	Addr       uint16
}

// Tick advances peripheral virtual time by dt, delegating to the
// backplane. Useful for callers that drive the CPU via RunUntil (which
// is CPU-only, by design — see its godoc) and need to keep peripherals
// synchronised. The standard pairing in a manual driver loop is:
//
//	r := inst.RunUntil(slice)
//	inst.Tick(virtualDuration(r.HalfCycles, speedHz))
//
// The TUI's tick path uses inst.Advance(dt), which does both in
// lockstep for callers that don't care about per-half-step bp/vector
// stops.
func (i *Instrument) Tick(dt time.Duration) { i.bp.Tick(dt) }

// SetBreakpoint arms an instruction-address breakpoint.
func (i *Instrument) SetBreakpoint(addr uint16) { i.bps[addr] = true }

// ClearBreakpoint removes one address breakpoint.
func (i *Instrument) ClearBreakpoint(addr uint16) { delete(i.bps, addr) }

// ClearBreakpoints removes all address breakpoints.
func (i *Instrument) ClearBreakpoints() { i.bps = map[uint16]bool{} }

// BreakOnVector toggles "stop when an interrupt vector is taken". 9a
// observes BRK (the only vectoring interp models); hardware-IRQ
// delivery is a separate, deferred feature.
func (i *Instrument) BreakOnVector(on bool) { i.brkOnVec = on }

// RunUntil is the deterministic debugger run: it advances the CPU one
// half-cycle at a time (so it is interruptible, unlike the batched
// free-run Advance), up to maxHalf half-cycles, stopping early on a
// vector-taken event or an armed address breakpoint (checked at the
// SYNC instruction boundary). This is what a debugger CLI's `go`
// drives; pair with Step for step-into-ISR.
func (i *Instrument) RunUntil(maxHalf int) RunResult {
	be := i.drv.Backend

	var brkRead func() uint64
	if tp, ok := be.(bus.Tappable); ok {
		for _, t := range tp.Taps() {
			if t.Name == "brk" {
				brkRead = t.Read
			}
		}
	}
	prevBrk := uint64(0)
	if brkRead != nil {
		prevBrk = brkRead()
	}

	for n := 0; n < maxHalf; n++ {
		be.HalfStep()
		// Fire the Driver's OnHalfStep observer — same callback the
		// driver's own halfStep wrapper invokes for Advance / StepOne /
		// StepInstruction. RunUntil bypasses that wrapper for efficiency
		// (one less indirection per cycle in the hot loop), but the
		// scope window relies on the callback to ring-buffer per-cycle
		// bus state. Without this, the scope only updates during single-
		// step, never during free-run.
		if i.drv.OnHalfStep != nil {
			i.drv.OnHalfStep()
		}
		if i.brkOnVec && brkRead != nil && brkRead() != prevBrk {
			return RunResult{uint64(n + 1), "vector", be.Registers().PC}
		}
		if be.SYNC() {
			if pc := be.Registers().PC; i.bps[pc] {
				return RunResult{uint64(n + 1), "breakpoint", pc}
			}
		}
	}
	return RunResult{uint64(maxHalf), "budget", be.Registers().PC}
}

// Taps aggregates every card's read-only observables (bus.Tappable),
// keyed "<card>.<tap>", plus the CPU under "cpu.<tap>". The web/MCP/
// debugger surfaces render this uniformly without knowing devices.
func (i *Instrument) Taps() map[string]uint64 {
	out := map[string]uint64{}
	for _, c := range i.bp.Components() {
		if tp, ok := c.(bus.Tappable); ok {
			for _, t := range tp.Taps() {
				out[c.Name()+"."+t.Name] = t.Read()
			}
		}
	}
	if tp, ok := i.drv.Backend.(bus.Tappable); ok {
		for _, t := range tp.Taps() {
			out["cpu."+t.Name] = t.Read()
		}
	}
	return out
}

// Reset returns the machine to power-on state: every Resettable card,
// then the CPU + clock counters. (clock.Driver.Reset deliberately keeps
// the running flag — hardware reset-button semantics.)
func (i *Instrument) Reset() {
	i.bp.Reset()
	i.drv.Reset()
}

// Step advances n whole instructions. No-op while running — the clock
// owns execution then. (Debugger `s`; lesson single-stepping.)
//
// Peripherals are ticked proportionally to half-cycles consumed by
// each instruction. Without this, the VIA's T1 timer, the display
// controller, and other Ticker cards see zero virtual time during a
// step — programs that wait on IRQs (timer-driven video updates,
// keypress polling) stall and the user observes "I stepped 100 but
// nothing happened on screen."
func (i *Instrument) Step(n int) {
	for k := 0; k < n; k++ {
		before := i.drv.Backend.HalfCycles()
		i.drv.StepInstruction()
		i.tickPeripherals(i.drv.Backend.HalfCycles() - before)
	}
}

// FinishInstruction advances the CPU through the rest of any
// in-flight instruction so PC lands on a clean SYNC boundary. No-op
// when SYNC is already true (the case for breakpoint halts, which
// trigger AT a SYNC). Bounded by the same 32-half-cycle ceiling
// StepInstruction uses, so a pathological backend can't lock the
// caller.
//
// Used by Hub.CmdStop after free-run halts: without this, the next
// user step from PC=mid-instruction only completes the in-flight
// fetch/execute, so the user perceives "I had to step twice."
func (i *Instrument) FinishInstruction() {
	be := i.drv.Backend
	if be.SYNC() {
		return
	}
	for n := 0; n < 32; n++ {
		i.drv.StepOne()
		if be.SYNC() {
			return
		}
	}
}

// StepCycle advances n half-cycles. (Debugger `t`; cycle-level lessons.)
// Peripherals tick proportionally — same rationale as Step.
func (i *Instrument) StepCycle(n int) {
	for k := 0; k < n; k++ {
		before := i.drv.Backend.HalfCycles()
		i.drv.StepOne()
		i.tickPeripherals(i.drv.Backend.HalfCycles() - before)
	}
}

// tickPeripherals translates the given half-cycle count into virtual
// wall-clock time (at the driver's configured Hz, defaulting to
// 1 MHz when unpaced) and ticks the backplane's Ticker cards. Used
// by Step / StepCycle so peripherals don't freeze during debugger
// single-stepping.
func (i *Instrument) tickPeripherals(halves uint64) {
	if halves == 0 {
		return
	}
	hz := i.drv.Speed().Hz
	if hz <= 0 {
		hz = 1_000_000
	}
	dt := time.Duration(halves) * time.Second / time.Duration(2*hz)
	i.bp.Tick(dt)
}

// SetRunning starts/stops free-run. Run-loop drivers then call Advance.
func (i *Instrument) SetRunning(on bool) { i.drv.SetRunning(on) }

// Running reports whether the clock is free-running.
func (i *Instrument) Running() bool { return i.drv.Running() }

// Driver exposes the clock driver for speed/batch configuration and the
// clock UI (the TUI's clock window, web speed control). The Instrument
// is the primary surface; this is the deliberate escape hatch for clock
// tuning, mirroring backplane's Trace() hatch.
func (i *Instrument) Driver() *clock.Driver { return i.drv }

// Backplane returns the underlying *backplane.Backplane so callers
// driving the system from outside (Hub.CmdIRQ for the debugger
// console line, future host-NMI plumbing) can reach the capability
// surface without piercing every layer. Like Driver, this is an
// intentional escape hatch — most code should stay above it.
func (i *Instrument) Backplane() *backplane.Backplane { return i.bp }

// Advance is the budget-tick clock driver (resolved open-question #1):
// it steps the CPU for the window AND ticks the bus/peripherals by the
// SAME window in lockstep — the pairing that cmd/6502-{sim,wasm}
// currently hand-duplicate (clockProv.Advance + b.Tick). Every run loop
// (TUI, web RAF, MCP) calls only this. Returns half-steps run.
func (i *Instrument) Advance(dt time.Duration) int {
	n := i.drv.Advance(dt)
	i.bp.Tick(dt)
	return n
}

// State snapshots registers + clock for observers.
func (i *Instrument) State() State {
	be := i.drv.Backend
	r := be.Registers()
	return State{
		A: r.A, X: r.X, Y: r.Y, S: r.S, P: r.P, PC: r.PC,
		HalfCycles: be.HalfCycles(),
		Running:    i.drv.Running(),
		SpeedHz:    i.drv.Speed().Hz,
	}
}

// Peek reads a byte WITHOUT stamping the bus read-trace — inspection
// must not look like the CPU touched memory (matches the codebase's
// existing untraced-innerBus convention for the memory windows).
func (i *Instrument) Peek(addr uint16) uint8 {
	return i.bp.Trace().Inner().Read(addr)
}

// Mem returns bytes in [lo, hi] inclusive via untraced reads. A
// "framebuffer"/display read is just Mem over the configured region —
// resolved open-question #2: the sim ships bytes, there is no display
// card in teach-min. Returns nil if hi < lo.
func (i *Instrument) Mem(lo, hi uint16) []byte {
	if hi < lo {
		return nil
	}
	inner := i.bp.Trace().Inner()
	out := make([]byte, int(hi)-int(lo)+1)
	for a := int(lo); a <= int(hi); a++ {
		out[a-int(lo)] = inner.Read(uint16(a))
	}
	return out
}

// Poke writes a byte through the traced bus: a debugger poke is a real
// mutation, and surfacing it in the UI write-trace is informative
// (unlike inspection reads, which must stay invisible).
func (i *Instrument) Poke(addr uint16, v uint8) {
	i.bp.Write(addr, v)
}
