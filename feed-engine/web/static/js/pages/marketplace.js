'use strict';

// marketplace.js — the marketplace and seller dashboard page script.
// Registered as a Shell page: executed once per session, the mount runs on
// every arrival of a marketplace Playground. Document-level listeners are
// installed once.
//
// Everything the browser reads about the marketplace is a server Facet:
//   /themis/v1/library                         — the buyer's library
//   /facets/marketplace/purchase/{id}/status   — the purchase modal's status line (polls itself)
//   /facets/marketplace/seller/stats           — seller stats strip
//   /facets/marketplace/seller/listings        — seller listings + count line
// What remains here is what only the browser can do: hold the seller's
// signing key and sign marketplace events with it (Themis verifies the
// signature — see themis/src/handlers.rs verify_pial_signature), encrypt
// content client-side before upload, and decrypt purchased content with a key
// the server never holds.
F33D3R.page('marketplace', function (root) {
  // Malkuth signing + ECDH keys are initialised globally in malkuth.js on every
  // page load — no per-page init needed here. PIAL carries all keys at login.
  F33D3R.once('marketplace:listeners', function () {
    // FA Live: marketplace_purchase_delivered — the library Facet is asked for
    // again; the purchase modal's status Facet reaches its final state on its
    // own next poll.
    document.addEventListener('sse:marketplace_purchase_delivered', function () {
      var library = document.getElementById('buyer-library');
      if (library && window.htmx) htmx.trigger(library, 'refresh');
    });

    // Payment-window countdown on any purchase modal the Shell places.
    document.addEventListener('htmx:afterSwap', function () {
      startTimers(document);
    });
  });

  function startTimers(scope) {
    scope.querySelectorAll('.market-timer[data-expires]:not([data-timer-started])').forEach(function (el) {
      el.dataset.timerStarted = 'true';
      startPaymentTimer(el);
    });
  }

  // Start expiry timers for any open purchase modals already in the DOM
  startTimers(root.querySelectorAll ? root : document);

  function startPaymentTimer(el) {
    var expires = new Date(el.dataset.expires).getTime();
    function tick() {
      if (!el.isConnected) return;
      var remaining = Math.max(0, expires - Date.now());
      var m = Math.floor(remaining / 60000);
      var s = Math.floor((remaining % 60000) / 1000);
      el.textContent = m + ':' + String(s).padStart(2, '0');
      if (remaining > 0) setTimeout(tick, 1000);
    }
    tick();
  }

  // The seller listings Facet is asked for again after a listing mutation.
  function refreshSellerListings() {
    var listings = document.getElementById('seller-listings');
    if (listings && window.htmx) htmx.trigger(listings, 'refresh');
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
        // The overlay closes and the listings Facet is asked for again.
        var slot = document.getElementById('overlay-slot');
        if (slot) setTimeout(function() { slot.innerHTML = ''; }, 600);
        setTimeout(refreshSellerListings, 700);
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
  if (window.Malkuth) {
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
  }

  // Called by the shop gate form submit button. An opened shop is a different
  // page: the Shell asks the server for it again and swaps the fresh
  // Playground in place.
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
      F33D3R.refresh();
    } else {
      btn.disabled = false;
      var errData = resp ? await resp.json().catch(function() { return {}; }) : {};
      if (errEl) {
        errEl.textContent = errData.message || errData.error || 'Could not open shop. Try again.';
        errEl.hidden = false;
      }
    }
  }

  // Close shop — sends marketplace.shop.close; the server renders the gate again.
  async function marketplaceToggleShop() {
    if (!confirm('Close your shop? Buyers will not be able to make new purchases while it is closed.')) return;

    var signedPayload = await Malkuth.signMarketplaceEvent({ event_type: 'marketplace.shop.close' });
    var resp = await fetch('/events', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(signedPayload),
    }).catch(function() { return null; });

    if (resp && resp.ok) {
      F33D3R.refresh();
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
      refreshSellerListings();
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
      refreshSellerListings();
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
      refreshSellerListings();
    } else {
      alert('Could not update listing. Try again.');
    }
  }

  window.marketplaceHandleFileDrop     = marketplaceHandleFileDrop;
  window.marketplaceCreateListing      = marketplaceCreateListing;
  window.marketplaceOpenShop           = marketplaceOpenShop;
  window.marketplaceToggleShop         = marketplaceToggleShop;
  window.marketplaceDeleteListing      = marketplaceDeleteListing;
  window.marketplaceToggleListing      = marketplaceToggleListing;
  window.marketplaceEditListing        = marketplaceEditListing;
});
