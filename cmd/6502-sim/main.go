package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"time"

	foxpro "github.com/carledwards/foxpro-go"
	"github.com/carledwards/foxpro-go/dialog"

	"github.com/carledwards/go6sim/asm"
	"github.com/carledwards/go6sim/backplane"
	"github.com/carledwards/go6sim/bridge"
	"github.com/carledwards/go6sim/bus"
	"github.com/carledwards/go6sim/clock"
	"github.com/carledwards/go6sim/components/display"
	"github.com/carledwards/go6sim/components/ram"
	"github.com/carledwards/go6sim/components/rom"
	"github.com/carledwards/go6sim/components/via"
	"github.com/carledwards/go6sim/cpu"
	"github.com/carledwards/go6sim/cpu/interp"
	"github.com/carledwards/go6sim/cpu/netsim"
	"github.com/carledwards/go6sim/instrument"
	"github.com/carledwards/go6sim/internal/demos"
	"github.com/carledwards/go6sim/ui/clockwin"
	"github.com/carledwards/go6sim/ui/cpuwin"
	"github.com/carledwards/go6sim/ui/displaywin"
	"github.com/carledwards/go6sim/ui/ramwin"
	"github.com/carledwards/go6sim/ui/scopewin"
	"github.com/carledwards/go6sim/ui/viawin"

	"github.com/gdamore/tcell/v2"
)

// Memory map. Modeled after a real 6502 machine: contiguous RAM in
// the bottom half, I/O up high, ROM at the top. The 8 KB RAM is one
// flat block — programs can use $0000-$00FF (zero page), $0100-$01FF
// (stack), and the rest as ordinary working memory.
//
// VIC bases are laid out so that each is a uniform +$8000 offset
// from the equivalent in older builds. That keeps demo addresses
// translatable by changing just the high nibble of the high byte
// ($02 → $82, $05 → $85, $08 → $88), and matches the C64-style
// "I/O lives high" convention.
const (
	ramBase   = 0x0000
	ramSize   = 0x2000 // 8 KB at $0000-$1FFF
	colorBase = 0xA000 // VIC color plane  (520 bytes, in 1 KB block)
	charBase  = 0xA400 // VIC char plane   (520 bytes, in 1 KB block)
	ctrlBase  = 0xA800 // VIC controller registers (16 bytes, in 1 KB CS block)
	viaBase   = 0xB000 // 6522 VIA #1 (own 256-byte CS; mirrors every 16 bytes)
	dispW     = 40
	dispH     = 13
	romBase   = 0xE000
	romSize   = 0x2000
)

// Memory map a demo author should know:
//   $0000-$1FFF  RAM (8 KB)
//   $8000+       VIC color plane  (40 × 13 = 520 bytes)
//   $8500+       VIC char plane   (520 bytes)
//   $8800-$8802  VIC controller   (cmd / pause / frame)
//   $E000-$FFFF  ROM (program loaded here, reset vector at $FFFC)

// tuneCandidates are the batch sizes auto-tune tries in order. They
// are already round numbers, so the picked value is also "memorable"
// — no separate rounding step needed.
var tuneCandidates = []int{500, 1000, 1500, 2000, 2500, 3000, 4000, 5000, 7500, 10000, 20000, 50000, 100000}

// autoTune runs increasing-size batches against the backend and
// returns the largest size that fit inside `budget`. Conservative
// by design: budget < tickPeriod leaves UI headroom.
//
// Mutates backend state (advances cycles); the caller should Reset
// the CPU after.
func autoTune(backend cpu.Backend, budget time.Duration) int {
	best := tuneCandidates[0]
	for _, n := range tuneCandidates {
		start := time.Now()
		for i := 0; i < n; i++ {
			backend.HalfStep()
		}
		elapsed := time.Since(start)
		if elapsed <= budget {
			best = n
			continue
		}
		break // batches will only get slower; stop
	}
	return best
}

const tickPeriod = 50 * time.Millisecond

