package cpu

// Memory is the minimal address space a CPU backend needs: byte reads
// and writes over a 16-bit address. It is deliberately tiny so the core
// can run against anything from a flat array to the full memory-mapped
// bus — and so an embedded/TinyGo build of the CPU pulls in no I/O,
// reflection, or assembler dependencies. bus.Bus satisfies it.
type Memory interface {
	Read(addr uint16) uint8
	Write(addr uint16, val uint8)
}

// Tap is a single named, observable signal a component or CPU exposes
// for the instrument/console (the "tap off the backplane" metaphor): an
// IRQ line, a timer counter, a port value, a vector-taken count. Read
// returns the signal's current value; it must be cheap and
// side-effect-free.
//
// It lives in package cpu (not bus) so the CPU core can publish Taps
// without importing the bus package — keeping the core dependency-free.
type Tap struct {
	Name string
	Read func() uint64
}

// Tappable is anything that publishes Taps. The instrument layer
// discovers it by type-assertion and renders the taps uniformly without
// knowing the concrete device.
type Tappable interface {
	Taps() []Tap
}
