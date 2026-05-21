// Pure-function tests for the Monitor command parser. The dispatch
// methods themselves talk to the bridge, so they're covered by
// hand-testing against a running sim-serve; everything here is the
// stuff that's cheap to assert in isolation.
package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/carledwards/go6sim/bridge"
)

func TestParseAddr(t *testing.T) {
	cases := []struct {
		in   string
		want uint16
		err  bool
	}{
		{"$E000", 0xE000, false},
		{"0xE000", 0xE000, false},
		{"0XE000", 0xE000, false},
		{"E000", 0xE000, false},
		{"e000", 0xE000, false},
		{"  $FFFC  ", 0xFFFC, false}, // surrounding whitespace tolerated
		{"$00", 0x0000, false},
		{"$FFFF", 0xFFFF, false},
		{"", 0, true},
		{"$", 0, true},
		{"0x", 0, true},
		{"$XYZ", 0, true},
		{"$10000", 0, true}, // overflow 16-bit
	}
	for _, tc := range cases {
		got, err := parseAddr(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("parseAddr(%q) = %#x, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAddr(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseAddr(%q) = $%04X, want $%04X", tc.in, got, tc.want)
		}
	}
}

func TestParseByte(t *testing.T) {
	cases := []struct {
		in   string
		want byte
		err  bool
	}{
		{"$A9", 0xA9, false},
		{"0xA9", 0xA9, false},
		{"a9", 0xA9, false},
		{"00", 0x00, false},
		{"FF", 0xFF, false},
		{"$100", 0, true}, // overflow 8-bit
		{"", 0, true},
		{"GZ", 0, true},
	}
	for _, tc := range cases {
		got, err := parseByte(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("parseByte(%q) = $%02X, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseByte(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseByte(%q) = $%02X, want $%02X", tc.in, got, tc.want)
		}
	}
}

func TestSplitVerb(t *testing.T) {
	cases := []struct {
		in              string
		wantVerb, wantR string
	}{
		{"r", "r", ""},
		{"  r  ", "r", ""},
		{"m $E000 16", "m", "$E000 16"},
		{": $E000 A9 2A", ":", "$E000 A9 2A"},
		{":$E000 A9", ":", "$E000 A9"}, // no space after `:`
		{".", ".", ""},
		{"? m", "?", "m"},
		{"help m", "help", "m"},
		{"", "", ""},
	}
	for _, tc := range cases {
		verb, rest := splitVerb(tc.in)
		if verb != tc.wantVerb || rest != tc.wantR {
			t.Errorf("splitVerb(%q) = (%q, %q), want (%q, %q)",
				tc.in, verb, rest, tc.wantVerb, tc.wantR)
		}
	}
}

func TestHexDumpShape(t *testing.T) {
	// 24-byte buffer at $E000 → 2 rows: $E000 (full) + $E010 (8 bytes)
	buf := []byte{
		0xA9, 0x2A, 0x8D, 0x10, 0x00, 0xEA, 0xEA, 0xEA,
		0x4C, 0x00, 0xE0, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x48, 0x49, 0x4A, 0x4B, 0x4C, 0x4D, 0x4E, 0x4F,
	}
	rows := hexDump(0xE000, buf)
	if len(rows) != 2 {
		t.Fatalf("hexDump rows = %d, want 2", len(rows))
	}
	if !strings.HasPrefix(rows[0], "E000  ") {
		t.Errorf("row 0 prefix = %q, want \"E000  \"", rows[0][:6])
	}
	if !strings.HasPrefix(rows[1], "E010  ") {
		t.Errorf("row 1 prefix = %q, want \"E010  \"", rows[1][:6])
	}
	// ASCII gutter on row 1 should show "HIJKLMNO" (no trailing pad
	// chars because hexDump pads the hex side but not the gutter).
	if !strings.HasSuffix(rows[1], "HIJKLMNO") {
		t.Errorf("row 1 ASCII gutter = %q, want suffix \"HIJKLMNO\"", rows[1])
	}
}

func TestHexDumpSingleByte(t *testing.T) {
	rows := hexDump(0x1234, []byte{0xAB})
	if len(rows) != 1 {
		t.Fatalf("hexDump rows = %d, want 1", len(rows))
	}
	if !strings.HasPrefix(rows[0], "1234  AB ") {
		t.Errorf("single-byte row = %q, want prefix \"1234  AB \"", rows[0])
	}
	// Trailing ASCII gutter for non-printable byte → "."
	if !strings.HasSuffix(rows[0], ".") {
		t.Errorf("non-printable byte gutter not '.': %q", rows[0])
	}
}

