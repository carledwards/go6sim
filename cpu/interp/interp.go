// Package interp is a textbook interpretive 6502 implementation that
// satisfies cpu.Backend. Routes all memory access through bus.Bus.
//
// The implementation is instruction-grained: each opcode executes in
// full on its first HalfStep, then the adapter "burns" the remaining
// half-cycles before fetching the next opcode. Bus reads/writes
// happen at instant-of-execution rather than spread across the
// instruction's individual cycles. For the demo this is fine — the
// netsim adapter is what you reach for if you want cycle-accurate
// bus timing.
//
// The core emulates either an NMOS 6502 (the default, matching the
// C64's 6510) or a CMOS 65C02 — select with NewArch. All 151 official
// NMOS opcodes plus the 65C02 additions (BRA, PHX/PHY/PLX/PLY, STZ,
// TRB/TSB, INC/DEC A, (zp) addressing, BIT immediate, JMP (abs,x)) are
// covered; remaining undocumented opcodes decode as NOP. ADC/SBC honour
// decimal mode with the correct NMOS and CMOS flag behaviours. The
// variants also differ in the JMP-indirect page bug (present on NMOS,
// fixed on CMOS) and in clearing decimal mode on interrupt entry (CMOS
// only).
package interp

import (
	"github.com/carledwards/go6sim/bus"
	"github.com/carledwards/go6sim/cpu"
)

const (
	flagC = cpu.FlagC
	flagZ = cpu.FlagZ
	flagI = cpu.FlagI
	flagD = cpu.FlagD
	flagB = cpu.FlagB
	flagU = cpu.FlagU
	flagV = cpu.FlagV
	flagN = cpu.FlagN
)

// Adapter implements cpu.Backend over a Bus.
type Adapter struct {
	bus  bus.Bus
	arch cpu.Arch

	a, x, y, sp, p uint8
	pc             uint16

	halfCyclesLeft  int
	halfCyclesTotal uint64

	// deltaCycles holds per-instruction cycle adjustments computed
	// during execution (e.g. the extra cycle a 65C02 spends in
	// decimal-mode ADC/SBC). Reset at the start of every execute().
	deltaCycles int

	// Last bus-access record — surfaced via AddressBus / DataBus /
	// ReadCycle so the UI can display A, D, R/W just like netsim.
	lastAddr    uint16
	lastData    uint8
	lastWasRead bool

	// sync mirrors a real 6502's SYNC pin: true on the half-step
	// where the next instruction's logic is actually executed
	// (analogous to T1 / opcode fetch), false on the burn ticks
	// that pad out the instruction's published cycle count.
	sync bool

	// brkCount increments whenever a BRK vectors through $FFFE — a
	// read-only observable for the instrument's vector-taken
	// breakpoint and Taps. interp models BRK only; hardware IRQ
	// delivery is a separate feature (docs/architecture-backplane.md).
	brkCount uint64

	// prevNMI remembers whether the bus's NMI line was asserted at
	// the last instruction boundary. The 6502 services NMI on a
	// rising edge (false → true sample), so we fire serviceNMI only
	// when the current sample is asserted AND the previous was not.
	// Released by the assertion source (the Pump's auto-deassert)
	// before the next pulse.
	prevNMI bool
}

// New creates an NMOS Adapter wired to the given bus. Call Reset before
// HalfStep — the constructor leaves the CPU in a zeroed state. NMOS is
// the default because it matches the 6510 in the Commodore 64.
func New(b bus.Bus) *Adapter { return &Adapter{bus: b, arch: cpu.NMOS} }

// NewArch is like New but selects the 6502 variant (cpu.NMOS or
// cpu.CMOS). The variant changes decimal-mode interrupt handling, the
// JMP-indirect bug, and which extra opcodes decode.
func NewArch(b bus.Bus, arch cpu.Arch) *Adapter { return &Adapter{bus: b, arch: arch} }

// Arch reports which 6502 variant this Adapter emulates.
func (a *Adapter) Arch() cpu.Arch { return a.arch }

// SetSP overwrites the stack pointer. Useful for test harnesses that
// place the CPU in a known state without running a full reset.
func (a *Adapter) SetSP(sp uint8) { a.sp = sp }

// SetA, SetX, SetY, SetP overwrite the remaining architectural
// registers. Like SetSP, these are state-injection hooks for tests; the
// running emulator never needs them.
func (a *Adapter) SetA(v uint8) { a.a = v }
func (a *Adapter) SetX(v uint8) { a.x = v }
func (a *Adapter) SetY(v uint8) { a.y = v }
func (a *Adapter) SetP(v uint8) { a.p = v }

func (a *Adapter) Reset() {
	a.a, a.x, a.y = 0, 0, 0
	a.sp = 0xFD
	a.p = flagI | flagU
	a.pc = a.read16(0xFFFC)
	a.halfCyclesLeft = 7*2 - 1 // reset takes 7 cycles
	a.halfCyclesTotal = 0
	a.brkCount = 0
	a.sync = false
}

