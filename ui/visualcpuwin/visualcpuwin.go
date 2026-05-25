// Package visualcpuwin renders a Visual6502-style live transistor view
// of the active CPU. Phase 1 is plumbing only: the window opens /
// raises / closes via a host-supplied lifecycle, a backend gate paints
// a "switch to NetSim" hint when the simulator is on the interpretive
// CPU (which has no transistor-level state to draw), and the canvas
// surface is sized for the eventual die rendering.
//
// Phase 2 will port visual6502's segdefs polygon data and wire each
// polygon's fill colour to a live netsim node state, producing the
// classic "lit transistor" view as the CPU executes.
//
// The design keeps the provider deliberately host-agnostic — the
// window doesn't know whether its pixel buffer is being composited
// onto a wasm canvas or streamed to a remote viewer over a WebSocket
// from the terminal build. Both paths just consume PixelSize/
// DrawPixels via the standard foxpro PixelContent interface.
package visualcpuwin

import (
	"github.com/gdamore/tcell/v2"

	foxpro "github.com/carledwards/foxpro-go"
	"github.com/carledwards/go6sim/cpu"
)

// PixelLayerID is the stable identifier the wasm bridge uses to match
// this provider with its host-side canvas overlay. Distinct from
// every other PixelContent in the app (scope, display, …).
const PixelLayerID = "visual-cpu"

// pixelPlaceholderRune (U+E000, Private Use Area) is the sentinel
// rune the wasm bridge looks for when compositing pixel content
// over cell-rendered windows. Numeric escape so the source survives
// clipboard round-trips that strip PUA codepoints.
const pixelPlaceholderRune = ''

// Natural minimums — chrome borders plus a small interior so a
// shrunk window still reads as a chip-view window.
const (
	MinW = 30
	MinH = 12
)

// Segment is one filled polygon belonging to a single bus node, on
// one silicon layer. Ports the visual6502 segdefs table — one entry
// per drawable polygon (a node can own several across multiple
// layers). Coords are vertex pairs (x,y,x,y,...) in the original
// chip-space (un-normalised; the renderer scales to fit the canvas).
//
// PullUp distinguishes pullup-side polygons from pulldown-side. Layer
// indexes the silicon layer the polygon belongs to (diffusion / poly
// / metal / vss / vcc etc.). NodeID indexes into the netsim node
// array — Phase 2b uses it to look up the live high/low state.
type Segment struct {
	NodeID uint16
	PullUp bool
	Layer  uint8
	Coords []int32
}

// Canvas pixel-size targets. The pixel layer scales with the cell
// grid (each cell becomes pxPerCell pixels horizontally and
// pxPerRow vertically), so these constants set the per-cell
// subdivision the wasm bridge uses.
//
// pxPerRow > pxPerCell on purpose: the wasm renderer's display
// cells are taller than wide (typical foxpro font: ~9 px wide ×
// 22 px tall, ratio 2.44), and the bridge stretches each chip-pixel
// from (pxPerCell × pxPerRow) up to (cellW × cellH) on the screen.
// If we use a square chip-pixel here, the stretch gives a 2.4×
// vertical squash and the chip silhouette looks tall-and-narrow.
// Making pxPerRow ≈ pxPerCell × (cellH/cellW) cancels the stretch
// so the chip's true aspect comes through. The TUI/WS Phase 3
// path will need its own pair of constants for its (different)
// cell aspect.
const (
	pxPerCell = 8
	pxPerRow  = 20
)

// Provider renders the chip-die canvas. Phase 1 just paints a flat
// background with a "switch to NetSim" hint when the CPU backend
// isn't transistor-level. Phase 2 will layer polygon fills over the
// background using node-state reads from a netsim Backend.
type Provider struct {
	foxpro.ScrollState

	// Backend is the live CPU backend. The provider doesn't read it
	// directly in Phase 1 — IsTransistorBackend is the gate — but
	// holding the reference makes the Phase 2 polygon-rendering
	// path's node-state queries a one-line addition.
	Backend cpu.Backend

	// IsTransistorBackend, when non-nil, returns true if the active
	// backend exposes transistor-level node state (i.e. netsim).
	// When false, DrawPixels paints the "switch CPU" message
	// instead of attempting a chip render. The host sets this once
	// at construction and updates it implicitly via the Backend
	// swap path it already runs on CPU-picker changes.
	IsTransistorBackend func() bool

	// lastCanvasCols / lastCanvasRows track the most recently
	// painted region. PixelRect returns these so the wasm bridge's
	// pixel-substitution scan stays inside this window — same
	// defensive pattern scopewin uses.
	lastCanvasCols int
	lastCanvasRows int

	// Static chip background cache. The 8233-polygon scan-fill takes
	// a noticeable chunk of frame time; the silhouette doesn't change
	// between frames, only the per-node highlight does. So we render
	// the base layer once per (pxW, pxH, overlayOn) combination and
	// per-frame just copy(buf, staticBg) before overlaying the lit
	// polygons. Cache invalidates on resize OR overlay toggle (the
	// overlay state changes the projection's margin so the chip
	// pixels are at different positions).
	staticBg                   []byte
	staticPxW, staticPxH       int
	staticOverlay              bool

	// nodeStates is reused across frames so we don't realloc the
	// per-node snapshot every Draw. Sized lazily when we first know
	// the backend's node count.
	nodeStates []bool

	// OverlayOn toggles a labelled annotation pass on top of the
	// chip: muted background, leader lines, and text callouts naming
	// the major functional blocks and pin pads. Click anywhere on
	// the canvas to flip; matches visual6502's "the chip explained"
	// annotated still.
	OverlayOn bool
}

