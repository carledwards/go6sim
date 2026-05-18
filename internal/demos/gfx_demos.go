package demos

import (
	"fmt"

	"github.com/carledwards/go6sim/asm"
)

// buildQuad — divides the framebuffer into 4 rectangular regions and
// rotates each independently in a different cardinal direction:
//
//	┌──── rows 0..5 ────┐
//	│  TL: rotate up    │  TR: rotate right
//	├──── row 6 div ────┤
//	│  BL: rotate down  │  BR: rotate left
//	└──── rows 7..12 ───┘
//
// All shifting is done by the VIC's CmdRect* commands — the CPU just
// sets RegRect{X,Y,W,H} and fires a CmdRectRot{Up,Right,Down,Left} at
// RegCmd. Pause + Frame commit each iteration as one snapshot.
func buildQuad() asm.Program {
	a := asm.New(0xE000)

	// Local command codes (not exported globally because they're
	// quad-specific — most demos don't use rect rotations).
	const (
		cmdRectRotUp    byte = 0x0F
		cmdRectRotDown  byte = 0x10
		cmdRectRotLeft  byte = 0x11
		cmdRectRotRight byte = 0x12
	)

	// fireRect emits the 5-instruction sequence that programs the
	// rect coords and fires a single CmdRect* command.
	fireRect := func(x, y, w, h, cmd byte) {
		a.LdaImm(x).StaAbs(RegRectX)
		a.LdaImm(y).StaAbs(RegRectY)
		a.LdaImm(w).StaAbs(RegRectW)
		a.LdaImm(h).StaAbs(RegRectH)
		a.LdaImm(cmd).StaAbs(RegCmd)
	}

	// === ENTRY ===
	a.Comment("clear the display").
		LdaImm(CmdClear).
		StaAbs(RegCmd)
	a.Comment("pause: UI only updates on RegFrame writes").
		LdaImm(0x01).
		StaAbs(RegPause)
	a.Comment("paint the initial quadrant patterns").
		JSR("DRAW")

	// === LOOP — issue four rect rotations, then frame-commit ===
	a.Label("LOOP")
	a.Comment("TL: rotate-up rect (0,0,20,6)")
	fireRect(0, 0, 20, 6, cmdRectRotUp)
	a.Comment("TR: rotate-right rect (20,0,20,6)")
	fireRect(20, 0, 20, 6, cmdRectRotRight)
	a.Comment("BL: rotate-down rect (0,7,20,6)")
	fireRect(0, 7, 20, 6, cmdRectRotDown)
	a.Comment("BR: rotate-left rect (20,7,20,6)")
	fireRect(20, 7, 20, 6, cmdRectRotLeft)
	a.Comment("commit the rotated frame").
		LdaImm(0x01).
		StaAbs(RegFrame)
	a.JMP("LOOP")

	// === DRAW — paint distinct color patterns into the 4 quadrants ===
	a.Label("DRAW")

	// drawHStripe paints a single 20-cell row with a constant color
	// byte. Used for TL and BL where vertical motion is the visible
	// effect (each row a distinct color so up/down shifting reads).
	drawHStripe := func(rowAddr uint16, colorByte byte) {
		a.LdaImm(colorByte)
		a.LdxImm(20 - 1)
		lbl := fmt.Sprintf("STR_%04X", rowAddr)
		a.Label(lbl)
		a.StaAbsX(rowAddr)
		a.DEX()
		a.BPL(lbl)
	}
	// TL rows 0..5 (color plane base $A000, 40 bytes/row).
	drawHStripe(0xA000, 0x1F)
	drawHStripe(0xA028, 0x2F)
	drawHStripe(0xA050, 0x3F)
	drawHStripe(0xA078, 0x4F)
	drawHStripe(0xA0A0, 0x5F)
	drawHStripe(0xA0C8, 0x6F)
	// BL rows 7..12.
	drawHStripe(0xA118, 0xAF)
	drawHStripe(0xA140, 0xBF)
	drawHStripe(0xA168, 0xCF)
	drawHStripe(0xA190, 0xDF)
	drawHStripe(0xA1B8, 0xEF)
	drawHStripe(0xA1E0, 0xFF)

	// drawVStripes paints multiple rows so each COLUMN gets a distinct
	// color (varies with X). Used for TR and BR where horizontal
	// motion is visible.
	drawVStripes := func(colorBias byte, rowAddrs ...uint16) {
		a.LdxImm(20 - 1)
		lbl := fmt.Sprintf("VS_%04X", rowAddrs[0])
		a.Label(lbl)
		a.TXA()
		a.CLC()
		a.AdcImm(colorBias)
		for _, addr := range rowAddrs {
			a.StaAbsX(addr)
		}
		a.DEX()
		a.BPL(lbl)
	}
	drawVStripes(0x80, 0xA014, 0xA03C, 0xA064, 0xA08C, 0xA0B4, 0xA0DC) // TR rows 0..5, columns 20..39
	drawVStripes(0x10, 0xA12C, 0xA154, 0xA17C, 0xA1A4, 0xA1CC, 0xA1F4) // BR rows 7..12, columns 20..39

	a.RTS()

	a.Symbol("DRAW", 0xE000, 0, "subroutine: paint the four quadrant patterns")

	return a.Build()
}

