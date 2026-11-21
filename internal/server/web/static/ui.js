// The pages' link to muster's UI kit (docs/ui-kit.md in ZacxDev/muster):
// kit.css and kit.js are loaded LIVE from muster's origin by the page, so the
// install's theme reaches this plugin on its next load. This file is the one
// place that knows whether they arrived.
//
// 🔴 The call must work without the kit. The fallback is muster's own kit,
// bundled (scripts/vendor-ui-kit.sh, the same render the tests pin):
// - kit.css is render-blocking and has run before this script, so a missing
//   --mu-bg token here means it did not load (a 404, muster down, a CSP
//   refusal); this links static/kit-fallback.css, its default theme.
// - kit.js: the page loads static/vendor/muster-ui-kit-v1.js right after this
//   script, always. It is kit.js byte for byte, and kit.js installs itself
//   only when no MusterKit exists, so it is a no-op when the live one loaded.
//   That is why everything here reads window.MusterKit when called, never at
//   load.
//
// <html data-kit> is "live" (both from muster) or "fallback".
(function () {
  'use strict';
  var doc = document.documentElement;
  var cssLive = getComputedStyle(doc).getPropertyValue('--mu-bg').trim() !== '';
  if (!cssLive) {
    var l = document.createElement('link');
    l.rel = 'stylesheet';
    l.href = '/static/kit-fallback.css';
    l.id = 'kit-fallback';
    document.head.insertBefore(l, document.head.firstChild);
  }
  doc.setAttribute('data-kit', cssLive && window.MusterKit ? 'live' : 'fallback');
  function K() { return window.MusterKit; }

  // icon is a decorative <svg class="mu-icon"> drawing the kit's icon name.
  function icon(name) { return K().icon(name); }

  // setIcon replaces el's first icon with name's (a toggle's pressed glyph).
  function setIcon(el, name) {
    if (el.getAttribute('data-icon') === name && el.querySelector('svg.mu-icon')) return;
    var old = el.querySelector('svg.mu-icon');
    var n = icon(name);
    if (old) el.replaceChild(n, old); else el.insertBefore(n, el.firstChild);
    el.setAttribute('data-icon', name);
  }

  // label sets an icon button's accessible name, which the kit draws as its
  // tooltip (or tip, when given).
  function label(el, text, tip) {
    el.setAttribute('aria-label', text);
    if (tip) el.setAttribute('data-mu-tooltip', tip); else el.removeAttribute('data-mu-tooltip');
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

  // decorate gives every [data-icon] element in root its icon (the
  // templates write the name; this draws the glyph).
  function decorate(root) {
    Array.prototype.forEach.call((root || document).querySelectorAll('[data-icon]'), function (el) {
      if (!el.querySelector('svg.mu-icon')) el.insertBefore(icon(el.getAttribute('data-icon')), el.firstChild);
    });
  }

  function toast(message, opts) { return K().toast(message, opts); }

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
    icon: icon, setIcon: setIcon, label: label, iconButton: iconButton, decorate: decorate, toast: toast,
    popover: popover, closePop: closePop,
    dur: dur, flip: flip, remove: remove, initials: initials
  };
})();
