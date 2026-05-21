package interp_test

import (
	"testing"

	"github.com/carledwards/go6sim/bus"
	"github.com/carledwards/go6sim/components/ram"
	"github.com/carledwards/go6sim/components/rom"
	"github.com/carledwards/go6sim/cpu/interp"
)

// Same end-to-end test as the netsim adapter — load a deterministic
// program, run a fixed number of cycles, assert architectural state
// lines up. Confirms the interpretive core executes the official
// opcodes correctly.
func TestRunsDemoProgram(t *testing.T) {
	b := bus.New()
	mainRAM := ram.New("ram", 0x0000, 0x2000)
	mainROM := rom.New("rom", 0xE000, 0x2000)

	prog := []uint8{
		0xA9, 0x42, // LDA #$42
		0xA2, 0x11, // LDX #$11
		0xA0, 0x22, // LDY #$22
		0x8D, 0x00, 0x00, // STA $0000
		0x8E, 0x01, 0x00, // STX $0001
		0x8C, 0x02, 0x00, // STY $0002
		0x4C, 0x0C, 0xE0, // JMP $E00C
	}
	if err := mainROM.Load(0, prog); err != nil {
		t.Fatalf("rom load: %v", err)
	}
	if err := mainROM.SetResetVector(0xE000); err != nil {
		t.Fatalf("reset vector: %v", err)
	}
	if err := b.Register(mainRAM); err != nil {
		t.Fatal(err)
	}
	if err := b.Register(mainROM); err != nil {
		t.Fatal(err)
	}

	be := interp.New(b)
	be.Reset()

	for i := 0; i < 200; i++ {
		be.HalfStep()
	}

	r := be.Registers()
	if r.A != 0x42 {
		t.Errorf("A: got $%02X, want $42", r.A)
	}
	if r.X != 0x11 {
		t.Errorf("X: got $%02X, want $11", r.X)
	}
	if r.Y != 0x22 {
		t.Errorf("Y: got $%02X, want $22", r.Y)
	}
	if got := b.Read(0x0000); got != 0x42 {
		t.Errorf("ram[0]: got $%02X, want $42", got)
	}
	if got := b.Read(0x0001); got != 0x11 {
		t.Errorf("ram[1]: got $%02X, want $11", got)
	}
	if got := b.Read(0x0002); got != 0x22 {
		t.Errorf("ram[2]: got $%02X, want $22", got)
	}
	if r.PC < 0xE00C || r.PC > 0xE00F {
		t.Errorf("PC: got $%04X, want in $E00C..$E00F", r.PC)
	}
}

// Adder loop: counts ADC carry chain over 10 iterations.
func TestAdcCarryLoop(t *testing.T) {
	b := bus.New()
	mainRAM := ram.New("ram", 0x0000, 0x100)
	mainROM := rom.New("rom", 0xE000, 0x2000)

	// LDA #$80 ; CLC ; ADC #$80 ; STA $00 ; spin
	prog := []uint8{
		0xA9, 0x80,
		0x18,
		0x69, 0x80,
		0x85, 0x00,
		0x4C, 0x07, 0xE0,
	}
	mainROM.Load(0, prog)
	mainROM.SetResetVector(0xE000)
	b.Register(mainRAM)
	b.Register(mainROM)

	be := interp.New(b)
	be.Reset()
	for i := 0; i < 100; i++ {
		be.HalfStep()
	}

	if got := b.Read(0x00); got != 0x00 {
		t.Errorf("ram[0]: got $%02X, want $00 (0x80+0x80 wraps)", got)
	}
	r := be.Registers()
	if r.P&0x01 == 0 {
		t.Errorf("carry flag: should be set after 0x80+0x80, got P=$%02X", r.P)
	}
}

// nmiBus wraps a bus.Bus and exposes a controllable NMI line so we
// can test interp's NMI edge-detection directly without going
// through the backplane / Hub / pump stack.
type nmiBus struct {
	bus.Bus
	nmi bool
}

func (n *nmiBus) NMI() bool { return n.nmi }