// nodeStateProvider is satisfied by netsim's Adapter (Phase 2b live
// view). interp doesn't satisfy it — visualcpuwin falls back to the
// unlit static base in that case. Defined here so we don't have to
// import netsim from visualcpuwin (which would create a wider
// dependency than this package needs).
type nodeStateProvider interface {
	NodeCount() int
	NodeStates(out []bool)
}

// New returns a Provider with default state. Backend + the gate
// callback are set by the host after construction.
func New() *Provider {
	return &Provider{}
}

// Draw stamps pixel sentinels across the canvas region so the wasm
// bridge knows which cells to overlay with the pixel buffer. The
// actual image work is in DrawPixels.
func (p *Provider) Draw(screen tcell.Screen, inner foxpro.Rect, theme foxpro.Theme, focused bool) {
	bg := theme.WindowBG
	c := foxpro.NewCanvas(screen, inner, &p.ScrollState)

	// Stamp the whole inner region with the pixel sentinel. The
	// wasm bridge composites DrawPixels output over these cells.
	// Terminal hosts (no pixel bridge) will see the sentinel render
	// as a placeholder glyph; a TUI fallback path is Phase 3 work.
	p.lastCanvasCols = inner.W
	p.lastCanvasRows = inner.H
	for y := 0; y < inner.H; y++ {
		for x := 0; x < inner.W; x++ {
			c.Set(x, y, pixelPlaceholderRune, bg)
		}
	}
}

// HandleKey — no keyboard controls.
func (p *Provider) HandleKey(ev *tcell.EventKey) bool { return false }

// HandleMouse: primary-button click anywhere inside the canvas
// toggles the labelled overlay.
func (p *Provider) HandleMouse(ev *tcell.EventMouse, inner foxpro.Rect) bool {
	if ev.Buttons()&tcell.Button1 == 0 {
		return false
	}
	mx, my := ev.Position()
	if !inner.Contains(mx, my) {
		return false
	}
	p.OverlayOn = !p.OverlayOn
	return true
}

// StatusHint reflects the current state in one line for the host
// status bar.
func (p *Provider) StatusHint() string {
	if p.IsTransistorBackend != nil && !p.IsTransistorBackend() {
		return " Switch CPU to NetSim to see the live chip view "
	}
	return " Visual 6502 — Phase 1: canvas placeholder, polygons coming in Phase 2 "
}

// ─── PixelContent + PixelRectContent ────────────────────────────

// PixelLayerID — stable identifier for the wasm canvas overlay.
func (p *Provider) PixelLayerID() string { return PixelLayerID }

// PixelSize returns the pixel-buffer dimensions sized to the most
// recently painted region. Returns (0, 0) before the first Draw so
// the wasm bridge skips the layer until we have real dimensions.
func (p *Provider) PixelSize() (int, int) {
	if p.lastCanvasCols <= 0 || p.lastCanvasRows <= 0 {
		return 0, 0
	}
	return p.lastCanvasCols * pxPerCell, p.lastCanvasRows * pxPerRow
}

// PixelRect places the canvas across the full window inner area.
// Width is the most recently painted cell count — see scopewin for
// the rationale (prevents the bridge's pixel-substitution scan from
// leaking into adjacent windows).
func (p *Provider) PixelRect() (x, y, w, h int) {
	if p.lastCanvasCols <= 0 || p.lastCanvasRows <= 0 {
		return 0, 0, 0, 0
	}
	sx, sy := p.ScrollOffset()
	return 0 - sx, 0 - sy, p.lastCanvasCols, p.lastCanvasRows
}

