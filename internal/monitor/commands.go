// Command dispatcher for the Monitor REPL. Verbs route through
// dispatch; each command reads tokens, makes one or a few Target
// calls, and prints result lines via AddEvent.
//
// Conventions (same as before the extraction):
//   - Addresses parse permissively: "$E000", "0xE000", "E000" all =
//     0xE000.
//   - Byte values parse the same way (8-bit max).
//   - Output that's a query result uses kind="result"; diagnostic /
//     status uses "info"; errors use "err"; advisories use "warn".
//   - Commands are case-insensitive on the verb only; arguments
//     preserve case.
package monitor

import (
	"bytes"
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"

	"github.com/carledwards/go6sim/bridge"
	"github.com/carledwards/go6sim/disasm"
)

// dispatch is the Monitor's single command entry point. The
// submit() path in monitor.go calls it after the user hits Enter.
func (m *Monitor) dispatch(line string) {
	verb, rest := SplitVerb(line)
	switch strings.ToLower(verb) {
	case "":
		return
	case "r", "reg", "registers":
		m.cmdRegisters()
	case "m", "mem":
		m.cmdMem(rest)
	case "d", "dis", "disasm":
		m.cmdDisasm(rest)
	case ":":
		m.cmdPoke(rest)
	case "s", "step":
		m.cmdStep(rest)
	case "g", "go", "run":
		m.cmdGo(rest)
	case ".", "stop":
		m.cmdStop()
	case "reset":
		m.cmdReset()
	case "reconnect":
		m.cmdReconnect()
	case "stack":
		m.cmdStack(rest)
	case "bp":
		m.cmdBPSet(rest)
	case "bc":
		m.cmdBPClear(rest)
	case "bl":
		m.cmdBPList()
	case "cls", "clear":
		m.Clear()
	case "hw", "info":
		m.cmdHardware()
	case "via":
		m.cmdVIA(rest)
	case "f", "fill":
		m.cmdFill(rest)
	case "t", "transfer":
		m.cmdTransfer(rest)
	case "h", "hunt":
		m.cmdHunt(rest)
	case "irq":
		m.cmdIRQ()
	case "nmi":
		m.cmdNMI()
	case "help", "?":
		m.cmdHelp(rest)
	case "q", "quit", "exit":
		m.AddEvent("info", "quit")
		m.host.Quit()
	default:
		m.AddEvent("err", "unknown command: "+verb+"  (try 'help')")
	}
}

// --- CPU + run control ---

func (m *Monitor) cmdRegisters() {
	st, e := m.target.CPUState()
	if e != nil {
		m.AddEvent("err", "cpu.state: "+e.Message)
		return
	}
	m.AddEvent("result",
		fmt.Sprintf("A=$%02X X=$%02X Y=$%02X  SP=$%02X P=$%02X  PC=$%04X",
			st.A, st.X, st.Y, st.SP, st.P, st.PC))
	m.AddEvent("result",
		fmt.Sprintf("flags  %s  (%d half-cycles)", FlagsStr(st.P), st.HalfCycles))
}

// cmdGo — `g` runs from current PC; `g <addr>` sets PC first.
func (m *Monitor) cmdGo(args string) {
	if t := strings.TrimSpace(args); t != "" {
		addr, err := m.resolveAddr(t)
		if err != nil {
			m.AddEvent("err", "g: "+err.Error())
			return
		}
		if _, e := m.target.SetPC(addr); e != nil {
			m.AddEvent("err", "cpu.setPC: "+e.Message)
			return
		}
		m.AddEvent("info", fmt.Sprintf("PC ← $%04X", addr))
	}
	if e := m.target.Run(0); e != nil {
		m.AddEvent("err", "clock.run: "+e.Message)
		return
	}
	m.AddEvent("info", "run")
}

func (m *Monitor) cmdStep(args string) {
	n := 1
	if t := strings.TrimSpace(args); t != "" {
		v, err := strconv.Atoi(t)
		if err != nil || v < 1 {
			m.AddEvent("err", "s: count must be positive integer")
			return
		}
		n = v
	}
	sr, e := m.target.Step(n)
	if e != nil {
		m.AddEvent("err", "clock.step: "+e.Message)
		return
	}
	m.AddEvent("result", fmt.Sprintf("stepped %d → PC=$%04X", n, sr.State.PC))
}

