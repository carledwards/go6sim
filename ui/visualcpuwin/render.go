package visualcpuwin

import "sort"

// Chip-data extents — empirically measured from the segdefs (the
// data doesn't fill the full 0..10000 chip-space). Used by the
// overlay's edge-aligned label placement so labels sit just outside
// the visible chip rather than out at the chip-space box edge
// (which leaves ~10 % whitespace between chip and label).
const (
	chipDataMinX = 214
	chipDataMaxX = 8983
	chipDataMinY = 179
	chipDataMaxY = 9807
)

// Chip-space constants match visual6502 so the polygon table
// projects identically. The 6502 die fits into a 10000×10000
// coordinate space; chipOffsetX nudges it to match the upstream
// canvas placement.
//
// Y IS flipped on projection (chipSize - y): visual6502's segdefs
// use silicon-layout convention where Y grows upward, but we want
// display Y growing downward with the opcode-decode ROM at the top
// (the orientation a 6502 datasheet shows).
const (
	chipSize    = 10000
	chipOffsetX = 400
	chipOffsetY = 0
)

// layerColors / layerAlphas mirror visual6502/expertWires.js's
// `colors` table exactly — RGB + per-layer alpha. The alpha values
// are load-bearing: layers 0 and 6 are translucent in visual6502
// so polygons "see through" each other, which is what produces the
// chip's characteristic mixed-colour look. Without blending, the
// last opaque layer drawn at any given pixel wins, so the segdefs's
// roughly-random layer-interleaving collapses to mostly-one-colour.
//
//	0 — metal (slate, 40% — drawn under everything)
//	1 — vss / ground (yellow, opaque)
//	2 — vcc / power (magenta, opaque)
//	3 — diffusion (green, opaque)
//	4 — poly (red, opaque)
//	5 — gate / overlay (deep purple, opaque)
//	6 — service / pad (purple, 75% — light blend over the rest)
var layerColors = [...][3]byte{
	{0x80, 0x80, 0xc0}, // 0 — metal
	{0xff, 0xff, 0x00}, // 1 — vss
	{0xff, 0x00, 0xff}, // 2 — vcc
	{0x4d, 0xff, 0x4d}, // 3 — diffusion
	{0xff, 0x4d, 0x4d}, // 4 — poly
	{0x80, 0x1a, 0xc0}, // 5 — gate
	{0x80, 0x00, 0xff}, // 6 — service
}

// layerAlphas is the per-layer fill opacity (0.0 = fully transparent,
// 1.0 = fully opaque). Multiplied by 256 in fillSegment so the
// blend math stays in integer arithmetic.
var layerAlphas = [...]uint16{
	102, // 0 — 40% ≈ 0.4 * 256
	256, // 1 — opaque
	256, // 2 — opaque
	256, // 3 — opaque
	256, // 4 — opaque
	256, // 5 — opaque
	192, // 6 — 75% ≈ 0.75 * 256
}

// litColor / litAlpha is the overlay paint for a polygon whose
// node is HIGH. Translucent red-pink at 40% alpha — matches
// visual6502's `overlayNode` fillStyle `rgba(255,0,64,0.4)` so the
// chip's structural colours show through with a red tint rather
// than getting fully replaced by an opaque highlight.
var (
	litColor = [3]byte{0xff, 0x00, 0x40}
	litAlpha = uint16(102) // ≈ 0.4 * 256
)

func layerRGB(layer uint8) (byte, byte, byte) {
	if int(layer) < len(layerColors) {
		c := layerColors[layer]
		return c[0], c[1], c[2]
	}
	return 0x80, 0x80, 0x80
}

// projectFn returns a closure that maps chip-space (cx, cy) to pixel
// coords inside a (pxW × pxH) buffer. The chip is a 10000×10000
// square; we pick the more-constraining axis to fit it into the
// canvas with aspect preserved, centered, with a small margin.
// chipBox is the chip's projected bounding box in display pixels.
// Computed alongside the projection so overlay labels can align to
// an exact edge.
type chipBox struct {
	left, top, right, bottom int
}

func projectFn(pxW, pxH int, withOverlay bool) func(cx, cy int32) (int, int) {
	fn, _ := projectFnWithBounds(pxW, pxH, withOverlay)
	return fn
}

