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

	// Pin-themed "lit" styles, shared between the bus-value text and
	// the chip diagram so $-values and the matching pin glow read as
	// the same indicator drawn twice.
	dLit := style.Background(theme.Palette.Yellow).Foreground(theme.Palette.Black)
	// White-on-Blue for the address bus highlight — magenta on the
	// cyan window bg was hard to read in the scope's dot-density;
	// blue has stronger luminance contrast and pairs naturally with
	// the FoxPro chrome.
	aLit := style.Background(theme.Palette.Blue).Foreground(theme.Palette.White)
	// Control lines (RW / IRQ / NMI) share one "lit" colour —
	// white silkscreen on the cyan window bg already covers the chip
	// body, so we flip to white-bg / black-fg for the highlight to
	// keep it distinct from yellow (D) and magenta (A).
	cLit := style.Background(theme.Palette.White).Foreground(theme.Palette.Black)

	c.Put(0, 0, fmt.Sprintf("A  $%02X   X  $%02X   Y  $%02X", regs.A, regs.X, regs.Y), style)
	c.Put(0, 1, fmt.Sprintf("S  $%02X   PC $%04X", regs.S, regs.PC), style)
	c.Put(0, 3, fmt.Sprintf("P  $%02X   N V - B D I Z C", regs.P), style)
	c.Put(0, 4, fmt.Sprintf("         %d %d %d %d %d %d %d %d",
		bit(regs.P, 7), bit(regs.P, 6), bit(regs.P, 5), bit(regs.P, 4),
		bit(regs.P, 3), bit(regs.P, 2), bit(regs.P, 1), bit(regs.P, 0)), style)

	// Bus block — right column. A/D values are painted with the same
	// pin-themed background so a quick glance pairs the live hex
	// readout to its corresponding pin glow on the chip below.
	const busX = 27
	rwLabel := "R"
	rwStyle := style
	if !p.Backend.ReadCycle() {
		rwLabel = "W"
		rwStyle = style.Foreground(tcell.ColorYellow)
	}
	c.Put(busX, 0, "A: ", style)
	c.Put(busX+3, 0, fmt.Sprintf("$%04X", p.Backend.AddressBus()), aLit)
	c.Put(busX, 1, "D: ", style)
	c.Put(busX+3, 1, fmt.Sprintf("$%02X", p.Backend.DataBus()), dLit)
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

	// Chip diagram — D pins on top, A pins on bottom, lit when the
	// corresponding bus bit is HIGH. Drawn on the right side of the
	// window when there's enough room. chipMinX = first column we'd
	// need; chipW = full footprint width.
	const (
		chipX = 42 // first col of the chip widget (after bus block)
		chipW = 22 // body width: 20 pin stubs + 2 borders
	)
	if inner.W >= chipX+chipW {
		drawChip(c, theme, chipX, 1,
			p.Backend.AddressBus(), p.Backend.DataBus(),
			p.Backend.ReadCycle(), p.Backend.IRQ(), p.Backend.NMI(),
			dLit, aLit, cLit)
	}

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

// pinInfo describes one physical chip pin: which signal lands on it,
// and (for bus pins) which bit. busType encodes the signal family:
//
//	'A' — address bus bit (bitIdx 0..15)
//	'D' — data bus bit    (bitIdx 0..7)
//	'R' — R/W                (control line, bitIdx ignored)
//	'I' — IRQ                (control line)
//	'N' — NMI                (control line)
//	 0  — unlabeled (VSS / VCC / PHI / SYNC / SO / RES / NC) —
//	      the stub is still drawn, just without a label.
type pinInfo struct {
	busType byte
	bitIdx  int
}

