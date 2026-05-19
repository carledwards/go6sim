// Package via models a 65C22 (W65C22S) Versatile Interface Adapter.
//
// The 6522 VIA was the workhorse peripheral chip on Apple II, Vic-20,
// Commodore PET, BBC Micro, and many other 6502-era machines. It
// provides:
//
//   - two 8-bit parallel I/O ports (A and B) with handshaking,
//   - two 16-bit timer/counters (T1 and T2),
//   - a shift register,
//   - eight interrupt sources gated through a flag (IFR) and an
//     enable (IER) register.
//
// Phase 1 (this file) implements only what the simulator needs today:
//
//   - **Timer 1**, free-running or one-shot mode, software-poll only
//     (no IRQ line wiring yet — programs check the IFR T1 bit).
//   - **IFR / IER** semantics so polling code reads / clears flags
//     exactly like the datasheet specifies.
//   - All other registers (ports, T2, SR, ACR shift bits, PCR) read /
//     write a backing byte but have no side effects yet — they're
//     wired so a programmer can poke at them in the inspector
//     without creating a misleading impression that the chip ignores
//     those addresses.
//
// The chip is driven by its own oscillator, not the CPU clock — this
// is the critical design point for a teaching simulator: when you
// halt the CPU (single-step, breakpoint, "Stop"), the VIA timer keeps
// running. Real W65C22S boards can be clocked the same way, so demos
// that pace off T1 transfer to silicon unmodified.
package via

import (
	"time"

	"github.com/carledwards/go6sim/asm"
	"github.com/carledwards/go6sim/bus"
)

// RegBlockSize is the count of distinct registers on the chip — the
// W65C22S has 4 register-select pins (RS0..RS3), giving 16 selectable
// regs.
const RegBlockSize = 16

// CSBlockSize is the chip-select region the chip claims on the bus.
// On real hardware a 6522's CS is asserted across whatever block its
// decoder gives it; the chip only honours the low 4 bits (RS pins),
// so register addresses mirror across the block. We model the same:
// the bus routes any address in [base, base+CSBlockSize) here, and
// Read/Write mask the offset to 4 bits to pick a register.
const CSBlockSize = 256

// Size is the bus footprint claimed by this component.
const Size = CSBlockSize

// Register offsets within the 16-byte block. Names match the W65C22S
// datasheet, abbreviated:
//
//	IORB / ORB    = port B input / output
//	DDRB / DDRA   = data direction
//	T1C-L / T1C-H = timer-1 counter low / high
//	T1L-L / T1L-H = timer-1 latch  low / high
//	T2C-L / T2C-H = timer-2 counter low / high
//	SR            = shift register
//	ACR           = auxiliary control
//	PCR           = peripheral control
//	IFR           = interrupt flag
//	IER           = interrupt enable
//	ORA-NH        = port A, no handshake
const (
	RegORB   uint16 = 0x0
	RegORA   uint16 = 0x1
	RegDDRB  uint16 = 0x2
	RegDDRA  uint16 = 0x3
	RegT1CL  uint16 = 0x4
	RegT1CH  uint16 = 0x5
	RegT1LL  uint16 = 0x6
	RegT1LH  uint16 = 0x7
	RegT2CL  uint16 = 0x8
	RegT2CH  uint16 = 0x9
	RegSR    uint16 = 0xA
	RegACR   uint16 = 0xB
	RegPCR   uint16 = 0xC
	RegIFR   uint16 = 0xD
	RegIER   uint16 = 0xE
	RegORANH uint16 = 0xF
)

// IFR bits. IFR_IRQ is read-only — it reflects "any enabled flag is
// set", computed at read time. Writing 1s to IFR clears the
// corresponding flags (write-1-to-clear), bit 7 ignored.
const (
	IFR_CA2 uint8 = 1 << 0
	IFR_CA1 uint8 = 1 << 1
	IFR_SR  uint8 = 1 << 2
	IFR_CB2 uint8 = 1 << 3
	IFR_CB1 uint8 = 1 << 4
	IFR_T2  uint8 = 1 << 5
	IFR_T1  uint8 = 1 << 6
	IFR_IRQ uint8 = 1 << 7
)

