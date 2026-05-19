//go:build js && wasm

// Command 6502-core-wasm exposes the headless go6sim instrument API to
// JavaScript: the deterministic core (machine preset + instrument), with
// NO foxpro/tcell TUI — that is cmd/6502-wasm. This is the bridge the
// web <CodeLab> drives: go6asm.wasm assembles a program, JS hands the
// bytes to go6sim.load(), then steps/runs and reads state/memory.
//
// It is the ONLY file in cmd/6502-core-wasm that imports syscall/js;
// machine/ and instrument/ stay wasm-clean.
//
// JS surface (a `go6sim` global):
//
//	load(Uint8Array) -> {} | {error}   build teach-min, load ROM, reset
//	reset()          -> {} | {error}
//	step(n=1)        -> {} | {error}    advance n instructions
//	state()          -> {a,x,y,sp,p,pc,halfCycles,running,speedHz}|{error}
//	peek(addr)       -> int | {error}
//	mem(lo,hi)       -> Uint8Array | {error}   inclusive span
//	poke(addr,val)   -> {} | {error}
//	setRunning(bool) -> {} | {error}
//	running()        -> bool | {error}
//	advance(ms)      -> int | {error}   budget-tick; the RAF-loop driver
//	frame()          -> Uint8Array | {error}   teach-min framebuffer RAM
//	taps()           -> {name:number,…} | {error}
//	setSpeedHz(hz)   -> bool | {error}
//	setBreakpoint(addr)/clearBreakpoint(addr)/clearBreakpoints() -> {}
//	breakOnVector(bool) -> {} | {error}
//	runUntil(maxHalf)   -> {halfCycles,reason,addr} | {error}
package main

import (
	"syscall/js"
	"time"

	"github.com/carledwards/go6sim/machine"
)

// mach is the single loaded machine. The <CodeLab> is one machine per
// page; load() (re)creates it, so there is no handle to juggle.
var mach *machine.Machine

func main() {
	js.Global().Set("go6sim", js.ValueOf(map[string]any{
		"load":  js.FuncOf(load),
		"reset": js.FuncOf(reset),
		"step":  js.FuncOf(step),
		"state": js.FuncOf(state),
		"peek":  js.FuncOf(peek),
		"mem":   js.FuncOf(mem),
		"poke":  js.FuncOf(poke),

		"setRunning": js.FuncOf(setRunning),
		"running":    js.FuncOf(running),
		"advance":    js.FuncOf(advance),
		"frame":      js.FuncOf(frame),
		"taps":       js.FuncOf(taps),
		"setSpeedHz": js.FuncOf(setSpeedHz),

		"setBreakpoint":    js.FuncOf(setBreakpoint),
		"clearBreakpoint":  js.FuncOf(clearBreakpoint),
		"clearBreakpoints": js.FuncOf(clearBreakpoints),
		"breakOnVector":    js.FuncOf(breakOnVector),
		"runUntil":         js.FuncOf(runUntil),
	}))
	select {} // keep the Go runtime alive for the callbacks
}

func errResult(msg string) any { return map[string]any{"error": msg} }

func load(_ js.Value, args []js.Value) any {
	if len(args) < 1 {
		return errResult("load: expected a Uint8Array image")
	}
	src := args[0]
	buf := make([]byte, src.Get("length").Int())
	js.CopyBytesToGo(buf, src)
	m := machine.TeachMin()
	if err := m.Load(buf); err != nil {
		return errResult(err.Error())
	}
	mach = m
	return map[string]any{}
}

func reset(js.Value, []js.Value) any {
	if mach == nil {
		return errResult("reset: no program loaded")
	}
	mach.Inst.Reset()
	return map[string]any{}
}

func step(_ js.Value, args []js.Value) any {
	if mach == nil {
		return errResult("step: no program loaded")
	}
	n := 1
	if len(args) > 0 {
		n = args[0].Int()
	}
	mach.Inst.Step(n)
	return map[string]any{}
}

func state(js.Value, []js.Value) any {
	if mach == nil {
		return errResult("state: no program loaded")
	}
	s := mach.Inst.State()
	return map[string]any{
		"a": int(s.A), "x": int(s.X), "y": int(s.Y),
		"sp": int(s.S), "p": int(s.P), "pc": int(s.PC),
		"halfCycles": float64(s.HalfCycles),
		"running":    s.Running,
		"speedHz":    s.SpeedHz,
	}
}

