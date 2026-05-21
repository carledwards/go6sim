package bridge_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/carledwards/go6sim/bridge"
	"github.com/carledwards/go6sim/machine"
)

// stubLoader bridges the bridge package to the concrete machine presets
// without bridge itself importing machine — exactly the role
// cmd/6502-sim-serve will play in production. Lives in the test so the
// bridge package stays preset-agnostic.
type stubLoader struct{}

func (stubLoader) Presets() []bridge.PresetInfo {
	return []bridge.PresetInfo{
		{Name: "teach-min", Label: "Teach Minimal", Summary: "RAM + framebuffer RAM + VIA + ROM"},
		{Name: "teach-merlin", Label: "Teach Merlin", Summary: "RAM + two VIAs + ROM (multi-VIA)"},
		{Name: "vic-demo", Label: "VIC Demo", Summary: "standalone VIC display demo"},
	}
}

func (stubLoader) Load(name string, image []byte) (*bridge.Hub, func(), error) {
	var m *machine.Machine
	var regs []bridge.Region
	switch name {
	case "teach-min":
		m = machine.TeachMin()
		regs = []bridge.Region{
			{Name: "ram", Lo: 0x0000, Hi: 0x1FFF, ReadOnly: false},
			{Name: "framebuffer", Lo: 0xA000, Hi: 0xAFFF, ReadOnly: false},
			{Name: "via1", Lo: 0xB000, Hi: 0xB00F, ReadOnly: false},
			{Name: "rom", Lo: 0xE000, Hi: 0xFFFF, ReadOnly: true},
		}
	case "teach-merlin":
		m = machine.TeachMerlin()
		regs = []bridge.Region{
			{Name: "ram", Lo: 0x0000, Hi: 0x1FFF, ReadOnly: false},
			{Name: "via1", Lo: 0xB000, Hi: 0xB00F, ReadOnly: false},
			{Name: "via2", Lo: 0xB100, Hi: 0xB10F, ReadOnly: false},
			{Name: "rom", Lo: 0xE000, Hi: 0xFFFF, ReadOnly: true},
		}
	case "vic-demo":
		m = machine.VICDemo()
	default:
		return nil, nil, fmt.Errorf("unknown preset %q", name)
	}
	if len(image) > 0 {
		if err := m.Load(image); err != nil {
			return nil, nil, err
		}
	}
	// Tests want unpaced execution; the default 10 Hz makes everything
	// time out under the new Hub pacing path.
	m.Inst.Driver().SetSpeedHz(0)
	hub := bridge.NewHub(m.Inst, regs, name)
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)
	cleanup := func() {
		cancel()
		<-hub.Done()
	}
	return hub, cleanup, nil
}

// client is a tiny in-process JSON-RPC client over a Conn — only what
// the round-trip test needs. Production clients (VS Code, MCP) are
// substantial but built on the exact same envelope shape.
type client struct {
	conn bridge.Conn
	id   int
}

func (c *client) call(t *testing.T, method string, params any) (json.RawMessage, *bridge.Error) {
	t.Helper()
	c.id++
	idRaw, _ := json.Marshal(c.id)
	req := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(idRaw), "method": method}
	if params != nil {
		req["params"] = params
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if err := c.conn.Write(b); err != nil {
		t.Fatalf("write: %v", err)
	}
	frame, err := c.conn.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var rsp struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *bridge.Error   `json:"error"`
	}
	if err := json.Unmarshal(frame, &rsp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return rsp.Result, rsp.Error
}

