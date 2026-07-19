'use strict';

// Module-level PIAL store so marketplaceOpenShop can re-init without a meta tag.
var _sellerPialId = '';

// Malkuth signing + ECDH keys are initialised globally in malkuth.js on every
// page load — no per-page init needed here. PIAL carries all keys at login.
document.addEventListener('DOMContentLoaded', function () {
  // SSE: marketplace_purchase_delivered — remove pending modal, refresh library
  document.addEventListener('sse:marketplace_purchase_delivered', function (e) {
    var data;
    try { data = JSON.parse(e.detail); } catch(_) { return; }
    // Remove any open purchase modal for this purchase
    var modal = document.querySelector('[data-purchase-id="' + data.purchase_id + '"]');
    if (modal) modal.remove();
    // Refresh buyer library if on that page
    var library = document.getElementById('buyer-library');
    if (library) htmx.trigger(library, 'refresh');
  });

  // Start expiry timers for any open purchase modals already in the DOM
  document.querySelectorAll('.market-timer[data-expires]').forEach(function (el) {
    startPaymentTimer(el);
  });

  // Also start timers + polling after HTMX swaps (e.g. when purchase modal is injected)
  document.addEventListener('htmx:afterSwap', function () {
    document.querySelectorAll('.market-timer[data-expires]:not([data-timer-started])').forEach(function (el) {
      el.dataset.timerStarted = 'true';
      startPaymentTimer(el);
    });
    document.querySelectorAll('.marketplace-purchase-modal[data-purchase-id]:not([data-polling])').forEach(function (modal) {
      modal.dataset.polling = 'true';
      startPurchasePolling(modal.dataset.purchaseId, modal);
    });
  });
});

function startPaymentTimer(el) {
  var expires = new Date(el.dataset.expires).getTime();
  function tick() {
    var remaining = Math.max(0, expires - Date.now());
    var m = Math.floor(remaining / 60000);
    var s = Math.floor((remaining % 60000) / 1000);
    el.textContent = m + ':' + String(s).padStart(2, '0');
    if (remaining > 0) {
      setTimeout(tick, 1000);
    } else {
      var modal = el.closest('.marketplace-purchase-modal');
      if (modal) modal.remove();
    }
  }
  tick();
}

// Poll /themis/v1/purchase/{id} every 10s as fallback when SSE webhook doesn't arrive.
function startPurchasePolling(purchaseId, modal) {
  if (!purchaseId || !modal) return;
  var interval = setInterval(function () {
    if (!document.contains(modal)) { clearInterval(interval); return; }
    fetch('/themis/v1/purchase/' + purchaseId)
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (data) {
        if (!data) return;
        if (data.status === 'delivered') {
          clearInterval(interval);
          modal.remove();
          var library = document.getElementById('buyer-library');
          if (library) htmx.trigger(library, 'refresh');
        } else if (data.status === 'expired') {
          clearInterval(interval);
          modal.remove();
        }
      })
      .catch(function () {});
  }, 10000);
}

// Called when user selects or drops a file in the file drop zone
function marketplaceHandleFileDrop(file) {
  if (!file) return;
  var nameEl = document.getElementById('listing-file-name');
  // Update the hidden input if dropped (not via input change)
  var input = document.getElementById('listing-file');
  if (input && !input.files.length && file) {
    // Can't set FileList directly; the file input's onchange handles input selection
    // For drag-drop, store the file reference
    input._droppedFile = file;
  }
  if (nameEl) {
    nameEl.textContent = file.name;
    nameEl.hidden = false;
  }
}