// DrawPixels fills buf with the pixel data the wasm bridge will
// composite onto the canvas. Phase 1 paints either:
//
//   - the "switch CPU" hint when the active backend isn't netsim
//   - a flat dark navy placeholder when netsim is active (the actual
//     transistor visualization is Phase 2)
//
// buf is sized at PixelSize()'s most recently returned dims — the
// bridge always passes the matching length, so we trust the caller
// rather than re-derive width / height here.
func (p *Provider) DrawPixels(buf []byte) {
	if len(buf) == 0 {
		return
	}
	pxW := p.lastCanvasCols * pxPerCell
	pxH := p.lastCanvasRows * pxPerRow

	// Background fill — pure black, matching visual6502's chipbg
	// fillStyle. The translucent layer-0 metal blends with this
	// black to a dim slate (~rgba 51,51,77 final), which is what
	// gives the chip its dark "substrate" look beneath the
	// wires. A dim navy would mute every other layer's colour.
	bgR, bgG, bgB := byte(0x00), byte(0x00), byte(0x00)
	for i := 0; i < pxW*pxH; i++ {
		buf[4*i+0] = bgR
		buf[4*i+1] = bgG
		buf[4*i+2] = bgB
		buf[4*i+3] = 0xFF
	}

	wrongBackend := p.IsTransistorBackend != nil && !p.IsTransistorBackend()
	if wrongBackend {
		paintCenteredText(buf, pxW, pxH,
			[]string{
				"Visual 6502 needs the transistor-level CPU.",
				"",
				"Open Hardware -> CPU... and pick Transistor (netsim).",
			},
			// White text on a Blue panel — matches the scope's
			// "too sparse" hint and the address-bus pen.
			byte(0xff), byte(0xff), byte(0xff), // fg
			byte(0x00), byte(0x00), byte(0xaa), // bg
		)
		return
	}

	// Netsim active. Two-pass render: copy the cached static base
	// (per-layer colours), then overlay only the polygons whose
	// node is currently HIGH with a brighter highlight colour.
	// Rebuild the cache on first frame and on resize.
	if p.staticBg == nil || p.staticPxW != pxW || p.staticPxH != pxH || p.staticOverlay != p.OverlayOn {
		p.staticBg = make([]byte, pxW*pxH*4)
		for i := 0; i < pxW*pxH; i++ {
			p.staticBg[4*i+0] = bgR
			p.staticBg[4*i+1] = bgG
			p.staticBg[4*i+2] = bgB
			p.staticBg[4*i+3] = 0xFF
		}
		renderChip(p.staticBg, pxW, pxH, p.OverlayOn)
		p.staticPxW = pxW
		p.staticPxH = pxH
		p.staticOverlay = p.OverlayOn
	}
	copy(buf, p.staticBg)

	// Live overlay — only if the backend exposes per-node state
	// (netsim Adapter satisfies nodeStateProvider; interp doesn't).
	nsp, ok := p.Backend.(nodeStateProvider)
	if !ok {
		return
	}
	n := nsp.NodeCount()
	if len(p.nodeStates) < n {
		p.nodeStates = make([]bool, n)
	}
	nsp.NodeStates(p.nodeStates)
	highlightChip(buf, pxW, pxH, p.OverlayOn, p.nodeStates)

	if p.OverlayOn {
		drawOverlay(buf, pxW, pxH)
	}
}

// paintCenteredText draws a multi-line message centered in the
// pixel buffer using a 5x7 bitmap glyph table. Crude on purpose —
// it's a placeholder until Phase 2 renders the actual chip.
//
// Each character takes (glyphW+1) horizontal pixels (1px gap) and
// (glyphH+2) vertical pixels (1px gap top + bottom). The text is
// scaled 2x so it remains readable at typical window sizes.
func paintCenteredText(buf []byte, pxW, pxH int, lines []string, fgR, fgG, fgB, bgR, bgG, bgB byte) {
	const (
		glyphW = 5
		glyphH = 7
		scale  = 2
		gap    = 1
	)
	cellW := (glyphW + gap) * scale
	cellH := (glyphH + gap + 1) * scale
	totalH := cellH * len(lines)
	startY := (pxH - totalH) / 2
	if startY < 0 {
		startY = 0
	}
	for li, line := range lines {
		lineW := len(line) * cellW
		startX := (pxW - lineW) / 2
		if startX < 0 {
			startX = 0
		}
		y0 := startY + li*cellH
		for ci, r := range line {
			drawGlyph(buf, pxW, pxH, startX+ci*cellW, y0,
				r, scale, fgR, fgG, fgB, bgR, bgG, bgB)
		}
	}
}

// drawGlyph paints one 5x7 character at (px, py) scaled by scale.
// Unknown glyphs render as a blank cell. The bitmap font is sparse
// on purpose — covers ASCII letters, digits, and the few punctuation
// marks Phase 1 needs.
func drawGlyph(buf []byte, pxW, pxH, px, py int, r rune, scale int, fgR, fgG, fgB, bgR, bgG, bgB byte) {
	bits, ok := glyph5x7[r]
	if !ok {
		return // blank for missing glyphs
	}
	for row := 0; row < 7; row++ {
		bitsRow := bits[row]
		for col := 0; col < 5; col++ {
			lit := (bitsRow>>(4-col))&1 == 1
			if !lit {
				continue
			}
			for sy := 0; sy < scale; sy++ {
				for sx := 0; sx < scale; sx++ {
					x := px + col*scale + sx
					y := py + row*scale + sy
					if x < 0 || x >= pxW || y < 0 || y >= pxH {
						continue
					}
					i := (y*pxW + x) * 4
					buf[i+0] = fgR
					buf[i+1] = fgG
					buf[i+2] = fgB
					buf[i+3] = 0xFF
				}
			}
		}
	}
	_ = bgR
	_ = bgG
	_ = bgB // bg drawn by the caller's fill; kept on the signature
	// for future use if we add per-glyph clearing.
}