// TestNMIEdgeFiresOnRisingEdge — interp should service NMI exactly
// once per rising edge of the line. Holding NMI asserted across many
// instruction boundaries must NOT re-trigger; the source has to drop
// the line and reassert it for a second service.
func TestNMIEdgeFiresOnRisingEdge(t *testing.T) {
	inner := bus.New()
	mainRAM := ram.New("ram", 0x0000, 0x200)
	mainROM := rom.New("rom", 0xE000, 0x2000)

	// $E000  JMP $E000    (tight loop)
	// $E100  STA $0050    ; "NMI handler" — writes a sentinel
	// $E103  RTI          ; return so the next pulse can fire again
	if err := mainROM.Load(0, []uint8{0x4C, 0x00, 0xE0}); err != nil {
		t.Fatalf("rom load: %v", err)
	}
	if err := mainROM.Load(0x0100, []uint8{0x8D, 0x50, 0x00, 0x40}); err != nil {
		t.Fatalf("handler load: %v", err)
	}
	if err := mainROM.SetResetVector(0xE000); err != nil {
		t.Fatalf("reset vector: %v", err)
	}
	// NMI vector at $FFFA → $E100. $FFFA offset within $E000-base ROM = 0x1FFA.
	if err := mainROM.Load(0x1FFA, []uint8{0x00, 0xE1}); err != nil {
		t.Fatalf("nmi vector: %v", err)
	}
	if err := inner.Register(mainRAM); err != nil {
		t.Fatal(err)
	}
	if err := inner.Register(mainROM); err != nil {
		t.Fatal(err)
	}

	nb := &nmiBus{Bus: inner}
	be := interp.New(nb)
	be.Reset()

	// Count handler entries by sampling PC after each HalfStep —
	// transitions FROM outside the handler range TO inside it count
	// as one entry. Crude but doesn't require instrumenting interp.
	insideHandler := func() bool {
		pc := be.Registers().PC
		return pc >= 0xE100 && pc < 0xE104
	}
	entries := 0
	wasInside := false
	step := func(n int) {
		for i := 0; i < n; i++ {
			be.HalfStep()
			inside := insideHandler()
			if inside && !wasInside {
				entries++
			}
			wasInside = inside
		}
	}

	// Pulse #1: assert NMI, give the CPU plenty of boundaries to
	// observe + service it.
	nb.nmi = true
	step(60)
	if entries != 1 {
		t.Fatalf("after first pulse, entries=%d, want 1", entries)
	}
	nb.nmi = false

	// Hold low: handler must NOT re-fire.
	step(60)
	if entries != 1 {
		t.Errorf("NMI deasserted but handler re-fired (entries=%d, want 1)", entries)
	}

	// Pulse #2: rising edge — handler fires a second time.
	nb.nmi = true
	step(60)
	if entries != 2 {
		t.Errorf("after second pulse, entries=%d, want 2 (edge-trigger reset broken)", entries)
	}

	// Sustained high: holding past the service must NOT re-trigger.
	step(400)
	if entries != 2 {
		t.Errorf("NMI held high re-triggered handler (entries=%d, want 2)", entries)
	}
}

// JSR/RTS round-trip.
func TestJsrRts(t *testing.T) {
	b := bus.New()
	mainRAM := ram.New("ram", 0x0000, 0x200)
	mainROM := rom.New("rom", 0xE000, 0x2000)

	// $E000  LDA #$AA
	// $E002  JSR $E010
	// $E005  STA $0100   ; should run after RTS with A=$BB
	// $E008  JMP $E008   ; spin
	// $E010  LDA #$BB    ; subroutine
	// $E012  RTS
	prog := []uint8{
		0xA9, 0xAA,
		0x20, 0x10, 0xE0,
		0x8D, 0x00, 0x01,
		0x4C, 0x08, 0xE0,
	}
	mainROM.Load(0, prog)
	mainROM.Load(0x0010, []uint8{0xA9, 0xBB, 0x60})
	mainROM.SetResetVector(0xE000)
	b.Register(mainRAM)
	b.Register(mainROM)

	be := interp.New(b)
	be.Reset()
	for i := 0; i < 200; i++ {
		be.HalfStep()
	}

	if got := b.Read(0x0100); got != 0xBB {
		t.Errorf("ram[0x100]: got $%02X, want $BB (subroutine ran, RTS returned, STA fired)", got)
	}
}
