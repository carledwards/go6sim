// Command 6502-control is a foxpro-go TUI "control console" for a
// remote go6sim bridge server. It is a pure client — every action
// goes over the wire (cmd/6502-sim-serve on the other end today,
// cmd/6502-sim --serve once that lands).
//
// Three windows:
//   ┌─ CONTROL ─┐    hotkey legend, connection status, last halt
//   ┌─ CPU ─────┐    live registers mirrored from state.snapshot
//   ┌─ EVENTS ──┐    scrolling notification log (taps, bp.hit, halts)
//
// Hotkeys (the "cool buttons" effect):
//   R   Run             T   Step
//   S   Stop            B   BP @ current PC
//   Q   Quit
//
// Defaults: --connect 127.0.0.1:6502 (matches cmd/6502-sim-serve),
//           --preset teach-min,
//           --program <file>  (optional raw bytes for ROM at $E000).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	foxpro "github.com/carledwards/foxpro-go"
	"github.com/gdamore/tcell/v2"

	"github.com/carledwards/go6sim/bridge"
	"github.com/carledwards/go6sim/internal/bridgeclient"
)

var (
	connect = flag.String("connect", "127.0.0.1:6502", "bridge server address")
	preset  = flag.String("preset", "teach-min", "machine preset to load")
	program = flag.String("program", "", "optional path to raw 6502 ROM image (loaded at $E000)")
)

