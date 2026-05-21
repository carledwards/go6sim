package bridge_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/carledwards/go6sim/bridge"
	"github.com/carledwards/go6sim/machine"
)

// captureSink is a thread-safe event recorder for tests. It mirrors
// what a real session does (marshal + Write under a writeMu); here
// we just append to a slice protected by a mutex.
type captureSink struct {
	mu     sync.Mutex
	events []capturedEvent
}

type capturedEvent struct {
	topic  string
	params any
}

func (s *captureSink) send(topic string, params any) {
	s.mu.Lock()
	s.events = append(s.events, capturedEvent{topic, params})
	s.mu.Unlock()
}

func (s *captureSink) snapshot() []capturedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]capturedEvent, len(s.events))
	copy(out, s.events)
	return out
}

func (s *captureSink) waitFor(t *testing.T, topic string, timeout time.Duration) capturedEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range s.snapshot() {
			if e.topic == topic {
				return e
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waitFor %q: timeout after %v (captured: %v)", topic, timeout, topicsOf(s.snapshot()))
	return capturedEvent{}
}

func topicsOf(es []capturedEvent) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.topic
	}
	return out
}

// startHub spins a Hub bound to a freshly-built teach-min machine
// (with the tiny program loaded into ROM) and returns it plus a
// teardown closure.
func startHub(t *testing.T) (*bridge.Hub, *captureSink, func()) {
	t.Helper()
	m := machine.TeachMin()
	// Drop the driver's default 10 Hz so the pump runs unpaced;
	// tests want determinism + speed, not wall-clock pacing.
	m.Inst.Driver().SetSpeedHz(0)
	// LDA #$2A ; STA $0010 ; BRK — 6 bytes at $E000.
	if err := m.Load([]byte{0xA9, 0x2A, 0x8D, 0x10, 0x00, 0x00}); err != nil {
		t.Fatalf("machine.Load: %v", err)
	}
	regions := []bridge.Region{
		{Name: "ram", Lo: 0x0000, Hi: 0x1FFF, ReadOnly: false},
		{Name: "rom", Lo: 0xE000, Hi: 0xFFFF, ReadOnly: true},
	}
	hub := bridge.NewHub(m.Inst, regions, "teach-min")
	sink := &captureSink{}

	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)

	cleanup := func() {
		cancel()
		select {
		case <-hub.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("hub.Run did not return after cancel")
		}
	}
	return hub, sink, cleanup
}

// TestHubRunHitsBreakpoint — the core integration: subscribe to
// clock.halt, set a bp at $E005 (the BRK), issue CmdRun, observe the
// halt event arrive with reason "breakpoint" + correct addr.
func TestHubRunHitsBreakpoint(t *testing.T) {
	hub, sink, cleanup := startHub(t)
	defer cleanup()

	hub.Subscribe([]string{"clock.halt"}, sink.send)
	hub.CmdBPSet(0xE005)
	hub.CmdRun(0, 0) // no budget, max speed

	ev := sink.waitFor(t, "clock.halt", 2*time.Second)
	r, ok := ev.params.(bridge.RunResult)
	if !ok {
		t.Fatalf("clock.halt params = %T, want bridge.RunResult", ev.params)
	}
	if r.Reason != "breakpoint" {
		t.Errorf("reason = %q, want breakpoint", r.Reason)
	}
	if r.Addr != 0xE005 {
		t.Errorf("addr = $%04X, want $E005", r.Addr)
	}
}

// TestHubRunUntilHalves — CmdRun with a budget. The CPU runs at
// most that many half-cycles and the Pump emits clock.halt with
// reason "limit".
func TestHubRunUntilHalves(t *testing.T) {
	hub, sink, cleanup := startHub(t)
	defer cleanup()

	hub.Subscribe([]string{"clock.halt"}, sink.send)
	hub.CmdRun(50, 0) // 50 half-cycle budget; will not hit BRK at $E005 (only ~12 halves to reach it)

	ev := sink.waitFor(t, "clock.halt", 2*time.Second)
	r := ev.params.(bridge.RunResult)
	// The tinyProg reaches BRK after ~12 halves, so we expect the
	// bp/vector halt — but with no bp set, RunUntil sees the BRK
	// as a normal instruction (BreakOnVector is off) and keeps
	// going. The 50-halfcycle budget then trips → "limit".
	if r.Reason != "limit" {
		t.Errorf("reason = %q, want limit (no bp set; only budget should fire)", r.Reason)
	}
}