func peek(_ js.Value, args []js.Value) any {
	if mach == nil {
		return errResult("peek: no program loaded")
	}
	if len(args) < 1 {
		return errResult("peek: expected an address")
	}
	return int(mach.Inst.Peek(uint16(args[0].Int())))
}

func mem(_ js.Value, args []js.Value) any {
	if mach == nil {
		return errResult("mem: no program loaded")
	}
	if len(args) < 2 {
		return errResult("mem: expected (lo, hi)")
	}
	b := mach.Inst.Mem(uint16(args[0].Int()), uint16(args[1].Int()))
	return toU8(b)
}

func toU8(b []byte) js.Value {
	out := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(out, b)
	return out
}

func poke(_ js.Value, args []js.Value) any {
	if mach == nil {
		return errResult("poke: no program loaded")
	}
	if len(args) < 2 {
		return errResult("poke: expected (addr, val)")
	}
	mach.Inst.Poke(uint16(args[0].Int()), uint8(args[1].Int()))
	return map[string]any{}
}

func setRunning(_ js.Value, args []js.Value) any {
	if mach == nil {
		return errResult("setRunning: no program loaded")
	}
	on := len(args) > 0 && args[0].Bool()
	mach.Inst.SetRunning(on)
	return map[string]any{}
}

func running(js.Value, []js.Value) any {
	if mach == nil {
		return errResult("running: no program loaded")
	}
	return mach.Inst.Running()
}

// advance runs the budget-tick clock for `ms` of virtual time and
// returns the half-cycles executed. This is the RAF-loop driver: JS
// calls setRunning(true) then advance(~16) each animation frame.
func advance(_ js.Value, args []js.Value) any {
	if mach == nil {
		return errResult("advance: no program loaded")
	}
	ms := 16
	if len(args) > 0 {
		ms = args[0].Int()
	}
	return mach.Inst.Advance(time.Duration(ms) * time.Millisecond)
}

// frame returns the teach-min framebuffer region ($A000-$AFFF) as raw
// bytes — resolved Q2: the sim ships bytes, the consumer renders.
func frame(js.Value, []js.Value) any {
	if mach == nil {
		return errResult("frame: no program loaded")
	}
	return toU8(mach.Inst.Mem(0xA000, 0xAFFF))
}

func taps(js.Value, []js.Value) any {
	if mach == nil {
		return errResult("taps: no program loaded")
	}
	out := map[string]any{}
	for k, v := range mach.Inst.Taps() {
		out[k] = float64(v)
	}
	return out
}

func setSpeedHz(_ js.Value, args []js.Value) any {
	if mach == nil {
		return errResult("setSpeedHz: no program loaded")
	}
	if len(args) < 1 {
		return errResult("setSpeedHz: expected hz")
	}
	return mach.Inst.Driver().SetSpeedHz(args[0].Int())
}

func setBreakpoint(_ js.Value, args []js.Value) any {
	if mach == nil {
		return errResult("setBreakpoint: no program loaded")
	}
	if len(args) < 1 {
		return errResult("setBreakpoint: expected addr")
	}
	mach.Inst.SetBreakpoint(uint16(args[0].Int()))
	return map[string]any{}
}

func clearBreakpoint(_ js.Value, args []js.Value) any {
	if mach == nil {
		return errResult("clearBreakpoint: no program loaded")
	}
	if len(args) < 1 {
		return errResult("clearBreakpoint: expected addr")
	}
	mach.Inst.ClearBreakpoint(uint16(args[0].Int()))
	return map[string]any{}
}

func clearBreakpoints(js.Value, []js.Value) any {
	if mach == nil {
		return errResult("clearBreakpoints: no program loaded")
	}
	mach.Inst.ClearBreakpoints()
	return map[string]any{}
}

func breakOnVector(_ js.Value, args []js.Value) any {
	if mach == nil {
		return errResult("breakOnVector: no program loaded")
	}
	mach.Inst.BreakOnVector(len(args) > 0 && args[0].Bool())
	return map[string]any{}
}

// runUntil is the deterministic debugger run: advance up to maxHalf
// half-cycles, stopping early on a breakpoint or a taken vector.
func runUntil(_ js.Value, args []js.Value) any {
	if mach == nil {
		return errResult("runUntil: no program loaded")
	}
	max := 1 << 20
	if len(args) > 0 {
		max = args[0].Int()
	}
	r := mach.Inst.RunUntil(max)
	return map[string]any{
		"halfCycles": float64(r.HalfCycles),
		"reason":     r.Reason,
		"addr":       int(r.Addr),
	}
}
