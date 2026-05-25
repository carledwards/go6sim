// sim.js — boots the 6502 simulator wasm and hands the canvas off
// to FoxproRender (shared renderer shipped by foxpro-go). All cell
// painting, pixel-layer compositing, drop-shadow tinting, and
// keyboard/mouse plumbing lives in foxpro.js.
(() => {
  const canvas = document.getElementById('screen');
  const statusEl = document.getElementById('status');

  // Attach binds the renderer to the canvas. It waits for the
  // wasm bridge to publish itself (via window.onFoxproReady) before
  // starting the per-frame loop.
  //
  // Responsive cell size + grid:
  //   desktop (>720 px): fontPx=12 — paired with a 160×40 cell grid
  //                       in cmd/6502-wasm/main.go (~1280×720 canvas).
  //   mobile  (≤720 px): fontPx=8  — paired with a 140×35 cell grid
  //                       (~700×490 canvas). Both the JS-side font
  //                       and the Go-side grid pivot off the same
  //                       720 px breakpoint (also used by index.html
  //                       for the single-column info-card reflow).
  // One-shot at page load — neither side reflows on rotate / resize.
  const isMobile = window.matchMedia('(max-width: 720px)').matches;
  const fontPx = isMobile ? 8 : 12;
  FoxproRender.attach(canvas, { statusEl, fontPx });

  // Quick-pick demo buttons. Populated once the wasm bridge has
  // published its sim-specific globals (window.go6sim) — that
  // happens during setup, before wasm.Run, so by the time
  // onFoxproReady fires the catalog is callable.
  const prevReady = window.onFoxproReady;
  window.onFoxproReady = () => {
    if (typeof prevReady === 'function') {
      try { prevReady(); } catch (e) { console.warn('[sim] prev onFoxproReady threw', e); }
    }
    if (!window.go6sim || typeof window.go6sim.quickPicks !== 'function') return;
    const picksEl = document.getElementById('picks');
    if (!picksEl) return;
    const picks = window.go6sim.quickPicks();
    picksEl.innerHTML = '';
    for (const p of picks) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.dataset.name = p.name;
      btn.textContent = p.label;
      btn.addEventListener('click', () => {
        window.go6sim.applyPreset(p.name);
        // Return keyboard focus to the canvas so the user can keep
        // driving the sim without re-clicking it.
        canvas.focus();
      });
      picksEl.appendChild(btn);
    }
  };

  // Boot the wasm. The bridge calls onFoxproReady once it's up;
  // attach() picks that up and starts rendering.
  FoxproRender.bootWasm('sim.wasm', statusEl);
})();