func main() {
	// Defaults are tuned for "open the TUI, see the demo running".
	// Interp is fast enough to make the marquee look alive without
	// the user having to tweak anything; -cpu=netsim opts into the
	// transistor-level backend for visualization.
	cpuFlag := flag.String("cpu", "interp", "CPU backend: interp or netsim")
	runFlag := flag.Bool("run", true, "start the clock running immediately (default true)")
	speedFlag := flag.String("speed", "max", "starting clock speed: 1, 10, 100, 1k (or 1000), max")
	batchFlag := flag.Int("batch", 0, "max HalfSteps per UI tick (0 = auto-tune at startup based on the chosen backend)")
	cpuProfile := flag.String("cpuprofile", "", "write CPU profile to file (active for the lifetime of the process)")
	memProfile := flag.String("memprofile", "", "write heap profile to file at exit")
	serveAddr := flag.String("serve", "", "expose the live machine over the bridge on this addr (e.g. 127.0.0.1:6502); empty = no listener. Loopback-only in v1.")
	flag.Parse()

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			log.Fatalf("cpuprofile create: %v", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatalf("cpuprofile start: %v", err)
		}
		defer pprof.StopCPUProfile()
	}
	if *memProfile != "" {
		defer func() {
			f, err := os.Create(*memProfile)
			if err != nil {
				log.Printf("memprofile create: %v", err)
				return
			}
			defer f.Close()
			runtime.GC()
			if err := pprof.WriteHeapProfile(f); err != nil {
				log.Printf("memprofile write: %v", err)
			}
		}()
	}

	// Bus + memory map. The outer TraceBus stamps each read/write with
	// a generation counter so the memory viewer can tint cells that
	// have been touched recently. The inner bus is what the memory
	// viewer's display reads use, so its own polling doesn't pollute
	// the trace.
	bp := backplane.New()
	innerBus := bp.Trace().Inner() // raw bus: untraced provider reads + Components()
	b := bp                        // the backplane (IS bus.Bus + Tick + Attach)
	mainRAM := ram.New("ram", ramBase, ramSize)
	colorPlane := display.New("display.color", colorBase, dispW, dispH)
	charPlane := display.New("display.char", charBase, dispW, dispH)
	mainROM := rom.New("rom", romBase, romSize)

	// paintInitialDisplay seeds the framebuffer with a diagonal-gradient
	// background so there's something to see before any program runs.
	// Also called when switching demos to give a clean canvas.
	paintInitialDisplay := func() {
		for y := 0; y < dispH; y++ {
			for x := 0; x < dispW; x++ {
				colorPlane.SetPixel(x, y, uint8(((x+y)%16)<<4))
				charPlane.SetPixel(x, y, 0x20)
			}
		}
	}
	paintInitialDisplay()

	dispCtrl := display.NewController("display.ctrl", ctrlBase, colorPlane, charPlane)

	// 6522 VIA #1 — clocked from its own 1 MHz oscillator. Demos use
	// Timer 1 in free-running mode to pace animation. Independent of
	// CPU clock, so it keeps running while stepping or paused — same
	// as a real W65C22S board with a separate timer crystal.
	via1 := via.New("via1", viaBase, 1_000_000)

	bootDemo := demos.MarqueeDemo
	// currentDemo tracks which demo is currently loaded so the
	// File → Load… dialog can pre-highlight it. Updated whenever
	// loadDemo runs.
	currentDemo := bootDemo.Name
	must(mainROM.Load(0, bootDemo.Bytes))
	must(mainROM.SetResetVector(0xE000))
	must(b.Attach(mainRAM))
	must(b.Attach(colorPlane))
	must(b.Attach(charPlane))
	must(b.Attach(dispCtrl))
	must(b.Attach(via1))
	must(b.Attach(mainROM))

	// CPU backend — mutable so the CPU menu can swap it at runtime.
	buildBackend := func(name string) (cpu.Backend, error) {
		switch name {
		case "netsim":
			return netsim.New(b)
		case "interp":
			return interp.New(b), nil
		}
		return nil, fmt.Errorf("unknown cpu %q (want netsim or interp)", name)
	}

	backend, err := buildBackend(*cpuFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	backend.Reset()
	currentCPU := *cpuFlag
	cpuTitle := fmt.Sprintf("CPU (%s)", currentCPU)

	// foxpro-go app.
	app, err := foxpro.NewApp()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init:", err)
		os.Exit(1)
	}
	defer app.Close()

	// Opt in to foxpro's standard CLEAR / HELP / QUIT / VER command-window
	// commands. As of foxpro-go's switch to opt-in builtins, this call
	// is required to keep the F2 command window populated.
	foxpro.RegisterBuiltinCommands(app)

	// Track every window we create so we can toggle visibility from
	// a Window menu after the user closes one.
	var windows []*foxpro.Window
	addWindow := func(title string, bounds foxpro.Rect, content foxpro.ContentProvider, minW, minH int) *foxpro.Window {
		w := foxpro.NewWindow(title, bounds, content)
		w.MinW = minW
		w.MinH = minH
		app.Manager.Add(w)
		windows = append(windows, w)
		return w
	}

	pcHighlight := func() (uint16, bool) {
		return backend.Registers().PC, true
	}

	// Hardware-symbol harvest from any bus.Labeller component (VIC,
	// VIA). Merged with each demo's program-local symbols so the
	// memory window's Labels view annotates both regions.
	hwSyms := []asm.Symbol{}
	for _, c := range innerBus.Components() {
		if l, ok := c.(bus.Labeller); ok {
			hwSyms = append(hwSyms, l.Symbols()...)
		}
	}
	mergeSymbols := func(demoSyms []asm.Symbol) []asm.Symbol {
		out := make([]asm.Symbol, 0, len(demoSyms)+len(hwSyms))
		out = append(out, demoSyms...)
		out = append(out, hwSyms...)
		return out
	}

	cpuProv := &cpuwin.Provider{Backend: backend}
	cpuWindow := addWindow(cpuTitle,
		foxpro.Rect{X: 2, Y: 1, W: 38, H: 13},
		cpuProv,
		cpuwin.MinW, cpuwin.MinH)

	ramProv := &ramwin.Provider{
		Bus:          innerBus, // read display state without tracing it
		Trace:        bp.Trace(),
		Backend:      backend,
		Base:         0x0000,
		Length:       0x100,
		Highlight:    pcHighlight,
		EditableBase: true,
		Symbols:      mergeSymbols(bootDemo.Symbols),
		Annotations:  bootDemo.Annotations,
	}
	memWin := addWindow("Memory",
		foxpro.Rect{X: 42, Y: 1, W: 76, H: 14},
		ramProv,
		ramwin.MinW, ramwin.MinH)
	ramProv.Window = memWin

	romProv := &ramwin.Provider{
		Bus:          innerBus,
		Trace:        bp.Trace(),
		Backend:      backend,
		Base:         romBase,
		Length:       romSize,
		Highlight:    pcHighlight,
		EditableBase: true,
		View:         ramwin.ViewDisasm,
		Symbols:      mergeSymbols(bootDemo.Symbols),
		Annotations:  bootDemo.Annotations,
	}
	romWin := addWindow("Memory",
		foxpro.Rect{X: 42, Y: 16, W: 76, H: 8},
		romProv,
		ramwin.MinW, ramwin.MinH)
	romProv.Window = romWin

	// One shared clock driver: the clock window renders it, the
	// instrument (run loop) drives it. TUI is now an instrument client.
	drv := clock.NewDriver(backend)
	clockProv := clockwin.NewProviderWithDriver(drv)
	inst := instrument.New(bp, drv)

	// --serve: attach a bridge listener to the live Instrument so a
	// remote controller (cmd/6502-control) can drive this same TUI
	// over the wire. When any client is connected, `remote.Active()`
	// goes true and the keyboard handler suppresses Run/Stop/Step/
	// Reset — clock ownership transfers to the controller (per the
	// design decision: "controller takes over the clock"). See
	// cmd/6502-sim/serve.go.
	// Build the shared Hub up-front, always. The Pump is the sole
	// driver of CPU + peripherals — the TUI's keyboard, menus, and any
	// attached bridge sessions are all co-equal commanders sending
	// into the same hub.commandsCh. No second pump, no lockout, no
	// race. (The Hub heartbeats peripherals while idle so the 1 MHz
	// crystal keeps running between steps — see hub.go.)
	simTUIRegions := []bridge.Region{
		{Name: "ram", Lo: 0x0000, Hi: 0x1FFF, ReadOnly: false},
		{Name: "framebuffer", Lo: 0xA000, Hi: 0xA7FF, ReadOnly: false},
		{Name: "via1", Lo: 0xB000, Hi: 0xB00F, ReadOnly: false},
		{Name: "rom", Lo: 0xE000, Hi: 0xFFFF, ReadOnly: true},
	}
	sharedHub := bridge.NewHub(inst, simTUIRegions, "sim-tui")
	hubCtx, cancelHub := context.WithCancel(context.Background())
	go sharedHub.Run(hubCtx)
	defer cancelHub()

	remote := &RemoteState{} // kept for the serve listener; no longer used for lockout
	if *serveAddr != "" {
		loader := newSimTUILoader(sharedHub, func(image []byte) error {
			mainROM.Clear()
			if err := mainROM.Load(0, image); err != nil {
				return err
			}
			if err := mainROM.SetResetVector(0xE000); err != nil {
				return err
			}
			sharedHub.CmdReset()
			return nil
		})
		if err := StartServe(context.Background(), *serveAddr, loader, remote); err != nil {
			log.Fatalf("--serve: %v", err)
		}
	}
	if *batchFlag > 0 {
		clockProv.MaxBatch = *batchFlag
	} else {
		// Auto-tune at startup: pick a batch size that lands the
		// per-tick cost at ~70% of the 50 ms UI tick. Keeps the UI
		// responsive while letting fast backends (interp) cruise.
		clockProv.MaxBatch = autoTune(backend, 35*time.Millisecond)
	}
	addWindow("Clock",
		foxpro.Rect{X: 2, Y: 13, W: 38, H: 7},
		clockProv,
		clockwin.MinW, clockwin.MinH)

	viaProv := &viawin.Provider{VIA: via1, Base: viaBase}
	addWindow("I/O",
		foxpro.Rect{X: 2, Y: 21, W: 56, H: 20},
		viaProv,
		viawin.MinW, viawin.MinH)

	// Logic-analyzer scope: 256 cycles of trace history. Hidden by
	// default — toggled via the Window menu. In TUI mode the canvas
	// region is blank (no graphics overlay); the cell-rendered
	// label strip is still useful as a placeholder.
	// TUI has no graphics layer; cell-mode (one sample per cell).
	scopeProv := scopewin.New(128, false)
	scopeWin := addWindow("Logic Analyzer",
		foxpro.Rect{X: 1, Y: 1, W: 139, H: 30},
		scopeProv,
		scopewin.MinW, scopewin.MinH)
	app.Manager.Remove(scopeWin)

	prevSync := false
	clockProv.OnHalfStep = func() {
		s := backend.SYNC()
		edge := s && !prevSync
		prevSync = s
		scopeProv.Capture(backend.AddressBus(), backend.DataBus(), edge)
	}

	// machineReset = full simulated-machine restart: drop VIC pause,
	// clear RAM, reset peripherals, repaint display, reset CPU. ROM
	// stays loaded with the current demo so reset starts it over.
	//
	// Modeled on a real hardware reset button: the clock keeps
	// running. If the user had the simulator running, it stays
	// running and the demo restarts immediately. If it was stopped,
	// it stays stopped until the user hits R.
	machineReset := func() {
		// Drained on the Pump goroutine so the peripheral resets
		// don't race with an in-flight CPU slice.
		sharedHub.IssueCommand(func(_ *bridge.Pump) {
			dispCtrl.Reset()
			mainRAM.Reset()
			via1.Reset()
			scopeProv.Reset()
		})
		// CPU + driver shallow-reset (also on the Pump goroutine).
		sharedHub.CmdReset()
		// Repaint is a UI op — fine to call from the caller's thread.
		paintInitialDisplay()
	}
	cpuProv.OnReset = machineReset

	dispProv := &displaywin.Provider{
		// inner bus so the window's own hex-dump reads don't pollute
		// the read-trace. Component dispatch is identical — every
		// component is registered on the inner bus via TraceBus's
		// delegating Register, and button POKEs to $8800 still hit
		// the controller normally; they just aren't shown in the
		// per-cell trace tinting.
		Bus:        innerBus,
		Controller: dispCtrl,
		ColorBase:  colorBase,
		CharBase:   charBase,
		CtrlBase:   ctrlBase,
		HasChars:   true,
		HasCtrl:    true,
		Width:      dispW,
		Height:     dispH,
	}
	// Click on the Mode picker → open a popup just under the field.
	dispProv.OnPickMode = func() {
		current := 0
		if dispCtrl.Mode() == display.ModeGraphics {
			current = 1
		}
		mr := dispProv.ModeRect()
		// Anchor: nudged so the popup overlaps the picker field
		// itself rather than sitting in the empty area below it —
		// reads as the field "expanding" into the dropdown.
		w := dialog.NewPopupWindow(
			[]string{"Text", "Graphics"}, current,
			mr.X+1, mr.Y+mr.H-3,
			func(idx int) {
				val := uint8(display.ModeChar)
				if idx == 1 {
					val = display.ModeGraphics
				}
				b.Write(ctrlBase+display.RegMode, val)
			},
		)
		w.OnClose = func() { app.Manager.Remove(w) }
		app.Manager.Add(w)
	}
	dispTitle := fmt.Sprintf("Video $%04X-$%04X", colorBase, ctrlBase+6)
	addWindow(dispTitle,
		foxpro.Rect{X: 60, Y: 1, W: 77, H: 22},
		dispProv,
		displaywin.MinW, displaywin.MinH)

	// Run loop. App.Tick fires on the UI thread, so simulator
	// advancement, register reads, and bus reads all serialize
	// naturally — no locks needed.
	// Sub-tick: split each app.Tick into N slices, advancing CPU and
	// then the bus's Tickers in each slice. Without this, polling-
	// based demos (those that LDA / poll a peripheral flag in a tight
	// loop) only see flag transitions at app.Tick boundaries — so a
	// large CPU batch can spend the whole batch in a wait loop, never
	// observing the VIA timer underflow that's about to come.
	// (subTicks / subPeriod removed: the Hub Pump owns the
	// sub-cycle slicing now; app.Tick no longer drives the CPU.)

	// Auto-tune the scope's sampling stride to the current CPU
	// speed. At low Hz we capture every half-cycle (100%); at
	// higher rates we decimate so the visible trace covers a
	// useful time window instead of overwriting itself in
	// microseconds.
	scopeDecimate := func() int {
		hz := clockProv.Speed().Hz
		switch {
		case hz == 0: // Max
			return 256
		case hz >= 100000:
			return 32
		case hz >= 10000:
			return 8
		case hz >= 1000:
			return 4
		}
		return 1
	}

	// VIA ticks by wall-clock, not CPU half-steps — the chip's own
	// 1 MHz crystal runs continuously regardless of CPU speed, so
	// demos pace consistently whether the user is watching at 1 Hz
	// or Max. The init race this used to expose (T1 self-disarming
	// in one-shot mode before the demo set ACR=$40) is closed at
	// the demo level: every demo programs ACR *before* writing
	// T1C_H, so T1 arms straight into free-run.
	app.Tick(tickPeriod, func() {
		scopeProv.Decimate = scopeDecimate()
		// CPU + peripheral driving lives entirely on the Hub Pump
		// (see sharedHub above). This tick is now just for UI
		// state that the foxpro draw loop needs nudged on a steady
		// cadence (scope decimation). The Hub heartbeats peripherals
		// while idle, so the 1 MHz crystal still runs even when the
		// CPU is paused / stepping.
	})

	// Global key bindings. Active in any focused window so the user
	// can drive the simulator without first focusing the Clock window.
	app.OnKey = func(ev *tcell.EventKey) bool {
		if ev.Key() != tcell.KeyRune {
			return false
		}
		// Clock-affecting keys route through the Hub — same path the
		// bridge controller uses, so TUI and remote co-exist as
		// co-equal commanders. No lockout needed.
		switch ev.Rune() {
		case 'r', 'R':
			sharedHub.CmdRun(0, 0)
			return true
		case '.':
			sharedHub.CmdStop()
			return true
		case 's', 'S':
			sharedHub.CmdStep("instruction", 1)
			return true
		case 't', 'T':
			sharedHub.CmdStep("halfcycle", 1)
			return true
		case 'z', 'Z':
			machineReset()
			return true
		case '<', ',':
			clockProv.CycleSpeed(-1)
			return true
		case '>', '/':
			clockProv.CycleSpeed(1)
			return true
		}
		return false
	}

	// loadDemo swaps in a different ROM payload and resets the
	// machine. Preserves the running state — if the clock was
	// running before, the new demo starts running immediately.
	loadDemo := func(d demos.Demo) {
		wasRunning := clockProv.Running()
		clockProv.SetRunning(false)
		// Resume the VIC so a previous framed demo's pause state
		// doesn't leak into a live demo.
		dispCtrl.Reset()
		via1.Reset()
		scopeProv.Reset()
		mainROM.Clear()
		_ = mainROM.Load(0, d.Bytes)
		_ = mainROM.SetResetVector(0xE000)
		merged := mergeSymbols(d.Symbols)
		ramProv.Symbols = merged
		ramProv.Annotations = d.Annotations
		romProv.Symbols = merged
		romProv.Annotations = d.Annotations
		paintInitialDisplay()
		clockProv.Reset()
		currentDemo = d.Name
		if wasRunning {
			clockProv.SetRunning(true)
		}
	}

	// switchCPU swaps the CPU backend at runtime. The bus stays the
	// same — RAM, display, and ROM contents are preserved across the
	// switch — so the freshly-Reset CPU starts from $E000 against the
	// existing memory map.
	// switchCPU swaps in the named backend, resets the program,
	// and resumes the clock if it was running before. Order —
	// stop, build, reset, swap, retune, machineReset, resume —
	// matters: running netsim with interp's auto-tuned MaxBatch
	// would freeze the UI loop, and the autoTune call advances
	// HalfSteps that need a Reset first (netsim's getNodeValue
	// nil-derefs on an unreset transistor net).
	switchCPU := func(name string) {
		if name == currentCPU {
			return
		}
		wasRunning := clockProv.Running()
		clockProv.SetRunning(false)
		newBackend, err := buildBackend(name)
		if err != nil {
			return
		}
		newBackend.Reset()
		backend = newBackend
		clockProv.Backend = newBackend
		cpuProv.Backend = newBackend
		ramProv.Backend = newBackend
		romProv.Backend = newBackend
		// Re-tune MaxBatch for the new backend. Netsim is ~30x
		// slower per cycle than interp, so reusing the previous
		// batch size would spend the entire UI tick inside a single
		// CPU advance call, starving input. Skip when the user
		// supplied an explicit -batch flag (respect the override).
		if *batchFlag <= 0 {
			clockProv.MaxBatch = autoTune(newBackend, 35*time.Millisecond)
		}
		currentCPU = name
		cpuWindow.Title = fmt.Sprintf("CPU (%s)", name)
		// If the prior speed is above the new backend's plausible
		// ceiling (e.g. switching from interp@1MHz to netsim, which
		// caps near 26 kHz), drop to Max so the displayed speed
		// matches what's actually delivered.
		if limit := cpuMaxSettableHz(name); limit > 0 {
			if cur := clockProv.Speed().Hz; cur != 0 && cur > limit {
				clockProv.SetSpeedHz(0)
			}
		}
		machineReset()
		if wasRunning {
			clockProv.SetRunning(true)
		}
	}

	// Window menu — toggle visibility for each window we created.
	// Closing a window via the ■ glyph removes it from the manager
	// but keeps the *foxpro.Window alive (we hold a reference here),
	// so toggling adds the same instance back with its scroll
	// position and other state intact.
	windowItems := make([]foxpro.MenuItem, 0, len(windows))
	for _, w := range windows {
		w := w
		windowItems = append(windowItems, foxpro.MenuItem{
			Label: w.Title,
			OnSelect: func() {
				if app.Manager.Contains(w) {
					app.Manager.Remove(w)
				} else {
					app.Manager.Add(w)
				}
			},
		})
	}

	app.MenuBar = foxpro.NewMenuBar([]foxpro.Menu{
		// System menu — FoxPro for DOS convention, first slot.
		{
			Label: "&System",
			Items: []foxpro.MenuItem{
				{Label: "&About", OnSelect: func() { openAbout(app) }},
			},
		},
		{
			Label: "&File",
			Items: []foxpro.MenuItem{
				{Label: "&Load...", OnSelect: func() { openDemoPicker(app, &currentDemo, loadDemo, false) }},
				{Separator: true},
				{Label: "E&xit", Hotkey: "Esc", OnSelect: app.Quit},
			},
		},
		{
			Label: "&Run",
			Items: []foxpro.MenuItem{
				{Label: "R&un", Hotkey: "R", OnSelect: func() { sharedHub.CmdRun(0, 0) }},
				{Label: "S&top", Hotkey: ".", OnSelect: func() { sharedHub.CmdStop() }},
				{Label: "&Step instruction", Hotkey: "S", OnSelect: func() { sharedHub.CmdStep("instruction", 1) }},
				{Label: "&Tick (½ cycle)", Hotkey: "T", OnSelect: func() { sharedHub.CmdStep("halfcycle", 1) }},
			},
		},
		{
			Label: "&Hardware",
			Items: []foxpro.MenuItem{
				{Label: "&Reset", Hotkey: "Z", OnSelect: machineReset},
				{Separator: true},
				{Label: "&CPU...", OnSelect: func() { openCPUPicker(app, &currentCPU, switchCPU) }},
				{Label: "&Speed...", OnSelect: func() { openSpeedPicker(app, clockProv, currentCPU) }},
			},
		},
		{
			Label: "&View",
			Items: windowItems,
		},
		{
			Label: "&Window",
			Items: []foxpro.MenuItem{
				{Label: "&Command", Hotkey: "Ctrl+F2", OnSelect: app.ToggleCommandWindow},
				{Label: "C&ycle", Hotkey: "F6", OnSelect: app.Manager.FocusNext},
			},
		},
	})

	// Live tray — top-right of the menu bar. Three cells, each
	// clickable: speed (opens picker), running/stopped (toggles),
	// CPU (opens picker). The speed cell renders empty while the
	// clock is stopped — empty Compute strings are skipped by the
	// tray, so the bar reads "stopped │ CPU: …" without a stale
	// speed slot.
	app.MenuBar.Tray = []foxpro.TrayItem{
		{
			Compute: func() string {
				if !clockProv.Running() {
					return ""
				}
				sp := clockProv.Speed()
				if sp.Hz == 0 {
					return cpuwin.FormatHz(cpuProv.Rate())
				}
				return sp.Label
			},
			OnClick: func() { openSpeedPicker(app, clockProv, currentCPU) },
		},
		{
			Compute: func() string {
				if clockProv.Running() {
					return "running"
				}
				return "stopped"
			},
			OnClick: func() { clockProv.SetRunning(!clockProv.Running()) },
		},
		{
			Compute: func() string {
				return fmt.Sprintf("CPU: %s", currentCPU)
			},
			OnClick: func() { openCPUPicker(app, &currentCPU, switchCPU) },
		},
	}

	if *speedFlag != "" {
		hz := -1
		switch *speedFlag {
		case "1":
			hz = 1
		case "10":
			hz = 10
		case "100":
			hz = 100
		case "1k", "1000":
			hz = 1000
		case "max", "0":
			hz = 0
		}
		if hz < 0 || !clockProv.SetSpeedHz(hz) {
			fmt.Fprintf(os.Stderr, "unknown -speed=%q (want 1, 10, 100, 1k, max)\n", *speedFlag)
			os.Exit(2)
		}
	}

	if *runFlag {
		sharedHub.CmdRun(0, 0)
	}

	app.Run()
}

