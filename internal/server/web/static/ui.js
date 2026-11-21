// The pages' link to muster's UI kit (docs/ui-kit.md in ZacxDev/muster):
// kit.css and kit.js are loaded LIVE from muster's origin by the page, so the
// install's theme reaches this plugin on its next load. This file is the one
// place that knows whether they arrived.
//
// 🔴 The call must work without the kit. kit.css is render-blocking and runs
// before this script, so a missing --mu-bg token here means it did not load
// (a 404, muster down, a CSP refusal): the page then links the built-in
// fallback (static/kit-fallback.css, the kit's components and its default
// theme) and draws its icons from static/kit-fallback-icons.svg. kit.js can be
// missing on its own: every behaviour the pages use goes through MeetingsUI,
// which falls back to a small built-in version of it.
//
// <html data-kit> says what happened: "live" (both from muster), "css"
// (kit.css only), "js" (kit.js only), "fallback" (neither).
(function () {
  'use strict';
  var doc = document.documentElement;
  var K = window.MusterKit || null;
  var cssLive = getComputedStyle(doc).getPropertyValue('--mu-bg').trim() !== '';
  if (!cssLive) {
    var l = document.createElement('link');
    l.rel = 'stylesheet';
    l.href = '/static/kit-fallback.css';
    l.id = 'kit-fallback';
    document.head.insertBefore(l, document.head.firstChild);
  }
  doc.setAttribute('data-kit', cssLive && K ? 'live' : cssLive ? 'css' : K ? 'js' : 'fallback');

  var SVGNS = 'http://www.w3.org/2000/svg';

  // icon is a decorative <svg class="mu-icon"> drawing the kit's icon name.
  function icon(name) {
    if (K) return K.icon(name);
    var svg = document.createElementNS(SVGNS, 'svg');
    svg.setAttribute('class', 'mu-icon');
    svg.setAttribute('aria-hidden', 'true');
    svg.setAttribute('focusable', 'false');
    var use = document.createElementNS(SVGNS, 'use');
    use.setAttribute('href', '/static/kit-fallback-icons.svg#mu-' + name);
    svg.appendChild(use);
    return svg;
  }

  // setIcon replaces el's first icon with name's (a toggle's pressed glyph).
  function setIcon(el, name) {
    if (el.getAttribute('data-icon') === name) return;
    var old = el.querySelector('svg.mu-icon');
    var n = icon(name);
    if (old) el.replaceChild(n, old); else el.insertBefore(n, el.firstChild);
    el.setAttribute('data-icon', name);
  }

  // label sets an icon button's accessible name (and so the kit's tooltip),
  // or, with no kit.js, the browser's own tooltip.
  function label(el, text, tip) {
    el.setAttribute('aria-label', text);
    if (tip) el.setAttribute('data-mu-tooltip', tip); else el.removeAttribute('data-mu-tooltip');
    if (!K) el.title = tip || text;
  }

  // iconButton is a kit icon button: one icon, an aria-label, a tooltip.
  function iconButton(name, text, cls, onClick) {
    var b = document.createElement('button');
    b.type = 'button';
    b.className = 'mu-icon-btn' + (cls ? ' ' + cls : '');
    b.appendChild(icon(name));
    b.setAttribute('data-icon', name);
    label(b, text);
    if (onClick) b.addEventListener('click', onClick);
    return b;
  }

  // decorate gives every [data-icon] element in root its icon and, with no
  // kit.js, every labelled icon button its title (the templates write the
  // name; this draws the glyph, so a page with no script at all still has
  // its text).
  function decorate(root) {
    Array.prototype.forEach.call((root || document).querySelectorAll('[data-icon]'), function (el) {
      if (!el.querySelector('svg.mu-icon')) el.insertBefore(icon(el.getAttribute('data-icon')), el.firstChild);
      if (!K && el.classList.contains('mu-icon-btn') && !el.title) el.title = el.getAttribute('data-mu-tooltip') || el.getAttribute('aria-label') || '';
    });
  }

  // ---- toasts: the kit's, or a plain one in the same classes ----
  function toast(message, opts) {
    if (K) return K.toast(message, opts);
    opts = opts || {};
    var r = document.querySelector('.mu-toast-region');
    if (!r) {
      r = document.createElement('div');
      r.className = 'mu-toast-region';
      r.setAttribute('role', 'status');
      r.setAttribute('aria-live', 'polite');
      document.body.appendChild(r);
    }
    var el = document.createElement('div');
    el.className = 'mu-toast' + (opts.kind && opts.kind !== 'info' ? ' mu-toast--' + opts.kind : '');
    var text = document.createElement('div');
    text.className = 'toast-text';
    text.textContent = String(message);
    el.appendChild(text);
    var timer = null;
    function dismiss() { if (timer) clearTimeout(timer); el.remove(); }
    if (opts.action && opts.action.label) {
      var a = document.createElement('button');
      a.type = 'button';
      a.className = 'mu-btn mu-btn--ghost mu-btn--sm';
      a.textContent = String(opts.action.label);
      a.addEventListener('click', function () { opts.action.onClick(); dismiss(); });
      el.appendChild(a);
    }
    var x = iconButton('close', 'Dismiss', 'mu-btn--sm', dismiss);
    el.appendChild(x);
    r.appendChild(el);
    var t = opts.timeout != null ? opts.timeout : opts.kind === 'error' ? 0 : 5000;
    if (t > 0) timer = setTimeout(dismiss, t);
    return dismiss;
  }

  // ---- sheets (modal): the kit's, or hidden toggling with Escape ----
  function openSheet(el, opts) {
    if (K) { K.openSheet(el, opts); return; }
    if (el._open) return;
    var before = document.activeElement;
    var back = document.createElement('div');
    back.className = 'mu-backdrop';
    el.parentNode.insertBefore(back, el);
    el.hidden = false;
    var f = el.querySelector('button, [href], input, select');
    if (f) f.focus();
    function onKey(e) { if (e.key === 'Escape') { e.preventDefault(); closeSheet(el); } }
    back.addEventListener('click', function () { closeSheet(el); });
    document.addEventListener('keydown', onKey);
    el._open = { back: back, onKey: onKey, before: before, onClose: opts && opts.onClose };
  }
  function closeSheet(el) {
    if (K) { K.closeSheet(el); return; }
    var st = el._open;
    if (!st) return;
    el._open = null;
    el.hidden = true;
    st.back.remove();
    document.removeEventListener('keydown', st.onKey);
    if (st.before && st.before.focus) st.before.focus();
    if (st.onClose) st.onClose();
  }

  // ---- popovers (non-modal): a menu or a details card by its trigger ----
  // The kit's menu opens below its trigger; a call's controls sit at the
  // bottom of the screen, so this places it above when there is no room
  // below, and keeps it on screen. Escape and a click outside close it;
  // a menu's arrow keys move between its items.
  var openPop = null;
  function closePop(refocus) {
    if (!openPop) return;
    var p = openPop;
    openPop = null;
    p.el.hidden = true;
    p.trigger.setAttribute('aria-expanded', 'false');
    if (refocus) p.trigger.focus();
  }
  function place(trigger, el) {
    var r = trigger.getBoundingClientRect();
    el.style.position = 'fixed';
    el.style.top = '0px';
    el.style.left = '0px';
    var w = el.offsetWidth, h = el.offsetHeight;
    var vw = window.innerWidth, vh = window.innerHeight;
    var top = r.bottom + 6;
    if (top + h > vh - 6) top = Math.max(6, r.top - h - 6);
    var left = Math.max(6, Math.min(r.left + r.width / 2 - w / 2, vw - w - 6));
    el.style.top = top + 'px';
    el.style.left = left + 'px';
  }
  function items(el) { return Array.prototype.slice.call(el.querySelectorAll('.mu-menu__item:not([disabled]), input, button')); }
  function popover(trigger, el) {
    trigger.setAttribute('aria-expanded', 'false');
    trigger.setAttribute('aria-controls', el.id);
    trigger.addEventListener('click', function (e) {
      e.stopPropagation();
      if (openPop && openPop.el === el) { closePop(true); return; }
      closePop(false);
      el.hidden = false;
      place(trigger, el);
      trigger.setAttribute('aria-expanded', 'true');
      openPop = { el: el, trigger: trigger };
      var f = items(el);
      if (f.length && el.getAttribute('role') === 'menu') f[0].focus();
    });
  }
  document.addEventListener('click', function (e) {
    if (!openPop) return;
    if (openPop.el.contains(e.target)) {
      if (e.target.closest && e.target.closest('.mu-menu__item')) closePop(false);
      return;
    }
    closePop(false);
  });
  document.addEventListener('keydown', function (e) {
    if (!openPop) return;
    if (e.key === 'Escape') { e.preventDefault(); closePop(true); return; }
    if (openPop.el.getAttribute('role') !== 'menu') return;
    var f = items(openPop.el), i = f.indexOf(document.activeElement);
    if (e.key === 'ArrowDown') { e.preventDefault(); f[(i + 1) % f.length].focus(); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); f[(i - 1 + f.length) % f.length].focus(); }
    else if (e.key === 'Tab') closePop(false);
  });
  window.addEventListener('resize', function () { closePop(false); });

  // ---- motion: the kit's tokens decide every duration ----
  // dur is a --mu-dur-<name> token in milliseconds: 0 under
  // prefers-reduced-motion (kit.css sets them to 0ms), so every animation
  // here is off with it, the same as the kit's own.
  function dur(name) {
    var v = getComputedStyle(doc).getPropertyValue('--mu-dur-' + name).trim();
    var n = parseFloat(v);
    if (!isFinite(n)) return 0;
    return /ms$/.test(v) ? n : n * 1000;
  }
  function ease(name) { return getComputedStyle(doc).getPropertyValue('--mu-ease-' + name).trim() || 'ease'; }

  // flip moves els with mutate() and animates each from where it was to
  // where it is (FLIP), in --mu-dur-base. With reduced motion (a 0 ms token)
  // it only moves them.
  function flip(els, mutate) {
    var d = dur('base');
    var before = d > 0 ? els.map(function (el) { return el.getBoundingClientRect(); }) : null;
    mutate();
    if (!before) return;
    var e = ease('standard');
    els.forEach(function (el, i) {
      if (!el.isConnected || !el.animate) return;
      var a = before[i], b = el.getBoundingClientRect();
      if (!a.width || !b.width) return;
      var dx = a.left - b.left, dy = a.top - b.top, sx = a.width / b.width, sy = a.height / b.height;
      if (Math.abs(dx) < 1 && Math.abs(dy) < 1 && Math.abs(sx - 1) < 0.01 && Math.abs(sy - 1) < 0.01) return;
      el.animate([
        { transformOrigin: 'top left', transform: 'translate(' + dx + 'px,' + dy + 'px) scale(' + sx + ',' + sy + ')' },
        { transformOrigin: 'top left', transform: 'none' }
      ], { duration: d, easing: e });
    });
  }

  // remove takes el out with its .leaving transition (fade and scale, in
  // tokens), then from the page; at once under reduced motion.
  function remove(el) {
    var d = dur('base');
    if (!d) { el.remove(); return; }
    el.classList.add('leaving');
    el.setAttribute('aria-hidden', 'true');
    setTimeout(function () { el.remove(); }, d);
  }

  // initials of a display name, for a tile with no picture: the first
  // letters of its first and last words ("Ada Lovelace" AL, "Grace" G).
  // Text only.
  function initials(name) {
    var parts = String(name || '').replace(/\(guest\)\s*$/, '').trim().split(/\s+/).filter(Boolean);
    if (!parts.length) return '?';
    var first = Array.from(parts[0])[0], last = parts.length > 1 ? Array.from(parts[parts.length - 1])[0] : '';
    return (first + last).toUpperCase();
  }

  window.MeetingsUI = {
    kit: K, icon: icon, setIcon: setIcon, label: label, iconButton: iconButton, decorate: decorate, toast: toast,
    openSheet: openSheet, closeSheet: closeSheet, popover: popover, closePop: closePop,
    dur: dur, flip: flip, remove: remove, initials: initials
  };
  decorate(document);
})();
