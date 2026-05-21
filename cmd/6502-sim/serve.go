package main

// This file holds the bridge-listener half of cmd/6502-sim — the
// `--serve <addr>` flag's plumbing. It's deliberately separated from
// main.go so the TUI's existing wiring stays unchanged; the only
// edits in main.go are (1) parse the flag, (2) call StartServe if
// set, and (3) consult `remote.Active()` in the keyboard handler to
// lock out clock-affecting keys while a controller is connected.
//
// Design (docs/bridge.md, plus the user's chunk-B decision):
//
//   - The TUI builds its own Instrument as it always has.
//   - With --serve, a bridge.Server is attached to *that same*
//     Instrument — not a fresh one per session. The loader returns
//     the same inst regardless of preset name; the bridge's
//     `machine.load` call from a client becomes a no-op (or an
//     optional re-imaging via the image param).
//   - When a client is connected, `remote.Active()` returns true.
//     The TUI's keyboard handler treats Run/Stop/Step/Reset as
//     "consumed but ignored." Clock ownership transfers to the
//     controller for the duration of the connection.
//   - On disconnect the flag clears; TUI resumes driving.
//
// Concurrency caveats (honest):
//
//   The bridge runner mutates inst between slices; the TUI's render
//   path reads inst.State() / mem on its own goroutines. The lockout
//   prevents two MUTATORS from running at once, but readers and a
//   mutator still race for the value-typed State copy. In practice
//   that's a torn read that recovers within one ~33 ms frame — visual
//   only, never corrupting. Tightening to a shared Instrument mutex
//   is a worthwhile future change; for v1 the looseness is the
//   trade for not touching the entire TUI render pipeline.

import (
	"context"
	"errors"
	"log"
	"net"
	"sync/atomic"
	"time"

	"github.com/carledwards/go6sim/bridge"
	"github.com/carledwards/go6sim/internal/ndjson"
)

// RemoteState tracks how many bridge clients are currently connected
// to this TUI. The keyboard handler only needs Active(); the counter
// itself is bumped/decremented around each Serve invocation.
type RemoteState struct {
	count atomic.Int64
}

// Active reports whether at least one bridge client is currently
// driving. The TUI's keyboard handler checks this before taking
// clock-affecting actions; when true, those keys are suppressed.
func (r *RemoteState) Active() bool { return r.count.Load() > 0 }

// Count returns the live client count (informational; the TUI may
// surface it as a status indicator like "[REMOTE: 1]").
func (r *RemoteState) Count() int64 { return r.count.Load() }

func (r *RemoteState) enter() { r.count.Add(1) }
func (r *RemoteState) leave() { r.count.Add(-1) }

// sharedLoader implements bridge.Loader by returning the TUI's
// shared Hub (built once at TUI startup). Every machine.load call
// from any bridge client returns the SAME Hub — i.e. all attached
// controllers see the same machine. preset-name routing collapses
// to a no-op (the TUI's machine was chosen at startup); image, if
// given, is written through to ROM in place via the caller-supplied
// loadImage closure (so the reset vector + clock-reset happen in the
// same path the keyboard's 'z' uses).
//
// cleanup returned by Load is a no-op — the Hub outlives any single
// bridge session; only process exit retires it.
type sharedLoader struct {
	hub    *bridge.Hub
	preset string

	loadImage func(image []byte) error
}

func (l *sharedLoader) Presets() []bridge.PresetInfo {
	return []bridge.PresetInfo{
		{
			Name:    l.preset,
			Label:   "Sim TUI (shared)",
			Summary: "the TUI's live machine — shared via --serve",
		},
	}
}

func (l *sharedLoader) Load(name string, image []byte) (*bridge.Hub, func(), error) {
	if len(image) > 0 && l.loadImage != nil {
		if err := l.loadImage(image); err != nil {
			return nil, nil, err
		}
	}
	return l.hub, func() {}, nil
}

// newSimTUILoader returns a bridge.Loader bound to the TUI's
// existing Hub. loadImage is the caller-supplied closure that
// re-images ROM + resets the clock — wired by main to the same path
// the 'z' keyboard demo-reload uses.
func newSimTUILoader(hub *bridge.Hub, loadImage func([]byte) error) bridge.Loader {
	return &sharedLoader{
		hub:       hub,
		preset:    "sim-tui",
		loadImage: loadImage,
	}
}

// StartServe binds the TCP listener, attaches a bridge.Server backed
// by the shared inst, and accepts connections forever in a goroutine.
// Returns an error only if the initial bind fails — once accepted,
// per-connection errors are logged but never bubble up. The listener
// is closed when ctx is cancelled.
//
// remote.Active() flips true on each accepted connection and back to
// false when it closes; multiple simultaneous clients are accumulated
// (the bridge per-session contract is fine with this — they all share
// the same Instrument, only one is driving the clock at a time per
// design).
func StartServe(
	ctx context.Context,
	addr string,
	loader bridge.Loader,
	remote *RemoteState,
) error {
	if !isLoopback(hostOf(addr)) {
		// Honor docs/bridge.md §9 even from the TUI variant: refuse
		// non-loopback binds by default. Operators who really need
		// remote access can set up an SSH tunnel; v1 ships no auth.
		return errors.New("serve: non-loopback bind refused (loopback only in v1)")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("6502-sim --serve listening on %s (NDJSON bridge; see docs/bridge.md)", ln.Addr())

	srv := bridge.NewServer(loader)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				if errors.Is(err, net.ErrClosed) {
					return
				}
				log.Printf("--serve accept: %v", err)
				time.Sleep(50 * time.Millisecond)
				continue
			}
			log.Printf("--serve client connected: %s", c.RemoteAddr())
			remote.enter()
			go func(conn net.Conn) {
				defer remote.leave()
				defer log.Printf("--serve client disconnected: %s", conn.RemoteAddr())
				if err := srv.Serve(ctx, ndjson.New(conn)); err != nil {
					log.Printf("--serve client %s: %v", conn.RemoteAddr(), err)
				}
			}(c)
		}
	}()
	return nil
}

func hostOf(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func isLoopback(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
