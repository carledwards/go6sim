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

  // Boot the wasm. The bridge calls onFoxproReady once it's up;
  // attach() picks that up and starts rendering.
  FoxproRender.bootWasm('sim.wasm', statusEl);
})();
