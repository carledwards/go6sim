package bridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/carledwards/go6sim/clock"
	"github.com/carledwards/go6sim/instrument"
)

// Loader is the bridge's seam to the machine presets: a small interface
// the caller implements so the bridge package itself does not import
// any concrete preset package. cmd/6502-sim-serve wires a real Loader
// that knows go6sim/machine; tests wire a stub.
type Loader interface {
	// Presets lists what `machine.list` returns.
	Presets() []PresetInfo
	// Load builds the named preset and, if image != nil, writes it as
	// ROM at the preset's program origin. Returns the live Instrument
	// and the live memory map for the session.
	Load(name string, image []byte) (*instrument.Instrument, []Region, error)
}

// Capabilities the bridge advertises in `hello`. These are *optional
// feature groups* per docs/bridge.md §8 — `breakpoints` covers bp.*,
// `taps` covers taps.*, `frame` covers mem.frame. The core methods
// (hello, machine.*, cpu.state, clock.*, mem.peek/poke/read) are
// always present and are NOT capability-gated.
var Capabilities = []string{"breakpoints", "taps", "frame", "events"}

// Handler is the per-method dispatch shape: it gets the session and
// the raw params blob, and returns either a result value or a typed
// Error. The result is JSON-marshalled by the server.
type Handler func(*Session, json.RawMessage) (any, *Error)

// Server hosts JSON-RPC dispatch over any [Conn]. Each Serve call gets
// its own [Session] — one connection, one session, one machine, per
// design §4 single-machine rule.
type Server struct {
	loader   Loader
	handlers map[string]Handler
}

// NewServer constructs a Server bound to a Loader. Handlers are
// registered here so the dispatch map is fixed at construction.
func NewServer(loader Loader) *Server {
	s := &Server{loader: loader, handlers: map[string]Handler{}}

	// Lifecycle + machine
	s.register("hello", helloHandler)
	s.register("machine.list", machineListHandler)
	s.register("machine.load", machineLoadHandler)

	// CPU state
	s.register("cpu.state", cpuStateHandler)

	// Clock (synchronous control; async clock.run + events come later)
	s.register("clock.step", clockStepHandler)
	s.register("clock.stepCycle", clockStepCycleHandler)
	s.register("clock.setSpeedHz", clockSetSpeedHandler)
	s.register("clock.advance", clockAdvanceHandler)
	s.register("clock.running", clockRunningHandler)
	s.register("clock.runUntil", clockRunUntilHandler)

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

	// Async run + events (Phase 1c-2)
	s.register("clock.run", clockRunHandler)
	s.register("clock.stop", clockStopHandler)
	s.register("events.subscribe", eventsSubscribeHandler)
	s.register("events.unsubscribe", eventsUnsubscribeHandler)
	return s
}

func (s *Server) register(name string, h Handler) { s.handlers[name] = h }

// Session is the per-connection state: hello status, the loaded
// Instrument, the live memory map (so mem.read by region name works),
// the session-scoped breakpoint book (the Instrument has only the raw
// arming — ids, kinds, and the vector-bp refcount live here), the
// subscription state, and the async-run goroutine bookkeeping.
//
// Concurrency: there are at most two goroutines touching a Session at
// any time — the Serve dispatch loop (one handler call at a time) and,
// when clock.run is active, the runner goroutine. `mu` guards all
// mutating session state including `inst` (handlers and the runner
// both lock around Instrument calls); `writeMu` serialises Conn.Write
// so notification frames and response frames never interleave.
type Session struct {
	s    *Server
	conn Conn

	mu      sync.Mutex // guards everything below except writeMu and conn
	writeMu sync.Mutex // serialises conn.Write

	initialised bool
	inst        *instrument.Instrument
	preset      string

	regions []Region
	bps     map[string]bpInfo
	nextBP  int

	// Subscription state. Default-empty: a client receives no
	// notifications until it calls events.subscribe (design §5.5).
	subClockHalt    bool
	subBPHit        bool
	subState        bool
	stateIntervalMs int // default 100; configurable later
	subTapAll       bool
	subTapNames     map[string]bool

	// Async-run state. runActive transitions false→true on clock.run
	// and back on either clock.stop or natural halt. runCancel is
	// closed by clock.stop (or Serve teardown) to signal the runner.
	runActive bool
	runCancel chan struct{}
	runDone   chan struct{}

	quit chan struct{} // closed when Serve returns
}