// Called by the listing form submit button
async function marketplaceCreateListing() {
  var form = document.getElementById('listing-form');
  var fileInput = document.getElementById('listing-file');
  var stepsEl = document.getElementById('listing-upload-steps');
  var submitBtn = document.getElementById('listing-submit-btn');
  if (!form || !fileInput || !stepsEl) return;

  var file = (fileInput.files && fileInput.files[0]) || fileInput._droppedFile;
  if (!file) { alert('Select a file first.'); return; }

  // Disable form during upload
  submitBtn.disabled = true;
  stepsEl.hidden = false;

  function setStep(id, state) {  // state: 'active' | 'done' | 'error'
    ['active','done','error','pending'].forEach(function(s) {
      document.getElementById(id).classList.remove('marketplace-upload-step--' + s);
    });
    document.getElementById(id).classList.add('marketplace-upload-step--' + state);
  }

  try {
    setStep('step-encrypt', 'active');
    var result = await Malkuth.encryptContent(file);
    setStep('step-encrypt', 'done');

    setStep('step-upload', 'active');
    var fd = new FormData();
    fd.append('file', result.ciphertext, 'encrypted');
    var upResp = await fetch('/api/media/upload-encrypted', { method: 'POST', body: fd });
    if (!upResp.ok) { setStep('step-upload', 'error'); submitBtn.disabled = false; return; }
    var upData = await upResp.json();
    setStep('step-upload', 'done');

    setStep('step-list', 'active');
    var visInput = form.querySelector('[name=visibility]:checked');
    var payload = {
      event_type:    'marketplace.listing.create',
      title:         form.querySelector('[name=title]').value,
      description:   form.querySelector('[name=description]').value,
      price_sats:    parseInt(form.querySelector('[name=price_sats]').value, 10),
      content_url:   upData.url,
      content_hash:  result.content_hash,
      cek_encrypted: result.cek_encrypted_b64,
      visibility:    visInput ? visInput.value : 'public',
    };
    var signedPayload = await Malkuth.signMarketplaceEvent(payload);
    var resp = await fetch('/events', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(signedPayload),
    });
    if (resp.ok) {
      setStep('step-list', 'done');
      // Close modal and refresh listings
      var modal = form.closest('.marketplace-listing-modal');
      if (modal) setTimeout(function() { modal.remove(); }, 600);
      // Re-init seller dashboard to refresh listing grid
      var pialMeta = document.querySelector('meta[name="f33d3r:pial"]');
      if (pialMeta && pialMeta.content) {
        setTimeout(function() { marketplaceInitSellerDashboard(pialMeta.content); }, 700);
      }
    } else {
      setStep('step-list', 'error');
      var errData = await resp.json().catch(function() { return {}; });
      alert('Error: ' + (errData.error || resp.statusText));
      submitBtn.disabled = false;
    }
  } catch (err) {
    ['step-encrypt','step-upload','step-list'].forEach(function(id) {
      var el = document.getElementById(id);
      if (el && el.classList.contains('marketplace-upload-step--active')) {
        setStep(id, 'error');
      }
    });
    alert('Error: ' + err.message);
    submitBtn.disabled = false;
  }
}

// Decrypt and display purchased content
Malkuth.decryptAndShow = async function(purchaseId, cekForBuyerB64, contentUrl, contentHash, mimeType) {
  var display = document.getElementById('viewer-display-' + purchaseId);
  var locked  = document.getElementById('viewer-locked-' + purchaseId);
  var errEl   = document.getElementById('viewer-error-' + purchaseId);
  if (!display || !locked) return;

  try {
    var plaintext = await Malkuth.decryptContent(cekForBuyerB64, contentUrl, contentHash);
    var blob = new Blob([plaintext], { type: mimeType || 'application/octet-stream' });
    var url  = URL.createObjectURL(blob);

    if (mimeType && mimeType.startsWith('image/')) {
      var img = document.createElement('img');
      img.src = url;
      img.alt = 'Decrypted content';
      display.appendChild(img);
    } else if (mimeType && mimeType.startsWith('video/')) {
      var vid = document.createElement('video');
      vid.src = url;
      vid.controls = true;
      vid.autoplay = false;
      display.appendChild(vid);
    } else {
      var a = document.createElement('a');
      a.href = url;
      a.download = 'content';
      a.textContent = 'Download file';
      display.appendChild(a);
    }

    locked.hidden  = true;
    display.hidden = false;
  } catch (err) {
    if (errEl) {
      errEl.textContent = 'Decryption failed: ' + err.message;
      errEl.hidden = false;
    }
  }
};

// Main init function for /create/marketplace — called on DOMContentLoaded by the template
async function marketplaceInitSellerDashboard(pialId) {
  if (!pialId) return;
  _sellerPialId = pialId; // store for re-use after shop open

  var gate      = document.getElementById('shop-gate');
  var dashboard = document.getElementById('shop-dashboard');
  if (!gate || !dashboard) return;

  var statusResp = await fetch('/themis/v1/shop/' + pialId + '/status').catch(function() { return null; });
  if (!statusResp || !statusResp.ok) {
    // Show gate on error — safer than showing dashboard
    gate.hidden = false;
    return;
  }
  var status = await statusResp.json();

  if (!status.exists || !status.is_active) {
    gate.hidden = false;
    dashboard.hidden = true;
    return;
  }

  // Shop is open — show dashboard
  gate.hidden = true;
  dashboard.hidden = false;

  // Populate status bar
  var btcEl = document.getElementById('shop-status-btc');
  if (btcEl && status.btc_address) {
    var addr = status.btc_address;
    btcEl.textContent = addr.length > 20 ? addr.slice(0, 10) + '…' + addr.slice(-8) : addr;
  }

  // Load stats
  marketplaceLoadSellerStats(pialId);

  // Load listings
  marketplaceLoadSellerListings(pialId);
}