func must(err error) {
	if err != nil {
		log.Fatalf("setup: %v", err)
	}
}

// cpuPickerOptions returns the catalogue of CPU backends shown in
// the Hardware → CPU... dialog.
func cpuPickerOptions() []dialog.Option {
	return []dialog.Option{
		{
			Name:  "interp",
			Label: "Interpretive (fast)",
			Description: []string{
				"Conventional 151-opcode 6502 interpreter.",
				"Several MHz on a modern host. Bus pins aren't",
				"pin-accurate within an instruction.",
			},
		},
		{
			Name:  "netsim",
			Label: "Transistor (netsim)",
			Description: []string{
				"Visual6502 transistor-level netlist port.",
				"~3500 transistors per half-cycle, ~26 kHz.",
				"Every pin matches the behavior of real silicon.",
			},
		},
	}
}

// openCPUPicker pops the Hardware → CPU... dialog. current points at
// the host's currentCPU variable so the live value is read at open
// time.
func openCPUPicker(a *foxpro.App, current *string, switchCPU func(name string)) {
	sw, sh := a.Screen.Size()
	var w *foxpro.Window
	w = dialog.NewWindow("Choose CPU", cpuPickerOptions(), *current, switchCPU, nil, sw, sh)
	w.OnClose = func() { a.Manager.Remove(w) }
	a.Manager.Add(w)
}

