package bridge_test

import (
	"context"
	"testing"
	"time"

	"github.com/carledwards/go6sim/bridge"
	"github.com/carledwards/go6sim/machine"
)

// TestHubDirectImplementsTarget — Phase A keystone. Proves that the
// in-process target shares the wire-client's contract: both
// *bridgeclient.Client and *bridge.HubDirect satisfy bridge.Target.
// The compile-time `var _ Target = ...` assertions already block
// regressions; this test exercises a few representative methods at
// runtime so we know the implementations actually work, not just
// type-check.
func TestHubDirectImplementsTarget(t *testing.T) {
	m := machine.TeachMin()
	m.Inst.Driver().SetSpeedHz(0) // unpaced
	if err := m.Load([]byte{
		0xA9, 0x2A, // LDA #$2A
		0x8D, 0x10, 0x00, // STA $0010
		0x4C, 0x05, 0xE0, // JMP $E005 (self-loop on STA's next)
	}); err != nil {
		t.Fatalf("machine.Load: %v", err)
	}
	hub := bridge.NewHub(m.Inst, nil, "teach-min")
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)
	defer func() { cancel(); <-hub.Done() }()

	// Assignable to Target — the in-process equivalent of holding a
	// *bridgeclient.Client. The Monitor REPL will hold one of these
	// and not care which concrete type it is.
	var tgt bridge.Target = bridge.NewHubDirect(hub)

	// CPUState — query.
	st, e := tgt.CPUState()
	if e != nil {
		t.Fatalf("CPUState: %v", e)
	}
	if st.PC != 0xE000 {
		t.Errorf("post-load PC=$%04X, want $E000", st.PC)
	}

	// Step — command + state mutation.
	sr, e := tgt.Step(1)
	if e != nil {
		t.Fatalf("Step: %v", e)
	}
	if sr.State.A != 0x2A {
		t.Errorf("post-step A=$%02X, want $2A (LDA #$2A)", sr.State.A)
	}

	// MemPoke + MemPeek round-trip.
	if _, e := tgt.MemPoke(0x0200, []byte{0xDE, 0xAD, 0xBE, 0xEF}); e != nil {
		t.Fatalf("MemPoke: %v", e)
	}
	buf, e := tgt.MemPeek(0x0200, 4)
	if e != nil {
		t.Fatalf("MemPeek: %v", e)
	}
	want := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	for i, b := range want {
		if buf[i] != b {
			t.Errorf("MemPeek[%d] = $%02X, want $%02X", i, buf[i], b)
		}
	}

	// SetPC — interp implements cpu.PCSetter; HubDirect must surface
	// the post-set state.
	post, e := tgt.SetPC(0xE003)
	if e != nil {
		t.Fatalf("SetPC: %v", e)
	}
	if post.PC != 0xE003 {
		t.Errorf("post-SetPC PC=$%04X, want $E003", post.PC)
	}

	// Notifications — Run, wait briefly, expect at least one
	// state.snapshot.
	notifs := tgt.Notifications()
	if e := tgt.Run(0); e != nil {
		t.Fatalf("Run: %v", e)
	}
	deadline := time.After(500 * time.Millisecond)
	sawSnapshot := false
loop:
	for !sawSnapshot {
		select {
		case n := <-notifs:
			if n.Method == "state.snapshot" {
				sawSnapshot = true
			}
		case <-deadline:
			break loop
		}
	}
	if !sawSnapshot {
		t.Error("no state.snapshot received via HubDirect.Notifications within 500ms")
	}
}
