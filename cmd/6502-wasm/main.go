//go:build js && wasm

// 6502-wasm is the browser port of the 6502 simulator.
//
// Same widgets, same demos, same dual-CPU backend (interp + netsim) as
// cmd/6502-sim — only the host changes. Foxpro-go's wasm bridge does
// the heavy lifting (SimulationScreen → JS canvas, browser keys/mouse
// → tcell events), so this file is mostly the same wiring as the TUI
// main, with terminal-only bits stripped (flag parsing, profiling).
//
// Defaults differ from the terminal build because they're host-tuned:
//
//   - CPU: interp (netsim is slow in wasm; user can swap via menu)
//   - Auto-start running: yes (browser visitors expect motion)
//   - QuitKeys: cleared (Esc/Ctrl+Q would kill the wasm runtime)
//   - BackgroundDragChords: shift+click only (right-click = native menu)
package main

import (
	"context"
	"fmt"
	"syscall/js"
	"time"

	foxpro "github.com/carledwards/foxpro-go"
	"github.com/carledwards/foxpro-go/dialog"
	"github.com/carledwards/foxpro-go/wasm"
	"github.com/gdamore/tcell/v2"

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
	"github.com/carledwards/go6sim/internal/monitor"
	"github.com/carledwards/go6sim/ui/clockwin"
	"github.com/carledwards/go6sim/ui/cpuwin"
	"github.com/carledwards/go6sim/ui/visualcpuwin"
	"github.com/carledwards/go6sim/ui/displaywin"
	"github.com/carledwards/go6sim/ui/ramwin"
	"github.com/carledwards/go6sim/ui/scopewin"
	"github.com/carledwards/go6sim/ui/viawin"
)

// Memory map. The whole VIC lives in a single contiguous 16 KB region
// at $A000–$DFFF — color + char + controller in low sub-blocks,
// graphics in the upper 8 KB. Sub-blocks land on natural 1 KB / 256 B
// boundaries so 6502 indexed addressing modes line up cleanly.
//
//	$A000–$A3FF  color plane     (1 KB block, 520 bytes used)
//	$A400–$A7FF  char plane      (1 KB block, 520 bytes used)
//	$A800–$ABFF  VIC controller  (1 KB CS region, 16 B used)
//	$AC00–$AFFF  reserved        (1 KB; future VIC expansion)
//	$B000–$B0FF  6522 VIA #1     (256 B CS region; 16 regs mirror ×16)
//	$B100–$BFFF  peripheral slots (15 × 256 B; VIA #2, ACIA, …)
//	$C000–$DF7F  graphics        (8 KB block, 160×100 @ 4bpp = 8000 bytes)
//
// Decoder model: a 74HC138 on A13/A14/A15 → 8 × 8 KB CS lines, with
// a second 74HC138 on A8-A11 (enabled by CS5_bar) breaking the I/O
// zone into 256-byte sub-CS lines. Each peripheral lives on its own
// CS line — no two chips share a region.
const (
	ramBase   = 0x0000
	ramSize   = 0x2000 // 8 KB at $0000-$1FFF
	colorBase = 0xA000 // VIC color plane  (520 bytes)
	charBase  = 0xA400 // VIC char plane   (520 bytes)
	ctrlBase  = 0xA800 // VIC controller registers (16 bytes used)
	viaBase   = 0xB000 // 6522 VIA #1 (own CS, mirrors every 16 bytes)
	dispW     = 40
	dispH     = 13
	gfxBase   = 0xC000
	gfxW      = 160
	gfxH      = 100
	gfxBPP    = 4
	romBase   = 0xE000
	romSize   = 0x2000
)

// tuneCandidates and autoTune mirror cmd/6502-sim. Browser tick
// budgets are slightly tighter, but the same auto-tune loop applies —
// it just lands on a smaller batch number.
var tuneCandidates = []int{500, 1000, 1500, 2000, 2500, 3000, 4000, 5000, 7500, 10000, 20000, 50000, 100000}

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
		break
	}
	return best
}

const tickPeriod = 50 * time.Millisecond

