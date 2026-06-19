package sim_test

import (
	"testing"

	"github.com/carledwards/go6sim/sim"
)

// Status-flag bits, for readable assertions.
const (
	fC = 0x01
	fZ = 0x02
	fI = 0x04
	fD = 0x08
	fV = 0x40
	fN = 0x80
)

// run1 loads prog at $0200, applies setup, then single-steps one
// instruction and returns the machine.
func run1(t *testing.T, arch sim.Arch, setup func(*sim.Machine), prog ...byte) *sim.Machine {
	t.Helper()
	m := sim.New(arch)
	m.StoreBytes(0x0200, prog)
	if setup != nil {
		setup(m)
	}
	m.SetPC(0x0200)
	m.Step()
	return m
}

// --- decimal mode (the C64-relevant NMOS path, plus CMOS) -----------------

// Result and carry of valid-BCD ADC/SBC are identical on NMOS and CMOS;
// these vectors pin that arithmetic without depending on the
// arch-specific N/V/Z quirks for invalid BCD.
func TestDecimalADC(t *testing.T) {
	cases := []struct {
		a, v  byte
		cin   bool
		wantA byte
		wantC bool
	}{
		{0x00, 0x00, false, 0x00, false},
		{0x09, 0x01, false, 0x10, false},
		{0x09, 0x09, false, 0x18, false},
		{0x09, 0x09, true, 0x19, false},
		{0x50, 0x50, false, 0x00, true},
		{0x99, 0x01, false, 0x00, true},
		{0x99, 0x99, true, 0x99, true},
		{0x12, 0x34, false, 0x46, false},
		{0x58, 0x46, true, 0x05, true},
	}
	for _, arch := range []sim.Arch{sim.NMOS, sim.CMOS} {
		for _, c := range cases {
			m := run1(t, arch, func(m *sim.Machine) {
				m.SetA(c.a)
				p := byte(fD)
				if c.cin {
					p |= fC
				}
				m.SetP(p)
			}, 0x69, c.v) // ADC #v
			if got := m.A(); got != c.wantA {
				t.Errorf("%s ADC %02X+%02X cin=%v: A=$%02X want $%02X", arch, c.a, c.v, c.cin, got, c.wantA)
			}
			if got := m.P()&fC != 0; got != c.wantC {
				t.Errorf("%s ADC %02X+%02X cin=%v: C=%v want %v", arch, c.a, c.v, c.cin, got, c.wantC)
			}
		}
	}
}

func TestDecimalSBC(t *testing.T) {
	cases := []struct {
		a, v  byte
		cin   bool
		wantA byte
		wantC bool
	}{
		{0x00, 0x00, true, 0x00, true},
		{0x10, 0x01, true, 0x09, true},
		{0x00, 0x01, true, 0x99, false},
		{0x50, 0x25, true, 0x25, true},
		{0x99, 0x99, true, 0x00, true},
		{0x46, 0x12, true, 0x34, true},
		{0x32, 0x02, false, 0x29, true},
		{0x12, 0x34, true, 0x78, false},
	}
	for _, arch := range []sim.Arch{sim.NMOS, sim.CMOS} {
		for _, c := range cases {
			m := run1(t, arch, func(m *sim.Machine) {
				m.SetA(c.a)
				p := byte(fD)
				if c.cin {
					p |= fC
				}
				m.SetP(p)
			}, 0xE9, c.v) // SBC #v
			if got := m.A(); got != c.wantA {
				t.Errorf("%s SBC %02X-%02X cin=%v: A=$%02X want $%02X", arch, c.a, c.v, c.cin, got, c.wantA)
			}
			if got := m.P()&fC != 0; got != c.wantC {
				t.Errorf("%s SBC %02X-%02X cin=%v: C=%v want %v", arch, c.a, c.v, c.cin, got, c.wantC)
			}
		}
	}
}

