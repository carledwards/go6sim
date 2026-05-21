// SimHost is the Monitor host for the native sim TUI. The Monitor's
// shared package (internal/monitor) calls back into here for machine
// metadata + lifecycle hooks; the bridge.Target side is satisfied by
// a bridge.HubDirect wrapping the sim's shared Hub. Same Monitor
// code runs in cmd/6502-control over the wire and here in-process —
// the architectural payoff of the bridge facade refactor.
package main

import (
	"encoding/json"
	"fmt"

	"github.com/carledwards/go6sim/bridge"
	"github.com/carledwards/go6sim/internal/monitor"
)

// jsonUnmarshalBytes is a one-liner so callers stay terse. Malformed
// payloads from the in-process Hub are bugs, not recoverable errors.
func jsonUnmarshalBytes(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}

func fmtBPHit(p bridge.BPHitPayload) string {
	return fmt.Sprintf("bp.hit %s @ $%04X", p.ID, p.Addr)
}

func fmtClockHalt(p bridge.RunResult) string {
	return fmt.Sprintf("halt %s @ $%04X", p.Reason, p.Addr)
}

func fmtTap(p bridge.TapChangedPayload) string {
	return fmt.Sprintf("%s = %d", p.Name, p.Value)
}

// SimHost implements monitor.Host for the in-process sim TUI. Most
// methods return constant per-launch info — the sim TUI knows its
// machine at startup and doesn't reload; quitSignal is wired by
// main once foxpro is up.
type SimHost struct {
	machineLabel string
	buildSummary string
	programName  string
	programSize  int
	regions      []bridge.Region

	quit         func()
	helpOpener   func()
}

func newSimHost(label, summary string, regions []bridge.Region) *SimHost {
	return &SimHost{
		machineLabel: label,
		buildSummary: summary,
		regions:      regions,
	}
}

func (h *SimHost) MachineLabel() string     { return h.machineLabel }
func (h *SimHost) BuildSummary() string     { return h.buildSummary }
func (h *SimHost) ProgramName() string      { return h.programName }
func (h *SimHost) ProgramSize() int         { return h.programSize }
func (h *SimHost) Regions() []bridge.Region { return h.regions }

func (h *SimHost) OpenHelpWindow() {
	if h.helpOpener != nil {
		h.helpOpener()
	}
}

func (h *SimHost) Quit() {
	if h.quit != nil {
		h.quit()
	}
}

// Reconnect is a no-op for the in-process target — there's no
// transport to re-establish. We just want the user to know the
// command isn't doing nothing silently.
func (h *SimHost) Reconnect() {
	// The Monitor logs "reconnect requested" before calling this; the
	// info line is enough. No further action needed for direct mode.
}

// consumeHubDirectNotifications drains the HubDirect's notification
// channel and forwards sim-pushed events into the Monitor's
// scrollback. The sim TUI's CPU + Display windows read inst state
// directly, so state.snapshot is intentionally ignored — no mirror
// to keep current. bp.hit / clock.halt / tap.changed all surface to
// the user as event lines.
func consumeHubDirectNotifications(hd *bridge.HubDirect, mon *monitor.Monitor) {
	for n := range hd.Notifications() {
		switch n.Method {
		case "bp.hit":
			var p bridge.BPHitPayload
			if err := jsonUnmarshalBytes(n.Params, &p); err == nil {
				mon.AddEvent("bp", fmtBPHit(p))
			}
		case "clock.halt":
			var p bridge.RunResult
			if err := jsonUnmarshalBytes(n.Params, &p); err == nil {
				mon.AddEvent("halt", fmtClockHalt(p))
			}
		case "tap.changed":
			var p bridge.TapChangedPayload
			if err := jsonUnmarshalBytes(n.Params, &p); err == nil {
				mon.AddEvent("tap", fmtTap(p))
			}
		}
	}
}