// buildBouncingBalls — four colored balls bouncing around the 160×100
// graphics plane. Pause+Frame so the user only sees fully-drawn
// frames; pacing via the 6522 VIA Timer 1 so motion speed is anchored
// to the VIA's own crystal, not the (variable) CPU clock.
//
// Pacing approach:
//
//	The VIA runs at 1 MHz on its own oscillator. We program T1 in
//	free-running mode with a latch of 50_000 cycles → underflow
//	every 50 ms. Each iteration of the main loop polls the T1 IFR
//	flag, clears it, and proceeds. The CPU is free to run as fast as
//	it likes — pacing comes from the chip, exactly like a real
//	W65C22S. When you halt the CPU to step, the timer keeps running,
//	so step-debugging through the wait loop shows the IFR bit
//	flipping just as it would on real silicon.
//
// Zero-page layout:
//
//	$10..$13   ball X positions
//	$14..$17   ball Y positions
//	$18..$1B   ball X velocities (signed: $01 = +1, $FF = -1)
//	$1C..$1F   ball Y velocities
//	$20..$23   ball palette indices
func buildBouncingBalls() asm.Program {
	a := asm.New(0xE000)

	const (
		ballRadius byte = 4
		numBalls   byte = 4
		minXY      byte = 4
		maxX       byte = 155 // 159 - radius
		maxY       byte = 95  // 99 - radius (graphics is 100 tall)
	)

	a.Symbol("BALL_X", 0x10, 4, "ball X positions (4 bytes)")
	a.Symbol("BALL_Y", 0x14, 4, "ball Y positions")
	a.Symbol("BALL_VX", 0x18, 4, "ball X velocities (signed)")
	a.Symbol("BALL_VY", 0x1C, 4, "ball Y velocities (signed)")
	a.Symbol("BALL_COL", 0x20, 4, "ball palette indices")

	// === ENTRY ===
	a.Comment("switch the VIC into graphics mode").
		LdaImm(0x01).
		StaAbs(RegMode)
	a.Comment("pause — display only updates on RegFrame writes").
		LdaImm(0x01).
		StaAbs(RegPause)

	// Program VIA Timer 1 for ~50 ms pacing.
	// Latch = 50_000 = $C350, at 1 MHz VIA crystal → 50 ms period.
	// Order matters: ACR=$40 (free-run) goes BEFORE T1C_H so T1
	// arms straight into free-run mode. If ACR is set after, a
	// wall-clock VIA can underflow in the gap and self-disarm via
	// one-shot semantics. Then T1L_L, then T1C_H (the high write
	// transfers latch→counter and starts T1).
	a.Comment("ACR bit 6 = T1 free-run mode (auto-reload on underflow)").
		LdaImm(ViaT1Bit).
		StaAbs(ViaACR)
	a.Comment("VIA T1 latch low: $50 (50_000 = $C350 → 50 ms @ 1 MHz)").
		LdaImm(0x50).
		StaAbs(ViaT1L_L)
	a.Comment("VIA T1 latch high — arms T1 in free-run mode").
		LdaImm(0xC3).
		StaAbs(ViaT1C_H)

	// Initialise four balls. Positions chosen to avoid initial overlap;
	// velocities a mix of ±1 in each axis; colors picked to look good
	// on the dark-blue clear background.
	initX := []byte{20, 80, 130, 50}
	initY := []byte{20, 40, 70, 85}
	initVX := []byte{0x01, 0xFF, 0x01, 0xFF}
	initVY := []byte{0x01, 0x01, 0xFF, 0xFF}
	initC := []byte{0x04, 0x02, 0x0E, 0x0B} // red, green, yellow, lt-cyan
	for i := byte(0); i < numBalls; i++ {
		a.LdaImm(initX[i]).StaZP(0x10 + i)
		a.LdaImm(initY[i]).StaZP(0x14 + i)
		a.LdaImm(initVX[i]).StaZP(0x18 + i)
		a.LdaImm(initVY[i]).StaZP(0x1C + i)
		a.LdaImm(initC[i]).StaZP(0x20 + i)
	}

	// === MAIN LOOP ===
	a.Label("MAIN")

	// Clear by drawing a full-screen filled rectangle in the
	// background color. Same visible result as CmdGfxClear, but it
	// shows off the rect-fill primitive — and a future demo could
	// pick a different rect to clear only part of the plane.
	a.Comment("background color: dark blue").
		LdaImm(0x01).
		StaAbs(RegGfxColor)
	a.Comment("rect origin (0,0)").
		LdaImm(0).
		StaAbs(RegRectX)
	a.LdaImm(0).StaAbs(RegRectY)
	a.Comment("rect size: full graphics plane (160x100)").
		LdaImm(160).
		StaAbs(RegRectW)
	a.LdaImm(100).StaAbs(RegRectH)
	a.Comment("fire filled-rect — paints the background").
		LdaImm(CmdGfxRectFill).
		StaAbs(RegCmd)

	// --- Draw all balls (fill circle for each).
	a.LdxImm(0)
	a.Label("DRAW")
	a.LdaZPX(0x10).StaAbs(RegRectX)
	a.LdaZPX(0x14).StaAbs(RegRectY)
	a.LdaImm(ballRadius).StaAbs(RegRectW)
	a.LdaZPX(0x20).StaAbs(RegGfxColor)
	a.LdaImm(CmdGfxFillCircle).StaAbs(RegCmd)
	a.INX()
	a.CpxImm(numBalls)
	a.BNE("DRAW")

	// --- Update positions + bounce off walls.
	a.LdxImm(0)
	a.Label("UPDATE")

	// x += vx
	a.CLC()
	a.LdaZPX(0x10)
	a.AdcZPX(0x18)
	a.StaZPX(0x10)
	a.Comment("hit left wall? if x < minXY").
		CmpImm(minXY).
		BCS("X_LEFT_OK")
	a.LdaZPX(0x18).EorImm(0xFE).StaZPX(0x18)
	a.LdaImm(minXY).StaZPX(0x10)
	a.IncZPX(0x20)
	a.Label("X_LEFT_OK")

	a.LdaZPX(0x10)
	a.Comment("hit right wall? if x >= maxX+1").
		CmpImm(maxX + 1).
		BCC("X_RIGHT_OK")
	a.LdaZPX(0x18).EorImm(0xFE).StaZPX(0x18)
	a.LdaImm(maxX).StaZPX(0x10)
	a.IncZPX(0x20)
	a.Label("X_RIGHT_OK")

	// y += vy
	a.CLC()
	a.LdaZPX(0x14)
	a.AdcZPX(0x1C)
	a.StaZPX(0x14)
	a.Comment("hit top wall?").
		CmpImm(minXY).
		BCS("Y_TOP_OK")
	a.LdaZPX(0x1C).EorImm(0xFE).StaZPX(0x1C)
	a.LdaImm(minXY).StaZPX(0x14)
	a.IncZPX(0x20)
	a.Label("Y_TOP_OK")

	a.LdaZPX(0x14)
	a.Comment("hit bottom wall?").
		CmpImm(maxY + 1).
		BCC("Y_BOT_OK")
	a.LdaZPX(0x1C).EorImm(0xFE).StaZPX(0x1C)
	a.LdaImm(maxY).StaZPX(0x14)
	a.IncZPX(0x20)
	a.Label("Y_BOT_OK")

	a.INX()
	a.CpxImm(numBalls)
	a.BNE("UPDATE")

	// --- Commit the frame.
	a.Comment("RegFrame: any write captures a snapshot").
		LdaImm(0x01).
		StaAbs(RegFrame)

	// --- Wait for VIA T1 underflow (50 ms pacing).
	a.Label("WAIT_TICK")
	a.Comment("read IFR; T1 sets bit 6 on underflow").
		LdaAbs(ViaIFR)
	a.AndImm(ViaT1Bit).
		BEQ("WAIT_TICK")
	a.Comment("read T1C-L to clear IFR T1 — ready for next period").
		LdaAbs(ViaT1C_L)

	a.JMP("MAIN")

	return a.Build()
}
