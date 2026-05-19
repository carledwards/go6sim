package instrument_test

import (
	"bytes"
	"testing"
	"time"

	go6 "github.com/carledwards/go6asm/asm"

	"github.com/carledwards/go6sim/backplane"
	"github.com/carledwards/go6sim/clock"
	"github.com/carledwards/go6sim/components/ram"
	"github.com/carledwards/go6sim/components/rom"
	"github.com/carledwards/go6sim/cpu/interp"
	"github.com/carledwards/go6sim/instrument"
)

// Same deterministic program as the brick-2 golden: writes 0..7 to
// $10-$17 with a counted loop, then spins. Driving it entirely through
// the Instrument surface is the brick-6 litmus — a debugger's
// step/dump/peek/poke/go must all be expressible here.
const program = `
  ldx #$00
loop:
  txa
  sta $10,x
  inx
  cpx #$08
  bne loop
done:
  jmp done
`

func newMachine(t *testing.T) *instrument.Instrument {
	t.Helper()
	res := go6.Assemble(go6.Input{
		Entry:  "t",
		Files:  []go6.SourceFile{{Name: "t", Content: []byte(program)}},
		Layer0: true,
		Target: "sim-tui",
	})
	if !res.Ok() {
		t.Fatalf("assemble: %v", res.Errors)
	}
	bp := backplane.New()
	if err := bp.Attach(ram.New("ram", 0x0000, 0x2000)); err != nil {
		t.Fatal(err)
	}
	r := rom.New("rom", 0xE000, 0x2000)
	if err := r.Load(0, res.Image); err != nil {
		t.Fatal(err)
	}
	if err := bp.Attach(r); err != nil {
		t.Fatal(err)
	}
	drv := clock.NewDriver(interp.New(bp))
	return instrument.New(bp, drv)
}

func TestInstrumentDrivesAProgram(t *testing.T) {
	in := newMachine(t)

	in.Reset()
	if pc := in.State().PC; pc != 0xE000 {
		t.Fatalf("reset PC = $%04X, want $E000", pc)
	}

	// Single-step instructions to completion (ample budget; it then
	// spins at `done`).
	for k := 0; k < 80; k++ {
		in.Step(1)
	}

	st := in.State()
	if st.A != 0x07 || st.X != 0x08 || st.PC != 0xE00A {
		t.Fatalf("after run: A=%02X X=%02X PC=%04X, want 07/08/E00A", st.A, st.X, st.PC)
	}
	if got := in.Mem(0x10, 0x17); !bytes.Equal(got, []byte{0, 1, 2, 3, 4, 5, 6, 7}) {
		t.Fatalf("zp $10-$17 = % X, want 00..07", got)
	}

	// peek / poke roundtrip on RAM.
	in.Poke(0x0050, 0xAB)
	if v := in.Peek(0x0050); v != 0xAB {
		t.Fatalf("peek $0050 = %02X, want AB", v)
	}

	// go: free-run via the budget-tick Advance driver, deterministically.
	if in.Running() {
		t.Fatal("should not be running before SetRunning")
	}
	in.SetRunning(true)
	in.Driver().MaxBatch = 10
	in.Driver().SetSpeedHz(0) // Max mode → n = cap·(dt/referenceTick)
	before := in.State().HalfCycles
	n := in.Advance(50 * time.Millisecond)
	if n != 10 || in.State().HalfCycles != before+10 {
		t.Fatalf("Advance ran %d (halfcycles %d→%d), want 10 / +10",
			n, before, in.State().HalfCycles)
	}
	if !in.Running() {
		t.Fatal("Running() should be true after SetRunning(true)")
	}
}

// compile-time: the surface a debugger CLI needs is all present.
var _ = []any{
	(*instrument.Instrument)(nil),
	clock.NewDriver, backplane.New,
}
