// Command 6502-control is a foxpro-go TUI "control console" for a
// remote go6sim bridge server. Pure client — every action goes over
// the wire (cmd/6502-sim-serve, or cmd/6502-sim --serve).
//
// Three windows:
//   ┌─ Control ─┐    connection + machine + clock + hotkey legend
//   ┌─ CPU ─────┐    live registers mirrored from state.snapshot
//   ┌─ Monitor ─┐    REPL — see internal/monitor for the command set
//
// Hotkeys (active when Monitor is NOT focused):
//   R run    T step    S stop    B bp@PC    X reset    Q quit
//
// Defaults: --connect 127.0.0.1:6502, --preset teach-min,
//           --program <file> (optional raw bytes for ROM at $E000).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	foxpro "github.com/carledwards/foxpro-go"
	"github.com/gdamore/tcell/v2"

	"github.com/carledwards/go6sim/bridge"
	"github.com/carledwards/go6sim/internal/bridgeclient"
	"github.com/carledwards/go6sim/internal/monitor"
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

	machineLabel := mlr.Label
	if machineLabel == "" {
		machineLabel = mlr.Preset
	}
	ctrl := newController(client, *connect, mlr.Preset, machineLabel, mlr.Summary, programName, image, mlr.Regions)
	if st, e := client.CPUState(); e == nil {
		ctrl.setState(st)
	}

	// 2. foxpro app + windows.
	app, err := foxpro.NewApp()
	if err != nil {
		fmt.Fprintln(os.Stderr, "6502-control: foxpro init:", err)
		os.Exit(1)
	}
	defer app.Close()
	foxpro.RegisterBuiltinCommands(app)

	// Quit signal + help window opener feed the Host interface. Wire
	// them before constructing the Monitor so Host methods that
	// reference them have valid targets.
	quitCh := make(chan struct{}, 1)
	ctrl.quitSignal = quitCh
	ctrl.helpWindowFn = func() { openHelpWindow(app) }

	// Monitor — shared REPL. Bound to the Target (the wire client) +
	// Host (this Controller). The Controller forwards machine
	// metadata + lifecycle hooks; the Monitor owns its scrollback.
	mon := monitor.New(ctrl, ctrl.currentClient())
	ctrl.mon = mon

	controlWin := foxpro.NewWindow("Control",
		foxpro.Rect{X: 2, Y: 1, W: 38, H: 11},
		&controlProvider{c: ctrl})
	cpuWin := foxpro.NewWindow("CPU",
		foxpro.Rect{X: 42, Y: 1, W: 38, H: 11},
		&cpuProvider{c: ctrl})
	monitorWin := foxpro.NewWindow("Monitor",
		foxpro.Rect{X: 2, Y: 13, W: 78, H: 22},
		mon)
	app.Manager.Add(controlWin)
	app.Manager.Add(cpuWin)
	app.Manager.Add(monitorWin)

	// Menu bar — System / Control / Window / Help. Control items
	// mirror hotkeys for mouse users; the Window menu handles focus
	// + Clear Monitor; Help opens the persistent reference window.
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
				{Label: "C&lear Monitor", OnSelect: func() { mon.Clear() }},
			},
		},
		{
			Label: "&Help",
			Items: []foxpro.MenuItem{
				{Label: "&Commands (window)", OnSelect: func() { openHelpWindow(app) }},
				{Label: "&Hotkeys", OnSelect: func() { showHotkeys(mon) }},
				{Separator: true},
				{Label: "&About", OnSelect: func() { openAbout(app) }},
			},
		},
	})

	// Live tray — running/stopped reflects the mirror.
	app.MenuBar.Tray = []foxpro.TrayItem{
		{
			Compute: func() string {
				if ctrl.isRunning() {
					return "running"
				}
				return "stopped"
			},
		},
	}

	// Startup banner — context on first paint.
	mon.AddEvent("system", fmt.Sprintf("connected to %s", *connect))
	mon.AddEvent("system", fmt.Sprintf("machine : %s", machineLabel))
	if programName != "" {
		mon.AddEvent("system", fmt.Sprintf("program : %s (%d bytes)", programName, len(image)))
	}
	mon.AddEvent("system", "type 'help' or '?'")

	// 3. Global hotkeys — fire only when Monitor is NOT focused. With
	//    focus on Monitor, runes flow to the input editor.
	app.OnKey = func(ev *tcell.EventKey) bool {
		if ev.Key() != tcell.KeyRune {
			return false
		}
		if app.Manager.Active() == monitorWin {
			return false
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

	// 4. Notification consumer + heal loop.
	go ctrl.consumeNotifications()
	go ctrl.healLoop()

	// 5. Tick → repaint nudge so Monitor's cursor blink stays smooth.
	app.Tick(33*time.Millisecond, func() {})

	// Quit watcher — `q` / `quit` triggers app shutdown without the
	// dispatcher (foxpro loop) calling app.Quit on itself.
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

// --- Controller ---

// Controller owns the wire client (with atomic-swap for heal-loop
// reconnects), the CPU/connection mirror state read by the Control
// + CPU windows, and a back-reference to the Monitor for emitting
// events (notifications, heal-loop messages, hotkey echoes).
//
// Implements monitor.Host so the shared Monitor can call back for
// machine metadata + quit + open-help.
type Controller struct {
	client atomic.Pointer[bridgeclient.Client]
	addr   string

	// Machine metadata — set at construction, immutable.
	preset       string
	machineLabel string
	buildSummary string
	programName  string
	programSize  int
	image        []byte
	regions      []bridge.Region

	// Mirror state read by controlProvider / cpuProvider.
	mu        sync.Mutex
	state     bridge.CPUState
	stateSeen bool
	running   bool
	lastHalt  string
	lastErr   string

	// Latching stack-depth tracker. Fires one warn event per
	// threshold crossing (80/90/100%); resets below 75% (hysteresis).
	stackWarn int

	// Connection status drives the Control pane's server-line colour.
	connStateMu sync.Mutex
	connState   string
	healMu      sync.Mutex

	// Wired by main once foxpro is up.
	mon          *monitor.Monitor // back-ref for AddEvent
	quitSignal   chan<- struct{}
	helpWindowFn func()
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
		connState:    "connected",
	}
	ctrl.client.Store(c)
	return ctrl
}

func (c *Controller) currentClient() *bridgeclient.Client { return c.client.Load() }

// --- monitor.Host implementation ---

func (c *Controller) MachineLabel() string     { return c.machineLabel }
func (c *Controller) BuildSummary() string     { return c.buildSummary }
func (c *Controller) ProgramName() string      { return c.programName }
func (c *Controller) ProgramSize() int         { return c.programSize }
func (c *Controller) Regions() []bridge.Region { return c.regions }

func (c *Controller) OpenHelpWindow() {
	if c.helpWindowFn != nil {
		c.helpWindowFn()
	}
}

func (c *Controller) Quit() {
	if c.quitSignal == nil {
		return
	}
	select {
	case c.quitSignal <- struct{}{}:
	default:
	}
}

func (c *Controller) Reconnect() {
	cli := c.client.Load()
	if cli == nil {
		return
	}
	cli.Close() // heal loop detects via Done() and re-dials
}

// --- connection state ---

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
// Used at startup and by the heal loop. Closes the partial client on
// any failure so we don't leak goroutines.
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

// healLoop watches the active client's Done channel and reconnects
// with backoff. Atomically swaps the new client in;
// consumeNotifications cycles to pick it up.
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
		c.mon.AddEvent("err", "bridge disconnected — attempting reconnect")

		delay := baseDelay
		for attempt := 1; ; attempt++ {
			c.setConnStatus("reconnecting")
			nc, err := c.dialAndInit()
			if err == nil {
				c.client.Store(nc)
				c.setConnStatus("connected")
				c.mon.AddEvent("info",
					fmt.Sprintf("reconnected to %s (attempt %d)", c.addr, attempt))
				if st, e := nc.CPUState(); e == nil {
					c.setState(st)
				}
				break
			}
			c.mon.AddEvent("err",
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

// --- mirror state ---

func (c *Controller) setState(st bridge.CPUState) {
	c.mu.Lock()
	c.state = st
	c.stateSeen = true
	c.running = st.Running

	used := int(0xFF - st.SP)
	var newWarn int
	switch {
	case used >= 0xFF:
		newWarn = 3
	case used >= 230:
		newWarn = 2
	case used >= 204:
		newWarn = 1
	}
	var crossed int
	switch {
	case newWarn > c.stackWarn:
		crossed = newWarn
		c.stackWarn = newWarn
	case used < 191 && c.stackWarn != 0:
		c.stackWarn = 0
	}
	sp := st.SP
	c.mu.Unlock()

	if crossed == 0 || c.mon == nil {
		return
	}
	switch crossed {
	case 1:
		c.mon.AddEvent("warn", fmt.Sprintf("stack at 80%% (SP=$%02X, %d bytes used)", sp, used))
	case 2:
		c.mon.AddEvent("warn", fmt.Sprintf("stack at 90%% (SP=$%02X, %d bytes used)", sp, used))
	case 3:
		c.mon.AddEvent("err", fmt.Sprintf("STACK OVERFLOW IMMINENT — SP=$%02X, next push wraps to $01FF", sp))
	}
}

func (c *Controller) cpuSnapshot() (st bridge.CPUState, seen, running bool, lastHalt, lastErr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.stateSeen, c.running, c.lastHalt, c.lastErr
}

func (c *Controller) isRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// --- hotkey-bound actions (run/stop/step/reset/bp@PC) ---
// These mirror what the Monitor's commands do but emit echoes via
// mon.AddEvent so the user sees the same scrollback whether they
// hit R or typed `g`.

func (c *Controller) run() {
	if e := c.client.Load().Run(0); e != nil {
		c.mon.AddEvent("err", "clock.run: "+e.Message)
		return
	}
	c.mu.Lock()
	c.running = true
	c.lastHalt = ""
	c.mu.Unlock()
	c.mon.AddEvent("info", "run")
}

func (c *Controller) stop() {
	sr, e := c.client.Load().Stop()
	if e != nil {
		c.mon.AddEvent("err", "clock.stop: "+e.Message)
		return
	}
	c.setState(sr.State)
	c.mon.AddEvent("info", "stop ("+sr.Reason+")")
}

func (c *Controller) step() {
	sr, e := c.client.Load().Step(1)
	if e != nil {
		c.mon.AddEvent("err", "clock.step: "+e.Message)
		return
	}
	c.setState(sr.State)
	c.mon.AddEvent("info", fmt.Sprintf("step → PC=$%04X", sr.State.PC))
}

func (c *Controller) reset() {
	st, e := c.client.Load().Reset()
	if e != nil {
		c.mon.AddEvent("err", "machine.reset: "+e.Message)
		return
	}
	c.setState(st)
	c.mon.AddEvent("info", fmt.Sprintf("reset → PC=$%04X  (CPU stopped)", st.PC))
}

func (c *Controller) bpAtPC() {
	c.mu.Lock()
	pc := c.state.PC
	c.mu.Unlock()
	r, e := c.client.Load().BPSet(pc)
	if e != nil {
		c.mon.AddEvent("err", "bp.set: "+e.Message)
		return
	}
	c.mon.AddEvent("info", fmt.Sprintf("bp %s @ $%04X", r.ID, r.Addr))
}

// --- notification consumer ---

// consumeNotifications cycles per client lifetime: drains the current
// client's Notifications channel until it closes (connection died),
// then loops back and picks up whatever client the heal loop has
// swapped in.
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
				_ = jsonUnmarshal(n.Params, &p)
				c.setState(p.State)
			case "bp.hit":
				var p bridge.BPHitPayload
				_ = jsonUnmarshal(n.Params, &p)
				c.setState(p.State)
				c.mon.AddEvent("bp", fmt.Sprintf("bp.hit %s @ $%04X", p.ID, p.Addr))
			case "clock.halt":
				var p bridge.RunResult
				_ = jsonUnmarshal(n.Params, &p)
				c.setState(p.State)
				c.mu.Lock()
				c.running = false
				c.lastHalt = p.Reason
				c.mu.Unlock()
				c.mon.AddEvent("halt", fmt.Sprintf("halt %s @ $%04X", p.Reason, p.Addr))
			case "tap.changed":
				var p bridge.TapChangedPayload
				_ = jsonUnmarshal(n.Params, &p)
				c.mon.AddEvent("tap", fmt.Sprintf("%s = %d", p.Name, p.Value))
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// --- providers ---

type controlProvider struct{ c *Controller }

func (p *controlProvider) Draw(s tcell.Screen, inner foxpro.Rect, th foxpro.Theme, _ bool) {
	style := th.WindowBG
	dim := style.Dim(true)

	_, seen, running, lastHalt, lastErr := p.c.cpuSnapshot()

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
	prog := "(none)"
	if p.c.programName != "" {
		prog = fmt.Sprintf("%s (%d b)", p.c.programName, p.c.programSize)
	}

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

	st, seen, _, _, _ := p.c.cpuSnapshot()

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
	drawInRect(s, inner, 5, style, "       "+monitor.FlagsStr(st.P))
	drawInRect(s, inner, 7, dim, "cycles")
	drawInRect(s, inner, 8, style, fmt.Sprintf("       %d half", st.HalfCycles))
}

func (p *cpuProvider) HandleKey(_ *tcell.EventKey) bool { return false }

// --- dialogs + windows ---

func openHelpWindow(a *foxpro.App) {
	w := foxpro.NewWindow("Help — Monitor",
		foxpro.Rect{X: 6, Y: 2, W: 70, H: 32},
		foxpro.NewTextProvider(monitor.HelpTable))
	a.Manager.Add(w)
}

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
// user can read it inline while typing.
func showHotkeys(mon *monitor.Monitor) {
	lines := []string{
		"hotkeys (active when Monitor is NOT focused):",
		"  R  run         T  step",
		"  S  stop        B  bp @ PC",
		"  X  reset       Q  quit",
		"  F6  cycle window",
		"in Monitor:  ↑/↓ history  ·  PgUp/PgDn scrollback  ·  Esc clear input",
	}
	for _, l := range lines {
		mon.AddEvent("result", l)
	}
}

// --- helpers ---

// drawInRect writes `str` at row `row` (zero-based, relative to
// inner.Y) of the given window-inner rect, clipping vertical out-of-
// range AND horizontal overflow so content can't spill past chrome.
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

// jsonUnmarshal stays a one-liner so the notification handler isn't
// littered with error vars; malformed payloads from the server are
// protocol bugs, not recoverable runtime errors.
func jsonUnmarshal(raw []byte, v any) error {
	return json.Unmarshal(raw, v)
}
