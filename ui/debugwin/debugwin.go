// Package debugwin renders a live disassembly view of a memory
// region, with the line containing the current PC highlighted.
package debugwin

import (
	"fmt"

	"github.com/gdamore/tcell/v2"

	foxpro "github.com/carledwards/foxpro-go"
	"github.com/carledwards/go6sim/bus"
	"github.com/carledwards/go6sim/cpu"
	"github.com/carledwards/go6sim/disasm"
)

const (
	MinW = 36
	MinH = 4
)

// Provider disassembles `Length` bytes of bus state starting at
// `Base` and highlights the line whose address range covers the
// current PC. Re-decodes every Draw, so self-modifying code is
// reflected without manual refresh.
type Provider struct {
	Bus     bus.Bus
	Backend cpu.Backend
	Base    uint16
	Length  int

	foxpro.ScrollState

	lastInstrCount int
}

func (p *Provider) Draw(screen tcell.Screen, inner foxpro.Rect, theme foxpro.Theme, focused bool) {
	bg := theme.WindowBG
	hl := theme.Focus
	c := foxpro.NewCanvas(screen, inner, &p.ScrollState)

	pc := p.Backend.Registers().PC

	instrs := make([]disasm.Instr, 0, 64)
	addr := p.Base
	end := uint64(p.Base) + uint64(p.Length)
	for uint64(addr) < end {
		ins := disasm.Decode(addr, p.Bus.Read)
		instrs = append(instrs, ins)
		next := uint64(addr) + uint64(ins.Size())
		if next > 0x10000 {
			break
		}
		addr = uint16(next)
	}
	p.lastInstrCount = len(instrs)

	pcLine := -1
	for i, ins := range instrs {
		size := uint16(ins.Size())
		if pc >= ins.Addr && pc < ins.Addr+size {
			pcLine = i
			break
		}
	}

	// Auto-scroll to keep PC line visible.
	if pcLine >= 0 {
		_, vh := p.LastViewport()
		if vh > 0 {
			if pcLine < p.Y {
				p.SetScrollOffset(p.X, pcLine)
			} else if pcLine >= p.Y+vh {
				p.SetScrollOffset(p.X, pcLine-vh+1)
			}
		}
	}

	for i, ins := range instrs {
		marker := "  "
		st := bg
		if i == pcLine {
			marker = "> "
			st = hl
		}
		line := fmt.Sprintf("%s$%04X  %s  %s",
			marker, ins.Addr, disasm.HexBytes(ins.Bytes), ins.Pretty)
		c.Put(0, i, line, st)
	}
}

func (p *Provider) HandleKey(ev *tcell.EventKey) bool {
	_, vh := p.LastViewport()
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
		p.SetScrollOffset(p.X, p.Y-vh)
		return true
	case tcell.KeyPgDn:
		p.SetScrollOffset(p.X, p.Y+vh)
		return true
	case tcell.KeyHome:
		p.SetScrollOffset(0, 0)
		return true
	case tcell.KeyEnd:
		p.SetScrollOffset(p.X, p.lastInstrCount)
		return true
	}
	return false
}

func (p *Provider) StatusHint() string {
	return "↑/↓/←/→ scroll  PgUp/PgDn page  Home/End jump"
}
