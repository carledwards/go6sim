// Pure parsing + formatting helpers used by the Monitor's command
// dispatcher. No bridge.Target / Host dependency — all string in,
// typed value out. Easy to unit-test in isolation.
package monitor

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SplitVerb returns the first whitespace-separated token and the rest
// of the line, both trimmed. The leading `:`, `.`, and `?` operators
// are special-cased so users can write `: $E000 A9 2A` with or
// without a space after the operator.
func SplitVerb(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", ""
	}
	switch line[0] {
	case ':', '.', '?':
		return string(line[0]), strings.TrimSpace(line[1:])
	}
	i := strings.IndexAny(line, " \t")
	if i < 0 {
		return line, ""
	}
	return line[:i], strings.TrimSpace(line[i+1:])
}

// ParseAddr accepts "$E000", "0xE000", "E000" — always hex, max 16
// bits. Decimal addresses aren't a 6502 idiom and would only invite
// off-by-base bugs, so we don't support them.
func ParseAddr(s string) (uint16, error) {
	t := strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(t, "$"):
		t = t[1:]
	case strings.HasPrefix(t, "0x"), strings.HasPrefix(t, "0X"):
		t = t[2:]
	}
	if t == "" {
		return 0, errors.New("empty address")
	}
	v, err := strconv.ParseUint(t, 16, 16)
	if err != nil {
		return 0, fmt.Errorf("bad address %q", s)
	}
	return uint16(v), nil
}

// ParseByte accepts the same prefixes as ParseAddr but tops out at 8
// bits.
func ParseByte(s string) (byte, error) {
	t := strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(t, "$"):
		t = t[1:]
	case strings.HasPrefix(t, "0x"), strings.HasPrefix(t, "0X"):
		t = t[2:]
	}
	if t == "" {
		return 0, errors.New("empty byte")
	}
	v, err := strconv.ParseUint(t, 16, 8)
	if err != nil {
		return 0, fmt.Errorf("bad byte %q", s)
	}
	return byte(v), nil
}

// VIAPinKind distinguishes the four meanings a VIA pin/port reference
// can have. Per-bit forms target one bit via RMW; whole-port forms
// rewrite the entire 8-bit register in one write. Same syntax
// (`via set <pin> <val>`); different register + width.
type VIAPinKind int

const (
	VIAPinOutput  VIAPinKind = iota // PA0..PA7, PB0..PB7
	VIAPinDDR                       // DDRA0..DDRA7, DDRB0..DDRB7
	VIAPortOutput                   // PA, PB              — whole ORA / ORB
	VIAPortDDR                      // DDRA, DDRB          — whole DDRA / DDRB
)

// ParseVIAPin parses pin / port tokens in four flavours:
//
//	PB3   / pa0     single-bit output  (ORB/ORA RMW)
//	DDRB7 / ddra0   single-bit DDR     (DDRB/DDRA RMW)
//	PB    / pa      whole-port output  (ORB/ORA write)
//	DDRB  / ddra    whole-port DDR     (DDRB/DDRA write)
//
// For per-bit forms `bit` is 0..7; for whole-port forms it's 0
// (unused — caller dispatches on kind). Bad tokens return an error.
func ParseVIAPin(s string) (kind VIAPinKind, port byte, bit int, err error) {
	t := strings.ToUpper(strings.TrimSpace(s))

	if strings.HasPrefix(t, "DDR") {
		switch len(t) {
		case 4:
			if t[3] != 'A' && t[3] != 'B' {
				return 0, 0, 0, fmt.Errorf("bad pin %q (port must be A or B)", s)
			}
			return VIAPortDDR, t[3], 0, nil
		case 5:
			if t[3] != 'A' && t[3] != 'B' {
				return 0, 0, 0, fmt.Errorf("bad pin %q (port must be A or B)", s)
			}
			if t[4] < '0' || t[4] > '7' {
				return 0, 0, 0, fmt.Errorf("bad pin %q (bit must be 0..7)", s)
			}
			return VIAPinDDR, t[3], int(t[4] - '0'), nil
		default:
			return 0, 0, 0, fmt.Errorf("bad pin %q (want DDRA/DDRB or DDRA0..DDRB7)", s)
		}
	}

	if len(t) > 0 && t[0] == 'P' {
		switch len(t) {
		case 2:
			if t[1] != 'A' && t[1] != 'B' {
				return 0, 0, 0, fmt.Errorf("bad pin %q (port must be A or B)", s)
			}
			return VIAPortOutput, t[1], 0, nil
		case 3:
			if t[1] != 'A' && t[1] != 'B' {
				return 0, 0, 0, fmt.Errorf("bad pin %q (port must be A or B)", s)
			}
			if t[2] < '0' || t[2] > '7' {
				return 0, 0, 0, fmt.Errorf("bad pin %q (bit must be 0..7)", s)
			}
			return VIAPinOutput, t[1], int(t[2] - '0'), nil
		default:
			return 0, 0, 0, fmt.Errorf("bad pin %q (want PA/PB or PA0..PB7)", s)
		}
	}

	return 0, 0, 0, fmt.Errorf("bad pin %q (want PA0..PB7, PA/PB, DDRA0..DDRB7, DDRA/DDRB)", s)
}

