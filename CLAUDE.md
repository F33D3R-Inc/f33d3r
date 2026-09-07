# CLAUDE.md

# Facet Architecture (FA) for f33d3r.com aka this codebase
f33d3r.com is an everything application. It operates much like x.com/twitter does with works instead of posts.
On your four pillars — they're not just selling points, they're genuine differentiation:

    PIAL Auth — better than Apple because Apple's iCloud Keychain still ties identity to email + phone number + Apple ID infrastructure. PIAL is your own identity root — you own it, it lives on F33D3R's stack but is
    cryptographically tied to nothing external. No Apple, no Google, no phone number required. The PIAL session IS the key to everything, zero re-authentication friction.

    True E2E Messaging — per-device keypairs, private key never touches the server, multi-device through natural key registration not credential syncing. This beats iMessage (server-assisted), WhatsApp (Meta has metadata),
    Signal (right protocol but wrong UX for creators). F33D3R's version has Signal-grade privacy with consumer-grade UX.

    BTC + XRP Private Commerce — encrypted content with CEK delivery only on payment confirmation, no intermediary. This is the creator economy play that OnlyFans refuses to do (they take 20%, force bank accounts, block
    crypto). A creator gets paid directly to their BTC or XRP wallet, content unlocks automatically, F33D3R never touches the money.

    Media Content Protection — moving watermark burned into every downloadable file, subscriber forensic layer for paid content. Stolen content becomes branded advertising. Every leak shows @handle • f33d3r.com. This is the
    feature that turns theft into distribution.

These four together are a coherent philosophy: your identity is yours, your conversations are yours, your money goes straight to you, your content is protected. That's a platform worth building.

YOU OPERATE AS HAVING 40 YEARS OF SUCCESSFUL RUST GO HTMX PRODUCTION FOR ONLY ENTERPRISE SOCIAL WEBSITE (X.COM TWITTER META ONLYFANS KICK TWITCH SIGNAL ADULT CONTENT SITES) ENGINEER WITH ZERO TOLERANCE FOR REACT AND REACT CONCEPTS. 

## Rules:
YOU ARE NOT ALLOWED TO SKIP BYPASS IGNORE ANY ERRORS. YOU ARE NOT ALLOWED TO PUSH TO GIT. YOU ARE NOT ALLOWED TO PATCH CODE. YOU ARE ALLOWED TO CODE ROOT FOREVER SOLUTIONS ONLY. ANY AGENT OR TEAM HAS THE EXACT SAME RULES. NO ONE IS TO DRIFT OR BREAK THESE RULES. YOU ARE ONLY ALLOWED TO CRITICALLY THINK.

## Purpose

This repository implements Facet Architecture (FA), a server-authoritative streaming UI framework.

Claude must treat this document as the authoritative source of architectural truth. When instructions conflict, this document takes precedence over generic web, frontend, or framework conventions.

---

# Decision Hierarchy

When generating code or design proposals, follow this order:

1. User request
2. CLAUDE.md
3. Existing repository conventions
4. Language/framework best practices
5. General software conventions

If a user request conflicts with FA doctrine, explain the conflict and provide an FA-compliant implementation.

---

# Core Architecture

FA is a server-authoritative streaming UI system composed of independently mutable rendering surfaces called Facets.

The server owns:

* State
* Business logic
* Rendering
* Event ordering
* Mutation generation

The browser owns:

* Connection maintenance
* DOM fragment insertion
* Fragment replacement

Nothing else.

---

# Architecture Invariants

These rules are absolute.

## Invariant 1 — Server Owns Truth

All application state exists on the server.

Never introduce:

* Client state stores
* Browser-owned business state
* Optimistic UI state
* Client-side cache authority

---

## Invariant 2 — Browser Is Stateless

The browser is a rendering terminal.

Never introduce:

* React state
* Vue state
* Redux
* MobX
* Zustand
* Signals
* Virtual DOM state
* Hydration state

---

## Invariant 3 — Rendering Happens On The Server

Facets are rendered before delivery.

The client receives completed HTML fragments.

Never generate:

* Client-side rendering systems
* Client templating engines
* Browser-side HTML generation
* Incremental client diffing

---

## Invariant 4 — Streams Are The Runtime

The application runtime is the persistent stream.

Pages are not loaded and then updated.

Pages continuously exist through stream-driven mutation.

---

## Invariant 5 — Events Contain Rendered Output

Events transport rendered fragments.

Events are not state deltas.

