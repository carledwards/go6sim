//go:build !js || !wasm

package bridge

// yieldMaxMode is a per-slice yield hook called from the Pump's
// running branch when no Hz pacing is configured ("Max" mode). On
// native Go, preemptive scheduling already keeps every other
// goroutine fed — no explicit yield needed, so this compiles to a
// no-op call the optimizer can inline away. The wasm build
// (yield_wasm.go) overrides this with a time-budget yield so the JS
// event loop gets repainting time.
//
// The Pump pointer is taken so the wasm variant can consult
// p.lastYield without us needing a separate registration step.
func yieldMaxMode(p *Pump) {}
