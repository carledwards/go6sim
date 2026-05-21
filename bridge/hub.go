package bridge

// The Hub is the v2 keystone (see docs/bridge-v2.md). It binds one
// Instrument to a single Pump goroutine and a fan-out subscription
// registry. Commanders post commands; the Pump drains and acts on
// them. Subscribers register interest in topics; the Pump emits via
// the Hub's broadcast.
//
// Phase A scope: just the foundation — Hub + Pump + subscription
// registry + the execution commands (run/stop/step/setSpeed/bp.*).
// Only one topic emitted today: clock.halt. The rest of the topic
// catalog (instr.executed, bp.hit, tap.changed, state.snapshot,
// irq.taken, region.elided) lands in Phase B; the Hub's broadcast
// surface is already shaped for them — they're just additional
// emit sites in the Pump loop.
//
// Concurrency contract:
//   - Pump goroutine is the sole mutator of inst.
//   - Commanders send hubCommand values into commandsCh; they never
//     touch inst directly.
//   - Subscribers receive events via their send func; they never
//     touch inst either (queries go through a separate read path
//     under hub.QueryLock — see Phase A-2 in server.go).
//   - One mutex (subMu) guards the subscriber registry; never held
//     across emits to avoid deadlocks with slow subscribers.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/carledwards/go6sim/cpu"
	"github.com/carledwards/go6sim/instrument"
)

// Hub owns one Instrument's pump + subscriptions. New via NewHub;
// Run blocks until ctx is cancelled (call as a goroutine).
type Hub struct {
	inst    *instrument.Instrument
	regions []Region
	preset  string

	commandsCh chan hubCommand

	subMu       sync.RWMutex
	nextSubID   int
	subscribers map[int]*subscriber

	// queryMu serialises queries (peek/state/etc.) with the Pump's
	// mutations. Callers issuing queries from outside the Pump take
	// it for the duration of an inst read. Pump takes it inside its
	// slice critical section.
	queryMu sync.Mutex

	done chan struct{} // closed when Run returns
}

// subscriber tracks one client's topic interest. send is the caller-
// supplied emit closure (typically: marshal + Conn.Write under the
// session's writeMu).
type subscriber struct {
	id     int
	topics map[string]bool
	send   func(method string, params any)
}

// hubCommand is the unit the Pump drains. Phase A uses one closure-
// per-command for terseness; future phases may switch to a typed-
// payload struct if we need command introspection/logging. Each
// closure runs synchronously on the Pump goroutine and may mutate
// pumpState freely.
type hubCommand func(p *Pump)

// NewHub binds an Instrument to a fresh Hub. The Pump isn't running
// yet — call Run(ctx) in its own goroutine to start it. preset and
// regions are informational; the Hub passes them through to query
// callers (image.describe / mem.read by region).
func NewHub(inst *instrument.Instrument, regions []Region, preset string) *Hub {
	return &Hub{
		inst:        inst,
		regions:     regions,
		preset:      preset,
		commandsCh:  make(chan hubCommand, 64),
		subscribers: map[int]*subscriber{},
		done:        make(chan struct{}),
	}
}

// Inst exposes the wrapped Instrument. Queries should take QueryLock
// before reading.
func (h *Hub) Inst() *instrument.Instrument { return h.inst }

// Regions returns the live memory map (read-only — do not mutate).
func (h *Hub) Regions() []Region { return h.regions }

// Preset returns the bound preset name (informational).
func (h *Hub) Preset() string { return h.preset }

// QueryLock returns the mutex that serialises external reads against
// the Pump's writes. Use as:
//
//	hub.QueryLock().Lock()
//	st := hub.Inst().State()
//	hub.QueryLock().Unlock()
//
// The Pump takes it for the duration of its slice work; queries
// briefly block during a slice but never starve.
func (h *Hub) QueryLock() *sync.Mutex { return &h.queryMu }

// Done is closed when Run returns (ctx cancelled).
func (h *Hub) Done() <-chan struct{} { return h.done }

