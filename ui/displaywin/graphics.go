package displaywin

import (
	"github.com/gdamore/tcell/v2"

	foxpro "github.com/carledwards/foxpro-go"
	"github.com/carledwards/go6sim/components/display"
)

// pixelPlaceholderRune is the Unicode codepoint stamped into every
// framebuffer cell when the controller is in graphics mode. The wasm
// bridge passes the cell snapshot through unchanged; the JS-side
// renderer detects this codepoint and substitutes pixel content
// from the graphics plane in those cells. Any cell whose content has
// been overwritten by another window (or that window's drop shadow)
// no longer carries the sentinel, and renders as a normal cell —
// which is how z-order is honored for the pixel layer.
//
// Chosen from the BMP Private Use Area so it never collides with
// content the CPU might legitimately put on the screen.
const pixelPlaceholderRune = ''

// classicCGAPalette is the 16-color CGA palette used by the renderer
// to convert palette-indexed graphics-plane pixels into RGB. Matches
// the colors foxpro-go exposes on its own Palette so a graphics
// window blends visually with the rest of the FoxPro chrome.
var classicCGAPalette = [16][3]uint8{
	{0, 0, 0},       // 0  Black
	{0, 0, 170},     // 1  Blue
	{0, 170, 0},     // 2  Green
	{0, 170, 170},   // 3  Cyan
	{170, 0, 0},     // 4  Red
	{170, 0, 170},   // 5  Magenta
	{170, 85, 0},    // 6  Brown
	{170, 170, 170}, // 7  LightGray
	{85, 85, 85},    // 8  DarkGray
	{85, 85, 255},   // 9  LightBlue
	{85, 255, 85},   // 10 LightGreen
	{85, 255, 255},  // 11 LightCyan
	{255, 85, 85},   // 12 LightRed
	{255, 85, 255},  // 13 LightMagenta
	{255, 255, 85},  // 14 Yellow
	{255, 255, 255}, // 15 White
}

// drawGraphicsBlockArt renders a graphics plane onto the canvas using
// upper-half-block (▀) characters: each cell shows two stacked pixels
// via fg (top) + bg (bottom). Coords (x0, y0, w, h) are relative to
// the canvas's content region — the foxpro Canvas will translate them
// to screen positions and clip as needed.
//
// Used only by terminal hosts; in browser the pixel-layer overlay
// paints over this output.
func drawGraphicsBlockArt(c *foxpro.Canvas, g *display.GraphicsPlane, x0, y0, w, h int) {
	if g == nil {
		return
	}
	pw := g.Width()
	ph := g.Height()
	cellsX := w
	if pw < cellsX {
		cellsX = pw
	}
	cellsY := h
	if (ph+1)/2 < cellsY {
		cellsY = (ph + 1) / 2
	}
	for cy := 0; cy < cellsY; cy++ {
		py0 := cy * 2
		for cx := 0; cx < cellsX; cx++ {
			top := classicCGAPalette[g.GetPixel(cx, py0)&0x0F]
			var bot [3]uint8
			if py0+1 < ph {
				bot = classicCGAPalette[g.GetPixel(cx, py0+1)&0x0F]
			}
			style := tcell.StyleDefault.
				Foreground(tcell.NewRGBColor(int32(top[0]), int32(top[1]), int32(top[2]))).
				Background(tcell.NewRGBColor(int32(bot[0]), int32(bot[1]), int32(bot[2])))
			c.Set(x0+cx, y0+cy, '▀', style)
		}
	}
}

// graphicsActive reports whether the provider should render in
// graphics mode this frame: requires a Graphics plane attached AND
// the controller reporting RegMode == ModeGraphics.
func (p *Provider) graphicsActive() bool {
	if p.Graphics == nil {
		return false
	}
	if p.Controller == nil {
		return false
	}
	return p.Controller.Mode() == display.ModeGraphics
}

// ─── foxpro.PixelContent + PixelRectContent ─────────────────────
//
// These let the wasm bridge overlay a real <canvas> on the
// framebuffer area in graphics mode. PixelSize returns (0, 0) in
// char mode so the bridge skips this provider entirely; switching
// modes at runtime is a single register write away on the CPU side.

// stripModeTag removes a trailing " [TEXT]" / " [GFX]" tag from a
// title so we don't accumulate "[TEXT][TEXT]…" on repeated Draws.
func stripModeTag(t string) string {
	for _, suffix := range []string{"  [TEXT]", "  [GFX]"} {
		if len(t) >= len(suffix) && t[len(t)-len(suffix):] == suffix {
			return t[:len(t)-len(suffix)]
		}
	}
	return t
}

// PixelLayerID — stable identifier for the wasm canvas overlay.
func (p *Provider) PixelLayerID() string { return "vic-display" }

// PixelSize returns the graphics plane's pixel dimensions, or (0, 0)
// when the provider is in char mode (or has no graphics plane).
func (p *Provider) PixelSize() (int, int) {
	if !p.graphicsActive() {
		return 0, 0
	}
	return p.Graphics.Width(), p.Graphics.Height()
}

// DrawPixels fills buf with RGBA bytes (4 per pixel, row-major).
// Reads through Controller.ReadGfxPixel so pause/frame state is
// honored — when the CPU has paused the VIC and committed a frame,
// the user sees the captured snapshot, not in-progress pixels. Same
// double-buffer story as the existing color/char planes.
func (p *Provider) DrawPixels(buf []byte) {
	if !p.graphicsActive() {
		return
	}
	g := p.Graphics
	pw := g.Width()
	ph := g.Height()
	for y := 0; y < ph; y++ {
		for x := 0; x < pw; x++ {
			rgb := classicCGAPalette[p.Controller.ReadGfxPixel(x, y)&0x0F]
			i := (y*pw + x) * 4
			buf[i+0] = rgb[0]
			buf[i+1] = rgb[1]
			buf[i+2] = rgb[2]
			buf[i+3] = 0xFF
		}
	}
}

// PixelRect tells the bridge that the pixel layer should overlay only
// the framebuffer area within the window's body — leaving the right-
// column buttons and bottom hex strip as native cell content.
//
// The framebuffer cells nominally start at (1, 1) inside the inner
// rect (one row of border). When the window is scrolled (arrow keys
// / PgUp / PgDn), the foxpro Canvas shifts every write by the scroll
// offset, so the actual screen position of the framebuffer cells is
// (1 - scrollX, 1 - scrollY) relative to inner. We pass that through
// so the bridge — and the JS substitution loop — find sentinels at
// their real screen positions after scrolling.
func (p *Provider) PixelRect() (x, y, w, h int) {
	cpp := p.cellsPerPixel()
	sx, sy := p.ScrollOffset()
	return 1 - sx, 1 - sy, p.Width * cpp, p.Height
}