func (m *Monitor) cmdStop() {
	sr, e := m.target.Stop()
	if e != nil {
		m.AddEvent("err", "clock.stop: "+e.Message)
		return
	}
	m.AddEvent("info", "stop ("+sr.Reason+")")
}

func (m *Monitor) cmdReset() {
	st, e := m.target.Reset()
	if e != nil {
		m.AddEvent("err", "machine.reset: "+e.Message)
		return
	}
	m.AddEvent("info", fmt.Sprintf("reset → PC=$%04X  (CPU stopped)", st.PC))
}

func (m *Monitor) cmdReconnect() {
	m.AddEvent("info", "reconnect requested")
	m.host.Reconnect()
}

// --- memory + disasm ---

func (m *Monitor) cmdMem(args string) {
	toks := strings.Fields(args)
	var addr uint16
	if len(toks) < 1 {
		st, e := m.target.CPUState()
		if e != nil {
			m.AddEvent("err", "m: "+e.Message)
			return
		}
		addr = st.PC
	} else {
		a, err := m.resolveAddr(toks[0])
		if err != nil {
			m.AddEvent("err", "m: "+err.Error())
			return
		}
		addr = a
	}
	n := 16
	if len(toks) >= 2 {
		v, err := strconv.Atoi(toks[1])
		if err != nil {
			m.AddEvent("err", "m: count must be decimal: "+toks[1])
			return
		}
		n = v
	}
	if n < 1 {
		n = 1
	}
	if n > 256 {
		n = 256
	}
	buf, e := m.target.MemPeek(addr, n)
	if e != nil {
		m.AddEvent("err", "mem.peek: "+e.Message)
		return
	}
	for _, line := range HexDump(addr, buf) {
		m.AddEvent("result", line)
	}
}

func (m *Monitor) cmdDisasm(args string) {
	toks := strings.Fields(args)
	var addr uint16
	if len(toks) < 1 {
		st, e := m.target.CPUState()
		if e != nil {
			m.AddEvent("err", "d: "+e.Message)
			return
		}
		addr = st.PC
	} else {
		a, err := m.resolveAddr(toks[0])
		if err != nil {
			m.AddEvent("err", "d: "+err.Error())
			return
		}
		addr = a
	}
	n := 10
	if len(toks) >= 2 {
		v, err := strconv.Atoi(toks[1])
		if err != nil || v < 1 {
			m.AddEvent("err", "d: count must be positive integer")
			return
		}
		n = v
	}
	if n > 32 {
		n = 32
	}
	peekN := n * 3
	if peekN > 256 {
		peekN = 256
	}
	buf, e := m.target.MemPeek(addr, peekN)
	if e != nil {
		m.AddEvent("err", "mem.peek: "+e.Message)
		return
	}
	read := func(p uint16) uint8 {
		off := int(p) - int(addr)
		if off < 0 || off >= len(buf) {
			return 0xEA
		}
		return buf[off]
	}
	pc := addr
	for i := 0; i < n; i++ {
		off := int(pc) - int(addr)
		if off >= len(buf) {
			break
		}
		inst := disasm.Decode(pc, read)
		m.AddEvent("result",
			fmt.Sprintf("%04X  %-8s  %s",
				pc, disasm.HexBytes(inst.Bytes), inst.Pretty))
		pc += uint16(inst.Size())
	}
}

func (m *Monitor) cmdPoke(args string) {
	toks := strings.Fields(args)
	if len(toks) < 2 {
		m.AddEvent("err", "usage: : <addr> <byte> [byte…]")
		return
	}
	addr, err := ParseAddr(toks[0])
	if err != nil {
		m.AddEvent("err", ": "+err.Error())
		return
	}
	buf := make([]byte, 0, len(toks)-1)
	for _, t := range toks[1:] {
		v, err := ParseByte(t)
		if err != nil {
			m.AddEvent("err", ": "+err.Error())
			return
		}
		buf = append(buf, v)
	}
	written, e := m.target.MemPoke(addr, buf)
	if e != nil {
		m.AddEvent("err", "mem.poke: "+e.Message)
		return
	}
	m.AddEvent("result", fmt.Sprintf("wrote %d byte(s) @ $%04X", written, addr))
}

// --- range ops: fill / transfer / hunt ---

const rangeBytesCap = 16 * 1024