// TestHelloAndCPUState round-trips the minimum useful conversation
// end-to-end through the bridge: enforce the pre-conditions, advance
// the state machine (hello → load → query), and check the returned
// CPUState matches a freshly-reset interp machine. Also asserts the
// error codes for the three guard transitions and an unknown method.
func TestHelloAndCPUState(t *testing.T) {
	serverConn, clientConn := bridge.Pipe()
	srv := bridge.NewServer(stubLoader{})

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, serverConn) }()
	defer func() {
		cancel()
		_ = clientConn.Close()
		select {
		case <-serveErr:
		case <-time.After(time.Second):
			t.Fatal("server did not return after cancel")
		}
	}()

	c := &client{conn: clientConn}

	// 1. cpu.state before hello -> NotInitialised.
	if _, err := c.call(t, "cpu.state", nil); err == nil || err.Code != bridge.CodeNotInitialised {
		t.Fatalf("cpu.state before hello: got err=%v, want NotInitialised", err)
	}

	// 2. hello succeeds and advertises the protocol + capabilities.
	res, errObj := c.call(t, "hello", bridge.HelloParams{
		ClientName: "bridge-test", ClientVersion: "0.0.0",
		Protocols: []string{"1.0"},
	})
	if errObj != nil {
		t.Fatalf("hello: %v", errObj)
	}
	var hr bridge.HelloResult
	if err := json.Unmarshal(res, &hr); err != nil {
		t.Fatalf("hello decode: %v", err)
	}
	if hr.Protocol != bridge.Protocol {
		t.Fatalf("hello protocol = %q, want %q", hr.Protocol, bridge.Protocol)
	}
	if hr.ServerVersion != bridge.ServerVersion {
		t.Fatalf("hello serverVersion = %q, want %q", hr.ServerVersion, bridge.ServerVersion)
	}
	if len(hr.Capabilities) == 0 {
		t.Fatalf("hello capabilities empty; expected at least cpu.state + machine.list")
	}

	// 3. cpu.state after hello but before machine.load -> NoMachine.
	if _, err := c.call(t, "cpu.state", nil); err == nil || err.Code != bridge.CodeNoMachine {
		t.Fatalf("cpu.state before machine.load: got err=%v, want NoMachine", err)
	}

	// 4. machine.list returns the stub presets.
	res, errObj = c.call(t, "machine.list", nil)
	if errObj != nil {
		t.Fatalf("machine.list: %v", errObj)
	}
	var mlr bridge.MachineListResult
	if err := json.Unmarshal(res, &mlr); err != nil {
		t.Fatalf("machine.list decode: %v", err)
	}
	if len(mlr.Presets) < 2 {
		t.Fatalf("machine.list presets = %v, want at least teach-min + teach-merlin", mlr.Presets)
	}

	// 5. machine.load teach-min.
	res, errObj = c.call(t, "machine.load", bridge.MachineLoadParams{Preset: "teach-min"})
	if errObj != nil {
		t.Fatalf("machine.load: %v", errObj)
	}
	var lr bridge.MachineLoadResult
	if err := json.Unmarshal(res, &lr); err != nil {
		t.Fatalf("machine.load decode: %v", err)
	}
	if lr.Preset != "teach-min" {
		t.Fatalf("machine.load preset = %q, want teach-min", lr.Preset)
	}
	if len(lr.Regions) == 0 {
		t.Fatalf("machine.load regions empty; expected ram/framebuffer/via1/rom")
	}
	// Verify the human-readable fields from the loader's Presets list
	// round-tripped onto the load response — clients render these
	// rather than the slug.
	if lr.Label != "Teach Minimal" {
		t.Errorf("machine.load label = %q, want %q", lr.Label, "Teach Minimal")
	}
	if lr.Summary == "" {
		t.Errorf("machine.load summary empty; want loader's preset summary")
	}

	// 6. cpu.state now succeeds and reports a freshly-reset interp
	//    CPU (SP=$FD per the 6502 reset sequence, not running yet).
	res, errObj = c.call(t, "cpu.state", nil)
	if errObj != nil {
		t.Fatalf("cpu.state: %v", errObj)
	}
	var st bridge.CPUState
	if err := json.Unmarshal(res, &st); err != nil {
		t.Fatalf("cpu.state decode: %v", err)
	}
	if st.SP != 0xFD {
		t.Errorf("CPUState.SP = $%02X, want $FD (interp reset value)", st.SP)
	}
	if st.Running {
		t.Errorf("CPUState.Running = true, want false (machine reset, not running)")
	}

	// 7. Unknown method -> MethodNotFound (sanity-check dispatch).
	if _, err := c.call(t, "no.such.method", nil); err == nil || err.Code != bridge.CodeMethodNotFound {
		t.Fatalf("no.such.method: got err=%v, want MethodNotFound", err)
	}

	// (v1 used to test "machine.load nope after load → UnknownPreset"
	// here; v2 phase A allows machine.load to re-image an existing
	// hub, so the loader is invoked first and returns the unknown-
	// preset error. The shape changed; the assertion is dropped in
	// favour of TestUnknownPresetBeforeLoad below.)
}