// projectFnWithBounds returns both the projection closure and the
// chip's display-pixel bounding box. Overlay code uses the box to
// place edge-aligned labels at a fixed offset from the chip edge.
func projectFnWithBounds(pxW, pxH int, withOverlay bool) (func(cx, cy int32) (int, int), chipBox) {
	// Margin reserves whitespace around the chip for overlay labels
	// + leader lines to land. Overlay-off fills the canvas; overlay-on
	// shrinks the chip enough to give edge labels a clear band of
	// black to sit in (each side gets margin/2 of breathing room).
	margin := 4
	if withOverlay {
		margin = 40 
	}
	sx := (float64(pxW) - float64(margin)) / float64(chipSize)
	sy := (float64(pxH) - float64(margin)) / float64(chipSize)
	scale := sx
	if sy < sx {
		scale = sy
	}
	// Center the (square) chip within the (possibly non-square)
	// canvas.
	fit := float64(chipSize) * scale
	offsetX := (float64(pxW) - fit) / 2
	offsetY := (float64(pxH) - fit) / 2
	project := func(cx, cy int32) (int, int) {
		px := int(offsetX + (float64(cx)+chipOffsetX)*scale)
		// Y flip: chipSize - cy + offset (matches visual6502).
		py := int(offsetY + (chipSize-float64(cy)+chipOffsetY)*scale)
		return px, py
	}
	box := chipBox{
		left:   int(offsetX),
		top:    int(offsetY),
		right:  int(offsetX + fit),
		bottom: int(offsetY + fit),
	}
	return project, box
}

// renderChip rasterises every segment onto buf using the per-layer
// base palette. Used once per (pxW, pxH) to build the static
// background cache; the per-frame path doesn't call this.
func renderChip(buf []byte, pxW, pxH int, withOverlay bool) {
	project := projectFn(pxW, pxH, withOverlay)
	xs := make([]int, 0, 32)
	pxs := make([]int, 0, 64)
	pys := make([]int, 0, 64)
	for i := range Segments {
		seg := &Segments[i]
		r, g, b := layerRGB(seg.Layer)
		alpha := uint16(256)
		if int(seg.Layer) < len(layerAlphas) {
			alpha = layerAlphas[seg.Layer]
		}
		fillSegment(buf, pxW, pxH, seg, project, r, g, b, alpha, &xs, &pxs, &pys)
		// Layers 0 (metal) and 6 (service) get a translucent white
		// edge outline on top of the fill — same as visual6502's
		// `if((c==0)||(c==6)) ctx.stroke()`. Gives the metal layer
		// its visible grid structure; without it, large metal
		// regions read as uniform slate.
		if seg.Layer == 0 || seg.Layer == 6 {
			strokeSegment(buf, pxW, pxH, seg, project, 0xff, 0xff, 0xff, 128)
		}
	}
}

// strokeSegment draws translucent line outlines along the polygon's
// edges. DDA line walker — for each edge, step pixel-by-pixel and
// alpha-blend onto buf. Cheap because layer-0/6 polygons are small
// and there are only ~850 of them; this only runs at static-cache
// build time, never per-frame.
func strokeSegment(buf []byte, pxW, pxH int, seg *Segment,
	project func(cx, cy int32) (int, int),
	r, g, b byte, alpha uint16,
) {
	if len(seg.Coords) < 4 {
		return
	}
	n := len(seg.Coords) / 2
	inv := uint16(256) - alpha
	pset := func(x, y int) {
		if x < 0 || x >= pxW || y < 0 || y >= pxH {
			return
		}
		o := (y*pxW + x) * 4
		if o+3 >= len(buf) {
			return
		}
		buf[o+0] = byte((uint16(r)*alpha + uint16(buf[o+0])*inv) >> 8)
		buf[o+1] = byte((uint16(g)*alpha + uint16(buf[o+1])*inv) >> 8)
		buf[o+2] = byte((uint16(b)*alpha + uint16(buf[o+2])*inv) >> 8)
		buf[o+3] = 0xff
	}
	for i := 0; i < n; i++ {
		j := (i + 1) % n
		x0, y0 := project(seg.Coords[2*i], seg.Coords[2*i+1])
		x1, y1 := project(seg.Coords[2*j], seg.Coords[2*j+1])
		// DDA line — step along the longer axis, interpolate the other.
		dx := x1 - x0
		dy := y1 - y0
		steps := dx
		if steps < 0 {
			steps = -steps
		}
		ady := dy
		if ady < 0 {
			ady = -ady
		}
		if ady > steps {
			steps = ady
		}
		if steps == 0 {
			pset(x0, y0)
			continue
		}
		for s := 0; s <= steps; s++ {
			x := x0 + dx*s/steps
			y := y0 + dy*s/steps
			pset(x, y)
		}
	}
}

