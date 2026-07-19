/**
 * onboard_single.js — F33D3R unified onboarding page
 *
 * Follows LISTEN / LOCATE / PATCH / EMIT pattern only.
 * No frameworks. No global state libraries. No DOM building.
 *
 * Responsibilities:
 *   LISTEN  — input events on password / confirm, change on role radios
 *   LOCATE  — getElementById, querySelector — never by class on dynamic content
 *   PATCH   — update strength bar, match indicator, show/hide creator-type panel
 *   EMIT    — form submits naturally; no fetch overrides
 */

(function () {
  'use strict';

  // ── LOCATE ────────────────────────────────────────────────────────────────
  function $(id) { return document.getElementById(id); }

  // ── Password strength ─────────────────────────────────────────────────────

  /**
   * Score a password 0–4.
   * Simple heuristic: length bands + character class diversity.
   * We intentionally do NOT ship a full zxcvbn — just enough signal.
   */
  function scorePassword(pw) {
    if (!pw || pw.length === 0) return 0;
    var score = 0;
    if (pw.length >= 8)  score++;
    if (pw.length >= 12) score++;
    if (/[A-Z]/.test(pw) && /[a-z]/.test(pw)) score++;
    if (/[0-9]/.test(pw))                       score++;
    if (/[^A-Za-z0-9]/.test(pw))                score++;
    return Math.min(4, score);
  }

  var strengthLabels = ['', 'Weak', 'Fair', 'Good', 'Strong'];
  var strengthClasses = ['', 'onboard-pw-strength__bar--weak', 'onboard-pw-strength__bar--fair',
                             'onboard-pw-strength__bar--good', 'onboard-pw-strength__bar--strong'];
  var strengthWidths  = ['0%', '25%', '50%', '75%', '100%'];

  function patchStrength(pw) {
    var bar   = $('ob-pw-strength-bar');
    var label = $('ob-pw-strength-label');
    if (!bar || !label) return;

    if (!pw) {
      bar.style.width = '0%';
      bar.className   = 'onboard-pw-strength__bar';
      label.textContent = '';
      return;
    }

    var score = scorePassword(pw);
    bar.style.width = strengthWidths[score];
    bar.className   = 'onboard-pw-strength__bar ' + (strengthClasses[score] || '');
    label.textContent = score > 0 ? ('Password strength: ' + strengthLabels[score]) : '';
  }

  // ── Password match indicator ──────────────────────────────────────────────

  function patchMatch() {
    var pw      = $('ob-password');
    var confirm = $('ob-confirm-password');
    var err     = $('ob-confirm-error');
    if (!pw || !confirm || !err) return;

    if (confirm.value === '') {
      err.hidden = true;
      err.textContent = '';
      return;
    }

    if (pw.value !== confirm.value) {
      err.hidden = false;
      err.textContent = "Passwords don't match.";
    } else {
      err.hidden = true;
      err.textContent = '';
    }
  }

  // ── Role picker ───────────────────────────────────────────────────────────

  var ROLES = ['user', 'creator', 'official'];

  window.onboardSelectRole = function (selected) {
    ROLES.forEach(function (role) {
      var card  = $('role-card-' + role);
      var check = $('role-check-' + role);
      if (!card || !check) return;

      var isSelected = (role === selected);
      if (isSelected) {
        card.classList.add('role-card--selected');
        card.setAttribute('aria-checked', 'true');
        check.classList.remove('role-check--hidden');
      } else {
        card.classList.remove('role-card--selected');
        card.setAttribute('aria-checked', 'false');
        check.classList.add('role-check--hidden');
      }
    });

    // PATCH — show/hide creator type sub-picker
    var wrap = $('creator-type-wrap');
    if (wrap) {
      if (selected === 'creator') {
        wrap.hidden = false;
        wrap.setAttribute('aria-hidden', 'false');
      } else {
        wrap.hidden = true;
        wrap.setAttribute('aria-hidden', 'true');
        // Clear selection so it doesn't accidentally post a creator_type
        var sel = $('creator_type_select');
        if (sel) sel.value = '';
      }
    }
  };

  // ── Content picker ────────────────────────────────────────────────────────

  var CONTENT_PREFS = ['default', 'safe_mode', 'adult_enabled'];

  window.onboardSelectContent = function (selected) {
    CONTENT_PREFS.forEach(function (pref) {
      var card  = $('content-card-' + pref);
      var check = $('content-check-' + pref);
      if (!card || !check) return;

      var isSelected = (pref === selected);
      if (isSelected) {
        card.classList.add('role-card--selected');
        card.setAttribute('aria-checked', 'true');
        check.classList.remove('role-check--hidden');
      } else {
        card.classList.remove('role-card--selected');
        card.setAttribute('aria-checked', 'false');
        check.classList.add('role-check--hidden');
      }
    });
  };

  // ── Theme picker (reuse existing handler from onboard.js pattern) ─────────

  window.selectOnboardTheme = function (el, id, surface, accent) {
    document.querySelectorAll('.theme-option').forEach(function (o) {
      o.classList.remove('selected');
    });
    el.classList.add('selected');
    document.body.dataset.theme = id;
  };

  // ── Client-side submit guard ──────────────────────────────────────────────
  // Prevents double-submit; shows loading state. Does NOT block — server validates.

  function onSubmit(e) {
    var btn = $('ob-submit-btn');
    // Clear any prior inline errors before re-submitting
    ['ob-handle-error','ob-display-name-error',
     'ob-dob-error','ob-password-error','ob-confirm-error','ob-terms-error'].forEach(function (id) {
      var el = $(id);
      if (el) { el.hidden = true; el.textContent = ''; }
    });

    // Basic client-side terms check (server also enforces)
    var terms = document.querySelector('[name="terms_accepted"]');
    if (terms && !terms.checked) {
      e.preventDefault();
      var termsErr = $('ob-terms-error');
      if (termsErr) {
        termsErr.hidden = false;
        termsErr.textContent = 'You must accept the Terms of Service to continue.';
      }
      terms.focus();
      return;
    }

    // Confirm password match check (server also enforces)
    var pw      = $('ob-password');
    var confirm = $('ob-confirm-password');
    if (pw && confirm && pw.value !== confirm.value) {
      e.preventDefault();
      var matchErr = $('ob-confirm-error');
      if (matchErr) {
        matchErr.hidden = false;
        matchErr.textContent = "Passwords don't match.";
      }
      confirm.focus();
      return;
    }

    if (btn) {
      btn.disabled = true;
      btn.textContent = 'Creating account…';
    }
  }

  // ── LISTEN — wire all events on DOMContentLoaded ──────────────────────────

  document.addEventListener('DOMContentLoaded', function () {
    // Password strength — LISTEN on password input
    var pwInput = $('ob-password');
    if (pwInput) {
      pwInput.addEventListener('input', function () {
        patchStrength(pwInput.value);
        patchMatch();
      });
    }

    // Match indicator — LISTEN on confirm input
    var confirmInput = $('ob-confirm-password');
    if (confirmInput) {
      confirmInput.addEventListener('input', patchMatch);
    }

    // Form submit guard
    var form = $('onboard-single-form');
    if (form) {
      form.addEventListener('submit', onSubmit);
    }

    // Init role state
    onboardSelectRole('user');
  });
})();
