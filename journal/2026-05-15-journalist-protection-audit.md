# Journalist & Scientist Protection — Architecture Audit
**Date:** 2026-05-15
**Branch:** journal
**Author:** edd / Claude Sonnet 4.6

---

## Context

Audit of a proposed journalist/scientist protection system against F33D3R's existing architecture. The source document outlined a layered protection model covering identity, source protection, metadata minimization, evidence integrity, operational security, anti-harassment, and emergency controls.

The core thesis of the source document: anonymity alone is not enough. Real-world deanonymization happens through metadata, behavioral patterns, device fingerprints, upload fingerprints, timing correlation, payment trails, and social graph leakage.

---

## What F33D3R Already Has That's Genuinely Useful Here

**PIAL is the right spine.** The real-identity → pseudonym separation the document recommends is exactly what PIAL enables. A journalist could have a real PIAL (known to the platform, auditable internally) anchored to a pseudonymous handle that the public never connects to their real name. That's architecturally sound with what we've built.

**Vovin is strong.** ECDH-P256 + Double Ratchet, keys in IndexedDB only, platform can't read contents. That's not marketing — that's actually how it works. The document's SecureDrop-style inbox model would sit naturally on top of Vovin with some modifications.

**Isolated brain topology helps.** Because no brain reads another brain's DB, a legal demand against Nantar doesn't automatically expose Vovin's message store. That compartmentalization is real and meaningful.

**Capability enforcement is the right hook.** `CapProtectedJournalist` / `CapProtectedWhistleblower` slots directly into `model/types.go` and the existing `HasCapability` check pattern. The plumbing is there.

---

## What's Genuinely Missing Today

**Caeor (media brain) almost certainly doesn't strip metadata.** This is Tier 1, it's table stakes, and it should already be done regardless of journalism protection. EXIF with GPS coordinates in an upload is a catastrophic leak. We need to verify what Caeor does on ingest. If it's just transcoding for HLS/WebP without stripping EXIF, that's a live problem for all users right now, not just journalists.

**No delayed publishing.** The scheduler concept doesn't exist anywhere. This is one of the highest-impact, lowest-complexity features on the list. A `publish_at` field on `PostEvent` + a background job in Nantar is achievable without a new brain.

**No chain of custody.** Asset hashing on upload exists implicitly (dedup by hash) but there's no immutable audit log of transformations, access, or moderation actions on an asset. Caeor would need this.

**Social graph is currently exposed.** Shared followers, mutuals, who-liked-what — all queryable. For a protected account this is a serious deanonymization vector. We'd need per-account social graph visibility controls, which today don't exist.

**No panic mode.** Nothing in the current event model handles emergency lockdown. This would be a new event type and a new capability state in Elohim Veni.

---

## Where the Source Document Has Gaps

**The platform itself is the threat vector it doesn't name.** "Platform knows real identity under strict controls" is only safe if the platform can resist a legal demand. A subpoena to F33D3R for the PIAL → real identity mapping breaks the whole model. The document doesn't address jurisdiction, warrant canaries, what data is retained and for how long, or whether the platform should ever be in a position to know the real identity at all. For the highest-risk users (dissidents, whistleblowers in hostile states), "we know who you are but we protect it" is not good enough. We need a model where the platform *cannot* disclose what it doesn't have.

**Browser-based crypto is not SecureDrop.** SecureDrop is air-gapped, Tor-only, no JavaScript execution from a CDN. A browser-based source inbox running JavaScript served from our servers means a compromised server or CDN can serve malicious JS that exfiltrates keys. The document describes the goal correctly but undersells how hard the browser threat model is to satisfy. Vovin is good for routine protected communication. For true high-risk source protection, we'd need a separate hardened client — not a web app.

**Behavioral fingerprinting within the platform isn't addressed.** Writing style, posting cadence, interaction timing, device/browser fingerprint, session correlation — none of this is in the document. A journalist posting under a pseudonym at the same times every day, from the same IP range, with the same writing patterns, is deanonymizable even with perfect crypto. This is an operational security problem, not a platform problem, but the document should at least name it.

**The verification bootstrapping problem.** How does a journalist prove they're a journalist to get `CapProtectedJournalist` without exposing their identity in the verification process? The document doesn't address this. You either trust self-attestation (weak), require credential submission (creates a record), or build a third-party attestation model. Each has different risk profiles.

---

## The "Sanctum" Recommendation Is Right

Treat it as a separate operational environment with different defaults, different retention, different moderation paths, different legal handling. Don't bolt it onto the existing creator system. The threat models are different enough that shared infrastructure creates contamination risk.

---

## Honest Priority Ordering for F33D3R Specifically

### Do now regardless of journalism features
- Verify Caeor strips EXIF/metadata on every upload. If it doesn't, fix it. This is a live issue.

### Tier 1 — maps directly to existing architecture
- `CapProtectedJournalist` capability + protected mode defaults (discoverability off, DMs restricted, no recommendation amplification, social graph hidden)
- Delayed publishing (`publish_at` on `PostEvent`)
- Asset chain-of-custody hash log in Caeor

### Tier 1 — needs new thought
- Pseudonymous identity (secondary handle anchored to PIAL) — needs a new model and careful UI design
- What data we retain and for how long — legal/policy decision before any code

### Tier 2
- Panic mode (new event type, Elohim Veni state)
- Source inboxes on Vovin with important caveats about browser threat model
- Anti-brigading (Zodacare already exists for this — extend it)

### Don't rush
- Region-based visibility — complex, easy to get wrong
- Legal evidence export — get legal counsel involved before shipping this

---

## Bottom Line

The document is a serious, well-structured threat model. F33D3R's architecture is better positioned for this than most platforms. The gaps to close first are operational: EXIF stripping (verify today), retention policy (decide before building), and the legal compulsion question (what can we be forced to hand over?). Get those right before writing code.

---

## Open Questions

1. Does Caeor strip EXIF today? Who knows the answer?
2. What is our current data retention policy? Is it written down anywhere?
3. Where are we on jurisdiction — what legal regime governs the platform?
4. Is a warrant canary feasible given the current corporate structure?
5. Third-party attestation model for journalist verification — who would we partner with?
6. What does "Sanctum" look like as a brain in the topology? New brain or a capability layer on existing brains?
