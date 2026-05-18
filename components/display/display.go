// Package display is a memory-mapped framebuffer component. Each
// byte is one logical pixel; the UI maps the byte's low nibble
// through a 16-color palette.
package display

import (
	"time"

	"github.com/carledwards/go6sim/asm"
)

// Display is a width × height byte grid mapped onto a contiguous
// region of the bus. Reads / Writes through the bus interface
// behave like ordinary RAM.
type Display struct {
	name   string
	base   uint16
	width  int
	height int
	bytes  []uint8
}

// New creates a Display of the given dimensions at the given base.
func New(name string, base uint16, width, height int) *Display {
	return &Display{
		name:   name,
		base:   base,
		width:  width,
		height: height,
		bytes:  make([]uint8, width*height),
	}
}

func (d *Display) Name() string                   { return d.name }
func (d *Display) Base() uint16                   { return d.base }
func (d *Display) Size() int                      { return d.width * d.height }
func (d *Display) Read(offset uint16) uint8       { return d.bytes[offset] }
func (d *Display) Write(offset uint16, val uint8) { d.bytes[offset] = val }

func (d *Display) Width() int  { return d.width }
func (d *Display) Height() int { return d.height }

// SetPixel is a host-side helper for initial fills (the CPU writes
// via the bus, not through this).
func (d *Display) SetPixel(x, y int, val uint8) {
	if x < 0 || x >= d.width || y < 0 || y >= d.height {
		return
	}
	d.bytes[y*d.width+x] = val
}

// Bytes exposes the backing byte slice. Used by Controller for fast
// in-place transforms (shift, fill, invert) — bypasses the per-cell
// bus dispatch.
func (d *Display) Bytes() []uint8 { return d.bytes }

// Controller register offsets within its 7-byte block on the bus.
//
//	+0  command register — write triggers an op (Clear, shifts,
//	    rotates, Invert, rect-shifts, rect-rotates). Reads return
//	    the most-recently-written cmd.
//	+1  pause register — bit 0 selects display source: 0 = live
//	    (UI reads current memory), 1 = paused (UI reads snapshot
//	    captured at the most recent Pause / Frame). Read/write.
//	+2  frame trigger — any write captures a fresh snapshot. Reads
//	    return 0. Use while paused to commit a finished frame.
//	+3..+6  rect parameters X, Y, W, H — set before issuing a
//	    CmdRect* command. Whole-display commands (Clear, ShiftUp,
//	    Invert, etc.) ignore them. Coords are clamped to the display.
const (
	RegCmd      uint16 = 0
	RegPause    uint16 = 1
	RegFrame    uint16 = 2
	RegRectX    uint16 = 3
	RegRectY    uint16 = 4
	RegRectW    uint16 = 5
	RegRectH    uint16 = 6
	RegGfxColor uint16 = 7 // current draw color for CmdGfx* (palette index 0–15)
	RegMode     uint16 = 8 // display mode: 0 = char (default), 1 = graphics
)

// Display modes for RegMode.
const (
	ModeChar     uint8 = 0
	ModeGraphics uint8 = 1
)