// ParseVIABitValue accepts numeric (1/0) or alphabetic synonyms for
// a VIA bit value. Synonyms differ by kind so help text reads
// naturally:
//
//	output pins: 1, 0, high, low, h, l
//	DDR pins:    1, 0, out, in, output, input
//
// A mismatched synonym (e.g., "in" given for an output-pin command)
// is rejected so the user knows which register they really meant.
func ParseVIABitValue(s string, kind VIAPinKind) (int, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	switch t {
	case "1":
		return 1, nil
	case "0":
		return 0, nil
	}
	if kind == VIAPinDDR {
		switch t {
		case "out", "output", "o":
			return 1, nil
		case "in", "input", "i":
			return 0, nil
		}
	} else {
		switch t {
		case "high", "h":
			return 1, nil
		case "low", "l":
			return 0, nil
		}
	}
	return 0, fmt.Errorf("bad value %q (want 0/1, or 'in'/'out' for DDR, 'high'/'low' for pin)", s)
}

// HexDump formats buf into HESMON-style rows: address, 16 hex bytes,
// ASCII gutter. Returns one string per visual row so the caller can
// emit each as its own scrollback line.
func HexDump(base uint16, buf []byte) []string {
	const bytesPerRow = 16
	rows := (len(buf) + bytesPerRow - 1) / bytesPerRow
	out := make([]string, 0, rows)
	for r := 0; r < rows; r++ {
		off := r * bytesPerRow
		end := off + bytesPerRow
		if end > len(buf) {
			end = len(buf)
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "%04X  ", uint32(base)+uint32(off))
		for i := off; i < off+bytesPerRow; i++ {
			if i < end {
				fmt.Fprintf(&sb, "%02X ", buf[i])
			} else {
				sb.WriteString("   ")
			}
			if i-off == 7 {
				sb.WriteString(" ")
			}
		}
		sb.WriteString(" ")
		for i := off; i < end; i++ {
			b := buf[i]
			if b >= 0x20 && b < 0x7F {
				sb.WriteByte(b)
			} else {
				sb.WriteByte('.')
			}
		}
		out = append(out, sb.String())
	}
	return out
}

// FlagsStr renders P-register flag bits as "N V - B D I Z C" with
// letters for set bits and dots for clear. The "-" hyphen position
// is the always-1 "U" bit (bit 5) on a real 6502.
func FlagsStr(p uint8) string {
	bits := []byte{'N', 'V', '-', 'B', 'D', 'I', 'Z', 'C'}
	out := make([]byte, 0, 16)
	for i := 0; i < 8; i++ {
		c := byte('.')
		if p&(1<<(7-i)) != 0 {
			c = bits[i]
		}
		out = append(out, c, ' ')
	}
	return string(out)
}