func (m *Monitor) cmdFill(args string) {
	toks := strings.Fields(args)
	if len(toks) < 3 {
		m.AddEvent("err", "usage: f <start> <end> <byte>")
		return
	}
	start, err := m.resolveAddr(toks[0])
	if err != nil {
		m.AddEvent("err", "f: start: "+err.Error())
		return
	}
	end, err := m.resolveAddr(toks[1])
	if err != nil {
		m.AddEvent("err", "f: end: "+err.Error())
		return
	}
	if end < start {
		m.AddEvent("err", fmt.Sprintf("f: end ($%04X) < start ($%04X)", end, start))
		return
	}
	val, err := ParseByte(toks[2])
	if err != nil {
		m.AddEvent("err", "f: byte: "+err.Error())
		return
	}
	n := int(end) - int(start) + 1
	if n > rangeBytesCap {
		m.AddEvent("err", fmt.Sprintf("f: range %d bytes exceeds cap %d", n, rangeBytesCap))
		return
	}
	buf := bytes.Repeat([]byte{val}, n)
	written, e := m.target.MemPoke(start, buf)
	if e != nil {
		m.AddEvent("err", "mem.poke: "+e.Message)
		return
	}
	m.AddEvent("result",
		fmt.Sprintf("filled $%04X-$%04X with $%02X (%d byte(s))", start, end, val, written))
}

func (m *Monitor) cmdTransfer(args string) {
	toks := strings.Fields(args)
	if len(toks) < 3 {
		m.AddEvent("err", "usage: t <src> <dst> <len>")
		return
	}
	src, err := m.resolveAddr(toks[0])
	if err != nil {
		m.AddEvent("err", "t: src: "+err.Error())
		return
	}
	dst, err := m.resolveAddr(toks[1])
	if err != nil {
		m.AddEvent("err", "t: dst: "+err.Error())
		return
	}
	n, err := strconv.Atoi(toks[2])
	if err != nil || n <= 0 {
		m.AddEvent("err", "t: len must be positive integer (decimal)")
		return
	}
	if n > rangeBytesCap {
		m.AddEvent("err", fmt.Sprintf("t: len %d exceeds cap %d", n, rangeBytesCap))
		return
	}
	buf, e := m.target.MemPeek(src, n)
	if e != nil {
		m.AddEvent("err", "mem.peek: "+e.Message)
		return
	}
	written, e := m.target.MemPoke(dst, buf)
	if e != nil {
		m.AddEvent("err", "mem.poke: "+e.Message)
		return
	}
	m.AddEvent("result",
		fmt.Sprintf("copied $%04X-$%04X → $%04X (%d byte(s))",
			src, src+uint16(n-1), dst, written))
}

func (m *Monitor) cmdHunt(args string) {
	toks := strings.Fields(args)
	if len(toks) < 3 {
		m.AddEvent("err", "usage: h <start> <end> <byte> [byte…]")
		return
	}
	start, err := m.resolveAddr(toks[0])
	if err != nil {
		m.AddEvent("err", "h: start: "+err.Error())
		return
	}
	end, err := m.resolveAddr(toks[1])
	if err != nil {
		m.AddEvent("err", "h: end: "+err.Error())
		return
	}
	if end < start {
		m.AddEvent("err", fmt.Sprintf("h: end ($%04X) < start ($%04X)", end, start))
		return
	}
	pat := make([]byte, 0, len(toks)-2)
	for _, t := range toks[2:] {
		v, err := ParseByte(t)
		if err != nil {
			m.AddEvent("err", "h: pattern: "+err.Error())
			return
		}
		pat = append(pat, v)
	}
	n := int(end) - int(start) + 1
	if n > rangeBytesCap {
		m.AddEvent("err", fmt.Sprintf("h: range %d bytes exceeds cap %d", n, rangeBytesCap))
		return
	}
	buf, e := m.target.MemPeek(start, n)
	if e != nil {
		m.AddEvent("err", "mem.peek: "+e.Message)
		return
	}
	hits := 0
	const maxHits = 32
	for i := 0; i+len(pat) <= len(buf); i++ {
		if bytes.Equal(buf[i:i+len(pat)], pat) {
			hits++
			if hits <= maxHits {
				m.AddEvent("result",
					fmt.Sprintf("  $%04X", start+uint16(i)))
			}
		}
	}
	switch {
	case hits == 0:
		m.AddEvent("result", "(no matches)")
	case hits > maxHits:
		m.AddEvent("result",
			fmt.Sprintf("…and %d more (showing first %d of %d total)",
				hits-maxHits, maxHits, hits))
	default:
		m.AddEvent("result", fmt.Sprintf("(%d match(es))", hits))
	}
}

