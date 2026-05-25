package visualcpuwin

import "sync"

// Pad node IDs from visual6502/nodenames.js. One node per pad.
// The "Y X S A STATUS" interior labels aren't single nodes — they're
// register clusters, computed as the centroid of every bit's node
// in regCentroids below.
const (
	nodeNMI = 1297
	nodeIRQ = 103
	nodeRDY = 89
	nodeRES = 159
	nodeRW  = 1156
)

// addressPadNodes[i] is the node ID for AB<i> (i in 0..15).
// Order taken from nodenames.js's `ab0..ab15` entries.
var addressPadNodes = [16]int{
	268, 451, 1340, 211, 435, 736, 887, 1493,
	230, 148, 1443, 399, 1237, 349, 672, 195,
}

// dataPadNodes[i] is the node ID for DB<i> (i in 0..7).
var dataPadNodes = [8]int{
	1005, 82, 945, 650, 1393, 175, 1591, 1349,
}

// Register clusters — one entry per bit. centroidOfNodes averages
// them to anchor the register-name overlay label.
var (
	registerA      = [8]int{737, 1234, 978, 162, 727, 858, 1136, 1653}
	registerX      = [8]int{1216, 98, 1, 1648, 85, 589, 448, 777}
	registerY      = [8]int{64, 1148, 573, 305, 989, 615, 115, 843}
	registerS      = [8]int{1403, 183, 81, 1532, 1702, 1098, 1212, 1435}
	// p4 is dummy / p5 = -1 (no bit). Skip non-positive ids.
	registerP = [8]int{32, 627, 1553, 348, 1119, -1, 1625, 69}
)

// nodeCentroidsOnce protects the lazy build of nodeCentroid.
var (
	nodeCentroidsOnce sync.Once
	nodeCentroid      map[int][2]float64 // node id → (avgX, avgY) in chip-space
)

// buildNodeCentroids scans every Segment once and accumulates each
// node's polygon vertex coordinates into running sums. Centroids
// are vertex averages (NOT area-weighted centroids — for label
// anchoring the approximation is fine and the math is cheap).
func buildNodeCentroids() {
	type acc struct {
		sumX, sumY float64
		n          int
	}
	sums := make(map[int]*acc, 1800)
	for i := range Segments {
		seg := &Segments[i]
		a := sums[int(seg.NodeID)]
		if a == nil {
			a = &acc{}
			sums[int(seg.NodeID)] = a
		}
		for j := 0; j+1 < len(seg.Coords); j += 2 {
			a.sumX += float64(seg.Coords[j])
			a.sumY += float64(seg.Coords[j+1])
			a.n++
		}
	}
	nodeCentroid = make(map[int][2]float64, len(sums))
	for id, a := range sums {
		if a.n == 0 {
			continue
		}
		nodeCentroid[id] = [2]float64{a.sumX / float64(a.n), a.sumY / float64(a.n)}
	}
}

// centroidOf returns the chip-space centroid of a single node, or
// (0,0)+false if the node has no polygons.
func centroidOf(node int) (int32, int32, bool) {
	nodeCentroidsOnce.Do(buildNodeCentroids)
	c, ok := nodeCentroid[node]
	if !ok {
		return 0, 0, false
	}
	return int32(c[0]), int32(c[1]), true
}