// Command opcodes recognised by the command register at +0.
const (
	CmdNone       uint8 = 0x00
	CmdClear      uint8 = 0x01
	CmdShiftUp    uint8 = 0x02 // blank-bottom-row scroll up (whole display)
	CmdShiftDown  uint8 = 0x03 // blank-top-row scroll down (whole display)
	CmdShiftLeft  uint8 = 0x04 // blank-right-column scroll left (whole display)
	CmdShiftRight uint8 = 0x05 // blank-left-column scroll right (whole display)
	CmdInvert     uint8 = 0x06 // photographic negative of color plane
	CmdRotLeft    uint8 = 0x07 // wrap-around horizontal rotate left (whole display)
	CmdRotRight   uint8 = 0x08 // wrap-around horizontal rotate right (whole display)
	CmdRotUp      uint8 = 0x09 // wrap-around vertical rotate up (whole display)
	CmdRotDown    uint8 = 0x0A // wrap-around vertical rotate down (whole display)

	// Rect-bound variants — operate only on the rectangle defined
	// by RegRectX/Y/W/H. Coords clamp to the display; out-of-bounds
	// or zero-size rects are no-ops.
	CmdRectShiftUp    uint8 = 0x0B // blank-bottom-row scroll up within rect
	CmdRectShiftDown  uint8 = 0x0C // blank-top-row scroll down within rect
	CmdRectShiftLeft  uint8 = 0x0D // blank-right-column scroll left within rect
	CmdRectShiftRight uint8 = 0x0E // blank-left-column scroll right within rect
	CmdRectRotUp      uint8 = 0x0F // wrap-around vertical rotate up within rect
	CmdRectRotDown    uint8 = 0x10 // wrap-around vertical rotate down within rect
	CmdRectRotLeft    uint8 = 0x11 // wrap-around horizontal rotate left within rect
	CmdRectRotRight   uint8 = 0x12 // wrap-around horizontal rotate right within rect

	// Graphics-mode commands — only effective when the controller is
	// bound to a GraphicsPlane (NewControllerWithGraphics, or by
	// assigning Controller.Graphics). All use the current GfxColor
	// register as the draw color. Coords come from RegRectX/Y/W/H,
	// reinterpreted per command:
	//
	//   CmdGfxClear     — fill plane with GfxColor (no coords)
	//   CmdGfxPlot      — pixel at (RectX, RectY)
	//   CmdGfxLine      — line from (RectX, RectY) to (RectW, RectH)
	//                     (RectW/H reinterpreted as the second endpoint X/Y)
	//   CmdGfxRectFill  — fill rect (RectX, RectY, RectW, RectH)
	//   CmdGfxRectStroke— outline rect
	//   CmdGfxCircle    — outline circle, centre (RectX, RectY), radius RectW
	//   CmdGfxFillCircle— filled circle
	CmdGfxClear      uint8 = 0x20
	CmdGfxPlot       uint8 = 0x21
	CmdGfxLine       uint8 = 0x22
	CmdGfxRectFill   uint8 = 0x23
	CmdGfxRectStroke uint8 = 0x24
	CmdGfxCircle     uint8 = 0x25
	CmdGfxFillCircle uint8 = 0x26
)

// Controller is a memory-mapped command register that drives a
// color/char plane pair. Writes trigger the operation immediately;
// reads return the most recent command (useful for the CPU to
// confirm). Sits on the bus as a 1-byte component.
//
// Pause/Resume/Frame let the CPU hide partial draws: while paused,
// the UI reads a snapshot taken at Pause or Frame time, so writes
// in between are not visible until Frame is issued.
type Controller struct {
	name      string
	base      uint16
	color     *Display
	chars     *Display
	lastCmd   uint8
	lastCmdAt time.Time
	frameAt   time.Time // when RegFrame was last written
	paused    bool
	snapColor []uint8
	snapChar  []uint8
	snapGfx   []uint8 // graphics-plane snapshot, populated only when Graphics != nil

	// Rect parameters consumed by CmdRect* commands. Updated on
	// writes to RegRectX/Y/W/H; persist across commands so a CPU
	// targeting the same rect repeatedly only re-sets the cmd byte.
	rectX, rectY, rectW, rectH uint8

	// Graphics — optional; nil means graphics commands are no-ops.
	// Wired via NewControllerWithGraphics or direct assignment.
	Graphics *GraphicsPlane
	gfxColor uint8 // current draw color for CmdGfx* commands
	mode     uint8 // ModeChar or ModeGraphics; readable by renderers
}

// NewController binds the controller to a display pair. The two
// displays must have matching Width/Height. Graphics mode is not
// enabled — see NewControllerWithGraphics.
func NewController(name string, base uint16, color, chars *Display) *Controller {
	return &Controller{name: name, base: base, color: color, chars: chars}
}

// NewControllerWithGraphics binds the controller to a display pair
// AND a graphics plane. CmdGfx* commands (Clear, Plot, Line, Rect*,
// Circle*) become operative; the GfxColor register selects the
// current draw color.
func NewControllerWithGraphics(name string, base uint16, color, chars *Display, gfx *GraphicsPlane) *Controller {
	return &Controller{name: name, base: base, color: color, chars: chars, Graphics: gfx}
}

