// Package goldentrace is the runtime regression net for the backplane
// carve. The repo's other backstops (internal/demos equiv + analyze) are
// assembly-level — they prove go6asm emits the right bytes, but they do
// not execute the machine, so they cannot catch a runtime regression
// from the carve (notably brick 3: converting the wall-clock Ticker to
// cycle-based, and brick 4: the Backplane fan-out).
//
// This test assembles a tiny, fully deterministic program with go6asm,
// runs it on the *current* machine primitives (bus + RAM + ROM + interp
// CPU) for a fixed step budget, and snapshots {registers, a zero-page
// window} against a committed golden. Any brick that changes observable
// execution behaviour fails here loudly. Regenerate intentionally with
// GO6SIM_GOLDEN_UPDATE=1 (a deliberate, reviewed act — same discipline
// as go6asm's GO6ASM_UPDATE).
package goldentrace

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	go6 "github.com/carledwards/go6asm/asm"

	"github.com/carledwards/go6sim/bus"
	"github.com/carledwards/go6sim/components/ram"
	"github.com/carledwards/go6sim/components/rom"
	"github.com/carledwards/go6sim/cpu/interp"
)

// A deliberately simple program that touches registers, a counted loop,
// indexed zero-page stores, and a terminal spin — enough that a stepping
// or bus regression perturbs the snapshot. Layer-0: just instructions;
// go6asm infers the $E000 load address and synthesizes the vectors.
const program = `
  ldx #$00
loop:
  txa
  sta $10,x
  inx
  cpx #$08
  bne loop
done:
  jmp done
`

const (
	romBase  = 0xE000
	romSize  = 0x2000 // $E000-$FFFF, covers the synthesized vector block
	ramBase  = 0x0000
	ramSize  = 0x2000 // $0000-$1FFF
	stepHalf = 4000   // HalfSteps: ample to finish the loop and settle
)

func TestGoldenTrace(t *testing.T) {
	res := go6.Assemble(go6.Input{
		Entry:  "trace",
		Files:  []go6.SourceFile{{Name: "trace", Content: []byte(program)}},
		Layer0: true,
		Target: "sim-tui",
	})
	if !res.Ok() {
		t.Fatalf("assemble: %v", res.Errors)
	}
	if int(res.Origin) != romBase {
		t.Fatalf("origin = $%04X, want $%04X (sim-tui Layer-0)", res.Origin, romBase)
	}

	b := bus.New()
	mainRAM := ram.New("ram", ramBase, ramSize)
	mainROM := rom.New("rom", romBase, romSize)
	if err := mainROM.Load(0, res.Image); err != nil {
		t.Fatalf("rom load: %v", err)
	}
	if err := b.Register(mainRAM); err != nil {
		t.Fatalf("register ram: %v", err)
	}
	if err := b.Register(mainROM); err != nil {
		t.Fatalf("register rom: %v", err)
	}

	cpu := interp.New(b)
	cpu.Reset() // fetches the reset vector ($FFFC/$FFFD) from the ROM image
	for i := 0; i < stepHalf; i++ {
		cpu.HalfStep()
	}

	r := cpu.Registers()
	var zp [8]byte
	for i := range zp {
		zp[i] = b.Read(uint16(0x10 + i))
	}
	got := fmt.Sprintf(
		"regs A=%02X X=%02X Y=%02X S=%02X P=%02X PC=%04X\n"+
			"zp10: %02X %02X %02X %02X %02X %02X %02X %02X\n",
		r.A, r.X, r.Y, r.S, r.P, r.PC,
		zp[0], zp[1], zp[2], zp[3], zp[4], zp[5], zp[6], zp[7])

	goldenPath := filepath.Join("testdata", "trace.golden")
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
		t.Fatalf("runtime trace regressed.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
