//go:build js && wasm

// Command cpu-host-wasm is the browser-side Remote-CPU host. It boots
// a foxpro-go shell with two windows — a Connection panel showing pair
// status and a Visual 6502 panel that will (in a later step) show the
// transistor-level chip view — and dials the TUI's /cpu WebSocket
// endpoint to serve as the active CPU.
//
// CPU engine is netsim (the 6502-netsim-go transistor port). Each
// half-step the netsim core requests a bus access, the shared
// remote.ClientBus forwards it to the TUI, and the response comes
// back over the same WebSocket — Go goroutines can block on the
// wire without freezing the JS host page, so the request/response
// pattern works cleanly. The Visual 6502 window renders the
// transistor-level die from netsim's per-node state vector using
// the same visualcpuwin code path the TUI uses for its local view.
package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"syscall/js"
	"time"

	"github.com/coder/websocket"
	"github.com/gdamore/tcell/v2"

	foxpro "github.com/carledwards/foxpro-go"
	"github.com/carledwards/foxpro-go/wasm"

	"github.com/carledwards/go6sim/cpu/netsim"
	"github.com/carledwards/go6sim/cpu/remote"
	"github.com/carledwards/go6sim/ui/visualcpuwin"
)

func main() {
	// SimulationScreen → JS canvas via foxpro-go's wasm bridge.
	// 105×90 cells keeps the host page compact while giving the
	// Visual 6502 window enough room for a legible chip render.
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		panic(err)
	}
	s.SetSize(106, 59)
	s.EnableMouse()

	app := foxpro.NewAppWithScreen(s)
	app.Settings.QuitKeys = nil
	app.Settings.BackgroundDragChords = []foxpro.BackgroundDragChord{
		{Button: tcell.Button1, Mods: tcell.ModShift},
	}
	app.Settings.StatusBarLeft = " CPU Host "

	// Shared connection state — updated by the service goroutine,
	// read by the Connection panel's Draw.
	state := &hostState{
		v: stateView{url: detectWSURL(), status: "starting"},
	}

	// Persistent ClientBus + netsim Adapter. They outlive any single
	// connection so the visualcpuwin Provider has a stable Backend
	// pointer; the bus just swaps its conn pointer on reconnect.
	// NodeStates is read from this Adapter on every frame to
	// colour the die view.
	wb := remote.NewClientBus(state.noteBus)
	adapter, err := netsim.New(wb)
	if err != nil {
		panic(err)
	}

	connProv := &connectionPanel{state: state}
	connWin := foxpro.NewWindow("Connection", foxpro.Rect{X: 2, Y: 1, W: 101, H: 11}, connProv)
	connWin.Tag = "connection"
	app.Manager.Add(connWin)

	visualProv := visualcpuwin.New()
	visualProv.Backend = adapter
	visualProv.IsTransistorBackend = func() bool { return true }
	visualWin := foxpro.NewWindow("Visual 6502 — click to toggle overlay", foxpro.Rect{X: 2, Y: 13, W: 101, H: 43}, visualProv)
	visualWin.Tag = "visual"
	app.Manager.Add(visualWin)

	// Start the WS service in a goroutine — it keeps reconnecting
	// until the page is closed. Hands the persistent Adapter +
	// wireBus to each session so its conn pointer swaps in place.
	go runService(state.v.url, state, wb, adapter)

	// Periodic redraw nudge — the Connection panel changes outside
	// the input loop (network events), so without this the panel
	// stays stale until the user moves the mouse. 100 ms keeps
	// status text crisp without being wasteful.
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			app.Post(func() {}) // empty post triggers a frame
		}
	}()

	wasm.Run(app, s)
}

// detectWSURL builds ws://<page-host>/cpu from window.location, so the
// browser always connects to the same server that served the page.
func detectWSURL() string {
	loc := js.Global().Get("location")
	host := loc.Get("host").String()
	proto := "ws:"
	if loc.Get("protocol").String() == "https:" {
		proto = "wss:"
	}
	return fmt.Sprintf("%s//%s/cpu", proto, host)
}

// ---------------- panels ----------------

// hostState carries the live state the Connection panel reads each
// frame. fields embedded in stateView are the snapshot returned to
// the Draw goroutine — separate type so the mutex never crosses
// goroutines.
type hostState struct {
	mu sync.Mutex
	v  stateView
}

type stateView struct {
	url         string
	status      string // "starting", "dialing", "paired", "disconnected: <why>"
	halfCycles  uint64
	lastTUIMsg  string
	lastBusOp   string
	lastBusAddr uint16
	lastBusData uint8
}

