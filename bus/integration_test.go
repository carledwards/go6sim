package bus_test

import (
	"testing"

	"github.com/carledwards/go6sim/bus"
	"github.com/carledwards/go6sim/components/ram"
	"github.com/carledwards/go6sim/components/rom"
)

// Wires real RAM + ROM through the bus to make sure the Component
// contract holds end-to-end (and that ROM writes really are dropped
// at the bus level, not just at the component level).
func TestBusWithRealComponents(t *testing.T) {
	b := bus.New()

	mainRAM := ram.New("ram", 0x0000, 0x2000)
	mainROM := rom.New("rom", 0xE000, 0x2000)

	if err := mainROM.Load(0, []uint8{0xA9, 0x42, 0x8D, 0x00, 0x00}); err != nil {
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

	// RAM write/read round-trips through the bus.
	b.Write(0x0010, 0xAB)
	if got := b.Read(0x0010); got != 0xAB {
		t.Errorf("ram via bus: got %02X, want AB", got)
	}

	// ROM contents readable through the bus.
	if got := b.Read(0xE000); got != 0xA9 {
		t.Errorf("rom via bus: got %02X, want A9", got)
	}

	// Reset vector readable at $FFFC/$FFFD through the bus.
	lo := b.Read(0xFFFC)
	hi := b.Read(0xFFFD)
	if lo != 0x00 || hi != 0xE0 {
		t.Errorf("reset vector via bus: got $%02X%02X, want $E000", hi, lo)
	}

	// ROM writes dropped at the bus level.
	b.Write(0xE000, 0xFF)
	if got := b.Read(0xE000); got != 0xA9 {
		t.Errorf("rom write should not stick via bus: got %02X, want A9", got)
	}
}
