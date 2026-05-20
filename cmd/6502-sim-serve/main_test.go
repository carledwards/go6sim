package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/carledwards/go6sim/bridge"
)

// TestEndToEndNDJSON proves the full stack — TCP accept loop +
// ndjsonConn + bridge.Server + presetLoader + machine.TeachMin — works
// over a real local socket. It binds :0 (kernel-assigned port), dials
// in, and round-trips hello → machine.load (with a tiny ROM image) →
// cpu.state, asserting the CPU comes up at the post-reset SP=$FD.
//
// This is the smoke that mirrors what `nc localhost 6502` (or a future
// VS Code extension) would do — proof the wire is what we think it is.
func TestEndToEndNDJSON(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	srv := bridge.NewServer(presetLoader{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loopErr := make(chan error, 1)
	go func() { loopErr <- acceptLoop(ctx, ln, srv) }()

	// Client side.
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	r := bufio.NewReader(conn)
	send := func(id int, method string, params any) {
		t.Helper()
		req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
		if params != nil {
			req["params"] = params
		}
		b, _ := json.Marshal(req)
		if _, err := conn.Write(append(b, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	read := func() (json.RawMessage, *bridge.Error) {
		t.Helper()
		line, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var rsp struct {
			Result json.RawMessage `json:"result"`
			Error  *bridge.Error   `json:"error"`
		}
		if err := json.Unmarshal(line, &rsp); err != nil {
			t.Fatalf("decode: %v (%q)", err, string(line))
		}
		return rsp.Result, rsp.Error
	}

	// hello
	send(1, "hello", bridge.HelloParams{
		ClientName: "e2e", ClientVersion: "0.0.0",
		Protocols: []string{bridge.Protocol},
	})
	if _, e := read(); e != nil {
		t.Fatalf("hello: %v", e)
	}

	// machine.load teach-min with a 6-byte program at $E000.
	tinyProg := []byte{0xA9, 0x2A, 0x8D, 0x10, 0x00, 0x00}
	send(2, "machine.load", bridge.MachineLoadParams{
		Preset: "teach-min",
		Image: &bridge.Image{
			BytesB64: base64.StdEncoding.EncodeToString(tinyProg),
			Origin:   0xE000,
		},
	})
	if _, e := read(); e != nil {
		t.Fatalf("machine.load: %v", e)
	}

	// cpu.state — fresh reset, SP=$FD.
	send(3, "cpu.state", nil)
	res, e := read()
	if e != nil {
		t.Fatalf("cpu.state: %v", e)
	}
	var st bridge.CPUState
	_ = json.Unmarshal(res, &st)
	if st.SP != 0xFD {
		t.Errorf("CPUState.SP = $%02X, want $FD over the wire", st.SP)
	}
	if st.Running {
		t.Errorf("CPUState.Running = true, want false at reset")
	}

	// clock.step once — LDA #$2A; A should become $2A.
	send(4, "clock.step", bridge.ClockStepParams{N: 1})
	res, e = read()
	if e != nil {
		t.Fatalf("clock.step: %v", e)
	}
	var sr bridge.ClockStepResult
	_ = json.Unmarshal(res, &sr)
	if sr.State.A != 0x2A {
		t.Errorf("after step over the wire: A=$%02X, want $2A", sr.State.A)
	}

	// Done. Closing the conn EOFs the server-side Serve cleanly.
	_ = conn.Close()
	cancel()
	_ = ln.Close()
	select {
	case err := <-loopErr:
		if err != nil {
			t.Fatalf("acceptLoop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("acceptLoop did not return after cancel")
	}
}

// TestPresetLoaderShapes is a cheap sanity check that the preset map
// stays in sync with what machine actually constructs. If a preset
// goes away or a region range moves, this fails before the e2e test
// does.
func TestPresetLoaderShapes(t *testing.T) {
	pl := presetLoader{}
	names := map[string]bool{}
	for _, p := range pl.Presets() {
		names[p.Name] = true
	}
	for _, want := range []string{"teach-min", "teach-merlin", "vic-demo"} {
		if !names[want] {
			t.Errorf("Presets missing %q", want)
		}
		inst, regs, err := pl.Load(want, nil)
		if err != nil {
			t.Errorf("Load %q: %v", want, err)
			continue
		}
		if inst == nil {
			t.Errorf("Load %q: nil Instrument", want)
		}
		if len(regs) == 0 {
			t.Errorf("Load %q: no regions", want)
		}
	}
	if _, _, err := pl.Load("nope", nil); err == nil {
		t.Errorf("Load nope: want error, got nil")
	}
}
