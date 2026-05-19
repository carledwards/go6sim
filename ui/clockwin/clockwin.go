// Package clockwin is the tcell/foxpro *view* of the core clock driver.
// All run/step/speed logic lives in package clock (tcell-free,
// wasm-clean); Provider embeds *clock.Driver and adds only rendering +
// key handling, so the standalone TUI is unchanged while the same
// driver is reusable by the instrument API / web / MCP. Runs on the
// foxpro-go event loop — no goroutines.
package clockwin

import (
	"fmt"

	"github.com/gdamore/tcell/v2"

	foxpro "github.com/carledwards/foxpro-go"
	"github.com/carledwards/go6sim/clock"
	"github.com/carledwards/go6sim/cpu"
)

const (
	MinW = 32
	MinH = 7
)

// Re-exported so existing callers (cmd/*) keep using clockwin.Speed /
// clockwin.Speeds / clockwin.DefaultMaxBatchPerTick unchanged — the
// definitions now live in package clock.
type Speed = clock.Speed

var Speeds = clock.Speeds

const DefaultMaxBatchPerTick = clock.DefaultMaxBatchPerTick

// Provider is the simulator's run/step/stop UX. It embeds *clock.Driver
// (all run/step/speed behaviour, promoted to the Provider unchanged —
// Advance/StepOne/StepInstruction/Reset/Running/SetRunning/Speed/
// CycleSpeed/SetSpeedHz/EffectiveBatch plus the MaxBatch/OnHalfStep/
// Backend fields) and foxpro.ScrollState (scrollbars). No method-name
// collisions between the two embeds.
type Provider struct {
	*clock.Driver
	foxpro.ScrollState
}

// NewProvider returns a Provider that owns a fresh clock driver at the
// default speed (10 Hz).
func NewProvider(backend cpu.Backend) *Provider {
	return NewProviderWithDriver(clock.NewDriver(backend))
}

// NewProviderWithDriver returns a Provider that *shares* an existing
// clock driver, so the same driver can simultaneously back an
// instrument.Instrument (which the run loop calls) while this Provider
// renders the clock window. Both observe one run/step/speed state.
func NewProviderWithDriver(d *clock.Driver) *Provider {
	return &Provider{Driver: d}
}

func (p *Provider) Draw(screen tcell.Screen, inner foxpro.Rect, theme foxpro.Theme, focused bool) {
	bg := theme.WindowBG
	hi := theme.Focus
	c := foxpro.NewCanvas(screen, inner, &p.ScrollState)

	c.Put(0, 0, "[R]un  [.]stop  [S]tep  [T]ick  [Z]reset", bg)
	c.Put(0, 1, "[<]/[>] cycle speed", bg)

	x := c.Put(0, 3, "Speed: ", bg)
	cur := p.SpeedIndex()
	for i, sp := range Speeds {
		if i > 0 {
			x = c.Put(x, 3, " ", bg)
		}
		st := bg
		if i == cur {
			st = hi
		}
		x = c.Put(x, 3, sp.Label, st)
	}

	state := "Stopped"
	if p.Running() {
		state = "Running"
	}
	c.Put(0, 5, fmt.Sprintf("%s  %d steps", state, p.StepsDone()), bg)
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
			p.CycleSpeed(-1)
			return true
		case '>', '/':
			p.CycleSpeed(1)
			return true
		case 'z', 'Z':
			p.Reset()
			return true
		}
	}
	switch ev.Key() {
	case tcell.KeyRight:
		p.CycleSpeed(1)
		return true
	case tcell.KeyLeft:
		p.CycleSpeed(-1)
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