// --- stack ---

const stackMaxRows = 64

func (m *Monitor) cmdStack(args string) {
	showReturns := strings.EqualFold(strings.TrimSpace(args), "r")

	st, e := m.target.CPUState()
	if e != nil {
		m.AddEvent("err", "stack: "+e.Message)
		return
	}
	sp := st.SP
	if sp == 0xFF {
		m.AddEvent("result", "SP=$FF — stack empty (nothing pushed)")
		return
	}
	start := uint16(0x0100) | uint16(sp+1)
	n := int(0xFF - sp)
	if n > stackMaxRows {
		n = stackMaxRows
		m.AddEvent("info", fmt.Sprintf("(showing top %d bytes only; SP=$%02X)", stackMaxRows, sp))
	}
	buf, pe := m.target.MemPeek(start, n)
	if pe != nil {
		m.AddEvent("err", "mem.peek: "+pe.Message)
		return
	}
	if len(buf) < n {
		m.AddEvent("err", fmt.Sprintf("short stack read: got %d, want %d", len(buf), n))
		return
	}
	m.AddEvent("result",
		fmt.Sprintf("SP=$%02X  ·  stack frame $%04X-$01FF  (%d byte(s))",
			sp, start, n))
	for i, b := range buf {
		addr := start + uint16(i)
		var note string
		switch {
		case i == 0:
			note = "  ← top (last pushed)"
		case showReturns && i%2 == 0 && i+1 < len(buf):
			ret := (uint16(buf[i+1]) << 8) | uint16(b)
			note = fmt.Sprintf("  PCL  → return guess $%04X", ret+1)
		case showReturns && i%2 == 1:
			note = "  PCH"
		}
		m.AddEvent("result", fmt.Sprintf("  $%04X  %02X%s", addr, b, note))
	}
}

// --- breakpoints ---

func (m *Monitor) cmdBPSet(args string) {
	t := strings.TrimSpace(args)
	if t == "" {
		m.AddEvent("err", "usage: bp <addr>")
		return
	}
	addr, err := ParseAddr(t)
	if err != nil {
		m.AddEvent("err", "bp: "+err.Error())
		return
	}
	r, e := m.target.BPSet(addr)
	if e != nil {
		m.AddEvent("err", "bp.set: "+e.Message)
		return
	}
	m.AddEvent("result", fmt.Sprintf("bp %s @ $%04X", r.ID, r.Addr))
}

func (m *Monitor) cmdBPClear(args string) {
	t := strings.ToLower(strings.TrimSpace(args))
	id := ""
	if t != "" && t != "all" {
		id = t
	}
	r, e := m.target.BPClear(id)
	if e != nil {
		m.AddEvent("err", "bp.clear: "+e.Message)
		return
	}
	if id == "" {
		m.AddEvent("result", fmt.Sprintf("cleared %d breakpoint(s)", r.Cleared))
	} else {
		m.AddEvent("result", fmt.Sprintf("cleared %s (%d hit)", id, r.Cleared))
	}
}

func (m *Monitor) cmdBPList() {
	r, e := m.target.BPList()
	if e != nil {
		m.AddEvent("err", "bp.list: "+e.Message)
		return
	}
	if len(r.Breakpoints) == 0 {
		m.AddEvent("result", "(no breakpoints)")
		return
	}
	for _, bp := range r.Breakpoints {
		switch bp.Kind {
		case "addr":
			m.AddEvent("result", fmt.Sprintf("  %s  addr  $%04X", bp.ID, bp.Addr))
		case "vector":
			m.AddEvent("result", fmt.Sprintf("  %s  vector  %s", bp.ID, bp.Vector))
		default:
			m.AddEvent("result", fmt.Sprintf("  %s  %s", bp.ID, bp.Kind))
		}
	}
}

// --- interrupts ---

const flagIMask uint8 = 0x04

func (m *Monitor) cmdIRQ() {
	st, sterr := m.target.CPUState()
	if sterr != nil {
		m.AddEvent("err", "cpu.state: "+sterr.Message)
		return
	}
	if e := m.target.IRQ(); e != nil {
		m.AddEvent("err", "cpu.irq: "+e.Message)
		return
	}
	if st.P&flagIMask != 0 {
		m.AddEvent("info", "IRQ ignored as the I P flag is not cleared")
		return
	}
	m.AddEvent("info", "IRQ pulsed")
}