// TestHubStopWhileRunning — CmdRun an infinite loop, CmdStop after a
// brief delay, expect clock.halt { reason: "client" }.
func TestHubStopWhileRunning(t *testing.T) {
	m := machine.TeachMin()
	m.Inst.Driver().SetSpeedHz(0) // unpaced — see startHub for rationale
	// JMP $E000 — infinite loop.
	if err := m.Load([]byte{0x4C, 0x00, 0xE0}); err != nil {
		t.Fatalf("machine.Load: %v", err)
	}
	hub := bridge.NewHub(m.Inst, nil, "teach-min")
	sink := &captureSink{}
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)
	defer func() {
		cancel()
		<-hub.Done()
	}()

	hub.Subscribe([]string{"clock.halt"}, sink.send)
	hub.CmdRun(0, 0)
	time.Sleep(20 * time.Millisecond)
	hub.CmdStop()

	ev := sink.waitFor(t, "clock.halt", 2*time.Second)
	r := ev.params.(bridge.RunResult)
	if r.Reason != "client" {
		t.Errorf("reason = %q, want client", r.Reason)
	}
}

// TestHubSubscribeAcceptsKnownTopics — only known topics are
// accepted; unknown ones silently dropped (the client diffs returned
// `accepted` against requested).
func TestHubSubscribeAcceptsKnownTopics(t *testing.T) {
	hub, _, cleanup := startHub(t)
	defer cleanup()

	_, accepted := hub.Subscribe([]string{"clock.halt", "no.such.topic"}, func(string, any) {})
	if len(accepted) != 1 || accepted[0] != "clock.halt" {
		t.Errorf("accepted = %v, want [clock.halt] only", accepted)
	}
}

// TestHubUnsubscribeRemovesSubscriber — after Unsubscribe, the sink
// stops receiving events.
func TestHubUnsubscribeRemovesSubscriber(t *testing.T) {
	hub, sink, cleanup := startHub(t)
	defer cleanup()

	id, _ := hub.Subscribe([]string{"clock.halt"}, sink.send)
	hub.CmdBPSet(0xE005)
	hub.CmdRun(0, 0)
	_ = sink.waitFor(t, "clock.halt", 2*time.Second)

	hub.Unsubscribe(id)
	before := len(sink.snapshot())

	// Reset by clearing bp + running again (CPU is now at $E005, BRK).
	// Easier: just verify no NEW events arrive when we re-fire a run.
	hub.CmdBPClearAll()
	hub.CmdBPSet(0xE002)
	hub.CmdRun(0, 0)
	// Wait for the pump to halt internally — we can't wait on events
	// since we unsubscribed. Just sleep a beat and check sink unchanged.
	time.Sleep(50 * time.Millisecond)
	after := len(sink.snapshot())
	if after != before {
		t.Errorf("events grew %d → %d after unsubscribe; want unchanged", before, after)
	}
}

// TestHubMultipleSubscribers — two subscribers, both receive the
// halt event. Proves the fan-out works.
func TestHubMultipleSubscribers(t *testing.T) {
	hub, _, cleanup := startHub(t)
	defer cleanup()

	a, b := &captureSink{}, &captureSink{}
	hub.Subscribe([]string{"clock.halt"}, a.send)
	hub.Subscribe([]string{"clock.halt"}, b.send)
	hub.CmdBPSet(0xE005)
	hub.CmdRun(0, 0)
	_ = a.waitFor(t, "clock.halt", 2*time.Second)
	_ = b.waitFor(t, "clock.halt", 2*time.Second)
}

