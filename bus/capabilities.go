package bus

// Optional capability interfaces a Component may additionally implement.
// The backplane / instrument layer discovers them by type-assertion
// (idiomatic Go: small required interface + optional capabilities, like
// io.Reader + io.ReaderAt). A Component that implements only the required
// bus.Component still attaches and works; capabilities just unlock richer
// behaviour (reset, snapshot, observation) without per-device special
// casing. See docs/architecture-backplane.md.
//
// Already declared in bus.go and part of this set:
//   - Ticker   — clocked advancement (currently wall-clock; the carve's
//                brick 3 converts this to cycle-based for determinism).
//   - Labeller — exposes the component's register layout as symbols.

// Resettable is a Component that can be returned to power-on state.
// Implemented today by RAM, the VIA, and the display Controller; the
// backplane calls Reset on every Resettable on a machine reset.
type Resettable interface {
	Reset()
}

// Snapshotter is a Component that can serialize and restore its full
// state as an opaque blob. This is the uniform shape the instrument
// layer wants (machine save/restore, lesson "reset to checkpoint").
// Components migrate their existing bespoke Snapshot types onto this in
// their own carve bricks — no component is asserted against it yet.
type Snapshotter interface {
	Snapshot() []byte
	Restore([]byte) error
}

// Tap is a single named, observable signal a Component exposes for the
// instrument/console (the "tap off the backplane" metaphor): an IRQ
// line, a timer counter, the VIA PA/PB ports, a CPU interrupt-ack
// strobe. Read returns the signal's current value; it must be cheap and
// side-effect-free.
type Tap struct {
	Name string
	Read func() uint64
}

// Tappable is a Component that publishes Taps. The instrument layer
// renders these uniformly (TUI insight panels, web views, the future
// debugger CLI) without knowing the concrete device. Nothing implements
// this yet — VIA (IRQ asserted / fired-count) and the CPU card
// (interrupt-ack / vectoring) gain it in brick 7.
type Tappable interface {
	Taps() []Tap
}

// IRQSource is a Component that can pull the shared, active-low IRQ
// line. IRQAsserted reports true when this card currently requests an
// interrupt. The backplane wired-ORs every IRQSource into one line the
// CPU honors (brick 9b) — modeled on a real open-collector IRQ bus:
// any card may assert it; the CPU sees the OR.
type IRQSource interface {
	IRQAsserted() bool
}
