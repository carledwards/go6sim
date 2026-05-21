package bridgeclient_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/carledwards/go6sim/bridge"
	"github.com/carledwards/go6sim/internal/bridgeclient"
	"github.com/carledwards/go6sim/machine"
)

// stubLoader mirrors cmd/6502-sim-serve's preset loader so the test
// is a faithful end-to-end check: real TCP, real bridge.Server, real
// preset construction. Keeping it inline (instead of importing the
// cmd's loader) keeps the test independent of any cmd binary.
type stubLoader struct{}

func (stubLoader) Presets() []bridge.PresetInfo {
	return []bridge.PresetInfo{
		{Name: "teach-min", Label: "Teach Minimal", Summary: "RAM + framebuffer RAM + 6522 VIA + ROM"},
		{Name: "teach-merlin", Label: "Teach Merlin", Summary: "RAM + two 6522 VIAs + ROM (multi-VIA)"},
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
	default:
		return nil, nil, fmt.Errorf("unknown preset %q", name)
	}
	if len(image) > 0 {
		if err := m.Load(image); err != nil {
			return nil, nil, err
		}
	}
	// Unpaced — see bridge/hub_test.go startHub for rationale.
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

// ndjsonConn duplicates cmd/6502-sim-serve's adapter inline (rather
// than depending on the cmd package). If a third user appears, lift
// both into internal/ndjson/ and de-duplicate then.
type ndjsonConn struct {
	raw net.Conn
	r   *bufio.Reader
	w   *bufio.Writer
}

func (n *ndjsonConn) Read() ([]byte, error) {
	line, err := n.r.ReadBytes('\n')
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line, err
}

func (n *ndjsonConn) Write(b []byte) error {
	if _, err := n.w.Write(b); err != nil {
		return err
	}
	if err := n.w.WriteByte('\n'); err != nil {
		return err
	}
	return n.w.Flush()
}

func (n *ndjsonConn) Close() error { return n.raw.Close() }

// startServer binds :0 + serves one bridge.Server until ctx is
// cancelled. Returns the listener address for the client to dial.
func startServer(t *testing.T) (string, context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := bridge.NewServer(stubLoader{})
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.Serve(ctx, &ndjsonConn{
				raw: c, r: bufio.NewReader(c), w: bufio.NewWriter(c),
			})
		}
	}()

	cleanup := func() {
		cancel()
		_ = ln.Close()
	}
	return ln.Addr().String(), cleanup
}

// TestEndToEnd round-trips the whole client surface — Dial, Hello,
// MachineLoad (with a tiny ROM image), CPUState, Step, and Subscribe —
// against a real bridge.Server over a real local TCP socket. This is
// the same conversation cmd/6502-control will have in A2; if anything
// drifts, this fails first.
func TestEndToEnd(t *testing.T) {
	addr, cleanup := startServer(t)
	defer cleanup()

	c, err := bridgeclient.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	hr, e := c.Hello("client_test", "0.0.0")
	if e != nil {
		t.Fatalf("hello: %v", e)
	}
	if hr.Protocol != bridge.Protocol {
		t.Fatalf("hello protocol = %q, want %q", hr.Protocol, bridge.Protocol)
	}

	// Load tinyProg = LDA #$2A ; STA $0010 ; BRK.
	tinyProg := []byte{0xA9, 0x2A, 0x8D, 0x10, 0x00, 0x00}
	lr, e := c.MachineLoad("teach-min", tinyProg)
	if e != nil {
		t.Fatalf("MachineLoad: %v", e)
	}
	if lr.Preset != "teach-min" {
		t.Fatalf("MachineLoad preset = %q, want teach-min", lr.Preset)
	}

	st, e := c.CPUState()
	if e != nil {
		t.Fatalf("CPUState: %v", e)
	}
	if st.SP != 0xFD {
		t.Errorf("post-reset SP = $%02X, want $FD", st.SP)
	}
	if st.Running {
		t.Errorf("post-reset Running = true, want false")
	}

	sr, e := c.Step(1)
	if e != nil {
		t.Fatalf("Step: %v", e)
	}
	if sr.State.A != 0x2A {
		t.Errorf("after step over the wire: A = $%02X, want $2A", sr.State.A)
	}

	sub, e := c.Subscribe("clock.halt", "bp.hit")
	if e != nil {
		t.Fatalf("Subscribe: %v", e)
	}
	if len(sub.Subscribed) != 2 {
		t.Errorf("subscribed = %v, want 2 channels", sub.Subscribed)
	}
}

// TestNotificationDelivery wires up the async path: subscribe, set a
// bp, start a background run, drain the notification channel for
// bp.hit + clock.halt. Mirrors how the controller's event-render
// goroutine will consume Notifications().
func TestNotificationDelivery(t *testing.T) {
	addr, cleanup := startServer(t)
	defer cleanup()

	c, err := bridgeclient.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if _, e := c.Hello("client_test", "0.0.0"); e != nil {
		t.Fatalf("hello: %v", e)
	}
	tinyProg := []byte{0xA9, 0x2A, 0x8D, 0x10, 0x00, 0x00}
	if _, e := c.MachineLoad("teach-min", tinyProg); e != nil {
		t.Fatalf("MachineLoad: %v", e)
	}
	if _, e := c.Subscribe("clock.halt", "bp.hit"); e != nil {
		t.Fatalf("Subscribe: %v", e)
	}

	// Bp at $E005 (the BRK after STA). Then async run.
	if _, e := c.Call("bp.set", bridge.BPSetParams{Addr: 0xE005}); e != nil {
		t.Fatalf("bp.set: %v", e)
	}
	if e := c.Run(0); e != nil {
		t.Fatalf("clock.run: %v", e)
	}

	got := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for !(got["bp.hit"] && got["clock.halt"]) {
		select {
		case n := <-c.Notifications():
			got[n.Method] = true
		case <-deadline:
			t.Fatalf("missing notifications; got=%v", got)
		}
	}
}

// TestCallCtxTimeout: a Call against a server that never responds (we
// connect to a black-hole listener that just accepts and stalls)
// should respect the context deadline.
func TestCallCtxTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		// Hold open; never reply.
		<-time.After(5 * time.Second)
		_ = nc.Close()
	}()

	c, err := bridgeclient.Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, errObj := c.CallCtx(ctx, "hello", nil)
	if errObj == nil {
		t.Fatalf("expected ctx timeout error, got nil")
	}
}

// TestCloseUnblocksInflightCalls: a Call that's waiting on a response
// must return a transport error (not block forever) when the Client
// is Closed.
func TestCloseUnblocksInflightCalls(t *testing.T) {
	addr, cleanup := startServer(t)
	defer cleanup()

	c, err := bridgeclient.Dial(addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Don't call hello — server will return NotInitialised for the
	// in-flight call we're about to fire, but we close the conn
	// before that response can arrive.
	done := make(chan struct{})
	go func() {
		defer close(done)
		// This call will race with Close; either path should return.
		_, _ = c.CallCtx(context.Background(), "cpu.state", nil)
	}()

	// Give the goroutine a moment to issue the call.
	time.Sleep(50 * time.Millisecond)
	_ = c.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight Call did not return after Close")
	}
}

// Compile-time check: Notification.Params is json.RawMessage so the
// controller can unmarshal into bridge.{RunResult,BPHitPayload,...} at
// the call site without an extra conversion.
var _ = json.RawMessage(bridgeclient.Notification{}.Params)
