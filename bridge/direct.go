// HubDirect is the in-process counterpart to bridgeclient.Client.
// Same Target interface, no TCP / JSON-RPC framing — just direct
// Go calls into a Hub. Built for consumers that share the same
// process as the simulator (the sim TUI hosting a built-in Monitor;
// integration tests that want a real Hub without spinning a
// listener) but still want to speak the bridge protocol's
// vocabulary.
//
// Architectural note: server.go's TCP handlers and HubDirect's
// methods both do nearly the same work — taking QueryLock, calling
// Hub.Cmd* / Inst().*, returning typed results. Future consolidation
// would have server.go's handlers delegate to HubDirect (server =
// thin TCP shim over HubDirect). For now they evolve in parallel;
// adding a method here AND in server.go is a small acceptable cost
// for the architectural decoupling.
package bridge

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/carledwards/go6sim/cpu"
)

// HubDirect wraps a *Hub and exposes the bridge.Target interface
// over direct Go calls. Cheap to construct; a single Hub may back
// any number of HubDirects (typical case is one per local consumer).
type HubDirect struct {
	hub *Hub

	// notifyCh is the consumer-facing event stream. The HubDirect
	// subscribes to the Hub at construction; the subscriber callback
	// marshals payloads to JSON and forwards via notifyCh so the
	// shape matches the wire client (consumer code is identical).
	notifyCh chan Notification

	// subID is the Hub subscription id; saved so Close can clean up.
	subID int
}

// NewHubDirect returns a HubDirect bound to `hub`. The HubDirect
// subscribes to the Hub's event topics that real clients use
// (clock.halt / bp.hit / state.snapshot / tap.changed) and exposes
// them via Notifications(). Close to detach.
//
// notifyCapacity bounds the per-target buffer; 128 matches
// bridgeclient.Client's default. When full, new events are dropped
// (best-effort delivery — same semantics as the wire client).
func NewHubDirect(hub *Hub) *HubDirect {
	const notifyCapacity = 128
	d := &HubDirect{
		hub:      hub,
		notifyCh: make(chan Notification, notifyCapacity),
	}
	// Subscribe to every topic a Monitor cares about today. Marshaller
	// callback shapes payloads into the same JSON the wire emits, so
	// consumers see byte-identical events regardless of edge.
	id, _ := hub.Subscribe(
		[]string{"clock.halt", "bp.hit", "state.snapshot", "tap.changed"},
		func(method string, params any) {
			raw, err := json.Marshal(params)
			if err != nil {
				// A failed marshal of an internal payload is a bug;
				// drop silently rather than panic and tear down the
				// host process.
				return
			}
			n := Notification{Method: method, Params: raw}
			select {
			case d.notifyCh <- n:
			default:
				// Buffer full — drop. Same behaviour as the wire
				// client's notifyCh under back-pressure.
			}
		},
	)
	d.subID = id
	return d
}

// Close detaches the subscription and closes the notification
// channel so range-loop consumers exit cleanly. Idempotent.
func (d *HubDirect) Close() {
	d.hub.Unsubscribe(d.subID)
	close(d.notifyCh)
}

// Notifications returns the event channel. Matches bridge.Target.
func (d *HubDirect) Notifications() <-chan Notification { return d.notifyCh }

// --- queries ---

// CPUState returns a snapshot of the CPU's architectural state.
func (d *HubDirect) CPUState() (CPUState, *Error) {
	d.hub.QueryLock().Lock()
	defer d.hub.QueryLock().Unlock()
	return snapshot(d.hub.Inst()), nil
}

// MemPeek reads `n` bytes from `addr` through the bus.
func (d *HubDirect) MemPeek(addr uint16, n int) ([]byte, *Error) {
	if n <= 0 {
		n = 1
	}
	d.hub.QueryLock().Lock()
	defer d.hub.QueryLock().Unlock()
	end := int(addr) + n
	if end > 0x10000 {
		return nil, newErrData(CodeBusError, "mem.peek: range exceeds 64K",
			map[string]any{"addr": addr, "n": n})
	}
	buf := make([]byte, n)
	for i := 0; i < n; i++ {
		buf[i] = d.hub.Inst().Peek(addr + uint16(i))
	}
	return buf, nil
}

// BPList enumerates current breakpoints. HubDirect doesn't track
// session-scoped ids (those live in bridge.Session) — it surfaces
// raw addresses from the Hub instead, using a synthetic "bp_$XXXX"
// id format for client consistency.
func (d *HubDirect) BPList() (BPListResult, *Error) {
	// The Hub exposes BPs through the Instrument's bp set. We don't
	// have a direct accessor today, so use the typed wrapper path:
	// route through CmdBP* and inspect via the wire's bp.list shape.
	// Future Hub method (e.g., Hub.Breakpoints()) would let us skip
	// this — leaving a TODO for that consolidation.
	return BPListResult{}, &Error{
		Code:    CodeBusError,
		Message: "bp.list: not yet implemented on HubDirect (track in Hub)",
	}
}

// --- commands ---

// MemPoke writes bytes to memory via the bus.
func (d *HubDirect) MemPoke(addr uint16, b []byte) (int, *Error) {
	if int(addr)+len(b) > 0x10000 {
		return 0, newErrData(CodeBusError, "mem.poke: range exceeds 64K",
			map[string]any{"addr": addr, "n": len(b)})
	}
	d.hub.CmdMemPoke(addr, b)
	return len(b), nil
}