// TestUnknownPresetBeforeLoad verifies UnknownPreset still surfaces
// when the very first machine.load names an unknown preset.
func TestUnknownPresetBeforeLoad(t *testing.T) {
	serverConn, clientConn := bridge.Pipe()
	srv := bridge.NewServer(stubLoader{})
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx, serverConn)
	defer func() {
		cancel()
		_ = clientConn.Close()
	}()
	c := &client{conn: clientConn}
	_, _ = c.call(t, "hello", bridge.HelloParams{Protocols: []string{bridge.Protocol}})
	if _, err := c.call(t, "machine.load", bridge.MachineLoadParams{Preset: "nope"}); err == nil || err.Code != bridge.CodeUnknownPreset {
		t.Fatalf("machine.load nope (fresh session): got err=%v, want UnknownPreset", err)
	}
}

// --- shared helpers for the handler tests ---

// setupSession spins up a server + client pair, runs hello, and loads
// the named preset (optionally with a ROM image). Returns the client
// and a cleanup closure the caller must defer.
func setupSession(t *testing.T, preset string, image []byte) (*client, func()) {
	t.Helper()
	serverConn, clientConn := bridge.Pipe()
	srv := bridge.NewServer(stubLoader{})

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, serverConn) }()
	cleanup := func() {
		cancel()
		_ = clientConn.Close()
		select {
		case <-serveErr:
		case <-time.After(time.Second):
			t.Fatal("server did not return after cancel")
		}
	}

	c := &client{conn: clientConn}
	if _, err := c.call(t, "hello", bridge.HelloParams{
		ClientName: "handler-test", ClientVersion: "0.0.0",
		Protocols: []string{bridge.Protocol},
	}); err != nil {
		cleanup()
		t.Fatalf("hello: %v", err)
	}
	var img *bridge.Image
	if len(image) > 0 {
		img = &bridge.Image{
			BytesB64: base64.StdEncoding.EncodeToString(image),
			Origin:   0xE000,
		}
	}
	if _, err := c.call(t, "machine.load", bridge.MachineLoadParams{
		Preset: preset, Image: img,
	}); err != nil {
		cleanup()
		t.Fatalf("machine.load: %v", err)
	}
	return c, cleanup
}

func b64dec(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	return b
}

// tinyProg = LDA #$2A ; STA $0010 ; BRK (6 bytes; ROM-loaded at $E000).
// machine.Load(image) lays these into ROM and points the reset vector
// at $E000, so a fresh machine boots into this program.
var tinyProg = []byte{0xA9, 0x2A, 0x8D, 0x10, 0x00, 0x00}