// openDemoPicker pops the File → Load… dialog with all demos that
// are visible to this build (terminal hides RequiresGraphics demos).
// Picking one calls loadDemo with the matching Demo value. current
// points at the host's currentDemo string so the live value is read
// at open time and the matching row is pre-highlighted.
func openDemoPicker(a *foxpro.App, current *string, loadDemo func(demos.Demo), allowGraphics bool) {
	sw, sh := a.Screen.Size()
	opts := []dialog.Option{}
	byName := map[string]demos.Demo{}
	for _, sec := range demos.Sections() {
		for _, d := range sec.Demos {
			if d.RequiresGraphics && !allowGraphics {
				continue
			}
			opts = append(opts, dialog.Option{
				Name:        d.Name,
				Label:       stripAccel(d.Name),
				Description: d.Description,
			})
			byName[d.Name] = d
		}
	}
	var w *foxpro.Window
	w = dialog.NewWindow("Load Demo", opts, *current, func(name string) {
		if d, ok := byName[name]; ok {
			loadDemo(d)
		}
	}, nil, sw, sh)
	w.OnClose = func() { a.Manager.Remove(w) }
	a.Manager.Add(w)
}

// openSpeedPicker pops the Hardware → Speed... dialog letting the
// user pick a clock speed (the same set the < / > keys cycle).
// Speeds beyond the current backend's plausible ceiling are
// filtered out — netsim caps around 26 kHz, so offering 100 kHz or
// 1 MHz would just run at netsim's max while the tray label lied
// about the configured rate. cpuMaxSettableHz is the cutoff.
// Descriptions are intentionally omitted — labels are self-evident,
// and the picker auto-collapses to a compact box.
func openSpeedPicker(a *foxpro.App, clockProv *clockwin.Provider, currentCPU string) {
	sw, sh := a.Screen.Size()
	current := clockProv.Speed().Label
	limit := cpuMaxSettableHz(currentCPU)
	opts := make([]dialog.Option, 0, len(clockwin.Speeds))
	for _, sp := range clockwin.Speeds {
		if sp.Hz != 0 && limit > 0 && sp.Hz > limit {
			continue
		}
		opts = append(opts, dialog.Option{Name: sp.Label, Label: sp.Label})
	}
	var w *foxpro.Window
	w = dialog.NewWindow("Clock Speed", opts, current, func(name string) {
		for _, sp := range clockwin.Speeds {
			if sp.Label == name {
				clockProv.SetSpeedHz(sp.Hz)
				return
			}
		}
	}, nil, sw, sh)
	w.OnClose = func() { a.Manager.Remove(w) }
	a.Manager.Add(w)
}

