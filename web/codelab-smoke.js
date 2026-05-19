// Headless end-to-end proof of the CodeLab pipeline (no browser):
// load BOTH Go wasm modules on one runtime — go6asm (assemble) and
// go6sim core (run) — assemble the 8-LED seed, load it into the
// teach-min machine, run, and assert the VIA Port B byte ($B000 = the
// 8 LEDs) actually changes. This is the integration the browser page
// also performs; proving it here gates the wiring before deploy.
'use strict';
const fs = require('fs');
const path = require('path');
require(path.join(__dirname, 'wasm_exec.js')); // globalThis.Go

// 8-LED binary counter on VIA Port B. Layer-0 (simple): go6asm infers
// the $E000 load address + vectors; ViaBase ($B000) is in the sim-tui
// symbol pack. The busy-wait keeps it visibly stepping.
const SEED = `
; 8 LEDs on VIA Port B — a binary counter.
        LDX #$00
loop:   STX ViaBase        ; ViaBase = $B000 = VIA port B (the 8 LEDs)
        INX
        LDY #$00
delay:  INY
        BNE delay
        JMP loop
`;

function startGo(file) {
  const go = new Go();
  const buf = fs.readFileSync(path.join(__dirname, file));
  return WebAssembly.instantiate(buf, go.importObject).then(({ instance }) => {
    go.run(instance); // blocks on select{}; sets its global first
  });
}

(async () => {
  await startGo('go6asm.wasm');
  await startGo('core.wasm');
  for (let i = 0; i < 200 && !(globalThis.go6asm && globalThis.go6sim); i++) {
    await new Promise((r) => setTimeout(r, 5));
  }
  const asm = globalThis.go6asm, sim = globalThis.go6sim;
  if (!asm) throw new Error('go6asm global not registered');
  if (!sim) throw new Error('go6sim global not registered');

  // assemble (Layer-0 / sim-tui target)
  const r = asm.assemble({ source: SEED, target: 'sim-tui', simple: true });
  if (!r.success) {
    throw new Error('assemble failed: ' + JSON.stringify(r.errors));
  }
  if (!(r.bytes instanceof Uint8Array) || r.bytes.length === 0) {
    throw new Error('assemble produced no image');
  }

  // load into the headless teach-min machine
  const lo = sim.load(r.bytes);
  if (lo && lo.error) throw new Error('go6sim.load: ' + lo.error);
  if (sim.state().pc !== 0xe000) throw new Error('reset PC != $E000');

  // run: the LED byte ($B000, VIA Port B) must change as the counter runs
  const VIA_PB = 0xb000;
  const before = sim.peek(VIA_PB);
  sim.setRunning(true);
  sim.setSpeedHz(0); // Max — exercise plenty of the counter quickly
  let changed = false,
    last = before;
  for (let f = 0; f < 30 && !changed; f++) {
    sim.advance(16); // a RAF-sized budget tick
    const v = sim.peek(VIA_PB);
    if (v !== last) changed = true;
    last = v;
  }
  sim.setRunning(false);

  if (!changed) {
    throw new Error('VIA Port B never changed — LEDs not driven (before=' + before + ')');
  }
  console.log(
    'codelab pipeline OK — go6asm.assemble -> go6sim.load -> run; ' +
      'VIA PB $B000 advanced ' + before + ' -> ' + last + ' (8 LEDs live)'
  );
  process.exit(0);
})().catch((e) => {
  console.error('CODELAB SMOKE FAIL:', e.message);
  process.exit(1);
});
