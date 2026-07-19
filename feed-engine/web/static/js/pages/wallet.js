// Server values from <meta> tags — no template injection in JS
const MY_PIAL   = document.querySelector('meta[name="f33d3r:pial"]')?.content || '';
const MY_HANDLE = document.querySelector('meta[name="f33d3r:handle"]')?.content || '';

// ── Boot ──────────────────────────────────────────────────────────────────────
document.addEventListener('DOMContentLoaded', async () => {
  if (!MY_PIAL) return;
  await Promise.all([loadBalance(), loadTxHistory(), loadProposals()]);
});

// Re-run loaders after HTMX panel swaps — tx-list and proposals-list only exist
// in their respective panels, not on initial overview load.
document.body.addEventListener('htmx:afterSettle', function() {
  if (!MY_PIAL) return;
  if (document.getElementById('tx-list')) loadTxHistory();
  if (document.getElementById('proposals-list')) loadProposals();
});

// ── Balance ───────────────────────────────────────────────────────────────────
async function loadBalance() {
  try {
    const r = await fetch('/ainsoph/v1/balance/' + encodeURIComponent(MY_PIAL));
    if (!r.ok) return;
    const d = await r.json();
    const el = document.querySelector('.pixel-balance');
    if (el && d.balance_aet) el.textContent = d.balance_aet.replace(' AET', '');
    const ns = document.getElementById('wallet-network-status');
    if (ns) { ns.textContent = 'Online'; ns.style.color = '#22c55e'; }
  } catch {}
}

// ── Transaction history ───────────────────────────────────────────────────────
async function loadTxHistory() {
  const list = document.getElementById('tx-list');
  if (!list) return;
  try {
    const r = await fetch('/ainsoph/v1/transactions/' + encodeURIComponent(MY_PIAL) + '?limit=20');
    if (!r.ok) { list.innerHTML = _txEmpty(); return; }
    const d = await r.json();
    if (!d.entries || !d.entries.length) { list.innerHTML = _txEmpty(); return; }
    list.innerHTML = d.entries.map(_txItem).join('');
  } catch { list.innerHTML = '<p class="wallet-tx-loading">Could not load transactions.</p>'; }
}

function _txEmpty() {
  return '<p class="wallet-tx-loading">No transactions yet.</p>';
}

function _txItem(tx) {
  const isIn  = tx.direction === 'in';
  const isOut = tx.direction === 'out';
  const iconClass = isIn ? 'in' : (isOut ? 'out' : 'fee');
  const amtClass  = isIn ? 'in' : (isOut ? 'out' : 'fee');
  const sign      = isIn ? '+' : (isOut ? '−' : '');
  const icon      = isIn ? '↓' : (isOut ? '↑' : '⊙');
  const label     = tx.tx_type.charAt(0).toUpperCase() + tx.tx_type.slice(1);
  const who       = tx.counterparty ? ' · @' + esc(tx.counterparty.slice(0, 20)) : '';
  const ago       = timeAgo(new Date(tx.created_at));
  return `<div class="wallet-tx-item">
    <div class="wallet-tx-icon wallet-tx-icon--${iconClass}">${icon}</div>
    <div class="wallet-tx-body">
      <p class="wallet-tx-label">${label}${who}</p>
      <p class="wallet-tx-sub">${ago}</p>
    </div>
    <span class="wallet-tx-amount wallet-tx-amount--${amtClass}">${sign}${esc(tx.net_aet)}</span>
  </div>`;
}

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
  // Reset fields
  const r = document.getElementById('wss-recipient');
  const a = document.getElementById('wss-amount');
  const res = document.getElementById('wss-result');
  if (r) r.value = '';
  if (a) a.value = '';
  if (res) { res.textContent = ''; res.style.color = ''; }
  _hideSendDropdown();
}

// Escape closes the sheet
document.addEventListener('keydown', function(e) {
  if (e.key === 'Escape') closeSendSheet();
});

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

document.addEventListener('click', function(e) {
  const dd = document.getElementById('wss-recipient-dd');
  const wrap = dd && dd.parentElement;
  if (dd && wrap && !wrap.contains(e.target)) _hideSendDropdown();
});

