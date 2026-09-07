// atlas.js — Facet Atlas chrome interactions only. No framework, no app state.
// Three jobs: id search, tier filter, and jump-to-card flash highlight.
(function () {
  "use strict";

  var cards = Array.prototype.slice.call(document.querySelectorAll(".atlas-card"));
  var tiers = Array.prototype.slice.call(document.querySelectorAll(".atlas-tier"));
  var search = document.getElementById("atlas-search");
  var tierfilter = document.getElementById("atlas-tierfilter");
  var statusfilter = document.getElementById("atlas-statusfilter");

  var activeTier = "*";
  var activeStatus = "*";
  var query = "";

  function apply() {
    var q = query.trim().toLowerCase();
    cards.forEach(function (card) {
      var name = (card.getAttribute("data-name") || "").toLowerCase();
      var tier = card.getAttribute("data-tier") || "";
      var review = card.getAttribute("data-review") || "unreviewed";
      var okQ = q === "" || name.indexOf(q) !== -1;
      var okT = activeTier === "*" || tier === activeTier;
      var okS = activeStatus === "*" || review === activeStatus;
      card.classList.toggle("atlas-hidden", !(okQ && okT && okS));
    });
    // Hide a tier section entirely when none of its cards are visible.
    tiers.forEach(function (section) {
      var visible = section.querySelectorAll(".atlas-card:not(.atlas-hidden)").length;
      section.classList.toggle("atlas-hidden", visible === 0);
    });
  }

  // ── Toast ──
  var toast = document.getElementById("atlas-toast");
  var toastTimer = null;
  function showToast(msg) {
    if (!toast) return;
    toast.textContent = msg;
    toast.hidden = false;
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { toast.hidden = true; }, 1600);
  }

  if (search) {
    search.addEventListener("input", function () {
      query = search.value;
      apply();
    });
  }

  if (tierfilter) {
    tierfilter.addEventListener("click", function (e) {
      var btn = e.target.closest(".atlas-tierbtn");
      if (!btn) return;
      activeTier = btn.getAttribute("data-tier");
      tierfilter.querySelectorAll(".atlas-tierbtn").forEach(function (b) {
        b.classList.toggle("is-on", b === btn);
      });
      apply();
    });
  }

  if (statusfilter) {
    statusfilter.addEventListener("click", function (e) {
      var btn = e.target.closest(".atlas-statusbtn");
      if (!btn) return;
      activeStatus = btn.getAttribute("data-status");
      statusfilter.querySelectorAll(".atlas-statusbtn").forEach(function (b) {
        b.classList.toggle("is-on", b === btn);
      });
      apply();
    });
  }

  // ── Click-to-copy citable ids ──
  document.addEventListener("click", function (e) {
    var btn = e.target.closest(".atlas-copy");
    if (!btn) return;
    var text = btn.getAttribute("data-copy") || "";
    var done = function () {
      btn.classList.add("atlas-copied");
      setTimeout(function () { btn.classList.remove("atlas-copied"); }, 700);
      showToast("copied: " + text);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done, done);
    } else {
      done();
    }
  });

  // ── Admin review: persist approve / needs-work / clear ──
  function recount() {
    var ok = document.querySelectorAll('.atlas-card[data-review="approved"]').length;
    var nw = document.querySelectorAll('.atlas-card[data-review="needs_work"]').length;
    var a = document.getElementById("atlas-prog-approved");
    var n = document.getElementById("atlas-prog-needs");
    if (a) a.textContent = ok;
    if (n) n.textContent = nw;
    // per-tier "x/y approved"
    tiers.forEach(function (section) {
      var total = section.querySelectorAll(".atlas-card").length;
      var appr = section.querySelectorAll('.atlas-card[data-review="approved"]').length;
      var c = section.querySelector(".atlas-tiercount");
      if (c) c.textContent = appr + "/" + total + " approved";
    });
  }

  document.addEventListener("click", function (e) {
    var rbtn = e.target.closest(".atlas-rbtn");
    if (!rbtn) return;
    var bar = rbtn.closest(".atlas-review-bar");
    var card = rbtn.closest(".atlas-card");
    if (!bar || !card) return;
    var name = bar.getAttribute("data-facet");
    var status = rbtn.getAttribute("data-set");
    var noteEl = bar.querySelector(".atlas-rnote");
    var note = noteEl ? noteEl.value : "";

    var body = new URLSearchParams();
    body.set("name", name);
    body.set("status", status);
    body.set("note", note);

    fetch("/_atlas/review", {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body: body.toString(),
    }).then(function (res) {
      if (!res.ok) { showToast("save failed (" + res.status + ")"); return; }
      return res.text();
    }).then(function (newStatus) {
      if (!newStatus) return;
      card.setAttribute("data-review", newStatus);
      var chip = card.querySelector("[data-review-chip]");
      if (chip) {
        chip.textContent = newStatus;
        chip.className = "atlas-review-chip atlas-review-" + newStatus;
      }
      recount();
      apply();
      showToast(name + " → " + newStatus);
    }).catch(function () { showToast("save failed"); });
  });

  // Jump-to-card: clicking a slot/reverse chip scrolls to the target and flashes
  // it, even if a filter is currently hiding it.
  function flash(name) {
    var target = document.getElementById("facet-" + name);
    if (!target) return;
    target.classList.remove("atlas-hidden");
    var section = target.closest(".atlas-tier");
    if (section) section.classList.remove("atlas-hidden");
    target.scrollIntoView({ behavior: "smooth", block: "center" });
    target.classList.remove("atlas-flash");
    void target.offsetWidth; // restart the animation
    target.classList.add("atlas-flash");
  }

  document.addEventListener("click", function (e) {
    var link = e.target.closest("[data-jump]");
    if (!link) return;
    e.preventDefault();
    flash(link.getAttribute("data-jump"));
  });
})();
