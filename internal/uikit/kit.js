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
  var SPRITE = __MU_SPRITE__;
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
