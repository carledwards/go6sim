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
	"fmt"
	"time"

	foxpro "github.com/carledwards/foxpro-go"
	"github.com/carledwards/foxpro-go/dialog"
	"github.com/carledwards/foxpro-go/wasm"
	"github.com/gdamore/tcell/v2"

	"github.com/carledwards/go6sim/asm"
	"github.com/carledwards/go6sim/backplane"
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

	// SimulationScreen sized to fit every window plus menu+status rows.
	// 140×32 covers the widest layout (display window ends at col 136).
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		panic(err)
	}
	s.SetSize(140, 32)
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
	cpuWindow := addWindow(cpuTitle,
		foxpro.Rect{X: 2, Y: 1, W: 38, H: 13},
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
	// default — shown when the user enables it from the Window menu.
	// 128 cells of canvas = 128 visible samples. The run loop
	// auto-tunes Decimate based on the current CPU speed (see
	// scopeDecimate below) so the visible time window stays
	// useful across the full speed range without sacrificing
	// per-cycle resolution at low speeds.
	// 128 cells of canvas. WASM: graphics-mode pixel overlay,
	// 8 samples per cell → 1024 visible samples at high density.
	scopeProv := scopewin.New(128, true)
	scopeWin := addWindow("Logic Analyzer",
		foxpro.Rect{X: 1, Y: 1, W: 139, H: 30},
		scopeProv,
		scopewin.MinW, scopewin.MinH)
	app.Manager.Remove(scopeWin) // start hidden; toggle adds it back

	// Capture per-half-step bus state. Cheap closure, fired from
	// inside clockProv's HalfStep paths (Advance/Step*).
	// Yellow CLK pulses fire on the RISING EDGE of SYNC — i.e. the
	// half-step where opcode fetch begins. Edge-detect rather than
	// level-detect because netsim holds SYNC high for the full T1
	// cycle (2 half-steps) while interp synthesizes a 1-half-step
	// pulse. Edge-detection gives exactly one yellow per
	// instruction on both backends, with matching spacing.
	prevSync := false
	clockProv.OnHalfStep = func() {
		s := backend.SYNC()
		edge := s && !prevSync
		prevSync = s
		scopeProv.Capture(backend.AddressBus(), backend.DataBus(), edge)
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
	cpuProv.OnReset = machineReset

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
	dispWindow := addWindow(dispTitle,
		foxpro.Rect{X: 60, Y: 1, W: 77, H: 22},
		dispProv,
		displaywin.MinW, displaywin.MinH)
	dispProv.Window = dispWindow // lets Provider append [TEXT]/[GFX] to the title each Draw

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
	const subTicks = 10
	subPeriod := tickPeriod / subTicks

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
		scopeProv.Decimate = scopeDecimate()
		for i := 0; i < subTicks; i++ {
			// inst.Advance == drv.Advance + bp.Tick, in lockstep —
			// the exact pairing this loop used to hand-duplicate.
			inst.Advance(subPeriod)
		}
	})

	// Global key bindings — same set as the TUI.
	app.OnKey = func(ev *tcell.EventKey) bool {
		if ev.Key() != tcell.KeyRune {
			return false
		}
		switch ev.Rune() {
		case 'r', 'R':
			clockProv.SetRunning(true)
			return true
		case '.':
			clockProv.SetRunning(false)
			return true
		case 's', 'S':
			clockProv.StepInstruction()
			return true
		case 't', 'T':
			clockProv.StepOne()
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
			clockProv.SetRunning(true)
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
		// CPU advance call, starving mouse and key events.
		clockProv.MaxBatch = autoTune(newBackend, 35*time.Millisecond)
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
				{Label: "&Load...", OnSelect: func() { openDemoPicker(app, &currentDemo, loadDemo, true) }},
			},
		},
		{
			Label: "&Run",
			Items: []foxpro.MenuItem{
				{Label: "R&un", Hotkey: "R", OnSelect: func() { clockProv.SetRunning(true) }},
				{Label: "S&top", Hotkey: ".", OnSelect: func() { clockProv.SetRunning(false) }},
				{Label: "&Step instruction", Hotkey: "S", OnSelect: clockProv.StepInstruction},
				{Label: "&Tick (½ cycle)", Hotkey: "T", OnSelect: clockProv.StepOne},
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

	clockProv.SetRunning(true) // browser visitors expect motion right away
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
// the current backend's plausible ceiling are filtered out — netsim
// caps around 26 kHz, so offering 100 kHz / 1 MHz would just run at
// netsim's max while the tray label lied. cpuMaxSettableHz is the
// cutoff. Descriptions are intentionally omitted — labels are self-
// evident, and the picker auto-collapses to a compact box.
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
// makes sense for the given backend. 0 = no cap (interp). Netsim
// caps at 10 kHz: its real ceiling is around 26 kHz so 10 kHz is
// achievable while 100 kHz / 1 MHz are not.
func cpuMaxSettableHz(name string) int {
	if name == "netsim" {
		return 10000
	}
	return 0
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