// ACR bits used by Phase 1.
//
//	bit 6 = T1 free-running (1) or one-shot (0)
//	bit 7 = T1 PB7 output enable (Phase 2 — ignored here)
//
// Other bits (T2 mode, SR mode, port latching) are stored but
// inert in Phase 1.
const (
	ACR_T1_FREERUN uint8 = 1 << 6
	ACR_T1_PB7     uint8 = 1 << 7
)

// VIA is one 65C22 chip. Address it via Read/Write at offsets in
// [0, Size). It implements bus.Component and bus.Ticker so the host
// can drive its timer on wall-clock time.
type VIA struct {
	name      string
	base      uint16
	crystalHz uint64

	// Stub registers — semantics deferred to Phase 2. They store and
	// return what the CPU writes, so demos can experiment without
	// the values silently disappearing.
	orb, ora, ddrb, ddra uint8
	sr, pcr              uint8

	// Live registers.
	acr uint8
	ifr uint8 // bits 0..6 only; bit 7 is computed on read
	ier uint8 // bits 0..6 only

	// Timer 1 state.
	t1Counter uint16
	t1Latch   uint16
	t1Armed   bool // false until first write to T1C-H starts the timer

	// Wall-clock-to-cycle accumulator. Carry over the sub-cycle
	// remainder so we don't drop time across Tick boundaries.
	carryNs int64
}

// New builds a VIA at the given bus base. crystalHz is the timer's
// own oscillator frequency — typical values are 1_000_000 (1 MHz,
// matches Apple II / VIC-20) or 2_000_000.
func New(name string, base uint16, crystalHz uint64) *VIA {
	if crystalHz == 0 {
		crystalHz = 1_000_000
	}
	return &VIA{name: name, base: base, crystalHz: crystalHz}
}

// CrystalHz returns the timer oscillator frequency. Useful when a
// program wants to compute "how many cycles per millisecond" at
// runtime.
func (v *VIA) CrystalHz() uint64 { return v.crystalHz }

// --- bus.Component ---

func (v *VIA) Name() string { return v.name }
func (v *VIA) Base() uint16 { return v.base }
func (v *VIA) Size() int    { return Size }

// Taps exposes read-only observables (bus.Tappable): the IRQ-asserted
// state (any enabled IFR flag set — what a wired IRQ line *would*
// drive) and the live T1 counter. Per-source attribution is emergent:
// each VIA is its own card, so the instrument keys these under the
// card's name.
func (v *VIA) Taps() []bus.Tap {
	return []bus.Tap{
		{Name: "irq", Read: func() uint64 {
			if v.ifr&v.ier&0x7F != 0 {
				return 1
			}
			return 0
		}},
		{Name: "t1", Read: func() uint64 { return uint64(v.t1Counter) }},
	}
}

// IRQAsserted implements bus.IRQSource: the chip pulls the shared IRQ
// line low whenever an enabled IFR flag is set — exactly the condition
// IFR bit 7 reports on read. The backplane wired-ORs this into the line
// the CPU honors (brick 9b).
func (v *VIA) IRQAsserted() bool {
	return v.ifr&v.ier&0x7F != 0
}

// Read returns the register's current value and applies any read
// side effects (notably: reading T1C-L clears the T1 IFR flag).
//
// The 6522 has only 4 register-select pins, so any access within
// the chip-select block aliases to one of 16 registers — we mask
// the offset to the low 4 bits to model that mirroring.
func (v *VIA) Read(off uint16) uint8 {
	off &= 0x0F
	switch off {
	case RegORB:
		return v.orb
	case RegORA:
		return v.ora
	case RegDDRB:
		return v.ddrb
	case RegDDRA:
		return v.ddra
	case RegT1CL:
		// Read of T1C-L clears the T1 interrupt flag. This is the
		// canonical "ack the timer" trick on a 65C22.
		v.ifr &^= IFR_T1
		return uint8(v.t1Counter & 0xFF)
	case RegT1CH:
		return uint8(v.t1Counter >> 8)
	case RegT1LL:
		return uint8(v.t1Latch & 0xFF)
	case RegT1LH:
		return uint8(v.t1Latch >> 8)
	case RegT2CL, RegT2CH:
		// Phase 2 — return 0 for now. Real T2 has its own
		// counter / latch / IFR semantics distinct from T1.
		return 0
	case RegSR:
		return v.sr
	case RegACR:
		return v.acr
	case RegPCR:
		return v.pcr
	case RegIFR:
		out := v.ifr & 0x7F
		// Bit 7 (IRQ) reads as 1 iff any enabled flag is set.
		if (v.ifr & v.ier & 0x7F) != 0 {
			out |= IFR_IRQ
		}
		return out
	case RegIER:
		// Bit 7 reads as 1 always (per datasheet).
		return v.ier | 0x80
	case RegORANH:
		return v.ora
	}
	return 0
}