func (a *Adapter) HalfStep() {
	if a.halfCyclesLeft > 0 {
		a.halfCyclesLeft--
		a.sync = false
	} else {
		// At an instruction boundary a real 6502 prioritises
		// NMI > IRQ > normal-fetch. NMI is edge-triggered (rising
		// edge of the line); IRQ is level-sensitive AND gated on the
		// I-flag. Either path runs in place of the next opcode
		// fetch so programs that never invoke interrupts stay
		// byte-identical.
		switch {
		case a.nmiEdge():
			a.serviceNMI()
		case a.irqPending():
			a.serviceIRQ()
		default:
			a.execute()
		}
		// Mirror a real 6502's SYNC line going high during opcode
		// fetch (T1) / interrupt entry. Interp does the whole
		// instruction in one call, so SYNC is true on this boundary
		// half-step and false on the burn ticks that follow.
		a.sync = true
	}
	a.halfCyclesTotal++
}

func (a *Adapter) Registers() cpu.Registers {
	return cpu.Registers{A: a.a, X: a.x, Y: a.y, S: a.sp, P: a.p, PC: a.pc}
}

// SetPC overwrites the program counter and resets the half-cycle
// accumulator so the next HalfStep treats the address as an
// instruction-boundary fetch. Implements cpu.PCSetter — the
// debugger uses it for the "g <addr>" flow (set PC then run).
func (a *Adapter) SetPC(pc uint16) {
	a.pc = pc
	a.halfCyclesLeft = 0
	a.sync = false
	a.prevNMI = false // edge-trigger surface starts fresh after a manual jump
}

// irqPending reports whether the backplane is asserting the shared IRQ
// line and interrupts are enabled (I clear). Type-asserted so a plain
// bus with no IRQ aggregation simply never interrupts — non-backplane
// wirings and interrupt-free programs are wholly unaffected.
func (a *Adapter) irqPending() bool {
	if a.p&flagI != 0 {
		return false
	}
	src, ok := a.bus.(interface{ IRQ() bool })
	return ok && src.IRQ()
}

// serviceIRQ runs the 6502 hardware-IRQ sequence: push PC and P (B
// clear, U set), set I, vector through $FFFE. 7 cycles — like BRK but
// B=0 on the pushed status, and PC is not pre-incremented.
func (a *Adapter) serviceIRQ() {
	a.push(uint8(a.pc >> 8))
	a.push(uint8(a.pc))
	a.push((a.p | flagU) &^ flagB)
	a.setFlag(flagI, true)
	if a.arch == cpu.CMOS {
		a.setFlag(flagD, false) // 65C02 clears decimal mode on interrupt entry
	}
	a.pc = a.read16(0xFFFE)
	a.halfCyclesLeft = 2*7 - 1
}

// nmiEdge reports whether the bus's NMI line just transitioned from
// inactive to asserted since the previous instruction boundary —
// i.e., a rising edge. NMI on the 6502 is edge-triggered: holding
// the line asserted across many boundaries fires it ONCE, until the
// source deasserts and re-asserts. The bus may not implement a
// reader (older wirings); in that case we never fire NMI.
func (a *Adapter) nmiEdge() bool {
	src, ok := a.bus.(interface{ NMI() bool })
	if !ok {
		return false
	}
	curr := src.NMI()
	edge := curr && !a.prevNMI
	a.prevNMI = curr
	return edge
}

// serviceNMI runs the 6502 hardware-NMI sequence: push PC and P (B
// clear, U set), vector through $FFFA. 7 cycles. Differs from IRQ in
// the vector and in NOT setting I — NMI can preempt itself (in
// principle; the edge gate prevents a re-fire while the line is
// still asserted).
func (a *Adapter) serviceNMI() {
	a.push(uint8(a.pc >> 8))
	a.push(uint8(a.pc))
	a.push((a.p | flagU) &^ flagB)
	if a.arch == cpu.CMOS {
		a.setFlag(flagD, false) // 65C02 clears decimal mode on interrupt entry
	}
	a.pc = a.read16(0xFFFA)
	a.halfCyclesLeft = 2*7 - 1
}

func (a *Adapter) HalfCycles() uint64 { return a.halfCyclesTotal }

func (a *Adapter) AddressBus() uint16 { return a.lastAddr }
func (a *Adapter) DataBus() uint8     { return a.lastData }
func (a *Adapter) ReadCycle() bool    { return a.lastWasRead }

// interp doesn't model interrupt inputs — they're always inactive.
func (a *Adapter) IRQ() bool  { return true }
func (a *Adapter) NMI() bool  { return true }
func (a *Adapter) SYNC() bool { return a.sync }

// Taps exposes read-only observables (bus.Tappable). interp publishes
// the cumulative BRK-vector count; the instrument diffs it to detect a
// vector-taken event for break-on-vector debugging.
func (a *Adapter) Taps() []bus.Tap {
	return []bus.Tap{
		{Name: "brk", Read: func() uint64 { return a.brkCount }},
	}
}

// MaxHz reports interp as uncapped. The Go interpreter dispatches each
// 6502 instruction in tens of nanoseconds, so the realistic ceiling is
// well above any rate the picker offers.
func (a *Adapter) MaxHz() int { return 0 }

var _ cpu.Backend = (*Adapter)(nil)

