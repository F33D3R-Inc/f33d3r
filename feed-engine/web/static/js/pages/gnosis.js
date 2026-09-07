// Gnosis messaging — page UX for the plain path (scroll, active highlight, mobile
// master-detail). Sealed-mode crypto (WASM seal/open) loads as a separate module.
//
// Registered as a Shell page: the document-level listeners below are installed
// once per session; the mount (badge clear, scroll pin) runs on every arrival
// of the messages Playground.
F33D3R.page('gnosis', function () {
  function scrollMessages() {
    var list = document.getElementById('gnosis-messages');
    if (list) list.scrollTop = list.scrollHeight;
  }

  function clearDmBadge() {
    var b = document.getElementById('dm-badge');
    if (b) { b.style.display = 'none'; b.dataset.count = '0'; b.textContent = ''; }
  }

  // Landing on /messages clears the unread badge.
  clearDmBadge();
  scrollMessages();

  F33D3R.once('gnosis:listeners', function () {
    // After HTMX swaps a thread in or appends a sent bubble, keep the view pinned to
    // the latest message; opening a thread reveals the thread pane on mobile.
    document.body.addEventListener('htmx:afterSwap', function (e) {
      var t = e.target;
      if (!t) return;
      if (t.id === 'gnosis-thread' || t.id === 'gnosis-messages') {
        scrollMessages();
      }
      if (t.id === 'gnosis-thread') {
        var layout = document.getElementById('gnosis-layout');
        if (layout) layout.classList.add('gn-show-thread');
        clearDmBadge();
      }
    });

    // Highlight the open conversation; handle the mobile "back" control.
    document.body.addEventListener('click', function (e) {
      if (!e.target.closest) return;
      var convo = e.target.closest('.gn-convo');
      if (convo) {
        document.querySelectorAll('.gn-convo.gn-active').forEach(function (el) {
          el.classList.remove('gn-active');
        });
        convo.classList.add('gn-active');
        return;
      }
      if (e.target.closest('[data-gn-back]')) {
        var layout = document.getElementById('gnosis-layout');
        if (layout) layout.classList.remove('gn-show-thread');
      }
      // Empty-state "New message" — focus the start-by-handle input.
      if (e.target.closest('[data-gn-new]')) {
        var startInput = document.querySelector('.gn-new-input[name="handle"]');
        if (startInput) startInput.focus();
      }
    });

    // ── Contact requests ────────────────────────────────────────────────────────
    // Accepting or declining is recorded by the one handler that owns that decision
    // (/identity/contact/requests/decide), which renders the Contact settings surface.
    // The messaging surface does not try to reuse that fragment: it asks the server
    // for its OWN rendering of the same truth. The browser decides nothing here — it
    // re-requests, and the server answers.
    document.body.addEventListener('htmx:afterRequest', function (e) {
      var form = e.target && e.target.closest && e.target.closest('.gn-request-decide');
      if (!form || !e.detail || !e.detail.successful || !window.htmx) return;
      window.htmx.trigger(document.body, 'gnosis-contacts');
      window.htmx.trigger(document.body, 'gnosis-refresh');
    });

    // ── New-conversation user search ────────────────────────────────────────────
    // Typing in the "Message @handle…" input live-searches people. The server owns
    // the result set (/api/users/search); the browser only renders the returned rows
    // and, on selection, drives the existing hx-post that opens the conversation.
    var _gnSearchTimer = null;

    function gnDropdown() { return document.getElementById('gn-mention-dropdown'); }
    function gnHideDropdown() { var dd = gnDropdown(); if (dd) { dd.style.display = 'none'; dd.innerHTML = ''; } }

    // Build one result row with DOM nodes (no innerHTML interpolation of user text).
    function gnBuildRow(u, idx) {
      var row = document.createElement('button');
      row.type = 'button';
      row.className = 'gn-mention-row';
      row.setAttribute('role', 'option');
      row.dataset.handle = u.handle;
      row.dataset.idx = idx;

      var av;
      if (u.avatar_url) {
        av = document.createElement('img');
        av.className = 'gn-mention-av';
        av.src = u.avatar_url;
        av.alt = '';
      } else {
        av = document.createElement('span');
        av.className = 'gn-mention-av gn-mention-av-blank';
        av.textContent = (u.handle || '?').charAt(0).toUpperCase();
      }
      row.appendChild(av);

      var meta = document.createElement('span');
      meta.className = 'gn-mention-meta';
      var name = document.createElement('span');
      name.className = 'gn-mention-name';
      name.textContent = u.display_name || u.handle;
      var handle = document.createElement('span');
      handle.className = 'gn-mention-handle';
      handle.textContent = '@' + u.handle;
      meta.appendChild(name);
      meta.appendChild(handle);
      row.appendChild(meta);
      return row;
    }

    // The @handle field only, by name and not by class. There is deliberately no
    // Number field beside it: a Number given to you in confidence is not a search
    // term, and the Number affordance lives inside the conversation where a
    // message failed to land (Facet(contact_gate)), not in this pane.
    document.body.addEventListener('input', function (e) {
      if (!e.target.classList || !e.target.classList.contains('gn-new-input')) return;
      if (e.target.name !== 'handle') return;
      var input = e.target;
      var dd = gnDropdown();
      if (!dd) return;
      var q = input.value.trim().replace(/^@/, '');
      clearTimeout(_gnSearchTimer);
      if (q.length < 1) { gnHideDropdown(); return; }
      _gnSearchTimer = setTimeout(function () {
        fetch('/api/users/search?q=' + encodeURIComponent(q))
          .then(function (r) { return r.json(); })
          .then(function (users) {
            if (!Array.isArray(users) || !users.length) { gnHideDropdown(); return; }
            var myHandle = (document.body.dataset.handle || '').toLowerCase();
            dd.innerHTML = '';
            var shown = 0;
            users.forEach(function (u) {
              if (!u || !u.handle) return;
              if (myHandle && u.handle.toLowerCase() === myHandle) return;
              dd.appendChild(gnBuildRow(u, shown));
              shown++;
            });
            if (!shown) { gnHideDropdown(); return; }
            dd.style.display = 'block';
          })
          .catch(function () { gnHideDropdown(); });
      }, 180);
    });

    // Select a person → fill the input and submit, opening the conversation.
    document.body.addEventListener('click', function (e) {
      var row = e.target.closest && e.target.closest('.gn-mention-row');
      if (row) {
        var input = document.querySelector('.gn-new-input[name="handle"]');
        var form = input && input.closest('.gn-new');
        if (input && form) {
          input.value = row.dataset.handle;
          gnHideDropdown();
          if (typeof form.requestSubmit === 'function') form.requestSubmit();
          else form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
        }
        return;
      }
      // Click outside the @handle search input/dropdown closes it.
      if (!e.target.closest('.gn-new .gn-new-input-wrap')) gnHideDropdown();
    });

    // Keyboard nav within the user-search dropdown.
    document.addEventListener('keydown', function (e) {
      var dd = gnDropdown();
      if (!dd || dd.style.display === 'none') return;
      if (!document.activeElement || document.activeElement.name !== 'handle') return;
      var rows = Array.prototype.slice.call(dd.querySelectorAll('.gn-mention-row'));
      if (!rows.length) return;
      var active = dd.querySelector('.gn-mention-row.gn-mention-active');
      var idx = active ? parseInt(active.dataset.idx, 10) : -1;
      if (e.key === 'ArrowDown') { e.preventDefault(); idx = Math.min(idx + 1, rows.length - 1); }
      else if (e.key === 'ArrowUp') { e.preventDefault(); idx = Math.max(idx - 1, 0); }
      else if (e.key === 'Enter' && active) { e.preventDefault(); active.click(); return; }
      else if (e.key === 'Escape') { gnHideDropdown(); return; }
      else return;
      rows.forEach(function (r) { r.classList.remove('gn-mention-active'); });
      rows[idx].classList.add('gn-mention-active');
      rows[idx].scrollIntoView({ block: 'nearest' });
    });
  });
});