func (m *Monitor) cmdNMI() {
	if e := m.target.NMI(); e != nil {
		m.AddEvent("err", "cpu.nmi: "+e.Message)
		return
	}
	m.AddEvent("info", "NMI pulsed")
}

// --- hardware describe ---

func (m *Monitor) cmdHardware() {
	m.AddEvent("result", fmt.Sprintf("machine : %s", m.host.MachineLabel()))
	if s := m.host.BuildSummary(); s != "" {
		m.AddEvent("result", fmt.Sprintf("build   : %s", s))
	}
	m.AddEvent("result", "cpu     : MOS 6502")
	if pn := m.host.ProgramName(); pn != "" {
		m.AddEvent("result", fmt.Sprintf("program : %s (%d bytes)", pn, m.host.ProgramSize()))
	}
	regions := m.host.Regions()
	m.AddEvent("result", "regions :")
	if len(regions) == 0 {
		m.AddEvent("result", "  (none reported)")
	} else {
		for _, r := range regions {
			ro := ""
			if r.ReadOnly {
				ro = " (R/O)"
			}
			m.AddEvent("result",
				fmt.Sprintf("  $%04X-$%04X  %s%s", r.Lo, r.Hi, r.Name, ro))
		}
	}
	m.AddEvent("result", "vectors :")
	vecs, e := m.target.MemPeek(0xFFFA, 6)
	if e != nil {
		m.AddEvent("err", "  mem.peek $FFFA: "+e.Message)
		return
	}
	if len(vecs) < 6 {
		m.AddEvent("err", fmt.Sprintf("  got %d bytes from $FFFA, expected 6", len(vecs)))
		return
	}
	nmi := uint16(vecs[0]) | uint16(vecs[1])<<8
	reset := uint16(vecs[2]) | uint16(vecs[3])<<8
	irq := uint16(vecs[4]) | uint16(vecs[5])<<8
	m.AddEvent("result", fmt.Sprintf("  $FFFA NMI       → $%04X", nmi))
	m.AddEvent("result", fmt.Sprintf("  $FFFC RESET     → $%04X", reset))
	m.AddEvent("result", fmt.Sprintf("  $FFFE IRQ / BRK → $%04X", irq))
}

// --- VIA subcommand tree (full version migrated from old commands.go) ---

func (m *Monitor) cmdVIA(args string) {
	toks := strings.Fields(args)
	if len(toks) == 0 {
		m.cmdVIAHelp()
		return
	}
	switch strings.ToLower(toks[0]) {
	case "help", "?":
		m.cmdVIAHelp()
	case "list":
		m.cmdVIAList()
	case "dump":
		m.cmdVIADump(strings.Join(toks[1:], " "))
	case "set":
		m.cmdVIASet(toks[1:])
	default:
		m.AddEvent("err",
			fmt.Sprintf("via: unknown subcommand %q  (try 'via help')", toks[0]))
	}
}

func (m *Monitor) cmdVIAHelp() {
	lines := []string{
		"via subcommands:",
		"  via list                   enumerate VIAs available on this machine",
		"  via dump [name|addr]       hex dump of 16 registers with flag decode",
		"  via set [name] <pin> <v>   drive a pin or write a whole port",
		"",
		"  per-bit forms:",
		"    PA0..PA7, PB0..PB7         ORA / ORB latch bit",
		"    DDRA0..DDRA7, DDRB0..DDRB7 DDRA / DDRB direction bit",
		"    value: 0 / 1 / high / low / h / l       (output pins)",
		"    value: 0 / 1 / out / in / output / input (DDR pins)",
		"",
		"  whole-port forms:",
		"    PA / PB                     write whole ORA / ORB latch",
		"    DDRA / DDRB                 write whole DDRA / DDRB direction",
		"    value: any hex byte ($FF, FF, 0xFF — all equivalent)",
		"",
		"  name      region name (e.g. 'via1') — required when >1 VIA present",
		"  addr      hex base address (e.g. $B000) — alternative to name",
		"",
		"examples:",
		"  via dump                   dump the only VIA (auto)",
		"  via dump via2              dump the named VIA",
		"  via set DDRB FF            configure ALL of Port B as output",
		"  via set DDRB0 out          configure PB0 as output (DDRB bit 0 = 1)",
		"  via set PB AA              write $AA to ORB (alternating bits)",
		"  via set PB0 high           drive PB0 high  (ORB bit 0 = 1)",
		"  via set via2 PA 0          zero ORA on via2",
	}
	for _, l := range lines {
		m.AddEvent("result", l)
	}
}

