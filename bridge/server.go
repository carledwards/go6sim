package bridge

// Phase A-2 of the v2 pivot (see docs/bridge-v2.md). The Server's
// per-session handlers no longer drive a per-session runner
// goroutine; instead, each session owns a Hub (one per Instrument)
// and routes commands through Hub.Cmd* + queries through
// Hub.QueryLock. The v1 wire surface is preserved so existing
// clients (cmd/6502-control, internal/bridgeclient) continue to
// work — execution semantics shift to v2 (fire-and-forget commands,
// notifications via the Hub's broadcast).
//
// Phase A-2 still binds one Hub per session — fine for the headless
// case where each connection wants a fresh machine. Phase C will
// promote the Hub-per-Instrument model the v2 doc describes (TUI
// shares its live Hub with attached clients).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/carledwards/go6sim/clock"
	"github.com/carledwards/go6sim/instrument"
)

// Loader builds (or fetches) a Hub for a named preset. v2 phase A-4
// — Loaders now return a Hub directly rather than a raw Instrument,
// so the same interface serves both deployment shapes:
//
//   - **Per-session Hub** (the headless `cmd/6502-sim-serve` case):
//     each Load call builds a fresh Instrument, wraps it in a new
//     Hub, starts the Hub's pump goroutine, and returns a cleanup
//     that cancels it.
//   - **Shared Hub** (the `cmd/6502-sim --serve` case): every Load
//     call returns the same Hub the TUI itself drives. The TUI
//     created and started the Hub at startup. cleanup is a no-op —
//     the Hub outlives any single bridge session.
//
// This is what makes the Pump strictly "one mutator of the
// Instrument" in both modes: per-session vs shared is now just
// "who started the Hub," not a structural difference inside the
// bridge.
type Loader interface {
	Presets() []PresetInfo
	Load(name string, image []byte) (*Hub, func(), error)
}

// Capabilities advertised in `hello`. The v1 names survive; v2 adds
// `"events.topics"` — clients use it to detect topic-pubsub support
// (vs the v1 mode where notifications fired only from a server-side
// runner). See docs/bridge-v2.md §12.
var Capabilities = []string{"breakpoints", "taps", "frame", "events", "events.topics"}

// Handler is the per-method dispatch entry point.
type Handler func(*Session, json.RawMessage) (any, *Error)

// Server hosts JSON-RPC dispatch over any Conn. NewServer fixes the
// handler map; each Serve call gets its own Session.
type Server struct {
	loader   Loader
	handlers map[string]Handler
}

// NewServer constructs a Server with the v2-compatible handler set.
func NewServer(loader Loader) *Server {
	s := &Server{loader: loader, handlers: map[string]Handler{}}

	// Lifecycle + queries
	s.register("hello", helloHandler)
	s.register("machine.list", machineListHandler)
	s.register("machine.load", machineLoadHandler)
	s.register("machine.reset", machineResetHandler)
	s.register("cpu.state", cpuStateHandler)
	s.register("cpu.irq", cpuIRQHandler)
	s.register("cpu.nmi", cpuNMIHandler)
	s.register("cpu.setPC", cpuSetPCHandler)

	// Clock — v2 fire-and-forget for run/stop/step/runUntil
	s.register("clock.step", clockStepHandler)
	s.register("clock.stepCycle", clockStepCycleHandler)
	s.register("clock.setSpeedHz", clockSetSpeedHandler)
	s.register("clock.running", clockRunningHandler)
	s.register("clock.run", clockRunHandler)
	s.register("clock.stop", clockStopHandler)
	s.register("clock.runUntil", clockRunUntilHandler)
	s.register("clock.advance", clockAdvanceHandler)

	// Memory
	s.register("mem.peek", memPeekHandler)
	s.register("mem.poke", memPokeHandler)
	s.register("mem.read", memReadHandler)
	s.register("mem.frame", memFrameHandler)

	// Taps
	s.register("taps.list", tapsListHandler)
	s.register("taps.read", tapsReadHandler)

	// Breakpoints
	s.register("bp.set", bpSetHandler)
	s.register("bp.setVector", bpSetVectorHandler)
	s.register("bp.clear", bpClearHandler)
	s.register("bp.list", bpListHandler)

	// Events
	s.register("events.subscribe", eventsSubscribeHandler)
	s.register("events.unsubscribe", eventsUnsubscribeHandler)
	return s
}

