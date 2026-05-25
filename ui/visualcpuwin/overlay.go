package visualcpuwin

import "sync"

// drawOverlay paints the click-toggleable annotation pass: muted
// background + leader lines + text callouts naming functional
// blocks and pin pads. Drawn on top of the live chip render — the
// chip itself is not resized; the muting is just a translucent
// black wash so labels read clearly against the busy layout.
//
// Anchors are resolved lazily: pin labels look up their node id in
// the segdefs centroid table (so the leader points exactly at the
// pad polygon), register labels average the centroids of every bit
// node, and a few region labels use explicit chip-space coords.
func drawOverlay(buf []byte, pxW, pxH int) {
	muteOverlay(buf, pxW, pxH)
	resolveOverlayLabelsOnce.Do(resolveOverlayLabels)
	project, box := projectFnWithBounds(pxW, pxH, true)
	for _, lbl := range resolvedLabels {
		paintLabel(buf, pxW, pxH, project, box, lbl)
	}
}

// resolvedLabels is the final, position-resolved list painted each
// frame the overlay is on. Built once on first toggle from
// rawOverlayLabels below — node lookups go through centroidOf{,Nodes}.
var (
	resolveOverlayLabelsOnce sync.Once
	resolvedLabels           []overlayLabel
)

// resolveOverlayLabels populates resolvedLabels by walking
// rawOverlayLabels and computing each entry's chip-space anchor +
// final layout. Labels whose node lookup fails are silently
// dropped (would only happen if nodenames.js changed).
func resolveOverlayLabels() {
	resolvedLabels = make([]overlayLabel, 0, len(rawOverlayLabels))
	for _, r := range rawOverlayLabels {
		switch r.style {
		case styleEdgeAligned:
			var cx, cy int32
			var ok bool
			if r.chipX != 0 || r.chipY != 0 {
				// Explicit anchor — set after shift-click inspection
				// gave us the actual pad location for this pin. Wins
				// over the algorithmic lookup so we can hand-fix any
				// pad that padPolygonOnEdge mispicks.
				cx, cy, ok = r.chipX, r.chipY, true
			} else {
				cx, cy, ok = padPolygonOnEdge(r.node, r.side)
			}
			if !ok {
				continue
			}
			resolvedLabels = append(resolvedLabels, overlayLabel{
				text: r.text, chipX: cx, chipY: cy,
				side: r.side, leadPx: -1, // sentinel: edge-aligned, no leader
			})
		case styleInterior:
			// Resolution priority: explicit coords > hint > single
			// node centroid > averaged centroid. Explicit always
			// wins so a hardcoded chipX/chipY can override even a
			// `nodes`-based label (e.g. register anchored by hand
			// after a shift+click reading).
			var cx, cy int32
			ok := true
			switch {
			case r.chipX != 0 || r.chipY != 0:
				cx, cy = r.chipX, r.chipY
			case r.node > 0 && (r.hintX != 0 || r.hintY != 0):
				cx, cy, ok = padNearHint(r.node, r.hintX, r.hintY)
			case r.node > 0:
				cx, cy, ok = centroidOf(r.node)
			case len(r.nodes) > 0:
				cx, cy = centroidOfNodes(r.nodes)
			default:
				continue
			}
			if !ok {
				continue
			}
			resolvedLabels = append(resolvedLabels, overlayLabel{
				text: r.text, chipX: cx, chipY: cy,
				side: r.side, leadPx: r.leadPx,
			})
		}
	}
}

// muteOverlay alpha-blends 45% black over every pixel. Brings the
// chip's saturated colours down so the white labels and leader
// lines have enough contrast to read.
func muteOverlay(buf []byte, pxW, pxH int) {
	const alpha = 115 // ≈ 0.45 * 256
	inv := uint16(256 - alpha)
	for i := 0; i < pxW*pxH; i++ {
		o := 4 * i
		// src = black (0,0,0). out = dst * inv / 256.
		buf[o+0] = byte((uint16(buf[o+0]) * inv) >> 8)
		buf[o+1] = byte((uint16(buf[o+1]) * inv) >> 8)
		buf[o+2] = byte((uint16(buf[o+2]) * inv) >> 8)
	}
}

// labelSide controls which direction the leader line extends from
// the chip anchor toward the text label.
type labelSide uint8