func (m *Monitor) cmdVIAList() {
	vias := m.viaRegions()
	if len(vias) == 0 {
		m.AddEvent("err", "no VIA regions reported")
		return
	}
	m.AddEvent("result", fmt.Sprintf("%d VIA(s) present:", len(vias)))
	for _, r := range vias {
		m.AddEvent("result", fmt.Sprintf("  %-8s  $%04X-$%04X", r.Name, r.Lo, r.Hi))
	}
}

func (m *Monitor) cmdVIADump(arg string) {
	base, err := m.resolveVIABase(arg)
	if err != nil {
		m.AddEvent("err", "via dump: "+err.Error())
		return
	}
	buf, e := m.target.MemPeek(base, 16)
	if e != nil {
		m.AddEvent("err", "mem.peek: "+e.Message)
		return
	}
	if len(buf) < 16 {
		m.AddEvent("err", fmt.Sprintf("via dump: got %d bytes, expected 16", len(buf)))
		return
	}
	names := []string{
		"ORB/IRB ", "ORA/IRA ", "DDRB    ", "DDRA    ",
		"T1C-L   ", "T1C-H   ", "T1L-L   ", "T1L-H   ",
		"T2C-L   ", "T2C-H   ", "SR      ", "ACR     ",
		"PCR     ", "IFR     ", "IER     ", "ORA-nh  ",
	}
	m.AddEvent("result", fmt.Sprintf("VIA @ $%04X:", base))
	for i, n := range names {
		m.AddEvent("result",
			fmt.Sprintf("  $%04X  %s  $%02X  %s",
				base+uint16(i), n, buf[i], viaBitsHint(i, buf[i])))
	}
}

func (m *Monitor) cmdVIASet(toks []string) {
	if len(toks) < 2 {
		m.AddEvent("err", "usage: via set [name] <pin> <value>")
		return
	}
	var nameArg, pinArg, bitArg string
	if _, _, _, perr := ParseVIAPin(toks[0]); perr == nil {
		pinArg = toks[0]
		bitArg = toks[1]
	} else {
		if len(toks) < 3 {
			m.AddEvent("err", "usage: via set <name> <pin> <value>")
			return
		}
		nameArg = toks[0]
		pinArg = toks[1]
		bitArg = toks[2]
	}
	base, err := m.resolveVIABase(nameArg)
	if err != nil {
		m.AddEvent("err", "via set: "+err.Error())
		return
	}
	kind, port, pinBit, err := ParseVIAPin(pinArg)
	if err != nil {
		m.AddEvent("err", "via set: "+err.Error())
		return
	}
	switch kind {
	case VIAPortOutput:
		val, perr := ParseByte(bitArg)
		if perr != nil {
			m.AddEvent("err", "via set: "+perr.Error())
			return
		}
		m.viaSetOutputByte(base, port, val)
	case VIAPortDDR:
		val, perr := ParseByte(bitArg)
		if perr != nil {
			m.AddEvent("err", "via set: "+perr.Error())
			return
		}
		m.viaSetDDRByte(base, port, val)
	case VIAPinDDR:
		bitVal, perr := ParseVIABitValue(bitArg, kind)
		if perr != nil {
			m.AddEvent("err", "via set: "+perr.Error())
			return
		}
		m.viaSetDDRBit(base, port, pinBit, bitVal)
	case VIAPinOutput:
		bitVal, perr := ParseVIABitValue(bitArg, kind)
		if perr != nil {
			m.AddEvent("err", "via set: "+perr.Error())
			return
		}
		m.viaSetOutputBit(base, port, pinBit, bitVal)
	}
}

