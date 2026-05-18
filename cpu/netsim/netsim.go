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
	return a, nil
}

func (a *Adapter) Reset() {
	a.cpu.Reset()
	a.halfCycles = 0
}

func (a *Adapter) HalfStep() {
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
func (a *Adapter) IRQ() bool          { return a.cpu.IRQ() }
func (a *Adapter) NMI() bool          { return a.cpu.NMI() }
func (a *Adapter) SYNC() bool         { return a.cpu.SYNC() }

// Compile-time check that Adapter satisfies cpu.Backend.
var _ cpu.Backend = (*Adapter)(nil)
