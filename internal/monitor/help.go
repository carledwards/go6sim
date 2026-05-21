// Help strings — the inline grouped table printed by bare `help`/`?`
// plus per-verb detail blurbs printed by `help <cmd>`. Exported so
// tests can assert membership, and so the embedding app can render
// the same content in a separate window via Host.OpenHelpWindow.
package monitor

// HelpTable is the grouped scrollback dump for `help` / `?`. Same
// content the persistent help window renders, just in scrollback
// form. Single source of truth — Host's window opener reads this.
var HelpTable = []string{
	"commands:",
	"",
	"  CPU & run",
	"    r              CPU registers",
	"    g [addr]       run from PC (or set PC=addr then run)",
	"    s [n]          step n instructions (default 1)",
	"    .  /  stop     stop the clock",
	"    reset          reset CPU + clock driver",
	"    stack [r]      dump the active stack ($01xx); 'r' = guess return addrs",
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
	"    irq            pulse IRQ (one shot; honors I flag)",
	"    nmi            pulse NMI (one shot; ignores I; interp only)",
	"",
	"  hardware",
	"    hw  / info     describe machine + memory map + vectors",
	"    via <sub>      VIA subcommands  (try 'via help')",
	"",
	"  monitor",
	"    reconnect      force an immediate bridge reconnect (wire targets)",
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

// HelpDetail maps a single verb to its longer help text. Used by
// `help <verb>` to print expanded usage / examples / gotchas.
var HelpDetail = map[string][]string{
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
		"prints the machine label, CPU, optional program info, every",
		"region the server returned, and the three 6502 vectors live.",
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
		"reconnect          force an immediate bridge reconnect (wire targets)",
		"closes the current TCP connection; the heal loop detects the",
		"drop and dials a fresh session. In-process targets (sim TUI's",
		"built-in Monitor) treat this as a no-op + info line.",
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
	"stack": {
		"stack              dump the active 6502 stack ($01xx region)",
		"stack r            also overlay return-address guesses on pairs",
		"",
		"shows bytes from $01(SP+1) to $01FF — newest pushes at the top,",
		"oldest at the bottom. SP=$FF means the stack is empty.",
		"",
		"'r' mode interprets adjacent (lo, hi) pairs as JSR return",
		"frames; the displayed target = (hi<<8|lo) + 1, mirroring what",
		"RTS would compute. Heuristic: IRQ/BRK push three bytes",
		"(PCH PCL P) so a P value can masquerade as a low byte.",
	},
}
