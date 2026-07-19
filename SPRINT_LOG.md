# SPRINT_LOG.md

Session-by-session engineering log. Newest entries on top. Each entry: what
changed, why, where, and anything the next agent must not re-discover the hard way.

---

## 2026-06-03 — 3.0.1 — Quoted/embed works present transparent (no grey, no black stage)

**Problem:** Embedded/quoted/reposted/react-video works rendered with two unwanted
fills: (1) a grey "highlight" panel that darkened on hover, and (2) a black `#000`
stage behind the work. The work content itself is transparent, so that `#000`
showed through as a black box — most visible on NSFW embeds (which render only a
small "18+ content" flag) and on react-video stages. User wanted the work simply
presented on the normal card background, no differentiating tint, no black.

**Changes (CSS + one template inline style only — the fix lives entirely in
presentation, not in Go/JS):**

- `feed-engine/web/static/css/styles.css`
  - `.quoted-post-card` → `background: transparent` (was `var(--panel-hover)`);
    deleted `.quoted-post-card:hover { background: var(--bg-sunk); }` and the
    now-unused `transition: background`. Border kept (thin quote outline).
  - `.rv-view` and `.rv-view .rv-reaction, .rv-view .rv-post` →
    `background: transparent` (were `#000`). This is the react-video stage.
- `feed-engine/web/templates/partials/_media_container.html`
  - The `<video>` inline style `background:#000` → `background:transparent`, so an
    unplayed / letterboxed embed video no longer paints a black box.

**Scope note for next agent:** `.quoted-post-card` (`_quoted_work.html`) is the ONE
shared embed wireframe used by quote, repost, react-video original, AND the compose
preview — one change covers all surfaces (see [[project_work_core_facet]],
[[project_facet_atlas]]). Do not special-case per surface.

**Verify workflow:** cache-busters bumped z11 → z15 across all 24 `?v=` strings in
`feed-engine/web/templates/base.html`; `go build ./...` clean; rebuilt + restarted
`feed-engine` via docker-compose.local (healthy on :8081). User visually confirmed
works render correctly.

**Cache-buster state:** currently `20260603z15`. Next change bumps to `z16`.

**Release:** committed on `main`, tagged `v3.0.1`, pushed to `origin`
(gitlab.com/f33d3r-core/f33d3r) on explicit user authorization overriding the
CLAUDE.md no-push rule for this release.