func (s *Server) register(name string, h Handler) { s.handlers[name] = h }

// Session is the per-connection state. In v2, a session OWNs a Hub
// (one per loaded Instrument) and forwards execution commands through
// it. Queries take Hub.QueryLock briefly. Subscribers register with
// the Hub via events.subscribe; the Hub broadcasts events through
// the session's send closure (which marshals + Writes under writeMu).
type Session struct {
	s    *Server
	conn Conn

	writeMu sync.Mutex // serialises conn.Write
	mu      sync.Mutex // guards session-only state below

	initialised bool
	preset      string

	hub        *Hub
	hubCleanup func() // Loader-supplied teardown (cancel pump for per-session; noop for shared)
	subID      int    // 0 = not subscribed yet

	// Session-scoped breakpoint id mapping. v1 had identical
	// bookkeeping; v2 keeps it here because the Hub itself doesn't
	// know about per-id breakpoints (it just sets addresses on the
	// Instrument and toggles BreakOnVector).
	bps    map[string]bpInfo
	nextBP int
}

type bpInfo struct {
	ID     string
	Kind   string // "addr" or "vector"
	Addr   uint16
	Vector string
}

// Serve runs the dispatch loop until the Conn EOFs or ctx is
// cancelled. teardown stops the per-session Hub goroutine and
// closes the Conn. Idempotent.
func (s *Server) Serve(ctx context.Context, conn Conn) error {
	sess := &Session{
		s: s, conn: conn,
		bps: map[string]bpInfo{},
	}
	defer sess.teardown()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		frame, err := conn.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var req request
		if err := json.Unmarshal(frame, &req); err != nil {
			_ = sess.sendError(nil, newErr(CodeParse, "parse error: "+err.Error()))
			continue
		}
		if req.JSONRPC != "2.0" {
			_ = sess.sendError(req.ID, newErr(CodeInvalidRequest, `jsonrpc must be "2.0"`))
			continue
		}
		result, errObj := sess.dispatch(req)
		if req.ID == nil {
			// Notification from client — v1/v2 both accept none, but
			// silently ignore per JSON-RPC §4.1.
			continue
		}
		if errObj != nil {
			_ = sess.sendError(req.ID, errObj)
			continue
		}
		_ = sess.sendResult(req.ID, result)
	}
}

func (sess *Session) dispatch(req request) (any, *Error) {
	h, ok := sess.s.handlers[req.Method]
	if !ok {
		return nil, newErr(CodeMethodNotFound, "method not found: "+req.Method)
	}
	return h(sess, req.Params)
}

