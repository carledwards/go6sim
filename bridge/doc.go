// Package bridge implements the go6sim bridge protocol — the JSON-RPC
// 2.0 surface that exposes instrument.Instrument to external clients
// (VS Code, MCP, future hardware bridges, any other editor).
//
// See docs/bridge.md for the design contract this package implements.
//
// This package is transport-agnostic: it operates over a [Conn] that
// reads and writes one JSON frame at a time. The production WebSocket
// binding lives in cmd/6502-sim-serve (TODO) and a `--serve <addr>`
// flag on cmd/6502-sim; tests use [Pipe] for an in-memory transport.
//
// Carve invariants honoured here:
//   - no syscall/js, no net, no GUI imports;
//   - no import of any concrete preset package (machine presets are
//     reached through a caller-supplied [Loader]) — which keeps bridge
//     dep-free of go6asm and reusable by any process that wants to
//     host an Instrument over the wire.
//
// The skeleton ships `hello`, `machine.list`, `machine.load`, and
// `cpu.state`. The remainder of docs/bridge.md §5 lands incrementally
// behind the same [register] pattern, gated by [Capabilities].
package bridge