// highlightChip overlays litColor on each polygon whose node is
// currently HIGH. Skips polygons whose node ID is out of range
// (defensive — node table is fixed-size, but better not to panic
// on a sized-down states slice). Called every frame after the
// static background is blitted in.
//
// Cost: ~50-200 polygons per frame in practice (only the lit ones),
// so this is much cheaper than re-rasterising the whole chip.
func highlightChip(buf []byte, pxW, pxH int, withOverlay bool, states []bool) {
	project := projectFn(pxW, pxH, withOverlay)
	r, g, b := litColor[0], litColor[1], litColor[2]
	xs := make([]int, 0, 32)
	pxs := make([]int, 0, 64)
	pys := make([]int, 0, 64)
	for i := range Segments {
		seg := &Segments[i]
		id := int(seg.NodeID)
		if id >= len(states) || !states[id] {
			continue
		}
		// Lit overlay paints translucent (litAlpha ≈ 0.4) — the chip
		// structure shows through with a red tint, same as visual6502.
		fillSegment(buf, pxW, pxH, seg, project, r, g, b, litAlpha, &xs, &pxs, &pys)
	}
}

// fillSegment scan-line-fills one polygon at the supplied colour.
// Scratch slices are passed by pointer so callers can reuse them
// across thousands of polygons without per-call allocation.
//
// Algorithm: for each row y in the polygon's bbox, find every edge
// that straddles y, compute the x-intersection, sort, then fill
// spans pairwise. Half-open edge rule (count lower endpoint only)
// prevents double-counting at vertex pinch points where two edges
// share a y value.
// alpha is in 0..256 (256 = opaque). Values < 256 blend the source
// colour with whatever's already in buf using integer math:
//   out = (src * alpha + dst * (256 - alpha)) >> 8
// One multiply + one shift per channel — fast enough to apply over
// ~8200 polygons at the buffer sizes we render.
func fillSegment(buf []byte, pxW, pxH int, seg *Segment,
	project func(cx, cy int32) (int, int),
	r, g, b byte, alpha uint16, xs, pxs, pys *[]int) {
	if len(seg.Coords) < 6 { // need >= 3 vertices to fill
		return
	}
	*pxs = (*pxs)[:0]
	*pys = (*pys)[:0]
	ymin, ymax := pxH, 0
	for j := 0; j+1 < len(seg.Coords); j += 2 {
		px, py := project(seg.Coords[j], seg.Coords[j+1])
		*pxs = append(*pxs, px)
		*pys = append(*pys, py)
		if py < ymin {
			ymin = py
		}
		if py > ymax {
			ymax = py
		}
	}
	if ymin < 0 {
		ymin = 0
	}
	if ymax >= pxH {
		ymax = pxH - 1
	}
	n := len(*pxs)
	if n < 3 {
		return
	}
	for y := ymin; y <= ymax; y++ {
		*xs = (*xs)[:0]
		for k := 0; k < n; k++ {
			k2 := (k + 1) % n
			y1, y2 := (*pys)[k], (*pys)[k2]
			if (y1 <= y && y2 > y) || (y2 <= y && y1 > y) {
				x1, x2 := (*pxs)[k], (*pxs)[k2]
				xi := x1 + (y-y1)*(x2-x1)/(y2-y1)
				*xs = append(*xs, xi)
			}
		}
		if len(*xs) < 2 {
			continue
		}
		sort.Ints(*xs)
		for k := 0; k+1 < len(*xs); k += 2 {
			x0, x1 := (*xs)[k], (*xs)[k+1]
			if x0 < 0 {
				x0 = 0
			}
			if x1 >= pxW {
				x1 = pxW - 1
			}
			if x0 > x1 {
				continue
			}
			rowOff := y * pxW * 4
			if alpha >= 256 {
				// Opaque fast path — no blend, just write the colour.
				for x := x0; x <= x1; x++ {
					o := rowOff + x*4
					if o+3 >= len(buf) {
						break
					}
					buf[o+0] = r
					buf[o+1] = g
					buf[o+2] = b
					buf[o+3] = 0xff
				}
			} else {
				inv := uint16(256) - alpha
				for x := x0; x <= x1; x++ {
					o := rowOff + x*4
					if o+3 >= len(buf) {
						break
					}
					buf[o+0] = byte((uint16(r)*alpha + uint16(buf[o+0])*inv) >> 8)
					buf[o+1] = byte((uint16(g)*alpha + uint16(buf[o+1])*inv) >> 8)
					buf[o+2] = byte((uint16(b)*alpha + uint16(buf[o+2])*inv) >> 8)
					buf[o+3] = 0xff
				}
			}
		}
	}
}
