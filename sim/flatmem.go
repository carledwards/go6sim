package sim

import "github.com/carledwards/go6sim/cpu"

// flatMem is a 64K flat address space that satisfies cpu.Memory. Reads
// and writes hit a backing byte array unless an address has a registered
// hook, in which case the hook handles the access. Hooks make it cheap
// for a test to model a memory-mapped register without standing up a
// full component on the real bus.
type flatMem struct {
	cells  [0x10000]byte
	reads  map[uint16]func() byte
	writes map[uint16]func(v byte)
}

func (m *flatMem) Read(addr uint16) uint8 {
	if m.reads != nil {
		if h, ok := m.reads[addr]; ok {
			return h()
		}
	}
	return m.cells[addr]
}

func (m *flatMem) Write(addr uint16, v uint8) {
	if m.writes != nil {
		if h, ok := m.writes[addr]; ok {
			h(v)
			return
		}
	}
	m.cells[addr] = v
}

func (m *flatMem) onRead(addr uint16, fn func() byte) {
	if fn == nil {
		delete(m.reads, addr)
		return
	}
	if m.reads == nil {
		m.reads = make(map[uint16]func() byte)
	}
	m.reads[addr] = fn
}

func (m *flatMem) onWrite(addr uint16, fn func(v byte)) {
	if fn == nil {
		delete(m.writes, addr)
		return
	}
	if m.writes == nil {
		m.writes = make(map[uint16]func(v byte))
	}
	m.writes[addr] = fn
}

var _ cpu.Memory = (*flatMem)(nil)
