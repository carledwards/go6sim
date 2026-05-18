// Package demos holds the 6502 demo programs surfaced by the
// simulator's Demo menu. Each demo is built via the asm package so
// it carries per-instruction comments and named memory symbols
// alongside the bytes — used by the memory window's label view, the
// disassembly column, and (eventually) a region-aware debugger.
//
// Both the terminal binary (cmd/6502-sim) and the wasm binary
// (cmd/6502-wasm) import this package so they share an identical
// demo lineup.
package demos

import "github.com/carledwards/go6sim/asm"

// Memory map a demo author should know:
//
//	$00–$0F      Reserved by convention for "system / scratch" use.
//	             Nothing in the host writes here today, but new demos
//	             should keep these clear and put their own variables
//	             at $10+ for forward compatibility.
//	$10–$FF      User zero page — fast indexed addressing.
//	$0000-$1FFF  RAM (8 KB)
//	$A000-$A3FF  VIC color plane (40×13 cells)
//	$A400-$A7FF  VIC char plane
//	$A800-$A80F  VIC controller registers
//	$B000-$B0FF  6522 VIA #1 (timer-based pacing — see ViaT1* consts).
//	             Registers mirror every 16 bytes through the 256-byte
//	             block; the chip has only 4 register-select pins.
//	$B100-$BFFF  peripheral stack — 256-byte slots for future VIAs,
//	             ACIA, sound, etc. Each slot gets its own CS line via
//	             a 4-to-16 decoder driven by A8..A11.
//	$C000-$DFFF  VIC graphics plane (160×100 @ 4bpp when in graphics mode)
//	$E000-$FFFF  ROM (program loaded here, reset vector at $FFFC)
//
// Pacing animation: write a latch value to ViaT1L_L / ViaT1C_H,
// enable free-run in ACR (bit 6), then poll the IFR T1 flag (bit 6
// of ViaIFR) and clear it by reading T1C-L. See buildBouncingBalls
// for the canonical pattern.

// Common addresses + commands used throughout the demos. Exported so
// demo authors don't redefine them per file.
const (
	// VIC controller registers.
	RegCmd      uint16 = 0xA800
	RegPause    uint16 = 0xA801
	RegFrame    uint16 = 0xA802
	RegRectX    uint16 = 0xA803
	RegRectY    uint16 = 0xA804
	RegRectW    uint16 = 0xA805
	RegRectH    uint16 = 0xA806
	RegGfxColor uint16 = 0xA807
	RegMode     uint16 = 0xA808

	// VIC commands.
	CmdClear         byte = 0x01
	CmdShiftUp       byte = 0x02
	CmdRotLeft       byte = 0x07
	CmdGfxClear      byte = 0x20
	CmdGfxRectFill   byte = 0x23
	CmdGfxFillCircle byte = 0x26

	// Plane bases (color/char plane row N starts at base + 40*N).
	ColorBase uint16 = 0xA000
	CharBase  uint16 = 0xA400

	// 6522 VIA #1 register addresses. Used for timer-based pacing.
	// T1 in free-run mode reloads from latch on each underflow and
	// sets IFR bit 6 — the canonical "tick-pacing" peripheral.
	//
	// Pacing pattern:
	//   1. Write latch low to ViaT1L_L
	//   2. Write latch high to ViaT1C_H — starts T1, clears IFR T1
	//   3. Write $40 to ViaACR — bit 6 = T1 free-run mode
	//   4. Poll: LDA ViaIFR / AND #$40 / BEQ poll
	//   5. Read ViaT1C_L to clear IFR T1, then re-poll
	ViaBase  uint16 = 0xB000
	ViaT1C_L uint16 = 0xB004 // T1 counter low (read clears IFR T1; write = latch low)
	ViaT1C_H uint16 = 0xB005 // T1 counter high (write transfers latch→counter, starts T1)
	ViaT1L_L uint16 = 0xB006 // T1 latch low (no side effects)
	ViaT1L_H uint16 = 0xB007 // T1 latch high (no side effects)
	ViaACR   uint16 = 0xB00B // Aux control: bit 6 = T1 free-run
	ViaIFR   uint16 = 0xB00D // Interrupt flag: bit 6 = T1, bit 7 = any
	ViaIER   uint16 = 0xB00E // Interrupt enable

	// IFR/ACR bits.
	ViaT1Bit byte = 0x40 // IFR/IER bit 6 = T1
)

