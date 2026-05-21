// Package bridgeclient is a JSON-RPC 2.0 client for the go6sim bridge
// protocol (see go6sim/docs/bridge.md). It speaks the same NDJSON wire
// format as cmd/6502-sim-serve — one JSON object per `\n` — so any
// in-process binary that needs to drive a remote simulator imports
// this package instead of rolling its own client.
//
// First consumer: cmd/6502-control (the foxpro controller TUI).
//
// Concurrency: Call is safe to invoke from any goroutine; responses
// are routed by id. Server-pushed notifications surface on the
// channel returned by Notifications() — drain it promptly or messages
// drop (the design's tap-coalescing rationale: client must keep up).
package bridgeclient

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/carledwards/go6sim/bridge"
)

// Notification mirrors a JSON-RPC notification frame the server pushes
// (no id, method+params). Channels are server → client only.
type Notification struct {
	Method string
	Params json.RawMessage
}

// Client wraps an io.ReadWriteCloser carrying NDJSON-framed JSON-RPC
// 2.0. The reader goroutine demuxes responses (id-matched) and
// notifications (no id). Construction starts the reader; Close stops
// it cleanly.
type Client struct {
	rwc io.ReadWriteCloser
	r   *bufio.Reader

	writeMu sync.Mutex // serialises rwc.Write
	nextID  atomic.Int64

	mu      sync.Mutex // guards pending + closed
	pending map[int64]chan rawResponse
	closed  bool

	notifyCh chan Notification
	done     chan struct{}
}

type rawResponse struct {
	Result json.RawMessage
	Err    *bridge.Error
}

// Dial connects to a bridge server over TCP and returns a ready Client
// (reader goroutine started). Caller must Close() it when done.
//
// No hello is performed here; call (*Client).Hello after Dial, then
// machine.load, etc. — matching docs/bridge.md §4 lifecycle.
func Dial(addr string) (*Client, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bridgeclient: dial %s: %w", addr, err)
	}
	return New(conn), nil
}

// New wraps an existing connection (typically the result of net.Dial,
// but anything ReadWriteCloser-shaped works — stdio, in-memory pipes,
// a WebSocket bridge later). The reader goroutine starts immediately.
func New(rwc io.ReadWriteCloser) *Client {
	c := &Client{
		rwc:      rwc,
		r:        bufio.NewReader(rwc),
		pending:  map[int64]chan rawResponse{},
		notifyCh: make(chan Notification, 128),
		done:     make(chan struct{}),
	}
	go c.readLoop()
	return c
}

func (c *Client) readLoop() {
	defer close(c.done)
	for {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			c.shutdown()
			return
		}
		for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
			line = line[:len(line)-1]
		}
		if len(line) == 0 {
			continue
		}

		var raw struct {
			ID     *json.RawMessage `json:"id"`
			Result json.RawMessage  `json:"result"`
			Error  *bridge.Error    `json:"error"`
			Method string           `json:"method"`
			Params json.RawMessage  `json:"params"`
		}
		if err := json.Unmarshal(line, &raw); err != nil {
			// Garbage frame — server shouldn't send any; skip.
			continue
		}

		if raw.ID != nil {
			var id int64
			_ = json.Unmarshal(*raw.ID, &id)
			c.mu.Lock()
			ch, ok := c.pending[id]
			if ok {
				delete(c.pending, id)
			}
			c.mu.Unlock()
			if ok {
				ch <- rawResponse{Result: raw.Result, Err: raw.Error}
			}
			continue
		}

		// Notification — best-effort delivery. If the consumer hasn't
		// drained notifyCh, drop the new one rather than block the
		// reader (which would also stall response routing).
		select {
		case c.notifyCh <- Notification{Method: raw.Method, Params: raw.Params}:
		default:
		}
	}
}

func (c *Client) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()

	// Wake any in-flight Call() so it doesn't block forever.
	closedErr := &bridge.Error{Code: -1, Message: "bridgeclient: connection closed"}
	for _, ch := range pending {
		select {
		case ch <- rawResponse{Err: closedErr}:
		default:
		}
	}

	// Close the notification channel so consumers ranging over it
	// can detect the connection's death and (e.g.) loop back to wait
	// for an auto-reconnect to swap in a fresh client. Safe because
	// shutdown runs after the read loop has stopped producing
	// notifications — no goroutine is still attempting a send.
	close(c.notifyCh)
}

// Call sends a request and waits for the matching response. `method`
// is one of the docs/bridge.md §5 methods; `params` is any
// JSON-marshalable value (nil = no params field on the wire).
//
// Returns: result body (raw JSON; unmarshal into the appropriate
// bridge.* result type at the call site) and a typed *bridge.Error
// (nil on success). A non-nil Error with Code == -1 means a transport
// failure (connection closed, marshal error); other codes are the
// server's reserved set per docs/bridge.md §6.
func (c *Client) Call(method string, params any) (json.RawMessage, *bridge.Error) {
	return c.callCtx(context.Background(), method, params)
}