// TestHubIRQVectorsThroughFFFE — CmdIRQ pulses the host-driven IRQ
// line. With I flag clear, the CPU should vector through $FFFE at
// the next instruction boundary, abandoning the tight loop it was
// running in. We can't easily plant the vector value (TeachMin's
// $FFFE-$FFFF is ROM and Poke is rejected), so the assertion is
// "PC has left the loop range" — that proves the IRQ was honoured
// regardless of where the vector pointed.
func TestHubIRQVectorsThroughFFFE(t *testing.T) {
	m := machine.TeachMin()
	m.Inst.Driver().SetSpeedHz(0) // unpaced

	// Program: CLI ; JMP $E001 (loop forever after enabling IRQs).
	if err := m.Load([]byte{
		0x58,             // $E000 CLI
		0x4C, 0x01, 0xE0, // $E001 JMP $E001  (tight self-loop)
	}); err != nil {
		t.Fatalf("machine.Load: %v", err)
	}

	hub := bridge.NewHub(m.Inst, nil, "teach-min")
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)
	defer func() { cancel(); <-hub.Done() }()

	// Let the CPU loop for a bit so we know it's in the JMP cycle
	// with I flag clear.
	hub.CmdRun(0, 0)
	time.Sleep(30 * time.Millisecond)

	// Sanity: PC should be inside the loop range.
	preIRQ := pcOf(t, hub)
	if preIRQ < 0xE001 || preIRQ > 0xE003 {
		t.Fatalf("pre-IRQ PC=$%04X; expected $E001-$E003 (JMP loop)", preIRQ)
	}

	// Pulse IRQ; give the Pump a couple slices to service.
	hub.CmdIRQ()
	time.Sleep(30 * time.Millisecond)
	hub.CmdStop()

	post := pcOf(t, hub)
	if post >= 0xE001 && post <= 0xE003 {
		t.Errorf("after IRQ, PC=$%04X still in JMP loop — IRQ did not fire", post)
	}
}

// TestHubNMIVectorsThroughFFFA — analogous to the IRQ test, but
// NMI ignores the I flag, so we don't need a CLI in the program.
// We can't plant a non-zero NMI vector ($FFFA-$FFFB is in ROM and
// Poke is rejected), so once PC lands at the default 0x0000 target
// the CPU enters a BRK loop and PC stays there. That's fine for the
// "did NMI fire?" assertion: PC left the JMP loop. The edge-trigger
// behaviour is covered by interp's direct unit tests.
func TestHubNMIVectorsThroughFFFA(t *testing.T) {
	m := machine.TeachMin()
	m.Inst.Driver().SetSpeedHz(0)

	// Tight self-loop at $E000. I=1 from reset; NMI should fire
	// regardless of I.
	if err := m.Load([]byte{
		0x4C, 0x00, 0xE0, // JMP $E000
	}); err != nil {
		t.Fatalf("machine.Load: %v", err)
	}

	hub := bridge.NewHub(m.Inst, nil, "teach-min")
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)
	defer func() { cancel(); <-hub.Done() }()

	hub.CmdRun(0, 0)
	time.Sleep(30 * time.Millisecond)

	preNMI := pcOf(t, hub)
	if preNMI != 0xE000 {
		t.Fatalf("pre-NMI PC=$%04X; expected $E000 (JMP loop)", preNMI)
	}

	hub.CmdNMI()
	time.Sleep(30 * time.Millisecond)
	hub.CmdStop()

	post := pcOf(t, hub)
	if post == 0xE000 {
		t.Errorf("after NMI, PC=$E000 still in JMP loop — NMI did not fire")
	}
}

func pcOf(t *testing.T, h *bridge.Hub) uint16 {
	t.Helper()
	h.QueryLock().Lock()
	defer h.QueryLock().Unlock()
	return h.Inst().Driver().Backend.Registers().PC
}

// TestHubStopAlignsToSync — after CmdStop, the CPU must be at an
// instruction boundary (SYNC=true), not in the middle of an
// in-flight instruction. Otherwise the user's first step post-stop
// only finishes the in-flight instruction's last half-cycles instead
// of executing a fresh one (the "I had to step twice" symptom).
func TestHubStopAlignsToSync(t *testing.T) {
	m := machine.TeachMin()
	m.Inst.Driver().SetSpeedHz(0) // unpaced
	// Infinite loop of multi-cycle instructions so we're very likely
	// to halt mid-instruction.
	if err := m.Load([]byte{
		0xA9, 0x2A, // LDA #$2A   (2 cycles)
		0x8D, 0x10, 0x00, // STA $0010  (4 cycles)
		0x4C, 0x00, 0xE0, // JMP $E000  (3 cycles)
	}); err != nil {
		t.Fatalf("machine.Load: %v", err)
	}
	hub := bridge.NewHub(m.Inst, nil, "teach-min")
	sink := &captureSink{}
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)
	defer func() { cancel(); <-hub.Done() }()

	hub.Subscribe([]string{"clock.halt"}, sink.send)
	hub.CmdRun(0, 0)
	time.Sleep(50 * time.Millisecond)
	hub.CmdStop()
	_ = sink.waitFor(t, "clock.halt", time.Second)

	// PC must be at a SYNC boundary. We can't observe SYNC directly
	// from outside the package; assert via the CPU backend's SYNC()
	// method through the instrument.
	if sync := m.Inst.Driver().Backend.SYNC(); !sync {
		t.Errorf("after CmdStop, SYNC=false — PC was not aligned to instruction boundary")
	}
}

