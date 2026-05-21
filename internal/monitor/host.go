// Package monitor is the shared Monitor REPL — a foxpro content
// provider that hosts a HESMON-style command line for any consumer
// of the bridge.Target protocol. Today: cmd/6502-control (wire) and
// cmd/6502-sim / cmd/6502-wasm (in-process via HubDirect).
//
// The Monitor itself owns the scrollback storage + input editing +
// command dispatcher. Anything app-specific — machine metadata,
// quit signal, opening a help window, mirror state — is exposed
// through the Host interface so the same Monitor code can plug into
// different app shells without dragging app-specific assumptions
// across.
package monitor

import "github.com/carledwards/go6sim/bridge"

// Host is what the embedding application provides to the Monitor:
// access to machine metadata for descriptive commands (hw, info, via),
// a quit signal for the `q`/`quit` command, and an optional help-
// window opener. Implementations live in each command (6502-control,
// 6502-sim, 6502-wasm) and supply their own state-mirroring.
//
// Method receivers must be safe to call from the foxpro goroutine
// (which is also where command dispatch runs). None of these methods
// should block on the wire; they're read accessors over data the
// Host already has from its machine.load response.
type Host interface {
	// Machine metadata, sourced from the machine.load response.
	// Used by the `hw` / `info` command to describe what's wired.
	MachineLabel() string
	BuildSummary() string
	ProgramName() string
	ProgramSize() int
	Regions() []bridge.Region

	// OpenHelpWindow pops a persistent help window if the app
	// supports one. Implementations may no-op if there's no UI
	// surface for it (headless future hosts). The Monitor calls
	// this when the user types `help window`.
	OpenHelpWindow()

	// Quit signals the application to shut down. Called when the
	// user types `q` / `quit`. The Host owns whatever teardown
	// concerns its app has (saving state, closing connections, etc.).
	Quit()

	// Reconnect forces a re-establishment of the underlying transport.
	// Wire-client hosts close their current connection and let the
	// heal loop redial. In-process hosts (HubDirect-backed) no-op
	// and emit an info line via the Monitor — same surface, honest
	// behaviour about what each transport can do.
	Reconnect()
}
