// Package remote implements a cpu.Backend that drives a CPU running
// over the network instead of locally. The TUI binds a WebSocket
// listener on `/cpu`; a remote CPU host (Go binary, browser wasm with
// visual6502, eventually maybe a real FPGA-hosted 6502) dials in and
// becomes the active CPU. The local sim still owns RAM/ROM/peripherals
// — the bus is shipped across the wire one access at a time inside
// each half-step.
//
// Topology rationale: browsers can only open WebSocket connections,
// not accept them, so the TUI must be the server even though the
// "resource" conceptually lives on the remote side.
//
// Latency budget: with ~10 ms RTT, every half-step is 1 + 2N messages
// (N = bus accesses per cycle), capping the effective rate around
// 20–50 Hz. That's slow, but it's enough for "watch the silicon
// think" demos — Bouncer at 1 kHz here feels similar to netsim at
// its own ceiling. Higher rates would need batching / speculation
// (see docs in commit history).
package remote

import "encoding/json"

// msgType discriminates the wire messages. Each direction has its
// own message set; the discriminator field keeps them parseable on
// either end without separate envelopes.
const (
	// TUI → CPU
	MsgReset     = "reset"     // please reset
	MsgHalfStep  = "halfStep"  // advance one half-cycle
	MsgRegisters = "registers" // please send your register state
	MsgBusData   = "busData"   // here's the data you asked to read
	MsgBusAck    = "busAck"    // your write landed
	MsgBye       = "bye"       // closing — clean shutdown

	// CPU → TUI
	MsgHello        = "hello"        // initial handshake (CPU advertises self)
	MsgBusRead      = "busRead"      // please read this address
	MsgBusWrite     = "busWrite"     // please write this byte
	MsgHalfStepDone = "halfStepDone" // halfStep complete, pin state attached
	MsgResetDone    = "resetDone"    // reset complete
	MsgRegsResp     = "regsResp"     // here are my registers
)

// Msg is the union of every wire message. Optional fields are JSON-
// omitempty so a small message (e.g. a busRead) wires as ~30 bytes.
// Single struct beats one-type-per-message for v1: less ceremony,
// easier to log, and the protocol is small enough that the unused
// fields aren't a maintenance burden.
type Msg struct {
	Type string `json:"type"`

	// Bus op (busRead / busWrite / busData):
	Addr uint16 `json:"addr,omitempty"`
	Data uint8  `json:"data,omitempty"`

	// halfStepDone pin snapshot. Captures everything the local
	// cpu.Backend accessors need to return between half-steps, so
	// AddressBus / DataBus / ReadCycle / IRQ / NMI / SYNC are all
	// O(1) cache reads on the TUI side instead of more roundtrips.
	BusAddr uint16 `json:"busAddr,omitempty"`
	BusData uint8  `json:"busData,omitempty"`
	RW      bool   `json:"rw,omitempty"`
	SYNC    bool   `json:"sync,omitempty"`
	IRQ     bool   `json:"irq,omitempty"`
	NMI     bool   `json:"nmi,omitempty"`

	// Registers payload (regsResp):
	A  uint8  `json:"a,omitempty"`
	X  uint8  `json:"x,omitempty"`
	Y  uint8  `json:"y,omitempty"`
	S  uint8  `json:"s,omitempty"`
	P  uint8  `json:"p,omitempty"`
	PC uint16 `json:"pc,omitempty"`

	// hello payload:
	Role string `json:"role,omitempty"` // "cpu"
	Kind string `json:"kind,omitempty"` // "interp", "visual6502", ...
}

// Encode marshals a message to its wire form (one JSON line).
func Encode(m Msg) ([]byte, error) { return json.Marshal(m) }

// Decode parses a wire message back into Msg.
func Decode(b []byte, out *Msg) error { return json.Unmarshal(b, out) }