func (m *Monitor) viaSetDDRBit(base uint16, port byte, pinBit, bitVal int) {
	var ddrRegOff uint16
	switch port {
	case 'B':
		ddrRegOff = 2
	case 'A':
		ddrRegOff = 3
	}
	ddrAddr := base + ddrRegOff
	mask := byte(1 << pinBit)
	cur, e := m.target.MemPeek(ddrAddr, 1)
	if e != nil {
		m.AddEvent("err", "mem.peek DDR: "+e.Message)
		return
	}
	if len(cur) < 1 {
		m.AddEvent("err", "via set DDR: short read")
		return
	}
	next := cur[0]
	if bitVal == 1 {
		next |= mask
	} else {
		next &^= mask
	}
	if _, e := m.target.MemPoke(ddrAddr, []byte{next}); e != nil {
		m.AddEvent("err", "mem.poke DDR: "+e.Message)
		return
	}
	dir := "input"
	if bitVal == 1 {
		dir = "output"
	}
	m.AddEvent("result",
		fmt.Sprintf("DDR%c bit %d ← %s  ($%04X: $%02X → $%02X)",
			port, pinBit, dir, ddrAddr, cur[0], next))
}

func (m *Monitor) viaSetOutputBit(base uint16, port byte, pinBit, bitVal int) {
	var orRegOff, ddrRegOff uint16
	switch port {
	case 'B':
		orRegOff, ddrRegOff = 0, 2
	case 'A':
		orRegOff, ddrRegOff = 1, 3
	}
	orAddr := base + orRegOff
	ddrAddr := base + ddrRegOff
	mask := byte(1 << pinBit)

	ddr, e := m.target.MemPeek(ddrAddr, 1)
	if e != nil {
		m.AddEvent("err", "mem.peek DDR: "+e.Message)
		return
	}
	if len(ddr) < 1 {
		m.AddEvent("err", "via set: short read on DDR")
		return
	}
	if ddr[0]&mask == 0 {
		m.AddEvent("err",
			fmt.Sprintf("via set: P%c%d is input in DDR%c=$%02X — flip the direction first:  via set DDR%c%d out",
				port, pinBit, port, ddr[0], port, pinBit))
		return
	}
	cur, e := m.target.MemPeek(orAddr, 1)
	if e != nil {
		m.AddEvent("err", "mem.peek OR: "+e.Message)
		return
	}
	if len(cur) < 1 {
		m.AddEvent("err", "via set: short read on OR")
		return
	}
	next := cur[0]
	if bitVal == 1 {
		next |= mask
	} else {
		next &^= mask
	}
	if _, e := m.target.MemPoke(orAddr, []byte{next}); e != nil {
		m.AddEvent("err", "mem.poke OR: "+e.Message)
		return
	}
	m.AddEvent("result",
		fmt.Sprintf("P%c%d ← %d  (OR%c $%04X: $%02X → $%02X)",
			port, pinBit, bitVal, port, orAddr, cur[0], next))
}

func (m *Monitor) viaSetDDRByte(base uint16, port byte, val byte) {
	var ddrRegOff uint16
	switch port {
	case 'B':
		ddrRegOff = 2
	case 'A':
		ddrRegOff = 3
	}
	addr := base + ddrRegOff
	cur, e := m.target.MemPeek(addr, 1)
	if e != nil {
		m.AddEvent("err", "mem.peek DDR: "+e.Message)
		return
	}
	if _, e := m.target.MemPoke(addr, []byte{val}); e != nil {
		m.AddEvent("err", "mem.poke DDR: "+e.Message)
		return
	}
	outs := bits.OnesCount8(val)
	m.AddEvent("result",
		fmt.Sprintf("DDR%c ← $%02X  ($%04X: $%02X → $%02X)  %d output, %d input",
			port, val, addr, cur[0], val, outs, 8-outs))
}

func (m *Monitor) viaSetOutputByte(base uint16, port byte, val byte) {
	var orRegOff, ddrRegOff uint16
	switch port {
	case 'B':
		orRegOff, ddrRegOff = 0, 2
	case 'A':
		orRegOff, ddrRegOff = 1, 3
	}
	orAddr := base + orRegOff
	ddrAddr := base + ddrRegOff
	ddr, e := m.target.MemPeek(ddrAddr, 1)
	if e != nil {
		m.AddEvent("err", "mem.peek DDR: "+e.Message)
		return
	}
	cur, e := m.target.MemPeek(orAddr, 1)
	if e != nil {
		m.AddEvent("err", "mem.peek OR: "+e.Message)
		return
	}
	if _, e := m.target.MemPoke(orAddr, []byte{val}); e != nil {
		m.AddEvent("err", "mem.poke OR: "+e.Message)
		return
	}
	m.AddEvent("result",
		fmt.Sprintf("OR%c ← $%02X  ($%04X: $%02X → $%02X)",
			port, val, orAddr, cur[0], val))
	inputMask := byte(^ddr[0])
	if inputMask != 0 {
		m.AddEvent("info",
			fmt.Sprintf("DDR%c=$%02X — %d bit(s) configured as input; their OR latch value is not driven on the pin",
				port, ddr[0], bits.OnesCount8(inputMask)))
	}
}

