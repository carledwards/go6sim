// Package cpuwin renders the CPU registers + flag bits + cycle count
// as a foxpro-go content provider.
package cpuwin

import (
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"

	foxpro "github.com/carledwards/foxpro-go"
	"github.com/carledwards/go6sim/cpu"
)

// rateWindow controls the smoothing window for the displayed Hz —
// half a second is short enough to feel live, long enough to be
// stable at low speeds.
const rateWindow = 500 * time.Millisecond

type Provider struct {
	Backend cpu.Backend
	foxpro.ScrollState

	// OnReset is called when the user clicks the < Reset > button or
	// presses the bound key. Wired by main to a machine-reset that
	// also clears RAM and repaints the display.
	OnReset func()

	sampleHalf uint64
	sampleTime time.Time
	rate       float64 // smoothed half-cycles per second

	// Reset button drag state.
	pressed bool
	armed   bool
}

// Rate returns the most-recently-measured full-cycle rate in Hz.
// Updated every rateWindow inside Draw — readable from anywhere
// (e.g. a menu-bar tray indicator).
func (p *Provider) Rate() float64 { return p.rate / 2 }

// FormatHz exposes the rate-formatting helper for callers that want
// to render the same string elsewhere (tray, status bar, etc.).
func FormatHz(hz float64) string { return formatHz(hz) }

// Natural minimum so resize can't hide all useful content.
const (
	MinW = 28
	MinH = 6
)

// Layout: rows 0,1 regs; 3,4 flags; 6,7,8 cycle/rate; 10 reset button.
const (
	resetButtonY     = 10
	resetButtonLabel = "< Reset >"
	resetButtonW     = 9
)

func formatHz(hz float64) string {
	switch {
	case hz >= 1e6:
		return fmt.Sprintf("%.2f MHz", hz/1e6)
	case hz >= 1e3:
		return fmt.Sprintf("%.2f kHz", hz/1e3)
	case hz >= 1:
		return fmt.Sprintf("%.0f Hz", hz)
	default:
		return "—"
	}
}

func (p *Provider) Draw(screen tcell.Screen, inner foxpro.Rect, theme foxpro.Theme, focused bool) {
	regs := p.Backend.Registers()
	style := theme.WindowBG
	c := foxpro.NewCanvas(screen, inner, &p.ScrollState)

	c.Put(0, 0, fmt.Sprintf("A  $%02X   X  $%02X   Y  $%02X", regs.A, regs.X, regs.Y), style)
	c.Put(0, 1, fmt.Sprintf("S  $%02X   PC $%04X", regs.S, regs.PC), style)
	c.Put(0, 3, fmt.Sprintf("P  $%02X   N V - B D I Z C", regs.P), style)
	c.Put(0, 4, fmt.Sprintf("         %d %d %d %d %d %d %d %d",
		bit(regs.P, 7), bit(regs.P, 6), bit(regs.P, 5), bit(regs.P, 4),
		bit(regs.P, 3), bit(regs.P, 2), bit(regs.P, 1), bit(regs.P, 0)), style)

	// Bus block — right column. A/D values + R/W direction +
	// interrupt-line states. IRQ/NMI are active low, so 'L' = asserted
	// and is drawn in red to flag attention.
	const busX = 27
	rwLabel := "R"
	rwStyle := style
	if !p.Backend.ReadCycle() {
		rwLabel = "W"
		rwStyle = style.Foreground(tcell.ColorYellow)
	}
	c.Put(busX, 0, fmt.Sprintf("A: $%04X", p.Backend.AddressBus()), style)
	c.Put(busX, 1, fmt.Sprintf("D: $%02X", p.Backend.DataBus()), style)
	c.Put(busX, 2, "R/W: ", style)
	c.Put(busX+5, 2, rwLabel, rwStyle)

	pinLabel := func(high bool) (string, tcell.Style) {
		if high {
			return "H", style
		}
		return "L", style.Foreground(tcell.ColorRed)
	}
	irqL, irqS := pinLabel(p.Backend.IRQ())
	nmiL, nmiS := pinLabel(p.Backend.NMI())
	c.Put(busX, 4, "IRQ: ", style)
	c.Put(busX+5, 4, irqL, irqS)
	c.Put(busX, 5, "NMI: ", style)
	c.Put(busX+5, 5, nmiL, nmiS)

	hc := p.Backend.HalfCycles()
	c.Put(0, 6, fmt.Sprintf("HalfCycles: %d", hc), style)
	c.Put(0, 7, fmt.Sprintf("Cycles:     %d", hc/2), style)

	// Sample-and-hold rate calculation: every rateWindow we recompute
	// over the elapsed period and stash the result.
	now := time.Now()
	if p.sampleTime.IsZero() {
		p.sampleTime = now
		p.sampleHalf = hc
	} else if dt := now.Sub(p.sampleTime); dt >= rateWindow {
		p.rate = float64(hc-p.sampleHalf) / dt.Seconds()
		p.sampleTime = now
		p.sampleHalf = hc
	}
	c.Put(0, 8, fmt.Sprintf("Rate:       %s", formatHz(p.rate/2)), style)

	// Reset button — chrome style normally, theme.Focus when armed.
	chrome := tcell.StyleDefault.
		Background(theme.Palette.Cyan).
		Foreground(theme.Palette.Blue)
	bx := p.resetButtonX()
	btnStyle := chrome
	if p.pressed && p.armed {
		btnStyle = theme.Focus
	}
	c.Put(bx, resetButtonY, resetButtonLabel, btnStyle)
}