const (
	sideTop    labelSide = iota // text above anchor, leader goes up
	sideBottom                  // text below anchor, leader goes down
	sideLeft                    // text left of anchor, leader goes left
	sideRight                   // text right of anchor, leader goes right
)

// overlayLabel is one callout: a leader line from a chip-coord
// anchor point to a text label placed near the chip edge.
type overlayLabel struct {
	text    string
	chipX   int32     // anchor in chip-space (where the leader points)
	chipY   int32     // anchor in chip-space
	side    labelSide // which way the leader / text extends
	leadPx  int       // length of the leader line in pixels (0 = auto)
}

// labelStyle controls how a label is positioned and decorated.
type labelStyle uint8

const (
	// styleEdgeAligned: anchor at the pad polygon nearest the given
	// edge; text sits OUTSIDE the chip on that edge with the
	// varying axis at the pad centroid and the fixed axis at a
	// small gap from the chip-bbox edge. No leader line — the
	// labels along an edge form a clean column or row aligned to
	// the chip's edge. Used for all bus + control pins.
	styleEdgeAligned labelStyle = iota
	// styleInterior: anchor at explicit chipX/chipY (or node
	// centroid), draw a short leader line, place text at the end.
	// Used for functional-region labels like INSTRUCTION DECODE.
	styleInterior
)

// rawLabel is a label spec before resolution to pixel positions.
// Specify ONE of: node, nodes, or chipX/chipY (with appropriate
// style). hintX/hintY is optional and biases multi-polygon nets
// (VSS, VCC) toward the pad area we care about.
type rawLabel struct {
	text         string
	style        labelStyle
	chipX, chipY int32     // explicit anchor (styleInterior)
	hintX, hintY int32     // optional: bias multi-polygon node lookups
	node         int       // single-node lookup
	nodes        []int     // averaged centroid (styleInterior register clusters)
	side         labelSide // override for styleInterior leader direction
	leadPx       int       // leader length in pixels (styleInterior)
}

// rawOverlayLabels groups pin pads by which chip edge they sit on,
// so the labels along an edge form a clean column / row. The
// padPolygonOnEdge lookup with each group's edge param picks the
// polygon that's actually the bonding pad on that side, not a
// stray routing trace.
//
// All pin text is ≤3 chars so the label column / row stays narrow.
var rawOverlayLabels = buildRawOverlayLabels()

