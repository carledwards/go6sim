//go:build js

package netsim

// MaxHz reports the netsim transistor core's sustained ceiling under
// wasm. Measured at ~6 kHz in browsers — wasm bounds-check overhead
// plus the lack of native SIMD slow per-half-step throughput compared
// to a native build (~22 kHz).
func (a *Adapter) MaxHz() int { return 6000 }
