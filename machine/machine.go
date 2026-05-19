// Package machine assembles presets: a named composition of (clock
// driver, CPU card, device cards) wired onto a Backplane and fronted by
// one instrument.Instrument — the thing the TUI / web / MCP / learn
// edges actually instantiate. See docs/architecture-backplane.md.
//
// Brick 8 of the backplane carve. Two presets:
//   - VICDemo  — the existing standalone machine, composed *exactly* as
//     cmd/6502-sim hand-wires it today (byte-identical = the
//     proof the extraction is faithful; locked by a
//     deterministic demo-execution golden).
//   - TeachMin — the learn machine: RAM + ROM + VIA + a RAM region as
//     framebuffer, no display Controller (resolved Q2: a
//     dumb framebuffer is memory + an external viewport, not
//     a core card; the protocol-bearing VIC is VICDemo-only).
//
// This package is pure composition — it must not import internal/demos
// (presets don't know about demo content; only the test does, for its
// net).
package machine

import (
	"github.com/carledwards/go6sim/backplane"
	"github.com/carledwards/go6sim/clock"
	"github.com/carledwards/go6sim/components/display"
	"github.com/carledwards/go6sim/components/ram"
	"github.com/carledwards/go6sim/components/rom"
	"github.com/carledwards/go6sim/components/via"
	"github.com/carledwards/go6sim/cpu/interp"
	"github.com/carledwards/go6sim/instrument"
)

// The canonical sim-tui memory map (matches the go6asm "sim-tui"
// target and cmd/6502-sim). Programs assembled by go6asm for that
// target run on either preset.
const (
	ramBase   = 0x0000
	ramSize   = 0x2000 // $0000-$1FFF, 8 KB
	colorBase = 0xA000 // VIC color plane
	charBase  = 0xA400 // VIC char plane
	ctrlBase  = 0xA800 // VIC controller registers
	viaBase   = 0xB000 // 6522 VIA #1
	romBase   = 0xE000
	romSize   = 0x2000 // $E000-$FFFF, covers the $FFFA vector block
	dispW     = 40
	dispH     = 13

	// resetOrigin is where go6asm Layer-0 / the sim-tui target loads
	// code and points the reset vector.
	resetOrigin = 0xE000
)

// Machine is an assembled preset: the backplane (cards + bus), the
// clock driver, the instrument surface, and the program-memory ROM.
type Machine struct {
	BP   *backplane.Backplane
	Drv  *clock.Driver
	Inst *instrument.Instrument
	rom  *rom.ROM
}

// Load writes a program image into ROM, sets the reset vector, and
// resets the CPU so it boots into it. Mirrors cmd/6502-sim's
// ROM.Load + SetResetVector + reset sequence.
func (m *Machine) Load(image []byte) error {
	if err := m.rom.Load(0, image); err != nil {
		return err
	}
	if err := m.rom.SetResetVector(resetOrigin); err != nil {
		return err
	}
	m.Drv.Reset()
	return nil
}

// VICDemo composes the standalone machine exactly as cmd/6502-sim does:
// RAM, the VIC color/char planes + controller, a 1 MHz 6522 VIA, ROM,
// interp CPU. (The wasm build additionally wires a graphics plane; that
// variant can be added later — it is not needed by the learn path.)
func VICDemo() *Machine {
	bp := backplane.New()
	color := display.New("display.color", colorBase, dispW, dispH)
	chars := display.New("display.char", charBase, dispW, dispH)
	r := rom.New("rom", romBase, romSize)

	must(bp.Attach(ram.New("ram", ramBase, ramSize)))
	must(bp.Attach(color))
	must(bp.Attach(chars))
	must(bp.Attach(display.NewController("display.ctrl", ctrlBase, color, chars)))
	must(bp.Attach(via.New("via1", viaBase, 1_000_000)))
	must(bp.Attach(r))

	backend := interp.New(bp)
	backend.Reset()
	drv := clock.NewDriver(backend)
	return &Machine{BP: bp, Drv: drv, Inst: instrument.New(bp, drv), rom: r}
}

// TeachMin composes the minimal learn machine: main RAM, a RAM region
// standing in for the framebuffer ($A000-$AFFF — covers the sim-tui
// color/char planes so go6asm-assembled lessons that poke CharBase land
// and a consumer reads the bytes back), a 1 MHz VIA (timers/IRQ — the
// "real hardware" payoff), ROM, interp CPU. No display Controller: a
// dumb framebuffer is memory, not a core card.
func TeachMin() *Machine {
	bp := backplane.New()
	r := rom.New("rom", romBase, romSize)

	must(bp.Attach(ram.New("ram", ramBase, ramSize)))
	must(bp.Attach(ram.New("framebuffer", colorBase, 0x1000))) // $A000-$AFFF
	must(bp.Attach(via.New("via1", viaBase, 1_000_000)))
	must(bp.Attach(r))

	backend := interp.New(bp)
	backend.Reset()
	drv := clock.NewDriver(backend)
	return &Machine{BP: bp, Drv: drv, Inst: instrument.New(bp, drv), rom: r}
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