// Demo is one selectable ROM payload. Each demo starts at $E000;
// the reset vector is wired to point there. Switching demos at
// runtime: clear the ROM, load the new bytes, set the reset vector,
// repaint the host-side display init pattern, then Reset the CPU.
//
// Demo embeds asm.Program, so .Bytes / .Annotations / .Symbols are
// directly accessible on the Demo value.
type Demo struct {
	Name string
	// Description is a short multi-line teaching blurb shown in
	// the demo picker dialog. Each entry is one display line.
	// Optional — omit for demos that don't ship via the picker.
	Description []string
	// RequiresGraphics is true for demos that switch the VIC into
	// graphics mode and rely on the high-resolution pixel plane.
	// Hosts without a graphics plane (the terminal build) should
	// filter these out of their menu — they'd load and run, but
	// produce no visible output.
	RequiresGraphics bool
	asm.Program
}

// Section is a labelled group of demos shown in the Demo menu.
// Sections are separated by a Separator menu item.
type Section struct {
	Demos []Demo
}

// Top-level Demos — built once at package init and reused. main.go
// references these directly for the boot demo (so its Symbols /
// Annotations land on the memory views from frame zero).
var (
	MarqueeDemo = Demo{
		Name: "&Marquee (default)",
		Description: []string{
			"A scrolling text banner across the top row.",
			"Demonstrates VIA T1 free-run pacing and a",
			"polling loop that reads the IFR.",
		},
		Program: asm.MustFromSource("marquee.s", marqueeSrc),
	}
	BouncerDemo = Demo{
		Name: "&Bouncer",
		Description: []string{
			"A character bounces around the text grid.",
			"Velocity flips on edge collisions; pacing",
			"comes from VIA T1 underflow events.",
		},
		Program: asm.MustFromSource("bouncer.s", bouncerSrc),
	}
	ScrollerDemo = Demo{
		Name: "&Scroller",
		Description: []string{
			"Horizontal pixel-resolution text scroll using",
			"the VIC fine-scroll register. Same VIA pacing",
			"as Marquee but smooth instead of stepped.",
		},
		Program: asm.MustFromSource("scroller.s", scrollerSrc),
	}
	SnowDemo = Demo{
		Name: "S&now (LFSR)",
		Description: []string{
			"Hardware-style entropy: an LFSR drives one",
			"random pixel per tick into the char plane.",
			"Stable pattern after enough cycles.",
		},
		Program: asm.MustFromSource("snow.s", snowSrc),
	}
	ScrollerFramedDemo = Demo{
		Name: "Scroller (&framed)",
		Description: []string{
			"Like Scroller, but with a static border drawn",
			"in the color plane. Shows independent layers:",
			"foreground scrolling, background fixed.",
		},
		Program: asm.MustFromSource("scroller_framed.s", scrollerFramedSrc),
	}
	BlitterDemo = Demo{
		Name: "&Blitter (RAM→VIC)",
		Description: []string{
			"Copies a buffer from RAM to the VIC each frame.",
			"Demonstrates the bus throughput needed for",
			"frame-by-frame updates.",
		},
		Program: asm.MustFromSource("blitter.s", blitterSrc),
	}
	QuadDemo = Demo{
		Name: "&Quadrants (4 scrolls)",
		Description: []string{
			"Four independent text scrollers, one per",
			"quadrant of the screen. Stresses the scroll",
			"register and the interleaved update logic.",
		},
		Program: asm.MustFromSource("quad.s", quadSrc),
	}
	BouncingBallsDemo = Demo{
		Name: "&Bouncing Balls (graphics mode)",
		Description: []string{
			"Switches the VIC into 160x100 graphics mode.",
			"Four colored balls bounce off the edges.",
			"Hidden in the terminal build (no pixel plane).",
		},
		RequiresGraphics: true,
		Program:          asm.MustFromSource("balls.s", ballsSrc),
	}
)

// Compatibility byte slices. Older callers loaded these directly
// into ROM; new code should prefer the Demo values above so symbol /
// annotation metadata is available.
var (
	Marquee        = MarqueeDemo.Bytes
	Bouncer        = BouncerDemo.Bytes
	Scroller       = ScrollerDemo.Bytes
	Snow           = SnowDemo.Bytes
	Blitter        = BlitterDemo.Bytes
	ScrollerFramed = ScrollerFramedDemo.Bytes
	Quad           = QuadDemo.Bytes
	BouncingBalls  = BouncingBallsDemo.Bytes
)

// Sections returns the menu lineup. First section is "live" (UI
// updates as memory changes), second is "framed" (UI shows snapshot,
// CPU controls when to commit), third is graphics-mode programs.
func Sections() []Section {
	return []Section{
		{[]Demo{HelloDemo, MarqueeDemo, BouncerDemo, ScrollerDemo, SnowDemo}},
		{[]Demo{ScrollerFramedDemo, BlitterDemo, QuadDemo}},
		{[]Demo{BouncingBallsDemo}},
	}
}