// TestClockControl exercises clock.step / stepCycle / advance / running
// / setSpeedHz / runUntil against a real program loaded over the wire.
// Step semantics: instruction 1 leaves A=$2A; instruction 2 stores it
// at $0010; instruction 3 (BRK) vectors. The advance + runUntil paths
// are reachable from a fresh-loaded machine without needing the
// asynchronous run loop (that's Phase 1c-2).
func TestClockControl(t *testing.T) {
	c, cleanup := setupSession(t, "teach-min", tinyProg)
	defer cleanup()

	// clock.running on a fresh machine.
	res, errObj := c.call(t, "clock.running", nil)
	if errObj != nil {
		t.Fatalf("clock.running: %v", errObj)
	}
	var rn bridge.ClockRunningResult
	_ = json.Unmarshal(res, &rn)
	if rn.Running {
		t.Errorf("clock.running.Running = true, want false at reset")
	}

	// clock.setSpeedHz to a known table value.
	res, errObj = c.call(t, "clock.setSpeedHz", bridge.ClockSetSpeedParams{Hz: 1000})
	if errObj != nil {
		t.Fatalf("clock.setSpeedHz: %v", errObj)
	}
	var sp bridge.ClockSetSpeedResult
	_ = json.Unmarshal(res, &sp)
	if sp.Hz != 1000 {
		t.Errorf("clock.setSpeedHz: got Hz=%d, want 1000", sp.Hz)
	}

	// clock.setSpeedHz with a value not in the supported table -> InvalidParams.
	if _, err := c.call(t, "clock.setSpeedHz", bridge.ClockSetSpeedParams{Hz: 1234}); err == nil || err.Code != bridge.CodeInvalidParams {
		t.Fatalf("clock.setSpeedHz 1234: got err=%v, want InvalidParams", err)
	}

	// clock.step n=1 -> after LDA #$2A, A=$2A.
	res, errObj = c.call(t, "clock.step", bridge.ClockStepParams{N: 1})
	if errObj != nil {
		t.Fatalf("clock.step: %v", errObj)
	}
	var st bridge.ClockStepResult
	_ = json.Unmarshal(res, &st)
	if st.State.A != 0x2A {
		t.Errorf("after step: A=$%02X, want $2A (LDA #$2A)", st.State.A)
	}

	// clock.step n=1 -> STA $0010; verify with mem.peek that $0010 == $2A.
	if _, err := c.call(t, "clock.step", bridge.ClockStepParams{N: 1}); err != nil {
		t.Fatalf("clock.step 2: %v", err)
	}
	res, errObj = c.call(t, "mem.peek", bridge.MemPeekParams{Addr: 0x0010, N: 1})
	if errObj != nil {
		t.Fatalf("mem.peek $0010: %v", errObj)
	}
	var mp bridge.MemPeekResult
	_ = json.Unmarshal(res, &mp)
	if got := b64dec(t, mp.BytesB64); len(got) != 1 || got[0] != 0x2A {
		t.Errorf("mem[$0010] = %v, want [$2A]", got)
	}

	// clock.advance for a short window — exercises the surface; we just
	// assert it returns a positive halfCycles and a fresh state.
	res, errObj = c.call(t, "clock.advance", bridge.ClockAdvanceParams{DurationUs: 1000})
	if errObj != nil {
		t.Fatalf("clock.advance: %v", errObj)
	}
	var av bridge.ClockAdvanceResult
	_ = json.Unmarshal(res, &av)
	if av.HalfCycles < 0 {
		t.Errorf("clock.advance halfCycles = %d, want >=0", av.HalfCycles)
	}
}

// TestRunUntilHitsBreakpoint loads the same tiny program, arms a bp at
// $E005 (the BRK), and runs. The bridge should stop with
// reason="breakpoint", addr=$E005, bpId matching the id returned by
// bp.set — proof end-to-end that the bridge's id bookkeeping aligns
// with the Instrument's address bp.
func TestRunUntilHitsBreakpoint(t *testing.T) {
	c, cleanup := setupSession(t, "teach-min", tinyProg)
	defer cleanup()

	res, errObj := c.call(t, "bp.set", bridge.BPSetParams{Addr: 0xE005})
	if errObj != nil {
		t.Fatalf("bp.set: %v", errObj)
	}
	var bs bridge.BPSetResult
	_ = json.Unmarshal(res, &bs)
	if bs.ID == "" || bs.Addr != 0xE005 {
		t.Fatalf("bp.set: id=%q addr=$%04X, want non-empty id + $E005", bs.ID, bs.Addr)
	}

	res, errObj = c.call(t, "clock.runUntil", bridge.ClockRunUntilParams{MaxHalfCycles: 1000})
	if errObj != nil {
		t.Fatalf("clock.runUntil: %v", errObj)
	}
	var rr bridge.RunResult
	_ = json.Unmarshal(res, &rr)
	if rr.Reason != "breakpoint" {
		t.Errorf("RunResult.Reason = %q, want \"breakpoint\"", rr.Reason)
	}
	if rr.Addr != 0xE005 {
		t.Errorf("RunResult.Addr = $%04X, want $E005", rr.Addr)
	}
	if rr.BpID != bs.ID {
		t.Errorf("RunResult.BpID = %q, want %q", rr.BpID, bs.ID)
	}
}

