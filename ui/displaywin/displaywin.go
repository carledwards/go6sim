// Package displaywin renders the VIC framebuffer with a single-line
// blue-on-cyan border and a vertical strip of command buttons on
// the right. Button clicks POKE the controller via the bus — same
// path the CPU uses.
package displaywin

import (
	"fmt"
	"time"

	"github.com/gdamore/tcell/v2"

	foxpro "github.com/carledwards/foxpro-go"
	"github.com/carledwards/go6sim/bus"
	"github.com/carledwards/go6sim/components/display"
)

// MinW / MinH cover the chrome border + a tiny visible interior so a
// resized display is still recognisable as one. Real natural size is
// much bigger; Canvas + ScrollState lets the user drag down freely.
const (
	MinW = 14
	MinH = 8
)

// chromeStyle returns the blue-on-cyan look shared by the display
// border and the resting state of buttons. Pulls from the theme
// palette so cursor inversion and theming work.
func chromeStyle(theme foxpro.Theme) tcell.Style {
	return tcell.StyleDefault.
		Background(theme.Palette.Cyan).
		Foreground(theme.Palette.Blue)
}

// buttonDef describes one of the right-column command buttons. Each
// fires a single bus.Write to the controller's address+reg with the
// configured value — the same operation the CPU performs.
// buttonDef holds one row of the button grid. applicableInGfx is
// false for buttons whose POKE targets a register that doesn't
// apply in graphics mode (text-grid scroll/rotate, char-plane
// invert) — those hide themselves there. Frame Sync and Clear
// stay visible in both modes.
type buttonDef struct {
	label           string // padded to fixed width for column alignment
	reg             uint16 // controller register offset (0/1/2)
	val             uint8  // value to write
	applicableInGfx bool
}

// Labels are trimmed; Draw centers each one inside a fixed-width
// field so the brackets line up and shorter labels (Clear, Invert)
// don't hug the left edge.
var buttonDefs = []buttonDef{
	{"Frame Sync", display.RegFrame, 0x01, true},
	{"Clear", display.RegCmd, display.CmdClear, true},
	{"Invert", display.RegCmd, display.CmdInvert, false},
	{"Scroll Left", display.RegCmd, display.CmdShiftLeft, false},
	{"Scroll Right", display.RegCmd, display.CmdShiftRight, false},
	{"Scroll Up", display.RegCmd, display.CmdShiftUp, false},
	{"Scroll Down", display.RegCmd, display.CmdShiftDown, false},
	{"Rotate Left", display.RegCmd, display.CmdRotLeft, false},
	{"Rotate Right", display.RegCmd, display.CmdRotRight, false},
	{"Rotate Up", display.RegCmd, display.CmdRotUp, false},
	{"Rotate Down", display.RegCmd, display.CmdRotDown, false},
}

const (
	// buttonLabelW is the fixed width the label is centered into.
	// Wide enough to hold "Scroll Right" / "Rotate Right" (12 chars)
	// without truncation.
	buttonLabelW = 12

	// Each button is rendered as "○ < Label >" — indicator + space
	// + bracket + space + 12-char centered label + space + bracket.
	buttonW = 1 + 1 + 1 + 1 + buttonLabelW + 1 + 1 // = 18

	// Indicator stays "filled" for this long after a fire so the
	// user sees a brief acknowledgment.
	flashDuration = 300 * time.Millisecond
)

// centerLabel pads s with leading and trailing spaces so the result
// is exactly w cells wide, with s centered. Extra-odd remainder pads
// the right (matches FoxPro's "left-bias" centering convention).
// Assumes runeLen(s) <= w.
func centerLabel(s string, w int) string {
	n := runeLen(s)
	if n >= w {
		return s
	}
	left := (w - n) / 2
	right := w - n - left
	return spaces(left) + s + spaces(right)
}

func spaces(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}

