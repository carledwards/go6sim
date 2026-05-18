// Package viawin renders the live state of a 6522 VIA chip in a
// foxpro window. Counter, latch, mode, IFR/IER bits, and stub
// registers are all shown — the goal is "watch the timer count
// down, watch IFR T1 flicker on underflow, see the polling pattern
// in action."
//
// The provider reads via.VIA.Snapshot() each Draw, so it never
// triggers register-read side effects (a Bus-routed read of T1C-L
// would clear the IFR T1 flag — bad for a passive viewer).
package viawin

import (
	"fmt"

	"github.com/gdamore/tcell/v2"

	foxpro "github.com/carledwards/foxpro-go"

	"github.com/carledwards/go6sim/components/via"
)

const (
	MinW = 50
	MinH = 18
)

// Provider renders one VIA chip.
type Provider struct {
	VIA  *via.VIA
	Base uint16 // bus address of the register block (for the title bar)

	foxpro.ScrollState
}

// Draw paints the register dump. Layout (rough):
//
//	[$A810]  Crystal: 1 MHz
//	╶ Timer 1 ─────────────────────────────╴
//	  Counter   $C2A4   49828
//	  Latch     $C350   50000
//	  Mode      free-run     Armed: yes
//	╶ ACR / IFR / IER ─────────────────────╴
//	  ACR  $40   T1=free-run  T2=- SR=- PB=- PA=-
//	  IFR  $40   IRQ T1 T2 CB1 CB2 SR  CA1 CA2
//	              .  ●  .  .   .   .   .   .
//	  IER  $80   IRQ T1 T2 CB1 CB2 SR  CA1 CA2
//	              ●  .  .  .   .   .   .   .
//	╶ Ports / Misc ────────────────────────╴
//	  ORA  $00  DDRA $00     ORB  $00  DDRB $00
//	  SR   $00  PCR  $00     T2: (Phase 2)
func (p *Provider) Draw(screen tcell.Screen, inner foxpro.Rect, theme foxpro.Theme, focused bool) {
	bg := theme.WindowBG
	hdr := bg.Foreground(theme.Palette.Yellow)
	dim := bg.Foreground(theme.Palette.DarkGray)
	on := bg.Foreground(theme.Palette.LightGreen)

	c := foxpro.NewCanvas(screen, inner, &p.ScrollState)

	if p.VIA == nil {
		c.Put(0, 0, "(no VIA wired)", bg)
		return
	}
	s := p.VIA.Snapshot()

	c.Put(0, 0, fmt.Sprintf("[$%04X]  Crystal: %s", p.Base, formatHz(s.CrystalHz)), hdr)

	// --- Timer 1 ---
	c.Put(0, 2, "─ Timer 1 ───────────────────────────────────────", hdr)

	mode := "one-shot"
	if s.T1FreeRun {
		mode = "free-run"
	}

	// When T1 is disarmed (no write to T1C-H since reset / power-on),
	// the counter is meaningless and never advances. Dim the counter /
	// latch rows and call out the disarmed state — without this the
	// reader assumes "stuck counter = broken simulator".
	if !s.T1Armed {
		c.Put(2, 3, fmt.Sprintf("Counter   $%04X   %5d", s.T1Counter, s.T1Counter), dim)
		c.Put(2, 4, fmt.Sprintf("Latch     $%04X   %5d", s.T1Latch, s.T1Latch), dim)
		c.Put(2, 5, fmt.Sprintf("Mode      %-10s", mode), dim)
		warn := bg.Foreground(theme.Palette.Yellow)
		c.Put(2, 6, fmt.Sprintf("DISARMED — write T1C-H ($%04X) to start the timer", p.Base+via.RegT1CH), warn)
	} else {
		c.Put(2, 3, fmt.Sprintf("Counter   $%04X   %5d", s.T1Counter, s.T1Counter), bg)
		c.Put(2, 4, fmt.Sprintf("Latch     $%04X   %5d", s.T1Latch, s.T1Latch), bg)
		c.Put(2, 5, fmt.Sprintf("Mode      %-10s   Armed: yes", mode), bg)
	}

	// --- ACR / IFR / IER ---
	c.Put(0, 7, "─ ACR / IFR / IER ───────────────────────────────", hdr)
	c.Put(2, 8, fmt.Sprintf("ACR  $%02X   %s", s.ACR, decodeACR(s.ACR)), bg)

	bits := []string{"CA2", "CA1", "SR", "CB2", "CB1", "T2", "T1", "IRQ"}
	// Print IFR row + dot row.
	c.Put(2, 9, fmt.Sprintf("IFR  $%02X  ", s.IFR), bg)
	drawBitTable(c, 14, 9, 10, bits, bg, hdr)
	drawBitDots(c, 14, 10, s.IFR, bits, bg, on)

	c.Put(2, 12, fmt.Sprintf("IER  $%02X  ", s.IER|0x80), bg)
	drawBitTable(c, 14, 12, 13, bits, bg, hdr)
	drawBitDots(c, 14, 13, s.IER|0x80, bits, bg, on)

	// --- Ports / Misc ---
	c.Put(0, 15, "─ Ports / Misc ──────────────────────────────────", hdr)
	c.Put(2, 16, fmt.Sprintf("ORA  $%02X  DDRA $%02X     ORB  $%02X  DDRB $%02X",
		s.ORA, s.DDRA, s.ORB, s.DDRB), bg)
	c.Put(2, 17, fmt.Sprintf("SR   $%02X  PCR  $%02X     ", s.SR, s.PCR), bg)
	c.Put(36, 17, "T2: (Phase 2)", dim)
}