// centroidOfNodes averages the centroids of every (positive) node
// id in the slice — used for the register-cluster labels (Y/X/S/A/
// STATUS) where one label points at a multi-bit storage element.
func centroidOfNodes(nodes []int) (int32, int32) {
	var sx, sy float64
	var n int
	for _, id := range nodes {
		if id <= 0 {
			continue
		}
		cx, cy, ok := centroidOf(id)
		if !ok {
			continue
		}
		sx += float64(cx)
		sy += float64(cy)
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return int32(sx / float64(n)), int32(sy / float64(n))
}

// padPolygonOnEdge returns the OUTER-EDGE midpoint of the node's
// bonding pad on a chip side.
//
// Visual6502's layer 0 is the metal/pad layer (translucent slate,
// stroked white). The bonding pad is always a layer-0 polygon, so
// restricting the search to that layer gives us a clean shortlist
// (~1-5 polygons for a signal node). Among those we still pick the
// most-extreme, area-tiebreak combination so a tiny metal stub
// that pokes a hair further than the real pad can't win.
//
// Falls back to "any layer" if the node has no layer-0 polygons
// (defensive — should never happen for documented bus / control
// pins, since they all have metal pads by definition).
func padPolygonOnEdge(node int, edge labelSide) (int32, int32, bool) {
	if cx, cy, ok := padPolygonOnEdgeLayer(node, edge, 0); ok {
		return cx, cy, true
	}
	return padPolygonOnEdgeLayer(node, edge, -1)
}

// padPolygonOnEdgeLayer is padPolygonOnEdge restricted to a single
// silicon layer. Pass layer=-1 to consider every layer.
func padPolygonOnEdgeLayer(node int, edge labelSide, layer int) (int32, int32, bool) {
	// Pass 1: find the most extreme bbox-edge for this node + layer.
	maxVal := int32(-(1 << 30))
	for i := range Segments {
		seg := &Segments[i]
		if int(seg.NodeID) != node {
			continue
		}
		if layer >= 0 && int(seg.Layer) != layer {
			continue
		}
		if len(seg.Coords) < 4 {
			continue
		}
		v := extremityForSeg(seg, edge)
		if v > maxVal {
			maxVal = v
		}
	}
	if maxVal == int32(-(1<<30)) {
		return 0, 0, false
	}
	// Pass 2: of polygons within 5 % of chipSize from that extreme,
	// pick the one with the largest bbox area.
	const tolerance int32 = chipSize / 20
	threshold := maxVal - tolerance
	var bestArea int64
	var bestCX, bestCY int32
	found := false
	for i := range Segments {
		seg := &Segments[i]
		if int(seg.NodeID) != node {
			continue
		}
		if layer >= 0 && int(seg.Layer) != layer {
			continue
		}
		if len(seg.Coords) < 4 {
			continue
		}
		v, area, cx, cy := infoForSeg(seg, edge)
		if v < threshold {
			continue
		}
		if !found || area > bestArea {
			bestArea = area
			bestCX = cx
			bestCY = cy
			found = true
		}
	}
	return bestCX, bestCY, found
}

// extremityForSeg returns the "how far toward `edge`" value for one
// polygon — larger means closer to that edge.
func extremityForSeg(seg *Segment, edge labelSide) int32 {
	minX, maxX, minY, maxY := segBBox(seg)
	switch edge {
	case sideLeft:
		return -minX
	case sideRight:
		return maxX
	case sideTop:
		return maxY
	case sideBottom:
		return -minY
	}
	return 0
}

// infoForSeg returns extremity, bbox area, and outer-edge midpoint
// for one polygon on a given edge.
func infoForSeg(seg *Segment, edge labelSide) (val int32, area int64, cx, cy int32) {
	minX, maxX, minY, maxY := segBBox(seg)
	area = int64(maxX-minX) * int64(maxY-minY)
	switch edge {
	case sideLeft:
		val = -minX
		cx = minX
		cy = (minY + maxY) / 2
	case sideRight:
		val = maxX
		cx = maxX
		cy = (minY + maxY) / 2
	case sideTop:
		val = maxY
		cx = (minX + maxX) / 2
		cy = maxY
	case sideBottom:
		val = -minY
		cx = (minX + maxX) / 2
		cy = minY
	}
	return
}

func segBBox(seg *Segment) (minX, maxX, minY, maxY int32) {
	minX, minY = int32(1<<30), int32(1<<30)
	maxX, maxY = int32(-(1 << 30)), int32(-(1 << 30))
	for j := 0; j+1 < len(seg.Coords); j += 2 {
		x, y := seg.Coords[j], seg.Coords[j+1]
		if x < minX {
			minX = x
		}
		if x > maxX {
			maxX = x
		}
		if y < minY {
			minY = y
		}
		if y > maxY {
			maxY = y
		}
	}
	return
}

// padNearHint walks every polygon of node and returns the centroid
// of the one whose own centroid is CLOSEST to (hintX, hintY) in
// chip-space. Used for power/ground nets (VSS, VCC, …) where the
// per-node centroid is uselessly in the middle of the chip because
// the rail spans the whole die — the hint pins us to the polygon
// that's actually the package pad.
func padNearHint(node int, hintX, hintY int32) (int32, int32, bool) {
	var bestX, bestY int32
	bestDist := float64(1 << 62)
	found := false
	for i := range Segments {
		seg := &Segments[i]
		if int(seg.NodeID) != node {
			continue
		}
		var sx, sy float64
		var n int
		for j := 0; j+1 < len(seg.Coords); j += 2 {
			sx += float64(seg.Coords[j])
			sy += float64(seg.Coords[j+1])
			n++
		}
		if n == 0 {
			continue
		}
		cx, cy := sx/float64(n), sy/float64(n)
		dx := cx - float64(hintX)
		dy := cy - float64(hintY)
		d := dx*dx + dy*dy
		if d < bestDist {
			bestDist = d
			bestX = int32(cx)
			bestY = int32(cy)
			found = true
		}
	}
	return bestX, bestY, found
}
