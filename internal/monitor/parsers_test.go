// Pure-function tests for the Monitor's parsers + formatters.
// Migrated from cmd/6502-control/commands_test.go when the Monitor
// moved into its own package.
package monitor

import (
	"reflect"
	"strings"
	"testing"
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
		{"  $FFFC  ", 0xFFFC, false},
		{"$00", 0x0000, false},
		{"$FFFF", 0xFFFF, false},
		{"", 0, true},
		{"$", 0, true},
		{"0x", 0, true},
		{"$XYZ", 0, true},
		{"$10000", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseAddr(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseAddr(%q) = %#x, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseAddr(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseAddr(%q) = $%04X, want $%04X", tc.in, got, tc.want)
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
		{"$100", 0, true},
		{"", 0, true},
		{"GZ", 0, true},
	}
	for _, tc := range cases {
		got, err := ParseByte(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseByte(%q) = $%02X, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseByte(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseByte(%q) = $%02X, want $%02X", tc.in, got, tc.want)
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
		{":$E000 A9", ":", "$E000 A9"},
		{".", ".", ""},
		{"? m", "?", "m"},
		{"help m", "help", "m"},
		{"", "", ""},
	}
	for _, tc := range cases {
		verb, rest := SplitVerb(tc.in)
		if verb != tc.wantVerb || rest != tc.wantR {
			t.Errorf("SplitVerb(%q) = (%q, %q), want (%q, %q)",
				tc.in, verb, rest, tc.wantVerb, tc.wantR)
		}
	}
}

func TestHexDumpShape(t *testing.T) {
	buf := []byte{
		0xA9, 0x2A, 0x8D, 0x10, 0x00, 0xEA, 0xEA, 0xEA,
		0x4C, 0x00, 0xE0, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x48, 0x49, 0x4A, 0x4B, 0x4C, 0x4D, 0x4E, 0x4F,
	}
	rows := HexDump(0xE000, buf)
	if len(rows) != 2 {
		t.Fatalf("HexDump rows = %d, want 2", len(rows))
	}
	if !strings.HasPrefix(rows[0], "E000  ") {
		t.Errorf("row 0 prefix = %q, want \"E000  \"", rows[0][:6])
	}
	if !strings.HasPrefix(rows[1], "E010  ") {
		t.Errorf("row 1 prefix = %q, want \"E010  \"", rows[1][:6])
	}
	if !strings.HasSuffix(rows[1], "HIJKLMNO") {
		t.Errorf("row 1 ASCII gutter = %q, want suffix \"HIJKLMNO\"", rows[1])
	}
}

func TestHexDumpSingleByte(t *testing.T) {
	rows := HexDump(0x1234, []byte{0xAB})
	if len(rows) != 1 {
		t.Fatalf("HexDump rows = %d, want 1", len(rows))
	}
	if !strings.HasPrefix(rows[0], "1234  AB ") {
		t.Errorf("single-byte row = %q, want prefix \"1234  AB \"", rows[0])
	}
	if !strings.HasSuffix(rows[0], ".") {
		t.Errorf("non-printable byte gutter not '.': %q", rows[0])
	}
}

func TestParseVIAPin(t *testing.T) {
	cases := []struct {
		in       string
		wantKind VIAPinKind
		wantPort byte
		wantBit  int
		err      bool
	}{
		{"PB0", VIAPinOutput, 'B', 0, false},
		{"PB7", VIAPinOutput, 'B', 7, false},
		{"PA0", VIAPinOutput, 'A', 0, false},
		{"pa3", VIAPinOutput, 'A', 3, false},
		{"  pb1  ", VIAPinOutput, 'B', 1, false},
		{"DDRB0", VIAPinDDR, 'B', 0, false},
		{"DDRA7", VIAPinDDR, 'A', 7, false},
		{"ddra0", VIAPinDDR, 'A', 0, false},
		{"  DDRB3  ", VIAPinDDR, 'B', 3, false},
		{"PA", VIAPortOutput, 'A', 0, false},
		{"PB", VIAPortOutput, 'B', 0, false},
		{"pa", VIAPortOutput, 'A', 0, false},
		{"  pb  ", VIAPortOutput, 'B', 0, false},
		{"DDRA", VIAPortDDR, 'A', 0, false},
		{"DDRB", VIAPortDDR, 'B', 0, false},
		{"ddra", VIAPortDDR, 'A', 0, false},
		{"P0", 0, 0, 0, true},
		{"PC0", 0, 0, 0, true},
		{"PC", 0, 0, 0, true},
		{"DDRC", 0, 0, 0, true},
		{"PB8", 0, 0, 0, true},
		{"PBX", 0, 0, 0, true},
		{"DDR0", 0, 0, 0, true},
		{"DDRC0", 0, 0, 0, true},
		{"DDRA8", 0, 0, 0, true},
		{"DDR", 0, 0, 0, true},
		{"DDRAB0", 0, 0, 0, true},
		{"P", 0, 0, 0, true},
		{"", 0, 0, 0, true},
	}
	for _, tc := range cases {
		kind, port, bit, err := ParseVIAPin(tc.in)
		if tc.err {
			if err == nil {
				t.Errorf("ParseVIAPin(%q) = (%v,%c,%d), want error", tc.in, kind, port, bit)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVIAPin(%q): %v", tc.in, err)
			continue
		}
		if kind != tc.wantKind || port != tc.wantPort || bit != tc.wantBit {
			t.Errorf("ParseVIAPin(%q) = (%v,%c,%d), want (%v,%c,%d)",
				tc.in, kind, port, bit, tc.wantKind, tc.wantPort, tc.wantBit)
		}
	}
}

func TestParseVIABitValue(t *testing.T) {
	cases := []struct {
		in   string
		kind VIAPinKind
		want int
		err  bool
	}{
		{"1", VIAPinOutput, 1, false},
		{"0", VIAPinDDR, 0, false},
		{"high", VIAPinOutput, 1, false},
		{"low", VIAPinOutput, 0, false},
		{"H", VIAPinOutput, 1, false},
		{"l", VIAPinOutput, 0, false},
		{"out", VIAPinDDR, 1, false},
		{"in", VIAPinDDR, 0, false},
		{"OUTPUT", VIAPinDDR, 1, false},
		{"input", VIAPinDDR, 0, false},
		{"in", VIAPinOutput, 0, true},
		{"high", VIAPinDDR, 0, true},
		{"yes", VIAPinOutput, 0, true},
		{"", VIAPinDDR, 0, true},
	}
	for _, tc := range cases {
		got, err := ParseVIABitValue(tc.in, tc.kind)
		if tc.err {
			if err == nil {
				t.Errorf("ParseVIABitValue(%q, %v) = %d, want error", tc.in, tc.kind, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVIABitValue(%q, %v): %v", tc.in, tc.kind, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseVIABitValue(%q, %v) = %d, want %d", tc.in, tc.kind, got, tc.want)
		}
	}
}

func TestHelpTablesPresent(t *testing.T) {
	expected := []string{"r ", "m ", "d ", ": ", "f ", "t ", "h ", "s ", "g ", ".", "reset", "bp ", "bc ", "bl ", "hw ", "via ", "cls ", "irq", "nmi", "reconnect", "stack"}
	full := strings.Join(HelpTable, "\n")
	for _, want := range expected {
		if !strings.Contains(full, want) {
			t.Errorf("HelpTable missing %q", want)
		}
	}
	keys := make([]string, 0, len(HelpDetail))
	for k := range HelpDetail {
		keys = append(keys, k)
	}
	want := []string{".", ":", "bc", "bl", "bp", "cls", "d", "f", "g", "h", "hw", "irq", "m", "nmi", "r", "reconnect", "reset", "s", "stack", "t", "via"}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("HelpDetail keys = %v, want %v", keys, want)
	}
}