// NMOS must IGNORE decimal mode is wrong — NMOS honours it. This guards
// that the D flag actually changes behaviour on the NMOS (C64) core.
func TestNMOSHonoursDecimal(t *testing.T) {
	bin := run1(t, sim.NMOS, func(m *sim.Machine) { m.SetA(0x09) }, 0x69, 0x01)             // binary: 0x0A
	dec := run1(t, sim.NMOS, func(m *sim.Machine) { m.SetA(0x09); m.SetP(fD) }, 0x69, 0x01) // decimal: 0x10
	if bin.A() != 0x0A {
		t.Errorf("binary ADC: A=$%02X want $0A", bin.A())
	}
	if dec.A() != 0x10 {
		t.Errorf("decimal ADC: A=$%02X want $10", dec.A())
	}
}

// --- 65C02 opcodes beevik cannot execute (covered here instead) -----------

func TestCMOSIndirectZP(t *testing.T) {
	// Pointer at $40 -> $1234; value $1234 = $7E.
	setPtr := func(m *sim.Machine) {
		m.StoreByte(0x40, 0x34)
		m.StoreByte(0x41, 0x12)
		m.StoreByte(0x1234, 0x7E)
	}

	// LDA ($40)
	if m := run1(t, sim.CMOS, setPtr, 0xB2, 0x40); m.A() != 0x7E {
		t.Errorf("LDA (zp): A=$%02X want $7E", m.A())
	}
	// ORA ($40) with A=$01 -> $7F
	if m := run1(t, sim.CMOS, func(m *sim.Machine) { setPtr(m); m.SetA(0x01) }, 0x12, 0x40); m.A() != 0x7F {
		t.Errorf("ORA (zp): A=$%02X want $7F", m.A())
	}
	// AND ($40) with A=$0F -> $0E
	if m := run1(t, sim.CMOS, func(m *sim.Machine) { setPtr(m); m.SetA(0x0F) }, 0x32, 0x40); m.A() != 0x0E {
		t.Errorf("AND (zp): A=$%02X want $0E", m.A())
	}
	// EOR ($40) with A=$FF -> $81
	if m := run1(t, sim.CMOS, func(m *sim.Machine) { setPtr(m); m.SetA(0xFF) }, 0x52, 0x40); m.A() != 0x81 {
		t.Errorf("EOR (zp): A=$%02X want $81", m.A())
	}
	// ADC ($40) binary with A=$01 -> $7F
	if m := run1(t, sim.CMOS, func(m *sim.Machine) { setPtr(m); m.SetA(0x01) }, 0x72, 0x40); m.A() != 0x7F {
		t.Errorf("ADC (zp): A=$%02X want $7F", m.A())
	}
	// SBC ($40) with A=$80, carry set -> $02
	if m := run1(t, sim.CMOS, func(m *sim.Machine) { setPtr(m); m.SetA(0x80); m.SetP(fC) }, 0xF2, 0x40); m.A() != 0x02 {
		t.Errorf("SBC (zp): A=$%02X want $02", m.A())
	}
	// CMP ($40) with A=$7E -> Z set, C set
	if m := run1(t, sim.CMOS, func(m *sim.Machine) { setPtr(m); m.SetA(0x7E) }, 0xD2, 0x40); m.P()&fZ == 0 || m.P()&fC == 0 {
		t.Errorf("CMP (zp): P=$%02X want Z and C set", m.P())
	}
	// STA ($40) with A=$5A -> $1234 = $5A
	if m := run1(t, sim.CMOS, func(m *sim.Machine) { setPtr(m); m.SetA(0x5A) }, 0x92, 0x40); m.Peek(0x1234) != 0x5A {
		t.Errorf("STA (zp): $1234=$%02X want $5A", m.Peek(0x1234))
	}
}

