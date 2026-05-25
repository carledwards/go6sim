// host.js — boots the Remote CPU Host wasm. Same FoxproRender shim
// the main simulator uses, just pointed at host.wasm. No quick-pick
// or simulator-specific JS — this page is just the CPU on the other
// end of the wire.
(() => {
  const canvas = document.getElementById('screen');
  const statusEl = document.getElementById('status');

  const isMobile = window.matchMedia('(max-width: 720px)').matches;
  const fontPx = isMobile ? 9 : 13;
  FoxproRender.attach(canvas, { statusEl, fontPx });

  FoxproRender.bootWasm('host.wasm', statusEl);
})();
