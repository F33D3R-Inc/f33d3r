// wallet.js — the send sheet's open/close and the recipient autocomplete.
// Registered as a Shell page: executed once per session, the mount runs on
// every arrival of the wallet Playground. Document-level listeners are
// installed once.
//
// Everything the wallet shows is rendered by the server: the balance in
// wallet_hero, the history in wallet_tx_rows, governance in governance_proposals
// and governance_proposal_detail, and a send is a tip event answered with
// tip_result. The page never names an identity: the server acts as the session.
F33D3R.page('wallet', function () {

  // ── Send sheet ────────────────────────────────────────────────────────────────
  function openSendSheet() {
    const sheet = document.getElementById('wallet-send-sheet');
    if (!sheet) return;
    sheet.classList.remove('hidden');
    document.body.style.overflow = 'hidden';
    // Close on backdrop click
    sheet.onclick = function(e) { if (e.target === sheet) closeSendSheet(); };
    setTimeout(function() {
      var inp = document.getElementById('wss-recipient');
      if (inp) inp.focus();
    }, 80);
  }

  function closeSendSheet() {
    const sheet = document.getElementById('wallet-send-sheet');
    if (!sheet) return;
    sheet.classList.add('hidden');
    document.body.style.overflow = '';
    const form = sheet.querySelector('form');
    if (form) form.reset();
    const res = document.getElementById('wss-result');
    if (res) res.innerHTML = '';
    _hideSendDropdown();
  }

  // ── Recipient autocomplete ────────────────────────────────────────────────────
  var _sendSearchTimer = null;

  function onSendRecipientInput(val) {
    clearTimeout(_sendSearchTimer);
    val = val.replace(/^@/, '').trim();
    if (val.length < 1) { _hideSendDropdown(); return; }
    _sendSearchTimer = setTimeout(function() { _doHandleSearch(val); }, 220);
  }

  async function _doHandleSearch(q) {
    const dd = document.getElementById('wss-recipient-dd');
    if (!dd) return;
    try {
      const r = await fetch('/api/users/search?q=' + encodeURIComponent(q));
      if (!r.ok) { _hideSendDropdown(); return; }
      const users = await r.json();
      if (!users || !users.length) { _hideSendDropdown(); return; }
      dd.innerHTML = users.slice(0, 6).map(function(u) {
        return `<div class="wss-dd-item" onclick="selectSendRecipient('${esc(u.handle)}')">
          <div>
            <p class="wss-dd-handle">@${esc(u.handle)}</p>
            ${u.display_name ? `<p class="wss-dd-name">${esc(u.display_name)}</p>` : ''}
          </div>
        </div>`;
      }).join('');
      dd.style.display = '';
    } catch { _hideSendDropdown(); }
  }

  function selectSendRecipient(handle) {
    const inp = document.getElementById('wss-recipient');
    if (inp) inp.value = handle;
    _hideSendDropdown();
    const amt = document.getElementById('wss-amount');
    if (amt) amt.focus();
  }

  function _hideSendDropdown() {
    const dd = document.getElementById('wss-recipient-dd');
    if (dd) dd.style.display = 'none';
  }

  // ── Helpers ───────────────────────────────────────────────────────────────────
  function esc(s) { return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }

  window.openSendSheet        = openSendSheet;
  window.closeSendSheet       = closeSendSheet;
  window.onSendRecipientInput = onSendRecipientInput;
  window.selectSendRecipient  = selectSendRecipient;

  // ── Document-level listeners — installed once ─────────────────────────────────
  F33D3R.once('wallet:listeners', function () {
    // Escape closes the sheet
    document.addEventListener('keydown', function(e) {
      if (e.key === 'Escape' && window.closeSendSheet) window.closeSendSheet();
    });
    document.addEventListener('click', function(e) {
      const dd = document.getElementById('wss-recipient-dd');
      const wrap = dd && dd.parentElement;
      if (dd && wrap && !wrap.contains(e.target)) dd.style.display = 'none';
    });
  });

  return function teardown() {
    document.body.style.overflow = '';
  };
});