func TestCMOSStoreZero(t *testing.T) {
	// STZ zp / zp,x / abs / abs,x
	if m := run1(t, sim.CMOS, func(m *sim.Machine) { m.StoreByte(0x30, 0xFF) }, 0x64, 0x30); m.Peek(0x30) != 0 {
		t.Errorf("STZ zp: $30=$%02X want 0", m.Peek(0x30))
	}
	if m := run1(t, sim.CMOS, func(m *sim.Machine) { m.SetX(0x05); m.StoreByte(0x35, 0xFF) }, 0x74, 0x30); m.Peek(0x35) != 0 {
		t.Errorf("STZ zp,x: $35=$%02X want 0", m.Peek(0x35))
	}
	if m := run1(t, sim.CMOS, func(m *sim.Machine) { m.StoreByte(0x1000, 0xFF) }, 0x9C, 0x00, 0x10); m.Peek(0x1000) != 0 {
		t.Errorf("STZ abs: $1000=$%02X want 0", m.Peek(0x1000))
	}
	if m := run1(t, sim.CMOS, func(m *sim.Machine) { m.SetX(0x10); m.StoreByte(0x1010, 0xFF) }, 0x9E, 0x00, 0x10); m.Peek(0x1010) != 0 {
		t.Errorf("STZ abs,x: $1010=$%02X want 0", m.Peek(0x1010))
	}
}

func TestCMOSBranchAlways(t *testing.T) {
	// BRA +4 from $0200 -> PC = $0202 + 4 = $0206
	m := run1(t, sim.CMOS, nil, 0x80, 0x04)
	if m.PC() != 0x0206 {
		t.Errorf("BRA: PC=$%04X want $0206", m.PC())
	}
	// backward branch: BRA -2 -> infinite-loop target $0200
	m = run1(t, sim.CMOS, nil, 0x80, 0xFE)
	if m.PC() != 0x0200 {
		t.Errorf("BRA back: PC=$%04X want $0200", m.PC())
	}
}

func TestCMOSIncDecA(t *testing.T) {
	if m := run1(t, sim.CMOS, func(m *sim.Machine) { m.SetA(0x7F) }, 0x1A); m.A() != 0x80 || m.P()&fN == 0 {
		t.Errorf("INC A: A=$%02X P=$%02X want A=$80, N set", m.A(), m.P())
	}
	if m := run1(t, sim.CMOS, func(m *sim.Machine) { m.SetA(0x01) }, 0x3A); m.A() != 0x00 || m.P()&fZ == 0 {
		t.Errorf("DEC A: A=$%02X P=$%02X want A=$00, Z set", m.A(), m.P())
	}
}

func TestCMOSJmpAbsX(t *testing.T) {
	// JMP ($0210,X), X=4 -> pointer at $0214 = $3456
	m := run1(t, sim.CMOS, func(m *sim.Machine) {
		m.SetX(0x04)
		m.StoreByte(0x0214, 0x56)
		m.StoreByte(0x0215, 0x34)
	}, 0x7C, 0x10, 0x02)
	if m.PC() != 0x3456 {
		t.Errorf("JMP (abs,x): PC=$%04X want $3456", m.PC())
	}
}

func TestCMOSBitImmediateOnlyZ(t *testing.T) {
	// BIT #imm modifies only Z on the 65C02 (N and V untouched), unlike
	// every other BIT form. Start with N and V clear; operand has bits 7
	// and 6 set; they must stay clear, and Z reflects A & imm.
	m := run1(t, sim.CMOS, func(m *sim.Machine) { m.SetA(0x0F); m.SetP(0) }, 0x89, 0xC0)
	if m.P()&fN != 0 || m.P()&fV != 0 {
		t.Errorf("BIT #imm touched N/V: P=$%02X", m.P())
	}
	if m.P()&fZ == 0 { // 0x0F & 0xC0 == 0 -> Z set
		t.Errorf("BIT #imm: Z not set for zero result, P=$%02X", m.P())
	}
	// nonzero result -> Z clear
	m = run1(t, sim.CMOS, func(m *sim.Machine) { m.SetA(0x0F); m.SetP(0) }, 0x89, 0x01)
	if m.P()&fZ != 0 {
		t.Errorf("BIT #imm: Z set for nonzero result, P=$%02X", m.P())
	}
}

// --- other 65C02 opcodes (these DO match beevik, but pin them here too) ---

