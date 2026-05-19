// Node smoke for cmd/6502-core-wasm (brick cw-1). Loads core.wasm,
// drives the go6sim JS API against a hand-built ROM, asserts the
// headless instrument works end-to-end. Run via `make core-smoke`.
'use strict';
const fs = require('fs');
const path = require('path');
require(path.join(__dirname, 'wasm_exec.js')); // defines globalThis.Go

(async () => {
  const go = new Go();
  const buf = fs.readFileSync(path.join(__dirname, 'core.wasm'));
  const { instance } = await WebAssembly.instantiate(buf, go.importObject);
  // main() ends in select{} and never returns; it sets globalThis.go6sim
  // synchronously before blocking, so start it (don't await) then yield.
  go.run(instance);
  for (let i = 0; i < 100 && !globalThis.go6sim; i++) {
    await new Promise((r) => setTimeout(r, 5));
  }
  const g = globalThis.go6sim;
  if (!g) throw new Error('go6sim global was never registered');

  // Tiny ROM ($E000-$FFFF). Reset vector -> $E000.
  //   E000  A9 42     LDA #$42
  //   E002  85 00     STA $00
  //   E004  4C 04 E0  JMP $E004   (spin)
  const rom = new Uint8Array(0x2000);
  rom.set([0xa9, 0x42, 0x85, 0x00, 0x4c, 0x04, 0xe0], 0);
  rom[0x1ffc] = 0x00;
  rom[0x1ffd] = 0xe0;

  const fail = (m) => { throw new Error(m); };

  let r = g.load(rom);
  if (r && r.error) fail('load: ' + r.error);

  let s = g.state();
  if (s.error) fail('state: ' + s.error);
  if (s.pc !== 0xe000) fail('after load/reset pc=$' + s.pc.toString(16) + ', want $E000');

  g.step(2); // LDA #$42 ; STA $00
  s = g.state();
  if (s.a !== 0x42) fail('A=$' + s.a.toString(16) + ', want $42');

  if (g.peek(0x0000) !== 0x42) fail('peek($00)=' + g.peek(0x0000) + ', want 0x42');

  const m = g.mem(0x0000, 0x0003);
  if (!(m instanceof Uint8Array) || m.length !== 4 || m[0] !== 0x42) {
    fail('mem($0,$3) bad: ' + JSON.stringify(Array.from(m || [])));
  }

  // reset returns to the vector and clears the run.
  g.reset();
  if (g.state().pc !== 0xe000) fail('after reset pc != $E000');

  // --- cw-2 surface ---

  g.poke(0x0010, 0x99);
  if (g.peek(0x0010) !== 0x99) fail('poke/peek mismatch');

  // frame = teach-min framebuffer region $A000-$AFFF, raw bytes.
  const fb = g.frame();
  if (!(fb instanceof Uint8Array) || fb.length !== 0x1000) {
    fail('frame() size = ' + (fb && fb.length) + ', want 4096');
  }

  // taps aggregate every card; teach-min has the VIA.
  const tp = g.taps();
  if (typeof tp !== 'object' || !('via1.irq' in tp)) {
    fail('taps missing via1.irq: ' + JSON.stringify(tp));
  }

  // budget-tick run advances the clock (the RAF-loop driver).
  g.setRunning(true);
  if (!(g.advance(50) > 0)) fail('advance returned no half-cycles');
  if (g.running() !== true) fail('running() not true after setRunning(true)');
  g.setRunning(false);

  // breakpoint via runUntil: stop at the JMP-self at $E004.
  g.reset();
  g.breakOnVector(false);
  g.setBreakpoint(0xe004);
  const rr = g.runUntil(2000);
  if (rr.reason !== 'breakpoint' || rr.addr !== 0xe004) {
    fail('runUntil = ' + JSON.stringify(rr) + ', want breakpoint @E004');
  }

  console.log(
    'core-wasm smoke OK — cw-1 (load/reset/step/state/peek/mem) + ' +
      'cw-2 (poke/frame/taps/advance/runUntil-breakpoint)'
  );
  process.exit(0);
})().catch((e) => {
  console.error('SMOKE FAIL:', e.message);
  process.exit(1);
});
