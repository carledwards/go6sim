package demos

import "github.com/carledwards/go6sim/asm"

// buildMarquee — "HELLO 6502 SIM" scrolling marquee.
//
// Setup phase: clear, copy a 14-byte message into row 6 of the char
// plane, paint that range navy-bg/yellow-fg, program VIA Timer 1 in
// free-run mode for ~65 ms pacing.
// Steady state: rotate left, wait for T1 underflow, repeat.
//
// Pacing via VIA T1 instead of busy-loop means the scroll speed is
// anchored to the VIA's own crystal — independent of the CPU clock.
// Crank the CPU to "Max" and the marquee still scrolls at the same
// pace, just like real hardware where the timer chip runs on its
// own oscillator.
func buildMarquee() asm.Program {
	a := asm.New(0xE000)

	// === Setup ===
	a.Comment("clear the screen via VIC controller").
		LdaImm(CmdClear).
		StaAbs(RegCmd)

	// Program VIA T1 for free-running pacing.
	// Order matters: write ACR=$40 (free-run) BEFORE T1C_H so T1
	// arms directly into free-run mode. If we set ACR after T1C_H,
	// a wall-clock VIA can underflow in the gap and self-disarm
	// (one-shot semantics) before the program switches modes.
	// Latch = $FFFF → ~65 ms period at 1 MHz → ~15 scrolls / sec.
	a.Comment("ACR bit 6 = T1 free-run (auto-reload from latch)").
		LdaImm(ViaT1Bit).
		StaAbs(ViaACR)
	a.Comment("VIA T1 latch low: $FF").
		LdaImm(0xFF).
		StaAbs(ViaT1L_L)
	a.Comment("VIA T1 latch high: $FF — arms T1 in free-run mode").
		LdaImm(0xFF).
		StaAbs(ViaT1C_H)

	a.Comment("loop counter for message copy").
		LdxImm(0)

	// Copy 14-byte message into row 6 of the char plane and paint
	// the same range in the color plane.
	a.Label("MSG_LOOP")
	a.Comment("read message byte from MSG table (laid out after code)").
		LdaAbsXLabel("MSG")
	a.Comment("write to char plane row 6 (offset 240 = 6*40)").
		StaAbsX(CharBase + 240)
	a.Comment("color: bg=navy ($1), fg=yellow ($E)").
		LdaImm(0x1E)
	a.Comment("write color attribute").
		StaAbsX(ColorBase + 240)
	a.INX()
	a.Comment("13 = msg length - 1; loop while X < 14").
		CpxImm(14)
	a.BNE("MSG_LOOP")

	// === Steady state ===
	a.Label("ROT_FOREVER")
	a.Comment("CmdRotLeft = $07: rotate every row 1 cell left, wrapping").
		LdaImm(CmdRotLeft)
	a.StaAbs(RegCmd)
	// Wait for T1 underflow before next rotation.
	a.Label("WAIT_TICK")
	a.Comment("read IFR; T1 sets bit 6 on underflow").
		LdaAbs(ViaIFR)
	a.AndImm(ViaT1Bit).
		BEQ("WAIT_TICK")
	a.Comment("read T1C-L to clear IFR T1 — ready for next period").
		LdaAbs(ViaT1C_L)
	a.JMP("ROT_FOREVER")

	// === Data ===
	msgAddr := a.PC()
	a.Label("MSG")
	a.Bytes('H', 'E', 'L', 'L', 'O', ' ', '6', '5', '0', '2', ' ', 'S', 'I', 'M')

	a.Symbol("MSG", msgAddr, 14, "marquee message bytes")

	return a.Build()
}

