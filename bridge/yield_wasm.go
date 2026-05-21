//go:build js && wasm

package bridge

import "time"

// wasmYieldInterval is how long the Pump may run flat-out between
// explicit time.Sleep yields to the JS event loop. Browsers paint at
// 60 fps (16.6 ms/frame); 10 ms gives the renderer a full frame of
// headroom while letting the Pump batch many slices between yields
// — far better throughput than "yield every slice" (~75 kHz halves)
// while keeping the UI smooth.
const wasmYieldInterval = 10 * time.Millisecond

// yieldMaxMode hands control back to the JS event loop when due.
// On wasm the Go runtime is cooperatively scheduled inside a single
// JS thread — without an explicit time.Sleep the Pump's tight loop
// monopolises every other goroutine including the foxpro render
// path, and the browser tab spins at 100 % CPU with a frozen page.
//
// We don't sleep every slice — that caps throughput at ~1 / (slice
// + sleep ms). Instead, after each slice we check whether
// wasmYieldInterval has elapsed since the previous yield; if so,
// time.Sleep(1 ms) (the smallest reliable browser-side yield), then
// reset the timestamp. The net effect: ~10× the per-slice
// throughput of the naïve "sleep every slice" version, without
// re-introducing the freeze.
func yieldMaxMode(p *Pump) {
	if time.Since(p.lastYield) < wasmYieldInterval {
		return
	}
	time.Sleep(time.Millisecond)
	p.lastYield = time.Now()
}
