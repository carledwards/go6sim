package instrument_test

import (
	"testing"

	go6 "github.com/carledwards/go6asm/asm"

	"github.com/carledwards/go6sim/backplane"
	"github.com/carledwards/go6sim/clock"
	"github.com/carledwards/go6sim/components/ram"
	"github.com/carledwards/go6sim/components/rom"
	"github.com/carledwards/go6sim/components/via"
	"github.com/carledwards/go6sim/cpu/interp"
	"github.com/carledwards/go6sim/instrument"
)

// Layout (Layer-0, origin $E000):
//
//	E000 nop
//	E001 nop
//	E002 loop: nop
//	E003 brk
//	E004 jmp loop
//
// Reaching E002 at a SYNC boundary fires an address breakpoint; the BRK
// at E003 vectors and bumps the cpu.brk tap.
const brkProg = `
  nop
  nop
loop:
  nop
  brk
  jmp loop
`

func newVIAMachine(t *testing.T) *instrument.Instrument {
	t.Helper()
	res := go6.Assemble(go6.Input{
		Entry:  "b",
		Files:  []go6.SourceFile{{Name: "b", Content: []byte(brkProg)}},
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
	if err := bp.Attach(via.New("via1", 0xB000, 1_000_000)); err != nil {
		t.Fatal(err)
	}
	r := rom.New("rom", 0xE000, 0x2000)
	if err := r.Load(0, res.Image); err != nil {
		t.Fatal(err)
	}
	if err := bp.Attach(r); err != nil {
		t.Fatal(err)
	}
	in := instrument.New(bp, clock.NewDriver(interp.New(bp)))
	in.Reset()
	return in
}

func TestTapsAggregation(t *testing.T) {
	in := newVIAMachine(t)
	tp := in.Taps()
	for _, k := range []string{"via1.irq", "via1.t1", "cpu.brk"} {
		if _, ok := tp[k]; !ok {
			t.Fatalf("Taps() missing %q; got keys %v", k, keys(tp))
		}
	}
	if tp["via1.irq"] != 0 || tp["cpu.brk"] != 0 {
		t.Fatalf("initial taps not zero: via1.irq=%d cpu.brk=%d", tp["via1.irq"], tp["cpu.brk"])
	}
}

func TestBreakOnVector(t *testing.T) {
	in := newVIAMachine(t)
	in.BreakOnVector(true)
	r := in.RunUntil(2000)
	if r.Reason != "vector" {
		t.Fatalf("RunUntil reason = %q, want vector (%+v)", r.Reason, r)
	}
	if got := in.Taps()["cpu.brk"]; got != 1 {
		t.Fatalf("cpu.brk tap = %d after a BRK, want 1", got)
	}
}

func TestAddressBreakpoint(t *testing.T) {
	in := newVIAMachine(t)
	in.SetBreakpoint(0xE002) // the `loop:` nop
	r := in.RunUntil(2000)
	if r.Reason != "breakpoint" || r.Addr != 0xE002 {
		t.Fatalf("RunUntil = %+v, want breakpoint @E002", r)
	}
}

func TestBudgetExhaustion(t *testing.T) {
	in := newVIAMachine(t)
	// 3 half-cycles is still inside the 7-cycle reset burn — no SYNC,
	// no vector — so the run exhausts its budget.
	r := in.RunUntil(3)
	if r.Reason != "budget" || r.HalfCycles != 3 {
		t.Fatalf("RunUntil = %+v, want budget after 3 half-cycles", r)
	}
}

func keys(m map[string]uint64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