// Physical 6502 DIP-40 pinout, rendered horizontally as a 90° CCW
// rotation of the canonical vertical view — so pin 1 (VSS) lands at
// the bottom-left corner, matching how the chip is most often shown
// in datasheets when laid on its side.
//
// bottomPins[i] is the bottom-edge pin reading left → right and
// corresponds to physical pins 1 → 20 (the left side of the
// vertical layout, rotated down). topPins[i] corresponds to
// physical pins 40 → 21 (the right side, rotated up — so pin 40 is
// at top-left, pin 21 is at top-right).
//
// Per the original MOS Technology MCS6502 datasheet (pin 21 is a
// second VSS, pin 34 is RW; some secondary sources have these wrong).
//
// Index → physical pin → signal:
//
//	top[0]  → pin 40 → RES           bot[0]  → pin 1  → VSS
//	top[1]  → pin 39 → PHI2 OUT      bot[1]  → pin 2  → RDY
//	top[2]  → pin 38 → SO            bot[2]  → pin 3  → PHI1 OUT
//	top[3]  → pin 37 → PHI0 IN       bot[3]  → pin 4  → IRQ
//	top[4]  → pin 36 → NC            bot[4]  → pin 5  → NC
//	top[5]  → pin 35 → NC            bot[5]  → pin 6  → NMI
//	top[6]  → pin 34 → RW            bot[6]  → pin 7  → SYNC
//	top[7]  → pin 33 → D0            bot[7]  → pin 8  → VCC
//	top[8]  → pin 32 → D1            bot[8]  → pin 9  → A0
//	top[9]  → pin 31 → D2            bot[9]  → pin 10 → A1
//	top[10] → pin 30 → D3            bot[10] → pin 11 → A2
//	top[11] → pin 29 → D4            bot[11] → pin 12 → A3
//	top[12] → pin 28 → D5            bot[12] → pin 13 → A4
//	top[13] → pin 27 → D6            bot[13] → pin 14 → A5
//	top[14] → pin 26 → D7            bot[14] → pin 15 → A6
//	top[15] → pin 25 → A15           bot[15] → pin 16 → A7
//	top[16] → pin 24 → A14           bot[16] → pin 17 → A8
//	top[17] → pin 23 → A13           bot[17] → pin 18 → A9
//	top[18] → pin 22 → A12           bot[18] → pin 19 → A10
//	top[19] → pin 21 → VSS           bot[19] → pin 20 → A11
var (
	topPins = [20]pinInfo{
		{}, {}, {}, {}, {}, {},
		{'R', 0}, // pin 34: R/W
		{'D', 0}, {'D', 1}, {'D', 2}, {'D', 3},
		{'D', 4}, {'D', 5}, {'D', 6}, {'D', 7},
		{'A', 15}, {'A', 14}, {'A', 13}, {'A', 12},
		{},
	}
	bottomPins = [20]pinInfo{
		{}, {}, {},
		{'I', 0}, // pin 4:  IRQ
		{},
		{'N', 0}, // pin 6:  NMI
		{},
		{}, // VCC at pin 8
		{'A', 0}, {'A', 1}, {'A', 2}, {'A', 3},
		{'A', 4}, {'A', 5}, {'A', 6}, {'A', 7},
		{'A', 8}, {'A', 9}, {'A', 10}, {'A', 11},
	}
)