// drawBitTable prints bit names left-to-right at (x,y). Bit 7 first,
// matching how byte values are read by humans.
func drawBitTable(c *foxpro.Canvas, x, y, _ int, names []string, bg tcell.Style, hdr tcell.Style) {
	cx := x
	// names[] is bit 0..7; we print 7..0.
	for i := len(names) - 1; i >= 0; i-- {
		cx = c.Put(cx, y, fmt.Sprintf("%-3s ", names[i]), hdr)
	}
}

// drawBitDots prints a "●" for set bits and "." for clear, aligned
// under the names produced by drawBitTable.
func drawBitDots(c *foxpro.Canvas, x, y int, val byte, names []string, bg tcell.Style, on tcell.Style) {
	cx := x
	for i := len(names) - 1; i >= 0; i-- {
		mark := "."
		st := bg
		if val&(1<<uint(i)) != 0 {
			mark = "●"
			st = on
		}
		cx = c.Put(cx, y, fmt.Sprintf("%-3s ", " "+mark), st)
	}
}

// decodeACR returns a one-line human description of the ACR's
// programmer-relevant bits (only T1 mode is implemented in Phase 1,
// so the rest are shown as "-").
func decodeACR(acr byte) string {
	t1 := "one-shot"
	if acr&via.ACR_T1_FREERUN != 0 {
		t1 = "free-run"
	}
	pb7 := "-"
	if acr&via.ACR_T1_PB7 != 0 {
		pb7 = "PB7-out"
	}
	return fmt.Sprintf("T1=%-9s  PB7=%-7s  T2=- SR=- PB=- PA=-", t1, pb7)
}

func formatHz(hz uint64) string {
	switch {
	case hz >= 1_000_000:
		return fmt.Sprintf("%d MHz", hz/1_000_000)
	case hz >= 1_000:
		return fmt.Sprintf("%d kHz", hz/1_000)
	}
	return fmt.Sprintf("%d Hz", hz)
}

// HandleKey — no interactive controls in Phase 1.
func (p *Provider) HandleKey(ev *tcell.EventKey) bool { return false }

// StatusHint — nothing to advertise yet.
func (p *Provider) StatusHint() string { return "" }