// Provider renders a memory-mapped framebuffer. ColorBase points at
// the color plane (low nibble = fg, high nibble = bg). CharBase, if
// HasChars, points at a parallel char plane. CtrlBase, if HasCtrl,
// points at the controller's command register — the buttons POKE
// values there.
type Provider struct {
	Bus           bus.Bus
	Controller    *display.Controller // pause-aware framebuffer reads
	ColorBase     uint16
	CharBase      uint16
	CtrlBase      uint16
	HasChars      bool
	HasCtrl       bool
	Width         int
	Height        int
	CellsPerPixel int

	// Graphics is an optional pixel plane. When set AND the controller
	// reports graphics mode (RegMode == ModeGraphics), the framebuffer
	// area of the window switches its rendering. Leave nil to disable
	// graphics mode entirely.
	Graphics *display.GraphicsPlane

	// Window, if set, has its Title rewritten each Draw to reflect
	// the current display mode — so the VIC's own chrome shows
	// "[TEXT]" or "[GFX]" without needing a separate status indicator.
	// The base title is captured on first Draw; subsequent updates
	// only swap the trailing tag.
	Window    *foxpro.Window
	baseTitle string

	// RenderBlockArt selects the graphics-mode rendering strategy:
	//
	//   - false (default): framebuffer cells are stamped with a
	//     pixel-placeholder sentinel rune. Hosts that can substitute
	//     pixel data per-cell (the wasm bridge) see the sentinel and
	//     overlay actual pixels there. Terminal hosts will render
	//     the sentinel as a missing-glyph "tofu" — set this flag to
	//     true to get block-art instead.
	//
	//   - true: framebuffer cells are filled with upper-half-block
	//     (▀) glyphs whose fg/bg encode two stacked pixels. Loses
	//     half the vertical resolution but renders cleanly in any
	//     terminal that supports 24-bit color.
	//
	// The flag has no effect when not in graphics mode.
	RenderBlockArt bool

	foxpro.ScrollState

	// Button drag state. pressedIdx is the index in buttonDefs of
	// the button under the initial mouse-down (-1 if no press).
	// armed tracks whether the cursor is still over that same button.
	pressedIdx int
	armed      bool

	// Per-button "recently fired" timestamps for the indicator flash.
	lastFire []time.Time

	// modeRect captures the on-screen rect of the Mode picker box
	// from the last Draw, in screen coords. HandleMouse uses it to
	// detect click-to-open-popup. Refreshed every frame.
	modeRect foxpro.Rect

	// OnPickMode, when set, is called with a click on the Mode
	// picker. The host opens its own popup (so displaywin doesn't
	// take a hard dependency on dialog or on app.Manager). If nil,
	// the click cycles Text↔Graphics directly via the controller.
	OnPickMode func()

	// Expanded controls whether the buttons grid (Frame Sync, Clear,
	// Scroll/Rotate …) renders below the framebuffer. Collapsed
	// (default) is a quieter view that only shows the live screen +
	// Status + Mode picker — appropriate when the host wants the
	// window to act as a passive display and the user only
	// occasionally needs the manual POKE controls. The checkbox
	// drawn in the right column toggles it.
	Expanded bool

	// OnToggleExpanded fires after Expanded flips. Hosts use it to
	// resize the containing window so the buttons grid actually has
	// room (or the collapsed window shrinks back down). The flag
	// flips before the callback, so the host can read p.Expanded to
	// pick the new height. Optional — nil means the flag toggles but
	// the window keeps its current bounds.
	OnToggleExpanded func()

	// expandedRect captures the on-screen rect of the "[X] Expanded
	// view" checkbox from the most recent Draw, in screen coords.
	// HandleMouse uses it for click hit-testing.
	expandedRect foxpro.Rect
}

// cellsPerPixel returns the on-screen cell width of one logical
// pixel. Default: 1 in text mode, 2 in graphics mode.
func (p *Provider) cellsPerPixel() int {
	if p.CellsPerPixel > 0 {
		return p.CellsPerPixel
	}
	if p.HasChars {
		return 1
	}
	return 2
}