// ── Send AET / Tip ────────────────────────────────────────────────────────────
async function sendTip() {
  const recipientRaw = (document.getElementById('wss-recipient') || document.getElementById('tip-recipient'))?.value.replace(/^@/, '').trim();
  const amountStr    = (document.getElementById('wss-amount')    || document.getElementById('tip-amount'))?.value;
  const result       = document.getElementById('wss-result') || document.getElementById('tip-result');
  const amount       = parseFloat(amountStr);

  if (!recipientRaw) { if (result) { result.textContent = 'Enter a recipient.'; result.style.color = '#ef4444'; } return; }
  if (!amount || amount <= 0) { if (result) { result.textContent = 'Enter a valid amount.'; result.style.color = '#ef4444'; } return; }

  // Resolve handle → PIAL
  let toPial = recipientRaw;
  if (!recipientRaw.includes('-')) {
    try {
      const r = await fetch('/api/pial/by-handle/' + encodeURIComponent(recipientRaw));
      if (!r.ok) { if (result) { result.textContent = '@' + recipientRaw + ' not found.'; result.style.color = '#ef4444'; } return; }
      const j = await r.json();
      toPial = j.identity;
    } catch { if (result) { result.textContent = 'Could not resolve recipient.'; result.style.color = '#ef4444'; } return; }
  }

  if (result) { result.textContent = 'Sending…'; result.style.color = 'var(--text-secondary)'; }
  try {
    const r = await fetch('/ainsoph/v1/tip', {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({ from_pial_id: MY_PIAL, to_pial_id: toPial, amount_aet: amount })
    });
    const d = await r.json();
    if (!r.ok) {
      if (result) { result.textContent = d.message || 'Send failed.'; result.style.color = '#ef4444'; }
      return;
    }
    if (result) { result.textContent = 'Sent ' + d.net_aet + ' to @' + recipientRaw; result.style.color = '#22c55e'; }
    setTimeout(closeSendSheet, 1800);
    setTimeout(loadBalance, 800);
    setTimeout(loadTxHistory, 1000);
  } catch(e) {
    if (result) { result.textContent = 'Send failed: ' + e.message; result.style.color = '#ef4444'; }
  }
}

// ── Governance proposals ──────────────────────────────────────────────────────
async function loadProposals() {
  const list = document.getElementById('proposals-list');
  if (!list) return;
  try {
    const r = await fetch('/ainsoph/v1/proposals');
    if (!r.ok) { list.innerHTML = '<p style="font-size:12px;color:var(--text-muted);text-align:center">No proposals yet.</p>'; return; }
    const proposals = await r.json();
    if (!proposals || !proposals.length) {
      list.innerHTML = '<p style="font-size:12px;color:var(--text-muted);text-align:center;padding:8px 0">No active proposals.</p>';
      return;
    }
    list.innerHTML = proposals.map(p => {
      const isActive = p.status === 'active';
      const statusColor = isActive ? '#22c55e' : 'var(--text-muted)';
      const ends = new Date(p.ends_at);
      return `<div style="padding:12px;background:rgba(255,255,255,.03);border:1px solid var(--border);border-radius:12px;cursor:pointer;margin-bottom:8px" onclick="openProposal('${p.id}')">
        <div style="display:flex;align-items:center;gap:6px;margin-bottom:4px">
          <span style="font-size:11px;font-weight:600;color:${statusColor}">● ${p.status.toUpperCase()}</span>
          <span style="font-size:10px;color:var(--text-muted)">· ${timeAgo(ends)} ${isActive ? 'remaining' : ''}</span>
        </div>
        <p style="font-size:13px;font-weight:500;color:var(--text-primary);margin:0 0 4px">${esc(p.title)}</p>
        <p style="font-size:11px;color:var(--text-secondary);margin:0">${p.total_votes} vote${p.total_votes===1?'':'s'}</p>
      </div>`;
    }).join('');
  } catch { list.innerHTML = '<p style="font-size:12px;color:var(--text-muted);text-align:center">Could not load proposals.</p>'; }
}