// CallCtx is Call with cancellation/deadline support.
func (c *Client) CallCtx(ctx context.Context, method string, params any) (json.RawMessage, *bridge.Error) {
	return c.callCtx(ctx, method, params)
}

func (c *Client) callCtx(ctx context.Context, method string, params any) (json.RawMessage, *bridge.Error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, &bridge.Error{Code: -1, Message: "bridgeclient: client closed"}
	}
	id := c.nextID.Add(1)
	ch := make(chan rawResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	idRaw, _ := json.Marshal(id)
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(idRaw),
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	body, err := json.Marshal(req)
	if err != nil {
		c.dropPending(id)
		return nil, &bridge.Error{Code: -1, Message: "bridgeclient: marshal: " + err.Error()}
	}

	c.writeMu.Lock()
	_, werr := c.rwc.Write(append(body, '\n'))
	c.writeMu.Unlock()
	if werr != nil {
		c.dropPending(id)
		return nil, &bridge.Error{Code: -1, Message: "bridgeclient: write: " + werr.Error()}
	}

	select {
	case r := <-ch:
		return r.Result, r.Err
	case <-ctx.Done():
		c.dropPending(id)
		return nil, &bridge.Error{Code: -1, Message: "bridgeclient: " + ctx.Err().Error()}
	}
}

func (c *Client) dropPending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Notifications returns the channel server-pushed notifications arrive
// on. Drain it promptly — when full, new notifications are dropped
// (best-effort, matches the design's tap-coalescing intent).
func (c *Client) Notifications() <-chan Notification { return c.notifyCh }

// Done is closed when the reader goroutine has exited (peer closed,
// read error, or local Close). Use it as a connection-liveness signal.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close shuts down the connection and waits for the reader to exit.
// Idempotent.
func (c *Client) Close() error {
	err := c.rwc.Close()
	<-c.done
	return err
}

// --- typed convenience wrappers ---
//
// These wrap Call with the right param/result types so callers don't
// re-unmarshal at each site. The full method catalog from
// docs/bridge.md §5 isn't all wrapped yet — the controller (A2) will
// add helpers as it needs them. Wrappers are pure thin layers; raw
// Call always works for anything not yet wrapped.

func (c *Client) Hello(clientName, clientVersion string) (bridge.HelloResult, *bridge.Error) {
	res, e := c.Call("hello", bridge.HelloParams{
		ClientName:    clientName,
		ClientVersion: clientVersion,
		Protocols:     []string{bridge.Protocol},
	})
	if e != nil {
		return bridge.HelloResult{}, e
	}
	var out bridge.HelloResult
	_ = json.Unmarshal(res, &out)
	return out, nil
}

func (c *Client) MachineLoad(preset string, image []byte) (bridge.MachineLoadResult, *bridge.Error) {
	p := bridge.MachineLoadParams{Preset: preset}
	if len(image) > 0 {
		p.Image = &bridge.Image{
			BytesB64: encodeB64(image),
			Origin:   0xE000,
		}
	}
	res, e := c.Call("machine.load", p)
	if e != nil {
		return bridge.MachineLoadResult{}, e
	}
	var out bridge.MachineLoadResult
	_ = json.Unmarshal(res, &out)
	return out, nil
}

func (c *Client) CPUState() (bridge.CPUState, *bridge.Error) {
	res, e := c.Call("cpu.state", nil)
	if e != nil {
		return bridge.CPUState{}, e
	}
	var out bridge.CPUState
	_ = json.Unmarshal(res, &out)
	return out, nil
}

func (c *Client) Subscribe(channels ...string) (bridge.EventsSubscribeResult, *bridge.Error) {
	res, e := c.Call("events.subscribe", bridge.EventsSubscribeParams{Channels: channels})
	if e != nil {
		return bridge.EventsSubscribeResult{}, e
	}
	var out bridge.EventsSubscribeResult
	_ = json.Unmarshal(res, &out)
	return out, nil
}

func (c *Client) Run(speedHz int) *bridge.Error {
	var p bridge.ClockRunParams
	if speedHz > 0 {
		p.SpeedHz = &speedHz
	}
	_, e := c.Call("clock.run", p)
	return e
}

func (c *Client) Stop() (bridge.ClockStopResult, *bridge.Error) {
	res, e := c.Call("clock.stop", nil)
	if e != nil {
		return bridge.ClockStopResult{}, e
	}
	var out bridge.ClockStopResult
	_ = json.Unmarshal(res, &out)
	return out, nil
}

func (c *Client) Step(n int) (bridge.ClockStepResult, *bridge.Error) {
	res, e := c.Call("clock.step", bridge.ClockStepParams{N: n})
	if e != nil {
		return bridge.ClockStepResult{}, e
	}
	var out bridge.ClockStepResult
	_ = json.Unmarshal(res, &out)
	return out, nil
}

