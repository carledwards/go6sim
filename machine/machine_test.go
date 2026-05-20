package machine_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/carledwards/go6sim/bus"
	"github.com/carledwards/go6sim/internal/demos"
	"github.com/carledwards/go6sim/machine"
)

// layout returns "name@HEX" for every attached card, base-sorted (the
// backplane already returns Components base-sorted).
func layout(bp interface{ Components() []bus.Component }) []string {
	var out []string
	for _, c := range bp.Components() {
		out = append(out, fmt.Sprintf("%s@%04X", c.Name(), c.Base()))
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hexBytes(b []byte) string {
	s := ""
	for _, x := range b {
		s += fmt.Sprintf(" %02X", x)
	}
	return s
}

// Structural proof: the presets compose exactly the documented maps.
// For VICDemo this is the "byte-identical to today's cmd/6502-sim"
// guarantee at the wiring level.
func TestPresetLayouts(t *testing.T) {
	wantVIC := []string{
		"ram@0000", "display.color@A000", "display.char@A400",
		"display.ctrl@A800", "via1@B000", "rom@E000",
	}
	if got := layout(machine.VICDemo().BP); !eq(got, wantVIC) {
		t.Fatalf("VICDemo layout:\n got %v\nwant %v", got, wantVIC)
	}

	wantTeach := []string{
		"ram@0000", "framebuffer@A000", "via1@B000", "rom@E000",
	}
	if got := layout(machine.TeachMin().BP); !eq(got, wantTeach) {
		t.Fatalf("TeachMin layout:\n got %v\nwant %v", got, wantTeach)
	}

	wantMerlin := []string{
		"ram@0000", "via1@B000", "via2@B100", "rom@E000",
	}
	if got := layout(machine.TeachMerlin().BP); !eq(got, wantMerlin) {
		t.Fatalf("TeachMerlin layout:\n got %v\nwant %v", got, wantMerlin)
	}
}

// TeachMerlin contract: two independent VIAs share the bus. A write to
// one VIA's Port B must not leak into the other's, and vice versa —
// they're distinct chips at distinct base addresses, even though both
// are 6522s. This is the structural composability proof the bridge spec
// leans on (two of the same card type, isolated, taps namespaced).
func TestTeachMerlinTwoVIAsAreIndependent(t *testing.T) {
	m := machine.TeachMerlin()

	m.Inst.Poke(0xB000, 0xAA) // via1 ORB
	m.Inst.Poke(0xB100, 0x55) // via2 ORB

	if got := m.Inst.Peek(0xB000); got != 0xAA {
		t.Errorf("via1 ORB: got $%02X, want $AA", got)
	}
	if got := m.Inst.Peek(0xB100); got != 0x55 {
		t.Errorf("via2 ORB: got $%02X, want $55", got)
	}

	// Flip them; check the other did not change as a side effect.
	m.Inst.Poke(0xB000, 0x11)
	if got := m.Inst.Peek(0xB100); got != 0x55 {
		t.Errorf("via2 leaked from via1 write: got $%02X, want $55", got)
	}
	m.Inst.Poke(0xB100, 0x22)
	if got := m.Inst.Peek(0xB000); got != 0x11 {
		t.Errorf("via1 leaked from via2 write: got $%02X, want $11", got)
	}
}

// Deterministic demo-execution golden: compose VICDemo via the preset,
// load a real shipped demo, drive it through the instrument for a fixed
// virtual budget, and snapshot {registers, a char-plane span} against a
// committed golden. This is the first demo-driven execution net and the
// proof the VICDemo preset runs programs identically to the hand-wired
// machine. Regenerate intentionally with GO6SIM_GOLDEN_UPDATE=1.
func TestVICDemoRunsHelloDeterministically(t *testing.T) {
	m := machine.VICDemo()
	if err := m.Load(demos.HelloDemo.Bytes); err != nil {
		t.Fatalf("load HelloDemo: %v", err)
	}

	m.Inst.SetRunning(true)
	m.Inst.Driver().MaxBatch = 2000
	m.Inst.Driver().SetSpeedHz(0) // Max → 2000 half-steps per Advance(referenceTick)
	for k := 0; k < 6; k++ {
		m.Inst.Advance(50 * time.Millisecond)
	}

	st := m.Inst.State()
	// HelloDemo clears the screen then writes "HELLO FROM go6asm" to
	// row 6 (offset 6*40=240): char plane $A400+240=$A4F0, color plane
	// $A000+240=$A0F0. Snapshot exactly that row so the golden proves
	// real rendering — not just the CmdClear space-fill.
	charRow6 := m.Inst.Mem(0xA4F0, 0xA517)  // 40 bytes (one row)
	colorRow6 := m.Inst.Mem(0xA0F0, 0xA103) // first 20 cells

	got := fmt.Sprintf("regs A=%02X X=%02X Y=%02X P=%02X PC=%04X\n",
		st.A, st.X, st.Y, st.P, st.PC)
	got += "char  row6:" + hexBytes(charRow6) + "\n"
	got += "color row6:" + hexBytes(colorRow6) + "\n"

	goldenPath := filepath.Join("testdata", "vicdemo_hello.golden")
	if os.Getenv("GO6SIM_GOLDEN_UPDATE") != "" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden updated:\n%s", got)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (bootstrap with GO6SIM_GOLDEN_UPDATE=1): %v", err)
	}
	if got != string(want) {
		t.Fatalf("VICDemo execution regressed.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TeachMin contract: the framebuffer is plain RAM the consumer reads —
// no display card, no protocol (resolved Q2). A poke to the char-plane
// address must read straight back as bytes.
func TestTeachMinFramebufferIsRAM(t *testing.T) {
	m := machine.TeachMin()
	m.Inst.Poke(0xA400, 0x41) // 'A' to the char plane region
	m.Inst.Poke(0xA401, 0x42)
	if g := m.Inst.Mem(0xA400, 0xA401); g[0] != 0x41 || g[1] != 0x42 {
		t.Fatalf("framebuffer RAM read-back = % X, want 41 42", g)
	}
}
