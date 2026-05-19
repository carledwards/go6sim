package display

import (
	"testing"
	"time"
)

// Brick-3 net: the display Controller's recency clock must be virtual
// (driven only by Tick dt) and therefore deterministic — never derived
// from time.Now(). This test runs an identical command/frame sequence
// twice: once with no real delay, once sleeping real wall-clock between
// every step. If any wall-clock leaked back into the controller the two
// runs would diverge. It also pins the seen-flag and Reset semantics.
func TestControllerVirtualTimeDeterministic(t *testing.T) {
	const ms = time.Millisecond

	type result struct {
		cmd0     time.Duration // SinceLastCmd right after the RegCmd write
		cmd0ok   bool
		cmd5     time.Duration // after +5ms virtual
		frame0   time.Duration // SinceLastFrame right after RegFrame
		frame0ok bool
		frame3   time.Duration // after +3ms virtual
	}

	run := func(realDelay time.Duration) result {
		color := New("color", 0xA000, 40, 13)
		chars := New("char", 0xA400, 40, 13)
		c := NewController("ctrl", 0xA800, color, chars)

		if _, ok := c.SinceLastCmd(); ok {
			t.Fatal("cmd reported seen before any write")
		}
		if _, ok := c.SinceLastFrame(); ok {
			t.Fatal("frame reported seen before any write")
		}

		time.Sleep(realDelay)
		c.Tick(10 * ms)
		time.Sleep(realDelay)
		c.Write(RegCmd, 0) // benign no-op command
		cmd0, cmd0ok := c.SinceLastCmd()

		time.Sleep(realDelay)
		c.Tick(5 * ms)
		cmd5, _ := c.SinceLastCmd()

		time.Sleep(realDelay)
		c.Write(RegFrame, 1)
		frame0, frame0ok := c.SinceLastFrame()

		time.Sleep(realDelay)
		c.Tick(3 * ms)
		frame3, _ := c.SinceLastFrame()

		c.Reset()
		if _, ok := c.SinceLastCmd(); ok {
			t.Fatal("cmd reported seen after Reset")
		}
		if _, ok := c.SinceLastFrame(); ok {
			t.Fatal("frame reported seen after Reset")
		}

		return result{cmd0, cmd0ok, cmd5, frame0, frame0ok, frame3}
	}

	fast := run(0)
	slow := run(15 * ms) // real wall-clock between every step

	want := result{
		cmd0: 0, cmd0ok: true,
		cmd5:   5 * ms,
		frame0: 0, frame0ok: true,
		frame3: 3 * ms,
	}
	if fast != want {
		t.Fatalf("virtual-time values wrong:\n got %+v\nwant %+v", fast, want)
	}
	if fast != slow {
		t.Fatalf("controller leaked wall-clock — fast/slow runs diverged:\n"+
			"fast %+v\nslow %+v", fast, slow)
	}
}
