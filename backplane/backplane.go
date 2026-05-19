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