// buildBouncer — single colored '*' bounces left-and-right across
// row 6, erasing its trail. ZP[$10] = x, ZP[$11] = direction (+1/-1).
// Pacing via VIA T1 free-run underflow so motion stays at ~20 hops/sec
// independent of CPU clock — same pattern as Marquee / Bouncing Balls.
func buildBouncer() asm.Program {
	a := asm.New(0xE000)

	// === Setup ===
	a.Comment("clear the screen").
		LdaImm(CmdClear).
		StaAbs(RegCmd)
	a.Comment("pause — display only updates on RegFrame writes").
		LdaImm(0x01).
		StaAbs(RegPause)

	// Program VIA T1 for ~50 ms free-run pacing.
	// Latch = 50_000 = $C350, at 1 MHz VIA crystal → 50 ms period.
	// Order matters: ACR=$40 (free-run) goes BEFORE T1C_H so T1
	// arms straight into free-run mode and won't self-disarm.
	a.Comment("ACR bit 6 = T1 free-run (auto-reload from latch)").
		LdaImm(ViaT1Bit).
		StaAbs(ViaACR)
	a.Comment("VIA T1 latch low: $50 ($C350 = 50_000 → 50 ms @ 1 MHz)").
		LdaImm(0x50).
		StaAbs(ViaT1L_L)
	a.Comment("VIA T1 latch high — arms T1 in free-run mode").
		LdaImm(0xC3).
		StaAbs(ViaT1C_H)

	a.Comment("initial X = 20 (centre)").
		LdxImm(20).
		StxZP(0x10)
	a.Comment("initial dx = +1 (moving right)").
		LdaImm(0x01).
		StaZP(0x11)

	// === Main loop ===
	a.Label("LOOP")

	a.Comment("load X position into X register").
		LdxZP(0x10)

	a.Comment("erase old char (write space)").
		LdaImm(' ').
		StaAbsX(CharBase + 240)
	a.Comment("erase old color (black on black)").
		LdaImm(0x00).
		StaAbsX(ColorBase + 240)

	// Move: if dx >= 0 then INX else DEX.
	a.Comment("check direction sign").
		LdaZP(0x11).
		BPL("MOVE_RIGHT")
	a.DEX()
	a.JMP("STORE_X")
	a.Label("MOVE_RIGHT")
	a.INX()
	a.Label("STORE_X")
	a.StxZP(0x10)

	// Draw: '*' at row 6 col x.
	a.Comment("'*' character").
		LdaImm('*').
		StaAbsX(CharBase + 240)
	a.Comment("color: bg=navy, fg=yellow").
		LdaImm(0x1E).
		StaAbsX(ColorBase + 240)

	// Commit the erase+draw as one atomic frame — without RegFrame
	// the user sees the blank cell between the space-write and the
	// star-write (visible blink at low CPU speeds).
	a.Comment("RegFrame: any write captures a snapshot").
		LdaImm(0x01).
		StaAbs(RegFrame)

	// Wait for VIA T1 underflow (~50 ms regardless of CPU speed).
	a.Label("WAIT_TICK")
	a.Comment("read IFR; T1 sets bit 6 on underflow").
		LdaAbs(ViaIFR)
	a.AndImm(ViaT1Bit).
		BEQ("WAIT_TICK")
	a.Comment("read T1C-L to clear IFR T1 — ready for next period").
		LdaAbs(ViaT1C_L)

	// Bounds: if X==0 or X==39, flip direction.
	a.Comment("hit left wall?").
		CpxImm(0).
		BEQ("FLIP")
	a.Comment("hit right wall?").
		CpxImm(39).
		BEQ("FLIP")
	a.JMP("LOOP")

	a.Label("FLIP")
	a.Comment("dx = -dx via two's complement").
		LdaZP(0x11).
		EorImm(0xFF).
		CLC().
		AdcImm(0x01).
		StaZP(0x11)
	a.JMP("LOOP")

	a.Symbol("X", 0x10, 1, "current X position 0..39")
	a.Symbol("DX", 0x11, 1, "direction byte: +1 or -1 (signed)")

	return a.Build()
}

// buildScroller — fills the bottom row with X+counter, then pokes
// CmdShiftUp. Diagonal gradient climbs up the screen each iteration.
func buildScroller() asm.Program {
	a := asm.New(0xE000)

	a.Label("LOOP")
	a.Comment("counter at user-ZP $10 (system reserves $00..$0F)").
		IncZP(0x10)
	a.Comment("X iterates 0..39 across the bottom row").
		LdxImm(0)

	a.Label("FILL")
	a.Comment("A = X").
		TXA()
	a.CLC()
	a.Comment("A = X + counter").
		AdcZP(0x10)
	a.Comment("color plane bottom row: $A000 + 12*40 = $A1E0").
		StaAbsX(ColorBase + 480)
	a.Comment("char plane bottom row: $A400 + 12*40 = $A5E0").
		StaAbsX(CharBase + 480)
	a.INX()
	a.CpxImm(40)
	a.BNE("FILL")

	a.Comment("CmdShiftUp scrolls the entire display up one row").
		LdaImm(CmdShiftUp).
		StaAbs(RegCmd)

	a.JMP("LOOP")

	a.Symbol("COUNTER", 0x10, 1, "scroll iteration counter (color/char gradient seed)")

	return a.Build()
}

