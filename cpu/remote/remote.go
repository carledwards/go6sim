package remote

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/carledwards/go6sim/bus"
	"github.com/carledwards/go6sim/cpu"
)

// Adapter is the TUI-side cpu.Backend for a remote CPU. It binds an
// HTTP+WebSocket listener; the first CPU client to dial `/cpu` becomes
// the paired CPU until disconnect.
//
// Threading model: WS reads/writes happen inline on the goroutine
// calling the cpu.Backend methods (Pump goroutine for HalfStep,
// arbitrary readers for Registers / Bus accessors served from cache).
// All cached pin state lives behind a single mutex; WS conn pointer
// is read under that same mutex, then released so the IO doesn't
// hold the lock during the network roundtrip.
type Adapter struct {
	bus bus.Bus

	mu         sync.Mutex
	conn       *websocket.Conn
	cancelConn context.CancelFunc // releases HandleWS when we drop server-side
	onConnect  func()
	onDisconn  func()
	kindCached string // remote-side CPU kind reported in hello

	// rpcMu serializes every wire IO sequence (HalfStep, Reset,
	// Registers). The Backend interface is sync but its methods are
	// called from multiple goroutines (Pump for HalfStep, UI for
	// Registers, etc.); without this lock the request/response
	// frames interleave on the conn and the peer sees corrupted
	// WebSocket frames ("unexpected rsv bits set").
	rpcMu sync.Mutex

	// Cached state — last halfStep/regs response. Accessors served
	// from here so the Pump's accessor calls don't roundtrip.
	stateMu    sync.RWMutex
	addrBus    uint16
	dataBus    uint8
	rw         bool
	syncPin    bool
	irq        bool
	nmi        bool
	regs       cpu.Registers
	halfCycles uint64
}

// New returns an Adapter ready to be paired with a CPU client. The
// caller is responsible for binding an HTTP server and registering
// Adapter.HandleWS at whatever path it likes (conventionally /cpu).
// Adapter is usable immediately — its Backend methods noop or return
// cached zero state until a CPU dials in.
func New(b bus.Bus) *Adapter {
	return &Adapter{
		bus: b,
		irq: true, // 6502 IRQ is active-low, default released
		nmi: true,
	}
}

// Connected reports whether a CPU is currently paired.
func (a *Adapter) Connected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conn != nil
}

// SetCallbacks installs the connect/disconnect hooks after
// construction (main typically can't pass them in at New time because
// the Hub doesn't exist yet). Safe to call before or after a CPU
// has paired; the next state transition triggers the callbacks.
func (a *Adapter) SetCallbacks(onConnect, onDisconnect func()) {
	a.mu.Lock()
	a.onConnect = onConnect
	a.onDisconn = onDisconnect
	a.mu.Unlock()
}

// Kind returns the remote-side CPU kind reported in the hello
// handshake ("interp", "visual6502", ...). Empty when not connected.
func (a *Adapter) Kind() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.kindCached
}