// visibleButtonDefs returns the buttonDef indices that should render
// in the current mode. Frame Sync and Clear (applicableInGfx=true)
// always show; the rest only in text mode.
func (p *Provider) visibleButtonDefs() []int {
	gfx := p.graphicsActive()
	out := make([]int, 0, len(buttonDefs))
	for i, def := range buttonDefs {
		if gfx && !def.applicableInGfx {
			continue
		}
		out = append(out, i)
	}
	return out
}

// buttonGridStartY returns the canvas-y of the button grid's first
// row — one blank line below the display's bottom border.
func (p *Provider) buttonGridStartY() int { return p.Height + 3 }

// buttonGridCols returns how many button columns fit across the
// inner width. Buttons are 18 cells wide with a 2-cell gap between
// columns so the indicator '○' on the next button doesn't sit
// directly against the '>' of the previous one.
func buttonGridCols(innerW int) int {
	const colW = buttonW + 2 // 18 + 2 gap
	if innerW < buttonW {
		return 1
	}
	n := (innerW + 2) / colW
	if n < 1 {
		n = 1
	}
	return n
}

// buttonRect returns the (x, y, width) of the visible button at
// position `visIdx` in the grid. Visible buttons are laid out
// left-to-right, top-to-bottom in a `cols`-wide grid starting at
// buttonGridStartY().
func (p *Provider) buttonRect(visIdx, cols int) (x, y, w int) {
	if cols < 1 {
		cols = 1
	}
	row := visIdx / cols
	col := visIdx % cols
	return col * (buttonW + 2), p.buttonGridStartY() + row, buttonW
}

// buttonHitIdx returns the buttonDefs index at screen-space (mx, my),
// or -1 if no visible button is hit. Translates through the canvas
// scroll and through the visible-only grid mapping.
func (p *Provider) buttonHitIdx(mx, my int, inner foxpro.Rect) int {
	lx := (mx - inner.X) + p.X
	ly := (my - inner.Y) + p.Y
	visible := p.visibleButtonDefs()
	cols := buttonGridCols(inner.W)
	for vIdx, defIdx := range visible {
		bx, by, bw := p.buttonRect(vIdx, cols)
		if lx >= bx && lx < bx+bw && ly == by {
			return defIdx
		}
	}
	return -1
}

// fireButton writes the button's POKE pair to the controller via
// the bus, then records the timestamp for the indicator flash.
func (p *Provider) fireButton(idx int) {
	if !p.HasCtrl {
		return
	}
	def := buttonDefs[idx]
	p.Bus.Write(p.CtrlBase+def.reg, def.val)
	if p.lastFire == nil {
		p.lastFire = make([]time.Time, len(buttonDefs))
	}
	p.lastFire[idx] = time.Now()
}