// ---------- memory + flag helpers ----------

func (a *Adapter) read(addr uint16) uint8 {
	v := a.bus.Read(addr)
	a.lastAddr = addr
	a.lastData = v
	a.lastWasRead = true
	return v
}

func (a *Adapter) write(addr uint16, v uint8) {
	a.bus.Write(addr, v)
	a.lastAddr = addr
	a.lastData = v
	a.lastWasRead = false
}

func (a *Adapter) read16(addr uint16) uint16 {
	return uint16(a.read(addr)) | uint16(a.read(addr+1))<<8
}

func (a *Adapter) push(v uint8) {
	a.write(0x0100|uint16(a.sp), v)
	a.sp--
}

func (a *Adapter) pull() uint8 {
	a.sp++
	return a.read(0x0100 | uint16(a.sp))
}

func (a *Adapter) setFlag(f uint8, on bool) {
	if on {
		a.p |= f
	} else {
		a.p &^= f
	}
}

func (a *Adapter) setZN(v uint8) {
	a.setFlag(flagZ, v == 0)
	a.setFlag(flagN, v&0x80 != 0)
}

// ---------- addressing modes (return effective address, advance PC) ----------

func (a *Adapter) amIM() uint16 { addr := a.pc; a.pc++; return addr }
func (a *Adapter) amZP() uint16 { v := a.read(a.pc); a.pc++; return uint16(v) }
func (a *Adapter) amZPX() uint16 {
	v := a.read(a.pc) + a.x
	a.pc++
	return uint16(v)
}
func (a *Adapter) amZPY() uint16 {
	v := a.read(a.pc) + a.y
	a.pc++
	return uint16(v)
}
func (a *Adapter) amABS() uint16 { addr := a.read16(a.pc); a.pc += 2; return addr }
func (a *Adapter) amABSX() uint16 {
	addr := a.read16(a.pc) + uint16(a.x)
	a.pc += 2
	return addr
}
func (a *Adapter) amABSY() uint16 {
	addr := a.read16(a.pc) + uint16(a.y)
	a.pc += 2
	return addr
}
// amIND models the canonical 6502 JMP indirect bug: the high byte of
// the target is fetched from the same page as the low byte even when
// the pointer's low byte is 0xFF.
func (a *Adapter) amIND() uint16 {
	ptr := a.read16(a.pc)
	a.pc += 2
	lo := a.read(ptr)
	hi := a.read((ptr & 0xFF00) | uint16(uint8(ptr)+1))
	return uint16(lo) | uint16(hi)<<8
}
func (a *Adapter) amINDX() uint16 {
	zp := a.read(a.pc) + a.x
	a.pc++
	lo := a.read(uint16(zp))
	hi := a.read(uint16(zp + 1))
	return uint16(lo) | uint16(hi)<<8
}
func (a *Adapter) amINDY() uint16 {
	zp := a.read(a.pc)
	a.pc++
	lo := a.read(uint16(zp))
	hi := a.read(uint16(zp + 1))
	base := uint16(lo) | uint16(hi)<<8
	return base + uint16(a.y)
}

// ---------- operation primitives ----------

func (a *Adapter) opORA(v uint8) { a.a |= v; a.setZN(a.a) }
func (a *Adapter) opAND(v uint8) { a.a &= v; a.setZN(a.a) }
func (a *Adapter) opEOR(v uint8) { a.a ^= v; a.setZN(a.a) }

func (a *Adapter) opADC(v uint8) {
	if a.p&flagD != 0 {
		if a.arch == cpu.CMOS {
			a.adcDecimalCMOS(v)
		} else {
			a.adcDecimalNMOS(v)
		}
		return
	}
	cin := uint16(0)
	if a.p&flagC != 0 {
		cin = 1
	}
	sum := uint16(a.a) + uint16(v) + cin
	a.setFlag(flagC, sum > 0xFF)
	// Overflow when sign of operands matches and differs from result.
	a.setFlag(flagV, ((a.a^uint8(sum))&(v^uint8(sum))&0x80) != 0)
	a.a = uint8(sum)
	a.setZN(a.a)
}

func (a *Adapter) opSBC(v uint8) {
	if a.p&flagD != 0 {
		if a.arch == cpu.CMOS {
			a.sbcDecimalCMOS(v)
		} else {
			a.sbcDecimalNMOS(v)
		}
		return
	}
	a.opADC(^v)
}

func (a *Adapter) carryIn() uint32 {
	if a.p&flagC != 0 {
		return 1
	}
	return 0
}

// adcDecimalNMOS implements BCD add-with-carry with NMOS 6502 flag
// semantics: carry and the final value are BCD-corrected; N/Z come from
// the corrected result, V from the binary high-nibble overflow.
func (a *Adapter) adcDecimalNMOS(v uint8) {
	acc, add := uint32(a.a), uint32(v)
	lo := (acc & 0x0f) + (add & 0x0f) + a.carryIn()
	var carrylo uint32
	if lo >= 0x0a {
		carrylo = 0x10
		lo -= 0x0a
	}
	hi := (acc & 0xf0) + (add & 0xf0) + carrylo
	if hi >= 0xa0 {
		a.setFlag(flagC, true)
		hi -= 0xa0
	} else {
		a.setFlag(flagC, false)
	}
	res := hi | lo
	a.setFlag(flagV, (acc^res)&0x80 != 0 && (acc^add)&0x80 == 0)
	a.a = uint8(res)
	a.setZN(a.a)
}

