// Package clockwin owns the simulator's run/step/stop UX and drives
// the half-cycle advance from App.Tick. No goroutines — all work
// runs on the foxpro-go event loop.
package clockwin

import (
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"

	foxpro "github.com/carledwards/foxpro-go"
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

// DefaultMaxBatchPerTick is the default ceiling on HalfStep calls
// per Advance() invocation. Bounds UI-thread stall during Max mode
// and acts as a safety net for very high target Hz. Override per
// Provider via the MaxBatch field.
const DefaultMaxBatchPerTick = 500

const (
	MinW = 32
	MinH = 7
)

type Provider struct {
	Backend cpu.Backend
	foxpro.ScrollState

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
// optional OnHalfStep hook. All internal callers (Advance / StepOne
// / StepInstruction) route through here so observers see every
// step regardless of trigger source.
func (p *Provider) halfStep() {
	p.Backend.HalfStep()
	if p.OnHalfStep != nil {
		p.OnHalfStep()
	}
}

// NewProvider returns a Provider with a sensible default speed
// (10 Hz — slow enough to watch, fast enough to see progress).
func NewProvider(backend cpu.Backend) *Provider {
	p := &Provider{Backend: backend}
	for i, sp := range Speeds {
		if sp.Hz == 10 {
			p.speedIdx = i
			break
		}
	}
	return p
}

// referenceTick is the canonical "one frame" duration the MaxBatch
// figure is calibrated against. Run loops that sub-divide their
// app.Tick callback into smaller slices pass the slice's elapsed,
// and Advance scales the cap proportionally — so a 50 ms app.Tick
// split into ten 5 ms sub-ticks runs the same total batch as one
// 50 ms call, just spread across ten interleaved CPU/bus rounds.
const referenceTick = 50 * time.Millisecond

// Advance is called by the run loop on a fixed cadence.
// Advance executes HalfSteps for the given wall-clock window and
// returns how many it actually ran. Callers driving peripherals from
// the same loop should use the return value (× 500 ns at 1 MHz
// nominal) for those peripherals' Tick budget so the bus stays in
// lockstep with the CPU clock — see cmd/6502-{sim,wasm}/main.go.
// Returns 0 when the clock is paused or speed gates the next step
// out of this window.
func (p *Provider) Advance(elapsed time.Duration) int {
	if !p.running {
		return 0
	}
	cap := p.MaxBatch
	if cap <= 0 {
		cap = DefaultMaxBatchPerTick
	}
	hz := Speeds[p.speedIdx].Hz
	if hz == 0 {
		// Max mode: run cap HalfSteps per referenceTick, scaled by
		// the actual elapsed window. Without this scaling, sub-tick
		// run loops would multiply the effective rate by their slice
		// count (e.g. 10× sub-ticks ⇒ 10× faster CPU).
		n := int(float64(cap) * float64(elapsed) / float64(referenceTick))
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			p.halfStep()
		}
		p.stepsDone += uint64(n)
		return n
	}
	p.accum += float64(hz*2) * elapsed.Seconds()
	n := int(p.accum)
	p.accum -= float64(n)
	if n > cap {
		n = cap
	}
	for i := 0; i < n; i++ {
		p.halfStep()
	}
	p.stepsDone += uint64(n)
	return n
}

// StepOne advances exactly one half-cycle. Wired to the [T]ick UI.
func (p *Provider) StepOne() {
	if p.running {
		return
	}
	p.halfStep()
	p.stepsDone++
}

// StepInstruction runs HalfSteps until PC changes from its entry
// value. Bounded by maxStepHalves so a stuck CPU can't lock the UI.
// For interp this advances exactly one instruction; for netsim it
// advances to the next PC change (typically partway through an
// instruction but always perceptible).
const maxStepHalves = 32

func (p *Provider) StepInstruction() {
	if p.running {
		return
	}
	start := p.Backend.Registers().PC
	for i := 0; i < maxStepHalves; i++ {
		p.halfStep()
		p.stepsDone++
		if p.Backend.Registers().PC != start {
			return
		}
	}
}

// Reset performs a CPU-and-counters reset — Backend.Reset, plus
// zeroing the fractional-cycle accumulator and the steps-done
// counter. The running flag is intentionally NOT touched: this is
// modeled on a real hardware reset button, which doesn't stop the
// system clock. Callers that want a hard stop should call
// SetRunning(false) explicitly before or after.
func (p *Provider) Reset() {
	p.accum = 0
	p.stepsDone = 0
	p.Backend.Reset()
}

func (p *Provider) Running() bool        { return p.running }
func (p *Provider) Speed() Speed         { return Speeds[p.speedIdx] }
func (p *Provider) SetRunning(on bool)   { p.running = on; p.accum = 0 }
func (p *Provider) CycleSpeed(delta int) { p.cycleSpeed(delta) }

// EffectiveBatch returns the active per-tick HalfStep cap, falling
// back to the package default when MaxBatch is unset.
func (p *Provider) EffectiveBatch() int {
	if p.MaxBatch > 0 {
		return p.MaxBatch
	}
	return DefaultMaxBatchPerTick
}

// SetSpeedHz selects the speed entry whose Hz matches. Returns
// false if no entry matches.
func (p *Provider) SetSpeedHz(hz int) bool {
	for i, sp := range Speeds {
		if sp.Hz == hz {
			p.speedIdx = i
			p.accum = 0
			return true
		}
	}
	return false
}

func (p *Provider) cycleSpeed(delta int) {
	p.speedIdx = (p.speedIdx + delta + len(Speeds)) % len(Speeds)
	p.accum = 0
}

func (p *Provider) Draw(screen tcell.Screen, inner foxpro.Rect, theme foxpro.Theme, focused bool) {
	bg := theme.WindowBG
	hi := theme.Focus
	c := foxpro.NewCanvas(screen, inner, &p.ScrollState)

	c.Put(0, 0, "[R]un  [.]stop  [S]tep  [T]ick  [Z]reset", bg)
	c.Put(0, 1, "[<]/[>] cycle speed", bg)

	x := c.Put(0, 3, "Speed: ", bg)
	for i, sp := range Speeds {
		if i > 0 {
			x = c.Put(x, 3, " ", bg)
		}
		st := bg
		if i == p.speedIdx {
			st = hi
		}
		x = c.Put(x, 3, sp.Label, st)
	}

	state := "Stopped"
	if p.running {
		state = "Running"
	}
	c.Put(0, 5, fmt.Sprintf("%s  %d steps", state, p.stepsDone), bg)
}

func (p *Provider) HandleKey(ev *tcell.EventKey) bool {
	if ev.Key() == tcell.KeyRune {
		switch ev.Rune() {
		case 'r', 'R':
			p.SetRunning(true)
			return true
		case '.':
			p.SetRunning(false)
			return true
		case 's', 'S':
			p.StepOne()
			return true
		case '<', ',':
			p.cycleSpeed(-1)
			return true
		case '>', '/':
			p.cycleSpeed(1)
			return true
		case 'z', 'Z':
			p.Reset()
			return true
		}
	}
	switch ev.Key() {
	case tcell.KeyRight:
		p.cycleSpeed(1)
		return true
	case tcell.KeyLeft:
		p.cycleSpeed(-1)
		return true
	case tcell.KeyUp:
		p.SetScrollOffset(p.X, p.Y-1)
		return true
	case tcell.KeyDown:
		p.SetScrollOffset(p.X, p.Y+1)
		return true
	}
	return false
}

func (p *Provider) StatusHint() string {
	return "R run  . stop  S step  T tick  </> speed  Z reset"
}