func main() {
	// Bus + memory map. Outer TraceBus stamps reads/writes for the
	// memory viewer's "recently touched" tinting.
	bp := backplane.New()
	innerBus := bp.Trace().Inner() // raw bus: untraced provider reads + Components()
	b := bp                        // the backplane (IS bus.Bus + Tick + Attach)
	mainRAM := ram.New("ram", ramBase, ramSize)
	colorPlane := display.New("display.color", colorBase, dispW, dispH)
	charPlane := display.New("display.char", charBase, dispW, dispH)
	mainROM := rom.New("rom", romBase, romSize)

	paintInitialDisplay := func() {
		for y := 0; y < dispH; y++ {
			for x := 0; x < dispW; x++ {
				colorPlane.SetPixel(x, y, uint8(((x+y)%16)<<4))
				charPlane.SetPixel(x, y, 0x20)
			}
		}
	}
	paintInitialDisplay()

	gfxPlane := display.NewGraphicsPlane(display.GraphicsConfig{
		Name:   "display.gfx",
		Base:   gfxBase,
		Width:  gfxW,
		Height: gfxH,
		BPP:    gfxBPP,
	})
	dispCtrl := display.NewControllerWithGraphics("display.ctrl", ctrlBase, colorPlane, charPlane, gfxPlane)

	// Phase-1 VIA: a 65C22 chip clocked from its own 1 MHz oscillator
	// (independent of the CPU clock). Demos use Timer 1 in
	// free-running mode to pace animation. The chip keeps ticking
	// while the CPU is halted/stepping — so step-debugging through
	// pacing code observes timer registers advance, just like real
	// hardware with a separate timer crystal.
	via1 := via.New("via1", viaBase, 1_000_000)

	// Boot directly into BouncingBalls — graphics is always wired in
	// the wasm build (gfxPlane registered above), so this is the
	// most visually interesting first impression. Other demos
	// (Marquee, Bouncer, etc.) are still selectable via the Demo
	// menu. The terminal build (cmd/6502-sim) keeps Marquee as its
	// default since it doesn't currently include a graphics plane.
	bootDemo := demos.BouncingBallsDemo
	// currentDemo tracks which demo is currently loaded so the
	// File → Load… dialog can pre-highlight it.
	currentDemo := bootDemo.Name
	must(mainROM.Load(0, bootDemo.Bytes))
	must(mainROM.SetResetVector(0xE000))
	must(b.Attach(mainRAM))
	must(b.Attach(colorPlane))
	must(b.Attach(charPlane))
	must(b.Attach(dispCtrl))
	must(b.Attach(gfxPlane))
	must(b.Attach(via1))
	must(b.Attach(mainROM))

	buildBackend := func(name string) (cpu.Backend, error) {
		switch name {
		case "netsim":
			return netsim.New(b)
		case "interp":
			return interp.New(b), nil
		}
		return nil, fmt.Errorf("unknown cpu %q (want netsim or interp)", name)
	}

	currentCPU := "interp" // wasm default; interp is fast enough to be lively
	backend, err := buildBackend(currentCPU)
	if err != nil {
		panic(err)
	}
	backend.Reset()
	cpuTitle := fmt.Sprintf("CPU (%s)", currentCPU)

	// SimulationScreen size — desktop is 160×40 (gives the Monitor
	// pane room to sit alongside CPU/VIA/Video without overlapping).
	// Mobile (≤720 px viewport at load time) drops to 140×35 so the
	// canvas fits a phone screen with fewer cells, each rendered at
	// a smaller fontPx by sim.js. Layout is one-shot at boot — a JS
	// resize / orientation change doesn't reflow the windows.
	layout := pickLayout(detectMobile())
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		panic(err)
	}
	s.SetSize(layout.cols, layout.rows)
	s.EnableMouse()

	app := foxpro.NewAppWithScreen(s)

	// Browser-appropriate settings.
	app.Settings.QuitKeys = nil
	app.Settings.BackgroundDragChords = []foxpro.BackgroundDragChord{
		{Button: tcell.Button1, Mods: tcell.ModShift},
	}
	app.Settings.StatusBarLeft = " Esc to close "

	// Track every window we create so we can toggle visibility from
	// the Window menu after a close.
	var windows []*foxpro.Window
	addWindow := func(tag, title string, bounds foxpro.Rect, content foxpro.ContentProvider, minW, minH int) *foxpro.Window {
		w := foxpro.NewWindow(title, bounds, content)
		w.Tag = tag
		w.MinW = minW
		w.MinH = minH
		app.Manager.Add(w)
		windows = append(windows, w)
		return w
	}

	pcHighlight := func() (uint16, bool) {
		return backend.Registers().PC, true
	}

	// componentSymbols collects register-layout symbols from any
	// bus.Labeller-implementing peripheral (VIA, VIC, …). Stable for
	// the lifetime of the process — components don't relocate.
	componentSymbols := func() []asm.Symbol {
		out := []asm.Symbol{}
		for _, c := range innerBus.Components() {
			if l, ok := c.(bus.Labeller); ok {
				out = append(out, l.Symbols()...)
			}
		}
		return out
	}
	hwSyms := componentSymbols()

	// mergeSymbols combines a demo's program-local symbols (variables,
	// subroutine entry points) with the hardware register symbols, so
	// the memory windows can show labels for both regions in one
	// unified table — region-filtered at render time.
	mergeSymbols := func(demoSyms []asm.Symbol) []asm.Symbol {
		out := make([]asm.Symbol, 0, len(demoSyms)+len(hwSyms))
		out = append(out, demoSyms...)
		out = append(out, hwSyms...)
		return out
	}

	cpuProv := &cpuwin.Provider{Backend: backend}
	// Closure over currentCPU so the chip-body button auto-shows /
	// hides on backend swap without a separate refresh path.
	cpuProv.IsTransistorBackend = func() bool { return currentCPU == "netsim" }
	cpuWindow := addWindow("cpu", cpuTitle,
		layout.cpu,
		cpuProv,
		cpuwin.MinW, cpuwin.MinH)

	ramProv := &ramwin.Provider{
		Bus:          innerBus,
		Trace:        bp.Trace(),
		Backend:      backend,
		Base:         0x0000,
		Length:       0x100,
		Highlight:    pcHighlight,
		EditableBase: true,
		Symbols:      mergeSymbols(bootDemo.Symbols),
		Annotations:  bootDemo.Annotations,
	}
	memWin := addWindow("memory", "Memory ($0000)",
		layout.ram,
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
	romWin := addWindow("rom", "Memory · disasm ($E000 - $FFFF)",
		layout.rom,
		romProv,
		ramwin.MinW, ramwin.MinH)
	romProv.Window = romWin

	// One shared clock driver: the clock window renders it, the
	// instrument is its facade. CPU drive lives on the Hub Pump
	// below; the driver carries speed / OnHalfStep / MaxBatch only.
	drv := clock.NewDriver(backend)
	clockProv := clockwin.NewProviderWithDriver(drv)
	inst := instrument.New(bp, drv)
	// Clock window omitted — same as the native sim TUI. The tray
	// indicator (top-right of the menu bar) shows running/stopped +
	// current Hz and is clickable; Run / Hardware menus + R/S/T/Z
	// hotkeys cover every action. clockProv stays alive as the
	// speed/MaxBatch/OnHalfStep holder.

	// Hub-driven execution — same pattern as cmd/6502-sim. CPU +
	// peripheral driving lives entirely on the Pump goroutine; the
	// app.Tick callback below no longer calls inst.Advance. Lets us
	// expose a bridge.HubDirect so the in-process Monitor can talk
	// to the same vocabulary the wire-client Monitor uses.
	wasmRegions := []bridge.Region{
		{Name: "ram", Lo: ramBase, Hi: ramBase + ramSize - 1, ReadOnly: false},
		{Name: "color", Lo: colorBase, Hi: charBase - 1, ReadOnly: false},
		{Name: "char", Lo: charBase, Hi: ctrlBase - 1, ReadOnly: false},
		{Name: "video", Lo: ctrlBase, Hi: ctrlBase + 0x0F, ReadOnly: false},
		{Name: "via1", Lo: viaBase, Hi: viaBase + 0x0F, ReadOnly: false},
		{Name: "gfx", Lo: gfxBase, Hi: gfxBase + 0x1FFF, ReadOnly: false},
		{Name: "rom", Lo: romBase, Hi: romBase + romSize - 1, ReadOnly: true},
	}
	sharedHub := bridge.NewHub(inst, wasmRegions, "sim-wasm")
	hubCtx, cancelHub := context.WithCancel(context.Background())
	go sharedHub.Run(hubCtx)
	defer cancelHub()

	viaProv := &viawin.Provider{VIA: via1, Base: viaBase}
	viaTitle := fmt.Sprintf("VIA 1 — $%04X @ %s", viaBase, viawin.FormatHz(via1.CrystalHz()))
	viaWin := addWindow("via", viaTitle,
		layout.via,
		viaProv,
		viawin.MinW, viawin.MinH)

	// Monitor — same shared REPL as cmd/6502-control + cmd/6502-sim.
	// HubDirect plugs the wasm sim's Hub straight into the Target
	// interface without TCP. Hidden by default; toggleable from the
	// Window menu — wasm screen real estate is precious.
	simHost := newSimHost(
		"Sim WASM (shared)",
		"the browser TUI's live machine — in-process via HubDirect",
		wasmRegions,
	)
	hubDirect := bridge.NewHubDirect(sharedHub)
	mon := monitor.New(simHost, hubDirect)
	monitorWin := addWindow("monitor", "Monitor",
		layout.monitor,
		mon,
		20, 5)
	// Monitor is visible on startup in the wasm build — the
	// browser landing experience benefits from the REPL being
	// discoverable without a menu hunt. Users can close via the
	// chrome's X (or the Window menu's toggle).
	mon.AddEvent("system", "Monitor opened (in-process target)")
	mon.AddEvent("system", "type 'help' or '?' for commands")
	go consumeHubDirectNotifications(hubDirect, mon)

	// Logic-analyzer scope: 256 cycles of trace history. Hidden by
	// default — shown when the user enables it from the Window menu.
	// 128 cells of canvas = 128 visible samples. The run loop
	// auto-tunes Decimate based on the current CPU speed (see
	// scopeDecimate below) so the visible time window stays
	// useful across the full speed range without sacrificing
	// per-cycle resolution at low speeds.
	// 128 cells of canvas. WASM: graphics-mode pixel overlay,
	// 8 samples per cell → 1024 visible samples at high density.
	scopeProv := scopewin.New(128, true)
	scopeWin := addWindow("scope", "Logic Analyzer",
		layout.scope,
		scopeProv,
		scopewin.MinW, scopewin.MinH)
	app.Manager.Remove(scopeWin) // start hidden; toggle adds it back

	// Visual 6502 — live transistor-level chip view. Hidden by
	// default; opened by clicking the "<< Visual 6502 >>" button
	// inside the CPU widget's chip body. Phase 1 paints a
	// placeholder / "switch CPU" hint; Phase 2 will overlay the
	// segdefs polygons coloured by netsim node state.
	visualProv := visualcpuwin.New()
	visualProv.Backend = backend
	visualProv.IsTransistorBackend = func() bool { return currentCPU == "netsim" }
	visualWin := addWindow("visual", "Visual 6502 — click image to toggle overlay",
		// Square-ish window sized for the eventual die rendering.
		// Placed to overlap the scope's default slot; both start
		// hidden so the user only sees the one they ask for.
		foxpro.Rect{X: 10, Y: 2, W: 80, H: 30},
		visualProv,
		visualcpuwin.MinW, visualcpuwin.MinH)
	app.Manager.Remove(visualWin) // start hidden; opens on button click

	// 50 ms (~20 Hz) redraw heartbeat while the Visual 6502 window
	// is open — same idle-refresh pattern as the scope. Without it
	// the live overlay only updates on UI events; with the CPU
	// stepping in the background you'd see stale wire state until
	// you wiggled the mouse. Started / stopped by the click handler
	// and the chrome ■ close.
	var visualTick func()
	startVisualTick := func() {
		if visualTick != nil {
			return
		}
		visualTick = app.Tick(50*time.Millisecond, nil)
	}
	stopVisualTick := func() {
		if visualTick == nil {
			return
		}
		visualTick()
		visualTick = nil
	}
	visualWin.OnClose = func() {
		app.Manager.Remove(visualWin)
		stopVisualTick()
	}

	cpuProv.OnVisualClick = func() {
		if app.Manager.Contains(visualWin) {
			app.Manager.Raise(visualWin) // already open → bring forward
			return
		}
		app.Manager.Add(visualWin)
		app.Manager.Raise(visualWin)
		startVisualTick()
	}

	// Scope-visible redraw heartbeat — 50 ms (~20 Hz). See
	// cmd/6502-sim/main.go for why 30 Hz was too aggressive; the
	// wasm canvas's own requestAnimationFrame paints independently
	// so this only governs how often the Go side hands a fresh
	// snapshot to JS.
	var scopeTick func()
	startScopeTick := func() {
		if scopeTick != nil {
			return
		}
		scopeTick = app.Tick(50*time.Millisecond, nil)
	}
	stopScopeTick := func() {
		if scopeTick == nil {
			return
		}
		scopeTick()
		scopeTick = nil
	}
	scopeWin.OnClose = func() {
		app.Manager.Remove(scopeWin)
		stopScopeTick()
	}

	// VIA and the memory-disasm window are hidden by default — the
	// Monitor REPL covers most disassembly needs, and the VIA
	// internals are an "advanced" view. Both stay in the Window menu
	// and toggle back on with a click.
	app.Manager.Remove(viaWin)

	// Capture per-half-step bus state. Cheap closure, fired from
	// inside clockProv's HalfStep paths (Advance/Step*).
	// Yellow CLK pulses fire on the RISING EDGE of SYNC — i.e. the
	// half-step where opcode fetch begins. Edge-detect rather than
	// level-detect because netsim holds SYNC high for the full T1
	// cycle (2 half-steps) while interp synthesizes a 1-half-step
	// pulse. Edge-detection gives exactly one yellow per
	// instruction on both backends, with matching spacing.
	prevSync := false
	// scopeWasTooFast — see cmd/6502-sim/main.go for the rationale.
	// Clears the ring once on the down-transition so the trace grows
	// in from the right when the user slows the clock back down.
	scopeWasTooFast := false
	clockProv.OnHalfStep = func() {
		if !app.Manager.Contains(scopeWin) {
			return
		}
		tooFast := scopeProv.Decimate > 8
		if tooFast {
			scopeWasTooFast = true
			return
		}
		if scopeWasTooFast {
			scopeProv.Reset()
			scopeWasTooFast = false
		}
		s := backend.SYNC()
		edge := s && !prevSync
		prevSync = s
		// force=true when the clock isn't free-running: single-step
		// fires only a handful of half-cycles per `s` press, so the
		// speed-based Decimate throttle would swallow them entirely.
		scopeProv.Capture(backend.AddressBus(), backend.DataBus(), edge,
			backend.ReadCycle(), backend.IRQ(), backend.NMI(),
			!clockProv.Running())
	}

	// machineReset = simulated hardware reset button. Drops the VIC
	// pause/mode, clears RAM and graphics, resets the VIA, repaints
	// the display init pattern, then resets the CPU. The clock is
	// intentionally NOT stopped — a real reset button doesn't stop
	// the system clock, so if the simulator was running, the demo
	// restarts immediately.
	machineReset := func() {
		dispCtrl.Reset()
		gfxPlane.Clear(0)
		mainRAM.Reset()
		via1.Reset()
		scopeProv.Reset()
		paintInitialDisplay()
		clockProv.Reset()
	}

	dispProv := &displaywin.Provider{
		Bus:        innerBus,
		Controller: dispCtrl,
		ColorBase:  colorBase,
		CharBase:   charBase,
		CtrlBase:   ctrlBase,
		HasChars:   true,
		HasCtrl:    true,
		Width:      dispW,
		Height:     dispH,
		Graphics:   gfxPlane,
	}
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
	dispTitle := fmt.Sprintf("Video $%04X-$%04X", colorBase, ctrlBase+8)
	dispWindow := addWindow("video", dispTitle,
		layout.video,
		dispProv,
		displaywin.MinW, displaywin.MinH)
	dispProv.Window = dispWindow // lets Provider append [TEXT]/[GFX] to the title each Draw

	// Video starts collapsed: just the framebuffer + the "[ ] Expanded
	// view" toggle row. Expanding reveals Status, the Video Mode
	// picker, AND the buttons grid (Frame Sync, Clear, Scroll/Rotate
	// …) all at once. Capture both axes so collapsing also shrinks
	// width down to the framebuffer's natural footprint, not just
	// height.
	expandedVideoW := dispWindow.Bounds.W
	expandedVideoH := dispWindow.Bounds.H
	collapsedVideoW := layout.videoCollapsedW
	collapsedVideoH := layout.videoCollapsedH
	dispWindow.Bounds.W = collapsedVideoW
	dispWindow.Bounds.H = collapsedVideoH
	dispProv.Expanded = false
	dispProv.OnToggleExpanded = func() {
		if dispProv.Expanded {
			dispWindow.Bounds.W = expandedVideoW
			dispWindow.Bounds.H = expandedVideoH
		} else {
			dispWindow.Bounds.W = collapsedVideoW
			dispWindow.Bounds.H = collapsedVideoH
		}
	}

	// Z-order: addWindow accumulates in code order, but the desired
	// stack (back-to-front) is RAM → CPU → VIA → Video → Monitor → ROM
	// so overlapping rectangles draw correctly. Raise each in that
	// order; each Raise brings the named window to the top of the
	// current stack, so the LAST Raise wins the front.
	app.Manager.Raise(memWin)
	app.Manager.Raise(cpuWindow)
	app.Manager.Raise(viaWin)
	app.Manager.Raise(dispWindow)
	app.Manager.Raise(monitorWin)
	app.Manager.Raise(romWin)

	// Run loop. App.Tick fires on the UI goroutine, so simulator
	// advancement, register reads, and bus reads all serialize
	// naturally — no locks needed.
	//
	// Sub-ticking: split each app.Tick into N slices and interleave
	// CPU advancement with bus.Tick calls. Without this, the VIA
	// timer's IFR T1 flag can only flip once per app.Tick batch —
	// so a polling-based demo with a large CPU batch (or netsim's
	// slow per-cycle backend) spends an entire batch in the wait
	// loop, never observing the underflow about to come. Sub-ticks
	// give the timer chip multiple chances to fire per batch, which
	// is also more faithful to real hardware where the timer's
	// crystal runs continuously.
	// subTicks/subPeriod removed: Hub Pump owns sub-cycle slicing.

	// Pick a scope sampling stride that matches the current CPU
	// speed. At low Hz the buffer fills slowly enough that we
	// can capture every half-cycle (100% sampling); at very high
	// rates the buffer would otherwise overwrite faster than the
	// eye can track. Tiers chosen to keep ~1024 visible
	// half-cycles regardless of speed.
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
		// CPU + peripheral driving lives on the Hub Pump (see
		// sharedHub above). This callback only nudges UI state that
		// the foxpro draw loop needs on a steady cadence — scope
		// decimation auto-tunes off the current speed.
		scopeProv.Decimate = scopeDecimate()
	})

	// Global key bindings — same set as the TUI. Skipped when the
	// Monitor's REPL is focused so typed verbs flow into its input.
	app.OnKey = func(ev *tcell.EventKey) bool {
		if ev.Key() != tcell.KeyRune {
			return false
		}
		if app.Manager.Active() == monitorWin {
			return false
		}
		switch ev.Rune() {
		case 'r', 'R':
			sharedHub.CmdRun(0, 0)
			return true
		case '.':
			sharedHub.CmdStop()
			return true
		case 's', 'S':
			func() { sharedHub.CmdStep("instruction", 1) }()
			return true
		case 't', 'T':
			func() { sharedHub.CmdStep("halfcycle", 1) }()
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
		sharedHub.CmdStop()
		dispCtrl.Reset()
		gfxPlane.Clear(0)
		via1.Reset()
		scopeProv.Reset()
		mainROM.Clear()
		_ = mainROM.Load(0, d.Bytes)
		_ = mainROM.SetResetVector(0xE000)
		// Surface this demo's symbols + annotations to the memory
		// windows so the Labels view and the Disasm column reflect
		// the live program.
		merged := mergeSymbols(d.Symbols)
		ramProv.Symbols = merged
		ramProv.Annotations = d.Annotations
		romProv.Symbols = merged
		romProv.Annotations = d.Annotations
		paintInitialDisplay()
		clockProv.Reset()
		currentDemo = d.Name
		// Wasm always runs on load — visitors expect motion. The
		// wasRunning capture above is consumed here as: if the
		// clock had already been started (i.e. mid-session swap),
		// keep going; on first boot before the auto-start it'll
		// be false and the explicit start at end of main() will
		// kick things off.
		if wasRunning {
			sharedHub.CmdRun(0, 0)
		}
	}

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
		sharedHub.CmdStop()
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
		visualProv.Backend = newBackend
		// Re-tune MaxBatch for the new backend. Netsim is ~30x
		// slower per cycle than interp, so reusing the previous
		// batch size would spend the entire UI tick inside a single
		// CPU advance call, starving mouse and key events.
		clockProv.MaxBatch = autoTune(newBackend, 35*time.Millisecond)
		currentCPU = name
		cpuWindow.Title = fmt.Sprintf("CPU (%s)", name)
		// If the prior speed is above the new backend's plausible
		// ceiling (e.g. switching from interp@1MHz to netsim, which
		// caps near 6 kHz under wasm), drop to Max so the displayed
		// speed matches what's actually delivered.
		if limit := newBackend.MaxHz(); limit > 0 {
			if cur := clockProv.Speed().Hz; cur != 0 && cur > limit {
				clockProv.SetSpeedHz(0)
			}
		}
		machineReset()
		if wasRunning {
			sharedHub.CmdRun(0, 0)
		}
	}

	// setWindowVisible is the single chokepoint for show/hide so the
	// scope tick start/stop side effect happens consistently across
	// the menu, the visual-button hook, and Quick Pick restore.
	// Always raises on visible (even if already mounted) so callers
	// can walk a list of windows in bottom-to-top order and have the
	// accumulated raises produce the intended z-stack.
	setWindowVisible := func(w *foxpro.Window, visible bool) {
		contained := app.Manager.Contains(w)
		if visible {
			if !contained {
				app.Manager.Add(w)
				if w == scopeWin {
					startScopeTick()
				}
			}
			app.Manager.Raise(w)
		} else if contained {
			app.Manager.Remove(w)
			if w == scopeWin {
				stopScopeTick()
			}
		}
	}
	findWindowByTag := func(tag string) *foxpro.Window {
		for _, w := range windows {
			if w.Tag == tag {
				return w
			}
		}
		return nil
	}

	// Quick Pick presets — surfaced as buttons under the canvas via
	// the JS bridge below. Each preset bundles a demo + CPU choice +
	// clock speed (+ optional window layout); clicking dispatches
	// onto the foxpro UI goroutine via app.Post so the mutations
	// happen on the thread that owns the simulator state.
	type windowSpec struct {
		Tag           string
		X, Y, W, H    int
		Visible       bool
	}
	type quickPick struct {
		Name    string // stable id (matches HTML data-name)
		Label   string // human-readable button caption
		Demo    demos.Demo
		CPU     string
		Hz      int
		Windows []windowSpec // optional — empty leaves layout alone
	}
	quickPicks := []quickPick{
		{Name: "marquee", Label: "Marquee · interp · 100 Hz", Demo: demos.MarqueeDemo, CPU: "interp", Hz: 100,
			Windows: []windowSpec{
				{Tag: "memory", X: 0, Y: 13, W: 78, H: 8, Visible: true},
				{Tag: "cpu", X: 0, Y: 1, W: 78, H: 11, Visible: true},
				{Tag: "video", X: 80, Y: 1, W: 44, H: 18, Visible: true},
				{Tag: "monitor", X: 80, Y: 24, W: 78, H: 14, Visible: true},
				{Tag: "rom", X: 0, Y: 22, W: 78, H: 16, Visible: true},
				{Tag: "via", X: 44, Y: 1, W: 56, H: 16, Visible: false},
				{Tag: "scope", X: 1, Y: 1, W: 139, H: 30, Visible: false},
				{Tag: "visual", X: 10, Y: 2, W: 80, H: 30, Visible: false},
			},
		},
		{Name: "balls", Label: "Bouncing Balls · interp · 1 MHz", Demo: demos.BouncingBallsDemo, CPU: "interp", Hz: 1_000_000,
			Windows: []windowSpec{
				{Tag: "memory", X: 0, Y: 13, W: 78, H: 8, Visible: true},
				{Tag: "cpu", X: 0, Y: 1, W: 78, H: 11, Visible: true},
				{Tag: "video", X: 80, Y: 1, W: 44, H: 18, Visible: true},
				{Tag: "monitor", X: 80, Y: 24, W: 78, H: 14, Visible: true},
				{Tag: "rom", X: 0, Y: 22, W: 78, H: 16, Visible: true},
				{Tag: "via", X: 44, Y: 1, W: 56, H: 16, Visible: false},
				{Tag: "scope", X: 1, Y: 1, W: 139, H: 30, Visible: false},
				{Tag: "visual", X: 10, Y: 2, W: 80, H: 30, Visible: false},
			},
		},
		{Name: "bouncer", Label: "Bouncer · netsim · 1 kHz", Demo: demos.BouncerDemo, CPU: "netsim", Hz: 1000,
			Windows: []windowSpec{
				{Tag: "cpu", X: 0, Y: 1, W: 78, H: 11, Visible: true},
				{Tag: "memory", X: 0, Y: 13, W: 78, H: 8, Visible: true},
				{Tag: "rom", X: 0, Y: 22, W: 78, H: 16, Visible: true},
				{Tag: "via", X: 44, Y: 1, W: 56, H: 16, Visible: false},
				{Tag: "monitor", X: 81, Y: 31, W: 76, H: 7, Visible: true},
				{Tag: "scope", X: 1, Y: 1, W: 139, H: 30, Visible: false},
				{Tag: "visual", X: 77, Y: 2, W: 80, H: 30, Visible: true},
				{Tag: "video", X: 26, Y: 18, W: 44, H: 18, Visible: true},
			},
		},
	}
	applyPreset := func(name string) {
		var qp *quickPick
		for i := range quickPicks {
			if quickPicks[i].Name == name {
				qp = &quickPicks[i]
				break
			}
		}
		if qp == nil {
			return
		}
		// Order: CPU first (so the speed clamp + reset happen with
		// the right backend), then speed (clockProv carries the
		// setpoint into loadDemo's wasRunning path), then demo
		// (which Resets + re-CmdRuns if we were running), then
		// layout (only addressed windows are touched; others keep
		// their current position + visibility).
		switchCPU(qp.CPU)
		clockProv.SetSpeedHz(qp.Hz)
		loadDemo(qp.Demo)
		for _, ws := range qp.Windows {
			w := findWindowByTag(ws.Tag)
			if w == nil {
				continue
			}
			w.Bounds = foxpro.Rect{X: ws.X, Y: ws.Y, W: ws.W, H: ws.H}
			setWindowVisible(w, ws.Visible)
		}
	}
	js.Global().Set("go6sim", js.ValueOf(map[string]any{
		"quickPicks": js.FuncOf(func(this js.Value, args []js.Value) any {
			out := make([]any, 0, len(quickPicks))
			for _, qp := range quickPicks {
				out = append(out, map[string]any{
					"name":  qp.Name,
					"label": qp.Label,
				})
			}
			return js.ValueOf(out)
		}),
		"applyPreset": js.FuncOf(func(this js.Value, args []js.Value) any {
			if len(args) == 0 {
				return nil
			}
			name := args[0].String()
			app.Post(func() { applyPreset(name) })
			return nil
		}),
		// snapshot returns the current live state in the same JSON
		// shape a Quick Pick preset accepts — design tool for adding
		// new presets: configure the sim by hand, call
		// window.go6sim.snapshot() in DevTools, paste the result
		// into the quickPicks table.
		"snapshot": js.FuncOf(func(this js.Value, args []js.Value) any {
			// Order matters: visible windows are listed bottom-to-top
			// in current z-order (Manager.AllWindows() is z-sorted
			// with the front-most window last) so that restoring in
			// list order — each Add/Raise pushes to top — reproduces
			// the visible layering. Hidden windows tag onto the end;
			// their relative order is irrelevant until shown.
			seen := map[*foxpro.Window]bool{}
			sw := make([]any, 0, len(windows))
			addEntry := func(w *foxpro.Window, visible bool) {
				sw = append(sw, map[string]any{
					"tag":     w.Tag,
					"title":   w.Title,
					"x":       w.Bounds.X,
					"y":       w.Bounds.Y,
					"w":       w.Bounds.W,
					"h":       w.Bounds.H,
					"visible": visible,
				})
				seen[w] = true
			}
			for _, w := range app.Manager.AllWindows() {
				addEntry(w, true)
			}
			for _, w := range windows {
				if !seen[w] {
					addEntry(w, false)
				}
			}
			return js.ValueOf(map[string]any{
				"cpu":     currentCPU,
				"speedHz": clockProv.Speed().Hz,
				"demo":    currentDemo,
				"windows": sw,
			})
		}),
	}))

	windowItems := make([]foxpro.MenuItem, 0, len(windows))
	for _, w := range windows {
		w := w
		windowItems = append(windowItems, foxpro.MenuItem{
			Label: w.Title,
			OnSelect: func() {
				if app.Manager.Contains(w) {
					app.Manager.Remove(w)
					if w == scopeWin {
						stopScopeTick()
					}
				} else {
					app.Manager.Add(w)
					if w == scopeWin {
						startScopeTick()
					}
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
				{Label: "&Load...", OnSelect: func() { openDemoPicker(app, &currentDemo, loadDemo, true) }},
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
				{Label: "&Speed...", OnSelect: func() { openSpeedPicker(app, clockProv) }},
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
				{Separator: true},
				{Label: "Toggle &Monitor", OnSelect: func() {
					if app.Manager.Contains(monitorWin) {
						app.Manager.Remove(monitorWin)
					} else {
						app.Manager.Add(monitorWin)
						app.Manager.Raise(monitorWin)
					}
				}},
			},
		},
	})

	// Plumb the SimHost's quit + help-window hooks now that app is
	// fully constructed. Wasm `q` / `quit` calls app.Quit which the
	// foxpro-wasm runtime handles gracefully.
	simHost.quit = app.Quit
	simHost.helpOpener = func() {
		hw := foxpro.NewWindow("Help — Monitor",
			foxpro.Rect{X: 6, Y: 2, W: 70, H: 28},
			foxpro.NewTextProvider(monitor.HelpTable))
		app.Manager.Add(hw)
	}

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
			OnClick: func() { openSpeedPicker(app, clockProv) },
		},
		{
			Compute: func() string {
				if clockProv.Running() {
					return "running"
				}
				return "stopped"
			},
			OnClick: func() {
				if clockProv.Running() {
					sharedHub.CmdStop()
				} else {
					sharedHub.CmdRun(0, 0)
				}
			},
		},
		{
			Compute: func() string {
				return fmt.Sprintf("CPU: %s", currentCPU)
			},
			OnClick: func() { openCPUPicker(app, &currentCPU, switchCPU) },
		},
	}

	// Auto-tune the batch size for this browser / device before
	// kicking the clock off. The same logic the CPU menu's
	// "Auto-tune Batch" item runs — happens once at boot so first-
	// frame motion lands on a sensible batch instead of the default
	// 500. autoTune advances cycles, so reset everything afterwards
	// to start the demo from a clean slate.
	clockProv.MaxBatch = autoTune(backend, 35*time.Millisecond)
	backend.Reset()
	mainRAM.Reset()
	paintInitialDisplay()
	clockProv.Reset()

	// Default to max speed — no Hz cap, run as many HalfSteps per
	// UI tick as the auto-tuned batch allows. Visitors land on a
	// page that's already at full motion; they can throttle via
	// the Clock window or '<' / '>' keys if they want a slower view.
	clockProv.SetSpeedHz(0)

	sharedHub.CmdRun(0, 0) // browser visitors expect motion right away
	wasm.Run(app, s)
}