// drawChip paints a stylized 6502 DIP-40 package, horizontally
// oriented, with every pin in its physical location: A0..A11 on the
// top edge (pins 9..20 of the real package), A12..A15 + D7..D0 on
// the bottom edge (pins 21..32). The package's remaining pins (VSS,
// RDY, PHI, IRQ, NMI, SYNC, VCC, RW, SO, RES, NC) get unlabeled
// stubs so the IC reads as a true 40-pin part.
//
// A bus label cell flips background colour when its bit is HIGH —
// yellow for data, blue for address. The chip body and stubs stay
// white silkscreen so the "lit pin" visually reads as a glowing
// label hanging off an unchanged chip outline.
//
// Layout (x0,y0 = top-left, 22 wide × 7 tall):
//
//	row 0:                  0 1 2 3 4 5 6 7 F E D C    (D0..D7, A15..A12)
//	row 1:                  D D D D D D D D A A A A
//	row 2:  ┌─┴─┴─┴─┴─┴─┴─┴─┴─┴─┴─┴─┴─┴─┴─┴─┴─┴─┴─┴─┴─┐
//	row 3:  │              6502                │
//	row 4:  └─┬─┬─┬─┬─┬─┬─┬─┬─┬─┬─┬─┬─┬─┬─┬─┬─┬─┬─┬─┬─┘
//	row 5:                  0 1 2 3 4 5 6 7 8 9 A B    (A0..A11)
//	row 6:                  A A A A A A A A A A A A
func drawChip(c *foxpro.Canvas, theme foxpro.Theme, x0, y0 int,
	addr uint16, data uint8, rw, irq, nmi bool,
	dLit, aLit, cLit tcell.Style,
) {
	bg := theme.WindowBG
	chip := bg.Foreground(theme.Palette.White)
	const hexDigits = "0123456789ABCDEF"
	const bodyW = 22 // 20 pin columns + 2 borders

	// resolvePin returns the two label characters to render (digit
	// row, letter row) and the style to apply. Bus pins compute their
	// digit from bitIdx; control pins carry fixed 2-letter
	// abbreviations stacked vertically (R/W → R over W, IRQ → I/Q,
	// NMI → N/M). Empty digit (0) means the pin is unlabeled — caller
	// skips drawing.
	resolvePin := func(p pinInfo) (digit, letter rune, st tcell.Style) {
		switch p.busType {
		case 'A':
			lit := (addr>>p.bitIdx)&1 != 0
			st = bg
			if lit {
				st = aLit
			}
			return rune(hexDigits[p.bitIdx]), 'A', st
		case 'D':
			lit := (data>>p.bitIdx)&1 != 0
			st = bg
			if lit {
				st = dLit
			}
			return rune(hexDigits[p.bitIdx]), 'D', st
		case 'R':
			st = bg
			if rw {
				st = cLit
			}
			return 'R', 'W', st
		case 'I':
			st = bg
			if irq {
				st = cLit
			}
			return 'I', 'Q', st
		case 'N':
			st = bg
			if nmi {
				st = cLit
			}
			return 'N', 'M', st
		}
		return 0, 0, bg
	}

	// ── Top labels (rows 0,1) — over each pin's stub ───────────
	for i, p := range topPins {
		if p.busType == 0 {
			continue
		}
		x := x0 + 1 + i
		digit, letter, st := resolvePin(p)
		c.Set(x, y0+0, digit, st)
		c.Set(x, y0+1, letter, st)
	}

	// ── Chip top edge: 20 pin stubs in white ───────────────────
	bodyY := y0 + 2
	c.Set(x0, bodyY, '┌', chip)
	for i := 1; i < bodyW-1; i++ {
		c.Set(x0+i, bodyY, '┴', chip)
	}
	c.Set(x0+bodyW-1, bodyY, '┐', chip)

	// ── Chip body middle row with "6502" label ─────────────────
	c.Set(x0, bodyY+1, '│', chip)
	for i := 1; i < bodyW-1; i++ {
		c.Set(x0+i, bodyY+1, ' ', bg)
	}
	const label = "6502"
	labelX := x0 + (bodyW-len(label))/2
	c.Put(labelX, bodyY+1, label, bg)
	c.Set(x0+bodyW-1, bodyY+1, '│', chip)

	// ── Chip bottom edge: 20 pin stubs in white ────────────────
	c.Set(x0, bodyY+2, '└', chip)
	for i := 1; i < bodyW-1; i++ {
		c.Set(x0+i, bodyY+2, '┬', chip)
	}
	c.Set(x0+bodyW-1, bodyY+2, '┘', chip)

	// ── Bottom labels (rows 5,6) — under each pin's stub ───────
	for i, p := range bottomPins {
		if p.busType == 0 {
			continue
		}
		x := x0 + 1 + i
		digit, letter, st := resolvePin(p)
		c.Set(x, y0+5, digit, st)
		c.Set(x, y0+6, letter, st)
	}
}