// TestBreakpointBookkeeping covers bp.set + setVector + list + clear
// (single + all) and the UnknownBP error path. Doesn't actually run
// the CPU — purely the bridge's id/state book against the Instrument.
func TestBreakpointBookkeeping(t *testing.T) {
	c, cleanup := setupSession(t, "teach-min", nil)
	defer cleanup()

	res, _ := c.call(t, "bp.set", bridge.BPSetParams{Addr: 0xE100})
	var a1 bridge.BPSetResult
	_ = json.Unmarshal(res, &a1)

	res, _ = c.call(t, "bp.set", bridge.BPSetParams{Addr: 0xE200})
	var a2 bridge.BPSetResult
	_ = json.Unmarshal(res, &a2)

	res, _ = c.call(t, "bp.setVector", bridge.BPSetVectorParams{Vector: "irq"})
	var v1 bridge.BPSetVectorResult
	_ = json.Unmarshal(res, &v1)

	res, _ = c.call(t, "bp.list", nil)
	var lst bridge.BPListResult
	_ = json.Unmarshal(res, &lst)
	if len(lst.Breakpoints) != 3 {
		t.Fatalf("bp.list len = %d, want 3", len(lst.Breakpoints))
	}

	// Clear a single bp by id.
	res, _ = c.call(t, "bp.clear", bridge.BPClearParams{ID: a1.ID})
	var cl bridge.BPClearResult
	_ = json.Unmarshal(res, &cl)
	if cl.Cleared != 1 {
		t.Errorf("bp.clear one: got Cleared=%d, want 1", cl.Cleared)
	}

	// Unknown id -> UnknownBP.
	if _, err := c.call(t, "bp.clear", bridge.BPClearParams{ID: "bp_nope"}); err == nil || err.Code != bridge.CodeUnknownBP {
		t.Fatalf("bp.clear bp_nope: got err=%v, want UnknownBP", err)
	}

	// Invalid vector name -> InvalidParams.
	if _, err := c.call(t, "bp.setVector", bridge.BPSetVectorParams{Vector: "wat"}); err == nil || err.Code != bridge.CodeInvalidParams {
		t.Fatalf("bp.setVector wat: got err=%v, want InvalidParams", err)
	}

	// Clear all.
	res, _ = c.call(t, "bp.clear", nil)
	_ = json.Unmarshal(res, &cl)
	if cl.Cleared != 2 {
		t.Errorf("bp.clear all: got Cleared=%d, want 2 (one addr + one vector remaining)", cl.Cleared)
	}

	res, _ = c.call(t, "bp.list", nil)
	_ = json.Unmarshal(res, &lst)
	if len(lst.Breakpoints) != 0 {
		t.Errorf("bp.list after clear all: len=%d, want 0", len(lst.Breakpoints))
	}
}