func (p *Provider) Draw(screen tcell.Screen, inner foxpro.Rect, theme foxpro.Theme, focused bool) {
	// Rewrite the window's title to advertise the current display
	// mode. Captured once on first Draw so reset/runtime title
	// changes from outside flow through.
	if p.Window != nil {
		if p.baseTitle == "" {
			p.baseTitle = stripModeTag(p.Window.Title)
		}
		tag := "TEXT"
		if p.graphicsActive() {
			tag = "GFX"
		}
		p.Window.Title = p.baseTitle + "  [" + tag + "]"
	}

	c := foxpro.NewCanvas(screen, inner, &p.ScrollState)
	chrome := chromeStyle(theme)
	frame := chrome
	bg := theme.WindowBG

	cpp := p.cellsPerPixel()
	pxW := p.Width * cpp
	pxH := p.Height

	// ─── Display border + pixels ────────────────────────────
	c.Set(0, 0, '┌', frame)
	for i := 0; i < pxW; i++ {
		c.Set(1+i, 0, '─', frame)
	}
	c.Set(1+pxW, 0, '┐', frame)

	gfxMode := p.graphicsActive()

	for py := 0; py < pxH; py++ {
		ly := 1 + py
		c.Set(0, ly, '│', frame)
		if gfxMode {
			// Tag each framebuffer cell with the pixel-placeholder rune
			// (Unicode Private-Use-Area U+E000). The wasm bridge
			// passes this through to JS untouched; JS detects the
			// codepoint and substitutes pixel data from the graphics
			// plane there. Cells overwritten by other windows or by
			// drop shadows lose the sentinel and render normally —
			// the substitution honors foxpro's z-order for free.
			//
			// Terminal hosts can't substitute pixels per-cell, so the
			// drawGraphicsBlockArt pass below overwrites these cells
			// with block-art glyphs before the frame ships.
			for k := 0; k < pxW; k++ {
				c.Set(1+k, ly, pixelPlaceholderRune, bg)
			}
			c.Set(1+pxW, ly, '│', frame)
			continue
		}
		for px := 0; px < p.Width; px++ {
			off := py*p.Width + px
			var color uint8
			if p.Controller != nil {
				color = p.Controller.ReadColor(off)
			} else {
				color = p.Bus.Read(p.ColorBase + uint16(off))
			}
			fg := palette[color&0x0F]
			bgPx := palette[(color>>4)&0x0F]
			cellStyle := tcell.StyleDefault.Background(bgPx).Foreground(fg)

			ch := rune(' ')
			if p.HasChars {
				var raw uint8
				if p.Controller != nil {
					raw = p.Controller.ReadChar(off)
				} else {
					raw = p.Bus.Read(p.CharBase + uint16(off))
				}
				if raw >= 0x20 && raw < 0x7F {
					ch = rune(raw)
				}
			}

			lx := 1 + px*cpp
			c.Set(lx, ly, ch, cellStyle)
			for k := 1; k < cpp; k++ {
				c.Set(lx+k, ly, ' ', cellStyle)
			}
		}
		c.Set(1+pxW, ly, '│', frame)
	}

	// Block-art fallback (opt-in via RenderBlockArt) — terminal hosts
	// that can't do per-cell pixel substitution overwrite the sentinel
	// cells with upper-half-block glyphs encoding two stacked pixels.
	// Browser hosts must NOT enter this branch: the substitution path
	// relies on the sentinels staying intact in the cell snapshot.
	if gfxMode && p.Graphics != nil && p.RenderBlockArt {
		drawGraphicsBlockArt(c, p.Graphics, 1, 1, pxW, pxH)
	}

	by := 1 + pxH
	c.Set(0, by, '└', frame)
	for i := 0; i < pxW; i++ {
		c.Set(1+i, by, '─', frame)
	}
	c.Set(1+pxW, by, '┘', frame)

	// ─── Right column: Status + Mode picker ────────────────
	// Hidden when collapsed — they sit in the right column, so the
	// host can also shrink the window's width down to just the
	// framebuffer + borders when neither needs to be visible.
	rcX := pxW + 4
	p.modeRect = foxpro.Rect{} // cleared every frame; only set when actually drawn
	if p.Expanded && p.HasCtrl && p.Controller != nil {
		paused := p.Controller.IsPaused()
		status := "Running    "
		if paused {
			// "Paused" reads as "stopped" to a user, but really
			// the display has just cut over to the snapshot taken
			// at Frame Sync — so the demo controls when frames
			// commit. "Manual Sync" describes that mode honestly.
			status = "Manual Sync"
		}
		c.Put(rcX, 0, "Status: "+status, bg)

		// Mode field — boxed value the user clicks to open the
		// Text / Graphics popup. FoxPro picker fields use a
		// "shadow" border: single line on top + left, double line
		// on bottom + right, mixed corner glyphs (┌──╖ / ╘══╝).
		// That asymmetry reads as a 3D recessed surface, marking
		// the cell as clickable.
		//
		// Label reflects Controller.Mode() directly, NOT
		// graphicsActive(). The latter is gated on a Graphics plane
		// being attached — true in wasm, false in the terminal
		// build — so using it would freeze the label at "Text" in
		// the sim even after the user selects Graphics.
		// Width is sized to the longest possible label ("Graphics")
		// + 1 cell of padding on each side. Shorter labels ("Text")
		// get centered in the inner span so the box visually
		// "holds" the value rather than left-hugging it.
		const modeMaxLabelLen = 8 // "Graphics"
		modeLabel := "Text"
		if p.Controller.Mode() == display.ModeGraphics {
			modeLabel = "Graphics"
		}
		modeBoxX := rcX + 12 // after "Video Mode: "
		modeBoxY := 2
		modeBoxW := modeMaxLabelLen + 4 // 2 borders + 2 padding + label
		modeInnerW := modeBoxW - 2
		labelLen := runeLen(modeLabel)
		labelPadL := (modeInnerW - labelLen) / 2
		c.Put(rcX, modeBoxY+1, "Video Mode: ", bg)
		// Top edge ─, right edge ║, bottom edge ═, left edge │.
		c.Set(modeBoxX, modeBoxY, '┌', frame)
		for i := 1; i < modeBoxW-1; i++ {
			c.Set(modeBoxX+i, modeBoxY, '─', frame)
		}
		c.Set(modeBoxX+modeBoxW-1, modeBoxY, '╖', frame)
		c.Set(modeBoxX, modeBoxY+1, '│', frame)
		// Fill inner row with body bg so the previous label's chars
		// don't ghost when the user picks a shorter option.
		for i := 1; i < modeBoxW-1; i++ {
			c.Set(modeBoxX+i, modeBoxY+1, ' ', bg)
		}
		c.Put(modeBoxX+1+labelPadL, modeBoxY+1, modeLabel, bg)
		c.Set(modeBoxX+modeBoxW-1, modeBoxY+1, '║', frame)
		c.Set(modeBoxX, modeBoxY+2, '╘', frame)
		for i := 1; i < modeBoxW-1; i++ {
			c.Set(modeBoxX+i, modeBoxY+2, '═', frame)
		}
		c.Set(modeBoxX+modeBoxW-1, modeBoxY+2, '╝', frame)
		// Save the screen rect for HandleMouse hit-testing. Convert
		// canvas-logical coords to screen coords by adding inner.X/Y
		// and subtracting the canvas scroll.
		p.modeRect = foxpro.Rect{
			X: inner.X + modeBoxX - p.X,
			Y: inner.Y + modeBoxY - p.Y,
			W: modeBoxW,
			H: 3,
		}
	}

	// ─── Expanded-view checkbox (below the framebuffer) ─────
	// Sits one row under the bottom border so it stays visible in
	// both collapsed and expanded states — collapsed gives just the
	// framebuffer + this toggle; expanded reveals Status + Mode +
	// the buttons grid. Click flips p.Expanded and fires
	// OnToggleExpanded so the host can resize the window.
	{
		mark := ' '
		if p.Expanded {
			mark = 'X'
		}
		label := fmt.Sprintf("[%c] Expanded view", mark)
		cbX := 1
		cbY := pxH + 2 // one row below the '└──┘' bottom edge
		c.Put(cbX, cbY, label, bg)
		p.expandedRect = foxpro.Rect{
			X: inner.X + cbX - p.X,
			Y: inner.Y + cbY - p.Y,
			W: runeLen(label),
			H: 1,
		}
	}

	// ─── Buttons grid (below the display) ───────────────────
	// Skipped entirely when Expanded is false — the right-column
	// checkbox toggles it back on. Right column (Status, Mode picker,
	// checkbox) stays visible in both states.
	if !p.Expanded {
		return
	}
	visible := p.visibleButtonDefs()
	cols := buttonGridCols(inner.W)
	now := time.Now()
	for vIdx, defIdx := range visible {
		def := buttonDefs[defIdx]
		bxc, byc, _ := p.buttonRect(vIdx, cols)

		// Indicator: ● when this button's action is currently in
		// flight, regardless of source — UI armed press, recent UI
		// fire, or recent CPU write that matches this button's
		// (reg, val) pair.
		lit := false
		if p.pressedIdx == defIdx && p.armed {
			lit = true
		}
		if p.lastFire != nil && now.Sub(p.lastFire[defIdx]) < flashDuration {
			lit = true
		}
		switch def.reg {
		case display.RegCmd:
			if p.Controller != nil && p.Controller.LastCmd() == def.val {
				if d, ok := p.Controller.SinceLastCmd(); ok && d < flashDuration {
					lit = true
				}
			}
		case display.RegFrame:
			if p.Controller != nil {
				if d, ok := p.Controller.SinceLastFrame(); ok && d < flashDuration {
					lit = true
				}
			}
		}
		ind := '○'
		if lit {
			ind = '●'
		}
		c.Set(bxc, byc, ind, chrome)

		labelStyle := chrome
		if p.pressedIdx == defIdx && p.armed {
			labelStyle = theme.Focus
		}
		c.Put(bxc+2, byc, "< "+centerLabel(def.label, buttonLabelW)+" >", labelStyle)
	}
}

