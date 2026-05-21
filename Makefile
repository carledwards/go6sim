.PHONY: build run tidy test clean wasm wasm-serve core-wasm core-smoke codelab-wasm codelab codelab-smoke

PORT       ?= 8765
WEB        := web
WASM_OUT   := $(WEB)/sim.wasm
CORE_OUT   := $(WEB)/core.wasm
GOASM_OUT  := $(WEB)/go6asm.wasm
GOASM_DIR  ?= ../go6asm
EXEC_OUT   := $(WEB)/wasm_exec.js
FOXPRO_OUT := $(WEB)/foxpro.js
GOROOT     := $(shell go env GOROOT)
EXEC_SRC   := $(firstword $(wildcard $(GOROOT)/lib/wasm/wasm_exec.js $(GOROOT)/misc/wasm/wasm_exec.js))
build:
	go build -o bin/6502-sim ./cmd/6502-sim

run: build
	./bin/6502-sim

# wasm — compile the browser build into web/ and copy the JS shim from
# the active Go toolchain. Re-run after editing any Go file; `go build`
# does its own dependency check so this is fast on no-op builds.
#
# foxpro.js is resolved at recipe time inside a single shell invocation:
# `go mod download` first guarantees the module is in $(GOMODCACHE),
# then `go list -m -f '{{.Dir}}'` returns the cache path. Computing
# this with `:=` at parse time fails on a fresh CI runner because the
# module isn't yet in the cache; computing with `=` (deferred) still
# proved unreliable when `go list -m` didn't populate the graph cache
# without an explicit `go mod download` first. So we just do both
# steps in one shell command, fail loudly if either step missed.
wasm:
	@mkdir -p $(WEB)
	GOOS=js GOARCH=wasm go build -o $(WASM_OUT) ./cmd/6502-wasm
	@if [ -z "$(EXEC_SRC)" ]; then \
	  echo "ERROR: wasm_exec.js not found under $(GOROOT)/{lib,misc}/wasm/"; \
	  exit 1; \
	fi
	@cp $(EXEC_SRC) $(EXEC_OUT)
	@set -e; \
	go mod download github.com/carledwards/foxpro-go; \
	src="$$(go list -m -f '{{.Dir}}' github.com/carledwards/foxpro-go)/wasm/foxpro.js"; \
	if [ ! -f "$$src" ]; then \
	  echo "ERROR: foxpro.js not found at $$src"; \
	  exit 1; \
	fi; \
	cp "$$src" $(FOXPRO_OUT)
	@ls -lh $(WASM_OUT) | awk '{print "  built " $$NF " (" $$5 ")"}'

# core-wasm — the headless instrument bridge (cmd/6502-core-wasm). No
# foxpro.js: it ships no TUI, just the deterministic core behind a JS
# API. Only needs the Go wasm + the wasm_exec.js shim.
core-wasm:
	@mkdir -p $(WEB)
	GOOS=js GOARCH=wasm go build -o $(CORE_OUT) ./cmd/6502-core-wasm
	@if [ -z "$(EXEC_SRC)" ]; then \
	  echo "ERROR: wasm_exec.js not found under $(GOROOT)/{lib,misc}/wasm/"; \
	  exit 1; \
	fi
	@cp $(EXEC_SRC) $(EXEC_OUT)
	@ls -lh $(CORE_OUT) | awk '{print "  built " $$NF " (" $$5 ")"}'

# core-smoke — build the core wasm and exercise the JS API end-to-end
# under Node (no browser needed). The carve's CI gate for the bridge.
core-smoke: core-wasm
	@command -v node >/dev/null || { echo "node not installed"; exit 1; }
	node $(WEB)/core-smoke.js

# codelab-wasm — build BOTH wasms the local CodeLab page drives: the
# go6sim headless core (this repo) and the go6asm assembler (sibling
# repo at $(GOASM_DIR)), into web/ next to wasm_exec.js. Local dev
# harness only — the deployed site builds these in CI from tags.
codelab-wasm: core-wasm
	@if [ ! -d "$(GOASM_DIR)" ]; then \
	  echo "ERROR: go6asm not found at $(GOASM_DIR) (set GOASM_DIR=/path)"; exit 1; \
	fi
	cd $(GOASM_DIR) && GOOS=js GOARCH=wasm go build -o "$(CURDIR)/$(GOASM_OUT)" ./cmd/go6asm-wasm
	@ls -lh $(GOASM_OUT) | awk '{print "  built " $$NF " (" $$5 ")"}'

# codelab-smoke — headless end-to-end proof: both wasms on one runtime,
# assemble the 8-LED seed, run it, assert VIA Port B changes. The
# pipeline gate before any deploy wiring.
codelab-smoke: codelab-wasm
	@command -v node >/dev/null || { echo "node not installed"; exit 1; }
	node $(WEB)/codelab-smoke.js

# codelab — serve the local spike page (open codelab.html).
codelab: codelab-wasm
	@echo "open http://localhost:$(PORT)/codelab.html"
	@cd $(WEB) && python3 -m http.server $(PORT)

# wasm-serve — rebuild then local static server on PORT (default 8765).
# Depends on `wasm` so a one-step `make wasm-serve` always serves the
# latest binary. Open http://localhost:$(PORT)/ once it starts and
# HARD-REFRESH the browser (Cmd-Shift-R / Ctrl-Shift-R) to dodge the
# .wasm cache that otherwise loads stale builds.
wasm-serve: wasm
	@echo "serving $(WEB)/ at http://localhost:$(PORT)/  (hard-refresh the browser)"
	@cd $(WEB) && python3 -m http.server $(PORT)

tidy:
	go mod tidy

test:
	go test ./...

clean:
	rm -rf bin $(WASM_OUT) $(CORE_OUT) $(GOASM_OUT) $(EXEC_OUT) $(FOXPRO_OUT)
