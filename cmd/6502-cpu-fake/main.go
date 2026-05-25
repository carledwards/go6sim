// Command 6502-cpu-fake is a Remote-CPU client that runs the standard
// interp backend internally and proxies every bus access over the
// WebSocket protocol defined in cpu/remote/proto.go. It's the
// reference Go client — useful as a smoke test for the wire without
// pulling a browser into the loop.
//
// Usage: ./6502-cpu-fake -addr=ws://localhost:7777/cpu
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/carledwards/go6sim/cpu/interp"
	"github.com/carledwards/go6sim/cpu/remote"
)

var addr = flag.String("addr", "ws://localhost:7777/cpu", "TUI WebSocket endpoint")

func main() {
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	for {
		if err := run(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("session ended: %v — reconnecting in 1s", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		return
	}
}

func run(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	c, _, err := websocket.Dial(dialCtx, *addr, nil)
	cancel()
	if err != nil {
		return err
	}
	defer c.Close(websocket.StatusNormalClosure, "shutdown")
	c.SetReadLimit(1 << 20)

	if err := remote.WriteMsg(ctx, c, remote.Msg{Type: remote.MsgHello, Role: "cpu", Kind: "interp"}); err != nil {
		return err
	}

	wb := remote.NewClientBus(nil)
	wb.SetConn(c, ctx)
	a := interp.New(wb)

	// Service loop — read TUI commands and dispatch. Bus replies
	// (busData/busAck) are NOT pulled from here — they're consumed
	// inside ClientBus.Read/Write during a halfStep / reset, and the
	// protocol is strictly request/response so the next read after
	// HalfStep returns is guaranteed to be the next TUI command.
	for {
		m, err := remote.ReadMsg(ctx, c)
		if err != nil {
			return err
		}
		switch m.Type {
		case remote.MsgHalfStep:
			a.HalfStep()
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
			log.Printf("fake-cpu: unexpected msg %q", m.Type)
		}
	}
}
