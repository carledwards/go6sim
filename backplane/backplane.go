// Package backplane is the stable composition surface for a go6sim
// machine: the one object presets compose and the instrument layer
// (TUI, web, MCP, future debugger) talks to. See
// docs/architecture-backplane.md.
//
// It deliberately *embeds* *bus.TraceBus rather than replacing it:
// TraceBus already is the working backplane (memory map + per-frame
// generation trace + Ticker fan-out), and the UI (ui/ramwin) depends on
// its concrete trace API for per-cell tinting. Embedding inherits that
// contract verbatim — zero reimplementation, zero behaviour change —
// while giving us one named surface to grow (Attach, Reset capability
// fan-out now; clock-driver + presets next).
package backplane

import "github.com/carledwards/go6sim/bus"

// Backplane is a machine's bus plus capability orchestration. It IS a
// bus.Bus (and a Ticker, and exposes the trace) via the embedded
// *bus.TraceBus, so CPU backends (interp.New / netsim.New) and the
// existing windows accept it unchanged.
type Backplane struct {
	*bus.TraceBus

	// hostIRQ is a debugger-driven IRQ assertion OR'd into the
	// wired-OR seen by the CPU. Used by Hub.CmdIRQ so the controller
	// REPL can pulse an interrupt — the actual peripherals stay in
	// charge of their own IRQAsserted bits; this is a separate
	// "console" line on top.
	hostIRQ bool

	// hostNMI is the analogous host-driven NMI line. Unlike IRQ, no
	// peripheral asserts NMI in this codebase today, so the wired-OR
	// degenerates to this single source. Edge-triggered: the CPU
	// adapter detects a false→true transition at SYNC and services
	// once per pulse.
	hostNMI bool
}

// New returns an empty backplane (mapping bus wrapped in the trace).
func New() *Backplane {
	return &Backplane{TraceBus: bus.NewTraceBus(bus.New())}
}

// Attach registers a card on the backplane. A card owns its own address
// range (Base/Size) — the spec's Attach(card,lo,hi) sketch yields to
// the codebase's component-owned-range model (same reconciliation as
// the brick-1 card contract). Returns an error on overlap / out of
// range, exactly as bus.Register.
func (b *Backplane) Attach(c bus.Component) error {
	return b.Register(c)
}

// Reset returns the machine to power-on state: every attached card that
// implements the bus.Resettable capability is reset; passive cards
// (plain RAM/ROM regions with no Reset) are skipped. This is the
// capability fan-out the instrument's "reset" / fresh-reboot uses.
func (b *Backplane) Reset() {
	for _, c := range b.Components() {
		if r, ok := c.(bus.Resettable); ok {
			r.Reset()
		}
	}
}

// Trace exposes the underlying trace bus for UI consumers that need the
// concrete generation/recency API (ui/ramwin's per-cell tinting). The
// instrument layer should prefer the Backplane surface; this is the
// escape hatch for the existing windows during the carve.
func (b *Backplane) Trace() *bus.TraceBus { return b.TraceBus }

// IRQ is the wired-OR of every attached bus.IRQSource card's
// IRQAsserted state — the shared interrupt line the CPU honors (brick
// 9b). True means at least one card is requesting an interrupt.
// Cards without the capability never pull the line. The debugger's
// host-IRQ line (AssertHostIRQ) is OR'd in too — same wire, separate
// driver.
func (b *Backplane) IRQ() bool {
	if b.hostIRQ {
		return true
	}
	for _, c := range b.Components() {
		if s, ok := c.(bus.IRQSource); ok && s.IRQAsserted() {
			return true
		}
	}
	return false
}

// AssertHostIRQ raises (or releases) the debugger-driven IRQ line.
// OR'd into the wired-OR seen by the CPU — peripheral IRQs are
// unaffected. Used by Hub.CmdIRQ to expose a single-pulse "fire an
// interrupt" affordance to the REPL controller.
//
// Concurrency: written by the Pump goroutine, read by the CPU
// goroutine. They are the same goroutine in our single-Pump design,
// so no synchronization is needed. If we ever spawn a parallel
// reader, this becomes an atomic.Bool.
func (b *Backplane) AssertHostIRQ(on bool) { b.hostIRQ = on }

// HostIRQAsserted reports the current host-IRQ line state — handy for
// tests + the Hub's deassert bookkeeping.
func (b *Backplane) HostIRQAsserted() bool { return b.hostIRQ }

// NMI reports whether the shared NMI line is asserted. Wired-OR of
// any future NMI-source peripherals (none today) plus the debugger's
// host-NMI line. Interp's nmiEdge reads this at SYNC; on the netsim
// path it's a no-op until the netsim-go module exposes SetNMI.
func (b *Backplane) NMI() bool { return b.hostNMI }

// AssertHostNMI raises (or releases) the debugger-driven NMI line.
// Edge-triggered: pulsing means asserting briefly then releasing so
// the CPU sees a clean rising edge. Hub.CmdNMI does the pulse pair.
func (b *Backplane) AssertHostNMI(on bool) { b.hostNMI = on }

// HostNMIAsserted reports the current host-NMI line state.
func (b *Backplane) HostNMIAsserted() bool { return b.hostNMI }