func (sess *Session) sendResult(id *json.RawMessage, result any) error {
	return sess.send(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (sess *Session) sendError(id *json.RawMessage, e *Error) error {
	return sess.send(response{JSONRPC: "2.0", ID: id, Error: e})
}

func (sess *Session) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	sess.writeMu.Lock()
	defer sess.writeMu.Unlock()
	return sess.conn.Write(b)
}

// sendNotification fans an event out via the same writeMu — used by
// the session's Hub-subscriber callback.
func (sess *Session) sendNotification(method string, params any) {
	_ = sess.send(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

// teardown unsubscribes from the Hub and invokes the Loader-supplied
// cleanup (which cancels the pump goroutine for per-session Hubs;
// is a no-op for shared Hubs). Then closes the Conn. Safe to call
// multiple times.
func (sess *Session) teardown() {
	sess.mu.Lock()
	hub := sess.hub
	subID := sess.subID
	cleanup := sess.hubCleanup
	sess.hub = nil
	sess.hubCleanup = nil
	sess.subID = 0
	sess.mu.Unlock()
	if hub != nil && subID != 0 {
		hub.Unsubscribe(subID)
	}
	if cleanup != nil {
		cleanup()
	}
	_ = sess.conn.Close()
}

// --- guards ---

func requireInitialised(sess *Session) *Error {
	sess.mu.Lock()
	init := sess.initialised
	sess.mu.Unlock()
	if !init {
		return newErr(CodeNotInitialised, "call hello before any other method")
	}
	return nil
}

func requireHub(sess *Session) *Error {
	if err := requireInitialised(sess); err != nil {
		return err
	}
	sess.mu.Lock()
	hub := sess.hub
	sess.mu.Unlock()
	if hub == nil {
		return newErr(CodeNoMachine, "no machine loaded; call machine.load first")
	}
	return nil
}

// hub returns sess.hub under lock (panics if nil — guarded by
// requireHub at handler entry).
func (sess *Session) getHub() *Hub {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.hub
}

// --- snapshot helper ---

// snapshot converts an Instrument.State into the wire CPUState
// (S→SP, drop SpeedHz which lives on the clock.running surface).
func snapshot(inst *instrument.Instrument) CPUState {
	s := inst.State()
	return CPUState{
		A: s.A, X: s.X, Y: s.Y, SP: s.S, P: s.P,
		PC:         s.PC,
		HalfCycles: s.HalfCycles,
		Running:    s.Running,
	}
}

// translateRunReason maps the Instrument's internal RunUntil reason
// onto the v2 wire vocabulary. Shared with the Hub.
func translateRunReason(r string) string {
	if r == "budget" {
		return "limit"
	}
	return r
}

// =====================================================================
//  Handlers
// =====================================================================

func helloHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	var p HelloParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, newErr(CodeInvalidParams, "hello: "+err.Error())
		}
	}
	if len(p.Protocols) > 0 && !slices.Contains(p.Protocols, Protocol) {
		return nil, newErrData(CodeInvalidParams, "no compatible protocol",
			map[string]any{"supported": []string{Protocol}})
	}
	sess.mu.Lock()
	sess.initialised = true
	sess.mu.Unlock()
	return HelloResult{
		ServerVersion: ServerVersion,
		Protocol:      Protocol,
		Capabilities:  Capabilities,
	}, nil
}

func machineListHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireInitialised(sess); err != nil {
		return nil, err
	}
	return MachineListResult{Presets: sess.s.loader.Presets()}, nil
}

func machineLoadHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireInitialised(sess); err != nil {
		return nil, err
	}
	var p MachineLoadParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "machine.load: "+err.Error())
	}
	if p.Preset == "" {
		return nil, newErr(CodeInvalidParams, "machine.load: missing preset")
	}
	var image []byte
	if p.Image != nil {
		b, err := base64.StdEncoding.DecodeString(p.Image.BytesB64)
		if err != nil {
			return nil, newErr(CodeImageReject, "machine.load: bad base64: "+err.Error())
		}
		image = b
	}

	// Tear down any prior session-Hub binding so a re-load works
	// (controllers may machine.load a new image without disconnecting).
	sess.mu.Lock()
	prevHub := sess.hub
	prevSubID := sess.subID
	prevCleanup := sess.hubCleanup
	sess.hub = nil
	sess.subID = 0
	sess.hubCleanup = nil
	sess.mu.Unlock()
	if prevHub != nil && prevSubID != 0 {
		prevHub.Unsubscribe(prevSubID)
	}
	if prevCleanup != nil {
		prevCleanup()
	}

	hub, cleanup, err := sess.s.loader.Load(p.Preset, image)
	if err != nil {
		return nil, newErr(CodeUnknownPreset, "machine.load: "+err.Error())
	}
	// Register the session as a (no-topic) subscriber up-front so
	// events.subscribe can incrementally add topics later. The
	// callback enriches bp.hit with the session-side bp id (the Hub
	// doesn't know about ids — only addresses).
	id, _ := hub.Subscribe(nil, func(method string, params any) {
		if method == "bp.hit" {
			if hp, ok := params.(BPHitPayload); ok {
				sess.mu.Lock()
				for id, info := range sess.bps {
					if info.Kind == "addr" && info.Addr == hp.Addr {
						hp.ID = id
						break
					}
				}
				sess.mu.Unlock()
				sess.sendNotification(method, hp)
				return
			}
		}
		sess.sendNotification(method, params)
	})

	sess.mu.Lock()
	sess.hub = hub
	sess.hubCleanup = cleanup
	sess.subID = id
	sess.preset = p.Preset
	sess.bps = map[string]bpInfo{}
	sess.nextBP = 0
	sess.mu.Unlock()

	// Look up the human-readable strings the loader records for this
	// preset so clients can render the machine instead of the slug.
	// Cheap (typically <10 entries in Presets) and only runs once per
	// machine.load.
	var label, summary string
	for _, pi := range sess.s.loader.Presets() {
		if pi.Name == p.Preset {
			label = pi.Label
			summary = pi.Summary
			break
		}
	}
	return MachineLoadResult{
		Preset:  p.Preset,
		Label:   label,
		Summary: summary,
		Regions: hub.Regions(),
	}, nil
}