async function openProposal(id) {
  const r = await fetch('/ainsoph/v1/proposals/' + id);
  if (!r.ok) return;
  const d = await r.json();
  const p = d.proposal;
  const opts = d.results.map(o => {
    const pct = Math.round(o.pct);
    const isLeading = d.leading === o.option;
    return `<div style="margin-bottom:8px">
      <div style="display:flex;justify-content:space-between;margin-bottom:3px">
        <span style="font-size:12px;font-weight:${isLeading?'600':'400'};color:${isLeading?'var(--text-primary)':'var(--text-secondary)'}">${esc(o.option)}</span>
        <span style="font-size:11px;color:var(--text-muted)">${pct}% · ${o.vote_count} vote${o.vote_count===1?'':'s'}</span>
      </div>
      <div style="height:4px;background:rgba(255,255,255,.06);border-radius:4px;overflow:hidden">
        <div style="height:100%;width:${pct}%;background:${isLeading?'var(--accent)':'rgba(255,255,255,.2)'};border-radius:4px;transition:width 400ms"></div>
      </div>
      <button onclick="castVote('${p.id}','${o.option}')" style="margin-top:6px;font-size:11px;padding:4px 12px;background:${isLeading?'var(--accent)':'transparent'};color:${isLeading?'var(--surface)':'var(--accent)'};border:1px solid var(--accent);border-radius:20px;cursor:pointer;font-family:'DM Sans',sans-serif">Vote ${esc(o.option)}</button>
    </div>`;
  }).join('');
  const modal = document.createElement('div');
  modal.style.cssText = 'position:fixed;inset:0;z-index:500;background:rgba(0,0,0,.6);backdrop-filter:blur(8px);display:flex;align-items:center;justify-content:center;padding:16px';
  modal.onclick = e => { if(e.target===modal) modal.remove(); };
  modal.innerHTML = `<div style="background:var(--panel);border:1px solid var(--border);border-radius:20px;padding:24px;width:380px;max-width:calc(100vw - 32px);max-height:80vh;overflow-y:auto">
    <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:16px">
      <h2 style="font-size:15px;font-weight:600;color:var(--text-primary)">${esc(p.title)}</h2>
      <button onclick="this.closest('[style*=position]').remove()" style="width:28px;height:28px;border:none;background:var(--border);border-radius:6px;cursor:pointer;color:var(--text-secondary)">✕</button>
    </div>
    ${p.description ? `<p style="font-size:13px;color:var(--text-secondary);margin-bottom:14px">${esc(p.description)}</p>` : ''}
    <div id="vote-opts-${p.id}">${opts}</div>
    <p style="font-size:10px;color:var(--text-muted);margin-top:12px">Weight model: ${p.weight_model} · ${p.total_votes} votes cast</p>
  </div>`;
  document.body.appendChild(modal);
}

async function castVote(proposalId, option) {
  const r = await fetch('/ainsoph/v1/proposals/' + proposalId + '/vote', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({ voter_id: MY_PIAL, option })
  });
  const d = await r.json();
  if (!r.ok) { toast(d.message || 'Vote failed', 'error'); return; }
  toast('Vote cast: ' + option);
  document.querySelector('[style*="position:fixed"][style*="inset:0"]:last-child')?.remove();
  loadProposals();
}

function toggleNewProposal() {
  const f = document.getElementById('proposal-form');
  if (f) f.style.display = f.style.display === 'none' ? '' : 'none';
}

async function submitProposal() {
  const title   = document.getElementById('prop-title').value.trim();
  const desc    = document.getElementById('prop-desc').value.trim();
  const weight  = document.getElementById('prop-weight').value;
  const endsVal = document.getElementById('prop-ends').value;
  if (!title)   { toast('Title is required', 'error'); return; }
  if (!endsVal) { toast('End date required', 'error'); return; }
  const r = await fetch('/ainsoph/v1/proposals', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({ creator_id: MY_PIAL, title, description: desc, weight_model: weight, ends_at: new Date(endsVal).toISOString() })
  });
  if (!r.ok) { const d = await r.json(); toast(d.message || 'Failed', 'error'); return; }
  toast('Proposal created');
  toggleNewProposal();
  loadProposals();
}

// ── Helpers ───────────────────────────────────────────────────────────────────
function esc(s) { return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;'); }
function timeAgo(d) {
  const s = Math.floor((Date.now() - new Date(d)) / 1000);
  if (Math.abs(s) < 60) return Math.abs(s) < 5 ? 'just now' : Math.abs(s) + 's';
  if (Math.abs(s) < 3600) return Math.floor(Math.abs(s)/60) + 'm';
  if (Math.abs(s) < 86400) return Math.floor(Math.abs(s)/3600) + 'h';
  return Math.floor(Math.abs(s)/86400) + 'd';
}