func must(err error) {
	if err != nil {
		panic(err)
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
				"Several MHz in the browser. Bus pins aren't",
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

// openCPUPicker pops the Hardware → CPU... dialog.
func openCPUPicker(a *foxpro.App, current *string, switchCPU func(name string)) {
	sw, sh := a.Screen.Size()
	var w *foxpro.Window
	w = dialog.NewWindow("Choose CPU", cpuPickerOptions(), *current, switchCPU, nil, sw, sh)
	w.OnClose = func() { a.Manager.Remove(w) }
	a.Manager.Add(w)
}

// openDemoPicker pops the File → Load… dialog. wasm shows every
// demo (graphics or not — pixel plane is always available). current
// points at the host's currentDemo so the matching row is pre-
// highlighted.
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

// openSpeedPicker pops the Hardware → Speed... dialog. Speeds beyond
// the active backend's MaxHz() ceiling are filtered out so the tray
// label never lies — picking 1 MHz under netsim would just run at
// netsim's real ceiling (~6 kHz under wasm) while pretending
// otherwise. Descriptions are omitted — labels are self-evident and
// the picker auto-collapses.
func openSpeedPicker(a *foxpro.App, clockProv *clockwin.Provider) {
	sw, sh := a.Screen.Size()
	current := clockProv.Speed().Label
	limit := clockProv.Backend.MaxHz()
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
		"Same code runs as a terminal app and as this",
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

// winLayout collects every floating window's rect + the overall
// SimulationScreen size. Two presets (desktop + mobile) are picked
// once at boot from viewport width — the cell grid never reflows
// after that, so a phone-rotate or window-resize doesn't move
// anything. User refreshes the page if they want the other layout.
type winLayout struct {
	cols, rows                          int
	cpu, via, video, ram, monitor, rom  foxpro.Rect
	scope                               foxpro.Rect
	// videoCollapsedW / videoCollapsedH are the Video window's
	// dimensions when "[ ] Expanded view" is unchecked: width shrinks
	// to just framebuffer + borders (no Status / Mode picker), height
	// shrinks to fit framebuffer + the checkbox row (no buttons grid).
	// video.W / video.H are the expanded dimensions.
	videoCollapsedW, videoCollapsedH    int
}

// pickLayout returns the desktop (160×40) layout, or a tighter
// 140×35 layout for narrow viewports. Mobile rects are proportionally
// trimmed from the desktop ones; z-order at the bottom of main()
// (RAM → CPU → VIA → Video → Monitor → ROM) is the same on both so
// the visual stack reads identically.
func pickLayout(isMobile bool) winLayout {
	// Collapsed Video dimensions: just framebuffer (40×13 cells) +
	// chrome + the "[ ] Expanded view" toggle row directly below. Math:
	//   W = pxW(40) + 2 borders + 2 chrome = 44
	//   H = pxH(13) + 2 borders + 1 checkbox row + 2 chrome = 18
	const (
		collapsedVideoW = 44
		collapsedVideoH = 18
	)
	if isMobile {
		return winLayout{
			cols: 140, rows: 35,
			cpu:             foxpro.Rect{X: 0, Y: 1, W: 68, H: 11},
			via:             foxpro.Rect{X: 40, Y: 1, W: 52, H: 14},
			video:           foxpro.Rect{X: 70, Y: 1, W: 68, H: 20},
			ram:             foxpro.Rect{X: 0, Y: 13, W: 68, H: 8},
			monitor:         foxpro.Rect{X: 70, Y: 22, W: 68, H: 11},
			rom:             foxpro.Rect{X: 0, Y: 22, W: 68, H: 11},
			scope:           foxpro.Rect{X: 1, Y: 1, W: 138, H: 26},
			videoCollapsedW: collapsedVideoW,
			videoCollapsedH: collapsedVideoH,
		}
	}
	return winLayout{
		cols: 160, rows: 40,
		cpu:             foxpro.Rect{X: 0, Y: 1, W: 78, H: 11},
		via:             foxpro.Rect{X: 44, Y: 1, W: 56, H: 16},
		video:           foxpro.Rect{X: 80, Y: 1, W: 72, H: 22},
		ram:             foxpro.Rect{X: 0, Y: 13, W: 78, H: 8},
		monitor:         foxpro.Rect{X: 80, Y: 24, W: 78, H: 14},
		rom:             foxpro.Rect{X: 0, Y: 22, W: 78, H: 16},
		scope:           foxpro.Rect{X: 1, Y: 1, W: 139, H: 30},
		videoCollapsedW: collapsedVideoW,
		videoCollapsedH: collapsedVideoH,
	}
}

// detectMobile asks the browser if the viewport is narrow enough
// to count as a phone. Same 720 px cutover used by sim.js for the
// fontPx switch and by index.html for the info-card single-column
// reflow — one breakpoint across the page. Safe-guards if matchMedia
// isn't present (very old browsers) — we just return false.
func detectMobile() bool {
	win := js.Global().Get("window")
	if !win.Truthy() {
		return false
	}
	if mm := win.Get("matchMedia"); !mm.Truthy() {
		return false
	}
	res := win.Call("matchMedia", "(max-width: 720px)")
	if !res.Truthy() {
		return false
	}
	return res.Get("matches").Bool()
}
