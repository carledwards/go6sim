package netsim_test

import (
	"testing"

	"github.com/carledwards/go6sim/bus"
	"github.com/carledwards/go6sim/components/ram"
	"github.com/carledwards/go6sim/components/rom"
	"github.com/carledwards/go6sim/cpu/netsim"
)

// End-to-end: build a real bus with RAM + ROM, load a small program
// into ROM, run via the netsim adapter, and verify the architectural
// state matches what the program should produce.
func TestNetsimAdapterRunsProgram(t *testing.T) {
	b := bus.New()

	mainRAM := ram.New("ram", 0x0000, 0x2000)
	mainROM := rom.New("rom", 0xE000, 0x2000)

	// $E000: LDA #$42       ; A = $42
	// $E002: LDX #$11       ; X = $11
	// $E004: LDY #$22       ; Y = $22
	// $E006: STA $00        ; ram[0] = $42
	// $E009: STX $01        ; ram[1] = $11
	// $E00C: STY $02        ; ram[2] = $22
	// $E00F: JMP $E00C      ; spin
	prog := []uint8{
		0xA9, 0x42,
		0xA2, 0x11,
		0xA0, 0x22,
		0x8D, 0x00, 0x00,
		0x8E, 0x01, 0x00,
		0x8C, 0x02, 0x00,
		0x4C, 0x0C, 0xE0,
	}
	if err := mainROM.Load(0, prog); err != nil {
		t.Fatalf("rom load: %v", err)
	}
	if err := mainROM.SetResetVector(0xE000); err != nil {
		t.Fatalf("reset vector: %v", err)
	}

	if err := b.Register(mainRAM); err != nil {
		t.Fatalf("register ram: %v", err)
	}
	if err := b.Register(mainROM); err != nil {
		t.Fatalf("register rom: %v", err)
	}

	be, err := netsim.New(b)
	if err != nil {
		t.Fatalf("netsim.New: %v", err)
	}
	be.Reset()

	for i := 0; i < 200; i++ {
		be.HalfStep()
	}

	regs := be.Registers()
	if regs.A != 0x42 {
		t.Errorf("A: got $%02X, want $42", regs.A)
	}
	if regs.X != 0x11 {
		t.Errorf("X: got $%02X, want $11", regs.X)
	}
	if regs.Y != 0x22 {
		t.Errorf("Y: got $%02X, want $22", regs.Y)
	}
	if got := b.Read(0x0000); got != 0x42 {
		t.Errorf("ram[0] via bus: got $%02X, want $42", got)
	}
	if got := b.Read(0x0001); got != 0x11 {
		t.Errorf("ram[1] via bus: got $%02X, want $11", got)
	}
	if got := b.Read(0x0002); got != 0x22 {
		t.Errorf("ram[2] via bus: got $%02X, want $22", got)
	}
	if regs.PC < 0xE00C || regs.PC > 0xE011 {
		t.Errorf("PC: got $%04X, want in spin loop $E00C..$E011", regs.PC)
	}
	if be.HalfCycles() != 200 {
		t.Errorf("HalfCycles: got %d, want 200", be.HalfCycles())
	}
}
