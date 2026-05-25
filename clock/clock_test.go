package clock

import (
	"testing"
	"time"

	"github.com/carledwards/go6sim/cpu"
)

// fakeCPU is a minimal cpu.Backend: counts HalfSteps and bumps PC every
// pcEvery half-steps (0 = never), so StepInstruction has a boundary.
type fakeCPU struct {
	half    uint64
	pc      uint16
	resets  int
	pcEvery uint64
}

func (f *fakeCPU) Reset() { f.resets++; f.half = 0 }
func (f *fakeCPU) HalfStep() {
	f.half++
	if f.pcEvery > 0 && f.half%f.pcEvery == 0 {
		f.pc++
	}
}
func (f *fakeCPU) Registers() cpu.Registers { return cpu.Registers{PC: f.pc} }
func (f *fakeCPU) HalfCycles() uint64       { return f.half }
func (f *fakeCPU) AddressBus() uint16       { return 0 }
func (f *fakeCPU) DataBus() uint8           { return 0 }
func (f *fakeCPU) ReadCycle() bool          { return true }
func (f *fakeCPU) IRQ() bool                { return true }
func (f *fakeCPU) NMI() bool                { return true }
func (f *fakeCPU) SYNC() bool               { return false }
func (f *fakeCPU) MaxHz() int                { return 0 }

// fakeSyncCPU pulses SYNC=true every `cycleHalves` halfSteps and
// keeps PC pinned at 0 — the test fixture for the JMP-self regression.
// SYNC mimics the 6502 SYNC pin going high on the opcode-fetch
// (boundary) cycle and low during burn cycles.
type fakeSyncCPU struct {
	pc          uint16
	half        uint64
	cycleHalves uint64 // SYNC rises at half % cycleHalves == 0
}

func (f *fakeSyncCPU) Reset()                       { f.half = 0 }
func (f *fakeSyncCPU) HalfStep()                    { f.half++ }
func (f *fakeSyncCPU) Registers() cpu.Registers     { return cpu.Registers{PC: f.pc} }
func (f *fakeSyncCPU) HalfCycles() uint64           { return f.half }
func (f *fakeSyncCPU) AddressBus() uint16           { return 0 }
func (f *fakeSyncCPU) DataBus() uint8               { return 0 }
func (f *fakeSyncCPU) ReadCycle() bool              { return true }
func (f *fakeSyncCPU) IRQ() bool                    { return true }
func (f *fakeSyncCPU) NMI() bool                    { return true }
func (f *fakeSyncCPU) SYNC() bool {
	if f.cycleHalves == 0 {
		return false
	}
	return f.half%f.cycleHalves == 0 && f.half > 0
}
func (f *fakeSyncCPU) MaxHz() int { return 0 }

func TestStepOneAndObserverGate(t *testing.T) {
	f := &fakeCPU{}
	d := NewDriver(f)
	var hooks int
	d.OnHalfStep = func() { hooks++ }

	d.StepOne()
	if f.half != 1 || hooks != 1 || d.StepsDone() != 1 {
		t.Fatalf("StepOne: half=%d hooks=%d steps=%d, want 1/1/1", f.half, hooks, d.StepsDone())
	}
	// While running, manual StepOne is a no-op (the run loop owns the clock).
	d.SetRunning(true)
	d.StepOne()
	if f.half != 1 {
		t.Fatalf("StepOne while running advanced the CPU (half=%d)", f.half)
	}
}

func TestStepInstructionStopsOnPCChange(t *testing.T) {
	f := &fakeCPU{pcEvery: 4}
	d := NewDriver(f)
	d.StepInstruction()
	if f.half != 4 || d.StepsDone() != 4 {
		t.Fatalf("StepInstruction: half=%d steps=%d, want 4/4 (PC changes at 4)", f.half, d.StepsDone())
	}
}

// TestStepInstructionStopsOnSyncRise — regression for the JMP-self
// pattern. A CPU where PC NEVER changes (e.g., `JMP <self>`) used to
// exhaust StepInstruction's 32-half budget every call because the
// algorithm only watched PC. Now we also watch SYNC: each rising
// edge means a boundary halfStep just executed an instruction, so
// the call returns even though PC didn't move.
//
// fakeSyncCPU pulses SYNC=true every 3rd halfStep (mimicking a
// 3-cycle JMP instruction); PC stays at 0.
func TestStepInstructionStopsOnSyncRise(t *testing.T) {
	f := &fakeSyncCPU{cycleHalves: 3}
	d := NewDriver(f)
	d.StepInstruction()
	// Should return at the first SYNC rising edge — halfStep 3.
	if f.half != 3 || d.StepsDone() != 3 {
		t.Fatalf("StepInstruction (JMP-self): half=%d steps=%d, want 3/3 (SYNC rises at 3)",
			f.half, d.StepsDone())
	}
	if f.pc != 0 {
		t.Errorf("fakeSyncCPU.pc moved (=$%04X); test fixture should hold PC at 0", f.pc)
	}
}

func TestSpeedSelection(t *testing.T) {
	d := NewDriver(&fakeCPU{})
	if d.Speed().Hz != 10 {
		t.Fatalf("default speed = %dHz, want 10Hz", d.Speed().Hz)
	}
	if !d.SetSpeedHz(1000) || d.Speed().Hz != 1000 {
		t.Fatalf("SetSpeedHz(1000) failed: now %dHz", d.Speed().Hz)
	}
	if d.SetSpeedHz(999) {
		t.Fatal("SetSpeedHz(999) should report no match")
	}
	idx := d.SpeedIndex()
	d.CycleSpeed(1)
	if d.SpeedIndex() != (idx+1)%len(Speeds) {
		t.Fatalf("CycleSpeed(1): idx %d → %d", idx, d.SpeedIndex())
	}
}

func TestAdvanceGateAndCap(t *testing.T) {
	f := &fakeCPU{}
	d := NewDriver(f)

	if n := d.Advance(50 * time.Millisecond); n != 0 || f.half != 0 {
		t.Fatalf("Advance while paused ran %d steps (half=%d), want 0", n, f.half)
	}
	d.SetRunning(true)
	d.MaxBatch = 5
	d.SetSpeedHz(0) // Max mode → cap·(elapsed/referenceTick) = 5·1
	if n := d.Advance(50 * time.Millisecond); n != 5 || d.StepsDone() != 5 {
		t.Fatalf("Advance Max-mode ran %d (steps=%d), want 5/5", n, d.StepsDone())
	}
}

func TestResetKeepsRunningFlag(t *testing.T) {
	f := &fakeCPU{}
	d := NewDriver(f)
	d.SetRunning(true)
	d.MaxBatch = 3
	d.SetSpeedHz(0)
	d.Advance(50 * time.Millisecond)

	d.Reset()
	if d.StepsDone() != 0 || f.resets != 1 {
		t.Fatalf("Reset: steps=%d backendResets=%d, want 0/1", d.StepsDone(), f.resets)
	}
	if !d.Running() {
		t.Fatal("Reset must NOT clear the running flag (hardware reset button semantics)")
	}
}