// ModeRect returns the on-screen rect of the Mode picker box from
// the most recent Draw, in screen coords. Hosts that wire up a
// popup via OnPickMode use this to anchor the popup just under the
// field. Returns the zero Rect before the first Draw.
func (p *Provider) ModeRect() foxpro.Rect { return p.modeRect }

// runeLen counts the runes in s — distinct from len(s) which returns
// bytes. Used for layout math against on-screen cell widths.
func runeLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
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
	}
	return false
}

func (p *Provider) HandleMouse(ev *tcell.EventMouse, inner foxpro.Rect) bool {
	btns := ev.Buttons()
	mx, my := ev.Position()

	if btns&tcell.Button1 == 0 {
		return false
	}

	if p.modeRect.Contains(mx, my) {
		if p.OnPickMode != nil {
			p.OnPickMode()
		} else if p.Controller != nil {
			cur := p.Controller.Mode()
			next := uint8(display.ModeChar)
			if cur == display.ModeChar {
				next = display.ModeGraphics
			}
			p.Bus.Write(p.CtrlBase+display.RegMode, next)
		}
		return true
	}

	if p.expandedRect.Contains(mx, my) {
		p.Expanded = !p.Expanded
		if p.OnToggleExpanded != nil {
			p.OnToggleExpanded()
		}
		return true
	}

	idx := p.buttonHitIdx(mx, my, inner)
	if idx >= 0 {
		p.pressedIdx = idx
		p.armed = true
		return true
	}
	p.pressedIdx = -1
	p.armed = false
	return false
}