// Subscribe registers a subscriber for the given topics. Returns the
// subscriber id (use for later Unsubscribe / UnsubscribeAll) and the
// list of topics the Hub accepted (unknown topics are silently
// dropped — clients diff this against what they asked for to detect
// unsupported topics, per design §13).
//
// `send` is the function the Pump calls to emit events to this
// subscriber. It must be safe to invoke from the Pump goroutine; the
// session implementation typically marshals + Writes under its own
// writeMu.
func (h *Hub) Subscribe(topics []string, send func(method string, params any)) (id int, accepted []string) {
	h.subMu.Lock()
	defer h.subMu.Unlock()
	h.nextSubID++
	id = h.nextSubID
	tm := map[string]bool{}
	for _, t := range topics {
		if !validTopic(t) {
			continue
		}
		tm[t] = true
		accepted = append(accepted, t)
	}
	h.subscribers[id] = &subscriber{id: id, topics: tm, send: send}
	return id, accepted
}

// AddTopics extends a subscriber's topic set in place. Returns the
// list of topics that were newly accepted.
func (h *Hub) AddTopics(id int, topics []string) (accepted []string) {
	h.subMu.Lock()
	defer h.subMu.Unlock()
	sub, ok := h.subscribers[id]
	if !ok {
		return nil
	}
	for _, t := range topics {
		if !validTopic(t) || sub.topics[t] {
			continue
		}
		sub.topics[t] = true
		accepted = append(accepted, t)
	}
	return accepted
}

// RemoveTopics clears specific topics from a subscriber. Empty list =
// clear all the subscriber's topics.
func (h *Hub) RemoveTopics(id int, topics []string) (removed []string) {
	h.subMu.Lock()
	defer h.subMu.Unlock()
	sub, ok := h.subscribers[id]
	if !ok {
		return nil
	}
	if len(topics) == 0 {
		for t := range sub.topics {
			removed = append(removed, t)
		}
		sub.topics = map[string]bool{}
		return removed
	}
	for _, t := range topics {
		if sub.topics[t] {
			delete(sub.topics, t)
			removed = append(removed, t)
		}
	}
	return removed
}

// Unsubscribe removes a subscriber entirely. Idempotent.
func (h *Hub) Unsubscribe(id int) {
	h.subMu.Lock()
	delete(h.subscribers, id)
	h.subMu.Unlock()
}

// IssueCommand enqueues a command for the Pump. Returns immediately
// (commands are fire-and-forget per the v2 design). The Pump will
// drain it on its next iteration.
//
// Callers that want a synchronous result use a one-shot channel
// inside their closure (see typed wrappers below).
func (h *Hub) IssueCommand(cmd hubCommand) {
	select {
	case h.commandsCh <- cmd:
	case <-h.done:
		// Hub stopped; drop. Callers can detect via Done().
	}
}

// broadcast fans an event out to every subscriber that registered
// the topic. Slow consumers see their send invoked synchronously —
// the session-side send is responsible for being non-blocking
// (typically by writing to a buffered channel or dropping under
// pressure). Held under RLock so concurrent Subscribe/Unsubscribe
// don't race; the lock is released before each send to avoid
// holding it across a slow consumer.
func (h *Hub) broadcast(topic string, params any) {
	h.subMu.RLock()
	targets := make([]*subscriber, 0, len(h.subscribers))
	for _, sub := range h.subscribers {
		if sub.topics[topic] {
			targets = append(targets, sub)
		}
	}
	h.subMu.RUnlock()
	for _, sub := range targets {
		sub.send(topic, params)
	}
}

// validTopic returns whether `t` is a topic the Hub knows. Phase A
// recognises only clock.halt; further topics are added by name as
// each phase lands.
func validTopic(t string) bool {
	switch t {
	case "clock.halt", "bp.hit", "state.snapshot":
		return true
	}
	return false
}

// --- Pump ---

// Pump is the per-Hub goroutine state. Fields are owned by the Pump
// goroutine; commands mutate them through hubCommand closures.
type Pump struct {
	h *Hub

	running        bool
	stepRemaining  int    // when > 0: halt after this many half-cycles, reason "step"
	runUntilBudget int    // when > 0: halt after this many half-cycles, reason "limit"
	consumedTotal  uint64 // running tally toward stepRemaining / runUntilBudget

	// lastStateSnapshot bounds how often state.snapshot fires while
	// the pump is running — ~10 Hz is plenty for a "CPU panel
	// animates" experience without flooding slow consumers.
	lastStateSnapshot time.Time

	// hostIRQClearAt is when, in wall-clock virtual time, to drop
	// the debugger-driven IRQ line. Set by CmdIRQ; cleared after
	// the Pump's next slice (i.e., once the CPU has had a chance to
	// sample SYNC and service the interrupt). Zero means "no pending
	// pulse."
	hostIRQClear bool

	// hostNMIClear is the analogous flag for the host-NMI line.
	// Auto-deassert is critical for NMI: it's edge-triggered, so
	// the line must drop after one service or the next pulse won't
	// be observable.
	hostNMIClear bool
}

