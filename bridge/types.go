package bridge

import "encoding/json"

// Protocol version negotiated in `hello`. Adding a method = bump minor;
// removing or changing the shape of an existing method = bump major.
// See docs/bridge.md §8.
const (
	Protocol      = "1.0"
	ServerVersion = "0.1.0"
)

// JSON-RPC 2.0 envelopes — unexported; only the typed Params/Result
// surfaces are public.

type request struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
}

// hello

type HelloParams struct {
	ClientName    string   `json:"clientName"`
	ClientVersion string   `json:"clientVersion"`
	Protocols     []string `json:"protocols,omitempty"`
}

type HelloResult struct {
	ServerVersion string   `json:"serverVersion"`
	Protocol      string   `json:"protocol"`
	Capabilities  []string `json:"capabilities"`
}

// machine.list

// PresetInfo describes one preset a server can build. Name is the
// machine-readable slug used by machine.load; Label is the
// human-readable display string; Summary is a one-line description of
// what the preset contains. Clients should display Label (falling
// back to Name) and Summary; Name is for programmatic addressing.
type PresetInfo struct {
	Name    string `json:"name"`
	Label   string `json:"label,omitempty"`
	Summary string `json:"summary"`
}

type MachineListResult struct {
	Presets []PresetInfo `json:"presets"`
}

// machine.load

type Image struct {
	BytesB64 string `json:"bytes_b64"`
	Origin   uint16 `json:"origin"`
}

type MachineLoadParams struct {
	Preset string `json:"preset"`
	Image  *Image `json:"image,omitempty"`
}

type Region struct {
	Name     string `json:"name"`
	Lo       uint16 `json:"lo"`
	Hi       uint16 `json:"hi"`
	ReadOnly bool   `json:"readOnly"`
}

