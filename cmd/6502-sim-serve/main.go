// Command 6502-sim-serve hosts the go6sim bridge protocol over TCP as
// newline-delimited JSON-RPC 2.0 (one JSON object per `\n`). Each TCP
// connection is one bridge session; preset construction goes through
// the local presetLoader. See docs/bridge.md for the wire contract.
//
// Default bind is 127.0.0.1 (loopback only). --insecure-bind unlocks
// non-loopback addresses for explicit deployments; v1 ships no auth
// (see docs/bridge.md §9), so the loopback-only default is the
// protection.
//
// Transport choice for v1 is NDJSON-over-TCP rather than WebSocket on
// purpose: stdlib-only (no new module deps), trivially testable with
// `nc`/`netcat`, and Node-side clients (the VS Code extension, MCP
// shim) connect via plain `net.Socket` without a WS upgrade dance.
// WebSocket can land as a second transport later without touching the
// bridge package — bridge.Conn is transport-agnostic.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/carledwards/go6sim/bridge"
	"github.com/carledwards/go6sim/internal/ndjson"
)

var (
	addr     = flag.String("addr", "127.0.0.1:6502", "TCP address to bind")
	insecure = flag.Bool("insecure-bind", false,
		"allow non-loopback binds (no auth in v1 — see docs/bridge.md §9)")
)

func main() {
	flag.Parse()

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		log.Fatalf("bad --addr %q: %v", *addr, err)
	}
	if !*insecure && !isLoopback(host) {
		log.Fatalf(
			"bind %q is non-loopback; v1 binds 127.0.0.1 only. "+
				"Pass --insecure-bind to override (no auth shipped yet).",
			*addr,
		)
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	log.Printf("6502-sim-serve listening on %s — NDJSON bridge (see docs/bridge.md)", ln.Addr())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Graceful shutdown on Ctrl-C / SIGTERM.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Print("shutting down…")
		cancel()
		_ = ln.Close()
	}()

	srv := bridge.NewServer(presetLoader{})
	if err := acceptLoop(ctx, ln, srv); err != nil {
		log.Fatalf("accept loop: %v", err)
	}
}

// acceptLoop owns the listener; each accepted conn is handed to a
// fresh bridge.Server.Serve in its own goroutine. Extracted so a test
// can drive the same loop against an in-memory listener.
func acceptLoop(ctx context.Context, ln net.Listener, srv *bridge.Server) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			log.Printf("accept: %v", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		log.Printf("client connected: %s", conn.RemoteAddr())
		go func(c net.Conn) {
			defer log.Printf("client disconnected: %s", c.RemoteAddr())
			if err := srv.Serve(ctx, ndjson.New(c)); err != nil {
				log.Printf("client %s: serve: %v", c.RemoteAddr(), err)
			}
		}(conn)
	}
}

func isLoopback(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