async function marketplaceLoadSellerStats(pialId) {
  var resp = await fetch('/themis/v1/seller/' + pialId + '/stats').catch(function() { return null; });
  if (!resp || !resp.ok) return;
  var data = await resp.json();

  var salesEl   = document.getElementById('stat-sales');
  var revenueEl = document.getElementById('stat-revenue');
  var pendingEl = document.getElementById('stat-pending');

  if (salesEl)   salesEl.textContent   = data.sales !== undefined ? data.sales : '—';
  if (revenueEl) revenueEl.textContent = data.revenue_sats !== undefined ? (data.revenue_sats).toLocaleString() + ' sats' : '—';
  if (pendingEl) pendingEl.textContent = data.pending !== undefined ? data.pending : '—';
}

async function marketplaceLoadSellerListings(pialId) {
  var container = document.getElementById('seller-listings');
  var countEl   = document.getElementById('listing-count-display');
  if (!container) return;

  var resp = await fetch('/themis/v1/shop/' + pialId).catch(function() { return null; });
  if (!resp || !resp.ok) {
    container.innerHTML = '<div class="marketplace-empty-state"><p class="marketplace-empty-state__body">Could not load listings.</p></div>';
    return;
  }
  var data = await resp.json();
  var listings = data.listings || [];

  if (countEl) {
    countEl.innerHTML = '<strong>' + listings.length + '</strong> of 12 listings used';
  }

  if (listings.length === 0) {
    container.innerHTML =
      '<div class="marketplace-empty-state">' +
        '<svg class="marketplace-empty-state__icon" width="40" height="40" fill="none" stroke="currentColor" stroke-width="1.5" viewBox="0 0 24 24"><path d="M6 2 3 6v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2V6l-3-4z"/><line x1="3" y1="6" x2="21" y2="6"/><path d="M16 10a4 4 0 0 1-8 0"/></svg>' +
        '<h3 class="marketplace-empty-state__title">No listings yet</h3>' +
        '<p class="marketplace-empty-state__body">Click "+ New Listing" above to create your first listing.</p>' +
      '</div>';
    return;
  }

  container.innerHTML = listings.map(function(l) {
    var inactive = !l.is_active ? ' marketplace-listing-card--inactive' : '';
    var badge = !l.is_active ? '<span class="marketplace-listing-card__status-badge">Inactive</span>' : '';
    var subBadge = l.visibility === 'subscribers_only' ? '<span class="marketplace-listing-card__badge">Subscribers only</span>' : '';
    return (
      '<article class="marketplace-listing-card' + inactive + '" data-listing-id="' + l.id + '" data-listing-active="' + l.is_active + '">' +
        '<div class="marketplace-listing-card__header">' +
          '<h3 class="marketplace-listing-card__title">' + escapeHtml(l.title) + '</h3>' +
          '<div style="display:flex;gap:6px;align-items:center">' + subBadge + badge + '</div>' +
        '</div>' +
        '<p class="marketplace-listing-card__description">' + escapeHtml(l.description || '') + '</p>' +
        '<div class="marketplace-listing-card__footer">' +
          '<span class="marketplace-listing-card__price">' + l.price_sats.toLocaleString() + ' sats</span>' +
          '<div class="marketplace-listing-card__seller-actions">' +
            '<button class="marketplace-listing-card__edit-btn" onclick="marketplaceEditListing(\'' + l.id + '\')" type="button">Edit</button>' +
            '<button class="marketplace-listing-card__toggle-btn" onclick="marketplaceToggleListing(\'' + l.id + '\',' + l.is_active + ')" type="button">' + (l.is_active ? 'Deactivate' : 'Activate') + '</button>' +
            '<button class="marketplace-listing-card__delete-btn" onclick="marketplaceDeleteListing(\'' + l.id + '\')" type="button">Delete</button>' +
          '</div>' +
        '</div>' +
      '</article>'
    );
  }).join('');
}

