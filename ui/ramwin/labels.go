package ramwin

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gdamore/tcell/v2"

	foxpro "github.com/carledwards/foxpro-go"

	"github.com/carledwards/go6sim/asm"
)

// drawLabels renders the Provider's symbols as a sortable table —
// but only those falling inside the window's current memory region
// (Base..Base+Length). When no symbols cover the region, falls back
// to a "byte list" view: one line per byte, address + live value.
//
// Header columns:
//
//	ADDR    TYPE      NAME           VALUE              DESCRIPTION
//	$0000   byte      TICK_LO        12   $0C           system frame counter
//	$0010   4 bytes   BALL_X         14,52,98,30        ball X positions
//
// Symbols are sorted by address each frame; values are read live via
// the bus so the user can watch state evolve.
func (p *Provider) drawLabels(c *foxpro.Canvas, theme foxpro.Theme, bg tcell.Style) {
	header := bg.Foreground(theme.Palette.Yellow)

	// Filter symbols to those overlapping the current region. A
	// symbol "covers" an address range; we include any symbol whose
	// range overlaps with [Base, Base+Length).
	regionStart := p.Base
	regionEnd := uint32(p.Base) + uint32(p.Length) // half-open
	inRegion := make([]asm.Symbol, 0, len(p.Symbols))
	for _, s := range p.Symbols {
		size := s.Size
		if size < 1 {
			size = 1
		}
		symEnd := uint32(s.Addr) + uint32(size) // half-open
		if uint32(s.Addr) < regionEnd && symEnd > uint32(regionStart) {
			inRegion = append(inRegion, s)
		}
	}

	if len(inRegion) == 0 {
		p.drawByteList(c, theme, bg, header)
		return
	}

	c.Put(0, 0, "  ADDR    TYPE      NAME           VALUE              DESCRIPTION", header)

	// Sort by address — gives the reader a memory-walk feel matching
	// the hex view. Stable so multi-byte symbols at same addr keep
	// their declaration order.
	sort.SliceStable(inRegion, func(i, j int) bool { return inRegion[i].Addr < inRegion[j].Addr })

	_, sy := p.ScrollOffset()
	inner := c.Inner()
	rows := inner.H - 1
	if rows < 1 {
		rows = 1
	}
	first := sy
	if first < 0 {
		first = 0
	}

	for i := 0; i < rows && first+i < len(inRegion); i++ {
		s := inRegion[first+i]
		ly := i + 1
		c.Put(2, ly, fmt.Sprintf("$%04X", s.Addr), bg)
		c.Put(10, ly, formatSymbolType(s.Size), bg)
		c.Put(20, ly, s.Name, bg)
		c.Put(35, ly, p.formatSymbolValue(s), bg)
		c.Put(54, ly, s.Note, bg)
	}

	p.lastInstrCount = len(inRegion)
}

// drawByteList is the fallback view shown when no symbols overlap
// the current region. One row per byte: address + decimal + hex +
// printable ASCII glyph. Useful for poking at a peripheral whose
// register layout hasn't been declared, or a chunk of RAM that's
// just data.
func (p *Provider) drawByteList(c *foxpro.Canvas, theme foxpro.Theme, bg tcell.Style, header tcell.Style) {
	c.Put(0, 0, "  ADDR    DEC   HEX   CHAR   (no symbols declared in this region)", header)

	_, sy := p.ScrollOffset()
	inner := c.Inner()
	rows := inner.H - 1
	if rows < 1 {
		rows = 1
	}
	first := sy
	if first < 0 {
		first = 0
	}
	if first >= p.Length {
		first = p.Length - 1
	}

	for i := 0; i < rows && first+i < p.Length; i++ {
		off := first + i
		addr := p.Base + uint16(off)
		v := p.Bus.Read(addr)
		ly := i + 1
		c.Put(2, ly, fmt.Sprintf("$%04X", addr), bg)
		c.Put(10, ly, fmt.Sprintf("%-3d", v), bg)
		c.Put(16, ly, fmt.Sprintf("$%02X", v), bg)
		ch := byte('.')
		if v >= 0x20 && v <= 0x7E {
			ch = v
		}
		c.Put(23, ly, string(ch), bg)
	}

	p.lastInstrCount = p.Length
}

// formatSymbolType returns "byte", "word", or "N bytes".
func formatSymbolType(size int) string {
	switch size {
	case 1:
		return "byte"
	case 2:
		return "word"
	default:
		return fmt.Sprintf("%d bytes", size)
	}
}

// formatSymbolValue reads the symbol's bytes through the bus and
// formats them based on declared size.
func (p *Provider) formatSymbolValue(s asm.Symbol) string {
	switch s.Size {
	case 1:
		v := p.Bus.Read(s.Addr)
		return fmt.Sprintf("%-3d  $%02X", v, v)
	case 2:
		lo := p.Bus.Read(s.Addr)
		hi := p.Bus.Read(s.Addr + 1)
		v := uint16(hi)<<8 | uint16(lo)
		return fmt.Sprintf("%-5d $%04X", v, v)
	}
	const inline = 6
	var sb strings.Builder
	n := s.Size
	if n > inline {
		n = inline
	}
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, "%02X", p.Bus.Read(s.Addr+uint16(i)))
	}
	if s.Size > inline {
		sb.WriteString("…")
	}
	return sb.String()
}

// resolveSymbol returns the symbol name + true if any symbol covers
// the given address. Used by the disassembler view to substitute
// label names for raw addresses.
func (p *Provider) resolveSymbol(addr uint16) (string, bool) {
	for _, s := range p.Symbols {
		size := s.Size
		if size < 1 {
			size = 1
		}
		if addr >= s.Addr && int(addr) < int(s.Addr)+size {
			return s.Name, true
		}
	}
	return "", false
}

// annotateInstrText post-processes a disassembled instruction string:
// substitutes operands matching known symbols, appends per-instruction
// comments from the Annotations table.
func (p *Provider) annotateInstrText(addr uint16, pretty string) string {
	out := pretty

	if len(p.Symbols) > 0 {
		// 16-bit absolute forms first so $XXXX doesn't get partially
		// matched as $XX.
		for _, s := range p.Symbols {
			t := fmt.Sprintf("$%04X", s.Addr)
			if strings.Contains(out, t) {
				out = strings.ReplaceAll(out, t, s.Name)
			}
		}
		// Zero-page (8-bit) forms.
		for _, s := range p.Symbols {
			if s.Addr > 0xFF {
				continue
			}
			t := fmt.Sprintf("$%02X", s.Addr)
			if strings.Contains(out, t) {
				out = strings.ReplaceAll(out, t, s.Name)
			}
		}
	}

	for _, ann := range p.Annotations {
		if ann.PC == addr && ann.Comment != "" {
			out += "  ; " + ann.Comment
			break
		}
	}
	return out
}
