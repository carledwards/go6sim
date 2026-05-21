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

	sampleHalf uint64
	sampleTime time.Time
	rate       float64 // smoothed half-cycles per second
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

// Layout: rows 0,1 regs; 3,4 flags; 6,7,8 cycle/rate. The earlier
// `< Reset >` button on row 10 was removed when the sim TUI's Clock
// window went away — Hardware → Reset menu + Z hotkey + the menu-
// bar tray cover the same action with less visual clutter.

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
