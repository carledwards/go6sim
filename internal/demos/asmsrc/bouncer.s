; Bouncer — a colored '*' bounces left/right across row 6, erasing
; its trail. Pacing comes from VIA Timer 1 underflow (free-run), so
; the bounce rate tracks the VIA's own crystal — independent of the
; CPU clock. Crank the CPU to Max and the bouncer still moves at the
; same ~20 hops/sec.
;
; Zero-page variables. Declaring them as exported constants makes
; go6asm surface them in the symbol table, so the simulator's memory
; view labels $10/$11 as X / DX instead of bare addresses.
X  = $10               ; current X position 0..39
DX = $11               ; direction byte: +1 (right) or -1 (left)
.exportzp X
.exportzp DX

        LDA #CmdClear          ; clear the screen
        STA RegCmd

        LDA #$01               ; pause — display only updates on RegFrame writes
        STA RegPause           ; (otherwise the erase-then-draw flicker is visible)

        LDA #ViaT1Bit          ; ACR bit 6 = T1 free-run (auto-reload from latch)
        STA ViaACR
        LDA #$50               ; VIA T1 latch low: $50 ($C350 = 50_000 → 50 ms @ 1 MHz)
        STA ViaT1L_L
        LDA #$C3               ; VIA T1 latch high — arms T1 in free-run mode
        STA ViaT1C_H

        LDX #20                ; initial X = 20 (centre)
        STX X
        LDA #$01               ; initial dx = +1 (moving right)
        STA DX

LOOP:
        LDX X                  ; load X position into the X register
        LDA #' '               ; erase old char (write space)
        STA CharBase+240,X
        LDA #$00               ; erase old color (black on black)
        STA ColorBase+240,X

        LDA DX                 ; check direction sign
        BPL MOVE_RIGHT
        DEX
        JMP STORE_X
MOVE_RIGHT:
        INX
STORE_X:
        STX X

        LDA #'*'               ; '*' character
        STA CharBase+240,X
        LDA #$1E               ; color: bg=navy, fg=yellow
        STA ColorBase+240,X

        LDA #$01               ; commit the erase+draw as one frame
        STA RegFrame           ; (any write captures a snapshot)

WAIT_TICK:
        LDA ViaIFR             ; read IFR; T1 sets bit 6 on underflow
        AND #ViaT1Bit
        BEQ WAIT_TICK
        LDA ViaT1C_L           ; read T1C-L to clear IFR T1 — ready for next period

        CPX #0                 ; hit left wall?
        BEQ FLIP
        CPX #39                ; hit right wall?
        BEQ FLIP
        JMP LOOP

FLIP:
        LDA DX                 ; dx = -dx via two's complement
        EOR #$FF
        CLC
        ADC #$01
        STA DX
        JMP LOOP