// adcDecimalCMOS implements BCD add-with-carry with 65C02 flag
// semantics: overflow is seeded before correction, then narrowed, and
// the instruction takes one extra cycle.
func (a *Adapter) adcDecimalCMOS(v uint8) {
	acc, add := uint32(a.a), uint32(v)
	a.setFlag(flagV, (acc^add)&0x80 == 0)
	lo := (acc & 0x0f) + (add & 0x0f) + a.carryIn()
	var carrylo uint32
	if lo >= 0x0a {
		carrylo = 0x10
		lo -= 0x0a
	}
	hi := (acc & 0xf0) + (add & 0xf0) + carrylo
	if hi >= 0xa0 {
		a.setFlag(flagC, true)
		if hi >= 0x180 {
			a.setFlag(flagV, false)
		}
		hi -= 0xa0
	} else {
		a.setFlag(flagC, false)
		if hi < 0x80 {
			a.setFlag(flagV, false)
		}
	}
	a.a = uint8(hi | lo)
	a.setZN(a.a)
	a.deltaCycles++
}

// sbcDecimalNMOS implements BCD subtract-with-borrow, NMOS flag rules.
func (a *Adapter) sbcDecimalNMOS(v uint8) {
	acc, sub := uint32(a.a), uint32(v)
	lo := 0x0f + (acc & 0x0f) - (sub & 0x0f) + a.carryIn()
	var carrylo uint32
	if lo < 0x10 {
		lo -= 0x06
	} else {
		lo -= 0x10
		carrylo = 0x10
	}
	hi := 0xf0 + (acc & 0xf0) - (sub & 0xf0) + carrylo
	if hi < 0x100 {
		a.setFlag(flagC, false)
		hi -= 0x60
	} else {
		a.setFlag(flagC, true)
		hi -= 0x100
	}
	res := hi | lo
	a.setFlag(flagV, (acc^res)&0x80 != 0 && (acc^sub)&0x80 != 0)
	a.a = uint8(res)
	a.setZN(a.a)
}

// sbcDecimalCMOS implements BCD subtract-with-borrow, 65C02 flag rules
// (overflow seeded before correction, +1 cycle).
func (a *Adapter) sbcDecimalCMOS(v uint8) {
	acc, sub := uint32(a.a), uint32(v)
	a.setFlag(flagV, (acc^sub)&0x80 != 0)
	lo := 0x0f + (acc & 0x0f) - (sub & 0x0f) + a.carryIn()
	var carrylo uint32
	if lo < 0x10 {
		lo -= 0x06
	} else {
		lo -= 0x10
		carrylo = 0x10
	}
	hi := 0xf0 + (acc & 0xf0) - (sub & 0xf0) + carrylo
	if hi < 0x100 {
		a.setFlag(flagC, false)
		if hi < 0x80 {
			a.setFlag(flagV, false)
		}
		hi -= 0x60
	} else {
		a.setFlag(flagC, true)
		if hi >= 0x180 {
			a.setFlag(flagV, false)
		}
		hi -= 0x100
	}
	a.a = uint8(hi | lo)
	a.setZN(a.a)
	a.deltaCycles++
}

// trb / tsb are 65C02 read-modify-write bit ops. Both set Z from the
// pre-modify AND with A; TSB sets the masked bits, TRB clears them.
func (a *Adapter) tsb(addr uint16) {
	m := a.read(addr)
	a.setFlag(flagZ, a.a&m == 0)
	a.write(addr, m|a.a)
}

func (a *Adapter) trb(addr uint16) {
	m := a.read(addr)
	a.setFlag(flagZ, a.a&m == 0)
	a.write(addr, m&^a.a)
}

// amINDZP is the 65C02 zero-page indirect mode, (zp): the two bytes at
// the zero-page pointer form the effective address, no indexing.
func (a *Adapter) amINDZP() uint16 {
	zp := a.read(a.pc)
	a.pc++
	lo := a.read(uint16(zp))
	hi := a.read(uint16(zp + 1))
	return uint16(lo) | uint16(hi)<<8
}

func (a *Adapter) opCMP(reg, v uint8) {
	a.setFlag(flagC, reg >= v)
	a.setZN(reg - v)
}

func (a *Adapter) opBIT(v uint8) {
	a.setFlag(flagZ, a.a&v == 0)
	a.setFlag(flagN, v&0x80 != 0)
	a.setFlag(flagV, v&0x40 != 0)
}