// HandleWS is the HTTP handler for the CPU client's WebSocket upgrade.
// Accepts the upgrade, reads the hello, holds the connection as the
// paired CPU until the socket closes. Rejects concurrent connections
// with 409 so the second client knows why it bounced.
func (a *Adapter) HandleWS(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	if a.conn != nil {
		a.mu.Unlock()
		http.Error(w, "cpu slot already taken", http.StatusConflict)
		return
	}
	a.mu.Unlock()

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // localhost dev tool — no origin policy
	})
	if err != nil {
		log.Printf("remote-cpu accept: %v", err)
		return
	}
	// Disable read limit — bus payloads are tiny but visual6502's
	// hello could grow if we add capabilities; future-proof rather
	// than chase a mystery hang.
	c.SetReadLimit(1 << 20)

	// Read the hello before declaring ourselves paired. If the client
	// doesn't speak the protocol we close with a useful status.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	helloMsg, err := ReadMsg(ctx, c)
	cancel()
	if err != nil || helloMsg.Type != MsgHello || helloMsg.Role != "cpu" {
		_ = c.Close(websocket.StatusPolicyViolation, "expected hello{role:cpu}")
		return
	}

	// connCtx is the slot's lifetime. Cancelled either by the client
	// (r.Context().Done) OR by us when an Adapter RPC hit an IO
	// error and called dropConn — without that side channel, server-
	// side drops would leave the HTTP handler blocked, the conn slot
	// occupied, and the next reconnect would get 409 Conflict.
	connCtx, connCancel := context.WithCancel(r.Context())
	a.mu.Lock()
	a.conn = c
	a.cancelConn = connCancel
	a.kindCached = helloMsg.Kind
	a.mu.Unlock()
	log.Printf("remote-cpu paired: kind=%s", helloMsg.Kind)
	if a.onConnect != nil {
		a.onConnect()
	}

	<-connCtx.Done()

	a.mu.Lock()
	if a.conn == c {
		a.conn = nil
		a.cancelConn = nil
		a.kindCached = ""
	}
	a.mu.Unlock()
	_ = c.Close(websocket.StatusNormalClosure, "session end")
	log.Printf("remote-cpu unpaired")
	if a.onDisconn != nil {
		a.onDisconn()
	}
}

// ---------------- cpu.Backend ----------------

// HalfStep sends a halfStep request, services any bus reads/writes
// the CPU emits mid-step, and caches the pin snapshot in the final
// halfStepDone reply. Noop if no CPU is paired — the Pump keeps
// running but no virtual time elapses.
func (a *Adapter) HalfStep() {
	conn := a.currentConn()
	if conn == nil {
		return
	}
	a.rpcMu.Lock()
	defer a.rpcMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := WriteMsg(ctx, conn, Msg{Type: MsgHalfStep}); err != nil {
		a.dropConn(conn, err)
		return
	}
	for {
		m, err := ReadMsg(ctx, conn)
		if err != nil {
			a.dropConn(conn, err)
			return
		}
		switch m.Type {
		case MsgBusRead:
			v := a.bus.Read(m.Addr)
			if err := WriteMsg(ctx, conn, Msg{Type: MsgBusData, Data: v}); err != nil {
				a.dropConn(conn, err)
				return
			}
		case MsgBusWrite:
			a.bus.Write(m.Addr, m.Data)
			if err := WriteMsg(ctx, conn, Msg{Type: MsgBusAck}); err != nil {
				a.dropConn(conn, err)
				return
			}
		case MsgHalfStepDone:
			a.stateMu.Lock()
			a.addrBus = m.BusAddr
			a.dataBus = m.BusData
			a.rw = m.RW
			a.syncPin = m.SYNC
			a.irq = m.IRQ
			a.nmi = m.NMI
			a.halfCycles++
			a.stateMu.Unlock()
			return
		default:
			// Unknown message — log and keep waiting; defensive
			// against future protocol additions we haven't taught
			// this client about yet.
			log.Printf("remote-cpu: unexpected msg %q during halfStep", m.Type)
		}
	}
}

// Reset sends a reset and waits for resetDone. Noop if disconnected.
// Clears local pin cache so accessors don't surface stale state.
func (a *Adapter) Reset() {
	a.stateMu.Lock()
	a.addrBus = 0
	a.dataBus = 0
	a.rw = false
	a.syncPin = false
	a.irq = true
	a.nmi = true
	a.regs = cpu.Registers{}
	a.halfCycles = 0
	a.stateMu.Unlock()

	conn := a.currentConn()
	if conn == nil {
		return
	}
	a.rpcMu.Lock()
	defer a.rpcMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := WriteMsg(ctx, conn, Msg{Type: MsgReset}); err != nil {
		a.dropConn(conn, err)
		return
	}
	// Reset may emit bus accesses (vector fetch on real silicon —
	// but we expect the CPU host to defer those to the next halfStep
	// for cleanliness). For v0a we just wait for resetDone.
	m, err := ReadMsg(ctx, conn)
	if err != nil {
		a.dropConn(conn, err)
		return
	}
	if m.Type != MsgResetDone {
		log.Printf("remote-cpu: expected resetDone, got %q", m.Type)
	}
}

