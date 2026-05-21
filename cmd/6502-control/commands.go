// Command dispatcher for the 6502-control Monitor REPL. Each command
// reads tokens, calls one bridge method (or a small composition),
// and prints zero-or-more event lines via Controller.addEvent.
//
// Conventions:
//   - Addresses parse permissively: "$E000", "0xE000", "E000" all =
//     0xE000. Decimal addresses are not supported — the 6502 idiom
//     is hex throughout.
//   - Byte values parse the same way (8-bit max).
//   - Output that's meant as the result of a query uses kind="result";
//     diagnostic / status uses kind="info"; errors use kind="err".
//   - Commands are case-insensitive on the verb only; arguments
//     preserve case (filenames may show up here later).
package main

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/carledwards/go6sim/bridge"
	"github.com/carledwards/go6sim/disasm"
)

// dispatch is the single Monitor entry point. It parses `line`,
// switches on the verb, and calls the matching handler.
func (c *Controller) dispatch(line string) {
	verb, rest := splitVerb(line)
	switch strings.ToLower(verb) {
	case "":
		// Empty submission already filtered in monitorProvider; defensive.
		return
	case "r", "reg", "registers":
		c.cmdRegisters()
	case "m", "mem":
		c.cmdMem(rest)
	case "d", "dis", "disasm":
		c.cmdDisasm(rest)
	case ":":
		c.cmdPoke(rest)
	case "s", "step":
		c.cmdStep(rest)
	case "g", "go", "run":
		c.cmdGo(rest)
	case ".", "stop":
		c.cmdStopMonitor()
	case "reset":
		c.reset() // reuses the hotkey-bound action
	case "reconnect":
		c.cmdReconnect()
	case "bp":
		c.cmdBPSet(rest)
	case "bc":
		c.cmdBPClear(rest)
	case "bl":
		c.cmdBPList()
	case "cls", "clear":
		c.clearEvents()
	case "hw", "info":
		c.cmdHardware()
	case "via":
		c.cmdVIA(rest)
	case "f", "fill":
		c.cmdFill(rest)
	case "t", "transfer":
		c.cmdTransfer(rest)
	case "h", "hunt":
		c.cmdHunt(rest)
	case "irq":
		c.cmdIRQ()
	case "nmi":
		c.cmdNMI()
	case "help", "?":
		c.cmdHelp(rest)
	case "q", "quit", "exit":
		// Dispatch runs on the foxpro goroutine; emit a result line
		// and let main shut down through the normal Quit signal once
		// we route through it. For now, exit the process.
		c.addEvent("info", "quit")
		// The app object isn't reachable from Controller; use a
		// process exit. Slice 3 will plumb a Quit closure through
		// once the menu bar lands.
		quitCh := c.quitSignal
		if quitCh != nil {
			select {
			case quitCh <- struct{}{}:
			default:
			}
		}
	default:
		c.addEvent("err", "unknown command: "+verb+"  (try 'help')")
	}
}

// splitVerb returns the first whitespace-separated token and the rest
// of the line, both trimmed. The leading `:` operator is special-cased
// because users write it as `: $E000 A9 2A` with a space, but also
// because it isn't a normal identifier so the default tokenizer would
// behave oddly if we ever extended it.
func splitVerb(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", ""
	}
	// Single-char operators that don't need a separator.
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

// --- commands ---

func (c *Controller) cmdRegisters() {
	st, e := c.client.Load().CPUState()
	if e != nil {
		c.addEvent("err", "cpu.state: "+e.Message)
		return
	}
	c.addEvent("result",
		fmt.Sprintf("A=$%02X X=$%02X Y=$%02X  SP=$%02X P=$%02X  PC=$%04X",
			st.A, st.X, st.Y, st.SP, st.P, st.PC))
	c.addEvent("result",
		fmt.Sprintf("flags  %s  (%d half-cycles)", flagsStr(st.P), st.HalfCycles))
}