const stateSnapshotInterval = 100 * time.Millisecond

// Run is the Pump's main loop. Cancel ctx to stop. Closes hub.done
// before returning.
//
// Peripheral ticking has TWO drivers depending on whether the Pump
// is running or idle:
//
//   - Running: each slice does `RunUntil(N) + Tick(virtualDt)` lockstep
//     (peripherals see virtual time proportional to CPU half-cycles).
//   - Idle: a heartbeat ticks peripherals at WALL-CLOCK rate so the
//     "1 MHz crystal runs continuously" intent holds — VIA timers
//     and other Tickers keep counting between user keystrokes / steps,
//     matching the original cmd/6502-sim behaviour.
//
// The heartbeat virtual-dt equals real elapsed time (≈ heartbeatPeriod)
// so the crystal advances at ~1 MHz virtual when peripherals are
// configured at 1 MHz — independent of CPU run state.
func (h *Hub) Run(ctx context.Context) {
	defer close(h.done)
	p := &Pump{h: h}
	const sliceHalvesMax = 200
	const heartbeatPeriod = 16 * time.Millisecond
	// Target wall-clock per slice when pacing. Keeps the UI redraw
	// budget around 60 Hz and means commands (Stop, Step) are at
	// most one slice late even at low Hz.
	const sliceWallTarget = 16 * time.Millisecond

	for {
		// 1. Drain any pending commands without blocking. Hold
		//    queryMu around each command — closures may mutate CPU
		//    state (FinishInstruction's HalfSteps, SetPC, Reset) or
		//    backplane lines, and external readers via Hub.QueryLock
		//    must serialize against those writes. Subscriber
		//    callbacks fired from broadcastState/Halt inside a
		//    command must NOT re-take queryMu (none do today; the
		//    session subscriber only writes to the connection).
		drained := false
		for !drained {
			select {
			case cmd := <-h.commandsCh:
				h.queryMu.Lock()
				cmd(p)
				h.queryMu.Unlock()
			case <-ctx.Done():
				return
			default:
				drained = true
			}
		}

		// 2. If idle, heartbeat-tick peripherals while waiting for a
		//    command. Crystal keeps running; ISR work doesn't happen
		//    (no CPU advancement) but timer counters/IFR bits update.
		if !p.running {
			select {
			case cmd := <-h.commandsCh:
				h.queryMu.Lock()
				cmd(p)
				h.queryMu.Unlock()
			case <-time.After(heartbeatPeriod):
				h.queryMu.Lock()
				h.inst.Tick(heartbeatPeriod)
				h.queryMu.Unlock()
			case <-ctx.Done():
				return
			}
			continue
		}

		// 3. Compute the slice budget. With a configured speed we
		//    chunk so each slice covers ~sliceWallTarget of wall time
		//    at the target Hz; unlimited (hz <= 0) uses the flat cap.
		configuredHz := h.inst.Driver().Speed().Hz
		sliceHalves := sliceHalvesMax
		if configuredHz > 0 {
			s := int(int64(2*configuredHz) *
				int64(sliceWallTarget) / int64(time.Second))
			switch {
			case s < 2:
				s = 2
			case s > sliceHalvesMax:
				s = sliceHalvesMax
			}
			sliceHalves = s
		}

		// 4. Advance a slice, holding queryMu so concurrent queries
		//    see a consistent inst state.
		sliceStart := time.Now()
		h.queryMu.Lock()
		r := h.inst.RunUntil(sliceHalves)
		// Tick peripherals for the same virtual time the CPU advanced;
		// RunUntil is CPU-only by design (see instrument.RunUntil).
		hz := configuredHz
		if hz <= 0 {
			hz = 1_000_000
		}
		virtualDt := time.Duration(r.HalfCycles) *
			time.Second / time.Duration(2*hz)
		h.inst.Tick(virtualDt)
		h.queryMu.Unlock()

		p.consumedTotal += r.HalfCycles

		// 4b. Auto-deassert the host-IRQ line after the slice in
		//     which it was raised. The CPU samples IRQ at every
		//     instruction boundary (SYNC); a single slice contains
		//     many boundaries unless the slice ends mid-instruction,
		//     so one slice is enough for the IRQ to fire if I-flag
		//     is clear. Holding it forever would re-fire on every
		//     boundary, which is rarely what the debugger wants.
		if p.hostIRQClear {
			h.inst.Backplane().AssertHostIRQ(false)
			p.hostIRQClear = false
		}
		if p.hostNMIClear {
			h.inst.Backplane().AssertHostNMI(false)
			p.hostNMIClear = false
		}

		// 5. Periodic state.snapshot emission while running, gated
		//    by stateSnapshotInterval so slow consumers aren't
		//    drowned. Subscribers get a steady-cadence view of CPU
		//    + taps; the controller's CPU panel animates.
		if time.Since(p.lastStateSnapshot) >= stateSnapshotInterval {
			h.broadcastState()
			p.lastStateSnapshot = time.Now()
		}

		// 6. Halt conditions. Evaluated BEFORE pacing-sleep so the
		//    halt event isn't delayed by a sleep window.
		if r.Reason != "budget" {
			// bp or vector — pump-side halt.
			reason := translateRunReason(r.Reason)
			p.running = false
			p.stepRemaining = 0
			p.runUntilBudget = 0
			h.broadcastHalt(reason, r)
			continue
		}
		if p.stepRemaining > 0 && p.consumedTotal >= uint64(p.stepRemaining) {
			p.running = false
			p.stepRemaining = 0
			h.broadcastHalt("step", r)
			continue
		}
		if p.runUntilBudget > 0 && p.consumedTotal >= uint64(p.runUntilBudget) {
			p.running = false
			p.runUntilBudget = 0
			h.broadcastHalt("limit", r)
			continue
		}

		// 7. Pace: if a finite speed is configured, sleep enough
		//    wall-clock so the slice consumed `virtualDt` of real
		//    time. Sleep is select-interruptible so Stop/Step/Reset
		//    don't wait out the full window — critical at 1–10 Hz
		//    where virtualDt is hundreds of ms per slice.
		if configuredHz > 0 {
			remaining := virtualDt - time.Since(sliceStart)
			if remaining > 0 {
				timer := time.NewTimer(remaining)
				select {
				case cmd := <-h.commandsCh:
					if !timer.Stop() {
						<-timer.C
					}
					cmd(p)
				case <-timer.C:
				case <-ctx.Done():
					if !timer.Stop() {
						<-timer.C
					}
					return
				}
			}
		}
	}
}