// resetButtonX returns the logical x-origin of the centered Reset
// button. Recomputed each Draw so it follows the natural width if
// MinW changes.
func (p *Provider) resetButtonX() int {
	w, _ := p.LastViewport()
	if w < resetButtonW {
		return 0
	}
	return (w - resetButtonW) / 2
}

func (p *Provider) buttonHit(mx, my int, inner foxpro.Rect) bool {
	lx := (mx - inner.X) + p.X
	ly := (my - inner.Y) + p.Y
	bx := p.resetButtonX()
	return ly == resetButtonY && lx >= bx && lx < bx+resetButtonW
}

func (p *Provider) HandleMouse(ev *tcell.EventMouse, inner foxpro.Rect) bool {
	if ev.Buttons()&tcell.Button1 == 0 {
		return false
	}
	if p.OnReset == nil {
		return false
	}
	mx, my := ev.Position()
	if p.buttonHit(mx, my, inner) {
		p.pressed = true
		p.armed = true
		return true
	}
	return false
}

func (p *Provider) HandleMouseMotion(ev *tcell.EventMouse, inner foxpro.Rect) {
	if !p.pressed {
		return
	}
	mx, my := ev.Position()
	p.armed = p.buttonHit(mx, my, inner)
}

func (p *Provider) HandleMouseRelease(ev *tcell.EventMouse, inner foxpro.Rect) {
	fire := p.pressed && p.armed
	p.pressed = false
	p.armed = false
	if fire && p.OnReset != nil {
		p.OnReset()
	}
}

func (p *Provider) HandleKey(ev *tcell.EventKey) bool {
	w, h := p.LastViewport()
	switch ev.Key() {
	case tcell.KeyUp:
		p.SetScrollOffset(p.X, p.Y-1)
		return true
	case tcell.KeyDown:
		p.SetScrollOffset(p.X, p.Y+1)
		return true
	case tcell.KeyLeft:
		p.SetScrollOffset(p.X-1, p.Y)
		return true
	case tcell.KeyRight:
		p.SetScrollOffset(p.X+1, p.Y)
		return true
	case tcell.KeyPgUp:
		p.SetScrollOffset(p.X, p.Y-h)
		return true
	case tcell.KeyPgDn:
		p.SetScrollOffset(p.X, p.Y+h)
		return true
	case tcell.KeyHome:
		p.SetScrollOffset(0, 0)
		return true
	case tcell.KeyEnd:
		p.SetScrollOffset(0, p.Y) // unused for cpuwin
	}
	_ = w
	return false
}

func bit(b uint8, n int) int { return int((b >> n) & 1) }