// Step advances n instructions and returns the post-step state.
// Mirrors server.go's clockStepHandler — including the host-IRQ /
// host-NMI auto-deassert so the idle-then-step pulse flow works.
func (d *HubDirect) Step(n int) (ClockStepResult, *Error) {
	if n <= 0 {
		n = 1
	}
	d.hub.QueryLock().Lock()
	d.hub.Inst().Step(n)
	d.hub.Inst().Backplane().AssertHostIRQ(false)
	d.hub.Inst().Backplane().AssertHostNMI(false)
	st := snapshot(d.hub.Inst())
	d.hub.QueryLock().Unlock()
	return ClockStepResult{State: st}, nil
}

// Run starts the clock at the given speed. speedHz=0 means "unpaced".
func (d *HubDirect) Run(speedHz int) *Error {
	d.hub.CmdRun(0, speedHz)
	return nil
}

// Stop halts the clock and aligns PC to the next SYNC boundary.
// Returns the post-halt CPU state along with the halt reason.
func (d *HubDirect) Stop() (ClockStopResult, *Error) {
	// Subscribe to clock.halt locally to capture the halt result the
	// Pump emits, since CmdStop is fire-and-forget.
	resultCh := make(chan ClockStopResult, 1)
	tempID, _ := d.hub.Subscribe([]string{"clock.halt"}, func(_ string, params any) {
		if r, ok := params.(RunResult); ok {
			select {
			case resultCh <- ClockStopResult{
				Reason: r.Reason,
				State:  r.State,
			}:
			default:
			}
		}
	})
	defer d.hub.Unsubscribe(tempID)
	d.hub.CmdStop()
	return <-resultCh, nil
}

// Reset re-initialises the CPU + clock and returns the post-reset
// state. Hub.CmdReset is synchronous.
func (d *HubDirect) Reset() (CPUState, *Error) {
	d.hub.CmdReset()
	d.hub.QueryLock().Lock()
	defer d.hub.QueryLock().Unlock()
	return snapshot(d.hub.Inst()), nil
}

// SetPC overwrites the program counter (interp only).
func (d *HubDirect) SetPC(pc uint16) (CPUState, *Error) {
	if err := d.hub.CmdSetPC(pc); err != nil {
		return CPUState{}, &Error{Code: CodeBusError, Message: err.Error()}
	}
	d.hub.QueryLock().Lock()
	defer d.hub.QueryLock().Unlock()
	return snapshot(d.hub.Inst()), nil
}

// BPSet arms an address breakpoint. HubDirect uses a synthetic id
// based on the address; multi-client uniqueness isn't a concern
// here (HubDirect is per-process).
func (d *HubDirect) BPSet(addr uint16) (BPSetResult, *Error) {
	d.hub.CmdBPSet(addr)
	return BPSetResult{
		ID:   fmt.Sprintf("bp_%04X", addr),
		Addr: addr,
	}, nil
}

// BPClear clears one or all breakpoints. The id matches the synthetic
// "bp_$XXXX" form BPSet returns; "" or "all" clears everything.
func (d *HubDirect) BPClear(id string) (BPClearResult, *Error) {
	if id == "" || id == "all" {
		d.hub.CmdBPClearAll()
		return BPClearResult{Cleared: -1}, nil // unknown-count sentinel
	}
	// Parse the synthetic id back to an address.
	var addr uint16
	if _, err := fmt.Sscanf(id, "bp_%04X", &addr); err != nil {
		return BPClearResult{}, &Error{
			Code:    CodeInvalidParams,
			Message: fmt.Sprintf("bp.clear: bad id %q (want bp_XXXX)", id),
		}
	}
	d.hub.CmdBPClear(addr)
	return BPClearResult{Cleared: 1}, nil
}

// IRQ pulses the host-driven IRQ line.
func (d *HubDirect) IRQ() *Error {
	d.hub.CmdIRQ()
	return nil
}

// NMI pulses the host-driven NMI line (interp only).
func (d *HubDirect) NMI() *Error {
	d.hub.CmdNMI()
	return nil
}

// Call is the escape hatch for unimplemented methods. Today
// HubDirect doesn't dispatch arbitrary methods — anyone needing
// bp.setVector or machine.list calls those out-of-band on the Hub
// directly. Returns a clear error so callers know they hit a gap.
func (d *HubDirect) Call(method string, params any) (json.RawMessage, *Error) {
	_ = params
	return nil, &Error{
		Code:    CodeBusError,
		Message: "HubDirect.Call: method " + method + " has no in-process handler yet",
	}
}

// --- compile-time interface assertion ---

// Verify *HubDirect satisfies bridge.Target — adding new Target
// methods breaks the build here until HubDirect grows the matching
// implementation.
var _ Target = (*HubDirect)(nil)

// silence-imports keeps the explicit cpu / base64 / errors imports
// from being marked unused while we wire methods in over time —
// they're cheap to keep available because HubDirect WILL grow to
// need them (cpu.PCSetter type assertion, base64 in image.load,
// errors for wrapped Hub errors).
var (
	_ = (*cpu.Backend)(nil)
	_ = base64.StdEncoding
	_ = errors.New
)
