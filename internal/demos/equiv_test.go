package demos

// Byte-equivalence oracle for the go6asm migration: every .s demo,
// assembled by go6asm, must produce code byte-identical to the legacy
// fluent Builder it replaces. While both paths coexist this test is
// the gate; once green the registrations flip to .s and the fluent
// builders are deleted.

import (
	"bytes"
	"testing"

	"github.com/carledwards/go6sim/asm"
)

func TestDemoByteEquivalence(t *testing.T) {
	cases := []struct {
		name   string
		src    []byte
		legacy asm.Program
	}{
		{"marquee.s", marqueeSrc, buildMarquee()},
		{"bouncer.s", bouncerSrc, buildBouncer()},
		{"scroller.s", scrollerSrc, buildScroller()},
		{"snow.s", snowSrc, buildSnow()},
		{"blitter.s", blitterSrc, buildBlitter()},
		{"scroller_framed.s", scrollerFramedSrc, buildScrollerFramed()},
		{"quad.s", quadSrc, buildQuad()},
		{"balls.s", ballsSrc, buildBouncingBalls()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := asm.FromSource(c.name, c.src)
			if err != nil {
				t.Fatalf("assemble: %v", err)
			}
			if len(p.Bytes) != 0x2000 {
				t.Fatalf("image %d bytes, want 8192 ($E000-$FFFF ROM)", len(p.Bytes))
			}
			n := len(c.legacy.Bytes)
			if n > len(p.Bytes) {
				t.Fatalf("legacy %d bytes > image %d", n, len(p.Bytes))
			}
			if !bytes.Equal(p.Bytes[:n], c.legacy.Bytes) {
				for i := 0; i < n; i++ {
					if p.Bytes[i] != c.legacy.Bytes[i] {
						lo, hi := max(0, i-4), min(n, i+8)
						t.Fatalf("first diff at +%d ($%04X): go6=$%02X legacy=$%02X\n  go6   =% X\n  legacy=% X",
							i, 0xE000+i, p.Bytes[i], c.legacy.Bytes[i],
							p.Bytes[lo:hi], c.legacy.Bytes[lo:hi])
					}
				}
			}
			// go6asm synthesizes the reset vector at $FFFC → $E000.
			if p.Bytes[0x1FFC] != 0x00 || p.Bytes[0x1FFD] != 0xE0 {
				t.Errorf("RESET vector = $%02X%02X, want $E000",
					p.Bytes[0x1FFD], p.Bytes[0x1FFC])
			}
		})
	}
}

// Exported constants/labels in each .s reach the Program symbol
// table so the memory view can name cells (4b). Bytes are unaffected
// (TestDemoByteEquivalence proves that) — this checks the names.
func TestExportedDemoSymbols(t *testing.T) {
	// Fixed-address ZP/RAM constants: exact address asserted.
	consts := map[string]map[string]uint16{
		"bouncer.s":         {"X": 0x10, "DX": 0x11},
		"scroller.s":        {"COUNTER": 0x10},
		"scroller_framed.s": {"COUNTER": 0x10},
		"snow.s":            {"LFSR": 0x10},
		"blitter.s":         {"COUNTER": 0x10, "SCRATCH": 0x1000},
		"balls.s": {"BALL_X": 0x10, "BALL_Y": 0x14, "BALL_VX": 0x18,
			"BALL_VY": 0x1C, "BALL_COL": 0x20},
	}
	// Code/data labels: present and in the $E000-$FFFF ROM window.
	labels := map[string][]string{
		"marquee.s": {"MSG"},
		"quad.s":    {"DRAW"},
	}
	src := map[string][]byte{
		"marquee.s": marqueeSrc, "bouncer.s": bouncerSrc,
		"scroller.s": scrollerSrc, "scroller_framed.s": scrollerFramedSrc,
		"snow.s": snowSrc, "blitter.s": blitterSrc,
		"quad.s": quadSrc, "balls.s": ballsSrc,
	}

	for name, want := range consts {
		p, err := asm.FromSource(name, src[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := map[string]uint16{}
		for _, s := range p.Symbols {
			got[s.Name] = s.Addr
		}
		for n, a := range want {
			if got[n] != a {
				t.Errorf("%s: %s = $%04X, want $%04X", name, n, got[n], a)
			}
		}
	}
	for name, names := range labels {
		p, err := asm.FromSource(name, src[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := map[string]uint16{}
		for _, s := range p.Symbols {
			got[s.Name] = s.Addr
		}
		for _, n := range names {
			if a, ok := got[n]; !ok || a < 0xE000 {
				t.Errorf("%s: label %s = $%04X (ok=%v), want a $E000+ address",
					name, n, a, ok)
			}
		}
	}
}
