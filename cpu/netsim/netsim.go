// Package netsim adapts the transistor-level CPU core from
// 6502-netsim-go to the simulator's Backend interface, routing all
// memory access through the supplied bus.
package netsim

import (
	netcpu "github.com/carledwards/6502-netsim-go/cpu"
	"github.com/carledwards/go6sim/bus"
	"github.com/carledwards/go6sim/cpu"
)

// Adapter is a cpu.Backend backed by the netsim transistor simulator.
type Adapter struct {
	cpu        *netcpu.CPU
	halfCycles uint64
	// irq is the bus's aggregated IRQ line (the backplane's wired-OR),
	// if the supplied bus exposes one. nil for a plain mapBus — then
	// the IRQ pin stays released, exactly as before.
	irq interface{ IRQ() bool }
}

// New wires the netsim CPU to the supplied bus. The CPU is created in
// an unreset state — call Reset before HalfStep.
func New(b bus.Bus) (*Adapter, error) {
	a := &Adapter{}
	c, err := netcpu.New(
		func(addr uint16) uint8 { return b.Read(addr) },
		func(addr uint16, val uint8) { b.Write(addr, val) },
	)
	if err != nil {
		return nil, err
	}
	a.cpu = c
	a.irq, _ = b.(interface{ IRQ() bool }) // *backplane.Backplane satisfies this
	return a, nil
}

func (a *Adapter) Reset() {
	a.cpu.Reset()
	a.halfCycles = 0
}

func (a *Adapter) HalfStep() {
	// Drive the transistor IRQ pin from the backplane's wired-OR each
	// half-step so a peripheral (e.g. the 6522 VIA) actually interrupts
	// the netsim core — parity with interp. brick 9b.
	if a.irq != nil {
		a.cpu.SetIRQ(a.irq.IRQ())
	}
	a.cpu.HalfStep()
	a.halfCycles++
}

func (a *Adapter) HalfCycles() uint64 { return a.halfCycles }

func (a *Adapter) Registers() cpu.Registers {
	r := a.cpu.Registers()
	return cpu.Registers{
		A:  r.A,
		X:  r.X,
		Y:  r.Y,
		S:  r.S,
		P:  r.P,
		PC: r.PC,
	}
}

func (a *Adapter) AddressBus() uint16 { return a.cpu.AddressBus() }
func (a *Adapter) DataBus() uint8     { return a.cpu.DataBus() }
func (a *Adapter) ReadCycle() bool    { return a.cpu.IsReadCycle() }

// IRQ reads the transistor IRQ node. It is now actively driven: every
// HalfStep pushes the backplane's wired-OR onto the pin via
// netcpu.SetIRQ, so a VIA (or any IRQSource card) interrupts the
// netsim core with full interp parity (brick 9b). NMI is still only a
// reader — no peripheral asserts NMI in v1 (no SetNMI driven).
func (a *Adapter) IRQ() bool  { return a.cpu.IRQ() }
func (a *Adapter) NMI() bool  { return a.cpu.NMI() }
func (a *Adapter) SYNC() bool { return a.cpu.SYNC() }

// Compile-time check that Adapter satisfies cpu.Backend.
var _ cpu.Backend = (*Adapter)(nil)

// NodeCount returns the allocated node count of the underlying
// transistor netlist. Visualisation clients (visualcpuwin) snapshot
// the full node-state vector per frame for polygon colouring.
// Interp doesn't model nodes, so this method exists only on
// Adapter; visualcpuwin uses an interface check to opt in.
func (a *Adapter) NodeCount() int { return a.cpu.NodeCount() }

// NodeStates fills out with the per-node logical state. out must
// have length >= NodeCount(). Cheap linear copy from the live node
// pointers — no recalc.
func (a *Adapter) NodeStates(out []bool) { a.cpu.NodeStates(out) }