// bpInfo is one bridge-side breakpoint record. Multiple vector bps may
// coexist with id-level granularity even though the Instrument exposes
// only a single BreakOnVector(bool) toggle — the bridge ref-counts.
type bpInfo struct {
	ID     string
	Kind   string // "addr" or "vector"
	Addr   uint16
	Vector string // "reset" | "nmi" | "irq" when Kind == "vector"
}

// Serve runs the dispatch loop until the Conn EOFs or ctx is cancelled.
// One frame in, one frame out (unless the frame is a notification, in
// which case nothing goes out). Notifications are also pushed by the
// async runner goroutine (see [Session.runner]); both paths serialise
// on writeMu so frames don't interleave on the wire.
func (s *Server) Serve(ctx context.Context, conn Conn) error {
	sess := &Session{
		s: s, conn: conn,
		quit:            make(chan struct{}),
		subTapNames:     map[string]bool{},
		stateIntervalMs: 100,
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
			// Notification from the client — v1 accepts none, but per
			// JSON-RPC §4.1 we silently ignore rather than respond.
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
	// writeMu serialises with the runner's notification sends so
	// JSON frames never interleave on the wire (one frame = one Write).
	sess.writeMu.Lock()
	defer sess.writeMu.Unlock()
	return sess.conn.Write(b)
}

// sendNotification serialises with response writes via writeMu.
// Frames are emitted strictly FIFO with whatever else holds writeMu —
// so a notification enqueued before a response is written first, and
// vice versa.
func (sess *Session) sendNotification(method string, params any) {
	_ = sess.send(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

// teardown is the per-session cleanup: signal the runner to exit if
// one is alive, wait for it, then close the Conn. Idempotent.
func (sess *Session) teardown() {
	sess.signalCancel()
	sess.mu.Lock()
	done := sess.runDone
	sess.mu.Unlock()
	if done != nil {
		<-done
	}
	select {
	case <-sess.quit:
	default:
		close(sess.quit)
	}
	_ = sess.conn.Close()
}

// signalCancel closes runCancel if a runner is alive and hasn't been
// asked to stop yet. Safe to call multiple times (e.g. clock.stop then
// teardown).
func (sess *Session) signalCancel() {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if !sess.runActive || sess.runCancel == nil {
		return
	}
	select {
	case <-sess.runCancel:
		// already cancelled
	default:
		close(sess.runCancel)
	}
}

// --- guards ---

func requireInitialised(sess *Session) *Error {
	if !sess.initialised {
		return newErr(CodeNotInitialised, "call hello before any other method")
	}
	return nil
}

func requireMachine(sess *Session) *Error {
	if err := requireInitialised(sess); err != nil {
		return err
	}
	if sess.inst == nil {
		return newErr(CodeNoMachine, "no machine loaded; call machine.load first")
	}
	return nil
}

// --- handlers ---

func helloHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	var p HelloParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, newErr(CodeInvalidParams, "hello: "+err.Error())
		}
	}
	// Protocol negotiation: skeleton only knows Protocol. Reject
	// clients that list protocols explicitly without including it.
	if len(p.Protocols) > 0 && !slices.Contains(p.Protocols, Protocol) {
		return nil, newErrData(CodeInvalidParams, "no compatible protocol",
			map[string]any{"supported": []string{Protocol}})
	}
	sess.initialised = true
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
	// Reject if a previous machine's run is still alive — the client
	// must clock.stop first. machine.load mid-run would yank inst out
	// from under the runner goroutine.
	sess.mu.Lock()
	if sess.runActive {
		sess.mu.Unlock()
		return nil, newErr(CodeNotInRun, "machine.load: run active; clock.stop first")
	}
	sess.mu.Unlock()

	inst, regions, err := sess.s.loader.Load(p.Preset, image)
	if err != nil {
		return nil, newErr(CodeUnknownPreset, "machine.load: "+err.Error())
	}
	sess.mu.Lock()
	sess.inst = inst
	sess.preset = p.Preset
	sess.regions = regions
	sess.bps = map[string]bpInfo{}
	sess.nextBP = 0
	sess.mu.Unlock()
	return MachineLoadResult{Preset: p.Preset, Regions: regions}, nil
}

func cpuStateHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return snapshot(sess.inst), nil
}