func (m *Monitor) viaRegions() []bridge.Region {
	var out []bridge.Region
	for _, r := range m.host.Regions() {
		if strings.HasPrefix(strings.ToLower(r.Name), "via") {
			out = append(out, r)
		}
	}
	return out
}

func (m *Monitor) resolveVIABase(arg string) (uint16, error) {
	arg = strings.TrimSpace(arg)
	vias := m.viaRegions()
	if arg == "" {
		switch len(vias) {
		case 0:
			return 0, errors.New("no VIA regions on this machine")
		case 1:
			return vias[0].Lo, nil
		default:
			names := make([]string, 0, len(vias))
			for _, r := range vias {
				names = append(names, fmt.Sprintf("%s ($%04X)", r.Name, r.Lo))
			}
			return 0, fmt.Errorf("multiple VIAs present (%s); specify name: 'via dump %s'",
				strings.Join(names, ", "), vias[0].Name)
		}
	}
	for _, r := range vias {
		if strings.EqualFold(r.Name, arg) {
			return r.Lo, nil
		}
	}
	if v, err := ParseAddr(arg); err == nil {
		return v, nil
	}
	return 0, fmt.Errorf("unknown VIA %q (run 'via list')", arg)
}

func viaBitsHint(reg int, v byte) string {
	switch reg {
	case 11:
		t1mode := []string{"timed (one-shot)", "free-run", "timed + PB7", "free-run + PB7"}
		return "T1=" + t1mode[(v>>6)&3]
	case 13:
		b := decodeBits(v, []string{"CA2", "CA1", "SR", "CB2", "CB1", "T2", "T1", "IRQ"})
		if b == "" {
			return "(no flags)"
		}
		return "set: " + b
	case 14:
		b := decodeBits(v&0x7F, []string{"CA2", "CA1", "SR", "CB2", "CB1", "T2", "T1"})
		if b == "" {
			return "(none enabled)"
		}
		return "enabled: " + b
	}
	return ""
}

func decodeBits(v byte, names []string) string {
	var out []string
	for i, n := range names {
		if v&(1<<i) != 0 {
			out = append(out, n)
		}
	}
	return strings.Join(out, " ")
}

// --- symbolic address resolution shared by m / d / g / f / t / h ---

func (m *Monitor) resolveAddr(s string) (uint16, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	switch t {
	case "pc":
		st, e := m.target.CPUState()
		if e != nil {
			return 0, fmt.Errorf("cpu.state: %s", e.Message)
		}
		return st.PC, nil
	case "sp":
		st, e := m.target.CPUState()
		if e != nil {
			return 0, fmt.Errorf("cpu.state: %s", e.Message)
		}
		return 0x0100 | uint16(st.SP), nil
	case "reset":
		return m.readVec(0xFFFC)
	case "irq", "brk":
		return m.readVec(0xFFFE)
	case "nmi":
		return m.readVec(0xFFFA)
	}
	return ParseAddr(s)
}

func (m *Monitor) readVec(addr uint16) (uint16, error) {
	buf, e := m.target.MemPeek(addr, 2)
	if e != nil {
		return 0, fmt.Errorf("mem.peek $%04X: %s", addr, e.Message)
	}
	if len(buf) < 2 {
		return 0, fmt.Errorf("short vector read at $%04X", addr)
	}
	return uint16(buf[0]) | uint16(buf[1])<<8, nil
}

// --- help ---

func (m *Monitor) cmdHelp(args string) {
	args = strings.ToLower(strings.TrimSpace(args))
	switch args {
	case "":
		for _, line := range HelpTable {
			m.AddEvent("result", line)
		}
		return
	case "window", "w", "win":
		m.host.OpenHelpWindow()
		m.AddEvent("info", "opened help window")
		return
	}
	if d, ok := HelpDetail[args]; ok {
		for _, line := range d {
			m.AddEvent("result", line)
		}
		return
	}
	m.AddEvent("err", "no help for: "+args)
}
