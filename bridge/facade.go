// This file defines the Go interface that the bridge protocol
// satisfies. Anything that can drive a 6502 (TCP client over the wire,
// an in-process Hub adapter, a future MCP shim translating tools into
// these methods) implements Target; anything that USES a 6502 (the
// Monitor REPL, IDE plugins, LLM-driven sessions) consumes it.
//
// The motivation is the "edges" architecture (see
// docs/architecture-backplane.md + the memory note
// learn-integration-architecture): one headless Machine, multiple
// edges talking to it through the same vocabulary. Target IS that
// vocabulary expressed in Go. The wire protocol is one encoding of
// Target; in-process Go calls are another. Both must keep the same
// shape — that's the whole point of declaring it as an interface
// rather than baking transport-specific assumptions into consumers.
package bridge

import "encoding/json"

// Notification is one server-pushed event. Matches the JSON-RPC
// notification frame on the wire; in-process targets (HubDirect)
// marshal payloads to json.RawMessage so consumers don't have to
// branch on transport.
type Notification struct {
	Method string
	Params json.RawMessage
}

// Target is the Go shape of the bridge protocol — the contract every
// 6502-driving edge must implement. Method names mirror the wire
// methods (cpu.state, mem.peek, etc.) for easy correspondence.
//
// Method receivers MUST be safe to call concurrently with whatever
// the implementation does internally (the wire client guards its own
// I/O; HubDirect takes the Hub's QueryLock as needed). Errors return
// *Error so wire and direct consumers see the same shape.
//
// Methods are grouped by spirit: queries are synchronous reads with
// no side effects; commands are synchronous-ack mutations; the
// notification channel is the asynchronous server→client stream.
type Target interface {
	// --- queries (synchronous, side-effect-free) ---

	// CPUState returns a snapshot of CPU registers, half-cycle count,
	// and whether the clock is currently running. Cheap.
	CPUState() (CPUState, *Error)

	// MemPeek reads `n` bytes from `addr` through the bus, so
	// peripheral registers respond as if read by the CPU.
	MemPeek(addr uint16, n int) ([]byte, *Error)

	// BPList returns every armed breakpoint on the current session.
	BPList() (BPListResult, *Error)

	// --- commands (mutations, synchronous ack) ---

	// MemPoke writes bytes to memory via the bus (devices observe).
	MemPoke(addr uint16, b []byte) (int, *Error)

	// Step advances n full instructions. Peripherals tick
	// proportionally. No-op while the clock is running.
	Step(n int) (ClockStepResult, *Error)

	// Run starts the clock; speedHz=0 selects "max" (unpaced).
	Run(speedHz int) *Error

	// Stop halts the clock and aligns PC to the next instruction
	// boundary so a subsequent Step lands cleanly.
	Stop() (ClockStopResult, *Error)

	// Reset re-fetches the reset vector, halts the clock, and
	// returns the post-reset CPU state.
	Reset() (CPUState, *Error)

	// SetPC overwrites the program counter. Backend must support
	// cpu.PCSetter (interp does; netsim doesn't) — error otherwise.
	SetPC(pc uint16) (CPUState, *Error)

	// BPSet arms an address breakpoint.
	BPSet(addr uint16) (BPSetResult, *Error)

	// BPClear clears one breakpoint by id, or all when id is "".
	BPClear(id string) (BPClearResult, *Error)

	// IRQ pulses the host-driven IRQ line (auto-deasserts; honored
	// only when the I flag is clear).
	IRQ() *Error

	// NMI pulses the host-driven NMI line (edge-triggered; ignores
	// the I flag; interp only).
	NMI() *Error

	// --- escape hatch ---

	// Call is the raw JSON-RPC-shaped accessor for methods that
	// don't have a typed wrapper yet (bp.setVector, machine.list,
	// future additions). Same signature as the wire's Call so the
	// Monitor's remaining hand-written calls keep working.
	Call(method string, params any) (json.RawMessage, *Error)

	// --- events ---

	// Notifications is the server→client event stream. Closes when
	// the underlying transport / Hub dies; consumers detect death by
	// the range loop exiting. Always non-nil while the target is
	// alive.
	Notifications() <-chan Notification
}
