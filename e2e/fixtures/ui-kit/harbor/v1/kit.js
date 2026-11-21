/* muster UI kit v1 — docs/ui-kit.md#kitjs. Additive only: a member here is a
   promise to every plugin that calls it. */
(function () {
  'use strict';
  if (window.MusterKit) { return; }

  var VERSION = '1.0.0';

  function onReady(fn) {
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', fn, { once: true });
    } else {
      fn();
    }
  }

  // --- the icon sprite ----------------------------------------------------
  // A browser will not draw <use href> from another origin, so the sprite
  // travels INSIDE this script (internal/uikit fills SPRITE from icons.go when
  // it serves kit.js) and is put in the page once. No request, no CORS: any
  // page that can load this script draws the icons.
  var SPRITE = "\u003csvg xmlns=\"http://www.w3.org/2000/svg\" aria-hidden=\"true\" hidden=\"hidden\" width=\"0\" height=\"0\" focusable=\"false\"\u003e\n\u003csymbol id=\"mu-camera\" viewBox=\"0 0 24 24\"\u003e\u003crect x=\"2\" y=\"6\" width=\"14\" height=\"12\" rx=\"2\"/\u003e\u003cpath d=\"m16 10 6-3.5v11L16 14z\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-camera-off\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M10.7 6H14a2 2 0 0 1 2 2v3.3l1 1 5-3.3v10M16 16v0a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h2M3 3l18 18\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-captions\" viewBox=\"0 0 24 24\"\u003e\u003crect x=\"2\" y=\"5\" width=\"20\" height=\"14\" rx=\"2\"/\u003e\u003cpath d=\"M10 10.5a2 2 0 1 0 0 3M17 10.5a2 2 0 1 0 0 3\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-chat\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M4 5h16a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1h-9l-5 4v-4H4a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1z\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-check\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"m4 12.5 5 5L20 6.5\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-chevron-down\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"m6 9 6 6 6-6\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-chevron-left\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"m15 6-6 6 6 6\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-chevron-right\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"m9 6 6 6-6 6\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-chevron-up\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"m6 15 6-6 6 6\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-close\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M6 6l12 12M18 6 6 18\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-copy\" viewBox=\"0 0 24 24\"\u003e\u003crect x=\"9\" y=\"9\" width=\"12\" height=\"12\" rx=\"2\"/\u003e\u003cpath d=\"M5 15H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1h10a1 1 0 0 1 1 1v1\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-error\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M7.9 2h8.2L22 7.9v8.2L16.1 22H7.9L2 16.1V7.9z\"/\u003e\u003cpath d=\"m15 9-6 6M9 9l6 6\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-external\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-fullscreen\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M3 8V3h5M16 3h5v5M21 16v5h-5M8 21H3v-5\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-fullscreen-exit\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M8 3v5H3M21 8h-5V3M16 21v-5h5M3 16h5v5\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-hand\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M18 11V6a2 2 0 0 0-4 0M14 10V4a2 2 0 0 0-4 0v6M10 10.5V6a2 2 0 0 0-4 0v8M18 8a2 2 0 0 1 4 0v6a8 8 0 0 1-8 8h-2c-2.8 0-4.5-.9-6-2.4l-3.6-3.6a2 2 0 0 1 2.8-2.8L6 14\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-info\" viewBox=\"0 0 24 24\"\u003e\u003ccircle cx=\"12\" cy=\"12\" r=\"10\"/\u003e\u003cpath d=\"M12 16v-4M12 8h.01\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-leave\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M3.6 14.6c-.8-.8-.8-2.1.1-2.8C5.9 9.9 8.8 9 12 9s6.1.9 8.3 2.8c.9.7.9 2 .1 2.8l-1.3 1.3a1.7 1.7 0 0 1-2.2.2l-1.7-1.2a1.7 1.7 0 0 1-.7-1.4v-1.4a10 10 0 0 0-5 0v1.4c0 .6-.3 1.1-.7 1.4l-1.7 1.2a1.7 1.7 0 0 1-2.2-.2z\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-link\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M10 14a5 5 0 0 0 7.1 0l3-3a5 5 0 0 0-7.1-7.1l-1.7 1.7M14 10a5 5 0 0 0-7.1 0l-3 3a5 5 0 0 0 7.1 7.1l1.7-1.7\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-mic\" viewBox=\"0 0 24 24\"\u003e\u003crect x=\"9\" y=\"3\" width=\"6\" height=\"11\" rx=\"3\"/\u003e\u003cpath d=\"M5 11a7 7 0 0 0 14 0M12 18v3M8 21h8\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-mic-off\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M15 9.3V6a3 3 0 0 0-5.7-1.3M9 9v2a3 3 0 0 0 5.1 2.1M19 11a7 7 0 0 1-1.2 3.9M5 11a7 7 0 0 0 11.1 5.7M12 18v3M8 21h8M3 3l18 18\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-minus\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M5 12h14\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-more\" viewBox=\"0 0 24 24\"\u003e\u003ccircle cx=\"5\" cy=\"12\" r=\"1.5\" fill=\"currentColor\"/\u003e\u003ccircle cx=\"12\" cy=\"12\" r=\"1.5\" fill=\"currentColor\"/\u003e\u003ccircle cx=\"19\" cy=\"12\" r=\"1.5\" fill=\"currentColor\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-more-vertical\" viewBox=\"0 0 24 24\"\u003e\u003ccircle cx=\"12\" cy=\"5\" r=\"1.5\" fill=\"currentColor\"/\u003e\u003ccircle cx=\"12\" cy=\"12\" r=\"1.5\" fill=\"currentColor\"/\u003e\u003ccircle cx=\"12\" cy=\"19\" r=\"1.5\" fill=\"currentColor\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-people\" viewBox=\"0 0 24 24\"\u003e\u003ccircle cx=\"9\" cy=\"8\" r=\"3.5\"/\u003e\u003cpath d=\"M2 20a7 7 0 0 1 14 0M16 4.5a3.5 3.5 0 0 1 0 7M18 13.5a7 7 0 0 1 4 6.5\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-phone\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M5 3h3l1.5 4.5-2 1.2a11 11 0 0 0 5 5l1.2-2L18 13v3a2 2 0 0 1-2 2A13 13 0 0 1 3 5a2 2 0 0 1 2-2z\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-pin\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M12 17v5M9 3h6l-1 6 3 3v2H7v-2l3-3z\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-plus\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M12 5v14M5 12h14\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-record\" viewBox=\"0 0 24 24\"\u003e\u003ccircle cx=\"12\" cy=\"12\" r=\"7\" fill=\"currentColor\" stroke=\"none\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-screen-share\" viewBox=\"0 0 24 24\"\u003e\u003crect x=\"2\" y=\"3\" width=\"20\" height=\"14\" rx=\"2\"/\u003e\u003cpath d=\"M8 21h8M12 17v4M9 10l3-3 3 3M12 7v6\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-screen-share-stop\" viewBox=\"0 0 24 24\"\u003e\u003crect x=\"2\" y=\"3\" width=\"20\" height=\"14\" rx=\"2\"/\u003e\u003cpath d=\"M8 21h8M12 17v4M9.5 7.5l5 5M14.5 7.5l-5 5\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-search\" viewBox=\"0 0 24 24\"\u003e\u003ccircle cx=\"11\" cy=\"11\" r=\"7\"/\u003e\u003cpath d=\"m21 21-4.3-4.3\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-send\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M22 2 11 13M22 2l-7 20-4-9-9-4z\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-settings\" viewBox=\"0 0 24 24\"\u003e\u003ccircle cx=\"12\" cy=\"12\" r=\"3\"/\u003e\u003cpath d=\"M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-share\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M4 12v7a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-7M16 6l-4-4-4 4M12 2v13\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-signal-0\" viewBox=\"0 0 24 24\"\u003e\u003crect x=\"3\" y=\"17\" width=\"3\" height=\"4\" rx=\".5\" fill=\"none\"/\u003e\u003crect x=\"8\" y=\"12\" width=\"3\" height=\"9\" rx=\".5\" fill=\"none\"/\u003e\u003crect x=\"13\" y=\"7\" width=\"3\" height=\"14\" rx=\".5\" fill=\"none\"/\u003e\u003crect x=\"18\" y=\"2\" width=\"3\" height=\"19\" rx=\".5\" fill=\"none\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-signal-1\" viewBox=\"0 0 24 24\"\u003e\u003crect x=\"3\" y=\"17\" width=\"3\" height=\"4\" rx=\".5\" fill=\"currentColor\"/\u003e\u003crect x=\"8\" y=\"12\" width=\"3\" height=\"9\" rx=\".5\" fill=\"none\"/\u003e\u003crect x=\"13\" y=\"7\" width=\"3\" height=\"14\" rx=\".5\" fill=\"none\"/\u003e\u003crect x=\"18\" y=\"2\" width=\"3\" height=\"19\" rx=\".5\" fill=\"none\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-signal-2\" viewBox=\"0 0 24 24\"\u003e\u003crect x=\"3\" y=\"17\" width=\"3\" height=\"4\" rx=\".5\" fill=\"currentColor\"/\u003e\u003crect x=\"8\" y=\"12\" width=\"3\" height=\"9\" rx=\".5\" fill=\"currentColor\"/\u003e\u003crect x=\"13\" y=\"7\" width=\"3\" height=\"14\" rx=\".5\" fill=\"none\"/\u003e\u003crect x=\"18\" y=\"2\" width=\"3\" height=\"19\" rx=\".5\" fill=\"none\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-signal-3\" viewBox=\"0 0 24 24\"\u003e\u003crect x=\"3\" y=\"17\" width=\"3\" height=\"4\" rx=\".5\" fill=\"currentColor\"/\u003e\u003crect x=\"8\" y=\"12\" width=\"3\" height=\"9\" rx=\".5\" fill=\"currentColor\"/\u003e\u003crect x=\"13\" y=\"7\" width=\"3\" height=\"14\" rx=\".5\" fill=\"currentColor\"/\u003e\u003crect x=\"18\" y=\"2\" width=\"3\" height=\"19\" rx=\".5\" fill=\"none\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-signal-4\" viewBox=\"0 0 24 24\"\u003e\u003crect x=\"3\" y=\"17\" width=\"3\" height=\"4\" rx=\".5\" fill=\"currentColor\"/\u003e\u003crect x=\"8\" y=\"12\" width=\"3\" height=\"9\" rx=\".5\" fill=\"currentColor\"/\u003e\u003crect x=\"13\" y=\"7\" width=\"3\" height=\"14\" rx=\".5\" fill=\"currentColor\"/\u003e\u003crect x=\"18\" y=\"2\" width=\"3\" height=\"19\" rx=\".5\" fill=\"currentColor\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-stop\" viewBox=\"0 0 24 24\"\u003e\u003crect x=\"6\" y=\"6\" width=\"12\" height=\"12\" rx=\"1.5\" fill=\"currentColor\" stroke=\"none\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-transcript\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M6 2h9l5 5v13a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2z\"/\u003e\u003cpath d=\"M14 2v6h6M8 13h8M8 17h6\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-volume\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M11 5 6 9H2v6h4l5 4zM15.5 8.5a5 5 0 0 1 0 7M19 5a10 10 0 0 1 0 14\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-volume-off\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M11 5 6 9H2v6h4l5 4zM22 9l-6 6M16 9l6 6\"/\u003e\u003c/symbol\u003e\n\u003csymbol id=\"mu-warning\" viewBox=\"0 0 24 24\"\u003e\u003cpath d=\"M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0zM12 9v4M12 17h.01\"/\u003e\u003c/symbol\u003e\n\u003c/svg\u003e\n";
  var ready = new Promise(function (resolve) {
    onReady(function () {
      if (!document.querySelector('svg[data-mu-sprite]')) {
        var svg = new DOMParser().parseFromString(SPRITE, 'image/svg+xml').documentElement;
        if (svg && svg.nodeName.toLowerCase() === 'svg') {
          var node = document.importNode(svg, true);
          node.setAttribute('data-mu-sprite', '');
          document.body.insertBefore(node, document.body.firstChild);
        }
      }
      resolve();
    });
  });

  var SVGNS = 'http://www.w3.org/2000/svg';
  function icon(name) {
    var svg = document.createElementNS(SVGNS, 'svg');
    svg.setAttribute('class', 'mu-icon');
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('focusable', 'false');
    var use = document.createElementNS(SVGNS, 'use');
    use.setAttribute('href', '#mu-' + name);
    svg.appendChild(use);
    return svg;
  }

  // --- tooltips -----------------------------------------------------------
  var TIP_SELECTOR = '.mu-icon-btn, [data-mu-tooltip]';
  var tipEl = null;
  var tipFor = null;

  function tipText(el) {
    if (el._muTip != null) { return el._muTip; }
    return el.getAttribute('data-mu-tooltip') || el.getAttribute('aria-label') || '';
  }

  function hideTip() {
    if (tipEl) { tipEl.remove(); }
    if (tipFor) { tipFor.removeAttribute('aria-describedby'); }
    tipEl = null;
    tipFor = null;
  }

  function showTip(el) {
    var text = tipText(el);
    if (!text) { return; }
    hideTip();
    tipEl = document.createElement('div');
    tipEl.className = 'mu-tooltip';
    tipEl.id = 'mu-tooltip';
    tipEl.setAttribute('role', 'tooltip');
    tipEl.textContent = text;
    document.body.appendChild(tipEl);
    tipFor = el;
    // A description, not the name: an aria-label stays the accessible name.
    if (el._muTip != null || el.hasAttribute('data-mu-tooltip')) { el.setAttribute('aria-describedby', 'mu-tooltip'); }
    var r = el.getBoundingClientRect();
    var t = tipEl.getBoundingClientRect();
    var top = r.top - t.height - 8;
    if (top < 4) { top = r.bottom + 8; }
    var left = Math.max(4, Math.min(window.innerWidth - t.width - 4, r.left + r.width / 2 - t.width / 2));
    tipEl.style.top = top + 'px';
    tipEl.style.left = left + 'px';
  }

  function tipTarget(node) {
    if (!node || !node.closest) { return null; }
    var el = node.closest(TIP_SELECTOR + ', [data-mu-tip-bound]');
    return el && !el.hasAttribute('data-mu-tip-off') ? el : null;
  }

  function tooltip(el, text) {
    if (text != null) { el._muTip = String(text); }
    el.setAttribute('data-mu-tip-bound', '');
    el.removeAttribute('data-mu-tip-off');
    return function () {
      if (tipFor === el) { hideTip(); }
      delete el._muTip;
      el.removeAttribute('data-mu-tip-bound');
      el.setAttribute('data-mu-tip-off', '');
    };
  }

  document.addEventListener('pointerover', function (e) { var el = tipTarget(e.target); if (el && el !== tipFor) { showTip(el); } });
  document.addEventListener('pointerout', function (e) { if (tipFor && !tipFor.contains(e.relatedTarget)) { hideTip(); } });
  document.addEventListener('focusin', function (e) { var el = tipTarget(e.target); if (el && el.matches(':focus-visible')) { showTip(el); } });
  document.addEventListener('focusout', function () { hideTip(); });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape') { hideTip(); } });

  // --- toggles ------------------------------------------------------------
  function toggle(el, pressed) {
    var next = pressed == null ? el.getAttribute('aria-pressed') !== 'true' : !!pressed;
    el.setAttribute('aria-pressed', next ? 'true' : 'false');
    el.dispatchEvent(new CustomEvent('mu-toggle', { bubbles: true, detail: { pressed: next } }));
    return next;
  }

  // --- focus trap ---------------------------------------------------------
  var FOCUSABLE = 'a[href], area[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"]), [contenteditable="true"]';

  function focusables(container) {
    return Array.prototype.filter.call(container.querySelectorAll(FOCUSABLE), function (n) {
      return !n.hidden && n.getClientRects().length > 0;
    });
  }

  function trapFocus(container) {
    var before = document.activeElement;
    function onKey(e) {
      if (e.key !== 'Tab') { return; }
      var f = focusables(container);
      if (!f.length) { e.preventDefault(); container.focus(); return; }
      var first = f[0];
      var last = f[f.length - 1];
      if (e.shiftKey && (document.activeElement === first || !container.contains(document.activeElement))) {
        e.preventDefault(); last.focus();
      } else if (!e.shiftKey && (document.activeElement === last || !container.contains(document.activeElement))) {
        e.preventDefault(); first.focus();
      }
    }
    document.addEventListener('keydown', onKey, true);
    var f = focusables(container);
    if (f.length) { f[0].focus(); } else { if (!container.hasAttribute('tabindex')) { container.setAttribute('tabindex', '-1'); } container.focus(); }
    return function release() {
      document.removeEventListener('keydown', onKey, true);
      if (before && before.focus && document.contains(before)) { before.focus(); }
    };
  }

  // --- sheets -------------------------------------------------------------
  function openSheet(el, opts) {
    if (!el || el._muOpen) { return; }
    var backdrop = document.createElement('div');
    backdrop.className = 'mu-backdrop';
    el.parentNode.insertBefore(backdrop, el);
    el.hidden = false;
    var release = trapFocus(el);
    function onKey(e) { if (e.key === 'Escape') { e.preventDefault(); closeSheet(el); } }
    backdrop.addEventListener('click', function () { closeSheet(el); });
    document.addEventListener('keydown', onKey);
    el._muOpen = { backdrop: backdrop, release: release, onKey: onKey, onClose: opts && opts.onClose };
  }

  function closeSheet(el) {
    var st = el && el._muOpen;
    if (!st) { return; }
    el._muOpen = null;
    el.hidden = true;
    st.backdrop.remove();
    document.removeEventListener('keydown', st.onKey);
    st.release();
    if (typeof st.onClose === 'function') { st.onClose(); }
  }

  // --- menus --------------------------------------------------------------
  var openMenu = null;

  function menuItems(menu) { return Array.prototype.slice.call(menu.querySelectorAll('.mu-menu__item:not([disabled])')); }

  function closeMenu(refocus) {
    if (!openMenu) { return; }
    var m = openMenu;
    openMenu = null;
    m.menu.hidden = true;
    m.trigger.setAttribute('aria-expanded', 'false');
    if (refocus) { m.trigger.focus(); }
  }

  function placeMenu(trigger, menu) {
    var r = trigger.getBoundingClientRect();
    var parent = menu.offsetParent || document.body;
    var p = parent.getBoundingClientRect();
    menu.style.top = (r.bottom - p.top + 4) + 'px';
    var left = r.left - p.left;
    menu.style.left = Math.max(0, Math.min(left, p.width - menu.offsetWidth)) + 'px';
  }

  function openMenuFor(trigger) {
    var menu = document.getElementById(trigger.getAttribute('data-mu-menu'));
    if (!menu) { return; }
    if (openMenu && openMenu.menu === menu) { closeMenu(true); return; }
    closeMenu(false);
    menu.hidden = false;
    placeMenu(trigger, menu);
    trigger.setAttribute('aria-expanded', 'true');
    openMenu = { trigger: trigger, menu: menu };
    var items = menuItems(menu);
    if (items.length) { items[0].focus(); }
  }

  document.addEventListener('keydown', function (e) {
    if (!openMenu) { return; }
    var items = menuItems(openMenu.menu);
    var i = items.indexOf(document.activeElement);
    if (e.key === 'Escape') { e.preventDefault(); closeMenu(true); }
    else if (e.key === 'ArrowDown') { e.preventDefault(); items[(i + 1) % items.length].focus(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); items[(i - 1 + items.length) % items.length].focus(); }
    else if (e.key === 'Home') { e.preventDefault(); items[0].focus(); }
    else if (e.key === 'End') { e.preventDefault(); items[items.length - 1].focus(); }
    else if (e.key === 'Tab') { closeMenu(false); }
  });

  // --- toasts -------------------------------------------------------------
  var KIND_ICON = { info: 'info', success: 'check', warning: 'warning', error: 'error' };

  function region() {
    var r = document.querySelector('.mu-toast-region');
    if (!r) {
      r = document.createElement('div');
      r.className = 'mu-toast-region';
      r.setAttribute('aria-live', 'polite');
      r.setAttribute('role', 'status');
      document.body.appendChild(r);
    }
    return r;
  }

  function toast(message, opts) {
    opts = opts || {};
    var kind = KIND_ICON[opts.kind] ? opts.kind : 'info';
    var timeout = opts.timeout != null ? opts.timeout : (kind === 'error' ? 0 : 5000);
    var el = document.createElement('div');
    el.className = 'mu-toast' + (kind === 'info' ? '' : ' mu-toast--' + kind);
    if (kind === 'error') { el.setAttribute('role', 'alert'); }
    el.appendChild(icon(KIND_ICON[kind]));
    var text = document.createElement('div');
    text.style.flex = '1 1 auto';
    text.textContent = String(message);
    el.appendChild(text);
    var timer = null;
    function dismiss() {
      if (timer) { clearTimeout(timer); timer = null; }
      el.remove();
    }
    if (opts.action && opts.action.label) {
      var a = document.createElement('button');
      a.type = 'button';
      a.className = 'mu-btn mu-btn--ghost mu-btn--sm';
      a.textContent = String(opts.action.label);
      a.addEventListener('click', function () { if (typeof opts.action.onClick === 'function') { opts.action.onClick(); } dismiss(); });
      el.appendChild(a);
    }
    var close = document.createElement('button');
    close.type = 'button';
    close.className = 'mu-btn mu-btn--ghost mu-btn--sm';
    close.setAttribute('aria-label', 'Dismiss');
    close.appendChild(icon('close'));
    close.addEventListener('click', dismiss);
    el.appendChild(close);
    region().appendChild(el);
    if (timeout > 0) { timer = setTimeout(dismiss, timeout); }
    return dismiss;
  }

  // --- wiring -------------------------------------------------------------
  function scan(root) {
    root = root || document;
    Array.prototype.forEach.call(root.querySelectorAll('.mu-icon-btn'), function (b) {
      if (!b.getAttribute('aria-label') && !b.getAttribute('aria-labelledby')) {
        b.setAttribute('data-mu-unlabelled', '');
        if (window.console) { console.warn('muster kit: a .mu-icon-btn needs an aria-label', b); }
      }
    });
    Array.prototype.forEach.call(root.querySelectorAll('[data-mu-menu]'), function (t) {
      t.setAttribute('aria-haspopup', 'menu');
      t.setAttribute('aria-controls', t.getAttribute('data-mu-menu'));
      if (!t.hasAttribute('aria-expanded')) { t.setAttribute('aria-expanded', 'false'); }
    });
    Array.prototype.forEach.call(root.querySelectorAll('[data-mu-sheet]'), function (t) {
      t.setAttribute('aria-haspopup', 'dialog');
      t.setAttribute('aria-controls', t.getAttribute('data-mu-sheet'));
    });
  }

  document.addEventListener('click', function (e) {
    var t = e.target && e.target.closest ? e.target : null;
    if (!t) { return; }
    var tog = t.closest('[data-mu-toggle]');
    if (tog && !tog.disabled && tog.getAttribute('aria-disabled') !== 'true') { toggle(tog); }
    var mt = t.closest('[data-mu-menu]');
    if (mt) { e.preventDefault(); openMenuFor(mt); return; }
    if (openMenu) {
      if (t.closest('.mu-menu__item') && openMenu.menu.contains(t)) { closeMenu(true); }
      else if (!openMenu.menu.contains(t)) { closeMenu(false); }
    }
    var st = t.closest('[data-mu-sheet]');
    if (st) { e.preventDefault(); openSheet(document.getElementById(st.getAttribute('data-mu-sheet'))); return; }
    var close = t.closest('[data-mu-close]');
    var sheet = close && close.closest('.mu-sheet');
    if (sheet) { closeSheet(sheet); }
  });

  onReady(function () { scan(document); });

  window.MusterKit = Object.freeze({
    version: VERSION,
    ready: ready,
    icon: icon,
    tooltip: tooltip,
    toggle: toggle,
    trapFocus: trapFocus,
    openSheet: openSheet,
    closeSheet: closeSheet,
    toast: toast,
    scan: scan
  });
})();