// TestMemoryAndTaps covers mem.peek + mem.poke + mem.read by-region +
// mem.frame, and taps.list + taps.read. Uses teach-min for the
// framebuffer region presence.
func TestMemoryAndTaps(t *testing.T) {
	c, cleanup := setupSession(t, "teach-min", nil)
	defer cleanup()

	// Poke a few bytes into RAM and peek them back.
	in := []byte{0x11, 0x22, 0x33, 0x44}
	if _, err := c.call(t, "mem.poke", bridge.MemPokeParams{
		Addr:     0x0040,
		BytesB64: base64.StdEncoding.EncodeToString(in),
	}); err != nil {
		t.Fatalf("mem.poke: %v", err)
	}
	res, _ := c.call(t, "mem.peek", bridge.MemPeekParams{Addr: 0x0040, N: 4})
	var mp bridge.MemPeekResult
	_ = json.Unmarshal(res, &mp)
	if got := b64dec(t, mp.BytesB64); !bytesEq(got, in) {
		t.Errorf("mem.peek: got %v, want %v", got, in)
	}

	// mem.read by region: ram contains the bytes we just poked.
	res, _ = c.call(t, "mem.read", bridge.MemReadParams{Region: "ram"})
	var mr bridge.MemReadResult
	_ = json.Unmarshal(res, &mr)
	if mr.Addr != 0x0000 || mr.Length != 0x2000 {
		t.Errorf("mem.read ram: addr=$%04X len=%d, want $0000 8192", mr.Addr, mr.Length)
	}
	ram := b64dec(t, mr.BytesB64)
	if len(ram) != 0x2000 || !bytesEq(ram[0x40:0x44], in) {
		t.Errorf("mem.read ram[$40..$43] != poked bytes")
	}

	// mem.frame -> framebuffer region for teach-min.
	res, _ = c.call(t, "mem.frame", nil)
	var fr bridge.MemReadResult
	_ = json.Unmarshal(res, &fr)
	if fr.Addr != 0xA000 || fr.Length != 0x1000 {
		t.Errorf("mem.frame: addr=$%04X len=%d, want $A000 4096", fr.Addr, fr.Length)
	}

	// mem.read with unknown region -> BusError.
	if _, err := c.call(t, "mem.read", bridge.MemReadParams{Region: "nope"}); err == nil || err.Code != bridge.CodeBusError {
		t.Fatalf("mem.read nope: got err=%v, want BusError", err)
	}

	// taps.list returns the live aggregated tap names. We just check
	// the surface — exact tap names depend on the VIA implementation
	// and may evolve; here we only assert "we got some".
	res, _ = c.call(t, "taps.list", nil)
	var tl bridge.TapsListResult
	_ = json.Unmarshal(res, &tl)
	if len(tl.Taps) == 0 {
		t.Errorf("taps.list returned no taps; expected via1.* aggregations")
	}

	// taps.read all.
	res, _ = c.call(t, "taps.read", nil)
	var tr bridge.TapsReadResult
	_ = json.Unmarshal(res, &tr)
	if len(tr.Values) != len(tl.Taps) {
		t.Errorf("taps.read all: got %d values, want %d (from taps.list)", len(tr.Values), len(tl.Taps))
	}

	// taps.read filtered by names. Use a fresh result struct — Go's
	// json.Unmarshal *merges* into an existing map rather than
	// replacing it, so reusing `tr` would silently keep the keys from
	// the previous all-call.
	if len(tl.Taps) > 0 {
		first := tl.Taps[0].Name
		res, _ = c.call(t, "taps.read", bridge.TapsReadParams{Names: []string{first}})
		var trFiltered bridge.TapsReadResult
		_ = json.Unmarshal(res, &trFiltered)
		if len(trFiltered.Values) != 1 {
			t.Errorf("taps.read filtered: got %d values, want 1", len(trFiltered.Values))
		}
	}
}

func bytesEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- async client (for the event-bearing surface) ---

// notification mirrors a JSON-RPC 2.0 notification frame the bridge
// pushes server→client.
type notification struct {
	Method string
	Params json.RawMessage
}

// asyncClient is a tiny in-process JSON-RPC client that demuxes
// responses (by id) from notifications (no id). One reader goroutine
// owns the Conn read; callers go through `call` for requests and
// `awaitNotify` (or the `notify` channel) for server pushes.
type asyncClient struct {
	conn bridge.Conn

	mu      sync.Mutex
	id      int
	pending map[int]chan rawResponse

	notify chan notification
	done   chan struct{}
}

type rawResponse struct {
	Result json.RawMessage
	Err    *bridge.Error
}

func newAsyncClient(conn bridge.Conn) *asyncClient {
	c := &asyncClient{
		conn:    conn,
		pending: map[int]chan rawResponse{},
		notify:  make(chan notification, 128),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

func (c *asyncClient) readLoop() {
	defer close(c.done)
	for {
		frame, err := c.conn.Read()
		if err != nil {
			return
		}
		var raw struct {
			ID     *json.RawMessage `json:"id"`
			Result json.RawMessage  `json:"result"`
			Error  *bridge.Error    `json:"error"`
			Method string           `json:"method"`
			Params json.RawMessage  `json:"params"`
		}
		if err := json.Unmarshal(frame, &raw); err != nil {
			continue
		}
		if raw.ID != nil {
			var id int
			_ = json.Unmarshal(*raw.ID, &id)
			c.mu.Lock()
			ch, ok := c.pending[id]
			if ok {
				delete(c.pending, id)
			}
			c.mu.Unlock()
			if ok {
				ch <- rawResponse{Result: raw.Result, Err: raw.Error}
			}
			continue
		}
		// Notification.
		select {
		case c.notify <- notification{Method: raw.Method, Params: raw.Params}:
		default:
			// Buffer full — drop oldest implicitly by skipping. Tests
			// should drain `notify` if they expect heavy event volume.
		}
	}
}

func (c *asyncClient) call(method string, params any) (json.RawMessage, *bridge.Error) {
	c.mu.Lock()
	c.id++
	id := c.id
	ch := make(chan rawResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	idRaw, _ := json.Marshal(id)
	req := map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(idRaw), "method": method,
	}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	if err := c.conn.Write(b); err != nil {
		return nil, &bridge.Error{Code: -1, Message: err.Error()}
	}
	select {
	case r := <-ch:
		return r.Result, r.Err
	case <-time.After(3 * time.Second):
		return nil, &bridge.Error{Code: -2, Message: "timeout"}
	}
}

// awaitNotify blocks until a notification matching `method` arrives,
// or t.Fatal on timeout.
func (c *asyncClient) awaitNotify(t *testing.T, method string, timeout time.Duration) json.RawMessage {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case n := <-c.notify:
			if n.Method == method {
				return n.Params
			}
			// Different notification — keep looking.
		case <-deadline:
			t.Fatalf("timeout waiting for notification %q", method)
		}
	}
}