// IRQ pulses the host-driven IRQ line. Fire-and-forget — the CPU
// services on the next instruction boundary (if I flag is clear) and
// a state.snapshot reflects the post-vector PC. Holds for one Pump
// slice or until the next step, whichever comes first; ignored when
// the CPU has I=1 (interrupts masked).
func (c *Client) IRQ() *bridge.Error {
	_, e := c.Call("cpu.irq", nil)
	return e
}

// SetPC overwrites the CPU's program counter and returns the
// post-set state. Used by the monitor's `g <addr>` flow (set PC,
// then run). Errors with backend-doesn't-support when the server is
// running netsim CPU mode.
func (c *Client) SetPC(pc uint16) (bridge.CPUState, *bridge.Error) {
	res, e := c.Call("cpu.setPC", bridge.CPUSetPCParams{PC: pc})
	if e != nil {
		return bridge.CPUState{}, e
	}
	var out bridge.CPUSetPCResult
	if err := json.Unmarshal(res, &out); err != nil {
		return bridge.CPUState{}, &bridge.Error{Code: -1, Message: "cpu.setPC: " + err.Error()}
	}
	return out.State, nil
}

// NMI pulses the host-driven NMI line. Edge-triggered: rises on
// assertion, drops at the auto-deassert, the CPU services exactly
// once per pulse via $FFFA. NOT masked by the I flag — NMI always
// fires.
//
// netsim CPU mode currently a no-op (upstream needs SetNMI).
func (c *Client) NMI() *bridge.Error {
	_, e := c.Call("cpu.nmi", nil)
	return e
}

// MemPeek reads `n` bytes starting at `addr` from the bridge's bus.
// n=0 is normalised to 1 by the server; the wire response is base64-
// encoded, which this wrapper decodes for the caller's convenience.
func (c *Client) MemPeek(addr uint16, n int) ([]byte, *bridge.Error) {
	p := bridge.MemPeekParams{Addr: addr, N: n}
	res, e := c.Call("mem.peek", p)
	if e != nil {
		return nil, e
	}
	var out bridge.MemPeekResult
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, &bridge.Error{Code: -1, Message: "mem.peek: " + err.Error()}
	}
	raw, derr := base64.StdEncoding.DecodeString(out.BytesB64)
	if derr != nil {
		return nil, &bridge.Error{Code: -1, Message: "mem.peek: base64: " + derr.Error()}
	}
	return raw, nil
}

// MemPoke writes the given bytes to memory at `addr` through the bus
// (so devices observe the writes — VIA register pokes really clock
// the chip). Returns the number of bytes the server accepted.
func (c *Client) MemPoke(addr uint16, b []byte) (int, *bridge.Error) {
	p := bridge.MemPokeParams{
		Addr:     addr,
		BytesB64: base64.StdEncoding.EncodeToString(b),
	}
	res, e := c.Call("mem.poke", p)
	if e != nil {
		return 0, e
	}
	var out bridge.MemPokeResult
	if err := json.Unmarshal(res, &out); err != nil {
		return 0, &bridge.Error{Code: -1, Message: "mem.poke: " + err.Error()}
	}
	return out.Written, nil
}

// BPSet arms an address breakpoint and returns the session-scoped id
// the server assigns (e.g. "bp#1"). Use Run / Step to actually reach
// the bp; once hit, the bp.hit event delivers the same id.
func (c *Client) BPSet(addr uint16) (bridge.BPSetResult, *bridge.Error) {
	res, e := c.Call("bp.set", bridge.BPSetParams{Addr: addr})
	if e != nil {
		return bridge.BPSetResult{}, e
	}
	var out bridge.BPSetResult
	_ = json.Unmarshal(res, &out)
	return out, nil
}

// BPClear clears the breakpoint with the given id. Empty id clears
// ALL address breakpoints (matches the server's documented behaviour).
func (c *Client) BPClear(id string) (bridge.BPClearResult, *bridge.Error) {
	res, e := c.Call("bp.clear", bridge.BPClearParams{ID: id})
	if e != nil {
		return bridge.BPClearResult{}, e
	}
	var out bridge.BPClearResult
	_ = json.Unmarshal(res, &out)
	return out, nil
}

// BPList returns every breakpoint currently armed on this session.
func (c *Client) BPList() (bridge.BPListResult, *bridge.Error) {
	res, e := c.Call("bp.list", nil)
	if e != nil {
		return bridge.BPListResult{}, e
	}
	var out bridge.BPListResult
	_ = json.Unmarshal(res, &out)
	return out, nil
}

// Reset issues machine.reset on the server: shallow CPU + clock-driver
// reset, returning the post-reset CPU state. RAM and peripheral state
// are NOT cleared — for that, call MachineLoad again with a fresh
// image.
func (c *Client) Reset() (bridge.CPUState, *bridge.Error) {
	res, e := c.Call("machine.reset", nil)
	if e != nil {
		return bridge.CPUState{}, e
	}
	var out struct {
		State bridge.CPUState `json:"state"`
	}
	_ = json.Unmarshal(res, &out)
	return out.State, nil
}

func encodeB64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