func buildRawOverlayLabels() []rawLabel {
	out := []rawLabel{
		// ── Control pins on the TOP edge ───────────────────────
		// -Y anchor: text sits ABOVE the chip aligned with each pad.
		{text: "NMI", style: styleEdgeAligned, node: nodeNMI, side: sideTop},
		{text: "IRQ", style: styleEdgeAligned, node: nodeIRQ, side: sideTop},
		{text: "RDY", style: styleEdgeAligned, node: nodeRDY, side: sideTop},
		{text: "RST", style: styleEdgeAligned, node: nodeRES, side: sideTop},
		// CLK label intentionally omitted — its pad sits in awkward
		// proximity to R/W and gets in the way of the dataflow read.
		// R/W verified via shift+click at chip(8806, 9482) → node 1156.
		{text: "R/W", style: styleEdgeAligned, node: nodeRW, side: sideTop,
			chipX: 8806, chipY: 9482},

		// ── Interior functional blocks ─────────────────────────
		{text: "INSTRUCTION DECODE", style: styleInterior,
			chipX: 4500, chipY: 8000, side: sideTop, leadPx: 0},
		// Register labels: leader length tuned so the label floats
		// just above the storage cells without flying up to the
		// chip top. Bit-node-centroid anchors landed on the actual
		// cells per user verification.
		{text: "Y", style: styleInterior, nodes: registerY[:], side: sideTop, leadPx: 80},
		// Shorter leader on X so its text sits BELOW Y's instead of
		// touching it — Y/X/S columns are close together so without
		// staggered leader lengths the labels collide horizontally.
		{text: "X", style: styleInterior, nodes: registerX[:], side: sideTop, leadPx: 40},
		{text: "S", style: styleInterior, nodes: registerS[:], side: sideTop, leadPx: 80},
		{text: "A", style: styleInterior, nodes: registerA[:], side: sideTop, leadPx: 80},
		{text: "STATUS", style: styleInterior, nodes: registerP[:], side: sideTop, leadPx: 60},
	}
	// ── Data bus on the RIGHT edge (+X anchor) ─────────────────
	// Explicit chip-space anchors override padPolygonOnEdge so we
	// can pin a label to the exact pad location read off the
	// shift+click inspector. Fill these in as we verify each pin.
	// All data-bus anchors verified by shift+click inspection.
	// D2, D4, D5 weren't clicked — linearly interpolated from
	// their neighbours (uneven spacing on the right edge: D0 sits
	// at the top corner, D1-D7 march down in a denser column).
	// D0-D3 verified by inspector. D4-D7 nudged from user feedback
	// after watching the live render — silicon-up Y means lower
	// number = lower on screen.
	dataPadAnchors := map[int][2]int32{
		0: {8806, 9137},
		1: {8944, 3965},
		2: {8944, 3448}, 
		3: {8944, 2931},
		4: {8944, 2000},
		5: {8944, 1450}, 
		6: {8944, 950}, 
		7: {8200, 400},  
	}
	for i, n := range dataPadNodes {
		lbl := rawLabel{
			text: pinName("D", i), style: styleEdgeAligned, node: n, side: sideRight,
		}
		if a, ok := dataPadAnchors[i]; ok {
			lbl.chipX, lbl.chipY = a[0], a[1]
		}
		out = append(out, lbl)
	}
	// ── Address bus pads ─ user-specified split: A0-A6 on the LEFT
	// edge (-X anchor), A7-A15 on the BOTTOM edge (+Y anchor).
	// Explicit anchors override padPolygonOnEdge — fill in as
	// shift+click readings are confirmed; missing entries fall back
	// to the algorithmic edge lookup.
	// Left-edge group (A0-A6) and bottom-edge group (A7-A15). All
	// except A8, A10-A13, A15 verified via shift+click inspector;
	// the remainder are interpolated from neighbouring readings.
	addrPadAnchors := map[int][2]int32{
		0: {393, 4900},
		1: {324, 3520},
		2: {324, 3031},
		3: {324, 2500},
		4: {324, 2041},
		5: {324, 1551},
		6: {324, 1025},
		7: {668, 617},
		// A7=668, A9=2186, A14=6462; A8 is halfway A7↔A9, A10-A13
		// step evenly between A9 and A14 (~855 per step); A15
		// extrapolates one step right of A14.
		8:  {1427, 517}, // interpolated
		9:  {2186, 517}, // verified
		10: {3041, 517},
		11: {3896, 517},
		12: {4751, 517},
		13: {5606, 517},
		14: {6462, 517}, // verified
		15: {7317, 517},
	}
	for i := 0; i <= 6; i++ {
		lbl := rawLabel{
			text: pinName("A", i), style: styleEdgeAligned,
			node: addressPadNodes[i], side: sideLeft,
		}
		if a, ok := addrPadAnchors[i]; ok {
			lbl.chipX, lbl.chipY = a[0], a[1]
		}
		out = append(out, lbl)
	}
	for i := 7; i <= 15; i++ {
		lbl := rawLabel{
			text: pinName("A", i), style: styleEdgeAligned,
			node: addressPadNodes[i], side: sideBottom,
		}
		if a, ok := addrPadAnchors[i]; ok {
			lbl.chipX, lbl.chipY = a[0], a[1]
		}
		out = append(out, lbl)
	}
	return out
}

// pinName formats a bus pin label. Compact ("D0".."D7", "A0".."A15")
// to keep the 24-pin perimeter readable; everything stays ≤3 chars.
func pinName(prefix string, idx int) string {
	if idx < 10 {
		return prefix + string(rune('0'+idx))
	}
	return prefix + "1" + string(rune('0'+(idx-10)))
}

