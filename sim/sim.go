// Package sim is a flat-memory 6502 test harness. It wraps go6sim's
// interpretive CPU core over a plain 64K address space and exposes the
// small surface an out-of-process program test needs: load an image,
// set PC/SP, single-step one instruction at a time, and peek/poke
// memory. Optional per-address read/write hooks let a test model a
// memory-mapped peripheral (e.g. a CIA keyboard matrix) without a full
// bus wiring.
//
// It is the go6sim-native replacement for the beevik/go6502
// cpu.CPU + cpu.FlatMemory pairing that merlin-c64 and emote-c64
// previously used to drive their assembled 6502 programs.
package sim

import (
	"github.com/carledwards/go6sim/cpu"
	"github.com/carledwards/go6sim/cpu/interp"
)

// Arch selects the 6502 variant. Re-exported from package cpu so callers
// need only import sim.
type Arch = cpu.Arch

const (
	// NMOS is the 6502/6510 (Commodore 64). Use this for C64 programs.
	NMOS = cpu.NMOS
	// CMOS is the 65C02.
	CMOS = cpu.CMOS
)

// Machine is a flat-memory 6502 plus its CPU.
type Machine struct {
	mem *flatMem
	cpu *interp.Adapter
}

// New builds a flat-memory machine running the given 6502 variant. The
// CPU registers start zeroed (no reset is performed), matching a bare
// power-on harness: callers set PC (and SP if they don't run setup code
// that does) before stepping.
func New(arch Arch) *Machine {
	m := &flatMem{}
	return &Machine{mem: m, cpu: interp.NewArch(m, arch)}
}

// --- memory access (names mirror beevik's FlatMemory) ---

// LoadByte returns the byte at addr (honouring any read hook).
func (m *Machine) LoadByte(addr uint16) byte { return m.mem.Read(addr) }

// StoreByte writes v at addr (honouring any write hook).
func (m *Machine) StoreByte(addr uint16, v byte) { m.mem.Write(addr, v) }

// StoreBytes writes b starting at addr, straight into backing memory
// (write hooks are bypassed — this is image loading, not a CPU store).
func (m *Machine) StoreBytes(addr uint16, b []byte) {
	for i, v := range b {
		m.mem.cells[addr+uint16(i)] = v
	}
}

// Peek reads backing memory directly, bypassing any read hook. Handy for
// inspecting a cell that a hook would otherwise synthesize.
func (m *Machine) Peek(addr uint16) byte { return m.mem.cells[addr] }

// --- CPU control ---

// SetPC points the program counter at addr; the next Step fetches there.
func (m *Machine) SetPC(addr uint16) { m.cpu.SetPC(addr) }

// SetSP sets the stack pointer.
func (m *Machine) SetSP(sp byte) { m.cpu.SetSP(sp) }

// SetA, SetX, SetY, SetP inject the remaining register state directly,
// for tests that need a precise starting point.
func (m *Machine) SetA(v byte) { m.cpu.SetA(v) }
func (m *Machine) SetX(v byte) { m.cpu.SetX(v) }
func (m *Machine) SetY(v byte) { m.cpu.SetY(v) }
func (m *Machine) SetP(v byte) { m.cpu.SetP(v) }

// PC returns the current program counter.
func (m *Machine) PC() uint16 { return m.cpu.Registers().PC }

// SP returns the current stack pointer.
func (m *Machine) SP() byte { return m.cpu.Registers().S }

// A, X, Y return the index/accumulator registers.
func (m *Machine) A() byte { return m.cpu.Registers().A }
func (m *Machine) X() byte { return m.cpu.Registers().X }
func (m *Machine) Y() byte { return m.cpu.Registers().Y }

// P returns the processor status register.
func (m *Machine) P() byte { return m.cpu.Registers().P }

// Cycles returns the number of CPU clock cycles executed so far.
func (m *Machine) Cycles() uint64 { return m.cpu.HalfCycles() / 2 }

// Step executes exactly one instruction, leaving PC at the next opcode —
// the same contract as beevik's CPU.Step. It advances the half-cycle
// core until the SYNC boundary where an instruction's logic runs.
func (m *Machine) Step() {
	for {
		m.cpu.HalfStep()
		if m.cpu.SYNC() {
			return
		}
	}
}

// --- peripheral hooks ---

// OnRead installs a handler invoked whenever the CPU reads addr; its
// return value is delivered instead of backing memory. Passing nil
// removes the hook.
func (m *Machine) OnRead(addr uint16, fn func() byte) { m.mem.onRead(addr, fn) }

// OnWrite installs a handler invoked whenever the CPU writes addr;
// backing memory is not touched. Passing nil removes the hook.
func (m *Machine) OnWrite(addr uint16, fn func(v byte)) { m.mem.onWrite(addr, fn) }
