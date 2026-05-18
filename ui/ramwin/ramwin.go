// Package ramwin is a memory inspector. Two views — hex dump or
// 6502 mnemonic disassembly — toggled with 'v'. The first row's
// address label doubles as a clickable goto button; press 'g' (or
// click) to retarget Base, type 4 hex digits, auto-applies.
package ramwin

import (
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"

	foxpro "github.com/carledwards/foxpro-go"
	"github.com/carledwards/go6sim/bus"
	"github.com/carledwards/go6sim/asm"
	"github.com/carledwards/go6sim/cpu"
	"github.com/carledwards/go6sim/disasm"
)

// View modes the Memory window can display. Cycle with 'v'.
const (
	ViewHex    = 0
	ViewDisasm = 1
	ViewLabels = 2
)

const bytesPerRow = 16

const (
	labelW               = 7 // "$XXXX: "
	hexW                 = bytesPerRow * 3
	asciiGap             = 2
	defaultFlashDuration = 350 * time.Millisecond

	MinW = labelW + hexW + 2
	MinH = 4
)

// Provider dumps `Length` bytes starting at `Base`, reading values
// through the bus on every Draw.
//
// Highlight (optional) marks a single address in the focus style and
// auto-scrolls to keep it visible.
//
// EditableBase exposes a clickable "$XXXX" jump field on the first
// data row. Press 'g' (or click) to enter typing mode, type 4 hex
// digits — auto-applies on the 4th. Esc cancels, Backspace edits.
type Provider struct {
	Bus           bus.Bus
	Base          uint16
	Length        int
	Highlight     func() (addr uint16, ok bool)
	FlashDuration time.Duration
	EditableBase  bool

	// Trace is optional. When set + View == ViewHex, cells that were
	// read or written within freshness ticks are tinted green / yellow
	// respectively. PC highlight overrides both.
	Trace *bus.TraceBus

	// Backend is optional. When set + ShowInfo is true + View ==
	// ViewDisasm, the disasm view shows a side panel describing the
	// PC-marked instruction with live operand values pulled from the
	// CPU registers + bus.
	Backend cpu.Backend

	// Window, if set, has its Title updated each Draw to reflect the
	// current view + visible range.
	Window *foxpro.Window

	// View selects between ViewHex / ViewDisasm / ViewLabels. Cycle with 'v'.
	View int

	// ShowInfo enables the disasm-mode side panel. Toggle with 'i'.
	ShowInfo bool

	// Symbols, if non-empty, drives:
	//
	//   - The Labels view (ViewLabels): a sortable list of named
	//     memory regions with current values pulled live from the bus.
	//   - The Disasm view: operand addresses that match a known
	//     symbol render as the symbol name (e.g. "LDA TICK_LO" instead
	//     of "LDA $00").
	//
	// Updated by main.go's loadDemo callback so each demo gets its own
	// labels surfaced.
	Symbols []asm.Symbol

	// Annotations, if non-empty, augments the Disasm view with a
	// per-instruction comment column at the right edge — the same
	// human comments the demo author attached via asm.Comment(...).
	Annotations []asm.Annotation

	foxpro.ScrollState

	snapshot []uint8
	changeAt []time.Time

	inputting bool
	inputBuf  string

	// Disasm-view bookkeeping — number of instructions rendered last
	// frame, used by content-size + scroll clamp.
	lastInstrCount int
}

func (p *Provider) totalRows() int {
	return (p.Length + bytesPerRow - 1) / bytesPerRow
}

// applyInput parses inputBuf as 4 hex digits and re-targets Base.
func (p *Provider) applyInput() {
	if len(p.inputBuf) > 0 {
		var v uint64
		ok := true
		for _, ch := range p.inputBuf {
			d := -1
			switch {
			case ch >= '0' && ch <= '9':
				d = int(ch - '0')
			case ch >= 'a' && ch <= 'f':
				d = int(ch-'a') + 10
			case ch >= 'A' && ch <= 'F':
				d = int(ch-'A') + 10
			}
			if d < 0 {
				ok = false
				break
			}
			v = v*16 + uint64(d)
		}
		if ok {
			p.Base = uint16(v)
			p.snapshot = nil
			p.changeAt = nil
			p.SetScrollOffset(0, 0)
		}
	}
	p.inputting = false
	p.inputBuf = ""
}