// broadcastState fans a state.snapshot event out to subscribers
// registered on the "state.snapshot" topic. Cheap (one Instrument
// State copy + a Taps map clone); callers gate the cadence.
func (h *Hub) broadcastState() {
	st := snapshot(h.inst)
	taps := copyTaps(h.inst.Taps())
	h.broadcast("state.snapshot", StateSnapshotPayload{State: st, Taps: taps})
}

func copyTaps(m map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (h *Hub) broadcastHalt(reason string, r instrument.RunResult) {
	st := snapshot(h.inst)
	addr := st.PC
	if r.Reason != "" {
		addr = r.Addr
	}
	// Phase A keeps v1 wire compat: when a breakpoint causes the
	// halt, also emit a bp.hit. The session's subscriber callback
	// fills in the session-scoped bp id on the way to the wire
	// (the Hub doesn't track ids; see server.go machineLoadHandler).
	if r.Reason == "breakpoint" {
		h.broadcast("bp.hit", BPHitPayload{Addr: r.Addr, State: st})
	}
	h.broadcast("clock.halt", RunResult{
		HalfCycles: r.HalfCycles,
		Reason:     reason,
		Addr:       addr,
		State:      st,
	})
}

// --- Typed command wrappers (commands the Pump understands) ---

// CmdRun enqueues a fire-and-forget run command. The Pump starts
// advancing immediately; halt is reported via the clock.halt topic
// (subscribers receive it; the caller does not block).
//
// untilHalves > 0 caps the run at that many half-cycles ("limit"
// halt). 0 = run indefinitely (until bp / vector / external stop).
// speedHz > 0 overrides the driver's current speed (= clock.setSpeed
// implicitly); 0 leaves it untouched.
func (h *Hub) CmdRun(untilHalves int, speedHz int) {
	h.IssueCommand(func(p *Pump) {
		if speedHz > 0 {
			_ = p.h.inst.Driver().SetSpeedHz(speedHz)
		}
		p.running = true
		p.consumedTotal = 0
		p.runUntilBudget = untilHalves
		p.stepRemaining = 0
		p.lastStateSnapshot = time.Time{} // force immediate periodic emit next slice
		p.h.inst.SetRunning(true)
		// Edge-trigger emit: tell subscribers "running started now."
		p.h.broadcastState()
	})
}

// CmdStop enqueues a stop command. Pump halts at next slice
// boundary and emits clock.halt { reason: "client" }.
//
// Note: free-run slices are 200 half-cycles, so the slice nearly
// always ends mid-instruction. We finish the in-flight instruction
// before broadcasting halt so PC lands on a clean SYNC boundary —
// without this, the user's first step after stop only completes the
// previous instruction's last few half-cycles instead of executing
// a new one, and they perceive "I had to step twice."
func (h *Hub) CmdStop() {
	h.IssueCommand(func(p *Pump) {
		if !p.running {
			return
		}
		p.running = false
		p.runUntilBudget = 0
		p.stepRemaining = 0
		p.h.inst.SetRunning(false)
		p.h.inst.FinishInstruction()
		p.h.broadcastHalt("client", instrument.RunResult{})
	})
}

// CmdStep enqueues a step command. kind ∈ {"instruction","halfcycle"}.
// n must be > 0. Pump runs at most `n` units of the named granularity
// and emits clock.halt { reason: "step" }.
//
// Phase A: "instruction" granularity is approximated as ~2
// half-cycles/step (mean 6502 instruction is 3 cycles = 6 half-
// cycles; we cap at the lower bound to favour responsiveness). A
// more precise implementation tracks SYNC boundaries directly; later
// phase.
func (h *Hub) CmdStep(kind string, n int) {
	if n <= 0 {
		n = 1
	}
	halves := n
	if kind == "instruction" {
		halves = n * 6 // approximate; refined in Phase B
	}
	h.IssueCommand(func(p *Pump) {
		p.running = true
		p.consumedTotal = 0
		p.stepRemaining = halves
		p.runUntilBudget = 0
		p.lastStateSnapshot = time.Time{}
		p.h.inst.SetRunning(true)
		p.h.broadcastState()
	})
}

// CmdIRQ pulses the host-driven IRQ line: assert immediately + flag
// the next slice to auto-deassert. The CPU samples IRQ at every
// instruction boundary; if the I flag is clear the next boundary
// services the vector via $FFFE. If interrupts are masked (I=1) the
// pulse goes unanswered — that's correct 6502 behaviour.
//
// Fire-and-forget; the controller observes the effect via the next
// state.snapshot (PC jumps to the IRQ handler).
//
// Caller note: if the CPU isn't currently running, the pulse will
// still take effect on the next Step or Run, because IRQ assertion
// is level-sensitive and remains until the auto-deassert in the
// next slice. For a "step-into-IRQ" workflow, pulse then step.
func (h *Hub) CmdIRQ() {
	h.IssueCommand(func(p *Pump) {
		p.h.inst.Backplane().AssertHostIRQ(true)
		p.hostIRQClear = true
		// Snapshot so subscribers see the pulse without waiting for
		// the next periodic emit.
		p.h.broadcastState()
	})
}

// CmdSetPC overwrites the CPU's program counter on the Pump
// goroutine, then broadcasts a fresh state.snapshot so subscribers
// see the new PC immediately. Returns the post-set state. Errors if
// the backend doesn't implement cpu.PCSetter (netsim).
//
// Safe to call while the CPU is running — the next instruction
// boundary will fetch from the new PC. The current instruction (if
// mid-execution) completes normally first.
func (h *Hub) CmdSetPC(pc uint16) error {
	done := make(chan error, 1)
	h.IssueCommand(func(p *Pump) {
		setter, ok := p.h.inst.Driver().Backend.(cpu.PCSetter)
		if !ok {
			done <- fmt.Errorf("cpu.setPC: backend does not support PC override (try interp CPU)")
			return
		}
		setter.SetPC(pc)
		p.h.broadcastState()
		done <- nil
	})
	return <-done
}

// CmdNMI pulses the host-driven NMI line: assert immediately + flag
// the next slice to auto-deassert. NMI is edge-triggered (rising
// edge) and ignores the I-flag — even with interrupts masked, the
// CPU services on the next instruction boundary. Vectors through
// $FFFA.
//
// Fire-and-forget; controller observes the effect via state.snapshot
// (PC jumps to the NMI handler) or clock.halt if the CPU was halted.
//
// Note: netsim CPU mode does NOT honour this yet — the upstream
// netsim-go module doesn't expose SetNMI. interp mode services
// normally.
func (h *Hub) CmdNMI() {
	h.IssueCommand(func(p *Pump) {
		p.h.inst.Backplane().AssertHostNMI(true)
		p.hostNMIClear = true
		p.h.broadcastState()
	})
}

// CmdSetSpeed updates the driver's clock speed. Returns synchronously
// via ack (the only command that does — clients want to know if the
// hz they sent was honored).
func (h *Hub) CmdSetSpeed(speedHz int) error {
	done := make(chan error, 1)
	h.IssueCommand(func(p *Pump) {
		if ok := p.h.inst.Driver().SetSpeedHz(speedHz); !ok {
			done <- fmt.Errorf("clock.setSpeed: hz=%d not in supported set", speedHz)
			return
		}
		done <- nil
	})
	return <-done
}

// CmdBPSet arms an address breakpoint synchronously, returning a
// session-independent id (caller in server.go layers session-scoped
// ids on top). Phase A keeps bp state on the Instrument directly.
func (h *Hub) CmdBPSet(addr uint16) {
	h.IssueCommand(func(p *Pump) {
		p.h.inst.SetBreakpoint(addr)
	})
}

// CmdBPClear clears one address breakpoint.
func (h *Hub) CmdBPClear(addr uint16) {
	h.IssueCommand(func(p *Pump) {
		p.h.inst.ClearBreakpoint(addr)
	})
}

// CmdBPClearAll clears every address bp + the vector-take toggle.
func (h *Hub) CmdBPClearAll() {
	h.IssueCommand(func(p *Pump) {
		p.h.inst.ClearBreakpoints()
		p.h.inst.BreakOnVector(false)
	})
}

// CmdBPSetVector toggles BreakOnVector on (Instrument has no per-
// vector filtering today; server.go layers the per-id bookkeeping).
func (h *Hub) CmdBPSetVector() {
	h.IssueCommand(func(p *Pump) {
		p.h.inst.BreakOnVector(true)
	})
}

// CmdBPClearVector toggles BreakOnVector off.
func (h *Hub) CmdBPClearVector() {
	h.IssueCommand(func(p *Pump) {
		p.h.inst.BreakOnVector(false)
	})
}

// CmdReset performs a shallow machine reset on the pump goroutine:
// stops any in-flight run, resets the CPU via the clock driver
// (re-fetches the reset vector), zeroes the pump's run-state. Does
// NOT clear RAM / VIA state — that's a deeper "machine restart"
// the caller composes via additional IssueCommand closures.
// Synchronous ack so callers (TUI keyboard / bridge handlers) can
// return the post-reset state.
func (h *Hub) CmdReset() {
	done := make(chan struct{}, 1)
	h.IssueCommand(func(p *Pump) {
		p.running = false
		p.stepRemaining = 0
		p.runUntilBudget = 0
		p.consumedTotal = 0
		p.h.inst.SetRunning(false)
		p.h.inst.Driver().Reset()
		// Tell every subscriber the machine moved. Reset == an
		// orderly halt that re-anchors PC at the reset vector;
		// reuse clock.halt with reason="reset" so existing client
		// halt-handlers (mirror refresh, event log, etc.) just work.
		p.h.broadcastHalt("reset", instrument.RunResult{})
		done <- struct{}{}
	})
	<-done
}

// CmdMemPoke writes bytes to memory through the traced bus (devices
// see it). Synchronous ack so the server can return Written count
// reliably.
func (h *Hub) CmdMemPoke(addr uint16, b []byte) {
	done := make(chan struct{}, 1)
	h.IssueCommand(func(p *Pump) {
		for i, v := range b {
			p.h.inst.Poke(addr+uint16(i), v)
		}
		done <- struct{}{}
	})
	<-done
}