func (c *Controller) Name() string { return c.name }
func (c *Controller) Base() uint16 { return c.base }
func (c *Controller) Size() int    { return 9 }

// Reset returns the controller to the post-power-on state: live
// (not paused), char mode, no snapshot, rect params zero, draw
// color zero, last-cmd state cleared. Pixel buffers (color / char
// / graphics) are NOT cleared here — callers reset those at their
// own granularity (e.g. host's machineReset clears RAM, repaints
// the display init pattern, then calls Reset on us).
func (c *Controller) Reset() {
	c.lastCmd = 0
	c.lastCmdAt = time.Time{}
	c.frameAt = time.Time{}
	c.paused = false
	c.snapColor = nil
	c.snapChar = nil
	c.snapGfx = nil
	c.rectX, c.rectY, c.rectW, c.rectH = 0, 0, 0, 0
	c.gfxColor = 0
	c.mode = ModeChar
}

// Symbols implements bus.Labeller — returns the controller's
// register layout so memory views can show labels for the VIC
// register block.
func (c *Controller) Symbols() []asm.Symbol {
	b := c.base
	return []asm.Symbol{
		{Name: "VIC_CMD", Addr: b + RegCmd, Size: 1, Note: "command register (write to fire op)"},
		{Name: "VIC_PAUSE", Addr: b + RegPause, Size: 1, Note: "0=live, 1=show snapshot"},
		{Name: "VIC_FRAME", Addr: b + RegFrame, Size: 1, Note: "any write captures snapshot"},
		{Name: "VIC_RECT_X", Addr: b + RegRectX, Size: 1, Note: "rect/draw X"},
		{Name: "VIC_RECT_Y", Addr: b + RegRectY, Size: 1, Note: "rect/draw Y"},
		{Name: "VIC_RECT_W", Addr: b + RegRectW, Size: 1, Note: "rect/draw width or radius"},
		{Name: "VIC_RECT_H", Addr: b + RegRectH, Size: 1, Note: "rect/draw height"},
		{Name: "VIC_GFX_COLOR", Addr: b + RegGfxColor, Size: 1, Note: "current draw color (palette idx)"},
		{Name: "VIC_MODE", Addr: b + RegMode, Size: 1, Note: "0=char, 1=graphics"},
	}
}

// Mode returns the current display mode (ModeChar or ModeGraphics).
// Renderers use this to decide whether to draw the character grid or
// the graphics plane.
func (c *Controller) Mode() uint8 { return c.mode }

func (c *Controller) Read(offset uint16) uint8 {
	switch offset {
	case RegCmd:
		return c.lastCmd
	case RegPause:
		if c.paused {
			return 1
		}
		return 0
	case RegFrame:
		return 0
	case RegRectX:
		return c.rectX
	case RegRectY:
		return c.rectY
	case RegRectW:
		return c.rectW
	case RegRectH:
		return c.rectH
	case RegGfxColor:
		return c.gfxColor
	case RegMode:
		return c.mode
	}
	return 0
}

func (c *Controller) Write(offset uint16, v uint8) {
	switch offset {
	case RegCmd:
		c.lastCmd = v
		c.lastCmdAt = time.Now()
		c.execute(v)
	case RegPause:
		wasPaused := c.paused
		c.paused = v != 0
		if c.paused && !wasPaused {
			c.captureSnapshot()
		}
	case RegFrame:
		c.frameAt = time.Now()
		if c.paused {
			c.captureSnapshot()
		}
	case RegRectX:
		c.rectX = v
	case RegRectY:
		c.rectY = v
	case RegRectW:
		c.rectW = v
	case RegRectH:
		c.rectH = v
	case RegGfxColor:
		c.gfxColor = v
	case RegMode:
		c.mode = v
	}
}