func (p *Provider) Draw(screen tcell.Screen, inner foxpro.Rect, theme foxpro.Theme, focused bool) {
	c := foxpro.NewCanvas(screen, inner, &p.ScrollState)

	bg := theme.WindowBG
	pcStyle := theme.Focus

	// Update window title (mode + range).
	if p.Window != nil {
		end := uint16(int(p.Base) + p.Length - 1)
		modeTag := ""
		if p.View == ViewDisasm {
			modeTag = " · disasm"
		}
		p.Window.Title = fmt.Sprintf("Memory%s ($%04X - $%04X)", modeTag, p.Base, end)
	}

	// Editable address — blue on cyan, button look.
	editStyle := tcell.StyleDefault.
		Background(theme.Palette.Cyan).
		Foreground(theme.Palette.Blue)

	hAddr, hOK := uint16(0), false
	if p.Highlight != nil {
		hAddr, hOK = p.Highlight()
	}

	switch p.View {
	case ViewDisasm:
		p.drawDisasm(c, bg, pcStyle, editStyle, hAddr, hOK)
		return
	case ViewLabels:
		p.drawLabels(c, theme, bg)
		return
	}
	p.drawHex(c, theme, bg, pcStyle, editStyle, hAddr, hOK)
}

// drawHex renders the classic hex dump (header row + N data rows).
func (p *Provider) drawHex(c *foxpro.Canvas, theme foxpro.Theme, bg, pcStyle, editStyle tcell.Style, hAddr uint16, hOK bool) {
	flashStyle := bg.Reverse(true)

	// Activity tints — pulled from the theme palette so the same
	// styling re-skins across themes:
	//   Yellow  → write that changed the byte (most interesting)
	//   Brown   → write that left the byte unchanged (touched but no-op)
	//   Green   → read
	//
	// Yellow is bright (CGA #FFFF55) — the inherited white fg from
	// `bg` is unreadable on top of it, so we force black fg there.
	// Brown and Green are dark enough that white fg keeps contrast.
	readStyle := bg.Background(theme.Palette.Green)
	writeChStyle := bg.Background(theme.Palette.Yellow).Foreground(theme.Palette.Black)
	writeNcStyle := bg.Background(theme.Palette.Brown)
	const traceFreshness = 20 // ~1 second at 20 fps

	if p.snapshot == nil {
		p.snapshot = make([]uint8, p.Length)
		p.changeAt = make([]time.Time, p.Length)
		for i := 0; i < p.Length; i++ {
			p.snapshot[i] = p.Bus.Read(uint16(int(p.Base) + i))
		}
	}
	flash := p.FlashDuration
	if flash == 0 {
		flash = defaultFlashDuration
	}
	now := time.Now()

	// Auto-scroll to keep highlight visible.
	if hOK && int(hAddr) >= int(p.Base) && int(hAddr) < int(p.Base)+p.Length {
		row := (int(hAddr) - int(p.Base)) / bytesPerRow
		ly := row + 1
		_, vh := p.LastViewport()
		if vh > 0 {
			if ly < p.Y {
				p.SetScrollOffset(p.X, ly)
			} else if ly >= p.Y+vh {
				p.SetScrollOffset(p.X, ly-vh+1)
			}
		}
	}

	// Column header.
	for col := 0; col < bytesPerRow; col++ {
		c.Put(labelW+col*3, 0, fmt.Sprintf(" %02X", col), bg)
	}

	// Data rows.
	rows := p.totalRows()
	for drow := 0; drow < rows; drow++ {
		ly := drow + 1
		rowAddr := uint16(int(p.Base) + drow*bytesPerRow)

		if drow == 0 && p.EditableBase {
			p.drawEditableAddr(c, 0, ly, editStyle)
			c.Put(5, ly, ": ", editStyle)
		} else {
			c.Put(0, ly, fmt.Sprintf("$%04X: ", rowAddr), bg)
		}

		for col := 0; col < bytesPerRow; col++ {
			off := drow*bytesPerRow + col
			if off >= p.Length {
				break
			}
			addr := uint16(int(p.Base) + off)
			b := p.Bus.Read(addr)

			if b != p.snapshot[off] {
				p.snapshot[off] = b
				p.changeAt[off] = now
			}

			cellStyle := bg
			if !p.changeAt[off].IsZero() && now.Sub(p.changeAt[off]) < flash {
				cellStyle = flashStyle
			}
			if p.Trace != nil {
				if p.Trace.RecentRead(addr, traceFreshness) {
					cellStyle = readStyle
				}
				// Write wins over read; value-changing write
				// wins over idempotent write.
				if p.Trace.RecentWrite(addr, traceFreshness) {
					cellStyle = writeNcStyle
				}
				if p.Trace.RecentWriteChanged(addr, traceFreshness) {
					cellStyle = writeChStyle
				}
			}
			if hOK && addr == hAddr {
				cellStyle = pcStyle // PC wins over everything
			}

			c.Put(labelW+col*3, ly, " ", bg)
			c.Put(labelW+col*3+1, ly, fmt.Sprintf("%02X", b), cellStyle)

			// ASCII column to the right of the hex bytes — printable
			// chars get rendered as themselves, anything else as '.'.
			ascii := byte('.')
			if b >= 0x20 && b < 0x7F {
				ascii = b
			}
			c.Set(labelW+hexW+asciiGap+col, ly, rune(ascii), cellStyle)
		}
	}
}

