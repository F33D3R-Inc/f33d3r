# F33D3R iOS dev server

The local backend the iOS app runs against. It serves the `/api/v1` JSON
contract the app was written for, the `POST /events` write lane the web app
uses, and stands in for the two other brains the app touches — Elohim Veni
(signing keys) and Ain Soph (the wallet ledger) — over a single SQLite file.

It exists because the machine developing the app cannot run the platform's
Docker stack, and because the `/api/v1` handlers the app depends on do not exist
in `feed-engine` yet. It is a stand-in, not a fork: the table and column names
are feed-engine's, the signed-work admission code is `work_event.go`'s, and the
DTOs are the ones the Swift models decode. When the handlers move into
`feed-engine/internal/api/`, the app changes its base URL and nothing else.

## Run

```bash
cd devserver
make run          # http://127.0.0.1:8081, database in ./data
make reset        # wipe ./data/f33d3r.sqlite, reseed, run
make test         # contract tests against the Kit's golden fixtures
```

Needs Go and a C toolchain (SQLite is cgo). No Docker, no Postgres.

The app's DEBUG build already points at `http://127.0.0.1:8081`. To reach it
from a phone on the same network: `F33D3R_ADDR=0.0.0.0:8081 make run` and set
`F33D3R_BASE_URL` in the app's scheme to your Mac's address.

## Accounts

Seeded on first start, all with password `f33d3rdev`:

| Handle | Role |
|---|---|
| @tehanibentley | Founder, creator, verified |
| @admin | Admin, verified |
| @miiyazuko | Creator, verified |
| @dev | Creator, verified |
| @guest | User |

Each starts with 50 AET so tipping can be exercised. A follow graph, ten works
(text, photos, a poll, a voice note, replies), reactions, a couple of tips and
the notifications they owe are seeded too, so every lane has something on first
launch. Everything seeded is an ordinary row: delete it, like it, reply to it.
`POST /api/v1/auth/signup` creates more accounts, and so does the sign-in
screen.

## What it serves

Reads, bearer auth, JSON:

| Route | Answers |
|---|---|
| `POST /api/v1/auth/login`, `/signup` | `SessionDTO` (token, user, needs_backup_codes) |
| `POST /api/v1/auth/logout` | 204 |
| `GET /api/v1/me` | `MeDTO` — flat user + settings + `pial_id` (owner only) |
| `GET /api/v1/feed?surface=following\|foryou\|trending\|music\|visions\|live` | `WorkPageDTO` |
| `GET /api/v1/works/{id}` | `WorkThreadDTO` — work, ancestors, first replies |
| `GET /api/v1/works/{id}/replies` | `WorkPageDTO` |
| `GET /api/v1/users/{handle}` | `ProfileDTO` |
| `GET /api/v1/users/{handle}/works?tab=works\|replies\|media\|likes` | `WorkPageDTO`; likes are `403 likes_private` for anyone but the owner |
| `GET /api/v1/notifications` | `NotificationPageDTO`, grouped, with `unread_count` |
| `GET /api/v1/wallet` | `WalletDTO` — settled, pending, entries |
| `GET /api/v1/search?q=` | works and people |
| `POST /api/v1/media` | multipart `file` → `{"url": "/media/…"}`; images only |
| `POST /api/v1/media/voice` | multipart `audio` (or `file`) → 201 `VoiceUploadDTO`; the bytes decide the type (M4A, AAC, MP3, Ogg, WebM); no duration — the recorder's number travels with the work |
| `POST /api/v1/media/video` | multipart `file` (or `media`) → 202 `VideoJobDTO{queued}`, or 200 `{duplicate}` naming the original's author when a live work already carries the same bytes |
| `GET /api/v1/media/video/{id}` | `VideoJobDTO`; 404 `video_job_not_found` for anyone but the uploader. The stand-in transcoder has no ffmpeg: the file is its own master and watermarked rendition, and duration and frame size are read off the MP4's `moov` box. Jobs live in memory for the life of the process |
| `GET /api/v1/gif/search?q=` | `GifSearchDTO`; trending when `q` is empty. KLIPY when `KLIPY_API_KEY` (or feed-engine's `KLIPHY_API`) is set; otherwise the GIFs in `seedmedia/gifs/` (regenerate with `go run ./cmd/seedgifs`); 503 `gif_search_unavailable` with neither |

Writes, all through `POST /events` with an `event_type` (form or JSON):

- Signed works: any body carrying a `cid`. Verified exactly as `work_event.go`
  does — canonical bytes, SHA-256, ECDSA-P256 over the CID against the key
  registered for the session's PIAL. Answers `201 {"work_id","cid"}`, or the
  real server's words: `cid mismatch`, `signature invalid`, `unknown work kind`,
  `reply requires parent_cid`. A duplicate CID answers `200` with the existing
  row rather than feed-engine's bare 500.
- `work_like`/`work_unlike`, `work_repost`/`work_unrepost`,
  `work_bookmark`/`work_unbookmark`, `work_dislike`/`work_undislike`,
  `work_delete`, `follow`/`unfollow`, `block`/`unblock`, `mute`/`unmute`,
  `tip` (`amount_aet` in hundredths, optional `work_id`), `set_reply_restriction`,
  `pin_work`/`unpin_work`, `report`, `poll_vote`, `notif_read`,
  `notif_read_all`, `not_interested`.

Visions and Live:

| Route | Answers |
|---|---|
| `GET /api/v1/visions` | the tray: the reader's own ring and followed accounts' rings, unseen first, live creators first, every vision inline |
| `GET /api/v1/visions/{id}/viewers` | who watched (owner only) |
| `GET /api/v1/live` | rooms live now, and the reader's own open room |
| `GET /api/v1/live/{id}` | one room with its chat backlog; top tippers for the owner only |
| `GET /api/v1/live/{id}/events` | SSE: `chat`, `tip`, `tips`, `viewers`, `hearts`, `pinned`, `status`. The open connection is the viewer's presence. |

Frequencies (live audio rooms):

| Route | Answers |
|---|---|
| `GET /api/v1/frequencies?lane=live\|scheduled\|ended\|mine` | `FrequencyListDTO` — the lane, plus `own`: whatever the caller has open right now |
| `GET /api/v1/frequencies/{id}` | `FrequencyRoomDTO` — the room, the stage, the co-hosts, the queue (moderators only) and the reader's own standing |
| `GET /api/v1/frequencies/{id}/events` | SSE: `room`, `counts`, `speaker_joined`, `speaker_left`, `muted`, `request`, `request_resolved`, `cohost`, `flags`, `state`. The first frame is the whole room, rendered for that reader. |

Events: `frequency_create`, `frequency_update`, `frequency_schedule`,
`frequency_start`, `frequency_end`, `frequency_cancel`, `frequency_join`,
`frequency_leave`, `frequency_request_mic`, `frequency_request_withdraw`,
`frequency_request_upvote`, `frequency_request_approve`,
`frequency_request_decline`, `frequency_mute`, `frequency_demote`,
`frequency_remove`, `frequency_block`, `frequency_unblock`,
`frequency_cohost`, `frequency_lock`, `frequency_requests_open`. Targets are
named by handle. Errors keep the brain's own codes — `not_live`, `locked`,
`blocked`, `full`, `speakers_full`, `host_already_live`, `is_host`,
`own_request`, `over` — so a screen can branch on them.

The state machine, the role matrix and the admission rules are transcribed
from `frequencies/src/domain/{lifecycle,role,permissions}.rs`, and the tables
carry the column names of `frequencies/migrations/0001,0002`. There is no
audio: rooms answer `audio_unavailable` with a sentence saying why, the way
live rooms answer `video_unavailable`. Presence is a participant row in state
`joined` — no Redis — so every count on a card comes from rows. The seed opens
one live room hosted by @miiyazuko (a co-host, a muted speaker, two listeners
and a question in the queue) and one scheduled by @dev for tomorrow.

Events: `vision` (unsigned, like feed-engine's `POST /visions`; text or image,
artboard presets, audience, `ttl_hours`, `allow_replies`, `poll_options`),
`vision_seen`, `vision_delete`, `vision_reply` (private; lands in the author's
inbox as `vision_reply`), `vision_poll_vote`, `vision_mute`/`vision_unmute`,
`live_start` (answers the room), `live_end` (answers the summary), `live_chat`,
`live_pin`, `live_heart`, and `tip` with `stream_id`.

Stand-ins:

- `POST /api/pial/signing-key/register` stores the key in `pial_signing_keys`
  and answers `{"ok":true}`. In production Nantar forwards this to Elohim Veni.
- The wallet is `ledger_entries`: a tip is one debit and one credit in a
  transaction, and a sender cannot overdraw (`402 insufficient balance`).

Not served: the user-level SSE stream (`/api/v1/events`), video upload and
transcoding, live video transport (rooms answer `has_video: false` with a
sentence saying why), messaging. Feed pages carry `live_count`; the `live`
lane itself is empty because rooms are read from `/api/v1/live`.

## Contract tests

`make test` runs the Go side of the same contract the Kit tests:

- Every case in `F33D3RKit/Tests/…/Fixtures/malkuth_canonical.json` produces
  byte-identical canonical JSON and the same CID here.
- Every signature vector in `malkuth_signatures.json` — Go-made and
  CryptoKit-made — verifies (or fails) as recorded.
- The keys of `me.json`, `session.json`, `user.json` and `error.json` match the
  live responses.
- No response other than the owner's `/me` contains a PIAL or the ranking
  vocabulary.
- An end-to-end run: register a key, post, see it on a follower's lane with the
  server-derived hashtag, like it, reply, tip, read the inbox and wallet on the
  other side, follow, delete.

## Layout

```
main.go                    flags, seed, listen
internal/store/            SQLite: schema.sql, users, works, notifications, ledger, keys, seed
internal/api/              routes, DTOs, auth, reads, events, signed-work admission, media
seedmedia/                 the images the seeded works reference
data/                      the database and uploads (git-ignored)
```