func (p *Provider) HandleMouseMotion(ev *tcell.EventMouse, inner foxpro.Rect) {
	if p.pressedIdx < 0 {
		return
	}
	mx, my := ev.Position()
	idx := p.buttonHitIdx(mx, my, inner)
	p.armed = idx == p.pressedIdx
}

func (p *Provider) HandleMouseRelease(ev *tcell.EventMouse, inner foxpro.Rect) {
	if p.armed && p.pressedIdx >= 0 {
		p.fireButton(p.pressedIdx)
	}
	p.pressedIdx = -1
	p.armed = false
}

func (p *Provider) StatusHint() string {
	return "↑/↓/←/→ scroll  PgUp/PgDn page  [/] mem ±row  {/} mem ±page  wheel mem"
}

// Palette — 16-entry CGA-ish color table indexed by the low nibble
// of each framebuffer byte.
var palette = [16]tcell.Color{
	tcell.ColorBlack,
	tcell.ColorNavy,
	tcell.ColorGreen,
	tcell.ColorTeal,
	tcell.ColorMaroon,
	tcell.ColorPurple,
	tcell.ColorOlive,
	tcell.ColorSilver,
	tcell.ColorGray,
	tcell.ColorBlue,
	tcell.ColorLime,
	tcell.ColorAqua,
	tcell.ColorRed,
	tcell.ColorFuchsia,
	tcell.ColorYellow,
	tcell.ColorWhite,
}
