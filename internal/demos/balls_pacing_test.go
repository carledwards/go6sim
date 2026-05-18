package demos

import (
	"testing"
	"time"

	"github.com/carledwards/go6sim/bus"
	"github.com/carledwards/go6sim/components/display"
	"github.com/carledwards/go6sim/components/ram"
	"github.com/carledwards/go6sim/components/rom"
	"github.com/carledwards/go6sim/components/via"
	"github.com/carledwards/go6sim/cpu/interp"
)

// rig builds the minimum machine wiring needed by the pacing tests:
// RAM, VIC ctrl + planes (with optional graphics), VIA at the
// production base ($B000), and ROM. Returns the trace bus, the VIA
// for direct snapshot reads, and a freshly-reset CPU bound to the
// trace bus. ROM is loaded with d's bytes and the reset vector is
// pointed at $E000.
func rig(t *testing.T, d Demo, withGraphics bool) (*bus.TraceBus, *via.VIA, *interp.Adapter) {
	t.Helper()
	innerBus := bus.New()
	b := bus.NewTraceBus(innerBus)

	mainRAM := ram.New("ram", 0x0000, 0x2000)
	colorPlane := display.New("color", 0xA000, 40, 13)
	charPlane := display.New("char", 0xA400, 40, 13)
	via1 := via.New("via1", 0xB000, 1_000_000)
	mainROM := rom.New("rom", 0xE000, 0x2000)

	comps := []bus.Component{mainRAM, colorPlane, charPlane}
	if withGraphics {
		gfxPlane := display.NewGraphicsPlane(display.GraphicsConfig{
			Name: "gfx", Base: 0xC000, Width: 160, Height: 100, BPP: 4,
		})
		dispCtrl := display.NewControllerWithGraphics("ctrl", 0xA800, colorPlane, charPlane, gfxPlane)
		comps = append(comps, dispCtrl, gfxPlane)
	} else {
		dispCtrl := display.NewController("ctrl", 0xA800, colorPlane, charPlane)
		comps = append(comps, dispCtrl)
	}
	comps = append(comps, via1, mainROM)
	for _, c := range comps {
		if err := b.Register(c); err != nil {
			t.Fatalf("register %s: %v", c.Name(), err)
		}
	}
	if err := mainROM.Load(0, d.Bytes); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := mainROM.SetResetVector(0xE000); err != nil {
		t.Fatalf("vector: %v", err)
	}
	cpu := interp.New(b)
	cpu.Reset()
	return b, via1, cpu
}

// run advances the CPU and bus in the same sub-tick pattern the
// production run loops use: split each app.Tick into N slices,
// interleaving HalfSteps with b.Tick. Without sub-ticking, polling
// demos can spend an entire batch in a wait loop never observing
// the timer underflow that's about to come.
func run(b *bus.TraceBus, cpu *interp.Adapter, ticks int) {
	const halfStepsPerSubTick = 500
	const subTicks = 10
	subPeriod := 5 * time.Millisecond
	for tick := 0; tick < ticks; tick++ {
		for s := 0; s < subTicks; s++ {
			for i := 0; i < halfStepsPerSubTick; i++ {
				cpu.HalfStep()
			}
			b.Tick(subPeriod)
		}
	}
}

// TestMarqueeArmsVIA — the boot demo for the TUI must program VIA
// T1 in free-run mode so the VIA window has live state to render
// and the marquee paces independently of the CPU clock.
func TestMarqueeArmsVIA(t *testing.T) {
	b, via1, cpu := rig(t, MarqueeDemo, false)
	run(b, cpu, 10)

	s := via1.Snapshot()
	if !s.T1Armed {
		t.Fatal("T1 not armed after Marquee setup — VIA window will read DISARMED")
	}
	if !s.T1FreeRun {
		t.Fatalf("T1 not in free-run mode (ACR=%02X)", s.ACR)
	}
	if s.T1Latch == 0 {
		t.Fatalf("T1 latch not programmed (=%d)", s.T1Latch)
	}
}

// TestBouncingBallsEscapesWaitTick — the BouncingBalls demo must
// escape its VIA-T1 polling loop and update at least one ball
// position. Sanity check that VIA pacing works end-to-end through
// the CPU and bus.
func TestBouncingBallsEscapesWaitTick(t *testing.T) {
	b, _, cpu := rig(t, BouncingBallsDemo, true)
	run(b, cpu, 10)

	if got := b.Read(0x10); got == 20 {
		t.Fatal("BALL_X[0] never advanced — CPU stuck in WAIT_TICK")
	}
}