function escapeHtml(str) {
  return String(str).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

// Called by the shop gate form submit button
async function marketplaceOpenShop() {
  var input  = document.getElementById('shop-gate-btc-input');
  var btn    = document.getElementById('shop-gate-btn');
  var errEl  = document.getElementById('shop-gate-error');
  if (!input) return;

  var btc = input.value.trim();
  if (!btc) {
    if (errEl) { errEl.textContent = 'Please enter your Bitcoin address.'; errEl.hidden = false; }
    return;
  }
  // Basic validation: must start with bc1, 1, or 3 and be at least 26 chars
  if (btc.length < 26 || !/^(bc1|[13])/.test(btc)) {
    if (errEl) { errEl.textContent = 'That does not look like a valid Bitcoin address.'; errEl.hidden = false; }
    return;
  }

  btn.disabled = true;
  if (errEl) errEl.hidden = true;

  var signedPayload = await Malkuth.signMarketplaceEvent({ event_type: 'marketplace.shop.open', btc_address: btc });
  var resp = await fetch('/events', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(signedPayload),
  }).catch(function() { return null; });

  if (resp && resp.ok) {
    // Shop opened — reload so the server renders the correct initial state
    location.reload();
  } else {
    btn.disabled = false;
    var errData = resp ? await resp.json().catch(function() { return {}; }) : {};
    if (errEl) {
      errEl.textContent = errData.message || errData.error || 'Could not open shop. Try again.';
      errEl.hidden = false;
    }
  }
}

// Close shop — sends marketplace.shop.close and reloads so server renders gate
async function marketplaceToggleShop() {
  if (!confirm('Close your shop? Buyers will not be able to make new purchases while it is closed.')) return;

  var signedPayload = await Malkuth.signMarketplaceEvent({ event_type: 'marketplace.shop.close' });
  var resp = await fetch('/events', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(signedPayload),
  }).catch(function() { return null; });

  if (resp && resp.ok) {
    location.reload();
  }
}

async function marketplaceDeleteListing(listingId) {
  if (!confirm('Delete this listing? This cannot be undone.')) return;

  var signedDeletePayload = await Malkuth.signMarketplaceEvent({ event_type: 'marketplace.listing.delete', listing_id: listingId });
  var resp = await fetch('/events', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(signedDeletePayload),
  }).catch(function() { return null; });

  if (resp && resp.ok) {
    var card = document.querySelector('[data-listing-id="' + listingId + '"]');
    if (card) card.remove();
    // Refresh count
    var pialMeta = document.querySelector('meta[name="f33d3r:pial"]');
    if (pialMeta && pialMeta.content) marketplaceLoadSellerListings(pialMeta.content);
  } else {
    alert('Could not delete listing. Try again.');
  }
}

async function marketplaceToggleListing(listingId, currentlyActive) {
  var signedToggleListingPayload = await Malkuth.signMarketplaceEvent({
    event_type: 'marketplace.listing.edit',
    listing_id: listingId,
    is_active:  !currentlyActive,
  });
  var resp = await fetch('/events', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(signedToggleListingPayload),
  }).catch(function() { return null; });

  if (resp && resp.ok) {
    var pialMeta = document.querySelector('meta[name="f33d3r:pial"]');
    if (pialMeta && pialMeta.content) marketplaceLoadSellerListings(pialMeta.content);
  } else {
    alert('Could not update listing. Try again.');
  }
}

async function marketplaceEditListing(listingId) {
  // Find current card values
  var card = document.querySelector('[data-listing-id="' + listingId + '"]');
  var currentTitle = card ? card.querySelector('.marketplace-listing-card__title').textContent.trim() : '';
  var currentDesc  = card ? card.querySelector('.marketplace-listing-card__description').textContent.trim() : '';

  var newTitle = prompt('Edit title:', currentTitle);
  if (newTitle === null) return;  // cancelled
  var newDesc = prompt('Edit description:', currentDesc);
  if (newDesc === null) return;

  var signedEditPayload = await Malkuth.signMarketplaceEvent({
    event_type:  'marketplace.listing.edit',
    listing_id:  listingId,
    title:       newTitle.trim() || currentTitle,
    description: newDesc,
  });
  var resp = await fetch('/events', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(signedEditPayload),
  }).catch(function() { return null; });

  if (resp && resp.ok) {
    var pialMeta = document.querySelector('meta[name="f33d3r:pial"]');
    if (pialMeta && pialMeta.content) marketplaceLoadSellerListings(pialMeta.content);
  } else {
    alert('Could not update listing. Try again.');
  }
}