// Registers returns the cached register snapshot. Issues a regsResp
// RPC to refresh first when connected — register state changes per
// halfStep but the Pump only reads registers when the UI repaints
// (~60 Hz), so the extra RTT is fine.
func (a *Adapter) Registers() cpu.Registers {
	conn := a.currentConn()
	if conn != nil {
		a.rpcMu.Lock()
		defer a.rpcMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := WriteMsg(ctx, conn, Msg{Type: MsgRegisters}); err != nil {
			a.dropConn(conn, err)
		} else if m, err := ReadMsg(ctx, conn); err != nil {
			a.dropConn(conn, err)
		} else if m.Type == MsgRegsResp {
			a.stateMu.Lock()
			a.regs = cpu.Registers{A: m.A, X: m.X, Y: m.Y, S: m.S, P: m.P, PC: m.PC}
			a.stateMu.Unlock()
		}
	}
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.regs
}

func (a *Adapter) HalfCycles() uint64 {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.halfCycles
}
func (a *Adapter) AddressBus() uint16 {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.addrBus
}
func (a *Adapter) DataBus() uint8 {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.dataBus
}
func (a *Adapter) ReadCycle() bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.rw
}
func (a *Adapter) IRQ() bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.irq
}
func (a *Adapter) NMI() bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.nmi
}
func (a *Adapter) SYNC() bool {
	a.stateMu.RLock()
	defer a.stateMu.RUnlock()
	return a.syncPin
}

// MaxHz reports the rate ceiling for the speed picker. Localhost
// WebSocket can sustain ~10 kHz (sub-ms RTT); over LAN that drops to
// ~1 kHz; over the open internet ~100 Hz. 10 kHz is the optimistic
// ceiling — picking it on a slow link just means actual delivery
// lags the setpoint (the rate display will read low, which is
// honest). 1 MHz / 2 MHz stay hidden because no remote configuration
// will ever get close.
func (a *Adapter) MaxHz() int { return 10000 }

var _ cpu.Backend = (*Adapter)(nil)

// currentConn returns the live conn or nil. Snapshot under lock so
// concurrent disconnect can't race a half-completed IO.
func (a *Adapter) currentConn() *websocket.Conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conn
}

// dropConn invalidates the conn after an IO error. Cancels handleWS's
// per-conn context so the HTTP handler returns promptly, freeing the
// slot before the client's reconnect attempt arrives. Also closes the
// underlying WS to nudge the client off the dead conn.
func (a *Adapter) dropConn(c *websocket.Conn, cause error) {
	log.Printf("remote-cpu: dropping conn: %v", cause)
	a.mu.Lock()
	if a.conn == c && a.cancelConn != nil {
		a.cancelConn()
	}
	a.mu.Unlock()
	_ = c.Close(websocket.StatusInternalError, fmt.Sprintf("io: %v", cause))
}

// ---------------- wire helpers ----------------

// ReadMsg reads one wire message off the WebSocket and decodes it.
// Exported so CPU-client binaries (cmd/6502-cpu-fake,
// cmd/cpu-host-wasm) share one canonical implementation instead of
// duplicating it.
func ReadMsg(ctx context.Context, c *websocket.Conn) (Msg, error) {
	_, data, err := c.Read(ctx)
	if err != nil {
		return Msg{}, err
	}
	var m Msg
	if err := Decode(data, &m); err != nil {
		return Msg{}, fmt.Errorf("decode: %w", err)
	}
	return m, nil
}

// WriteMsg encodes and sends one wire message as a text frame.
func WriteMsg(ctx context.Context, c *websocket.Conn, m Msg) error {
	data, err := Encode(m)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	return c.Write(ctx, websocket.MessageText, data)
}

