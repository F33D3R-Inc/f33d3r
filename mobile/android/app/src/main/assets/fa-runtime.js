/*
 * fa-runtime.js — the Android client runtime for Facet Architecture.
 *
 * This is the whole of what the client is allowed to do. It maintains no state, it
 * renders nothing, and it decides nothing about the application: it puts rendered
 * fragments where their data-facet-id says they belong, and it forwards a person's
 * intent to the server so the server can decide what that intent means.
 *
 * It is not a port of the browser runtime and does not replace it. The browser has
 * HTMX and a full page shell; this surface has neither, because on Android the Shell
 * is native and the only thing inside this document is the content plane.
 *
 * Three responsibilities, and no fourth:
 *   1. insert and replace fragments by facet address
 *   2. forward intents (taps, submits) to the native side
 *   3. report the surface's own geometry so the native Shell can size it
 */
(function () {
  'use strict';

  var root = document.getElementById('fa-root');

  // ── 1. Fragment insertion and replacement ─────────────────────────────────

  function parse(html) {
    var tmp = document.createElement('div');
    tmp.innerHTML = (html || '').trim();
    return tmp;
  }

  function firstElement(html) {
    var tmp = parse(html);
    return tmp.firstElementChild;
  }

  function find(address) {
    // Attribute selectors need the address escaped: facet ids contain colons, which
    // a CSS selector would otherwise read as a pseudo-class.
    return document.querySelector('[data-facet-id="' + address.replace(/"/g, '\\"') + '"]');
  }

  var FA = {
    /** Replaces the whole content plane with a rendered facet. */
    mount: function (html) {
      root.innerHTML = html;
      askedFor = '';
      measure();
    },

    /** Adds a rendered facet to the end of the content plane. Used for paging. */
    extend: function (html) {
      var tmp = parse(html);
      while (tmp.firstChild) root.appendChild(tmp.firstChild);
      measure();
    },

    /**
     * Replaces the facet at `address` with its new render.
     *
     * The fragment is a complete snapshot of that facet, so the old element is
     * replaced outright rather than merged into — there is nothing to merge, and
     * attempting it is how a client starts diffing.
     */
    apply: function (address, html) {
      var target = find(address);
      if (!target) return false;
      var replacement = firstElement(html);
      if (!replacement) return false;
      target.replaceWith(replacement);
      measure();
      return true;
    },

    /** Appends a rendered facet into the named container, if that container is open. */
    append: function (containerId, html) {
      var container = document.getElementById(containerId);
      if (!container) return false;
      var element = firstElement(html);
      if (!element) return false;
      container.appendChild(element);
      container.scrollTop = container.scrollHeight;
      measure();
      return true;
    },

    /** Puts a held-back facet at the top of the content plane, on the reader's word. */
    prepend: function (html) {
      var tmp = parse(html);
      var nodes = [];
      while (tmp.firstChild) nodes.push(tmp.removeChild(tmp.firstChild));
      for (var i = nodes.length - 1; i >= 0; i--) root.insertBefore(nodes[i], root.firstChild);
      measure();
    },

    /** Removes the facet at `address`. */
    remove: function (address) {
      var target = find(address);
      if (!target) return false;
      target.remove();
      measure();
      return true;
    },

    /** True when the content plane currently holds the named facet. */
    holds: function (address) {
      return !!find(address);
    },

    /** Fills a sealed bubble's body with plaintext the native side decrypted. */
    reveal: function (address, plaintext) {
      var bubble = find(address);
      if (!bubble) return false;
      var slot = bubble.querySelector('.gn-sealed[data-sealed="1"]');
      if (!slot) return false;
      slot.textContent = plaintext;
      slot.removeAttribute('data-sealed');
      slot.classList.remove('gn-sealed');
      return true;
    },

    /** Marks a sealed bubble unreadable by this identity. */
    sealedFailed: function (address, message) {
      var bubble = find(address);
      if (!bubble) return false;
      var slot = bubble.querySelector('.gn-sealed[data-sealed="1"]');
      if (!slot) return false;
      slot.textContent = message;
      return true;
    },

    /** Lists the sealed bubbles on screen so the native side can open them. */
    sealedBubbles: function () {
      var out = [];
      var slots = document.querySelectorAll('.gn-sealed[data-sealed="1"]');
      for (var i = 0; i < slots.length; i++) {
        var slot = slots[i];
        var bubble = slot.closest('[data-facet-id]');
        if (!bubble) continue;
        out.push({
          address: bubble.getAttribute('data-facet-id'),
          eph: slot.getAttribute('data-eph') || '',
          sealedKey: slot.getAttribute('data-sealed-key') || '',
          sealedNonce: slot.getAttribute('data-sealed-nonce') || '',
          bodyCt: slot.getAttribute('data-body-ct') || '',
          bodyNonce: slot.getAttribute('data-body-nonce') || ''
        });
      }
      return JSON.stringify(out);
    },

    /** The cursor for the next page, as the server wrote it into the last facet. */
    nextCursor: function () {
      var marker = root.querySelector('[data-next-after]');
      return marker ? (marker.getAttribute('data-next-after') || '') : '';
    }
  };

  window.FA = FA;

  // ── 2. Intent forwarding ──────────────────────────────────────────────────

  function bridge() {
    return window.FAHost;
  }

  function valuesOf(element) {
    var values = {};
    var raw = element.getAttribute('hx-vals') || element.getAttribute('data-fa-vals');
    if (raw) {
      try {
        var parsed = JSON.parse(raw);
        for (var key in parsed) if (parsed.hasOwnProperty(key)) values[key] = String(parsed[key]);
      } catch (e) { /* a malformed hx-vals is not a reason to lose the intent */ }
    }
    return values;
  }

  function targetOf(element) {
    var target = element.getAttribute('hx-target');
    if (!target) return '';
    if (target === 'this') {
      return element.getAttribute('data-facet-id') || '';
    }
    if (target.indexOf('closest ') === 0) {
      var found = element.closest(target.slice(8).trim());
      return found ? (found.getAttribute('data-facet-id') || '') : '';
    }
    var node = document.querySelector(target);
    return node ? (node.getAttribute('data-facet-id') || node.id || '') : target;
  }

  function send(intent) {
    var host = bridge();
    if (!host) return;
    host.intent(JSON.stringify(intent));
  }

  document.addEventListener('click', function (event) {
    var element = event.target.closest('[hx-post],[hx-get],[hx-delete],[data-fa-intent],a[href]');
    if (!element) return;

    var post = element.getAttribute('hx-post');
    var get = element.getAttribute('hx-get');
    var del = element.getAttribute('hx-delete');
    var href = element.tagName === 'A' ? element.getAttribute('href') : null;

    if (post || get || del) {
      event.preventDefault();
      send({
        kind: 'lane',
        method: post ? 'POST' : (del ? 'DELETE' : 'GET'),
        path: post || get || del,
        values: valuesOf(element),
        target: targetOf(element),
        swap: element.getAttribute('hx-swap') || 'innerHTML',
        origin: element.getAttribute('data-facet-id') || ''
      });
      return;
    }

    if (href && href.charAt(0) === '/') {
      // An internal link is a request to go somewhere. Where that is belongs to the
      // native Shell's navigation, not to this document — a surface that followed it
      // itself would load a whole page shell inside the content plane.
      event.preventDefault();
      send({ kind: 'navigate', path: href });
    }
  }, true);

  document.addEventListener('submit', function (event) {
    var form = event.target;
    if (!form || form.tagName !== 'FORM') return;
    event.preventDefault();

    var values = {};
    var data = new FormData(form);
    data.forEach(function (value, key) {
      if (typeof value === 'string') values[key] = value;
    });

    var sealed = form.classList.contains('gn-composer-sealed');
    send({
      kind: sealed ? 'seal' : 'lane',
      method: (form.getAttribute('method') || 'POST').toUpperCase(),
      path: form.getAttribute('hx-post') || form.getAttribute('action') || '',
      values: values,
      convo: form.getAttribute('data-convo') || '',
      target: form.getAttribute('hx-target') ? targetOf(form) : '',
      swap: form.getAttribute('hx-swap') || 'innerHTML',
      origin: form.getAttribute('data-facet-id') || ''
    });
  }, true);

  /** Clears a composer once the server has accepted what it held. */
  FA.clearComposer = function (address) {
    var composer = find(address) || root.querySelector('form.gn-composer, form.gn-composer-sealed');
    if (!composer) return false;
    var field = composer.querySelector('[name=body]');
    if (field) field.value = '';
    return true;
  };

  // ── 3. Geometry ───────────────────────────────────────────────────────────

  var lastHeight = 0;

  /* How far from the end the next page is asked for, so it arrives before it is
     needed rather than after the reader has stopped. */
  var PAGE_AHEAD = 1200;
  var askedFor = '';

  function onScroll() {
    var host = bridge();
    if (!host) return;
    var doc = document.documentElement;
    var remaining = doc.scrollHeight - (window.scrollY + window.innerHeight);
    if (remaining > PAGE_AHEAD) return;
    var cursor = FA.nextCursor();
    // The server writes the cursor into the last facet it rendered. No cursor means
    // there is no next page, and asking twice for the same one would append it twice.
    if (!cursor || cursor === askedFor) return;
    askedFor = cursor;
    host.pageForward(cursor);
  }

  window.addEventListener('scroll', onScroll, { passive: true });

  function measure() {
    var host = bridge();
    if (!host) return;
    var height = Math.ceil(root.getBoundingClientRect().height);
    if (height !== lastHeight) {
      lastHeight = height;
      host.measured(height);
    }
  }

  if (window.ResizeObserver) {
    new ResizeObserver(measure).observe(root);
  } else {
    window.addEventListener('resize', measure);
  }

  // Tells the native side the surface is ready to receive fragments. Until this
  // fires, a mount would be applied to a document that has not finished parsing.
  document.addEventListener('DOMContentLoaded', function () {
    var host = bridge();
    if (host) host.ready();
  });
  if (document.readyState !== 'loading') {
    var host = bridge();
    if (host) host.ready();
  }
})();