// drawDisasm renders one instruction per row, decoding forward from
// Base. Decode budget caps per-Draw cost so a large Length (e.g. an
// 8 KB ROM) doesn't burn the UI thread — we only walk far enough to
// fill the visible viewport plus the user's current scroll offset.
const maxDisasmInstrs = 256

func (p *Provider) drawDisasm(c *foxpro.Canvas, bg, pcStyle, editStyle tcell.Style, hAddr uint16, hOK bool) {
	addr := p.Base
	end := uint64(p.Base) + uint64(p.Length)

	_, vh := p.LastViewport()
	budget := p.Y + vh + 8 // visible + small overscroll
	if budget < 16 {
		budget = 16
	}
	if budget > maxDisasmInstrs {
		budget = maxDisasmInstrs
	}

	pcLineLY := -1
	var pcInstrPtr *disasm.Instr
	idx := 0
	for uint64(addr) < end && idx < budget {
		ins := disasm.Decode(addr, p.Bus.Read)
		ly := idx + 1

		marker := "  "
		rowStyle := bg
		size := uint16(ins.Size())
		if hOK && hAddr >= ins.Addr && hAddr < ins.Addr+size {
			marker = "> "
			rowStyle = pcStyle
			pcLineLY = ly
		}

		c.Put(0, ly, marker, rowStyle)
		if idx == 0 && p.EditableBase {
			p.drawEditableAddr(c, 2, ly, editStyle)
		} else {
			c.Put(2, ly, fmt.Sprintf("$%04X", ins.Addr), rowStyle)
		}
		c.Put(7, ly, "  ", rowStyle)
		c.Put(9, ly, disasm.HexBytes(ins.Bytes), rowStyle)
		// Pretty + symbol-substituted operand + per-instruction comment.
		c.Put(19, ly, "  "+p.annotateInstrText(ins.Addr, ins.Pretty), rowStyle)

		// Save the PC-marked instruction so we can render the info
		// panel after the loop (needs live registers + bus reads).
		if pcLineLY == ly {
			pcInstr := ins
			pcInstrPtr = &pcInstr
		}

		next := uint64(addr) + uint64(ins.Size())
		if next > 0x10000 {
			break
		}
		addr = uint16(next)
		idx++
	}
	p.lastInstrCount = idx

	// Side info panel — only when enabled, the PC line is in view, and
	// we have a backend to query for live registers.
	if p.ShowInfo && pcInstrPtr != nil && p.Backend != nil {
		p.drawInfoPanel(c, bg, *pcInstrPtr)
	}

	// Auto-scroll to keep PC line visible.
	if pcLineLY >= 0 {
		_, vh := p.LastViewport()
		if vh > 0 {
			if pcLineLY < p.Y {
				p.SetScrollOffset(p.X, pcLineLY)
			} else if pcLineLY >= p.Y+vh {
				p.SetScrollOffset(p.X, pcLineLY-vh+1)
			}
		}
	}
}