// cpuMaxSettableHz returns the highest non-Max speed entry that
// makes sense for the given backend. 0 = no cap (interp can hit
// every entry). Netsim caps at 10 kHz: its real ceiling is around
// 26 kHz, so 10 kHz is achievable while 100 kHz / 1 MHz are not.
func cpuMaxSettableHz(name string) int {
	if name == "netsim" {
		return 10000
	}
	return 0
}

// stripAccel removes the FoxPro accelerator-marker '&' from a label
// so picker rows display "Marquee (default)" instead of
// "&Marquee (default)". Two consecutive '&&' would represent a
// literal '&', but no demo names use that.
func stripAccel(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '&' {
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

// openAbout pops a draggable text window summarizing what this
// simulator is. Placeholder copy for now — fill in over time.
func openAbout(a *foxpro.App) {
	body := foxpro.NewTextProvider([]string{
		"6502 Simulator",
		"",
		"A floating-window 6502 microcomputer simulator.",
		"Same code runs as a terminal app and as a",
		"WebAssembly build.",
		"",
		"Hardware:",
		"  CPU      MOS 6502 (interp + transistor netsim)",
		"  RAM/ROM  8 KB each",
		"  Video    40x13 char + 160x100 graphics plane",
		"  I/O      65C22 VIA, T1 free-running on its own crystal",
		"",
		"Built on foxpro-go (TUI framework) and",
		"6502-netsim-go (Visual6502 transistor port).",
		"",
		"Source: github.com/carledwards/go6sim",
	})
	w := foxpro.NewWindow("About", foxpro.Rect{X: 30, Y: 4, W: 56, H: 20}, body)
	a.Manager.Add(w)
}
