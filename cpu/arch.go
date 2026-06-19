package cpu

// Arch selects which 6502 variant a backend emulates. The two differ in
// decimal-mode flag handling, a handful of opcodes, and the JMP-indirect
// page-boundary bug.
type Arch uint8

const (
	// NMOS is the original MOS 6502 (and the 6510 in the Commodore 64):
	// decimal mode survives interrupts, the JMP ($nnFF) indirect bug is
	// present, and the 65C02-only opcodes are undocumented.
	NMOS Arch = iota

	// CMOS is the WDC/Rockwell 65C02: decimal mode is cleared on
	// interrupt entry, the JMP-indirect bug is fixed, and the extra
	// opcodes (BRA, PHX/PHY/PLX/PLY, STZ, TRB/TSB, INC/DEC A, the
	// (zp) addressing mode, BIT immediate, JMP (abs,x)) are available.
	CMOS
)

func (a Arch) String() string {
	switch a {
	case CMOS:
		return "CMOS"
	default:
		return "NMOS"
	}
}