func (c *Controller) execute(cmd uint8) {
	w := c.color.Width()
	h := c.color.Height()
	cb := c.color.Bytes()
	chb := c.chars.Bytes()
	switch cmd {
	case CmdClear:
		for i := range cb {
			cb[i] = 0x00
		}
		for i := range chb {
			chb[i] = 0x20
		}
	case CmdShiftUp:
		copy(cb, cb[w:])
		copy(chb, chb[w:])
		blankRow(cb[(h-1)*w:h*w], 0x00)
		blankRow(chb[(h-1)*w:h*w], 0x20)
	case CmdShiftDown:
		copy(cb[w:], cb[:(h-1)*w])
		copy(chb[w:], chb[:(h-1)*w])
		blankRow(cb[:w], 0x00)
		blankRow(chb[:w], 0x20)
	case CmdShiftLeft:
		for y := 0; y < h; y++ {
			row := cb[y*w : (y+1)*w]
			copy(row, row[1:])
			row[w-1] = 0x00
			crow := chb[y*w : (y+1)*w]
			copy(crow, crow[1:])
			crow[w-1] = 0x20
		}
	case CmdShiftRight:
		for y := 0; y < h; y++ {
			row := cb[y*w : (y+1)*w]
			copy(row[1:], row[:w-1])
			row[0] = 0x00
			crow := chb[y*w : (y+1)*w]
			copy(crow[1:], crow[:w-1])
			crow[0] = 0x20
		}
	case CmdInvert:
		// Photographic negative: each nibble becomes its 15's
		// complement. Both bg and fg flip, so cells with bg-only
		// content (chars = space) still produce a visible change.
		// (Nibble-swap fg↔bg would zero out spaces' visible color.)
		for i := range cb {
			cb[i] = ^cb[i]
		}
	case CmdRotLeft:
		// Per-row left rotation — leftmost cell wraps to rightmost.
		for y := 0; y < h; y++ {
			row := cb[y*w : (y+1)*w]
			first := row[0]
			copy(row, row[1:])
			row[w-1] = first
			crow := chb[y*w : (y+1)*w]
			firstCh := crow[0]
			copy(crow, crow[1:])
			crow[w-1] = firstCh
		}
	case CmdRotRight:
		// Per-row right rotation — rightmost cell wraps to leftmost.
		for y := 0; y < h; y++ {
			row := cb[y*w : (y+1)*w]
			last := row[w-1]
			copy(row[1:], row[:w-1])
			row[0] = last
			crow := chb[y*w : (y+1)*w]
			lastCh := crow[w-1]
			copy(crow[1:], crow[:w-1])
			crow[0] = lastCh
		}
	case CmdRotUp:
		// Top row wraps to bottom; everything else shifts up by 1.
		saveC := make([]uint8, w)
		saveH := make([]uint8, w)
		copy(saveC, cb[:w])
		copy(saveH, chb[:w])
		copy(cb, cb[w:])
		copy(chb, chb[w:])
		copy(cb[(h-1)*w:], saveC)
		copy(chb[(h-1)*w:], saveH)
	case CmdRectShiftUp, CmdRectRotUp:
		c.rectVShift(w, h, cb, chb, true, cmd == CmdRectRotUp)
	case CmdRectShiftDown, CmdRectRotDown:
		c.rectVShift(w, h, cb, chb, false, cmd == CmdRectRotDown)
	case CmdRectShiftLeft, CmdRectRotLeft:
		c.rectHShift(w, h, cb, chb, true, cmd == CmdRectRotLeft)
	case CmdRectShiftRight, CmdRectRotRight:
		c.rectHShift(w, h, cb, chb, false, cmd == CmdRectRotRight)
	case CmdRotDown:
		// Bottom row wraps to top; everything else shifts down by 1.
		saveC := make([]uint8, w)
		saveH := make([]uint8, w)
		copy(saveC, cb[(h-1)*w:])
		copy(saveH, chb[(h-1)*w:])
		copy(cb[w:], cb[:(h-1)*w])
		copy(chb[w:], chb[:(h-1)*w])
		copy(cb[:w], saveC)
		copy(chb[:w], saveH)

	// Graphics-mode commands. Skipped silently when no graphics plane
	// is attached so terminal builds without a graphics window can
	// still receive the same demo bytecode without crashing.
	case CmdGfxClear:
		if c.Graphics != nil {
			c.Graphics.Clear(c.gfxColor)
		}
	case CmdGfxPlot:
		if c.Graphics != nil {
			c.Graphics.SetPixel(int(c.rectX), int(c.rectY), c.gfxColor)
		}
	case CmdGfxLine:
		if c.Graphics != nil {
			c.Graphics.Line(int(c.rectX), int(c.rectY), int(c.rectW), int(c.rectH), c.gfxColor)
		}
	case CmdGfxRectFill:
		if c.Graphics != nil {
			c.Graphics.FillRect(int(c.rectX), int(c.rectY), int(c.rectW), int(c.rectH), c.gfxColor)
		}
	case CmdGfxRectStroke:
		if c.Graphics != nil {
			c.Graphics.StrokeRect(int(c.rectX), int(c.rectY), int(c.rectW), int(c.rectH), c.gfxColor)
		}
	case CmdGfxCircle:
		if c.Graphics != nil {
			c.Graphics.Circle(int(c.rectX), int(c.rectY), int(c.rectW), c.gfxColor)
		}
	case CmdGfxFillCircle:
		if c.Graphics != nil {
			c.Graphics.FillCircle(int(c.rectX), int(c.rectY), int(c.rectW), c.gfxColor)
		}
	}
}