func main() {
	flag.Parse()

	// 1. Connect + initialise the bridge.
	client, err := bridgeclient.Dial(*connect)
	if err != nil {
		fmt.Fprintf(os.Stderr, "6502-control: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	if _, e := client.Hello("6502-control", "0.0.0"); e != nil {
		fail("hello", e)
	}

	var image []byte
	var programName string
	if *program != "" {
		b, err := os.ReadFile(*program)
		if err != nil {
			fmt.Fprintf(os.Stderr, "6502-control: read --program: %v\n", err)
			os.Exit(1)
		}
		image = b
		programName = filepath.Base(*program)
	}
	mlr, e := client.MachineLoad(*preset, image)
	if e != nil {
		fail("machine.load", e)
	}
	if _, e := client.Subscribe("clock.halt", "bp.hit", "state.snapshot", "tap.*"); e != nil {
		fail("events.subscribe", e)
	}

	// 2. Seed the mirror with the post-load CPU state so the panel
	//    has something to draw before the first state.snapshot arrives.
	//    machineLabel falls back to the slug if the server didn't
	//    return one (older servers / bare presets).
	machineLabel := mlr.Label
	if machineLabel == "" {
		machineLabel = mlr.Preset
	}
	ctrl := newController(client, *connect, mlr.Preset, machineLabel, mlr.Summary, programName, image, mlr.Regions)
	if st, e := client.CPUState(); e == nil {
		ctrl.setState(st)
	}

	// 3. foxpro app + windows.
	app, err := foxpro.NewApp()
	if err != nil {
		fmt.Fprintln(os.Stderr, "6502-control: foxpro init:", err)
		os.Exit(1)
	}
	defer app.Close()
	foxpro.RegisterBuiltinCommands(app)

	controlWin := foxpro.NewWindow("Control",
		foxpro.Rect{X: 2, Y: 1, W: 38, H: 11},
		&controlProvider{c: ctrl})
	cpuWin := foxpro.NewWindow("CPU",
		foxpro.Rect{X: 42, Y: 1, W: 38, H: 11},
		&cpuProvider{c: ctrl})

	// Monitor — REPL pane. Dispatch runs ctrl.dispatch (commands.go);
	// quitSignal lets the `q` / `quit` command shut us down without
	// calling app.Quit from a foreign goroutine.
	quitCh := make(chan struct{}, 1)
	ctrl.quitSignal = quitCh
	// Open a persistent help window when requested via `help window`
	// or the Help menu. Captures app so the dispatch goroutine (which
	// doesn't otherwise see it) can pop the window from a command.
	ctrl.helpWindowFn = func() { openHelpWindow(app) }
	monitor := newMonitorProvider(ctrl, ctrl.dispatch)
	monitorWin := foxpro.NewWindow("Monitor",
		foxpro.Rect{X: 2, Y: 13, W: 78, H: 22},
		monitor)
	app.Manager.Add(controlWin)
	app.Manager.Add(cpuWin)
	app.Manager.Add(monitorWin)

	// Menu bar — FoxPro for DOS layout: System slot first, then the
	// activity-shaped menus (Control / View / Help). The Control menu
	// mirrors the hotkeys so mouse + keyboard users both have a way to
	// drive the sim; the View menu hosts window-focus + a "clear the
	// scrollback" convenience that's awkward to type as a command.
	app.MenuBar = foxpro.NewMenuBar([]foxpro.Menu{
		{
			Label: "&System",
			Items: []foxpro.MenuItem{
				{Label: "&About", OnSelect: func() { openAbout(app) }},
				{Separator: true},
				{Label: "E&xit", Hotkey: "Q", OnSelect: app.Quit},
			},
		},
		{
			Label: "&Control",
			Items: []foxpro.MenuItem{
				{Label: "R&un", Hotkey: "R", OnSelect: ctrl.run},
				{Label: "S&top", Hotkey: ".", OnSelect: ctrl.stop},
				{Label: "&Step instruction", Hotkey: "T", OnSelect: ctrl.step},
				{Separator: true},
				{Label: "&BP @ PC", Hotkey: "B", OnSelect: ctrl.bpAtPC},
				{Separator: true},
				{Label: "&Reset", Hotkey: "X", OnSelect: ctrl.reset},
			},
		},
		{
			Label: "&Window",
			Items: []foxpro.MenuItem{
				{Label: "Focus &Monitor", OnSelect: func() { app.Manager.Raise(monitorWin) }},
				{Label: "Focus &Control", OnSelect: func() { app.Manager.Raise(controlWin) }},
				{Label: "Focus C&PU", OnSelect: func() { app.Manager.Raise(cpuWin) }},
				{Separator: true},
				{Label: "&Cycle window", Hotkey: "F6", OnSelect: app.Manager.FocusNext},
				{Separator: true},
				{Label: "C&lear Monitor", OnSelect: func() { ctrl.clearEvents() }},
			},
		},
		{
			Label: "&Help",
			Items: []foxpro.MenuItem{
				{Label: "&Commands (window)", OnSelect: func() { openHelpWindow(app) }},
				{Label: "Commands (&inline)", OnSelect: func() { ctrl.cmdHelp("") }},
				{Label: "&Hotkeys", OnSelect: func() { showHotkeys(ctrl) }},
				{Separator: true},
				{Label: "&About", OnSelect: func() { openAbout(app) }},
			},
		},
	})

	// Live tray — top-right of the menu bar. Mirrors the Control
	// window's clock state so the user can glance up while typing in
	// Monitor and still know what the sim is doing. Cheap (one bool
	// read per frame).
	app.MenuBar.Tray = []foxpro.TrayItem{
		{
			Compute: func() string {
				_, _, running, _, _, _ := ctrl.snapshot()
				if running {
					return "running"
				}
				return "stopped"
			},
		},
	}

	// Startup banner — shows up at the bottom of the Monitor on first
	// paint so the user has connection + machine context before
	// typing anything.
	ctrl.addEvent("system", fmt.Sprintf("connected to %s", *connect))
	ctrl.addEvent("system", fmt.Sprintf("machine : %s", machineLabel))
	if programName != "" {
		ctrl.addEvent("system", fmt.Sprintf("program : %s (%d bytes)", programName, len(image)))
	}
	ctrl.addEvent("system", "type 'help' or '?'")

	// 4. Global hotkeys — fire when the Monitor is NOT the focused
	//    window. When focus is on Monitor, the user is typing
	//    commands; runes flow to the InputProvider and 'r' becomes
	//    part of the buffer rather than triggering Run(). Tab to
	//    leave Monitor to use hotkeys.
	app.OnKey = func(ev *tcell.EventKey) bool {
		if ev.Key() != tcell.KeyRune {
			return false
		}
		if app.Manager.Active() == monitorWin {
			return false // let the keystroke reach the InputProvider
		}
		switch ev.Rune() {
		case 'r', 'R':
			ctrl.run()
			return true
		case 's', 'S':
			ctrl.stop()
			return true
		case 't', 'T':
			ctrl.step()
			return true
		case 'b', 'B':
			ctrl.bpAtPC()
			return true
		case 'x', 'X':
			ctrl.reset()
			return true
		case 'q', 'Q':
			app.Quit()
			return true
		}
		return false
	}

	// 5. Notification consumer — drains client.Notifications() into
	//    ctrl state. The foxpro tick handler just repaints from
	//    whatever the latest mirror says.
	go ctrl.consumeNotifications()

	// Heal loop — watches the active client's Done() channel and
	// auto-reconnects with backoff if it dies. Sim crashes,
	// network blips, deliberate Ctrl-C of the sim and restart
	// during a long session — all recover without the user having
	// to bounce 6502-control.
	go ctrl.healLoop()

	// 6. Repaint at ~30 fps so live CPU + event log feel responsive.
	// app.Tick posts an empty callback onto the main loop; the loop
	// repaints after every posted event, so this is enough to nudge
	// the UI when notifications have updated controller state.
	app.Tick(33*time.Millisecond, func() {})

	// Quit watcher — listens for `q` / `quit` typed into the Monitor
	// and asks foxpro to shut down. Runs in its own goroutine so the
	// command dispatcher (on the foxpro loop) only needs a one-line
	// `quitCh <- struct{}{}` rather than the full app teardown.
	go func() {
		<-quitCh
		app.Quit()
	}()

	app.Run()
}

func fail(method string, e *bridge.Error) {
	fmt.Fprintf(os.Stderr, "6502-control: %s: [%d] %s\n", method, e.Code, e.Message)
	os.Exit(1)
}

// --- controller (shared mirror state) ---

// Controller is the single source of truth for what the three windows
// draw: the latest CPU state, recent events, last halt reason. Both
// the foxpro draw loop and the notification consumer touch it, so
// every read/write goes through mu.
type Controller struct {
	// client is an atomic.Pointer so the heal loop can swap in a
	// fresh *bridgeclient.Client without locking the dispatch
	// handlers. Readers do c.client.Load().Foo(); reconnect does
	// c.client.Store(newClient) under c.healMu.
	client atomic.Pointer[bridgeclient.Client]
	addr   string

	// Display fields, all sourced from the machine.load response
	// (with preset slug as a fallback). machineLabel is what the
	// user sees in the Control pane's "machine:" row;  buildSummary
	// describes hardware; programName + programSize describe the
	// optional --program payload the client uploaded.
	preset        string
	machineLabel  string
	buildSummary  string
	programName   string
	programSize   int

	// image holds the optional program bytes from --program so the
	// heal loop can re-upload after a reconnect. nil when no program
	// was specified at launch.
	image []byte

	// regions is the memory map the server returned from machine.load.
	// Used by `hw` / `info` to describe what's wired where, and by
	// `via` to locate VIA1's base without the user having to remember
	// the address. Immutable after construction.
	regions []bridge.Region

	mu         sync.Mutex
	state      bridge.CPUState
	stateSeen  bool
	running    bool
	lastHalt   string
	lastErr    string
	events     []eventLine
	eventsCap  int

	// connState tracks the bridge connection's lifecycle —
	// "connected", "reconnecting", "disconnected", "failed". Read by
	// the controlProvider for the status indicator; written by the
	// heal loop. Protected by its own tiny mutex so the foxpro Draw
	// path doesn't contend with the main controller mutex.
	connStateMu sync.Mutex
	connState   string

	// healMu serializes reconnect attempts so two competing healers
	// can't both try to Dial. There's only ever one heal goroutine
	// today but the lock makes that assumption explicit + cheap.
	healMu sync.Mutex

	// quitSignal is set by main(); when the user types `q` / `quit`
	// in the Monitor we drop a value on it so main can shut the
	// foxpro app down through its own goroutine (calling app.Quit
	// from a dispatch handler that's not foxpro's owner would race).
	quitSignal chan<- struct{}

	// helpWindowFn is set by main() once foxpro is wired up; calling
	// it opens the persistent help window. Lives on the Controller
	// (not as a global) so commands can invoke it from the dispatch
	// goroutine without piercing the app object.
	helpWindowFn func()
}

type eventLine struct {
	t    time.Time
	// kind controls both prefix and color in the Monitor scrollback.
	//   input   → "> "  (user-typed command, echoed)
	//   result  → ""    (command output, plain)
	//   system  → "i "  (banner / informational, dim)
	//   info    → "  "  (action confirmations from hotkeys, dim)
	//   halt    → "* "  (sim halt, yellow)
	//   bp      → "* "  (breakpoint hit, red bold)
	//   tap     → "* "  (tap.changed, aqua)
	//   err     → "! "  (anything wrong, red)
	kind string
	text string
}

func newController(c *bridgeclient.Client, addr, preset, label, summary, programName string, image []byte, regions []bridge.Region) *Controller {
	ctrl := &Controller{
		addr:         addr,
		preset:       preset,
		machineLabel: label,
		buildSummary: summary,
		programName:  programName,
		programSize:  len(image),
		image:        image,
		regions:      regions,
		eventsCap:    200,
		connState:    "connected",
	}
	ctrl.client.Store(c)
	return ctrl
}

// connStatus returns the current connection state for the Control
// pane indicator + the `reconnect` command's no-op short-circuit.
func (c *Controller) connStatus() string {
	c.connStateMu.Lock()
	defer c.connStateMu.Unlock()
	return c.connState
}

func (c *Controller) setConnStatus(s string) {
	c.connStateMu.Lock()
	c.connState = s
	c.connStateMu.Unlock()
}

// dialAndInit performs the full session lifecycle: TCP dial, hello,
// machine.load with the original preset + image, events.subscribe.
// Used by both startup (one-shot path) and heal (retry loop). Closes
// the partial client on any failure so callers don't leak goroutines.
func (c *Controller) dialAndInit() (*bridgeclient.Client, error) {
	nc, err := bridgeclient.Dial(c.addr)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	if _, e := nc.Hello("6502-control", "0.0.0"); e != nil {
		nc.Close()
		return nil, fmt.Errorf("hello: %s", e.Message)
	}
	if _, e := nc.MachineLoad(c.preset, c.image); e != nil {
		nc.Close()
		return nil, fmt.Errorf("machine.load: %s", e.Message)
	}
	if _, e := nc.Subscribe("clock.halt", "bp.hit", "state.snapshot", "tap.*"); e != nil {
		nc.Close()
		return nil, fmt.Errorf("events.subscribe: %s", e.Message)
	}
	return nc, nil
}

// healLoop watches the current client's Done channel and, on any
// disconnect, retries dialAndInit with exponential backoff until it
// succeeds (or the controller exits). On success, atomically swaps
// the new client in; consumeNotifications cycles to pick it up.
//
// Per-session loaders (cmd/6502-sim-serve) build a fresh Hub on each
// machine.load — so reconnect = clean machine. Breakpoints set
// before disconnect are lost; users re-set them after reconnect. The
// shared-TUI loader (cmd/6502-sim --serve) preserves CPU/RAM state
// across reconnects since the Hub outlives any single session.
func (c *Controller) healLoop() {
	const baseDelay = 500 * time.Millisecond
	const maxDelay = 15 * time.Second
	for {
		cli := c.client.Load()
		if cli == nil {
			time.Sleep(baseDelay)
			continue
		}
		<-cli.Done()

		c.healMu.Lock()
		c.setConnStatus("disconnected")
		c.addEvent("err", "bridge disconnected — attempting reconnect")

		delay := baseDelay
		for attempt := 1; ; attempt++ {
			c.setConnStatus("reconnecting")
			nc, err := c.dialAndInit()
			if err == nil {
				c.client.Store(nc)
				c.setConnStatus("connected")
				c.addEvent("info",
					fmt.Sprintf("reconnected to %s (attempt %d)", c.addr, attempt))
				// If the post-reconnect CPU state is available
				// already, paint the mirror so the user sees it
				// immediately rather than waiting for the next
				// periodic snapshot.
				if st, e := nc.CPUState(); e == nil {
					c.setState(st)
				}
				break
			}
			c.addEvent("err",
				fmt.Sprintf("reconnect attempt %d: %s — retry in %s",
					attempt, err.Error(), delay))
			time.Sleep(delay)
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
		c.healMu.Unlock()
	}
}

// reconnectNow forces an immediate reconnect attempt — used by the
// `reconnect` monitor command. Closes the current client (which
// triggers the heal loop's normal Done() detection + retry path),
// so it's safe to call repeatedly; the heal loop dedupes.
func (c *Controller) reconnectNow() {
	cli := c.client.Load()
	if cli == nil {
		return
	}
	cli.Close()
}

func (c *Controller) setState(st bridge.CPUState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = st
	c.stateSeen = true
	c.running = st.Running
}

func (c *Controller) snapshot() (bridge.CPUState, bool, bool, string, string, []eventLine) {
	c.mu.Lock()
	defer c.mu.Unlock()
	evs := make([]eventLine, len(c.events))
	copy(evs, c.events)
	return c.state, c.stateSeen, c.running, c.lastHalt, c.lastErr, evs
}

func (c *Controller) addEvent(kind, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, eventLine{t: time.Now(), kind: kind, text: text})
	if len(c.events) > c.eventsCap {
		c.events = c.events[len(c.events)-c.eventsCap:]
	}
}

// clearEvents empties the scrollback buffer. Used by View → Clear
// Monitor; doesn't reset the running CPU mirror, only the displayed
// event history.
func (c *Controller) clearEvents() {
	c.mu.Lock()
	c.events = c.events[:0]
	c.mu.Unlock()
	c.addEvent("system", "(scrollback cleared)")
}

// --- actions (bridge calls) ---

func (c *Controller) run() {
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

func (c *Controller) stop() {
	sr, e := c.client.Load().Stop()
	if e != nil {
		c.addEvent("err", "clock.stop: "+e.Message)
		return
	}
	c.setState(sr.State)
	c.addEvent("info", "stop ("+sr.Reason+")")
}

func (c *Controller) step() {
	sr, e := c.client.Load().Step(1)
	if e != nil {
		c.addEvent("err", "clock.step: "+e.Message)
		return
	}
	c.setState(sr.State)
	c.addEvent("info", fmt.Sprintf("step → PC=$%04X", sr.State.PC))
}

func (c *Controller) reset() {
	st, e := c.client.Load().Reset()
	if e != nil {
		c.addEvent("err", "machine.reset: "+e.Message)
		return
	}
	c.setState(st)
	// reset also halts the clock (see Hub.CmdReset); make that visible
	// in the same line as the new PC so the user doesn't wonder why
	// nothing's animating after the command.
	c.addEvent("info", fmt.Sprintf("reset → PC=$%04X  (CPU stopped)", st.PC))
}

func (c *Controller) bpAtPC() {
	c.mu.Lock()
	pc := c.state.PC
	c.mu.Unlock()
	res, e := c.client.Load().Call("bp.set", bridge.BPSetParams{Addr: pc})
	if e != nil {
		c.addEvent("err", "bp.set: "+e.Message)
		return
	}
	var r bridge.BPSetResult
	_ = jsonDecode(res, &r)
	c.addEvent("info", fmt.Sprintf("bp %s @ $%04X", r.ID, r.Addr))
}

// --- notification consumer ---

// consumeNotifications cycles per client lifetime: drains the current
// client's Notifications channel until it closes (connection died),
// then loops back and picks up whatever client the heal loop has
// swapped in. Without the outer for-loop this goroutine would exit
// on first disconnect, leaving the controller permanently deaf to
// state.snapshot / bp.hit / clock.halt even after reconnect.
func (c *Controller) consumeNotifications() {
	for {
		cli := c.client.Load()
		if cli == nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		for n := range cli.Notifications() {
			switch n.Method {
			case "state.snapshot":
				var p bridge.StateSnapshotPayload
				_ = jsonDecode(n.Params, &p)
				c.setState(p.State)
			case "bp.hit":
				var p bridge.BPHitPayload
				_ = jsonDecode(n.Params, &p)
				c.setState(p.State)
				c.addEvent("bp", fmt.Sprintf("bp.hit %s @ $%04X", p.ID, p.Addr))
			case "clock.halt":
				var p bridge.RunResult
				_ = jsonDecode(n.Params, &p)
				c.setState(p.State)
				c.mu.Lock()
				c.running = false
				c.lastHalt = p.Reason
				c.mu.Unlock()
				c.addEvent("halt", fmt.Sprintf("halt %s @ $%04X", p.Reason, p.Addr))
			case "tap.changed":
				var p bridge.TapChangedPayload
				_ = jsonDecode(n.Params, &p)
				c.addEvent("tap", fmt.Sprintf("%s = %d", p.Name, p.Value))
			}
		}
		// Channel closed → connection died. Loop will pick up the
		// new client when the heal loop stores it.
		time.Sleep(50 * time.Millisecond)
	}
}

// jsonDecode is a one-line wrapper so the handlers above stay terse.
// Errors are intentionally dropped — a malformed payload from the
// server would be a protocol bug, not a recoverable runtime error,
// and the controller is fine continuing with a zero-valued payload.
func jsonDecode(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}

// --- providers ---

type controlProvider struct{ c *Controller }

func (p *controlProvider) Draw(s tcell.Screen, inner foxpro.Rect, th foxpro.Theme, _ bool) {
	style := th.WindowBG
	dim := style.Dim(true)

	_, seen, running, lastHalt, lastErr, _ := p.c.snapshot()

	// Compose machine line — label and summary joined by an em-dash
	// when both are present, else whichever is set. drawInRect clips
	// at the right chrome so long combinations don't bleed.
	machine := p.c.machineLabel
	if p.c.buildSummary != "" {
		if machine != "" {
			machine += " — " + p.c.buildSummary
		} else {
			machine = p.c.buildSummary
		}
	}
	if machine == "" {
		machine = p.c.preset
	}

	// program line: "blinky.bin (234 b)" — or "(none)" when no image
	// was uploaded. Anchored to bytes from the local --program flag,
	// not the wire (server has no notion of filename).
	prog := "(none)"
	if p.c.programName != "" {
		prog = fmt.Sprintf("%s (%d b)", p.c.programName, p.c.programSize)
	}

	// Server line includes connection status — yellow when
	// reconnecting / disconnected so users can spot trouble at a
	// glance without reading the Monitor scrollback.
	connSt := p.c.connStatus()
	serverStyle := style
	switch connSt {
	case "reconnecting":
		serverStyle = style.Foreground(tcell.ColorYellow).Bold(true)
	case "disconnected", "failed":
		serverStyle = style.Foreground(tcell.ColorRed).Bold(true)
	}
	drawInRect(s, inner, 0, serverStyle,
		fmt.Sprintf("server : %s  [%s]", p.c.addr, connSt))
	drawInRect(s, inner, 1, style, fmt.Sprintf("machine: %s", machine))
	drawInRect(s, inner, 2, style, fmt.Sprintf("program: %s", prog))

	state := "idle"
	if running {
		state = "RUNNING"
	} else if !seen {
		state = "(no state yet)"
	}
	// While disconnected the running flag is stale; surface that
	// instead of pretending we know.
	if connSt != "connected" {
		state = "(" + connSt + ")"
	}
	drawInRect(s, inner, 3, style, fmt.Sprintf("clock  : %s", state))
	if lastHalt != "" {
		drawInRect(s, inner, 4, style, fmt.Sprintf("halt   : %s", lastHalt))
	}
	if lastErr != "" {
		drawInRect(s, inner, 4, style.Foreground(tcell.ColorRed), "ERR: "+lastErr)
	}

	drawInRect(s, inner, 5, dim, "─── hotkeys ───────────────────────")
	drawInRect(s, inner, 6, style, "  R  run        T  step")
	drawInRect(s, inner, 7, style, "  S  stop       B  bp @ PC")
	drawInRect(s, inner, 8, style, "  X  reset      Q  quit")
}

func (p *controlProvider) HandleKey(_ *tcell.EventKey) bool { return false }

type cpuProvider struct{ c *Controller }

func (p *cpuProvider) Draw(s tcell.Screen, inner foxpro.Rect, th foxpro.Theme, _ bool) {
	style := th.WindowBG
	dim := style.Dim(true)

	st, seen, _, _, _, _ := p.c.snapshot()

	if !seen {
		drawInRect(s, inner, 0, dim, "waiting for state.snapshot...")
		return
	}
	drawInRect(s, inner, 0, style,
		fmt.Sprintf("A=$%02X   X=$%02X   Y=$%02X", st.A, st.X, st.Y))
	drawInRect(s, inner, 1, style,
		fmt.Sprintf("SP=$%02X  P=$%02X", st.SP, st.P))
	drawInRect(s, inner, 2, style,
		fmt.Sprintf("PC=$%04X", st.PC))
	drawInRect(s, inner, 4, dim, "flags  N V - B D I Z C")
	drawInRect(s, inner, 5, style, "       "+flagsStr(st.P))
	drawInRect(s, inner, 7, dim, "cycles")
	drawInRect(s, inner, 8, style, fmt.Sprintf("       %d half", st.HalfCycles))
}

func (p *cpuProvider) HandleKey(_ *tcell.EventKey) bool { return false }

// monitorProvider is the REPL pane: a scrollback area on top and a
// single-line input strip on the bottom. Vertical scrolling lives on
// the WINDOW CHROME via the standard foxpro Scrollable contract —
// embedding foxpro.ScrollState tells the framework to draw a
// scrollbar on the right edge of the border, the same way every
// other window in cmd/6502-sim does it.
//
// Input handling is rolled inline (rather than using foxpro's
// InputProvider) so the input row can render in its own
// distinct-from-the-window style — white on blue — which makes the
// "where I type" line instantly findable.
//
// Keys:
//   ↑/↓        — command history navigation (bash-style)
//   PgUp/PgDn  — scrollback by half a page (drives ScrollState.Y)
//   ←/→        — cursor inside input
//   Home/End   — cursor extremes of input
//   Backspace/Delete — edit
//   Esc        — clear input
//   Enter      — submit → dispatch
//   <rune>     — insert at cursor
type monitorProvider struct {
	foxpro.ScrollState
	c *Controller

	// Input edit state (replaces foxpro.InputProvider).
	input   []rune
	cursor  int // index into `input`
	scrollX int // horizontal scroll within the visible input strip

	// History — appended on each submitted line; ↑/↓ walks it. When
	// the user enters history navigation we save whatever was in the
	// buffer into `pending` so ↓ past the newest restores it (bash).
	history []string
	histIdx int    // -1 = not navigating
	pending []rune // saved current input while in history nav

	// dispatch is invoked when the user submits a command line. It
	// runs in the foxpro goroutine, so it must not block on the wire
	// for long — but bridge calls are cheap (low-ms) so direct is OK
	// for now.
	dispatch func(line string)
}

func newMonitorProvider(c *Controller, dispatch func(line string)) *monitorProvider {
	return &monitorProvider{
		c:        c,
		histIdx:  -1,
		dispatch: dispatch,
	}
}

func (p *monitorProvider) Draw(s tcell.Screen, inner foxpro.Rect, th foxpro.Theme, focused bool) {
	if inner.H < 2 || inner.W < 4 {
		return
	}

	// Layout: top (H-1) rows = scrollback (Canvas-bound so the
	// chrome's auto-scrollbar reflects this content), bottom 1 row =
	// input strip. Full width either way — the scrollbar lives on
	// the chrome's right border, not inside the inner rect.
	sbRows := inner.H - 1
	sbInner := foxpro.Rect{X: inner.X, Y: inner.Y, W: inner.W, H: sbRows}
	inputInner := foxpro.Rect{X: inner.X, Y: inner.Y + sbRows, W: inner.W, H: 1}

	// "Follow latest" behaviour: if the user was at the bottom of
	// the scrollback before this frame, snap to the new bottom after
	// new events arrive. If they had scrolled up, leave their Y put
	// so reading older context isn't yanked from under them.
	_, prevNatH := p.ContentSize()
	_, prevView := p.LastViewport()
	prevMaxY := prevNatH - prevView
	if prevMaxY < 0 {
		prevMaxY = 0
	}
	_, curY := p.ScrollOffset()
	wasAtBottom := curY >= prevMaxY

	_, _, _, _, _, evs := p.c.snapshot()

	// Canvas-bound drawing — every Put records (x, y) extents into
	// ScrollState so the chrome scrollbar can size itself, and the
	// scroll offset is applied automatically when rendering.
	c := foxpro.NewCanvas(s, sbInner, &p.ScrollState)
	style := th.WindowBG
	for i, e := range evs {
		prefix, lineStyle := monitorPrefixStyle(e.kind, style)
		c.Put(0, i, prefix+e.text, lineStyle)
	}

	// After Canvas updated naturalH, snap to bottom if appropriate.
	_, natH := p.ContentSize()
	newMaxY := natH - sbRows
	if newMaxY < 0 {
		newMaxY = 0
	}
	if wasAtBottom && newMaxY != curY {
		p.SetScrollOffset(0, newMaxY)
	}

	// Input strip — white on blue, deliberately not theme.Input (gray)
	// so the line where typing happens stands out from the scrollback.
	p.drawInput(s, inputInner, th, focused)
}

// drawInput renders the bottom input row in its own distinct style
// (white-on-blue) plus a "> " prompt glyph, scrolling horizontally so
// the cursor stays inside the visible window.
func (p *monitorProvider) drawInput(s tcell.Screen, area foxpro.Rect, th foxpro.Theme, focused bool) {
	// Custom style: high-contrast prompt independent of theme.Input.
	bg := th.Palette.Blue
	fg := th.Palette.White
	promptStyle := tcell.StyleDefault.Background(bg).Foreground(th.Palette.Yellow)
	textStyle := tcell.StyleDefault.Background(bg).Foreground(fg)

	// 2-cell prompt: "> " in yellow on blue.
	if area.W < 3 {
		return
	}
	s.SetContent(area.X, area.Y, '>', nil, promptStyle)
	s.SetContent(area.X+1, area.Y, ' ', nil, promptStyle)

	bodyX := area.X + 2
	bodyW := area.W - 2

	// Horizontal-scroll the input so the cursor stays in view.
	if p.cursor < p.scrollX {
		p.scrollX = p.cursor
	} else if p.cursor >= p.scrollX+bodyW {
		p.scrollX = p.cursor - bodyW + 1
	}
	if p.scrollX < 0 {
		p.scrollX = 0
	}

	// Fill body with bg first, then overlay characters.
	for x := bodyX; x < bodyX+bodyW; x++ {
		s.SetContent(x, area.Y, ' ', nil, textStyle)
	}
	for i := 0; i < bodyW && p.scrollX+i < len(p.input); i++ {
		s.SetContent(bodyX+i, area.Y, p.input[p.scrollX+i], nil, textStyle)
	}

	if !focused {
		s.HideCursor()
		return
	}

	cx := bodyX + (p.cursor - p.scrollX)
	if cx < bodyX {
		cx = bodyX
	}
	if cx >= bodyX+bodyW {
		cx = bodyX + bodyW - 1
	}

	// Self-rendered blinking white cursor. The terminal's built-in
	// cursor varies wildly — color, shape, even whether SetCursorStyle
	// is respected at all (some terminals + tmux combos ignore it).
	// Painting our own white block guarantees the user always sees a
	// high-contrast caret on the blue input strip, and the blink
	// timing is independent of terminal config.
	//
	// 500 ms half-period at the foxpro tick rate (~33 ms) gives a
	// comfortable, attention-getting flash. The cell renders the
	// character under the cursor inverted (blue on white) so the
	// content under the caret stays legible while highlighted.
	blinkOn := time.Now().UnixMilli()/500%2 == 0
	if blinkOn {
		cursorStyle := tcell.StyleDefault.
			Background(th.Palette.White).
			Foreground(th.Palette.Blue)
		var ch rune = ' '
		if p.cursor < len(p.input) {
			ch = p.input[p.cursor]
		}
		s.SetContent(cx, area.Y, ch, nil, cursorStyle)
	}
	// Hide the terminal's built-in cursor — we draw our own so the
	// two don't fight visually.
	s.HideCursor()
}

// HandleKey routes editing + history + scrollback keys.
func (p *monitorProvider) HandleKey(ev *tcell.EventKey) bool {
	switch ev.Key() {
	case tcell.KeyUp:
		p.historyBack()
		return true
	case tcell.KeyDown:
		p.historyForward()
		return true
	case tcell.KeyPgUp:
		// Scroll UP visually = look at OLDER content = decrease Y
		// (Canvas y=0 is the top/oldest row in our event ordering).
		_, curY := p.ScrollOffset()
		p.SetScrollOffset(0, curY-p.halfPage())
		return true
	case tcell.KeyPgDn:
		_, curY := p.ScrollOffset()
		p.SetScrollOffset(0, curY+p.halfPage())
		return true
	case tcell.KeyEnter:
		p.submit()
		return true
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if p.cursor > 0 {
			p.input = append(p.input[:p.cursor-1], p.input[p.cursor:]...)
			p.cursor--
			p.resetHistoryNav()
		}
		return true
	case tcell.KeyDelete:
		if p.cursor < len(p.input) {
			p.input = append(p.input[:p.cursor], p.input[p.cursor+1:]...)
			p.resetHistoryNav()
		}
		return true
	case tcell.KeyLeft:
		if p.cursor > 0 {
			p.cursor--
		}
		return true
	case tcell.KeyRight:
		if p.cursor < len(p.input) {
			p.cursor++
		}
		return true
	case tcell.KeyHome:
		p.cursor = 0
		return true
	case tcell.KeyEnd:
		p.cursor = len(p.input)
		return true
	case tcell.KeyEscape:
		if len(p.input) > 0 {
			p.input = p.input[:0]
			p.cursor = 0
			p.scrollX = 0
			p.resetHistoryNav()
			return true
		}
		return false
	case tcell.KeyRune:
		p.input = append(p.input[:p.cursor], append([]rune{ev.Rune()}, p.input[p.cursor:]...)...)
		p.cursor++
		p.resetHistoryNav()
		return true
	}
	return false
}

func (p *monitorProvider) halfPage() int {
	_, h := p.LastViewport()
	if h > 2 {
		return h / 2
	}
	return 1
}

func (p *monitorProvider) submit() {
	// Trim trailing whitespace but preserve significant inner spaces
	// (the parser handles them).
	line := strings.TrimRight(string(p.input), " \t")
	if line == "" {
		return
	}
	p.history = append(p.history, line)
	p.histIdx = -1
	p.pending = nil
	// Snap to bottom on submit so the user always sees their command
	// echo + result land. SetScrollOffset clamps; max-int parks at
	// the very bottom regardless of natural-height.
	p.SetScrollOffset(0, 1<<30)
	p.input = p.input[:0]
	p.cursor = 0
	p.scrollX = 0
	p.c.addEvent("input", line)
	if p.dispatch != nil {
		p.dispatch(line)
	}
}

func (p *monitorProvider) resetHistoryNav() {
	if p.histIdx >= 0 {
		p.histIdx = -1
		p.pending = nil
	}
}

func (p *monitorProvider) historyBack() {
	if len(p.history) == 0 {
		return
	}
	if p.histIdx < 0 {
		// First ↑ — save what they were typing.
		p.pending = append(p.pending[:0], p.input...)
		p.histIdx = len(p.history) - 1
	} else if p.histIdx > 0 {
		p.histIdx--
	}
	p.setInput(p.history[p.histIdx])
}

func (p *monitorProvider) historyForward() {
	if p.histIdx < 0 {
		return
	}
	if p.histIdx < len(p.history)-1 {
		p.histIdx++
		p.setInput(p.history[p.histIdx])
		return
	}
	// Stepped past newest — restore the saved pending line and exit
	// history mode.
	p.histIdx = -1
	p.input = append(p.input[:0], p.pending...)
	p.cursor = len(p.input)
	p.pending = nil
}

func (p *monitorProvider) setInput(s string) {
	p.input = p.input[:0]
	for _, r := range s {
		p.input = append(p.input, r)
	}
	p.cursor = len(p.input)
}

// monitorPrefixStyle picks the leading prefix + tcell style for each
// event kind. Errors render INVERTED (white text on red bg) so they
// pop against the cyan WindowBG — red FG alone on cyan had poor
// contrast and was hard to read at a glance.
func monitorPrefixStyle(kind string, base tcell.Style) (string, tcell.Style) {
	switch kind {
	case "input":
		return "> ", base.Bold(true)
	case "result":
		return "  ", base
	case "system":
		return "i ", base.Dim(true)
	case "info":
		return "  ", base.Dim(true)
	case "halt":
		return "* ", base.Foreground(tcell.ColorYellow).Bold(true)
	case "bp":
		return "* ", base.Foreground(tcell.ColorYellow).Bold(true)
	case "tap":
		return "* ", base.Foreground(tcell.ColorAqua)
	case "err":
		// Inverted: white on red. Most readable error color on the
		// cyan window background.
		return "! ", tcell.StyleDefault.Background(tcell.ColorMaroon).Foreground(tcell.ColorWhite).Bold(true)
	}
	return "  ", base
}

// --- tcell helpers ---

// drawInRect writes `str` at row `row` (zero-based, relative to
// inner.Y) of the given window-inner rect, clipping both vertically
// (row out of [0, inner.H)) and horizontally (chars past inner.W) so
// content never spills past the window chrome. The bleed-fix lives
// here: previously a naïve drawString wrote at x+i unconditionally,
// leaving stale teal cells in the screen background when a string was
// longer than the window or the row was out of range.
func drawInRect(s tcell.Screen, inner foxpro.Rect, row int, style tcell.Style, str string) {
	if row < 0 || row >= inner.H {
		return
	}
	y := inner.Y + row
	maxX := inner.X + inner.W
	x := inner.X
	for _, r := range str {
		if x >= maxX {
			return
		}
		s.SetContent(x, y, r, nil, style)
		x++
	}
}

// openHelpWindow pops a persistent draggable text window containing
// the full grouped help table. Lives separately from the inline
// `help` scrollback dump so the user can leave it open for reference
// while typing other commands. Same content as helpTable so changes
// only have to be made in one place.
func openHelpWindow(a *foxpro.App) {
	w := foxpro.NewWindow("Help — Monitor",
		foxpro.Rect{X: 6, Y: 2, W: 70, H: 32},
		foxpro.NewTextProvider(helpTable))
	a.Manager.Add(w)
}

// openAbout pops a draggable text window summarizing the controller.
// Mirrors cmd/6502-sim's About dialog so users see a consistent look
// across the two TUIs.
func openAbout(a *foxpro.App) {
	body := foxpro.NewTextProvider([]string{
		"6502 Controller",
		"",
		"A foxpro-go TUI that drives a remote go6sim bridge",
		"server. Same protocol as VS Code / MCP clients —",
		"this is a hand-driven peer.",
		"",
		"Three panes:",
		"  Control   connection + clock state + hotkey legend",
		"  CPU       live registers mirrored from state.snapshot",
		"  Monitor   REPL with command history + sim event log",
		"",
		"Hotkeys (outside Monitor):",
		"  R run    T step    S stop    B bp@PC    X reset    Q quit",
		"",
		"Source: github.com/carledwards/go6sim",
	})
	w := foxpro.NewWindow("About", foxpro.Rect{X: 20, Y: 4, W: 56, H: 19}, body)
	a.Manager.Add(w)
}

// showHotkeys dumps the hotkey legend into Monitor scrollback so the
// user can read it inline while typing. Doesn't open a window — the
// scrollback is the natural place when context-help is short.
func showHotkeys(c *Controller) {
	lines := []string{
		"hotkeys (active when Monitor is NOT focused):",
		"  R  run         T  step",
		"  S  stop        B  bp @ PC",
		"  X  reset       Q  quit",
		"  F6  cycle window",
		"in Monitor:  ↑/↓ history  ·  PgUp/PgDn scrollback  ·  Esc clear input",
	}
	for _, l := range lines {
		c.addEvent("result", l)
	}
}

func flagsStr(p uint8) string {
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