func (a *Adapter) opASL(v uint8) uint8 {
	a.setFlag(flagC, v&0x80 != 0)
	r := v << 1
	a.setZN(r)
	return r
}
func (a *Adapter) opLSR(v uint8) uint8 {
	a.setFlag(flagC, v&0x01 != 0)
	r := v >> 1
	a.setZN(r)
	return r
}
func (a *Adapter) opROL(v uint8) uint8 {
	cin := uint8(0)
	if a.p&flagC != 0 {
		cin = 1
	}
	a.setFlag(flagC, v&0x80 != 0)
	r := (v << 1) | cin
	a.setZN(r)
	return r
}
func (a *Adapter) opROR(v uint8) uint8 {
	cin := uint8(0)
	if a.p&flagC != 0 {
		cin = 0x80
	}
	a.setFlag(flagC, v&0x01 != 0)
	r := (v >> 1) | cin
	a.setZN(r)
	return r
}

// branch jumps if cond is true, advancing PC by the signed offset
// at PC. Cycle cost is 2; +1 if taken (we ignore for simplicity).
func (a *Adapter) branch(cond bool) {
	off := int8(a.read(a.pc))
	a.pc++
	if cond {
		a.pc = uint16(int(a.pc) + int(off))
	}
}

// ---------- dispatch ----------

