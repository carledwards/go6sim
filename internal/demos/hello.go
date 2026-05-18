package demos

// hello.go wires the first go6asm-built demo. Unlike the others, its
// program comes from a ca65 .s file assembled by go6asm — the
// canonical path the fluent Builder is being migrated to.

import (
	_ "embed"

	"github.com/carledwards/go6sim/asm"
)

// .s files live in asmsrc/ (a non-package dir) so the Go toolchain
// doesn't try to assemble them as Plan 9 assembly.
//
//go:embed asmsrc/hello.s
var helloSrc []byte

// HelloDemo is built from internal/demos/hello.s at package init via
// go6asm. A failure here is a bug in the bundled .s file.
var HelloDemo = Demo{
	Name: "&Hello (go6asm)",
	Description: []string{
		"The first demo assembled from a ca65 .s file by",
		"go6asm — not the fluent Go builder. Clears the",
		"screen and prints a banner on row 6.",
	},
	Program: asm.MustFromSource("hello.s", helloSrc),
}