// drawInfoPanel renders the side panel for the PC-marked instruction
// at logical x=panelX. Shows action (algebraic), value/address, and
// cycle count. Pulls live register state from p.Backend.
func (p *Provider) drawInfoPanel(c *foxpro.Canvas, bg tcell.Style, ins disasm.Instr) {
	const panelX = 36

	regs := p.Backend.Registers()
	mn := ins.Op.Mn

	// Action.
	eff := disasm.Effect(mn)
	if eff == "" {
		eff = "?"
	}
	c.Put(panelX, 1, "Action: "+eff, bg)

	// Operand resolution: figure out the effective value/address per
	// addressing mode, reading current memory + register state.
	row := 3
	switch ins.Op.Md {
	case disasm.Immediate:
		v := ins.Bytes[1]
		c.Put(panelX, row, fmt.Sprintf("Value:  $%02X (%d)", v, v), bg)
	case disasm.ZeroPage:
		a := uint16(ins.Bytes[1])
		v := p.Bus.Read(a)
		c.Put(panelX, row, fmt.Sprintf("Addr:   $%04X", a), bg)
		row++
		c.Put(panelX, row, fmt.Sprintf("Value:  $%02X (%d)", v, v), bg)
	case disasm.ZeroPageX:
		a := uint16(ins.Bytes[1] + regs.X)
		v := p.Bus.Read(a)
		c.Put(panelX, row, fmt.Sprintf("Addr:   $%04X (zp+X)", a), bg)
		row++
		c.Put(panelX, row, fmt.Sprintf("Value:  $%02X (%d)", v, v), bg)
	case disasm.ZeroPageY:
		a := uint16(ins.Bytes[1] + regs.Y)
		v := p.Bus.Read(a)
		c.Put(panelX, row, fmt.Sprintf("Addr:   $%04X (zp+Y)", a), bg)
		row++
		c.Put(panelX, row, fmt.Sprintf("Value:  $%02X (%d)", v, v), bg)
	case disasm.Absolute:
		a := uint16(ins.Bytes[1]) | uint16(ins.Bytes[2])<<8
		// JMP/JSR don't read the value, just jump.
		if mn == "JMP" || mn == "JSR" {
			c.Put(panelX, row, fmt.Sprintf("Target: $%04X", a), bg)
		} else {
			v := p.Bus.Read(a)
			c.Put(panelX, row, fmt.Sprintf("Addr:   $%04X", a), bg)
			row++
			c.Put(panelX, row, fmt.Sprintf("Value:  $%02X (%d)", v, v), bg)
		}
	case disasm.AbsoluteX:
		a := (uint16(ins.Bytes[1]) | uint16(ins.Bytes[2])<<8) + uint16(regs.X)
		v := p.Bus.Read(a)
		c.Put(panelX, row, fmt.Sprintf("Addr:   $%04X (abs+X)", a), bg)
		row++
		c.Put(panelX, row, fmt.Sprintf("Value:  $%02X (%d)", v, v), bg)
	case disasm.AbsoluteY:
		a := (uint16(ins.Bytes[1]) | uint16(ins.Bytes[2])<<8) + uint16(regs.Y)
		v := p.Bus.Read(a)
		c.Put(panelX, row, fmt.Sprintf("Addr:   $%04X (abs+Y)", a), bg)
		row++
		c.Put(panelX, row, fmt.Sprintf("Value:  $%02X (%d)", v, v), bg)
	case disasm.Indirect:
		ptr := uint16(ins.Bytes[1]) | uint16(ins.Bytes[2])<<8
		// 6502 indirect-bug-aware fetch.
		lo := p.Bus.Read(ptr)
		hi := p.Bus.Read((ptr & 0xFF00) | uint16(uint8(ptr)+1))
		t := uint16(lo) | uint16(hi)<<8
		c.Put(panelX, row, fmt.Sprintf("Ptr:    $%04X", ptr), bg)
		row++
		c.Put(panelX, row, fmt.Sprintf("Target: $%04X", t), bg)
	case disasm.IndirectX:
		zp := ins.Bytes[1] + regs.X
		a := uint16(p.Bus.Read(uint16(zp))) | uint16(p.Bus.Read(uint16(zp+1)))<<8
		v := p.Bus.Read(a)
		c.Put(panelX, row, fmt.Sprintf("Addr:   $%04X ($zp,X)", a), bg)
		row++
		c.Put(panelX, row, fmt.Sprintf("Value:  $%02X (%d)", v, v), bg)
	case disasm.IndirectY:
		zp := ins.Bytes[1]
		base := uint16(p.Bus.Read(uint16(zp))) | uint16(p.Bus.Read(uint16(zp+1)))<<8
		a := base + uint16(regs.Y)
		v := p.Bus.Read(a)
		c.Put(panelX, row, fmt.Sprintf("Addr:   $%04X ($zp),Y", a), bg)
		row++
		c.Put(panelX, row, fmt.Sprintf("Value:  $%02X (%d)", v, v), bg)
	case disasm.Relative:
		off := int8(ins.Bytes[1])
		t := uint16(int(ins.Addr) + 2 + int(off))
		c.Put(panelX, row, fmt.Sprintf("Target: $%04X", t), bg)
	case disasm.Accumulator:
		c.Put(panelX, row, fmt.Sprintf("On A:   $%02X (%d)", regs.A, regs.A), bg)
	}

	// Cycles always.
	cyc := disasm.Cycles[ins.Bytes[0]]
	if cyc == 0 {
		cyc = 2
	}
	c.Put(panelX, 7, fmt.Sprintf("Cycles: %d", cyc), bg)
}