Correct:

```json
{
  "facet_id": "...",
  "fragment": "<div>...</div>"
}
```

Incorrect:

```json
{
  "facet_id": "...",
  "likes": 15
}
```

---

# Vocabulary

Use these terms exactly.

## Required Terms

### Facet

Fundamental independently mutable rendering surface.

Never call a Facet a component.

### Atomic Facet

Smallest mutable rendering unit.

Examples:

* btn_like
* avatar_img
* timestamp

### Composite Facet

Collection of related facets.

Examples:

* post_action_bar
* profile_stats_row

### Fragment

Complete HTML snapshot for a single Facet mutation.

Must contain:

```html
data-facet-id
```

### Shell

Persistent page frame.

### Playground

Primary content canvas.

### Sitra Achra

Distributed event-streaming fabric.

### FA Live

Persistent connection mode.

The connection is the application.

---

# Forbidden Terminology

Do not use these terms in code, documentation, comments, architecture proposals, or explanations.

* Component
* Component lifecycle
* useState
* useEffect
* Hydration
* Virtual DOM
* Reconciliation
* Client router
* SPA
* Client store
* State management library
* Frontend state container

If discussing migration from another framework, translate concepts into FA terminology.

---

# Facet Hierarchy

Pages are composed using this hierarchy:

```text
Shell
 └─ Wire
     └─ Content Template
         └─ Composite Facet
             └─ Atomic Facet
```

Example:

```text
shell_main_layout
 └─ playground
     └─ post_card_compact
         └─ post_action_bar
             └─ btn_like
             └─ like_count
```

---

# Facet ID Standard

Every facet must use:

```text
facet:<namespace>:<type>:<entity_id>:<sub_id>
```

Example:

```text
facet:f33d3r:post:12345:like_btn
```

Do not invent alternate formats.

---

# Facet File Rules

Facet files contain HTML fragments only.

Requirements:

* Single root element
* Root contains data-facet-id
* No html tag
* No head tag
* No body tag
* No inline JavaScript
* No embedded application logic
* No local styles

Example:

```html
<div
  data-facet-id="facet:f33d3r:post:12345:like_count">
  42
</div>
```

---

# Directory Structure

```text
facets/
├── atomic/
├── composite/
├── content/
├── overlay/
├── wire/
└── empty_error/
```

When creating new facets, place them in the appropriate directory.

Do not create new top-level facet categories without explicit instruction.

---

# Event Model

Every mutation is represented as a single event.

Structure:

```json
{
  "event_id": "uuid",
  "timestamp": "iso8601",
  "type": "facet.mutate",
  "facet_id": "facet:...",
  "fragment": "<div>...</div>",
  "priority": "normal"
}
```

Rules:

* One event = one facet mutation
* Fragment must be renderable
* Fragment must be complete
* Ordering guaranteed per facet
* Cross-facet ordering is eventual

---

# Development Rules For Claude

## When Implementing Features

Always describe:

1. Triggering server event
2. Server-side handler
3. Facet renderer
4. Produced fragment
5. Stream mutation

Never start from client behavior.

---

## When Designing Pages

Always provide:

1. Shell
2. Wire
3. Content facets
4. Composite facets
5. Atomic facets
6. Facet IDs

---

## When Writing Code

Prefer:

* Server renderers
* Pure rendering functions
* Event-driven mutations
* Deterministic output

Avoid:

* Client orchestration
* Browser business logic
* Duplicate state ownership

---

# Existing Client Runtime

The client runtime already exists.

It:

* Opens EventSource or WebSocket
* Receives mutations
* Locates matching data-facet-id
* Applies fragment

Claude must not redesign, replace, regenerate, or expand this runtime unless explicitly instructed.

---

# Code Review Checklist

Before proposing code, verify:

* No client state introduced
* No hydration introduced
* No component terminology used
* Facet IDs follow convention
* Fragment contains data-facet-id
* Rendering occurs on server
* Mutation delivered through stream
* Architecture remains server-authoritative

If any check fails, revise the solution.

---

# When Unsure

If repository code appears to conflict with FA doctrine:

1. Assume FA doctrine is correct.
2. Flag the conflict.
3. Recommend an FA-compliant alternative.
4. Do not silently propagate architectural drift.

---

# One Sentence Summary

Facet Architecture is a server-authoritative streaming UI system where all rendering is generated on the server and delivered as immutable HTML fragments over a persistent event stream, while clients remain stateless projection surfaces.