func (c *Controller) captureSnapshot() {
	cb := c.color.Bytes()
	chb := c.chars.Bytes()
	if len(c.snapColor) != len(cb) {
		c.snapColor = make([]uint8, len(cb))
		c.snapChar = make([]uint8, len(chb))
	}
	copy(c.snapColor, cb)
	copy(c.snapChar, chb)
	if c.Graphics != nil {
		gb := c.Graphics.Bytes()
		if len(c.snapGfx) != len(gb) {
			c.snapGfx = make([]uint8, len(gb))
		}
		copy(c.snapGfx, gb)
	}
}

// IsPaused reports whether display reads return snapshot bytes.
func (c *Controller) IsPaused() bool { return c.paused }

// LastCmd returns the most recently written command byte at +0.
func (c *Controller) LastCmd() uint8 { return c.lastCmd }

// LastCmdAt returns the wall-clock time of the most recent command
// write at RegCmd. Used by the UI to flash button indicators when
// the CPU is firing commands (not just on direct UI clicks).
func (c *Controller) LastCmdAt() time.Time { return c.lastCmdAt }

// LastFrameAt returns the wall-clock time of the most recent write
// to RegFrame.
func (c *Controller) LastFrameAt() time.Time { return c.frameAt }

// ReadColor returns the color byte at the linear offset, honoring
// pause state. Used by the display window's renderer.
func (c *Controller) ReadColor(off int) uint8 {
	if c.paused {
		return c.snapColor[off]
	}
	return c.color.Bytes()[off]
}

// ReadGfxPixel returns the palette index of the graphics-plane pixel
// at (x, y), honoring pause state — when paused, the snapshot bytes
// are decoded; otherwise the live plane is sampled. Returns 0 if no
// graphics plane is attached or coords are out of bounds.
func (c *Controller) ReadGfxPixel(x, y int) uint8 {
	if c.Graphics == nil {
		return 0
	}
	g := c.Graphics
	if x < 0 || y < 0 || x >= g.Width() || y >= g.Height() {
		return 0
	}
	if !c.paused || len(c.snapGfx) == 0 {
		return g.GetPixel(x, y)
	}
	bitOffset := x * g.BPP()
	off := y*g.Stride() + bitOffset/8
	if off >= len(c.snapGfx) {
		return 0
	}
	bitWithin := bitOffset % 8
	shift := 8 - g.BPP() - bitWithin
	mask := byte((1 << g.BPP()) - 1)
	return (c.snapGfx[off] >> shift) & mask
}

// ReadChar returns the char byte at the linear offset, honoring
// pause state.
func (c *Controller) ReadChar(off int) uint8 {
	if c.paused {
		return c.snapChar[off]
	}
	return c.chars.Bytes()[off]
}