// drawEditableAddr renders the address as the live $XXXX or, while
// in edit mode, the partial input padded with underscores.
func (p *Provider) drawEditableAddr(c *foxpro.Canvas, x, y int, st tcell.Style) {
	var s string
	if p.inputting {
		padded := p.inputBuf + "____"
		s = "$" + padded[:4]
	} else {
		s = fmt.Sprintf("$%04X", p.Base)
	}
	c.Put(x, y, s, st)
}

func (p *Provider) HandleKey(ev *tcell.EventKey) bool {
	// Goto-input mode swallows most keys until Enter / Esc.
	if p.inputting {
		switch ev.Key() {
		case tcell.KeyEnter:
			p.applyInput()
			return true
		case tcell.KeyEscape:
			p.inputting = false
			p.inputBuf = ""
			return true
		case tcell.KeyBackspace, tcell.KeyBackspace2:
			if len(p.inputBuf) > 0 {
				p.inputBuf = p.inputBuf[:len(p.inputBuf)-1]
			}
			return true
		case tcell.KeyRune:
			r := ev.Rune()
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if isHex && len(p.inputBuf) < 4 {
				p.inputBuf += string(r)
				if len(p.inputBuf) == 4 {
					p.applyInput()
				}
			}
			return true
		}
		return true
	}

	if ev.Key() == tcell.KeyRune {
		switch ev.Rune() {
		case 'v', 'V':
			// Cycle Hex → Disasm → Labels → Hex. Skip Labels when
			// the provider has no symbols loaded — shows an empty
			// list which is confusing.
			switch p.View {
			case ViewHex:
				p.View = ViewDisasm
			case ViewDisasm:
				if len(p.Symbols) > 0 {
					p.View = ViewLabels
				} else {
					p.View = ViewHex
				}
			default: // ViewLabels (or unknown)
				p.View = ViewHex
			}
			p.SetScrollOffset(0, 0)
			return true
		case 'i', 'I':
			p.ShowInfo = !p.ShowInfo
			return true
		case 'g', 'G':
			if p.EditableBase {
				p.SetScrollOffset(0, 0)
				p.inputting = true
				p.inputBuf = ""
				return true
			}
		}
	}

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
		if p.View == ViewDisasm {
			p.SetScrollOffset(p.X, p.lastInstrCount)
		} else {
			p.SetScrollOffset(p.X, p.totalRows()+1)
		}
		return true
	}
	return false
}

func (p *Provider) StatusHint() string {
	if p.EditableBase {
		return "g/click goto  v hex/disasm  ↑/↓ scroll"
	}
	return "↑/↓/←/→ scroll  PgUp/PgDn page  v view"
}

// HandleMouse — Button1 click on the editable address label enters
// edit mode. The label sits at logical y=1 in both views; column
// range differs (hex: x=0..6, disasm: x=2..6).
func (p *Provider) HandleMouse(ev *tcell.EventMouse, inner foxpro.Rect) bool {
	if !p.EditableBase {
		return false
	}
	if ev.Buttons()&tcell.Button1 == 0 {
		return false
	}
	mx, my := ev.Position()
	lx := (mx - inner.X) + p.X
	ly := (my - inner.Y) + p.Y
	if ly != 1 {
		return false
	}
	hit := false
	if p.View == ViewDisasm {
		hit = lx >= 2 && lx < 7
	} else {
		hit = lx >= 0 && lx < 5
	}
	if hit {
		p.SetScrollOffset(0, 0)
		p.inputting = true
		p.inputBuf = ""
		return true
	}
	return false
}