func (a *Adapter) execute() {
	a.deltaCycles = 0
	op := a.read(a.pc)
	a.pc++

	// 65C02-only opcodes and the variant-specific behaviours are handled
	// first; anything the CMOS path doesn't claim falls through to the
	// shared NMOS dispatch below.
	if a.arch == cpu.CMOS && a.executeCMOS(op) {
		return
	}

	cycles := 2 // default; specific opcodes override

	switch op {
	// ADC
	case 0x69: a.opADC(a.read(a.amIM())); cycles = 2
	case 0x65: a.opADC(a.read(a.amZP())); cycles = 3
	case 0x75: a.opADC(a.read(a.amZPX())); cycles = 4
	case 0x6D: a.opADC(a.read(a.amABS())); cycles = 4
	case 0x7D: a.opADC(a.read(a.amABSX())); cycles = 4
	case 0x79: a.opADC(a.read(a.amABSY())); cycles = 4
	case 0x61: a.opADC(a.read(a.amINDX())); cycles = 6
	case 0x71: a.opADC(a.read(a.amINDY())); cycles = 5

	// AND
	case 0x29: a.opAND(a.read(a.amIM())); cycles = 2
	case 0x25: a.opAND(a.read(a.amZP())); cycles = 3
	case 0x35: a.opAND(a.read(a.amZPX())); cycles = 4
	case 0x2D: a.opAND(a.read(a.amABS())); cycles = 4
	case 0x3D: a.opAND(a.read(a.amABSX())); cycles = 4
	case 0x39: a.opAND(a.read(a.amABSY())); cycles = 4
	case 0x21: a.opAND(a.read(a.amINDX())); cycles = 6
	case 0x31: a.opAND(a.read(a.amINDY())); cycles = 5

	// ASL
	case 0x0A: a.a = a.opASL(a.a); cycles = 2
	case 0x06: addr := a.amZP();  a.write(addr, a.opASL(a.read(addr))); cycles = 5
	case 0x16: addr := a.amZPX(); a.write(addr, a.opASL(a.read(addr))); cycles = 6
	case 0x0E: addr := a.amABS(); a.write(addr, a.opASL(a.read(addr))); cycles = 6
	case 0x1E: addr := a.amABSX(); a.write(addr, a.opASL(a.read(addr))); cycles = 7

	// Branches (BCC, BCS, BEQ, BMI, BNE, BPL, BVC, BVS)
	case 0x90: a.branch(a.p&flagC == 0); cycles = 2
	case 0xB0: a.branch(a.p&flagC != 0); cycles = 2
	case 0xF0: a.branch(a.p&flagZ != 0); cycles = 2
	case 0x30: a.branch(a.p&flagN != 0); cycles = 2
	case 0xD0: a.branch(a.p&flagZ == 0); cycles = 2
	case 0x10: a.branch(a.p&flagN == 0); cycles = 2
	case 0x50: a.branch(a.p&flagV == 0); cycles = 2
	case 0x70: a.branch(a.p&flagV != 0); cycles = 2

	// BIT
	case 0x24: a.opBIT(a.read(a.amZP())); cycles = 3
	case 0x2C: a.opBIT(a.read(a.amABS())); cycles = 4

	// BRK — push PC+1 and P (with B set), jump via $FFFE/$FFFF.
	case 0x00:
		a.pc++
		a.push(uint8(a.pc >> 8))
		a.push(uint8(a.pc))
		a.push(a.p | flagB | flagU)
		a.setFlag(flagI, true)
		if a.arch == cpu.CMOS {
			a.setFlag(flagD, false) // 65C02 clears decimal mode on BRK
		}
		a.pc = a.read16(0xFFFE)
		a.brkCount++
		cycles = 7

	// Flag set/clear
	case 0x18: a.setFlag(flagC, false); cycles = 2 // CLC
	case 0xD8: a.setFlag(flagD, false); cycles = 2 // CLD
	case 0x58: a.setFlag(flagI, false); cycles = 2 // CLI
	case 0xB8: a.setFlag(flagV, false); cycles = 2 // CLV
	case 0x38: a.setFlag(flagC, true);  cycles = 2 // SEC
	case 0xF8: a.setFlag(flagD, true);  cycles = 2 // SED
	case 0x78: a.setFlag(flagI, true);  cycles = 2 // SEI

	// CMP
	case 0xC9: a.opCMP(a.a, a.read(a.amIM())); cycles = 2
	case 0xC5: a.opCMP(a.a, a.read(a.amZP())); cycles = 3
	case 0xD5: a.opCMP(a.a, a.read(a.amZPX())); cycles = 4
	case 0xCD: a.opCMP(a.a, a.read(a.amABS())); cycles = 4
	case 0xDD: a.opCMP(a.a, a.read(a.amABSX())); cycles = 4
	case 0xD9: a.opCMP(a.a, a.read(a.amABSY())); cycles = 4
	case 0xC1: a.opCMP(a.a, a.read(a.amINDX())); cycles = 6
	case 0xD1: a.opCMP(a.a, a.read(a.amINDY())); cycles = 5

	// CPX
	case 0xE0: a.opCMP(a.x, a.read(a.amIM())); cycles = 2
	case 0xE4: a.opCMP(a.x, a.read(a.amZP())); cycles = 3
	case 0xEC: a.opCMP(a.x, a.read(a.amABS())); cycles = 4

	// CPY
	case 0xC0: a.opCMP(a.y, a.read(a.amIM())); cycles = 2
	case 0xC4: a.opCMP(a.y, a.read(a.amZP())); cycles = 3
	case 0xCC: a.opCMP(a.y, a.read(a.amABS())); cycles = 4

	// DEC
	case 0xC6: addr := a.amZP();   v := a.read(addr) - 1; a.write(addr, v); a.setZN(v); cycles = 5
	case 0xD6: addr := a.amZPX();  v := a.read(addr) - 1; a.write(addr, v); a.setZN(v); cycles = 6
	case 0xCE: addr := a.amABS();  v := a.read(addr) - 1; a.write(addr, v); a.setZN(v); cycles = 6
	case 0xDE: addr := a.amABSX(); v := a.read(addr) - 1; a.write(addr, v); a.setZN(v); cycles = 7

	// DEX, DEY
	case 0xCA: a.x--; a.setZN(a.x); cycles = 2
	case 0x88: a.y--; a.setZN(a.y); cycles = 2

	// EOR
	case 0x49: a.opEOR(a.read(a.amIM())); cycles = 2
	case 0x45: a.opEOR(a.read(a.amZP())); cycles = 3
	case 0x55: a.opEOR(a.read(a.amZPX())); cycles = 4
	case 0x4D: a.opEOR(a.read(a.amABS())); cycles = 4
	case 0x5D: a.opEOR(a.read(a.amABSX())); cycles = 4
	case 0x59: a.opEOR(a.read(a.amABSY())); cycles = 4
	case 0x41: a.opEOR(a.read(a.amINDX())); cycles = 6
	case 0x51: a.opEOR(a.read(a.amINDY())); cycles = 5

	// INC
	case 0xE6: addr := a.amZP();   v := a.read(addr) + 1; a.write(addr, v); a.setZN(v); cycles = 5
	case 0xF6: addr := a.amZPX();  v := a.read(addr) + 1; a.write(addr, v); a.setZN(v); cycles = 6
	case 0xEE: addr := a.amABS();  v := a.read(addr) + 1; a.write(addr, v); a.setZN(v); cycles = 6
	case 0xFE: addr := a.amABSX(); v := a.read(addr) + 1; a.write(addr, v); a.setZN(v); cycles = 7

	// INX, INY
	case 0xE8: a.x++; a.setZN(a.x); cycles = 2
	case 0xC8: a.y++; a.setZN(a.y); cycles = 2

	// JMP
	case 0x4C: a.pc = a.amABS(); cycles = 3
	case 0x6C: a.pc = a.amIND(); cycles = 5

	// JSR — push return-1, jump to target.
	case 0x20:
		target := a.amABS()
		ret := a.pc - 1
		a.push(uint8(ret >> 8))
		a.push(uint8(ret))
		a.pc = target
		cycles = 6

	// LDA
	case 0xA9: a.opLDA(a.read(a.amIM())); cycles = 2
	case 0xA5: a.opLDA(a.read(a.amZP())); cycles = 3
	case 0xB5: a.opLDA(a.read(a.amZPX())); cycles = 4
	case 0xAD: a.opLDA(a.read(a.amABS())); cycles = 4
	case 0xBD: a.opLDA(a.read(a.amABSX())); cycles = 4
	case 0xB9: a.opLDA(a.read(a.amABSY())); cycles = 4
	case 0xA1: a.opLDA(a.read(a.amINDX())); cycles = 6
	case 0xB1: a.opLDA(a.read(a.amINDY())); cycles = 5

	// LDX
	case 0xA2: a.opLDX(a.read(a.amIM())); cycles = 2
	case 0xA6: a.opLDX(a.read(a.amZP())); cycles = 3
	case 0xB6: a.opLDX(a.read(a.amZPY())); cycles = 4
	case 0xAE: a.opLDX(a.read(a.amABS())); cycles = 4
	case 0xBE: a.opLDX(a.read(a.amABSY())); cycles = 4

	// LDY
	case 0xA0: a.opLDY(a.read(a.amIM())); cycles = 2
	case 0xA4: a.opLDY(a.read(a.amZP())); cycles = 3
	case 0xB4: a.opLDY(a.read(a.amZPX())); cycles = 4
	case 0xAC: a.opLDY(a.read(a.amABS())); cycles = 4
	case 0xBC: a.opLDY(a.read(a.amABSX())); cycles = 4

	// LSR
	case 0x4A: a.a = a.opLSR(a.a); cycles = 2
	case 0x46: addr := a.amZP();   a.write(addr, a.opLSR(a.read(addr))); cycles = 5
	case 0x56: addr := a.amZPX();  a.write(addr, a.opLSR(a.read(addr))); cycles = 6
	case 0x4E: addr := a.amABS();  a.write(addr, a.opLSR(a.read(addr))); cycles = 6
	case 0x5E: addr := a.amABSX(); a.write(addr, a.opLSR(a.read(addr))); cycles = 7

	// NOP
	case 0xEA: cycles = 2

	// ORA
	case 0x09: a.opORA(a.read(a.amIM())); cycles = 2
	case 0x05: a.opORA(a.read(a.amZP())); cycles = 3
	case 0x15: a.opORA(a.read(a.amZPX())); cycles = 4
	case 0x0D: a.opORA(a.read(a.amABS())); cycles = 4
	case 0x1D: a.opORA(a.read(a.amABSX())); cycles = 4
	case 0x19: a.opORA(a.read(a.amABSY())); cycles = 4
	case 0x01: a.opORA(a.read(a.amINDX())); cycles = 6
	case 0x11: a.opORA(a.read(a.amINDY())); cycles = 5

	// Stack ops
	case 0x48: a.push(a.a); cycles = 3                                  // PHA
	case 0x08: a.push(a.p | flagB | flagU); cycles = 3                  // PHP
	case 0x68: a.a = a.pull(); a.setZN(a.a); cycles = 4                 // PLA
	case 0x28: a.p = (a.pull() &^ flagB) | flagU; cycles = 4            // PLP

	// ROL
	case 0x2A: a.a = a.opROL(a.a); cycles = 2
	case 0x26: addr := a.amZP();   a.write(addr, a.opROL(a.read(addr))); cycles = 5
	case 0x36: addr := a.amZPX();  a.write(addr, a.opROL(a.read(addr))); cycles = 6
	case 0x2E: addr := a.amABS();  a.write(addr, a.opROL(a.read(addr))); cycles = 6
	case 0x3E: addr := a.amABSX(); a.write(addr, a.opROL(a.read(addr))); cycles = 7

	// ROR
	case 0x6A: a.a = a.opROR(a.a); cycles = 2
	case 0x66: addr := a.amZP();   a.write(addr, a.opROR(a.read(addr))); cycles = 5
	case 0x76: addr := a.amZPX();  a.write(addr, a.opROR(a.read(addr))); cycles = 6
	case 0x6E: addr := a.amABS();  a.write(addr, a.opROR(a.read(addr))); cycles = 6
	case 0x7E: addr := a.amABSX(); a.write(addr, a.opROR(a.read(addr))); cycles = 7

	// RTI
	case 0x40:
		a.p = (a.pull() &^ flagB) | flagU
		lo := a.pull()
		hi := a.pull()
		a.pc = uint16(lo) | uint16(hi)<<8
		cycles = 6

	// RTS
	case 0x60:
		lo := a.pull()
		hi := a.pull()
		a.pc = (uint16(lo) | uint16(hi)<<8) + 1
		cycles = 6

	// SBC
	case 0xE9: a.opSBC(a.read(a.amIM())); cycles = 2
	case 0xE5: a.opSBC(a.read(a.amZP())); cycles = 3
	case 0xF5: a.opSBC(a.read(a.amZPX())); cycles = 4
	case 0xED: a.opSBC(a.read(a.amABS())); cycles = 4
	case 0xFD: a.opSBC(a.read(a.amABSX())); cycles = 4
	case 0xF9: a.opSBC(a.read(a.amABSY())); cycles = 4
	case 0xE1: a.opSBC(a.read(a.amINDX())); cycles = 6
	case 0xF1: a.opSBC(a.read(a.amINDY())); cycles = 5

	// STA
	case 0x85: a.write(a.amZP(),   a.a); cycles = 3
	case 0x95: a.write(a.amZPX(),  a.a); cycles = 4
	case 0x8D: a.write(a.amABS(),  a.a); cycles = 4
	case 0x9D: a.write(a.amABSX(), a.a); cycles = 5
	case 0x99: a.write(a.amABSY(), a.a); cycles = 5
	case 0x81: a.write(a.amINDX(), a.a); cycles = 6
	case 0x91: a.write(a.amINDY(), a.a); cycles = 6

	// STX
	case 0x86: a.write(a.amZP(),  a.x); cycles = 3
	case 0x96: a.write(a.amZPY(), a.x); cycles = 4
	case 0x8E: a.write(a.amABS(), a.x); cycles = 4

	// STY
	case 0x84: a.write(a.amZP(),  a.y); cycles = 3
	case 0x94: a.write(a.amZPX(), a.y); cycles = 4
	case 0x8C: a.write(a.amABS(), a.y); cycles = 4

	// Transfers
	case 0xAA: a.x = a.a; a.setZN(a.x); cycles = 2  // TAX
	case 0xA8: a.y = a.a; a.setZN(a.y); cycles = 2  // TAY
	case 0xBA: a.x = a.sp; a.setZN(a.x); cycles = 2 // TSX
	case 0x8A: a.a = a.x; a.setZN(a.a); cycles = 2  // TXA
	case 0x9A: a.sp = a.x; cycles = 2               // TXS (no flag changes)
	case 0x98: a.a = a.y; a.setZN(a.a); cycles = 2  // TYA

	default:
		// Treat undocumented opcodes as NOP. Real 6502 opcodes have
		// quirky behaviours here; for the demo we just keep moving.
		cycles = 2
	}

	a.done(cycles)
}