// snapshot converts an Instrument.State into the wire CPUState (mainly
// the S→SP field rename, plus dropping SpeedHz which lives on the
// clock.running surface instead).
func snapshot(inst *instrument.Instrument) CPUState {
	s := inst.State()
	return CPUState{
		A: s.A, X: s.X, Y: s.Y, SP: s.S, P: s.P,
		PC:         s.PC,
		HalfCycles: s.HalfCycles,
		Running:    s.Running,
	}
}

// --- clock handlers ---

func clockStepHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	p := ClockStepParams{N: 1}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, newErr(CodeInvalidParams, "clock.step: "+err.Error())
		}
	}
	if p.N <= 0 {
		p.N = 1
	}
	sess.inst.Step(p.N)
	return ClockStepResult{State: snapshot(sess.inst)}, nil
}

func clockStepCycleHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	p := ClockStepCycleParams{Halves: 1}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, newErr(CodeInvalidParams, "clock.stepCycle: "+err.Error())
		}
	}
	if p.Halves <= 0 {
		p.Halves = 1
	}
	sess.inst.StepCycle(p.Halves)
	return ClockStepResult{State: snapshot(sess.inst)}, nil
}

func clockSetSpeedHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	var p ClockSetSpeedParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "clock.setSpeedHz: "+err.Error())
	}
	if ok := sess.inst.Driver().SetSpeedHz(p.Hz); !ok {
		supported := make([]int, 0, len(clock.Speeds))
		for _, s := range clock.Speeds {
			supported = append(supported, s.Hz)
		}
		return nil, newErrData(CodeInvalidParams,
			"clock.setSpeedHz: hz not in supported set",
			map[string]any{"supported": supported})
	}
	return ClockSetSpeedResult{Hz: sess.inst.Driver().Speed().Hz}, nil
}

func clockAdvanceHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	var p ClockAdvanceParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "clock.advance: "+err.Error())
	}
	if p.DurationUs <= 0 {
		return nil, newErr(CodeInvalidParams, "clock.advance: durationUs must be > 0")
	}
	n := sess.inst.Advance(time.Duration(p.DurationUs) * time.Microsecond)
	return ClockAdvanceResult{HalfCycles: n, State: snapshot(sess.inst)}, nil
}

func clockRunningHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return ClockRunningResult{
		Running: sess.inst.Running(),
		SpeedHz: sess.inst.Driver().Speed().Hz,
	}, nil
}

func clockRunUntilHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	var p ClockRunUntilParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "clock.runUntil: "+err.Error())
	}
	if p.MaxHalfCycles <= 0 {
		return nil, newErr(CodeInvalidParams, "clock.runUntil: maxHalfCycles must be > 0")
	}
	r := sess.inst.RunUntil(p.MaxHalfCycles)
	return RunResult{
		HalfCycles: r.HalfCycles,
		Reason:     translateRunReason(r.Reason),
		Addr:       r.Addr,
		BpID:       lookupBPID(sess, r),
		State:      snapshot(sess.inst),
	}, nil
}

// translateRunReason maps the Instrument's internal reasons onto the
// design §5.4 vocabulary. The interp's "budget" surfaces as "limit"
// per the design's intent (max-half-cycles exhausted).
func translateRunReason(r string) string {
	if r == "budget" {
		return "limit"
	}
	return r
}

// lookupBPID returns the session-side breakpoint id if the run stopped
// on a known address breakpoint; "" otherwise. Vector breaks share a
// single Instrument toggle, so any active vector bp could have fired
// and the bridge doesn't attribute one specifically.
func lookupBPID(sess *Session, r instrument.RunResult) string {
	if r.Reason != "breakpoint" {
		return ""
	}
	for id, info := range sess.bps {
		if info.Kind == "addr" && info.Addr == r.Addr {
			return id
		}
	}
	return ""
}

// --- mem handlers ---

func memPeekHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
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
	buf := sess.inst.Mem(p.Addr, uint16(end))
	return MemPeekResult{BytesB64: base64.StdEncoding.EncodeToString(buf)}, nil
}

func memPokeHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
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
	for i, v := range b {
		sess.inst.Poke(p.Addr+uint16(i), v)
	}
	return MemPokeResult{Written: len(b)}, nil
}

func memReadHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	var p MemReadParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "mem.read: "+err.Error())
	}
	r, ok := findRegion(sess, p.Region)
	if !ok {
		return nil, newErrData(CodeBusError, "mem.read: unknown region",
			map[string]any{"region": p.Region})
	}
	buf := sess.inst.Mem(r.Lo, r.Hi)
	return MemReadResult{
		BytesB64: base64.StdEncoding.EncodeToString(buf),
		Addr:     r.Lo,
		Length:   len(buf),
	}, nil
}

func memFrameHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	r, ok := findRegion(sess, "framebuffer")
	if !ok {
		return nil, newErr(CodeCapMissing, "mem.frame: preset has no framebuffer region")
	}
	buf := sess.inst.Mem(r.Lo, r.Hi)
	return MemReadResult{
		BytesB64: base64.StdEncoding.EncodeToString(buf),
		Addr:     r.Lo,
		Length:   len(buf),
	}, nil
}

func findRegion(sess *Session, name string) (Region, bool) {
	for _, r := range sess.regions {
		if r.Name == name {
			return r, true
		}
	}
	return Region{}, false
}

// --- taps handlers ---

func tapsListHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	m := sess.inst.Taps()
	out := make([]TapInfo, 0, len(m))
	for name := range m {
		out = append(out, TapInfo{Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return TapsListResult{Taps: out}, nil
}

func tapsReadHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	var p TapsReadParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, newErr(CodeInvalidParams, "taps.read: "+err.Error())
		}
	}
	all := sess.inst.Taps()
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

// --- bp handlers ---

func bpSetHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	var p BPSetParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "bp.set: "+err.Error())
	}
	if p.Kind != "" && p.Kind != "addr" {
		return nil, newErr(CodeInvalidParams, `bp.set: kind must be "addr"`)
	}
	sess.nextBP++
	id := fmt.Sprintf("bp_%d", sess.nextBP)
	sess.inst.SetBreakpoint(p.Addr)
	sess.bps[id] = bpInfo{ID: id, Kind: "addr", Addr: p.Addr}
	return BPSetResult{ID: id, Addr: p.Addr}, nil
}

func bpSetVectorHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
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
	sess.nextBP++
	id := fmt.Sprintf("bp_%d", sess.nextBP)
	sess.inst.BreakOnVector(true)
	sess.bps[id] = bpInfo{ID: id, Kind: "vector", Vector: p.Vector}
	return BPSetVectorResult{ID: id, Vector: p.Vector}, nil
}

func bpClearHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	var p BPClearParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, newErr(CodeInvalidParams, "bp.clear: "+err.Error())
		}
	}
	if p.ID == "" {
		// Clear all.
		n := len(sess.bps)
		sess.inst.ClearBreakpoints()
		sess.inst.BreakOnVector(false)
		sess.bps = map[string]bpInfo{}
		return BPClearResult{Cleared: n}, nil
	}
	info, ok := sess.bps[p.ID]
	if !ok {
		return nil, newErrData(CodeUnknownBP, "bp.clear: unknown id",
			map[string]any{"id": p.ID})
	}
	switch info.Kind {
	case "addr":
		sess.inst.ClearBreakpoint(info.Addr)
		delete(sess.bps, p.ID)
	case "vector":
		delete(sess.bps, p.ID)
		// Flip the Instrument toggle off only when the last vector bp
		// is gone — multiple vector bps may coexist by id.
		anyVec := false
		for _, b := range sess.bps {
			if b.Kind == "vector" {
				anyVec = true
				break
			}
		}
		if !anyVec {
			sess.inst.BreakOnVector(false)
		}
	}
	return BPClearResult{Cleared: 1}, nil
}

func bpListHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	out := make([]Breakpoint, 0, len(sess.bps))
	for _, b := range sess.bps {
		out = append(out, Breakpoint{
			ID: b.ID, Kind: b.Kind, Addr: b.Addr, Vector: b.Vector,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return BPListResult{Breakpoints: out}, nil
}

// --- async run + events (Phase 1c-2) ---

// sliceHalves is the per-iteration RunUntil budget — small enough that
// clock.stop responds within ~0.2 ms at a 1 MHz target, but large enough
// that the per-slice mutex churn doesn't dominate.
const sliceHalves = 200

// tapEmitMinInterval — per-channel coalescing budget for tap.changed.
// ≈60 Hz, matches docs/bridge.md §7 default. Hardcoded for v1; a future
// `tapMaxHz` option on events.subscribe can override.
const tapEmitMinInterval = 16 * time.Millisecond

func clockRunHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	var p ClockRunParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, newErr(CodeInvalidParams, "clock.run: "+err.Error())
		}
	}
	sess.mu.Lock()
	if sess.runActive {
		sess.mu.Unlock()
		return nil, newErr(CodeNotInRun, "clock.run: already running")
	}
	if p.SpeedHz != nil {
		if ok := sess.inst.Driver().SetSpeedHz(*p.SpeedHz); !ok {
			sess.mu.Unlock()
			return nil, newErr(CodeInvalidParams,
				"clock.run: speedHz not in supported set")
		}
	}
	sess.inst.SetRunning(true)
	sess.runActive = true
	sess.runCancel = make(chan struct{})
	sess.runDone = make(chan struct{})
	sess.mu.Unlock()
	go sess.runner()
	return map[string]any{}, nil
}

func clockStopHandler(sess *Session, _ json.RawMessage) (any, *Error) {
	if err := requireMachine(sess); err != nil {
		return nil, err
	}
	sess.mu.Lock()
	if !sess.runActive {
		sess.mu.Unlock()
		return nil, newErr(CodeNotInRun, "clock.stop: not running")
	}
	cancel := sess.runCancel
	done := sess.runDone
	sess.mu.Unlock()

	select {
	case <-cancel:
		// already cancelled (e.g. natural halt arrived concurrently)
	default:
		close(cancel)
	}
	<-done // wait for runner to emit halt and exit

	sess.mu.Lock()
	st := snapshot(sess.inst)
	sess.mu.Unlock()
	return ClockStopResult{State: st, Reason: "client"}, nil
}

func eventsSubscribeHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireInitialised(sess); err != nil {
		return nil, err
	}
	var p EventsSubscribeParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, newErr(CodeInvalidParams, "events.subscribe: "+err.Error())
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	acc := make([]string, 0, len(p.Channels))
	for _, ch := range p.Channels {
		switch {
		case ch == "clock.halt":
			sess.subClockHalt = true
		case ch == "bp.hit":
			sess.subBPHit = true
		case ch == "state":
			sess.subState = true
		case ch == "tap.*":
			sess.subTapAll = true
		case strings.HasPrefix(ch, "tap.") && len(ch) > 4:
			sess.subTapNames[ch[4:]] = true
		default:
			// Unknown channel — skip silently. Clients can compare the
			// returned `subscribed` list against what they asked for
			// to detect a server that doesn't know a channel.
			continue
		}
		acc = append(acc, ch)
	}
	return EventsSubscribeResult{Subscribed: acc}, nil
}

func eventsUnsubscribeHandler(sess *Session, raw json.RawMessage) (any, *Error) {
	if err := requireInitialised(sess); err != nil {
		return nil, err
	}
	var p EventsUnsubscribeParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, newErr(CodeInvalidParams, "events.unsubscribe: "+err.Error())
		}
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if len(p.Channels) == 0 {
		// No channels listed = unsubscribe all.
		sess.subClockHalt = false
		sess.subBPHit = false
		sess.subState = false
		sess.subTapAll = false
		sess.subTapNames = map[string]bool{}
		return EventsUnsubscribeResult{Unsubscribed: []string{"*"}}, nil
	}
	acc := make([]string, 0, len(p.Channels))
	for _, ch := range p.Channels {
		switch {
		case ch == "clock.halt":
			sess.subClockHalt = false
		case ch == "bp.hit":
			sess.subBPHit = false
		case ch == "state":
			sess.subState = false
		case ch == "tap.*":
			sess.subTapAll = false
		case strings.HasPrefix(ch, "tap.") && len(ch) > 4:
			delete(sess.subTapNames, ch[4:])
		default:
			continue
		}
		acc = append(acc, ch)
	}
	return EventsUnsubscribeResult{Unsubscribed: acc}, nil
}