// buildSnow — fills both planes with pseudo-random colored chars
// from an 8-bit Galois LFSR (taps = $B8), then clears via
// controller and repeats.
func buildSnow() asm.Program {
	a := asm.New(0xE000)

	// Seed the LFSR.
	a.Comment("seed = 1 (any non-zero starts the LFSR)").
		LdaImm(0x01).
		StaZP(0x10)

	a.Label("OUTER")
	a.LdxImm(0)

	// Pass 1: cells 0..255 — color $A000..$A0FF, char $A400..$A4FF.
	a.Label("PASS1")
	a.Comment("step LFSR: shift right, XOR taps if bit 0 was set").
		LdaZP(0x10).
		LsrA().
		BCC("P1_NOXOR")
	a.EorImm(0xB8)
	a.Label("P1_NOXOR")
	a.StaZP(0x10)
	a.Comment("paint color plane page 0").
		StaAbsX(0xA000)
	a.Comment("derive char from same byte").
		EorImm(0x5A)
	a.Comment("paint char plane page 0").
		StaAbsX(0xA400)
	a.INX()
	a.BNE("PASS1")

	// Pass 2: cells 256..511 — color $A100..$A1FF, char $A500..$A5FF.
	a.LdxImm(0)
	a.Label("PASS2")
	a.LdaZP(0x10).
		LsrA().
		BCC("P2_NOXOR")
	a.EorImm(0xB8)
	a.Label("P2_NOXOR")
	a.StaZP(0x10)
	a.StaAbsX(0xA100)
	a.EorImm(0x5A)
	a.StaAbsX(0xA500)
	a.INX()
	a.BNE("PASS2")

	// Delay so the user sees the snow.
	a.Comment("outer delay counter").
		LdyImm(0x10)
	a.Label("D_OUT")
	a.LdxImm(0xFF)
	a.Label("D_IN")
	a.DEX()
	a.BNE("D_IN")
	a.DEY()
	a.BNE("D_OUT")

	// Clear and repeat.
	a.Comment("clear via VIC, then loop").
		LdaImm(CmdClear).
		StaAbs(RegCmd)
	a.JMP("OUTER")

	a.Symbol("LFSR", 0x10, 1, "8-bit Galois LFSR state (taps = $B8)")

	return a.Build()
}

// buildBlitter — classic double-buffer pattern. Build a 256-byte image
// in off-screen RAM at $1000, then copy into both VIC planes, then
// fire RegFrame so the user only sees fully-rendered frames.
func buildBlitter() asm.Program {
	a := asm.New(0xE000)

	// Pause once at boot — VIC shows snapshot, not in-progress writes.
	a.Comment("pause: UI shows snapshot until RegFrame is written").
		LdaImm(0x01).
		StaAbs(RegPause)

	a.Label("LOOP")
	a.Comment("counter advances each frame").
		IncZP(0x10)
	a.LdxImm(0)

	// Build off-screen image in RAM at $1000: buffer[X] = X + counter.
	a.Label("BUILD")
	a.TXA()
	a.CLC()
	a.AdcZP(0x10)
	a.Comment("scratch buffer at $1000 (within RAM region)").
		StaAbsX(0x1000)
	a.INX()
	a.BNE("BUILD")

	// Blit RAM[$1000..] → color plane page 0.
	a.LdxImm(0)
	a.Label("BLIT_C")
	a.LdaAbsX(0x1000)
	a.StaAbsX(0xA000)
	a.INX()
	a.BNE("BLIT_C")

	// Blit RAM[$1000..] → char plane page 0, mapped into '@'..'_'.
	a.LdxImm(0)
	a.Label("BLIT_H")
	a.LdaAbsX(0x1000)
	a.Comment("low 5 bits give 0..31").
		AndImm(0x1F).
		CLC().
		AdcImm('@')
	a.StaAbsX(0xA400)
	a.INX()
	a.BNE("BLIT_H")

	// Commit the snapshot — UI now shows the freshly-built image.
	a.Comment("RegFrame: any write captures the new snapshot").
		StaAbs(RegFrame)

	a.JMP("LOOP")

	a.Symbol("COUNTER", 0x10, 1, "frame counter (drives gradient phase)")
	a.Symbol("SCRATCH", 0x1000, 256, "off-screen image buffer (256 bytes)")

	return a.Build()
}

// buildScrollerFramed — same shape as Scroller but uses Pause+Frame
// so the user only sees clean snapshots.
func buildScrollerFramed() asm.Program {
	a := asm.New(0xE000)

	a.Comment("pause once").
		LdaImm(0x01).
		StaAbs(RegPause)

	a.Label("LOOP")
	a.Comment("shift first so the bottom row is blank when we fill it").
		LdaImm(CmdShiftUp).
		StaAbs(RegCmd)

	a.IncZP(0x10)
	a.LdxImm(0)

	a.Label("FILL")
	a.TXA()
	a.CLC()
	a.AdcZP(0x10)
	a.StaAbsX(ColorBase + 480)
	a.StaAbsX(CharBase + 480)
	a.INX()
	a.CpxImm(40)
	a.BNE("FILL")

	a.Comment("commit the freshly-filled bottom row").
		StaAbs(RegFrame)

	a.JMP("LOOP")

	a.Symbol("COUNTER", 0x10, 1, "frame counter (drives gradient phase)")

	return a.Build()
}
