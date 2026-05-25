// Package cpuhost embeds the Remote-CPU browser host page assets so
// the TUI's HTTP listener can serve them at / when running with
// -cpu=remote. Embedding (vs serving from disk) keeps the binary
// self-contained — no relative-path dance, no missing files when
// the user runs the binary from an unexpected cwd, and the host
// page is on the same origin as /cpu so no cross-port dance either.
//
// Run `make host-wasm` whenever cmd/cpu-host-wasm changes to refresh
// the embedded host.wasm — `go build` of the TUI then bakes the
// updated bytes in.
package cpuhost

import (
	"embed"
	"io/fs"
)

//go:embed index.html host.js host.wasm wasm_exec.js foxpro.js
var files embed.FS

// FS returns the embedded filesystem rooted at this directory, ready
// to hand to http.FileServer(http.FS(cpuhost.FS())).
func FS() fs.FS { return files }