// done parks the CPU for the rest of the instruction's cycles, folding
// in any per-instruction delta (e.g. the 65C02 decimal-mode penalty).
func (a *Adapter) done(cycles int) {
	a.halfCyclesLeft = 2*(cycles+a.deltaCycles) - 1
}

// executeCMOS dispatches the opcodes whose behaviour is unique to the
// 65C02 — either entirely new opcodes or NMOS opcodes with corrected
// behaviour (the JMP-indirect fix). It returns true when it handled the
// opcode; false means "not a CMOS-specific opcode, use shared dispatch".
func (a *Adapter) executeCMOS(op uint8) bool {
	switch op {
	// (zp) zero-page indirect addressing
	case 0x12:
		a.opORA(a.read(a.amINDZP()))
		a.done(5)
	case 0x32:
		a.opAND(a.read(a.amINDZP()))
		a.done(5)
	case 0x52:
		a.opEOR(a.read(a.amINDZP()))
		a.done(5)
	case 0x72:
		a.opADC(a.read(a.amINDZP()))
		a.done(5)
	case 0xB2:
		a.opLDA(a.read(a.amINDZP()))
		a.done(5)
	case 0xD2:
		a.opCMP(a.a, a.read(a.amINDZP()))
		a.done(5)
	case 0xF2:
		a.opSBC(a.read(a.amINDZP()))
		a.done(5)
	case 0x92:
		a.write(a.amINDZP(), a.a)
		a.done(5)

	// STZ — store zero
	case 0x64:
		a.write(a.amZP(), 0)
		a.done(3)
	case 0x74:
		a.write(a.amZPX(), 0)
		a.done(4)
	case 0x9C:
		a.write(a.amABS(), 0)
		a.done(4)
	case 0x9E:
		a.write(a.amABSX(), 0)
		a.done(5)

	// BIT — immediate sets only Z; the new indexed modes behave normally
	case 0x89:
		v := a.read(a.amIM())
		a.setFlag(flagZ, a.a&v == 0)
		a.done(2)
	case 0x34:
		a.opBIT(a.read(a.amZPX()))
		a.done(4)
	case 0x3C:
		a.opBIT(a.read(a.amABSX()))
		a.done(4)

	// BRA — branch always
	case 0x80:
		a.branch(true)
		a.done(3)

	// INC / DEC accumulator
	case 0x1A:
		a.a++
		a.setZN(a.a)
		a.done(2)
	case 0x3A:
		a.a--
		a.setZN(a.a)
		a.done(2)

	// JMP (abs,x)
	case 0x7C:
		ptr := a.read16(a.pc) + uint16(a.x)
		a.pc += 2
		a.pc = uint16(a.read(ptr)) | uint16(a.read(ptr+1))<<8
		a.done(6)

	// JMP (ind) — 65C02 fixes the NMOS page-boundary fetch bug
	case 0x6C:
		ptr := a.read16(a.pc)
		a.pc += 2
		a.pc = uint16(a.read(ptr)) | uint16(a.read(ptr+1))<<8
		a.done(5)

	// TRB / TSB
	case 0x14:
		a.trb(a.amZP())
		a.done(5)
	case 0x1C:
		a.trb(a.amABS())
		a.done(6)
	case 0x04:
		a.tsb(a.amZP())
		a.done(5)
	case 0x0C:
		a.tsb(a.amABS())
		a.done(6)

	// Stack push/pull X and Y
	case 0xDA:
		a.push(a.x)
		a.done(3)
	case 0x5A:
		a.push(a.y)
		a.done(3)
	case 0xFA:
		a.x = a.pull()
		a.setZN(a.x)
		a.done(4)
	case 0x7A:
		a.y = a.pull()
		a.setZN(a.y)
		a.done(4)

	default:
		return false
	}
	return true
}

// opLDA / opLDX / opLDY pulled out so the dispatch above stays uniform.
func (a *Adapter) opLDA(v uint8) { a.a = v; a.setZN(v) }
func (a *Adapter) opLDX(v uint8) { a.x = v; a.setZN(v) }
func (a *Adapter) opLDY(v uint8) { a.y = v; a.setZN(v) }