// MachineLoadResult is what machine.load returns. Preset is the slug
// the client requested (echoed). Label + Summary are the human strings
// the server records for that preset — clients render those rather
// than the slug so the display reflects the machine, not its key.
type MachineLoadResult struct {
	Preset  string   `json:"preset"`
	Label   string   `json:"label,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Regions []Region `json:"regions"`
}

// cpu.state

type CPUState struct {
	A          uint8  `json:"a"`
	X          uint8  `json:"x"`
	Y          uint8  `json:"y"`
	SP         uint8  `json:"sp"`
	P          uint8  `json:"p"`
	PC         uint16 `json:"pc"`
	HalfCycles uint64 `json:"halfCycles"`
	Running    bool   `json:"running"`
}

// cpu.setPC — overwrite the program counter. Backend must implement
// cpu.PCSetter (interp does; netsim does not because the upstream
// netsim-go module exposes no register setters).
type CPUSetPCParams struct {
	PC uint16 `json:"pc"`
}

type CPUSetPCResult struct {
	OK    bool     `json:"ok"`
	State CPUState `json:"state"`
}

// clock.*

type ClockStepParams struct {
	N int `json:"n,omitempty"`
}

type ClockStepResult struct {
	State CPUState `json:"state"`
}

type ClockStepCycleParams struct {
	Halves int `json:"halves,omitempty"`
}

type ClockSetSpeedParams struct {
	Hz int `json:"hz"`
}

type ClockSetSpeedResult struct {
	Hz int `json:"hz"`
}

type ClockAdvanceParams struct {
	DurationUs int `json:"durationUs"`
}

type ClockAdvanceResult struct {
	HalfCycles int      `json:"halfCycles"`
	State      CPUState `json:"state"`
}

type ClockRunningResult struct {
	Running bool `json:"running"`
	SpeedHz int  `json:"speedHz"`
}

type ClockRunUntilParams struct {
	MaxHalfCycles int `json:"maxHalfCycles"`
}

// RunResult mirrors the design §5.4 shape — same envelope used by
// `clock.runUntil` synchronously and by the async `clock.halt`
// notification when Phase 1c-2 lands.
type RunResult struct {
	HalfCycles uint64   `json:"halfCycles"`
	Reason     string   `json:"reason"`
	Addr       uint16   `json:"addr"`
	BpID       string   `json:"bpId,omitempty"`
	State      CPUState `json:"state"`
}

// mem.*

type MemPeekParams struct {
	Addr uint16 `json:"addr"`
	N    int    `json:"n,omitempty"`
}

type MemPeekResult struct {
	BytesB64 string `json:"bytes_b64"`
}

type MemPokeParams struct {
	Addr     uint16 `json:"addr"`
	BytesB64 string `json:"bytes_b64"`
}

type MemPokeResult struct {
	Written int `json:"written"`
}

type MemReadParams struct {
	Region string `json:"region"`
}

type MemReadResult struct {
	BytesB64 string `json:"bytes_b64"`
	Addr     uint16 `json:"addr"`
	Length   int    `json:"length"`
}

// taps.*

type TapInfo struct {
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
}

type TapsListResult struct {
	Taps []TapInfo `json:"taps"`
}

type TapsReadParams struct {
	Names []string `json:"names,omitempty"`
}

type TapsReadResult struct {
	Values map[string]uint64 `json:"values"`
}

// bp.*

type BPSetParams struct {
	Addr uint16 `json:"addr"`
	Kind string `json:"kind,omitempty"`
}

type BPSetResult struct {
	ID   string `json:"id"`
	Addr uint16 `json:"addr"`
}

type BPSetVectorParams struct {
	Vector string `json:"vector"`
}

type BPSetVectorResult struct {
	ID     string `json:"id"`
	Vector string `json:"vector"`
}

type BPClearParams struct {
	ID string `json:"id,omitempty"`
}

type BPClearResult struct {
	Cleared int `json:"cleared"`
}

type Breakpoint struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Addr   uint16 `json:"addr,omitempty"`
	Vector string `json:"vector,omitempty"`
}

type BPListResult struct {
	Breakpoints []Breakpoint `json:"breakpoints"`
}

// clock.run / clock.stop (Phase 1c-2 async surface)

type ClockRunParams struct {
	SpeedHz *int `json:"speedHz,omitempty"`
}

type ClockStopResult struct {
	State  CPUState `json:"state"`
	Reason string   `json:"reason"`
}

// events.subscribe / unsubscribe

type EventsSubscribeParams struct {
	Channels []string `json:"channels"`
}

type EventsSubscribeResult struct {
	Subscribed []string `json:"subscribed"`
}

type EventsUnsubscribeParams struct {
	Channels []string `json:"channels,omitempty"`
}

type EventsUnsubscribeResult struct {
	Unsubscribed []string `json:"unsubscribed"`
}

// Server-pushed notification payloads. Each is the body of a
// JSON-RPC notification frame with the matching method name.

// BPHitPayload — method "bp.hit". Fired during an async run when an
// address breakpoint matches the SYNC boundary. ID is "" for vector
// breaks (the Instrument exposes only a single global toggle, so the
// bridge can't attribute the fire to a specific vector-bp id).
type BPHitPayload struct {
	ID    string   `json:"id,omitempty"`
	Addr  uint16   `json:"addr"`
	State CPUState `json:"state"`
}

// TapChangedPayload — method "tap.changed". Emitted when a tap's value
// differs from the prior slice and the per-channel coalescing budget
// (≈60 Hz, design §7) allows.
type TapChangedPayload struct {
	Name       string `json:"name"`
	Value      uint64 `json:"value"`
	HalfCycles uint64 `json:"halfCycles"`
}

// StateSnapshotPayload — method "state.snapshot". Emitted on the
// configured cadence (default 100 ms wall time) ONLY while subscribed
// to the "state" channel.
type StateSnapshotPayload struct {
	State CPUState          `json:"state"`
	Taps  map[string]uint64 `json:"taps,omitempty"`
}