// --- queries: take QueryLock briefly ---

func cpuStateHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	hub := sess.getHub()
	hub.QueryLock().Lock()
	defer hub.QueryLock().Unlock()
	return snapshot(hub.Inst()), nil
}

// cpuIRQHandler pulses the host-driven IRQ line. Fire-and-forget;
// the controller observes the effect via the next state.snapshot
// (PC jumps to the IRQ handler's address from $FFFE) or via the
// clock.halt event if the CPU was already stopped at the time.
//
// If the CPU has the I (interrupt-disable) flag set, the pulse is
// ignored — that's correct 6502 hardware behaviour. The next state
// snapshot will show I=1 / PC unchanged, which is the diagnostic the
// user wants to see.
func cpuIRQHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	sess.getHub().CmdIRQ()
	return struct {
		OK bool `json:"ok"`
	}{OK: true}, nil
}

// cpuNMIHandler pulses the host-driven NMI line. Like IRQ but
// edge-triggered, vectors via $FFFA, and is NOT masked by the
// I-flag — NMI always fires on a clean edge.
//
// In netsim CPU mode this is currently a no-op (the upstream
// 6502-netsim-go module doesn't expose SetNMI; tracked as a future
// upstream bump). interp mode services normally.
func cpuNMIHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	sess.getHub().CmdNMI()
	return struct {
		OK bool `json:"ok"`
	}{OK: true}, nil
}

// cpuSetPCHandler overwrites the program counter. Returns the
// post-set CPU state so clients can render the new PC without
// waiting for the next state.snapshot. Errors when the backend
// doesn't support it (netsim CPU mode).
func cpuSetPCHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	var p CPUSetPCParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "cpu.setPC: "+err.Error())
	}
	hub := sess.getHub()
	if err := hub.CmdSetPC(p.PC); err != nil {
		return nil, newErr(CodeBusError, err.Error())
	}
	hub.QueryLock().Lock()
	defer hub.QueryLock().Unlock()
	return CPUSetPCResult{OK: true, State: snapshot(hub.Inst())}, nil
}

func clockRunningHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	hub := sess.getHub()
	hub.QueryLock().Lock()
	defer hub.QueryLock().Unlock()
	return ClockRunningResult{
		Running: hub.Inst().Running(),
		SpeedHz: hub.Inst().Driver().Speed().Hz,
	}, nil
}

func memPeekHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	p := MemPeekParams{N: 1}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "mem.peek: "+err.Error())
	}
	if p.N <= 0 {
		p.N = 1
	}
	end := int(p.Addr) + p.N - 1
	if end > 0xFFFF {
		return nil, newErrData(CodeBusError, "mem.peek: range exceeds 64K",
			map[string]any{"addr": p.Addr, "n": p.N})
	}
	hub := sess.getHub()
	hub.QueryLock().Lock()
	buf := hub.Inst().Mem(p.Addr, uint16(end))
	hub.QueryLock().Unlock()
	return MemPeekResult{BytesB64: base64.StdEncoding.EncodeToString(buf)}, nil
}

func memReadHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	var p MemReadParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "mem.read: "+err.Error())
	}
	hub := sess.getHub()
	r, ok := findRegion(hub.Regions(), p.Region)
	if !ok {
		return nil, newErrData(CodeBusError, "mem.read: unknown region",
			map[string]any{"region": p.Region})
	}
	hub.QueryLock().Lock()
	buf := hub.Inst().Mem(r.Lo, r.Hi)
	hub.QueryLock().Unlock()
	return MemReadResult{
		BytesB64: base64.StdEncoding.EncodeToString(buf),
		Addr:     r.Lo,
		Length:   len(buf),
	}, nil
}

func memFrameHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	hub := sess.getHub()
	r, ok := findRegion(hub.Regions(), "framebuffer")
	if !ok {
		return nil, newErr(CodeCapMissing, "mem.frame: preset has no framebuffer region")
	}
	hub.QueryLock().Lock()
	buf := hub.Inst().Mem(r.Lo, r.Hi)
	hub.QueryLock().Unlock()
	return MemReadResult{
		BytesB64: base64.StdEncoding.EncodeToString(buf),
		Addr:     r.Lo,
		Length:   len(buf),
	}, nil
}

func findRegion(regions []Region, name string) (Region, bool) {
	for _, r := range regions {
		if r.Name == name {
			return r, true
		}
	}
	return Region{}, false
}

func tapsListHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	hub := sess.getHub()
	hub.QueryLock().Lock()
	m := hub.Inst().Taps()
	hub.QueryLock().Unlock()
	out := make([]TapInfo, 0, len(m))
	for name := range m {
		out = append(out, TapInfo{Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return TapsListResult{Taps: out}, nil
}

func tapsReadHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	var p TapsReadParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, newErr(CodeInvalidParams, "taps.read: "+err.Error())
		}
	}
	hub := sess.getHub()
	hub.QueryLock().Lock()
	all := hub.Inst().Taps()
	hub.QueryLock().Unlock()
	if len(p.Names) == 0 {
		return TapsReadResult{Values: all}, nil
	}
	out := map[string]uint64{}
	for _, n := range p.Names {
		if v, ok := all[n]; ok {
			out[n] = v
		}
	}
	return TapsReadResult{Values: out}, nil
}

func bpListHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	out := make([]Breakpoint, 0, len(sess.bps))
	for _, b := range sess.bps {
		out = append(out, Breakpoint{
			ID: b.ID, Kind: b.Kind, Addr: b.Addr, Vector: b.Vector,
		})
	}
	sess.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return BPListResult{Breakpoints: out}, nil
}

// --- mutators: route through Hub.Cmd* ---

func clockRunHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	var p ClockRunParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, newErr(CodeInvalidParams, "clock.run: "+err.Error())
		}
	}
	speedHz := 0
	if p.SpeedHz != nil {
		speedHz = *p.SpeedHz
	}
	sess.getHub().CmdRun(0, speedHz)
	return map[string]any{}, nil
}

func clockStopHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	hub := sess.getHub()
	// v2 wire compat: install a one-shot subscriber to wait for the
	// halt event that CmdStop will trigger, then return a v1-shaped
	// ClockStopResult. Pure event-bus clients use clock.halt
	// subscription instead and ignore this return shape.
	halted := make(chan RunResult, 1)
	tmpID, _ := hub.Subscribe([]string{"clock.halt"}, func(method string, params any) {
		if method == "clock.halt" {
			if r, ok := params.(RunResult); ok {
				select {
				case halted <- r:
				default:
				}
			}
		}
	})
	defer hub.Unsubscribe(tmpID)
	hub.CmdStop()
	select {
	case r := <-halted:
		return ClockStopResult{State: r.State, Reason: r.Reason}, nil
	case <-time.After(2 * time.Second):
		// Pump didn't emit a halt — either it wasn't running (no-op
		// stop) or it's wedged. Synthesise a "no-op" response.
		hub.QueryLock().Lock()
		st := snapshot(hub.Inst())
		hub.QueryLock().Unlock()
		return ClockStopResult{State: st, Reason: "noop"}, nil
	}
}

func clockStepHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	p := ClockStepParams{N: 1}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, newErr(CodeInvalidParams, "clock.step: "+err.Error())
		}
	}
	if p.N <= 0 {
		p.N = 1
	}
	// Synchronous step for v1 wire-compat: drive directly via inst
	// (Hub's CmdStep is the fire-and-forget alternative; clients
	// preferring v2 semantics can subscribe to clock.halt + send
	// the equivalent command). Held under QueryLock to coordinate
	// with the Pump.
	hub := sess.getHub()
	hub.QueryLock().Lock()
	hub.Inst().Step(p.N)
	// If a host-IRQ / host-NMI pulse was queued in idle (CmdIRQ /
	// CmdNMI assertion has no slice to auto-clear it in), this step
	// is the natural release point — the CPU has now had a chance
	// to sample SYNC and service the interrupt. Hold-forever would
	// re-fire IRQ every step, and would prevent NMI's edge gate
	// from triggering on the next pulse.
	hub.Inst().Backplane().AssertHostIRQ(false)
	hub.Inst().Backplane().AssertHostNMI(false)
	st := snapshot(hub.Inst())
	hub.QueryLock().Unlock()
	return ClockStepResult{State: st}, nil
}

func clockStepCycleHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	p := ClockStepCycleParams{Halves: 1}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, newErr(CodeInvalidParams, "clock.stepCycle: "+err.Error())
		}
	}
	if p.Halves <= 0 {
		p.Halves = 1
	}
	hub := sess.getHub()
	hub.QueryLock().Lock()
	hub.Inst().StepCycle(p.Halves)
	st := snapshot(hub.Inst())
	hub.QueryLock().Unlock()
	return ClockStepResult{State: st}, nil
}

func clockSetSpeedHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	var p ClockSetSpeedParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "clock.setSpeedHz: "+err.Error())
	}
	if err := sess.getHub().CmdSetSpeed(p.Hz); err != nil {
		supported := make([]int, 0, len(clock.Speeds))
		for _, s := range clock.Speeds {
			supported = append(supported, s.Hz)
		}
		return nil, newErrData(CodeInvalidParams, err.Error(),
			map[string]any{"supported": supported})
	}
	hub := sess.getHub()
	hub.QueryLock().Lock()
	hz := hub.Inst().Driver().Speed().Hz
	hub.QueryLock().Unlock()
	return ClockSetSpeedResult{Hz: hz}, nil
}

func clockRunUntilHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	var p ClockRunUntilParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "clock.runUntil: "+err.Error())
	}
	if p.MaxHalfCycles <= 0 {
		return nil, newErr(CodeInvalidParams, "clock.runUntil: maxHalfCycles must be > 0")
	}
	// v2: clock.runUntil = CmdRun with a budget. Returns a RunResult
	// constructed from the post-halt state. We synthesise a one-shot
	// internal subscriber to wait for clock.halt — preserves the v1
	// synchronous return shape that existing tests depend on.
	hub := sess.getHub()
	halted := make(chan RunResult, 1)
	tmpID, _ := hub.Subscribe([]string{"clock.halt"}, func(method string, params any) {
		if method != "clock.halt" {
			return
		}
		if r, ok := params.(RunResult); ok {
			select {
			case halted <- r:
			default:
			}
		}
	})
	defer hub.Unsubscribe(tmpID)
	hub.CmdRun(p.MaxHalfCycles, 0)
	select {
	case r := <-halted:
		// Fill BpID from the session's id map if this was a bp halt.
		if r.Reason == "breakpoint" {
			sess.mu.Lock()
			for id, info := range sess.bps {
				if info.Kind == "addr" && info.Addr == r.Addr {
					r.BpID = id
					break
				}
			}
			sess.mu.Unlock()
		}
		return r, nil
	case <-time.After(5 * time.Second):
		return nil, newErr(CodeInternalError, "clock.runUntil: timed out waiting for halt")
	}
}

// machineResetHandler runs a shallow Hub-level reset on the Pump
// goroutine (CPU + clock driver). Returns the post-reset CPUState
// so the caller doesn't have to follow with a cpu.state.
func machineResetHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	hub := sess.getHub()
	hub.CmdReset()
	hub.QueryLock().Lock()
	st := snapshot(hub.Inst())
	hub.QueryLock().Unlock()
	return struct {
		State CPUState `json:"state"`
	}{State: st}, nil
}

// clockAdvanceHandler — synchronous advance + tick (the v1 wire
// surface; useful for callers that want a precise "advance virtual
// time by dt" without involving the Pump goroutine).
func clockAdvanceHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	var p ClockAdvanceParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "clock.advance: "+err.Error())
	}
	if p.DurationUs <= 0 {
		return nil, newErr(CodeInvalidParams, "clock.advance: durationUs must be > 0")
	}
	hub := sess.getHub()
	hub.QueryLock().Lock()
	n := hub.Inst().Advance(time.Duration(p.DurationUs) * time.Microsecond)
	st := snapshot(hub.Inst())
	hub.QueryLock().Unlock()
	return ClockAdvanceResult{HalfCycles: n, State: st}, nil
}

func memPokeHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	var p MemPokeParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "mem.poke: "+err.Error())
	}
	b, err := base64.StdEncoding.DecodeString(p.BytesB64)
	if err != nil {
		return nil, newErr(CodeInvalidParams, "mem.poke: bad base64: "+err.Error())
	}
	if int(p.Addr)+len(b)-1 > 0xFFFF {
		return nil, newErrData(CodeBusError, "mem.poke: range exceeds 64K",
			map[string]any{"addr": p.Addr, "len": len(b)})
	}
	sess.getHub().CmdMemPoke(p.Addr, b)
	return MemPokeResult{Written: len(b)}, nil
}

// --- breakpoints ---

func bpSetHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	var p BPSetParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "bp.set: "+err.Error())
	}
	if p.Kind != "" && p.Kind != "addr" {
		return nil, newErr(CodeInvalidParams, `bp.set: kind must be "addr"`)
	}
	sess.mu.Lock()
	sess.nextBP++
	id := fmt.Sprintf("bp_%d", sess.nextBP)
	sess.bps[id] = bpInfo{ID: id, Kind: "addr", Addr: p.Addr}
	sess.mu.Unlock()
	sess.getHub().CmdBPSet(p.Addr)
	return BPSetResult{ID: id, Addr: p.Addr}, nil
}

func bpSetVectorHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	var p BPSetVectorParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "bp.setVector: "+err.Error())
	}
	switch p.Vector {
	case "reset", "nmi", "irq":
	default:
		return nil, newErr(CodeInvalidParams,
			`bp.setVector: vector must be "reset" | "nmi" | "irq"`)
	}
	sess.mu.Lock()
	sess.nextBP++
	id := fmt.Sprintf("bp_%d", sess.nextBP)
	sess.bps[id] = bpInfo{ID: id, Kind: "vector", Vector: p.Vector}
	sess.mu.Unlock()
	sess.getHub().CmdBPSetVector()
	return BPSetVectorResult{ID: id, Vector: p.Vector}, nil
}

func bpClearHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	var p BPClearParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, newErr(CodeInvalidParams, "bp.clear: "+err.Error())
		}
	}
	hub := sess.getHub()
	if p.ID == "" {
		sess.mu.Lock()
		n := len(sess.bps)
		sess.bps = map[string]bpInfo{}
		sess.mu.Unlock()
		hub.CmdBPClearAll()
		return BPClearResult{Cleared: n}, nil
	}
	sess.mu.Lock()
	info, ok := sess.bps[p.ID]
	if !ok {
		sess.mu.Unlock()
		return nil, newErrData(CodeUnknownBP, "bp.clear: unknown id",
			map[string]any{"id": p.ID})
	}
	delete(sess.bps, p.ID)
	// Determine if any vector bp remains (for the toggle).
	anyVec := false
	for _, b := range sess.bps {
		if b.Kind == "vector" {
			anyVec = true
			break
		}
	}
	sess.mu.Unlock()
	switch info.Kind {
	case "addr":
		hub.CmdBPClear(info.Addr)
	case "vector":
		if !anyVec {
			hub.CmdBPClearVector()
		}
	}
	return BPClearResult{Cleared: 1}, nil
}

// --- events ---

func eventsSubscribeHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	var p EventsSubscribeParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "events.subscribe: "+err.Error())
	}
	sess.mu.Lock()
	id := sess.subID
	sess.mu.Unlock()
	accepted := sess.getHub().AddTopics(id, p.Channels)
	return EventsSubscribeResult{Subscribed: accepted}, nil
}

func eventsUnsubscribeHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireHub(sess); err != nil {
		return nil, err
	}
	var p EventsUnsubscribeParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, newErr(CodeInvalidParams, "events.unsubscribe: "+err.Error())
		}
	}
	sess.mu.Lock()
	id := sess.subID
	sess.mu.Unlock()
	removed := sess.getHub().RemoveTopics(id, p.Channels)
	if len(p.Channels) == 0 {
		return EventsUnsubscribeResult{Unsubscribed: []string{"*"}}, nil
	}
	return EventsUnsubscribeResult{Unsubscribed: removed}, nil
}