// Write applies the register's write side effects. The most
// load-bearing writes:
//
//	$5 (T1C-H): copies latch → counter, clears IFR T1, starts T1.
//	$D (IFR):   write-1-to-clear; bit 7 ignored.
//	$E (IER):   bit 7=1 sets the masked bits, bit 7=0 clears them.
func (v *VIA) Write(off uint16, val uint8) {
	off &= 0x0F
	switch off {
	case RegORB:
		v.orb = val
	case RegORA:
		v.ora = val
	case RegDDRB:
		v.ddrb = val
	case RegDDRA:
		v.ddra = val
	case RegT1CL:
		// Latch low only — does not load counter.
		v.t1Latch = (v.t1Latch & 0xFF00) | uint16(val)
	case RegT1CH:
		// Latch high, transfer latch→counter, clear IFR T1, start.
		v.t1Latch = (v.t1Latch & 0x00FF) | (uint16(val) << 8)
		v.t1Counter = v.t1Latch
		v.ifr &^= IFR_T1
		v.t1Armed = true
		v.carryNs = 0
	case RegT1LL:
		v.t1Latch = (v.t1Latch & 0xFF00) | uint16(val)
	case RegT1LH:
		v.t1Latch = (v.t1Latch & 0x00FF) | (uint16(val) << 8)
		// Datasheet: writing T1L-H clears T1 interrupt flag too.
		v.ifr &^= IFR_T1
	case RegT2CL, RegT2CH:
		// Phase 2.
	case RegSR:
		v.sr = val
	case RegACR:
		v.acr = val
	case RegPCR:
		v.pcr = val
	case RegIFR:
		// Write-1-to-clear; bit 7 has no effect on writes.
		v.ifr &^= (val & 0x7F)
	case RegIER:
		if val&0x80 != 0 {
			// Set: bits with 1 in val become enabled.
			v.ier |= val & 0x7F
		} else {
			// Clear: bits with 1 in val become disabled.
			v.ier &^= val & 0x7F
		}
	case RegORANH:
		v.ora = val
	}
}

// --- bus.Ticker ---

// Tick advances Timer 1 by dt of wall-clock time, converted via the
// chip's own crystal. Does nothing until T1 is armed by a write to
// T1C-H. In free-running mode the counter auto-reloads from the
// latch on each underflow; in one-shot mode it underflows once and
// stops setting IFR T1 until the program restarts it.
func (v *VIA) Tick(dt time.Duration) {
	if !v.t1Armed {
		// Even if not armed, the counter physically would decrement,
		// but for Phase 1 we don't model it pre-arm. Keeps the carry
		// pristine.
		return
	}

	// Convert dt → cycles, carrying the sub-cycle remainder across
	// Ticks so we don't drift over many minutes.
	totalNs := v.carryNs + dt.Nanoseconds()
	nsPerCycle := int64(1_000_000_000 / v.crystalHz)
	if nsPerCycle <= 0 {
		nsPerCycle = 1
	}
	cycles := totalNs / nsPerCycle
	v.carryNs = totalNs - cycles*nsPerCycle
	if cycles <= 0 {
		return
	}

	freeRun := (v.acr & ACR_T1_FREERUN) != 0

	// Multi-underflow loop. dt can easily be 50+ ms (50_000 cycles at
	// 1 MHz) and the counter is 16-bit, so a single Tick may roll the
	// counter over many times when latch is small.
	for cycles > 0 {
		// On a real 65C22, T1 underflow happens on the cycle that
		// takes the counter from $0000 to $FFFF — i.e. counter+1
		// cycles from now.
		cyclesToUnderflow := int64(v.t1Counter) + 1
		if cycles < cyclesToUnderflow {
			v.t1Counter -= uint16(cycles)
			return
		}
		cycles -= cyclesToUnderflow
		v.ifr |= IFR_T1
		if freeRun {
			// Reload from latch and keep counting. (Datasheet says
			// the period is latch+2; we use latch+1 — close enough
			// for software pacing, which polls IFR.)
			v.t1Counter = v.t1Latch
		} else {
			// One-shot: counter wraps to $FFFF and just keeps
			// counting. We don't bother modeling further underflows
			// past the first — the program is responsible for
			// restarting it via a fresh write to T1C-H.
			v.t1Counter = 0xFFFF
			v.t1Armed = false
			return
		}
	}
}