// TestHubStateSnapshotOnRun — CmdRun edge-fires one state.snapshot
// immediately so subscribers see "running started" without waiting
// for the next periodic tick. Critical for the controller's CPU
// pane to flip to RUNNING the moment a remote or local R is hit.
func TestHubStateSnapshotOnRun(t *testing.T) {
	hub, sink, cleanup := startHub(t)
	defer cleanup()

	hub.Subscribe([]string{"state.snapshot"}, sink.send)
	hub.CmdRun(50, 0) // small budget so the run ends quickly

	ev := sink.waitFor(t, "state.snapshot", 2*time.Second)
	if _, ok := ev.params.(bridge.StateSnapshotPayload); !ok {
		t.Fatalf("state.snapshot params = %T, want StateSnapshotPayload", ev.params)
	}
}

// TestHubStateSnapshotPeriodicDuringRun — while running, the pump
// emits state.snapshot at ≈100 ms cadence (see stateSnapshotInterval
// in hub.go). 250 ms of run time should yield ≥ 2 snapshots; this is
// what makes the controller's CPU pane animate live.
func TestHubStateSnapshotPeriodicDuringRun(t *testing.T) {
	m := machine.TeachMin()
	m.Inst.Driver().SetSpeedHz(0) // unpaced — see startHub for rationale
	// Infinite loop at $E000: JMP $E000.
	if err := m.Load([]byte{0x4C, 0x00, 0xE0}); err != nil {
		t.Fatalf("machine.Load: %v", err)
	}
	hub := bridge.NewHub(m.Inst, nil, "teach-min")
	sink := &captureSink{}
	ctx, cancel := context.WithCancel(context.Background())
	go hub.Run(ctx)
	defer func() { cancel(); <-hub.Done() }()

	hub.Subscribe([]string{"state.snapshot"}, sink.send)
	hub.CmdRun(0, 0)
	time.Sleep(250 * time.Millisecond)
	hub.CmdStop()

	count := 0
	for _, e := range sink.snapshot() {
		if e.topic == "state.snapshot" {
			count++
		}
	}
	if count < 2 {
		t.Errorf("state.snapshot count = %d in 250ms; want ≥2 at 100ms cadence", count)
	}
}

// TestHubResetBroadcastsHalt — CmdReset emits clock.halt with reason
// "reset" so any subscriber (e.g., a remote controller) refreshes
// its mirrored state without polling. The TUI's Z key and the
// bridge's machine.reset both go through CmdReset.
func TestHubResetBroadcastsHalt(t *testing.T) {
	hub, sink, cleanup := startHub(t)
	defer cleanup()

	hub.Subscribe([]string{"clock.halt"}, sink.send)
	hub.CmdReset()

	ev := sink.waitFor(t, "clock.halt", time.Second)
	r, ok := ev.params.(bridge.RunResult)
	if !ok {
		t.Fatalf("clock.halt params = %T, want bridge.RunResult", ev.params)
	}
	if r.Reason != "reset" {
		t.Errorf("reason = %q, want reset", r.Reason)
	}
}

// TestHubBPHitFiresAlongsideClockHalt — a client subscribed only to
// bp.hit (NOT clock.halt) still receives a bp.hit event when a
// breakpoint causes the halt. Proves the two topics are emitted
// independently — same root event, two distinct fan-outs.
func TestHubBPHitFiresAlongsideClockHalt(t *testing.T) {
	hub, sink, cleanup := startHub(t)
	defer cleanup()

	hub.Subscribe([]string{"bp.hit"}, sink.send) // bp.hit only — NOT clock.halt
	hub.CmdBPSet(0xE005)
	hub.CmdRun(0, 0)

	ev := sink.waitFor(t, "bp.hit", 2*time.Second)
	hp, ok := ev.params.(bridge.BPHitPayload)
	if !ok {
		t.Fatalf("bp.hit params = %T, want bridge.BPHitPayload", ev.params)
	}
	if hp.Addr != 0xE005 {
		t.Errorf("addr = $%04X, want $E005", hp.Addr)
	}
}
