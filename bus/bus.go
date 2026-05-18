// Package bus is the memory-mapped IO substrate the simulator runs on.
//
// Components (RAM, ROM, peripherals) register themselves at a base
// address. The bus routes Read/Write calls from the CPU backend to
// whichever component owns that address. Unmapped reads return 0x00;
// unmapped writes are silently dropped.
package bus

import (
	"fmt"
	"sort"
	"time"

	"github.com/carledwards/go6sim/asm"
)

// Bus is a 16-bit-address memory map.
type Bus interface {
	Read(addr uint16) uint8
	Write(addr uint16, val uint8)

	// Register adds a component. Returns an error if its range
	// overlaps an already-registered component or extends past the
	// end of the 16-bit address space.
	Register(c Component) error

	// Components returns a snapshot of registered components, sorted
	// by base address.
	Components() []Component
}

// Component is a region-owner on the bus. Offsets passed to Read/Write
// are relative to the component's Base.
type Component interface {
	Name() string
	Base() uint16
	Size() int
	Read(offset uint16) uint8
	Write(offset uint16, val uint8)
}

// Ticker is an optional interface a component can implement to take
// part in wall-clock advancement. The bus calls Tick on every
// registered Ticker once per host frame, passing the elapsed duration
// since the previous Tick. Components use this to model peripherals
// driven by their own oscillator (e.g. a 6522 VIA timer) — they keep
// running while the CPU is halted, single-stepping, or breakpointed,
// because their crystal is not the CPU clock.
//
// dt is wall-clock time; the component is responsible for converting
// it to its own internal counter rate.
type Ticker interface {
	Tick(dt time.Duration)
}

// Labeller is an optional interface a component can implement to
// expose its register layout as named symbols. The memory window's
// Labels view merges component-provided symbols with program-provided
// ones, so a memory dump of the VIA's region shows readable names
// (T1C-L, IFR, ACR, …) instead of raw offsets.
//
// Components return absolute-address symbols (their own Base + offset),
// so callers can treat all symbols uniformly without knowing where
// the component lives on the bus.
type Labeller interface {
	Symbols() []asm.Symbol
}

// New returns an empty bus.
func New() Bus {
	return &mapBus{}
}

type mapBus struct {
	comps []Component
}

func (b *mapBus) Register(c Component) error {
	if c.Size() <= 0 {
		return fmt.Errorf("bus: %q has non-positive size %d", c.Name(), c.Size())
	}
	end := int(c.Base()) + c.Size()
	if end > 0x10000 {
		return fmt.Errorf("bus: %q range $%04X..$%X exceeds 16-bit address space",
			c.Name(), c.Base(), end-1)
	}
	for _, existing := range b.comps {
		if rangesOverlap(existing, c) {
			return fmt.Errorf("bus: %q range $%04X..$%04X overlaps %q ($%04X..$%04X)",
				c.Name(), c.Base(), uint16(end-1),
				existing.Name(), existing.Base(), uint16(int(existing.Base())+existing.Size()-1))
		}
	}
	b.comps = append(b.comps, c)
	sort.Slice(b.comps, func(i, j int) bool { return b.comps[i].Base() < b.comps[j].Base() })
	return nil
}

func (b *mapBus) Read(addr uint16) uint8 {
	if c := b.find(addr); c != nil {
		return c.Read(addr - c.Base())
	}
	return 0x00
}

func (b *mapBus) Write(addr uint16, val uint8) {
	if c := b.find(addr); c != nil {
		c.Write(addr-c.Base(), val)
	}
}

func (b *mapBus) Components() []Component {
	out := make([]Component, len(b.comps))
	copy(out, b.comps)
	return out
}

func (b *mapBus) find(addr uint16) Component {
	for _, c := range b.comps {
		base := int(c.Base())
		if int(addr) >= base && int(addr) < base+c.Size() {
			return c
		}
	}
	return nil
}

func rangesOverlap(a, b Component) bool {
	aStart, aEnd := int(a.Base()), int(a.Base())+a.Size()
	bStart, bEnd := int(b.Base()), int(b.Base())+b.Size()
	return aStart < bEnd && bStart < aEnd
}