func TestCMOSTrbTsb(t *testing.T) {
	// TSB zp: set bits of A into memory; Z from (A & mem_before).
	m := run1(t, sim.CMOS, func(m *sim.Machine) { m.SetA(0x0F); m.StoreByte(0x20, 0xF0) }, 0x04, 0x20)
	if m.Peek(0x20) != 0xFF {
		t.Errorf("TSB zp: $20=$%02X want $FF", m.Peek(0x20))
	}
	if m.P()&fZ == 0 { // 0x0F & 0xF0 == 0 -> Z set
		t.Errorf("TSB zp: Z not set, P=$%02X", m.P())
	}
	// TRB zp: clear bits of A from memory.
	m = run1(t, sim.CMOS, func(m *sim.Machine) { m.SetA(0x0F); m.StoreByte(0x20, 0xFF) }, 0x14, 0x20)
	if m.Peek(0x20) != 0xF0 {
		t.Errorf("TRB zp: $20=$%02X want $F0", m.Peek(0x20))
	}
	if m.P()&fZ != 0 { // 0x0F & 0xFF != 0 -> Z clear
		t.Errorf("TRB zp: Z set, P=$%02X", m.P())
	}
}

func TestCMOSStackXY(t *testing.T) {
	// PHX then check stack; PLY then check Y.
	m := run1(t, sim.CMOS, func(m *sim.Machine) { m.SetX(0xAB); m.SetSP(0xFF) }, 0xDA) // PHX
	if m.Peek(0x01FF) != 0xAB {
		t.Errorf("PHX: stack=$%02X want $AB", m.Peek(0x01FF))
	}
	if m.SP() != 0xFE {
		t.Errorf("PHX: SP=$%02X want $FE", m.SP())
	}
	m = run1(t, sim.CMOS, func(m *sim.Machine) { m.SetSP(0xFE); m.StoreByte(0x01FF, 0xCD) }, 0x7A) // PLY
	if m.Y() != 0xCD {
		t.Errorf("PLY: Y=$%02X want $CD", m.Y())
	}
}

// --- variant-specific behaviour -------------------------------------------

func TestJmpIndirectPageBug(t *testing.T) {
	// Pointer at $12FF. Low byte at $12FF, high byte: NMOS reads $1200
	// (the page-wrap bug); CMOS reads $1300 (fixed).
	setup := func(m *sim.Machine) {
		m.StoreByte(0x12FF, 0x34) // low
		m.StoreByte(0x1300, 0x56) // correct high
		m.StoreByte(0x1200, 0x99) // wrapped high (NMOS reads this)
	}
	nmos := run1(t, sim.NMOS, setup, 0x6C, 0xFF, 0x12)
	if nmos.PC() != 0x9934 {
		t.Errorf("NMOS JMP (ind) bug: PC=$%04X want $9934", nmos.PC())
	}
	cmos := run1(t, sim.CMOS, setup, 0x6C, 0xFF, 0x12)
	if cmos.PC() != 0x5634 {
		t.Errorf("CMOS JMP (ind) fixed: PC=$%04X want $5634", cmos.PC())
	}
}

func TestInterruptDecimalClear(t *testing.T) {
	// BRK with D set: NMOS keeps D, CMOS clears it. Vector at $FFFE.
	setup := func(m *sim.Machine) {
		m.SetP(fD)
		m.SetSP(0xFF)
		m.StoreByte(0xFFFE, 0x00)
		m.StoreByte(0xFFFF, 0x30)
	}
	nmos := run1(t, sim.NMOS, setup, 0x00) // BRK
	if nmos.P()&fD == 0 {
		t.Errorf("NMOS BRK cleared D, P=$%02X", nmos.P())
	}
	cmos := run1(t, sim.CMOS, setup, 0x00)
	if cmos.P()&fD != 0 {
		t.Errorf("CMOS BRK did not clear D, P=$%02X", cmos.P())
	}
	if cmos.PC() != 0x3000 {
		t.Errorf("BRK vector: PC=$%04X want $3000", cmos.PC())
	}
}
