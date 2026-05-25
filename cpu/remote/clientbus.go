package remote

import (
	"context"
	"log"
	"sync"

	"github.com/coder/websocket"

	"github.com/carledwards/go6sim/bus"
)

// ClientBus is the CPU-side proxy that satisfies bus.Bus by tunnelling
// every Read/Write across a WebSocket back to the TUI's real RAM /
// ROM / peripherals. CPU-host binaries (the Go fake, the browser
// wasm) instantiate ONE ClientBus at startup, hand it to their
// internal CPU (interp / netsim), and swap the underlying conn on
// reconnect via SetConn — the CPU adapter itself never sees the
// connection swap.
//
// Register / Components are no-ops because no real CPU backend calls
// them at runtime; they exist on the bus.Bus interface for the
// host's wiring code, which doesn't run on the client side.
type ClientBus struct {
	mu       sync.Mutex
	conn     *websocket.Conn
	ctx      context.Context
	onAccess func(op string, addr uint16, data uint8) // optional UI hook
}

// NewClientBus returns a ClientBus with no conn set. Call SetConn
// after dialing to make Read/Write functional. The optional
// onAccess callback fires after every successful bus transaction;
// use it to drive a "last bus op" status panel.
func NewClientBus(onAccess func(op string, addr uint16, data uint8)) *ClientBus {
	return &ClientBus{onAccess: onAccess}
}

// SetConn swaps the active connection (and its lifecycle context).
// Pass conn=nil to detach — Read/Write then return zero values until
// a new conn is installed.
func (c *ClientBus) SetConn(conn *websocket.Conn, ctx context.Context) {
	c.mu.Lock()
	c.conn = conn
	c.ctx = ctx
	c.mu.Unlock()
}

func (c *ClientBus) current() (*websocket.Conn, context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn, c.ctx
}

func (c *ClientBus) Read(addr uint16) uint8 {
	conn, ctx := c.current()
	if conn == nil {
		return 0
	}
	if err := WriteMsg(ctx, conn, Msg{Type: MsgBusRead, Addr: addr}); err != nil {
		log.Printf("clientbus read send: %v", err)
		return 0
	}
	m, err := ReadMsg(ctx, conn)
	if err != nil {
		log.Printf("clientbus read recv: %v", err)
		return 0
	}
	if c.onAccess != nil {
		c.onAccess("read", addr, m.Data)
	}
	return m.Data
}

func (c *ClientBus) Write(addr uint16, val uint8) {
	conn, ctx := c.current()
	if conn == nil {
		return
	}
	if err := WriteMsg(ctx, conn, Msg{Type: MsgBusWrite, Addr: addr, Data: val}); err != nil {
		log.Printf("clientbus write send: %v", err)
		return
	}
	if _, err := ReadMsg(ctx, conn); err != nil {
		log.Printf("clientbus write recv: %v", err)
		return
	}
	if c.onAccess != nil {
		c.onAccess("write", addr, val)
	}
}

func (c *ClientBus) Register(_ bus.Component) error { return nil }
func (c *ClientBus) Components() []bus.Component    { return nil }