// paintLabel renders one callout. Two modes via leadPx:
//
//	leadPx == -1 → edge-aligned: text sits just OUTSIDE the chip
//	               box on lbl.side, fixed axis from box edge,
//	               varying axis from the projected pad centroid.
//	               No leader line drawn.
//	leadPx >= 0  → interior: leader from chip anchor extends in
//	               lbl.side direction by leadPx pixels, text hangs
//	               off the leader tip.
func paintLabel(buf []byte, pxW, pxH int, project func(cx, cy int32) (int, int), box chipBox, lbl overlayLabel) {
	const (
		glyphW = 5
		glyphH = 7
		scale  = 2
		gap    = 1
	)
	cellW := (glyphW + gap) * scale
	cellH := (glyphH + 1) * scale
	// Visible text width: every char contributes (glyphW+gap)*scale
	// except the last one's trailing gap is unused space. Stripping
	// it makes the visual midpoint of the glyph row land at exactly
	// ax when we tx0-by-half centering — fixes the "letter sits 1
	// pixel left of the leader line" drift in the interior labels.
	textW := len(lbl.text)*cellW - gap*scale
	if textW < 0 {
		textW = 0
	}
	textH := cellH

	ax, ay := project(lbl.chipX, lbl.chipY)

	var tx0, ty0 int
	if lbl.leadPx < 0 {
		// Edge-aligned: fixed axis at the visible chip-data edge
		// (not the abstract chip-space box, which has ~10 % empty
		// whitespace on each side); varying axis at the projected
		// pad centroid.
		const edgeGap = 4
		dataLeftPx, _ := project(chipDataMinX, 0)
		dataRightPx, _ := project(chipDataMaxX, 0)
		// Y projection flips: chipDataMaxY corresponds to the TOP
		// of the display, chipDataMinY to the BOTTOM.
		_, dataTopPx := project(0, chipDataMaxY)
		_, dataBottomPx := project(0, chipDataMinY)
		_ = box // chip-space box kept for callers that still want it
		switch lbl.side {
		case sideLeft:
			tx0 = dataLeftPx - textW - edgeGap
			ty0 = ay - textH/2
		case sideRight:
			tx0 = dataRightPx + edgeGap
			ty0 = ay - textH/2
		case sideTop:
			tx0 = ax - textW/2
			ty0 = dataTopPx - textH - edgeGap
		case sideBottom:
			tx0 = ax - textW/2
			ty0 = dataBottomPx + edgeGap
		}
	} else {
		// Interior: optional leader, text hangs off the tip.
		lead := lbl.leadPx
		var tx, ty int
		switch lbl.side {
		case sideTop:
			tx, ty = ax, ay-lead
		case sideBottom:
			tx, ty = ax, ay+lead
		case sideLeft:
			tx, ty = ax-lead, ay
		case sideRight:
			tx, ty = ax+lead, ay
		}
		if lead > 0 {
			drawLineAA(buf, pxW, pxH, ax, ay, tx, ty, 0xff, 0xff, 0xff, 230)
		}
		switch lbl.side {
		case sideTop:
			tx0, ty0 = tx-textW/2, ty-textH-2
		case sideBottom:
			tx0, ty0 = tx-textW/2, ty+2
		case sideLeft:
			tx0, ty0 = tx-textW-2, ty-textH/2
		case sideRight:
			tx0, ty0 = tx+2, ty-textH/2
		}
	}
	for i, r := range lbl.text {
		drawGlyph(buf, pxW, pxH,
			tx0+i*cellW, ty0,
			r, scale,
			0xff, 0xff, 0xff,
			0, 0, 0)
	}
}

// drawLineAA is a thin alpha-blended line walker (DDA). Cheap and
// good enough for the leader lines — we'd reach for a real
// anti-aliased rasterizer if we needed more polish.
func drawLineAA(buf []byte, pxW, pxH, x0, y0, x1, y1 int, r, g, b byte, alpha uint16) {
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
		return
	}
	inv := uint16(256) - alpha
	for s := 0; s <= steps; s++ {
		x := x0 + dx*s/steps
		y := y0 + dy*s/steps
		if x < 0 || x >= pxW || y < 0 || y >= pxH {
			continue
		}
		o := (y*pxW + x) * 4
		if o+3 >= len(buf) {
			continue
		}
		buf[o+0] = byte((uint16(r)*alpha + uint16(buf[o+0])*inv) >> 8)
		buf[o+1] = byte((uint16(g)*alpha + uint16(buf[o+1])*inv) >> 8)
		buf[o+2] = byte((uint16(b)*alpha + uint16(buf[o+2])*inv) >> 8)
	}
}