func TestParseVIAPin(t *testing.T) {
	cases := []struct {
		in       string
		wantPort byte
		wantBit  int
		err      bool
	}{
		{"PB0", 'B', 0, false},
		{"PB7", 'B', 7, false},
		{"PA0", 'A', 0, false},
		{"pa3", 'A', 3, false}, // case-insensitive
		{"  pb1  ", 'B', 1, false},
		{"P0", 0, 0, true},  // missing port letter
		{"PC0", 0, 0, true}, // bad port
		{"PB8", 0, 0, true}, // bit out of range
		{"PBX", 0, 0, true}, // non-digit bit
		{"", 0, 0, true},
	}
	for _, tc := range cases {
		port, bit, err := parseVIAPin(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("parseVIAPin(%q) = (%c,%d), want error", tc.in, port, bit)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseVIAPin(%q): %v", tc.in, err)
			continue
		}
		if port != tc.wantPort || bit != tc.wantBit {
			t.Errorf("parseVIAPin(%q) = (%c,%d), want (%c,%d)",
				tc.in, port, bit, tc.wantPort, tc.wantBit)
		}
	}
}

// TestResolveVIABase_AmbiguityHandling — when multiple VIAs are
// present, an empty arg surfaces an error listing candidates rather
// than silently picking one. Named lookup works regardless of count.
func TestResolveVIABase_AmbiguityHandling(t *testing.T) {
	c := &Controller{
		regions: []bridge.Region{
			{Name: "ram", Lo: 0x0000, Hi: 0x1FFF},
			{Name: "via1", Lo: 0xB000, Hi: 0xB00F},
			{Name: "via2", Lo: 0xB100, Hi: 0xB10F},
			{Name: "rom", Lo: 0xE000, Hi: 0xFFFF},
		},
	}

	// Empty arg with multiple VIAs → error mentioning both names.
	if _, err := c.resolveVIABase(""); err == nil {
		t.Errorf("empty arg with 2 VIAs: got nil err, want ambiguity error")
	} else {
		msg := err.Error()
		if !strings.Contains(msg, "via1") || !strings.Contains(msg, "via2") {
			t.Errorf("ambiguity error should list both VIAs, got: %v", err)
		}
	}

	// Named lookup picks the right base.
	if base, err := c.resolveVIABase("via1"); err != nil || base != 0xB000 {
		t.Errorf("resolveVIABase(via1) = ($%04X, %v), want ($B000, nil)", base, err)
	}
	if base, err := c.resolveVIABase("via2"); err != nil || base != 0xB100 {
		t.Errorf("resolveVIABase(via2) = ($%04X, %v), want ($B100, nil)", base, err)
	}

	// Hex fallback also works.
	if base, err := c.resolveVIABase("$B000"); err != nil || base != 0xB000 {
		t.Errorf("resolveVIABase($B000) = ($%04X, %v), want ($B000, nil)", base, err)
	}

	// Unknown name surfaces a clear error.
	if _, err := c.resolveVIABase("via9"); err == nil {
		t.Errorf("unknown name: got nil err, want 'unknown VIA' error")
	}
}

// TestResolveVIABase_SingleVIAAutoResolves — when only one VIA is
// present, the empty-arg path quietly picks it.
func TestResolveVIABase_SingleVIAAutoResolves(t *testing.T) {
	c := &Controller{
		regions: []bridge.Region{
			{Name: "via1", Lo: 0xB000, Hi: 0xB00F},
		},
	}
	base, err := c.resolveVIABase("")
	if err != nil || base != 0xB000 {
		t.Errorf("single-VIA empty arg: got ($%04X, %v), want ($B000, nil)", base, err)
	}
}

func TestHelpTablesPresent(t *testing.T) {
	// Smoke test: the help table mentions each verb we wired, and the
	// detail map keys match what's documented. Catches typos where the
	// table promises a command but the dispatcher doesn't know it.
	expected := []string{"r ", "m ", "d ", ": ", "f ", "t ", "h ", "s ", "g ", ".", "reset", "bp ", "bc ", "bl ", "hw ", "via ", "cls ", "irq", "nmi", "reconnect"}
	full := strings.Join(helpTable, "\n")
	for _, want := range expected {
		if !strings.Contains(full, want) {
			t.Errorf("helpTable missing %q", want)
		}
	}
	// Detail map keys should all appear in the table (we don't want
	// orphan detail entries for unimplemented commands).
	keys := make([]string, 0, len(helpDetail))
	for k := range helpDetail {
		keys = append(keys, k)
	}
	want := []string{".", ":", "bc", "bl", "bp", "cls", "d", "f", "g", "h", "hw", "irq", "m", "nmi", "r", "reconnect", "reset", "s", "t", "via"}
	// sort for determinism
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("helpDetail keys = %v, want %v", keys, want)
	}
}
