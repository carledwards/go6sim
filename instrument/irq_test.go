package instrument_test

import (
	"testing"
	"time"

	"github.com/carledwards/go6sim/backplane"
	"github.com/carledwards/go6sim/clock"
	"github.com/carledwards/go6sim/components/ram"
	"github.com/carledwards/go6sim/components/rom"
	"github.com/carledwards/go6sim/components/via"
	"github.com/carledwards/go6sim/cpu/interp"
	"github.com/carledwards/go6sim/cpu/netsim"
	"github.com/carledwards/go6sim/instrument"
)

// buildIRQROM hand-assembles an 8 KB ROM image (no go6asm — this tests
// go6sim's IRQ *delivery*, not go6asm vector authoring). Layout is
// constant across variants (disabled blocks are NOP-filled) so the
// loop/ISR addresses never move:
//
//	$E000 LDA #$00 / STA $00            ; counter := 0
//	      LDA #$40 / STA $B00B          ; ACR: T1 free-run
//	[IER] LDA #$C0 / STA $B00E          ; enable T1 interrupt   (else 5×NOP)
//	      LDA #$20 / STA $B004          ; T1C-L (latch lo = $20)
//	      LDA #$00 / STA $B005          ; T1C-H -> arm/start T1
//	[CLI] CLI                           ; allow IRQ             (else NOP)
//	$E019 loop: JMP $E019               ; spin; IRQ preempts
//	$E01C isr:  INC $00                 ; bump the counter
//	            LDA $B004               ; read T1C-L -> clears IFR_T1
//	            RTI
//	$FFFC RESET=$E000  $FFFE IRQ=$E01C
func buildIRQROM(enableIER, doCLI bool) []byte {
	code := []byte{
		0xA9, 0x00, // LDA #$00
		0x85, 0x00, // STA $00
		0xA9, 0x40, // LDA #$40
		0x8D, 0x0B, 0xB0, // STA $B00B  (ACR free-run)
		0, 0, 0, 0, 0, // [9:14] IER block
		0xA9, 0x20, // LDA #$20
		0x8D, 0x04, 0xB0, // STA $B004  (T1C-L = latch lo $20)
		0xA9, 0x00, // LDA #$00
		0x8D, 0x05, 0xB0, // STA $B005  (T1C-H -> arm/start)
		0,                // [24] CLI/NOP
		0x4C, 0x19, 0xE0, // JMP $E019  (loop @ offset 25)
		0xE6, 0x00, // INC $00     (ISR @ offset 28 = $E01C)
		0xAD, 0x04, 0xB0, // LDA $B004 (clears IFR_T1)
		0x40, // RTI
	}
	if enableIER {
		copy(code[9:14], []byte{0xA9, 0xC0, 0x8D, 0x0E, 0xB0}) // LDA #$C0 / STA $B00E
	} else {
		copy(code[9:14], []byte{0xEA, 0xEA, 0xEA, 0xEA, 0xEA})
	}
	if doCLI {
		code[24] = 0x58 // CLI
	} else {
		code[24] = 0xEA // NOP
	}

	img := make([]byte, 0x2000) // $E000-$FFFF
	copy(img, code)
	img[0x1FFA], img[0x1FFB] = 0x00, 0xE0 // NMI   -> $E000
	img[0x1FFC], img[0x1FFD] = 0x00, 0xE0 // RESET -> $E000
	img[0x1FFE], img[0x1FFF] = 0x1C, 0xE0 // IRQ   -> $E01C
	return img
}

func runIRQ(t *testing.T, enableIER, doCLI bool) *instrument.Instrument {
	t.Helper()
	bp := backplane.New()
	if err := bp.Attach(ram.New("ram", 0x0000, 0x2000)); err != nil {
		t.Fatal(err)
	}
	if err := bp.Attach(via.New("via1", 0xB000, 1_000_000)); err != nil {
		t.Fatal(err)
	}
	r := rom.New("rom", 0xE000, 0x2000)
	if err := r.Load(0, buildIRQROM(enableIER, doCLI)); err != nil {
		t.Fatal(err)
	}
	if err := bp.Attach(r); err != nil {
		t.Fatal(err)
	}
	in := instrument.New(bp, clock.NewDriver(interp.New(bp)))
	in.Reset()

	in.SetRunning(true)
	in.Driver().SetSpeedHz(0) // Max
	in.Driver().MaxBatch = 2000
	for k := 0; k < 20; k++ { // deterministic virtual budget
		in.Advance(50 * time.Millisecond)
	}
	return in
}

// End-to-end: VIA T1 underflow -> backplane wired-OR -> interp services
// the IRQ -> ISR runs. Three gated variants prove the delivery is real
// and gated on BOTH the VIA asserting (IER) and the CPU's I-flag.
func TestHardwareIRQDelivery(t *testing.T) {
	// A: IER enabled + CLI -> IRQ delivered, ISR bumps the counter.
	a := runIRQ(t, true, true)
	if c := a.Peek(0x0000); c == 0 {
		t.Fatal("IER+CLI: counter is 0 — hardware IRQ was not delivered")
	}
	if a.Taps()["cpu.brk"] != 0 {
		t.Fatal("cpu.brk advanced — this must be a hardware IRQ, not BRK")
	}

	// B: IER never written -> VIA never asserts -> no IRQ.
	if c := runIRQ(t, false, true).Peek(0x0000); c != 0 {
		t.Fatalf("no-IER: counter=%d, want 0 (VIA must not assert)", c)
	}

	// C: IER enabled but I-flag never cleared -> IRQ gated off.
	if c := runIRQ(t, true, false).Peek(0x0000); c != 0 {
		t.Fatalf("no-CLI: counter=%d, want 0 (I-flag must gate the IRQ)", c)
	}
}

// netsim parity (brick 9b upstream SetIRQ): the same VIA-T1 IRQ must be
// delivered on the transistor-level core too. Smaller budget — netsim
// is ~30× interp — and we only need to prove delivery (counter > 0).
func TestHardwareIRQDeliveryNetsim(t *testing.T) {
	bp := backplane.New()
	if err := bp.Attach(ram.New("ram", 0x0000, 0x2000)); err != nil {
		t.Fatal(err)
	}
	if err := bp.Attach(via.New("via1", 0xB000, 1_000_000)); err != nil {
		t.Fatal(err)
	}
	r := rom.New("rom", 0xE000, 0x2000)
	if err := r.Load(0, buildIRQROM(true, true)); err != nil {
		t.Fatal(err)
	}
	if err := bp.Attach(r); err != nil {
		t.Fatal(err)
	}
	be, err := netsim.New(bp)
	if err != nil {
		t.Fatal(err)
	}
	in := instrument.New(bp, clock.NewDriver(be))
	in.Reset()

	in.SetRunning(true)
	in.Driver().SetSpeedHz(0)
	in.Driver().MaxBatch = 1500
	for k := 0; k < 4; k++ {
		in.Advance(50 * time.Millisecond)
	}
	if c := in.Peek(0x0000); c == 0 {
		t.Fatal("netsim: counter is 0 — IRQ not delivered to the transistor core")
	}
}
