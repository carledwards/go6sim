//go:build js && wasm

// SimHost mirror of cmd/6502-sim/simhost.go — same Monitor.Host
// implementation, same notification consumer. Lives here because
// the cmd/* directories are individual packages; the alternative is
// a shared internal/simhost package, worth doing if a third in-
// process host appears.
package main

import (
	"encoding/json"
	"fmt"

	"github.com/carledwards/go6sim/bridge"
	"github.com/carledwards/go6sim/internal/monitor"
)

// SimHost is the Monitor host for the in-process wasm sim TUI.
type SimHost struct {
	machineLabel string
	buildSummary string
	programName  string
	programSize  int
	regions      []bridge.Region

	quit       func()
	helpOpener func()
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

// Reconnect is a no-op for the in-process target.
func (h *SimHost) Reconnect() {}

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

// consumeHubDirectNotifications drains the HubDirect notification
// channel and forwards sim-pushed events into the Monitor scrollback.
// state.snapshot is intentionally dropped — wasm's CPU/Display
// windows read inst state directly, no mirror to maintain.
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