func (c *Controller) cmdMem(args string) {
	toks := strings.Fields(args)
	var addr uint16
	if len(toks) < 1 {
		st, e := c.client.Load().CPUState()
		if e != nil {
			c.addEvent("err", "m: "+e.Message)
			return
		}
		addr = st.PC
	} else {
		a, err := c.resolveAddr(toks[0])
		if err != nil {
			c.addEvent("err", "m: "+err.Error())
			return
		}
		addr = a
	}
	n := 16
	if len(toks) >= 2 {
		v, err := strconv.Atoi(toks[1])
		if err != nil {
			c.addEvent("err", "m: count must be decimal: "+toks[1])
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
	buf, e := c.client.Load().MemPeek(addr, n)
	if e != nil {
		c.addEvent("err", "mem.peek: "+e.Message)
		return
	}
	for _, line := range hexDump(addr, buf) {
		c.addEvent("result", line)
	}
}

// cmdDisasm decodes `count` instructions starting at `addr` and prints
// them HESMON-style. We peek enough bytes up front (up to 3× count,
// capped at 256) to cover the worst case where every instruction is
// 3 bytes; the disasm.Decode loop then walks the buffer.
//
// With no addr argument, defaults to the live PC — the "where am I
// in the code?" shortcut.
func (c *Controller) cmdDisasm(args string) {
	toks := strings.Fields(args)
	var addr uint16
	if len(toks) < 1 {
		st, e := c.client.Load().CPUState()
		if e != nil {
			c.addEvent("err", "d: "+e.Message)
			return
		}
		addr = st.PC
	} else {
		a, err := c.resolveAddr(toks[0])
		if err != nil {
			c.addEvent("err", "d: "+err.Error())
			return
		}
		addr = a
	}
	n := 10
	if len(toks) >= 2 {
		v, err := strconv.Atoi(toks[1])
		if err != nil || v < 1 {
			c.addEvent("err", "d: count must be positive integer")
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
	buf, e := c.client.Load().MemPeek(addr, peekN)
	if e != nil {
		c.addEvent("err", "mem.peek: "+e.Message)
		return
	}

	// read closure: returns $EA (NOP) for any byte outside the peeked
	// buffer; that's only reached if the operand of a 3-byte instr
	// pokes past the buffer end. Caller stops the loop before then.
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
		c.addEvent("result",
			fmt.Sprintf("%04X  %-8s  %s",
				pc, disasm.HexBytes(inst.Bytes), inst.Pretty))
		pc += uint16(inst.Size())
	}
}

func (c *Controller) cmdPoke(args string) {
	toks := strings.Fields(args)
	if len(toks) < 2 {
		c.addEvent("err", "usage: : <addr> <byte> [byte…]")
		return
	}
	addr, err := parseAddr(toks[0])
	if err != nil {
		c.addEvent("err", ": "+err.Error())
		return
	}
	buf := make([]byte, 0, len(toks)-1)
	for _, t := range toks[1:] {
		v, err := parseByte(t)
		if err != nil {
			c.addEvent("err", ": "+err.Error())
			return
		}
		buf = append(buf, v)
	}
	written, e := c.client.Load().MemPoke(addr, buf)
	if e != nil {
		c.addEvent("err", "mem.poke: "+e.Message)
		return
	}
	c.addEvent("result", fmt.Sprintf("wrote %d byte(s) @ $%04X", written, addr))
}

func (c *Controller) cmdStep(args string) {
	n := 1
	if t := strings.TrimSpace(args); t != "" {
		v, err := strconv.Atoi(t)
		if err != nil || v < 1 {
			c.addEvent("err", "s: count must be positive integer")
			return
		}
		n = v
	}
	sr, e := c.client.Load().Step(n)
	if e != nil {
		c.addEvent("err", "clock.step: "+e.Message)
		return
	}
	c.setState(sr.State)
	c.addEvent("result", fmt.Sprintf("stepped %d → PC=$%04X", n, sr.State.PC))
}

// cmdGo — `g` runs from the current PC; `g <addr>` sets PC first
// then runs. The addr accepts the same hex / symbolic forms as the
// `m` / `d` commands (so `g reset` is a common reboot-the-program
// shortcut).
func (c *Controller) cmdGo(args string) {
	if t := strings.TrimSpace(args); t != "" {
		addr, err := c.resolveAddr(t)
		if err != nil {
			c.addEvent("err", "g: "+err.Error())
			return
		}
		if _, e := c.client.Load().SetPC(addr); e != nil {
			c.addEvent("err", "cpu.setPC: "+e.Message)
			return
		}
		c.addEvent("info", fmt.Sprintf("PC ← $%04X", addr))
	}
	if e := c.client.Load().Run(0); e != nil {
		c.addEvent("err", "clock.run: "+e.Message)
		return
	}
	c.mu.Lock()
	c.running = true
	c.lastHalt = ""
	c.mu.Unlock()
	c.addEvent("info", "run")
}

// cmdBPSet — `bp <addr>`. Adds an address breakpoint.
func (c *Controller) cmdBPSet(args string) {
	t := strings.TrimSpace(args)
	if t == "" {
		c.addEvent("err", "usage: bp <addr>")
		return
	}
	addr, err := parseAddr(t)
	if err != nil {
		c.addEvent("err", "bp: "+err.Error())
		return
	}
	r, e := c.client.Load().BPSet(addr)
	if e != nil {
		c.addEvent("err", "bp.set: "+e.Message)
		return
	}
	c.addEvent("result", fmt.Sprintf("bp %s @ $%04X", r.ID, r.Addr))
}

// cmdBPClear — `bc <id>` clears one, `bc` or `bc all` clears all.
func (c *Controller) cmdBPClear(args string) {
	t := strings.ToLower(strings.TrimSpace(args))
	id := ""
	if t != "" && t != "all" {
		id = t
	}
	r, e := c.client.Load().BPClear(id)
	if e != nil {
		c.addEvent("err", "bp.clear: "+e.Message)
		return
	}
	switch {
	case id == "":
		c.addEvent("result", fmt.Sprintf("cleared %d breakpoint(s)", r.Cleared))
	default:
		c.addEvent("result", fmt.Sprintf("cleared %s (%d hit)", id, r.Cleared))
	}
}

// cmdBPList — `bl`. Dumps every armed breakpoint.
// flagIMask is bit 2 of P — the 6502 interrupt-disable flag. Set on
// reset and by SEI/BRK; cleared by CLI/RTI. When set, IRQ is masked.
const flagIMask uint8 = 0x04

// cmdIRQ pulses the host-driven IRQ line. The CPU services on the
// next instruction boundary IF the I flag is clear; if not, the
// pulse is silently ignored. We sample CPU state first so we can
// tell the user *why* nothing happened — silent "pulsed" lines while
// nothing visibly moves is the wrong UX.
func (c *Controller) cmdIRQ() {
	st, sterr := c.client.Load().CPUState()
	if sterr != nil {
		c.addEvent("err", "cpu.state: "+sterr.Message)
		return
	}
	if e := c.client.Load().IRQ(); e != nil {
		c.addEvent("err", "cpu.irq: "+e.Message)
		return
	}
	if st.P&flagIMask != 0 {
		c.addEvent("info", "IRQ ignored as the I P flag is not cleared")
		return
	}
	c.addEvent("info", "IRQ pulsed")
}

// cmdNMI pulses the host-driven NMI line. Edge-triggered, NOT masked
// by I flag, vectors through $FFFA. In interp CPU mode this fires
// reliably; netsim mode currently a no-op (upstream lacks SetNMI).
func (c *Controller) cmdNMI() {
	if e := c.client.Load().NMI(); e != nil {
		c.addEvent("err", "cpu.nmi: "+e.Message)
		return
	}
	c.addEvent("info", "NMI pulsed")
}

// rangeBytesCap bounds how many bytes a single Fill / Transfer /
// Hunt invocation may touch. Generous enough for any normal debugger
// use, low enough to keep a single bridge call snappy and avoid
// pathological allocations on the server side.
const rangeBytesCap = 16 * 1024

// cmdFill — `f <start> <end> <byte>`. Writes `byte` to every address
// in the inclusive range [start..end]. Uses a single mem.poke with a
// pre-filled buffer rather than N round trips — wire latency stays
// constant regardless of range size.
func (c *Controller) cmdFill(args string) {
	toks := strings.Fields(args)
	if len(toks) < 3 {
		c.addEvent("err", "usage: f <start> <end> <byte>")
		return
	}
	start, err := c.resolveAddr(toks[0])
	if err != nil {
		c.addEvent("err", "f: start: "+err.Error())
		return
	}
	end, err := c.resolveAddr(toks[1])
	if err != nil {
		c.addEvent("err", "f: end: "+err.Error())
		return
	}
	if end < start {
		c.addEvent("err", fmt.Sprintf("f: end ($%04X) < start ($%04X)", end, start))
		return
	}
	val, err := parseByte(toks[2])
	if err != nil {
		c.addEvent("err", "f: byte: "+err.Error())
		return
	}
	n := int(end) - int(start) + 1
	if n > rangeBytesCap {
		c.addEvent("err", fmt.Sprintf("f: range %d bytes exceeds cap %d", n, rangeBytesCap))
		return
	}
	buf := bytes.Repeat([]byte{val}, n)
	written, e := c.client.Load().MemPoke(start, buf)
	if e != nil {
		c.addEvent("err", "mem.poke: "+e.Message)
		return
	}
	c.addEvent("result",
		fmt.Sprintf("filled $%04X-$%04X with $%02X (%d byte(s))", start, end, val, written))
}

// cmdTransfer — `t <src> <dst> <len>`. Copies `len` bytes from
// `src` to `dst`. Peek + poke; src and dst may overlap (forward
// copy semantics from the peeked buffer, so overlapping copies do
// what a memcpy does, not memmove).
func (c *Controller) cmdTransfer(args string) {
	toks := strings.Fields(args)
	if len(toks) < 3 {
		c.addEvent("err", "usage: t <src> <dst> <len>")
		return
	}
	src, err := c.resolveAddr(toks[0])
	if err != nil {
		c.addEvent("err", "t: src: "+err.Error())
		return
	}
	dst, err := c.resolveAddr(toks[1])
	if err != nil {
		c.addEvent("err", "t: dst: "+err.Error())
		return
	}
	n, err := strconv.Atoi(toks[2])
	if err != nil || n <= 0 {
		c.addEvent("err", "t: len must be positive integer (decimal)")
		return
	}
	if n > rangeBytesCap {
		c.addEvent("err", fmt.Sprintf("t: len %d exceeds cap %d", n, rangeBytesCap))
		return
	}
	buf, e := c.client.Load().MemPeek(src, n)
	if e != nil {
		c.addEvent("err", "mem.peek: "+e.Message)
		return
	}
	written, e := c.client.Load().MemPoke(dst, buf)
	if e != nil {
		c.addEvent("err", "mem.poke: "+e.Message)
		return
	}
	c.addEvent("result",
		fmt.Sprintf("copied $%04X-$%04X → $%04X (%d byte(s))",
			src, src+uint16(n-1), dst, written))
}

// cmdHunt — `h <start> <end> <b1> [b2] [b3] ...`. Searches the
// inclusive range for the byte sequence and prints every match
// address. Pattern can be one byte (find all occurrences of a value)
// or many (find all occurrences of a contiguous sequence).
func (c *Controller) cmdHunt(args string) {
	toks := strings.Fields(args)
	if len(toks) < 3 {
		c.addEvent("err", "usage: h <start> <end> <byte> [byte…]")
		return
	}
	start, err := c.resolveAddr(toks[0])
	if err != nil {
		c.addEvent("err", "h: start: "+err.Error())
		return
	}
	end, err := c.resolveAddr(toks[1])
	if err != nil {
		c.addEvent("err", "h: end: "+err.Error())
		return
	}
	if end < start {
		c.addEvent("err", fmt.Sprintf("h: end ($%04X) < start ($%04X)", end, start))
		return
	}
	pat := make([]byte, 0, len(toks)-2)
	for _, t := range toks[2:] {
		v, err := parseByte(t)
		if err != nil {
			c.addEvent("err", "h: pattern: "+err.Error())
			return
		}
		pat = append(pat, v)
	}
	n := int(end) - int(start) + 1
	if n > rangeBytesCap {
		c.addEvent("err", fmt.Sprintf("h: range %d bytes exceeds cap %d", n, rangeBytesCap))
		return
	}
	buf, e := c.client.Load().MemPeek(start, n)
	if e != nil {
		c.addEvent("err", "mem.peek: "+e.Message)
		return
	}
	hits := 0
	const maxHits = 32 // don't drown the scrollback
	for i := 0; i+len(pat) <= len(buf); i++ {
		if bytes.Equal(buf[i:i+len(pat)], pat) {
			hits++
			if hits <= maxHits {
				c.addEvent("result",
					fmt.Sprintf("  $%04X", start+uint16(i)))
			}
		}
	}
	switch {
	case hits == 0:
		c.addEvent("result", "(no matches)")
	case hits > maxHits:
		c.addEvent("result",
			fmt.Sprintf("…and %d more (showing first %d of %d total)",
				hits-maxHits, maxHits, hits))
	default:
		c.addEvent("result", fmt.Sprintf("(%d match(es))", hits))
	}
}

// cmdReconnect forces an immediate reconnect attempt. Useful when
// the user knows the sim came back and doesn't want to wait for the
// next backoff tick. Closes the current client, which trips the heal
// loop's Done() detection — the heal loop then dials, hellos,
// machine.loads, and resubscribes.
func (c *Controller) cmdReconnect() {
	st := c.connStatus()
	if st == "connected" {
		c.addEvent("info", "reconnect: closing current connection to force a refresh")
	} else {
		c.addEvent("info", fmt.Sprintf("reconnect: status was %s; retrying", st))
	}
	c.reconnectNow()
}

// cmdHardware — `hw` / `info`. Dumps the machine label, summary,
// program info, every region the server reported, and the three 6502
// vectors (NMI / RESET / IRQ-BRK) read live from $FFFA-$FFFF. This is
// the "what am I looking at?" command — useful when attaching to a
// sim and not remembering which preset is loaded.
func (c *Controller) cmdHardware() {
	c.addEvent("result", fmt.Sprintf("machine : %s", c.machineLabel))
	if c.buildSummary != "" {
		c.addEvent("result", fmt.Sprintf("build   : %s", c.buildSummary))
	}
	c.addEvent("result", "cpu     : MOS 6502")
	if c.programName != "" {
		c.addEvent("result", fmt.Sprintf("program : %s (%d bytes)", c.programName, c.programSize))
	}
	c.addEvent("result", "regions :")
	if len(c.regions) == 0 {
		c.addEvent("result", "  (none reported by server)")
	} else {
		for _, r := range c.regions {
			ro := ""
			if r.ReadOnly {
				ro = " (R/O)"
			}
			c.addEvent("result",
				fmt.Sprintf("  $%04X-$%04X  %s%s", r.Lo, r.Hi, r.Name, ro))
		}
	}

	// 6502 vectors live in the last 6 bytes of address space. Reading
	// through the bus picks up whatever ROM (or trapped RAM) currently
	// answers — clients see the same map the CPU does on reset/IRQ/NMI.
	// Little-endian: low byte at lower address.
	c.addEvent("result", "vectors :")
	vecs, e := c.client.Load().MemPeek(0xFFFA, 6)
	if e != nil {
		c.addEvent("err", "  mem.peek $FFFA: "+e.Message)
		return
	}
	if len(vecs) < 6 {
		c.addEvent("err", fmt.Sprintf("  got %d bytes from $FFFA, expected 6", len(vecs)))
		return
	}
	nmi := uint16(vecs[0]) | uint16(vecs[1])<<8
	reset := uint16(vecs[2]) | uint16(vecs[3])<<8
	irq := uint16(vecs[4]) | uint16(vecs[5])<<8
	c.addEvent("result", fmt.Sprintf("  $FFFA NMI       → $%04X", nmi))
	c.addEvent("result", fmt.Sprintf("  $FFFC RESET     → $%04X", reset))
	c.addEvent("result", fmt.Sprintf("  $FFFE IRQ / BRK → $%04X", irq))
}

// cmdVIA is the top-level dispatcher for the via subcommand tree.
// Subcommands keep the global namespace clean and group all VIA-
// specific knobs (dump, set, list, help) under one verb.
//
// Shape:
//   via                    show subcommand help
//   via help / via ?       same
//   via list               enumerate VIAs from machine.load regions
//   via dump [name|addr]   16-register dump with flag decode
//   via set [name] <pin> <bit>   drive Port A/B pin high or low
//
// Where there's only one VIA, [name] is optional and auto-resolves.
// Where there are multiple, omitting the name surfaces an error
// listing the candidates rather than picking silently.
func (c *Controller) cmdVIA(args string) {
	toks := strings.Fields(args)
	if len(toks) == 0 {
		c.cmdVIAHelp()
		return
	}
	switch strings.ToLower(toks[0]) {
	case "help", "?":
		c.cmdVIAHelp()
	case "list":
		c.cmdVIAList()
	case "dump":
		c.cmdVIADump(strings.Join(toks[1:], " "))
	case "set":
		c.cmdVIASet(toks[1:])
	default:
		c.addEvent("err",
			fmt.Sprintf("via: unknown subcommand %q  (try 'via help')", toks[0]))
	}
}

// cmdVIAHelp prints the via subcommand list inline (cheaper than an
// About-style dialog; matches how the top-level `help` works).
func (c *Controller) cmdVIAHelp() {
	lines := []string{
		"via subcommands:",
		"  via list                 enumerate VIAs available on this machine",
		"  via dump [name|addr]     hex dump of 16 registers with flag decode",
		"  via set [name] <pin> <b> drive a pin output high or low",
		"",
		"  pin       PB0..PB7 (Port B) or PA0..PA7 (Port A)",
		"  bit       0 or 1",
		"  name      region name (e.g. 'via1') — required when >1 VIA present",
		"  addr      hex base address (e.g. $B000) — alternative to name",
		"",
		"examples:",
		"  via dump                 dump the only VIA (auto)",
		"  via dump via2            dump the named VIA",
		"  via dump $B000           dump by hex base",
		"  via set PB0 1            drive PB0 high (single-VIA machines)",
		"  via set via2 PA3 0       drive Port A bit 3 low on via2",
	}
	for _, l := range lines {
		c.addEvent("result", l)
	}
}

// cmdVIAList enumerates every VIA region the server returned from
// machine.load. Useful diagnostic before issuing a `via set` on a
// multi-VIA machine — show what's there + their base addresses.
func (c *Controller) cmdVIAList() {
	vias := c.viaRegions()
	if len(vias) == 0 {
		c.addEvent("err", "no VIA regions reported by server")
		return
	}
	c.addEvent("result", fmt.Sprintf("%d VIA(s) present:", len(vias)))
	for _, r := range vias {
		c.addEvent("result", fmt.Sprintf("  %-8s  $%04X-$%04X", r.Name, r.Lo, r.Hi))
	}
}

// cmdVIADump peeks 16 bytes from the resolved VIA base and pretty-
// prints them with canonical register names + flag decode hints.
// `arg` is either a region name, a hex base, or empty (auto-resolve).
func (c *Controller) cmdVIADump(arg string) {
	base, err := c.resolveVIABase(arg)
	if err != nil {
		c.addEvent("err", "via dump: "+err.Error())
		return
	}
	buf, e := c.client.Load().MemPeek(base, 16)
	if e != nil {
		c.addEvent("err", "mem.peek: "+e.Message)
		return
	}
	if len(buf) < 16 {
		c.addEvent("err", fmt.Sprintf("via dump: got %d bytes, expected 16", len(buf)))
		return
	}
	names := []string{
		"ORB/IRB ", "ORA/IRA ", "DDRB    ", "DDRA    ",
		"T1C-L   ", "T1C-H   ", "T1L-L   ", "T1L-H   ",
		"T2C-L   ", "T2C-H   ", "SR      ", "ACR     ",
		"PCR     ", "IFR     ", "IER     ", "ORA-nh  ",
	}
	c.addEvent("result", fmt.Sprintf("VIA @ $%04X:", base))
	for i, n := range names {
		c.addEvent("result",
			fmt.Sprintf("  $%04X  %s  $%02X  %s",
				base+uint16(i), n, buf[i], viaBitsHint(i, buf[i])))
	}
}

// cmdVIASet — `via set [name] <pin> <bit>`. The name is optional
// when exactly one VIA is present; required otherwise (surfaces an
// error listing candidates). pin is PA0..PA7 or PB0..PB7; bit is 0
// or 1.
//
// Strict DDR check: if the corresponding DDR bit configures the pin
// as input, the command fails with a clear message rather than
// silently writing to the output latch that no external observer
// can see. The DDR/OR/IR distinction is a thing 6502 learners
// stumble on; surfacing it inline is the teaching opportunity.
func (c *Controller) cmdVIASet(toks []string) {
	if len(toks) < 2 {
		c.addEvent("err", "usage: via set [name] <pin> <bit>")
		return
	}
	// The first token MAY be a VIA name (alphanumeric like "via1") OR
	// the pin itself. Disambiguate: if it parses as a pin, treat the
	// VIA as auto-resolved.
	var nameArg, pinArg, bitArg string
	if _, _, perr := parseVIAPin(toks[0]); perr == nil {
		// Form: via set <pin> <bit>
		pinArg = toks[0]
		bitArg = toks[1]
	} else {
		// Form: via set <name> <pin> <bit>
		if len(toks) < 3 {
			c.addEvent("err", "usage: via set <name> <pin> <bit>")
			return
		}
		nameArg = toks[0]
		pinArg = toks[1]
		bitArg = toks[2]
	}

	base, err := c.resolveVIABase(nameArg)
	if err != nil {
		c.addEvent("err", "via set: "+err.Error())
		return
	}
	port, pinBit, err := parseVIAPin(pinArg)
	if err != nil {
		c.addEvent("err", "via set: "+err.Error())
		return
	}
	bitVal, err := strconv.Atoi(strings.TrimSpace(bitArg))
	if err != nil || (bitVal != 0 && bitVal != 1) {
		c.addEvent("err", "via set: bit must be 0 or 1")
		return
	}

	// VIA register offsets:
	//   $00 ORB/IRB    $02 DDRB
	//   $01 ORA/IRA    $03 DDRA
	var orRegOff, ddrRegOff uint16
	switch port {
	case 'B':
		orRegOff, ddrRegOff = 0, 2
	case 'A':
		orRegOff, ddrRegOff = 1, 3
	}
	orAddr := base + orRegOff
	ddrAddr := base + ddrRegOff

	// Strict DDR check: bit must be configured as output.
	ddr, e := c.client.Load().MemPeek(ddrAddr, 1)
	if e != nil {
		c.addEvent("err", "mem.peek DDR: "+e.Message)
		return
	}
	if len(ddr) < 1 {
		c.addEvent("err", "via set: short read on DDR")
		return
	}
	mask := byte(1 << pinBit)
	if ddr[0]&mask == 0 {
		c.addEvent("err",
			fmt.Sprintf("via set: P%c%d is input in DDR%c=$%02X — set DDR first ('m $%04X 1' to inspect)",
				port, pinBit, port, ddr[0], ddrAddr))
		return
	}

	// RMW the OR register.
	cur, e := c.client.Load().MemPeek(orAddr, 1)
	if e != nil {
		c.addEvent("err", "mem.peek OR: "+e.Message)
		return
	}
	if len(cur) < 1 {
		c.addEvent("err", "via set: short read on OR")
		return
	}
	next := cur[0]
	if bitVal == 1 {
		next |= mask
	} else {
		next &^= mask
	}
	if _, e := c.client.Load().MemPoke(orAddr, []byte{next}); e != nil {
		c.addEvent("err", "mem.poke OR: "+e.Message)
		return
	}
	c.addEvent("result",
		fmt.Sprintf("P%c%d ← %d  (OR%c $%04X: $%02X → $%02X)",
			port, pinBit, bitVal, port, orAddr, cur[0], next))
}

// viaRegions returns every region whose name looks like a VIA — case-
// insensitive prefix match on "via". Matches teach-min's "via1" and
// teach-merlin's "via1" + "via2"; survives future renamings as long
// as the convention holds.
func (c *Controller) viaRegions() []bridge.Region {
	var out []bridge.Region
	for _, r := range c.regions {
		if strings.HasPrefix(strings.ToLower(r.Name), "via") {
			out = append(out, r)
		}
	}
	return out
}

// resolveVIABase turns the optional `arg` into the base address of a
// VIA. arg can be empty (auto-resolve if exactly one VIA), a region
// name ("via1"), or a hex base ($B000). Multi-VIA + empty arg
// surfaces an informative error listing candidates so the user knows
// which name(s) are valid.
func (c *Controller) resolveVIABase(arg string) (uint16, error) {
	arg = strings.TrimSpace(arg)
	vias := c.viaRegions()

	// Empty arg: auto-resolve if and only if there's exactly one.
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

	// Named match? (case-insensitive)
	for _, r := range vias {
		if strings.EqualFold(r.Name, arg) {
			return r.Lo, nil
		}
	}

	// Fall back to hex parse — lets the user spell out an explicit
	// base even if it doesn't match a region name.
	if v, err := parseAddr(arg); err == nil {
		return v, nil
	}
	return 0, fmt.Errorf("unknown VIA %q (run 'via list')", arg)
}

// parseVIAPin parses "PB3" / "pa0" / etc. Returns the port letter
// ('A' or 'B') and the bit index (0-7). Strict shape: P + port +
// single decimal digit. Returns an error for anything else so the
// caller can distinguish "this is a name, not a pin."
func parseVIAPin(s string) (port byte, bit int, err error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if len(t) != 3 || t[0] != 'P' {
		return 0, 0, fmt.Errorf("bad pin %q (want PA0..PA7 or PB0..PB7)", s)
	}
	if t[1] != 'A' && t[1] != 'B' {
		return 0, 0, fmt.Errorf("bad pin %q (port must be A or B)", s)
	}
	if t[2] < '0' || t[2] > '7' {
		return 0, 0, fmt.Errorf("bad pin %q (bit must be 0..7)", s)
	}
	return t[1], int(t[2] - '0'), nil
}

// viaBitsHint adds a short decode hint for the bytes where it's most
// useful to read flag values inline rather than mentally converting
// hex to bits.
func viaBitsHint(reg int, v byte) string {
	switch reg {
	case 11: // ACR
		t1mode := []string{"timed (one-shot)", "free-run", "timed + PB7", "free-run + PB7"}
		mode := t1mode[(v>>6)&3]
		return "T1=" + mode
	case 13: // IFR
		bits := decodeBits(v, []string{"CA2", "CA1", "SR", "CB2", "CB1", "T2", "T1", "IRQ"})
		if bits == "" {
			return "(no flags)"
		}
		return "set: " + bits
	case 14: // IER
		bits := decodeBits(v&0x7F, []string{"CA2", "CA1", "SR", "CB2", "CB1", "T2", "T1"})
		if bits == "" {
			return "(none enabled)"
		}
		return "enabled: " + bits
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

func (c *Controller) cmdBPList() {
	r, e := c.client.Load().BPList()
	if e != nil {
		c.addEvent("err", "bp.list: "+e.Message)
		return
	}
	if len(r.Breakpoints) == 0 {
		c.addEvent("result", "(no breakpoints)")
		return
	}
	for _, bp := range r.Breakpoints {
		switch bp.Kind {
		case "addr":
			c.addEvent("result", fmt.Sprintf("  %s  addr  $%04X", bp.ID, bp.Addr))
		case "vector":
			c.addEvent("result", fmt.Sprintf("  %s  vector  %s", bp.ID, bp.Vector))
		default:
			c.addEvent("result", fmt.Sprintf("  %s  %s", bp.ID, bp.Kind))
		}
	}
}

func (c *Controller) cmdStopMonitor() {
	sr, e := c.client.Load().Stop()
	if e != nil {
		c.addEvent("err", "clock.stop: "+e.Message)
		return
	}
	c.setState(sr.State)
	c.addEvent("info", "stop ("+sr.Reason+")")
}

// cmdHelp prints the command table or detail for a single verb.
//
//   help            → grouped scrollback dump (helpTable)
//   help window     → open the persistent foxpro help window
//   help <verb>     → terse detail for that one verb
//
// The window path goes through helpWindowFn (set by main when foxpro
// is wired up). If the callback isn't set (shouldn't happen in
// practice), falls back to the scrollback dump so we never lose the
// help surface.
func (c *Controller) cmdHelp(args string) {
	args = strings.ToLower(strings.TrimSpace(args))
	switch args {
	case "":
		for _, line := range helpTable {
			c.addEvent("result", line)
		}
		return
	case "window", "w", "win":
		if c.helpWindowFn != nil {
			c.helpWindowFn()
			c.addEvent("info", "opened help window")
			return
		}
		// Fallback when the window callback isn't wired.
		for _, line := range helpTable {
			c.addEvent("result", line)
		}
		return
	}
	if d, ok := helpDetail[args]; ok {
		for _, line := range d {
			c.addEvent("result", line)
		}
		return
	}
	c.addEvent("err", "no help for: "+args)
}

// --- parsing helpers ---

// resolveAddr accepts either a hex literal ($E000 / 0xE000 / E000) or
// one of the symbolic shortcuts that point at live machine state:
//
//   pc                 — current program counter (cpu.state)
//   sp                 — $01xx, the stack pointer's current top byte
//   reset              — *($FFFC), the reset vector target
//   irq  /  brk        — *($FFFE), the IRQ / BRK vector target
//   nmi                — *($FFFA), the NMI vector target
//
// Symbolic resolution does live reads (cpu.state or mem.peek), so it
// reflects whatever the machine looks like right now. Returns an
// informative error so the user knows whether the token didn't parse
// or the bridge call failed.
func (c *Controller) resolveAddr(s string) (uint16, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	switch t {
	case "pc":
		st, e := c.client.Load().CPUState()
		if e != nil {
			return 0, fmt.Errorf("cpu.state: %s", e.Message)
		}
		return st.PC, nil
	case "sp":
		st, e := c.client.Load().CPUState()
		if e != nil {
			return 0, fmt.Errorf("cpu.state: %s", e.Message)
		}
		return 0x0100 | uint16(st.SP), nil
	case "reset":
		return c.readVec(0xFFFC)
	case "irq", "brk":
		return c.readVec(0xFFFE)
	case "nmi":
		return c.readVec(0xFFFA)
	}
	return parseAddr(s)
}

// readVec peeks a 2-byte little-endian vector at `addr` (typically
// $FFFA/$FFFC/$FFFE) and returns the target address it points to.
func (c *Controller) readVec(addr uint16) (uint16, error) {
	buf, e := c.client.Load().MemPeek(addr, 2)
	if e != nil {
		return 0, fmt.Errorf("mem.peek $%04X: %s", addr, e.Message)
	}
	if len(buf) < 2 {
		return 0, fmt.Errorf("short vector read at $%04X", addr)
	}
	return uint16(buf[0]) | uint16(buf[1])<<8, nil
}

// parseAddr accepts "$E000", "0xE000", "E000" — always hex, max 16
// bits. Decimal addresses aren't a 6502 idiom and would only invite
// off-by-base bugs, so we don't support them.
func parseAddr(s string) (uint16, error) {
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

// parseByte accepts the same prefixes as parseAddr, but tops out at 8
// bits.
func parseByte(s string) (byte, error) {
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

// hexDump formats buf into HESMON-style rows: address, 16 hex bytes,
// ASCII gutter. Returns one string per visual row so the caller can
// emit each as its own scrollback line.
func hexDump(base uint16, buf []byte) []string {
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

// --- help tables ---

// helpTable is the grouped inline help printed by bare `help` / `?`.
// Sections are spaced for readability — 25+ verbs with no structure
// is a wall of text. `help window` opens the same content in a
// persistent foxpro window the user can drag aside for reference.
var helpTable = []string{
	"commands:",
	"",
	"  CPU & run",
	"    r              CPU registers",
	"    g [addr]       run from PC (or set PC=addr then run)",
	"    s [n]          step n instructions (default 1)",
	"    .  /  stop     stop the clock",
	"    reset          reset CPU + clock driver",
	"",
	"  memory",
	"    m [addr] [n]   memory hex dump (default at PC, n=16, max 256)",
	"    d [addr] [n]   disassemble (default at PC, n=10, max 32)",
	"    : <addr> <b>…  poke byte(s) at addr",
	"    f <s> <e> <b>  fill memory range [s..e] with byte b",
	"    t <s> <d> <n>  transfer (copy) n bytes from s to d",
	"    h <s> <e> <b>… hunt for byte sequence in [s..e]",
	"",
	"  breakpoints",
	"    bp <addr>      set address breakpoint",
	"    bc [id|all]    clear one breakpoint, or all",
	"    bl             list breakpoints",
	"",
	"  interrupts",
	"    irq            pulse IRQ (honors I flag)",
	"    nmi            pulse NMI (ignores I; interp only)",
	"",
	"  hardware",
	"    hw  / info     describe machine + memory map + vectors",
	"    via <sub>      VIA subcommands  (try 'via help')",
	"",
	"  monitor",
	"    reconnect      force an immediate bridge reconnect",
	"    cls / clear    clear the scrollback",
	"    help [cmd]     show this list, or detail for one command",
	"    help window    open the same help in a persistent window",
	"    ?              alias for help",
	"    q  /  quit     exit",
	"",
	"  addresses are hex: $E000 = 0xE000 = E000",
	"  symbolic (use anywhere addr is accepted):",
	"    pc   live program counter         sp   $01xx (stack top)",
	"    reset *($FFFC)   irq  *($FFFE)   nmi  *($FFFA)",
}

var helpDetail = map[string][]string{
	"r": {
		"r          CPU registers",
		"prints A/X/Y/SP/P/PC and decoded flags from cpu.state.",
	},
	"m": {
		"m [addr] [count]   memory hex dump",
		"reads up to <count> bytes (default 16, max 256) from <addr>",
		"through the bus, so peripheral registers respond as if read.",
		"With no addr, dumps at the current PC. Symbolic addrs:",
		"  m pc / m sp / m reset / m irq / m nmi",
	},
	"d": {
		"d [addr] [count]   disassemble n instructions (default 10, max 32)",
		"decodes 6502 opcodes from <addr> using the local disasm tables.",
		"output: addr, raw bytes, mnemonic + operand.",
		"With no addr, disassembles from current PC ('where am I?').",
		"Symbolic addrs:  d pc / d reset / d irq / d nmi",
	},
	":": {
		": <addr> <b>...    poke byte(s) at addr",
		"writes one or more hex bytes consecutively starting at <addr>",
		"through the bus — devices see the writes.",
	},
	"f": {
		"f <start> <end> <byte>    fill range [start..end] with byte",
		"single mem.poke under the hood; range capped at 16K bytes.",
		"example:  f $0200 $02FF $00     zero the second 256 bytes of RAM",
	},
	"t": {
		"t <src> <dst> <len>       copy len bytes from src to dst",
		"len is decimal. peek + poke through the bus; if regions overlap",
		"the copy is forward (memcpy-style, not memmove). cap: 16K.",
		"example:  t $E000 $0400 256    copy 256 bytes of ROM to $0400",
	},
	"h": {
		"h <start> <end> <b>...    hunt for byte sequence in range",
		"pattern is one or more hex bytes; prints every match address.",
		"first 32 matches printed; remainder counted in a summary line.",
		"example:  h $0000 $FFFF A9 2A   find every 'LDA #$2A'",
	},
	"s": {
		"s [n]              step n instructions (default 1)",
		"steps and reports the new PC. peripherals advance for the",
		"matching virtual time slice.",
	},
	"g": {
		"g [addr]           run from current PC, or set PC=addr first then run",
		"addr accepts hex ($E000) or symbolic (pc/reset/irq/nmi).",
		"  g            resume from wherever PC sits now",
		"  g $E000      set PC=$E000 then run",
		"  g reset      restart from the reset vector (common 'reboot')",
		"netsim CPU mode: 'g <addr>' fails (upstream lacks SetPC).",
	},
	".": {
		".  or  stop        stop the clock",
		"asks the pump to halt; sim emits clock.halt with reason=client.",
	},
	"reset": {
		"reset              reset CPU + clock driver",
		"reloads the reset vector from $FFFC/$FFFD. RAM is unchanged.",
	},
	"bp": {
		"bp <addr>          arm an address breakpoint",
		"server returns a session-scoped id (bp#1, bp#2, …) used by",
		"bc and the bp.hit event payload.",
	},
	"bc": {
		"bc                 clear all breakpoints",
		"bc all             same as bare 'bc'",
		"bc <id>            clear one breakpoint by id (see bl)",
	},
	"bl": {
		"bl                 list every armed breakpoint",
	},
	"hw": {
		"hw  /  info        describe loaded machine + memory map",
		"prints the machine label, CPU, optional program info, and",
		"every region the server returned from machine.load.",
	},
	"via": {
		"via <subcommand>   VIA-specific knobs grouped under one verb",
		"subcommands:",
		"  list                       enumerate VIAs on this machine",
		"  dump [name|addr]           hex dump of 16 registers (+ flag decode)",
		"  set [name] <pin> <bit>     drive PA/PB pin output 0 or 1",
		"  help / ?                   show this list",
		"",
		"name defaults to the only VIA when there's one; required for",
		"multi-VIA machines (e.g. teach-merlin has via1 + via2).",
		"  see 'via help' for full examples.",
	},
	"cls": {
		"cls  /  clear      clear the scrollback",
		"removes all event lines; equivalent to View → Clear Monitor.",
	},
	"reconnect": {
		"reconnect          force an immediate bridge reconnect",
		"closes the current TCP connection; the heal loop detects the",
		"drop and dials a fresh session (hello + machine.load + subscribe).",
		"useful when you know the sim just restarted and don't want to",
		"wait for the next backoff tick.",
	},
	"irq": {
		"irq                pulse the IRQ line (one shot)",
		"asserts host-IRQ for one Pump slice (when running) or until",
		"the next step (when idle). CPU services if I flag is clear;",
		"silently ignored if I=1. Vectors through $FFFE.",
	},
	"nmi": {
		"nmi                pulse the NMI line (one shot)",
		"edge-triggered; ignores the I flag (unlike IRQ); vectors via",
		"$FFFA. Fires reliably under interp; netsim CPU mode is a",
		"no-op (upstream 6502-netsim-go lacks SetNMI).",
	},
}
