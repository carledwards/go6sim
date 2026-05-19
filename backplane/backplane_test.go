package backplane_test

import (
	"testing"
	"time"

	"github.com/carledwards/go6sim/backplane"
	"github.com/carledwards/go6sim/bus"
)

// resettableCard implements bus.Component AND bus.Resettable.
type resettableCard struct {
	base   uint16
	mem    [16]byte
	resets int
}

func (c *resettableCard) Name() string              { return "rw" }
func (c *resettableCard) Base() uint16              { return c.base }
func (c *resettableCard) Size() int                 { return len(c.mem) }
func (c *resettableCard) Read(off uint16) uint8     { return c.mem[off] }
func (c *resettableCard) Write(off uint16, v uint8) { c.mem[off] = v }
func (c *resettableCard) Reset()                    { c.resets++ }

// passiveCard implements only bus.Component (no Reset) — must be skipped
// by Backplane.Reset, never panic.
type passiveCard struct{ base uint16 }

func (c *passiveCard) Name() string              { return "passive" }
func (c *passiveCard) Base() uint16              { return c.base }
func (c *passiveCard) Size() int                 { return 16 }
func (c *passiveCard) Read(off uint16) uint8     { return 0 }
func (c *passiveCard) Write(off uint16, v uint8) {}

// irqCard implements bus.Component AND bus.IRQSource.
type irqCard struct {
	base     uint16
	asserted bool
}

func (c *irqCard) Name() string              { return "irq" }
func (c *irqCard) Base() uint16              { return c.base }
func (c *irqCard) Size() int                 { return 16 }
func (c *irqCard) Read(off uint16) uint8     { return 0 }
func (c *irqCard) Write(off uint16, v uint8) {}
func (c *irqCard) IRQAsserted() bool         { return c.asserted }

func TestBackplaneIRQWiredOr(t *testing.T) {
	bp := backplane.New()
	pv := &passiveCard{base: 0x0000}
	ic := &irqCard{base: 0x0100}
	if err := bp.Attach(pv); err != nil {
		t.Fatal(err)
	}
	if bp.IRQ() {
		t.Fatal("IRQ asserted with only a non-IRQSource card attached")
	}
	if err := bp.Attach(ic); err != nil {
		t.Fatal(err)
	}
	if bp.IRQ() {
		t.Fatal("IRQ asserted while the IRQ card is idle")
	}
	ic.asserted = true
	if !bp.IRQ() {
		t.Fatal("IRQ not asserted after a card pulled the line")
	}
}

func TestBackplaneIsBusAndComposes(t *testing.T) {
	bp := backplane.New()

	// It must be usable wherever a bus.Bus is expected (CPU backends).
	var _ bus.Bus = bp

	rw := &resettableCard{base: 0x0000}
	pv := &passiveCard{base: 0x0100}
	if err := bp.Attach(rw); err != nil {
		t.Fatalf("attach rw: %v", err)
	}
	if err := bp.Attach(pv); err != nil {
		t.Fatalf("attach passive: %v", err)
	}

	// Bus transact routes through the embedded trace bus unchanged.
	bp.Write(0x0004, 0xAB)
	if got := bp.Read(0x0004); got != 0xAB {
		t.Fatalf("read back = %#x, want 0xAB", got)
	}

	// Tick is inherited from the embedded TraceBus (gen advances; the
	// trace API stays available for the UI).
	bp.Tick(time.Millisecond)
	if !bp.RecentWrite(0x0004, 2) {
		t.Fatal("RecentWrite trace not preserved through embedding")
	}
	if bp.Trace() == nil {
		t.Fatal("Trace() escape hatch is nil")
	}

	// Reset fans out only to Resettable cards; passive is skipped.
	bp.Reset()
	if rw.resets != 1 {
		t.Fatalf("resettable card reset %d times, want 1", rw.resets)
	}
}
