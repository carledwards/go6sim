// Package clock is the core clock-driver seam: it owns run/step/speed
// and advances a cpu.Backend, with no UI dependency (tcell-free,
// wasm-clean). It is one realization of "the clock is a card/driver"
// (docs/architecture-backplane.md): StepOne/StepInstruction are the
// single-step drivers; Advance is the free-run / budget-tick driver
// (the run loop calls it on a cadence). ui/clockwin embeds *Driver and
// adds only the tcell view; the instrument API (next brick) drives the
// same Driver. Logic moved verbatim from ui/clockwin in the backplane
// carve — behaviour unchanged.
package clock

import (
	"time"

	"github.com/carledwards/go6sim/cpu"
)

// Speed is a target full-cycle clock rate. Hz of 0 means "as fast as
// the host can manage" (still gated by the per-tick batch budget).
type Speed struct {
	Label string
	Hz    int
}

var Speeds = []Speed{
	{"1Hz", 1},
	{"10Hz", 10},
	{"100Hz", 100},
	{"1kHz", 1000},
	{"10kHz", 10000},
	{"100kHz", 100000},
	{"1MHz", 1000000},
	{"Max", 0},
}

// DefaultMaxBatchPerTick is the default ceiling on HalfStep calls per
// Advance() invocation. Bounds UI-thread stall during Max mode and acts
// as a safety net for very high target Hz. Override per Driver via the
// MaxBatch field.
const DefaultMaxBatchPerTick = 500

// Driver runs/steps a cpu.Backend at a selectable speed. No goroutines —
// all work runs on the caller's loop.
type Driver struct {
	Backend cpu.Backend

	// MaxBatch caps the number of HalfStep calls per Advance(). 0
	// means use DefaultMaxBatchPerTick. Lift it for fast backends
	// (interp) that aren't capped by the per-tick budget; lower it
	// if the UI feels janky in Max mode.
	MaxBatch int

	// OnHalfStep, if non-nil, fires after every Backend.HalfStep()
	// call regardless of which path triggered it (Advance, StepOne,
	// StepInstruction). Use it to feed a per-cycle observer such as
	// the scope window's bus-trace ring buffer. Kept hot — the
	// closure is invoked once per CPU half-cycle, so the
	// implementation should be cheap.
	OnHalfStep func()

	speedIdx  int
	running   bool
	accum     float64 // fractional half-cycles owed
	stepsDone uint64
}

// halfStep advances the backend by one half-cycle and fires the
// optional OnHalfStep hook. All internal callers (Advance / StepOne /
// StepInstruction) route through here so observers see every step
// regardless of trigger source.
func (d *Driver) halfStep() {
	d.Backend.HalfStep()
	if d.OnHalfStep != nil {
		d.OnHalfStep()
	}
}

// NewDriver returns a Driver with a sensible default speed (10 Hz —
// slow enough to watch, fast enough to see progress).
func NewDriver(backend cpu.Backend) *Driver {
	d := &Driver{Backend: backend}
	for i, sp := range Speeds {
		if sp.Hz == 10 {
			d.speedIdx = i
			break
		}
	}
	return d
}

// referenceTick is the canonical "one frame" duration the MaxBatch
// figure is calibrated against. Run loops that sub-divide their
// app.Tick callback into smaller slices pass the slice's elapsed, and
// Advance scales the cap proportionally — so a 50 ms app.Tick split
// into ten 5 ms sub-ticks runs the same total batch as one 50 ms call,
// just spread across ten interleaved CPU/bus rounds.
const referenceTick = 50 * time.Millisecond

// Advance is called by the run loop on a fixed cadence. It executes
// HalfSteps for the given window and returns how many it actually ran.
// Callers driving peripherals from the same loop should use the return
// value (× 500 ns at 1 MHz nominal) for those peripherals' Tick budget
// so the bus stays in lockstep with the CPU clock. Returns 0 when
// paused or speed gates the next step out of this window.
func (d *Driver) Advance(elapsed time.Duration) int {
	if !d.running {
		return 0
	}
	cap := d.MaxBatch
	if cap <= 0 {
		cap = DefaultMaxBatchPerTick
	}
	hz := Speeds[d.speedIdx].Hz
	if hz == 0 {
		// Max mode: run cap HalfSteps per referenceTick, scaled by the
		// actual elapsed window. Without this scaling, sub-tick run
		// loops would multiply the effective rate by their slice count.
		n := int(float64(cap) * float64(elapsed) / float64(referenceTick))
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			d.halfStep()
		}
		d.stepsDone += uint64(n)
		return n
	}
	d.accum += float64(hz*2) * elapsed.Seconds()
	n := int(d.accum)
	d.accum -= float64(n)
	if n > cap {
		n = cap
	}
	for i := 0; i < n; i++ {
		d.halfStep()
	}
	d.stepsDone += uint64(n)
	return n
}

// StepOne advances exactly one half-cycle (no-op while running).
func (d *Driver) StepOne() {
	if d.running {
		return
	}
	d.halfStep()
	d.stepsDone++
}

// StepInstruction runs HalfSteps until PC changes from its entry value.
// Bounded by maxStepHalves so a stuck CPU can't lock the caller. For
// interp this advances exactly one instruction; for netsim it advances
// to the next PC change.
const maxStepHalves = 32

func (d *Driver) StepInstruction() {
	if d.running {
		return
	}
	start := d.Backend.Registers().PC
	for i := 0; i < maxStepHalves; i++ {
		d.halfStep()
		d.stepsDone++
		if d.Backend.Registers().PC != start {
			return
		}
	}
}

// Reset performs a CPU-and-counters reset — Backend.Reset, plus zeroing
// the fractional-cycle accumulator and the steps-done counter. The
// running flag is intentionally NOT touched: modeled on a real hardware
// reset button, which doesn't stop the system clock.
func (d *Driver) Reset() {
	d.accum = 0
	d.stepsDone = 0
	d.Backend.Reset()
}

func (d *Driver) Running() bool        { return d.running }
func (d *Driver) Speed() Speed         { return Speeds[d.speedIdx] }
func (d *Driver) SetRunning(on bool)   { d.running = on; d.accum = 0 }
func (d *Driver) CycleSpeed(delta int) { d.cycleSpeed(delta) }

// StepsDone is the cumulative half-cycle count (UI status / instrument).
func (d *Driver) StepsDone() uint64 { return d.stepsDone }

// SpeedIndex is the current index into Speeds (UI highlight).
func (d *Driver) SpeedIndex() int { return d.speedIdx }

// EffectiveBatch returns the active per-tick HalfStep cap, falling back
// to the package default when MaxBatch is unset.
func (d *Driver) EffectiveBatch() int {
	if d.MaxBatch > 0 {
		return d.MaxBatch
	}
	return DefaultMaxBatchPerTick
}

// SetSpeedHz selects the speed entry whose Hz matches. Returns false if
// no entry matches.
func (d *Driver) SetSpeedHz(hz int) bool {
	for i, sp := range Speeds {
		if sp.Hz == hz {
			d.speedIdx = i
			d.accum = 0
			return true
		}
	}
	return false
}

func (d *Driver) cycleSpeed(delta int) {
	d.speedIdx = (d.speedIdx + delta + len(Speeds)) % len(Speeds)
	d.accum = 0
}