func (s *hostState) snapshot() stateView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v
}

func (s *hostState) setStatus(v string) { s.mu.Lock(); s.v.status = v; s.mu.Unlock() }
func (s *hostState) bumpHalf() {
	s.mu.Lock()
	s.v.halfCycles++
	s.mu.Unlock()
}
func (s *hostState) noteBus(op string, addr uint16, data uint8) {
	s.mu.Lock()
	s.v.lastBusOp, s.v.lastBusAddr, s.v.lastBusData = op, addr, data
	s.mu.Unlock()
}
func (s *hostState) noteMsg(t string) { s.mu.Lock(); s.v.lastTUIMsg = t; s.mu.Unlock() }

type connectionPanel struct {
	state *hostState
}

func (p *connectionPanel) Draw(screen tcell.Screen, inner foxpro.Rect, theme foxpro.Theme, focused bool) {
	st := p.state.snapshot()
	style := theme.WindowBG
	c := foxpro.NewCanvas(screen, inner, nil)
	c.Put(0, 0, fmt.Sprintf("Endpoint:   %s", st.url), style)
	c.Put(0, 2, fmt.Sprintf("Status:     %s", st.status), style)
	c.Put(0, 4, fmt.Sprintf("HalfCycles: %d", st.halfCycles), style)
	if st.lastTUIMsg != "" {
		c.Put(0, 6, fmt.Sprintf("Last cmd:   %s", st.lastTUIMsg), style)
	}
	if st.lastBusOp != "" {
		c.Put(0, 7, fmt.Sprintf("Last bus:   %-5s $%04X = $%02X", st.lastBusOp, st.lastBusAddr, st.lastBusData), style)
	}
}

func (p *connectionPanel) HandleKey(ev *tcell.EventKey) bool { return false }

// ---------------- WS service loop ----------------

func runService(url string, state *hostState, wb *remote.ClientBus, a *netsim.Adapter) {
	ctx := context.Background()
	for {
		state.setStatus("dialing " + url)
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		c, _, err := websocket.Dial(dialCtx, url, nil)
		cancel()
		if err != nil {
			state.setStatus(fmt.Sprintf("dial failed: %v — retrying in 1s", err))
			time.Sleep(time.Second)
			continue
		}
		c.SetReadLimit(1 << 20)
		wb.SetConn(c, ctx)
		state.setStatus("paired (kind=netsim)")
		if err := serve(ctx, c, state, a); err != nil {
			state.setStatus(fmt.Sprintf("disconnected: %v — reconnecting in 1s", err))
		} else {
			state.setStatus("disconnected — reconnecting in 1s")
		}
		wb.SetConn(nil, ctx)
		_ = c.Close(websocket.StatusNormalClosure, "session end")
		time.Sleep(time.Second)
	}
}

func serve(ctx context.Context, c *websocket.Conn, state *hostState, a *netsim.Adapter) error {
	if err := remote.WriteMsg(ctx, c, remote.Msg{Type: remote.MsgHello, Role: "cpu", Kind: "netsim"}); err != nil {
		return err
	}

	for {
		m, err := remote.ReadMsg(ctx, c)
		if err != nil {
			return err
		}
		state.noteMsg(m.Type)
		switch m.Type {
		case remote.MsgHalfStep:
			a.HalfStep()
			state.bumpHalf()
			if err := remote.WriteMsg(ctx, c, remote.Msg{
				Type:    remote.MsgHalfStepDone,
				BusAddr: a.AddressBus(),
				BusData: a.DataBus(),
				RW:      a.ReadCycle(),
				SYNC:    a.SYNC(),
				IRQ:     a.IRQ(),
				NMI:     a.NMI(),
			}); err != nil {
				return err
			}
		case remote.MsgReset:
			a.Reset()
			if err := remote.WriteMsg(ctx, c, remote.Msg{Type: remote.MsgResetDone}); err != nil {
				return err
			}
		case remote.MsgRegisters:
			r := a.Registers()
			if err := remote.WriteMsg(ctx, c, remote.Msg{
				Type: remote.MsgRegsResp,
				A:    r.A, X: r.X, Y: r.Y, S: r.S, P: r.P, PC: r.PC,
			}); err != nil {
				return err
			}
		case remote.MsgBye:
			return nil
		default:
			log.Printf("cpu-host: unexpected msg %q", m.Type)
		}
	}
}