// clampedRect returns the rect (RegRectX/Y/W/H) clipped to the
// display dimensions. Returns rw=0 or rh=0 when the rect is empty
// or fully outside the display, signalling the caller to no-op.
func (c *Controller) clampedRect(w, h int) (x, y, rw, rh int) {
	x = int(c.rectX)
	y = int(c.rectY)
	rw = int(c.rectW)
	rh = int(c.rectH)
	if x >= w || y >= h {
		return x, y, 0, 0
	}
	if x+rw > w {
		rw = w - x
	}
	if y+rh > h {
		rh = h - y
	}
	if rw < 0 {
		rw = 0
	}
	if rh < 0 {
		rh = 0
	}
	return
}

// rectVShift implements the four CmdRect{Shift,Rot}{Up,Down}
// variants. up=true shifts content toward lower row indices; rot=true
// preserves the freed row by writing the dropped row back to the
// opposite edge instead of blanking it.
func (c *Controller) rectVShift(w, h int, cb, chb []uint8, up, rot bool) {
	x, y, rw, rh := c.clampedRect(w, h)
	if rw == 0 || rh == 0 {
		return
	}
	rowSlice := func(plane []uint8, r int) []uint8 {
		off := (y+r)*w + x
		return plane[off : off+rw]
	}
	var saveC, saveH []uint8
	if rot {
		saveC = make([]uint8, rw)
		saveH = make([]uint8, rw)
		if up {
			copy(saveC, rowSlice(cb, 0))
			copy(saveH, rowSlice(chb, 0))
		} else {
			copy(saveC, rowSlice(cb, rh-1))
			copy(saveH, rowSlice(chb, rh-1))
		}
	}
	if up {
		// shift rows 1..rh-1 → rows 0..rh-2
		for r := 0; r < rh-1; r++ {
			copy(rowSlice(cb, r), rowSlice(cb, r+1))
			copy(rowSlice(chb, r), rowSlice(chb, r+1))
		}
		// fill last row
		if rot {
			copy(rowSlice(cb, rh-1), saveC)
			copy(rowSlice(chb, rh-1), saveH)
		} else {
			blankRow(rowSlice(cb, rh-1), 0x00)
			blankRow(rowSlice(chb, rh-1), 0x20)
		}
	} else {
		// shift rows 0..rh-2 → rows 1..rh-1 (back-to-front)
		for r := rh - 1; r > 0; r-- {
			copy(rowSlice(cb, r), rowSlice(cb, r-1))
			copy(rowSlice(chb, r), rowSlice(chb, r-1))
		}
		if rot {
			copy(rowSlice(cb, 0), saveC)
			copy(rowSlice(chb, 0), saveH)
		} else {
			blankRow(rowSlice(cb, 0), 0x00)
			blankRow(rowSlice(chb, 0), 0x20)
		}
	}
}

// rectHShift implements the four CmdRect{Shift,Rot}{Left,Right}
// variants. left=true shifts cells toward lower column indices.
func (c *Controller) rectHShift(w, h int, cb, chb []uint8, left, rot bool) {
	x, y, rw, rh := c.clampedRect(w, h)
	if rw == 0 || rh == 0 {
		return
	}
	for r := 0; r < rh; r++ {
		off := (y+r)*w + x
		row := cb[off : off+rw]
		crow := chb[off : off+rw]
		if left {
			firstC, firstCh := row[0], crow[0]
			copy(row, row[1:])
			copy(crow, crow[1:])
			if rot {
				row[rw-1] = firstC
				crow[rw-1] = firstCh
			} else {
				row[rw-1] = 0x00
				crow[rw-1] = 0x20
			}
		} else {
			lastC, lastCh := row[rw-1], crow[rw-1]
			copy(row[1:], row[:rw-1])
			copy(crow[1:], crow[:rw-1])
			if rot {
				row[0] = lastC
				crow[0] = lastCh
			} else {
				row[0] = 0x00
				crow[0] = 0x20
			}
		}
	}
}

func blankRow(row []uint8, fill uint8) {
	for i := range row {
		row[i] = fill
	}
}