func setupAsyncSession(t *testing.T, preset string, image []byte) (*asyncClient, func()) {
	t.Helper()
	serverConn, clientConn := bridge.Pipe()
	srv := bridge.NewServer(stubLoader{})

	ctx, cancel := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx, serverConn) }()
	ac := newAsyncClient(clientConn)
	cleanup := func() {
		cancel()
		_ = clientConn.Close()
		<-ac.done
		select {
		case <-serveErr:
		case <-time.After(2 * time.Second):
			t.Fatal("server did not return after cancel")
		}
	}

	if _, err := ac.call("hello", bridge.HelloParams{
		ClientName: "async-test", ClientVersion: "0.0.0",
		Protocols: []string{bridge.Protocol},
	}); err != nil {
		cleanup()
		t.Fatalf("hello: %v", err)
	}
	var img *bridge.Image
	if len(image) > 0 {
		img = &bridge.Image{
			BytesB64: base64.StdEncoding.EncodeToString(image),
			Origin:   0xE000,
		}
	}
	if _, err := ac.call("machine.load", bridge.MachineLoadParams{
		Preset: preset, Image: img,
	}); err != nil {
		cleanup()
		t.Fatalf("machine.load: %v", err)
	}
	return ac, cleanup
}

// TestClockRunHitsBreakpoint runs the tiny program until a bp at $E005
// fires asynchronously. Both bp.hit and clock.halt notifications must
// arrive, the bp.hit id must match the id bp.set returned, and the
// clock.halt RunResult must carry reason="breakpoint", addr=$E005.
func TestClockRunHitsBreakpoint(t *testing.T) {
	ac, cleanup := setupAsyncSession(t, "teach-min", tinyProg)
	defer cleanup()

	if _, err := ac.call("events.subscribe", bridge.EventsSubscribeParams{
		Channels: []string{"clock.halt", "bp.hit"},
	}); err != nil {
		t.Fatalf("events.subscribe: %v", err)
	}

	res, _ := ac.call("bp.set", bridge.BPSetParams{Addr: 0xE005})
	var bs bridge.BPSetResult
	_ = json.Unmarshal(res, &bs)

	if _, err := ac.call("clock.run", nil); err != nil {
		t.Fatalf("clock.run: %v", err)
	}

	hit := ac.awaitNotify(t, "bp.hit", 2*time.Second)
	var hp bridge.BPHitPayload
	_ = json.Unmarshal(hit, &hp)
	if hp.Addr != 0xE005 {
		t.Errorf("bp.hit Addr=$%04X, want $E005", hp.Addr)
	}
	if hp.ID != bs.ID {
		t.Errorf("bp.hit ID=%q, want %q", hp.ID, bs.ID)
	}

	halt := ac.awaitNotify(t, "clock.halt", 2*time.Second)
	var rr bridge.RunResult
	_ = json.Unmarshal(halt, &rr)
	if rr.Reason != "breakpoint" {
		t.Errorf("clock.halt Reason=%q, want breakpoint", rr.Reason)
	}
	if rr.Addr != 0xE005 {
		t.Errorf("clock.halt Addr=$%04X, want $E005", rr.Addr)
	}
}