// runner is the per-session async-run goroutine. It drives the CPU in
// small RunUntil slices so clock.stop can interrupt within a slice,
// emits tap.changed (coalesced) and state.snapshot (cadence) between
// slices, and on natural halt emits bp.hit (if applicable) then
// clock.halt before exiting. Cleanup via deferred close(runDone).
func (sess *Session) runner() {
	defer close(sess.runDone)

	// Snapshot the channels under mu so the loop can poll them
	// without holding the lock during select.
	sess.mu.Lock()
	runCancel := sess.runCancel
	quit := sess.quit
	lastTaps := copyTaps(sess.inst.Taps())
	sess.mu.Unlock()

	tapLastEmit := map[string]time.Time{}
	lastSnapshot := time.Now()

	for {
		select {
		case <-runCancel:
			sess.finishRun("client", instrument.RunResult{})
			return
		case <-quit:
			sess.finishRunSilent()
			return
		default:
		}

		sess.mu.Lock()
		r := sess.inst.RunUntil(sliceHalves)
		curTaps := copyTaps(sess.inst.Taps())
		sess.mu.Unlock()

		sess.emitTapChanges(lastTaps, curTaps, tapLastEmit, r.HalfCycles)
		lastTaps = curTaps

		sess.mu.Lock()
		wantState := sess.subState
		interval := time.Duration(sess.stateIntervalMs) * time.Millisecond
		sess.mu.Unlock()
		if wantState && time.Since(lastSnapshot) >= interval {
			sess.emitStateSnapshot()
			lastSnapshot = time.Now()
		}

		if r.Reason != "budget" {
			// Natural halt: breakpoint or vector.
			sess.finishRun(translateRunReason(r.Reason), r)
			return
		}
	}
}

// finishRun clears runActive and emits bp.hit + clock.halt according
// to subscriptions. `reason` is the wire reason; `r` carries the
// Instrument's data (HalfCycles, Addr) when applicable.
func (sess *Session) finishRun(reason string, r instrument.RunResult) {
	sess.mu.Lock()
	sess.inst.SetRunning(false)
	sess.runActive = false
	st := snapshot(sess.inst)
	bpID := ""
	if r.Reason == "breakpoint" {
		bpID = lookupBPID(sess, r)
	}
	subBP := sess.subBPHit
	subHalt := sess.subClockHalt
	sess.mu.Unlock()

	addr := st.PC
	if r.Reason != "" {
		addr = r.Addr
	}
	if r.Reason == "breakpoint" && subBP {
		sess.sendNotification("bp.hit", BPHitPayload{
			ID: bpID, Addr: r.Addr, State: st,
		})
	}
	if subHalt {
		sess.sendNotification("clock.halt", RunResult{
			HalfCycles: r.HalfCycles,
			Reason:     reason,
			Addr:       addr,
			BpID:       bpID,
			State:      st,
		})
	}
}

// finishRunSilent is the teardown path — Serve is exiting, do not emit
// notifications (the Conn is about to close).
func (sess *Session) finishRunSilent() {
	sess.mu.Lock()
	sess.inst.SetRunning(false)
	sess.runActive = false
	sess.mu.Unlock()
}

// emitTapChanges diffs prev→cur and, for tap names the session
// subscribed to (either specifically or via tap.*), emits tap.changed —
// throttled per channel by tapEmitMinInterval.
func (sess *Session) emitTapChanges(prev, cur map[string]uint64, lastEmit map[string]time.Time, halfCycles uint64) {
	sess.mu.Lock()
	subAll := sess.subTapAll
	specific := make(map[string]bool, len(sess.subTapNames))
	for k := range sess.subTapNames {
		specific[k] = true
	}
	sess.mu.Unlock()
	if !subAll && len(specific) == 0 {
		return
	}
	now := time.Now()
	for name, v := range cur {
		if prev[name] == v {
			continue
		}
		if !(subAll || specific[name]) {
			continue
		}
		if last, ok := lastEmit[name]; ok && now.Sub(last) < tapEmitMinInterval {
			continue
		}
		lastEmit[name] = now
		sess.sendNotification("tap.changed", TapChangedPayload{
			Name: name, Value: v, HalfCycles: halfCycles,
		})
	}
}

// emitStateSnapshot pushes one state.snapshot notification. Caller
// ensures the cadence (and that the client subscribed to "state").
func (sess *Session) emitStateSnapshot() {
	sess.mu.Lock()
	st := snapshot(sess.inst)
	taps := copyTaps(sess.inst.Taps())
	sess.mu.Unlock()
	sess.sendNotification("state.snapshot", StateSnapshotPayload{
		State: st, Taps: taps,
	})
}

func copyTaps(m map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