// Symbols returns this VIA's register layout as named symbols, with
// absolute addresses computed from the chip's bus base. Implements
// bus.Labeller so the memory window's Labels view annotates the
// register block automatically.
func (v *VIA) Symbols() []asm.Symbol {
	b := v.base
	return []asm.Symbol{
		{Name: "VIA_ORB", Addr: b + RegORB, Size: 1, Note: "port B output / IORB"},
		{Name: "VIA_ORA", Addr: b + RegORA, Size: 1, Note: "port A output / IORA (handshake)"},
		{Name: "VIA_DDRB", Addr: b + RegDDRB, Size: 1, Note: "data direction B"},
		{Name: "VIA_DDRA", Addr: b + RegDDRA, Size: 1, Note: "data direction A"},
		{Name: "VIA_T1C_L", Addr: b + RegT1CL, Size: 1, Note: "T1 counter low (read clears IFR T1)"},
		{Name: "VIA_T1C_H", Addr: b + RegT1CH, Size: 1, Note: "T1 counter high (write starts T1)"},
		{Name: "VIA_T1L_L", Addr: b + RegT1LL, Size: 1, Note: "T1 latch low"},
		{Name: "VIA_T1L_H", Addr: b + RegT1LH, Size: 1, Note: "T1 latch high"},
		{Name: "VIA_T2C_L", Addr: b + RegT2CL, Size: 1, Note: "T2 counter low (Phase 2)"},
		{Name: "VIA_T2C_H", Addr: b + RegT2CH, Size: 1, Note: "T2 counter high (Phase 2)"},
		{Name: "VIA_SR", Addr: b + RegSR, Size: 1, Note: "shift register"},
		{Name: "VIA_ACR", Addr: b + RegACR, Size: 1, Note: "aux ctl: bit6=T1 free-run"},
		{Name: "VIA_PCR", Addr: b + RegPCR, Size: 1, Note: "peripheral control"},
		{Name: "VIA_IFR", Addr: b + RegIFR, Size: 1, Note: "interrupt flags (bit6=T1, bit7=any)"},
		{Name: "VIA_IER", Addr: b + RegIER, Size: 1, Note: "interrupt enable"},
		{Name: "VIA_ORANH", Addr: b + RegORANH, Size: 1, Note: "port A no handshake"},
	}
}

// Snapshot is a read-only view of the chip's current state. Callers
// (notably the VIA debug window) use this to render live values
// without triggering register-read side effects — reading T1C-L
// through Read clears IFR T1, but a passive viewer must not.
type Snapshot struct {
	ORA, ORB, DDRA, DDRB uint8
	SR, PCR              uint8
	ACR                  uint8
	IFR, IER             uint8
	T1Counter, T1Latch   uint16
	T1Armed              bool
	T1FreeRun            bool
	CrystalHz            uint64
}

// Snapshot returns the current chip state without side effects.
func (v *VIA) Snapshot() Snapshot {
	return Snapshot{
		ORA: v.ora, ORB: v.orb,
		DDRA: v.ddra, DDRB: v.ddrb,
		SR: v.sr, PCR: v.pcr,
		ACR:       v.acr,
		IFR:       v.ifr,
		IER:       v.ier,
		T1Counter: v.t1Counter,
		T1Latch:   v.t1Latch,
		T1Armed:   v.t1Armed,
		T1FreeRun: (v.acr & ACR_T1_FREERUN) != 0,
		CrystalHz: v.crystalHz,
	}
}

// Reset clears all registers and disarms the timer. Call this when
// the host wants a clean slate (e.g. on demo reload).
func (v *VIA) Reset() {
	v.orb, v.ora = 0, 0
	v.ddrb, v.ddra = 0, 0
	v.sr, v.pcr = 0, 0
	v.acr = 0
	v.ifr = 0
	v.ier = 0
	v.t1Counter, v.t1Latch = 0, 0
	v.t1Armed = false
	v.carryNs = 0
}