// TestClockStopAsync runs an infinite-loop program and verifies that
// clock.stop both returns a ClockStopResult with reason="client" AND
// causes the runner to emit a clock.halt notification with the same
// reason.
func TestClockStopAsync(t *testing.T) {
	// JMP $E000 — infinite loop. Three bytes loaded at ROM origin.
	infinite := []byte{0x4C, 0x00, 0xE0}
	ac, cleanup := setupAsyncSession(t, "teach-min", infinite)
	defer cleanup()

	_, _ = ac.call("events.subscribe", bridge.EventsSubscribeParams{
		Channels: []string{"clock.halt"},
	})
	if _, err := ac.call("clock.run", nil); err != nil {
		t.Fatalf("clock.run: %v", err)
	}

	// Let the runner cycle a few times so we know it's really running.
	time.Sleep(50 * time.Millisecond)

	res, err := ac.call("clock.stop", nil)
	if err != nil {
		t.Fatalf("clock.stop: %v", err)
	}
	var sp bridge.ClockStopResult
	_ = json.Unmarshal(res, &sp)
	if sp.Reason != "client" {
		t.Errorf("clock.stop Reason=%q, want client", sp.Reason)
	}

	halt := ac.awaitNotify(t, "clock.halt", 2*time.Second)
	var rr bridge.RunResult
	_ = json.Unmarshal(halt, &rr)
	if rr.Reason != "client" {
		t.Errorf("clock.halt Reason=%q, want client", rr.Reason)
	}
}

// TestStateSnapshotPeriodic verifies that state.snapshot notifications
// fire on the default cadence (≈100 ms) while subscribed to the
// "state.snapshot" topic. End-to-end variant of the hub-level test:
// proves the events actually make it across the JSON-RPC wire to the
// session, not just to in-process subscribers.
func TestStateSnapshotPeriodic(t *testing.T) {
	infinite := []byte{0x4C, 0x00, 0xE0}
	ac, cleanup := setupAsyncSession(t, "teach-min", infinite)
	defer cleanup()

	if _, err := ac.call("events.subscribe", bridge.EventsSubscribeParams{
		Channels: []string{"state.snapshot"},
	}); err != nil {
		t.Fatalf("events.subscribe: %v", err)
	}
	if _, err := ac.call("clock.run", nil); err != nil {
		t.Fatalf("clock.run: %v", err)
	}

	// Wait 350 ms — at 100 ms cadence we expect at least 2 snapshots.
	deadline := time.After(350 * time.Millisecond)
	count := 0
loop:
	for {
		select {
		case n := <-ac.notify:
			if n.Method == "state.snapshot" {
				count++
			}
		case <-deadline:
			break loop
		}
	}
	if count < 2 {
		t.Errorf("state.snapshot count=%d, want >=2 in 350ms at 100ms cadence", count)
	}

	if _, err := ac.call("clock.stop", nil); err != nil {
		t.Fatalf("clock.stop: %v", err)
	}
}

// TestClockRunStateErrors covers the state-machine guards:
// clock.stop without an active run → NotInRun; clock.run while
// already running → NotInRun.
func TestClockRunStateErrors(t *testing.T) {
	infinite := []byte{0x4C, 0x00, 0xE0}
	ac, cleanup := setupAsyncSession(t, "teach-min", infinite)
	defer cleanup()

	// v2 Phase A note: in v1, clock.stop-while-idle returned
	// NotInRun and clock.run-while-running returned NotInRun. In v2
	// both are fire-and-forget no-ops at idempotent boundaries —
	// the assertion is dropped. (A future "strict" flag could
	// restore the v1 errors, but it's not the default v2 stance.)
	_, _ = ac.call("clock.stop", nil) // no-op when idle in v2

	_, _ = ac.call("events.subscribe", bridge.EventsSubscribeParams{
		Channels: []string{"clock.halt"},
	})
	if _, err := ac.call("clock.run", nil); err != nil {
		t.Fatalf("clock.run: %v", err)
	}
	// Issuing clock.run again under v2 is a no-op (already running).
	if _, err := ac.call("clock.run", nil); err != nil {
		t.Fatalf("clock.run twice (v2 no-op): got err=%v, want nil", err)
	}

	if _, err := ac.call("clock.stop", nil); err != nil {
		t.Fatalf("clock.stop: %v", err)
	}
}
