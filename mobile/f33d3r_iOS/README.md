# F33D3R for iOS

Native SwiftUI client for [f33d3r.com](https://f33d3r.com).

The backend (`feed-engine` / Nantar) is a server-rendered HTMX app with no JSON
API, so this project has two halves: a `/api/v1` JSON surface added to
feed-engine, and this app on top of it. Both are additive — the web app's Facet
routes are untouched.

Full plan: `~/.claude/plans/i-have-a-database-lexical-sunset.md`

## Layout

```
F33D3R.xcodeproj        App project (Xcode 16+ synchronized folders)
F33D3R/                 App target — SwiftUI views only
  Assets.xcassets/      App icon and the brand mark
  Auth/                 Sign-in
  Shell/                Tab shell, navigation routes
  Feed/                 Lane strip, header, work card and everything it composes
  Profile/              Profile screen
  Media/                Inline HLS player
  Support/              AppModel, avatars, badges, states, shared styles
icons/                  Supplied brand artwork the catalog was cut from
F33D3RKit/              SwiftPM package — everything that isn't a View
  Sources/F33D3RKit/
    Models/             Codable mirrors of the Go DTOs
    Networking/         APIClient, Endpoint, APIError
    Auth/               KeychainStore, SessionStore
    Feed/               WorkFeed — pagination and list state
    Design/             Design System v4 tokens, ported from styles.css
    Support/            Timestamps and count formatting, ported from the web
    Preview/            DEBUG-only sample fixtures
  Tests/                Contract + client tests (run headlessly)
```

`F33D3RKit` is a separate package on purpose: it builds and tests with
`swift test`, no simulator required, which is where the parts that must be
exactly right get verified.

## Running

The app talks to `http://127.0.0.1:8081` in DEBUG and `https://f33d3r.com` in
release. Override with the `F33D3R_BASE_URL` environment variable in the scheme.

There are two local backends.

### The dev server on this Mac (default)

```bash
cd devserver && make run          # Go + SQLite, no Docker; seeds five dev accounts
```

`devserver/` is a stand-in for Nantar's `/api/v1` surface plus the `POST /events`
write lane, with Elohim Veni (signing keys) and Ain Soph (the wallet ledger)
folded in. It exists because the platform's Docker stack does not run on this
Mac. The same `/api/v1` surface now lives in feed-engine itself
(`internal/handler/api_v1*.go`), so the dev server is only for working with no
Linux box on the network. See `devserver/README.md`.

Seeded dev accounts all use password `f33d3rdev` — `@tehanibentley`, `@admin`,
`@miiyazuko`, `@dev`, `@guest`. The sign-in screen can also create an account.

### The full platform on the Linux box

The complete stack (`docker-compose.local.yml`: Postgres, feed-engine, every
brain, live ingest) runs on the Linux machine and is reachable from this Mac,
the Simulator and a phone on the same LAN over real HTTPS:

```
https://xxxg-00w0.local:8443        # Bonjour name; or https://<LIVE_PUBLIC_HOST>:8443
```

One-time trust, because the certificate comes from Caddy's local CA
(`infra/README-CERTS.md` has the full procedure and the fingerprint check):

```bash
sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ../../infra/caddy-root.crt
xcrun simctl keychain booted add-root-cert ../../infra/caddy-root.crt      # per simulator
curl https://xxxg-00w0.local:8443/api/health                             # 200, no -k
```

Then set `F33D3R_BASE_URL=https://xxxg-00w0.local:8443` in the scheme's
environment. Nothing else changes: `APIClient` derives the `Origin` header from
the base URL (feed-engine's CSRF check requires it on every mutation), no ATS
exception is needed for a trusted root, and the project's
`NSLocalNetworkUsageDescription` is what lets iOS resolve the `.local` name — a
physical device asks once. Sign in with an account created through the web
app's own onboarding at that URL; the dev server's seeded accounts do not
exist there.

**What works there:** everything the app asks for. feed-engine serves the
whole `/api/v1` surface from `feed-engine/internal/handler/api_v1*.go`: auth,
`me`, `feed` (every lane, `tag`, and `live_count` on each page), works and
replies, profiles and every tab including `saves`, followers and following,
`notifications`, `wallet`, `search` (works, people, tags), `tags/trending`,
`sessions`, `visions` and vision viewers, `live` and live rooms, `media`, and
the two server-sent event streams (`events` for the account: notify, new_post,
post_deleted, balance, live_start; `live/{id}/events` for a room: chat, tip,
viewers, hearts, pinned, tips, status). The session token the app holds is the
web's own `user_sessions` row presented as `Authorization: Bearer`, so
signing out on the web signs the phone out too. Every event the app posts has
a case in feed-engine's event lane — reactions, follow/block/mute, tips
(including `stream_id` tips), polls, reports, notifications, pins,
not_interested, reply restriction, the vision events (`vision`, `vision_seen`,
`vision_delete`, `vision_reply`, `vision_poll_vote`, `vision_mute`), the live
events (`live_start`, `live_end`, `live_chat`, `live_pin`, `live_heart`), and
the account events (`profile_update`, `settings.privacy.*`,
`password_change`, `session_revoke`, `sessions_revoke_others`, `work_edit`).
Two honest limits: `work_purchase` answers `400 work is not for sale`, because
paid works on this platform sell through the Themis marketplace (BTC/XRP), not
a per-work AET price, so no work ever carries `price_uaet`; and a broadcast
started from the app is chat, tips and presence only (`has_video` false) until
the app has a video publish path — a web-started broadcast reports
`has_video` true but the app has no player field for its playlist yet. The
wallet reads Ain Soph, so it answers `503 wallet_unavailable` on a stack
started without that brain.

The Go side of the contract is `feed-engine/internal/handler/api_v1_contract_test.go`:
it reads the Swift models' coding keys from `F33D3RKit/Sources/F33D3RKit/Models`
and fails when the DTOs and the models disagree, so a key renamed on one side
fails `go test` on the Linux box before the app is ever built.

## Tests

```bash
cd F33D3RKit && swift test          # models, networking, signing, contract fixtures
cd devserver && make test           # the Go side of the same fixtures, plus an end-to-end write-lane run
```

### The contract fixtures

`F33D3RKit/Tests/F33D3RKitTests/Fixtures/*.json` are generated from the real Go
DTO projections, not written by hand. They make the client/server contract a
checked-in artifact both sides test against, so renaming a field on one side
fails the other side's build instead of failing on a device.

Regenerate after an intentional DTO change:

```bash
cd ~/f33d3r/feed-engine
F33D3R_GOLDEN_DIR=~/Documents/f33d3r_iOS/F33D3RKit/Tests/F33D3RKitTests/Fixtures \
  go test ./internal/handler/ -run TestWriteAPIV1GoldenFixtures -v
```

Then run `swift test` and fix whatever breaks.

## Conventions

- iOS 17+, SwiftUI, `@Observable`, async/await, Swift 6 strict concurrency.
- No third-party dependencies.
- Views are dumb. Fetching and caching live in the Kit.
- Never decode a field the server DTO does not expose. If the app needs a value,
  add it to the Go DTO deliberately — several fields are withheld on purpose
  (the PIAL identity spine, ranking internals) and are covered by tests on both
  sides.
- Design tokens come from `web/static/css/styles.css`. The web is the source of
  truth; port values rather than inventing them.

## The home screen — direction 3a, "Lanes"

Home is a header row, a lane strip, and one feed per lane in a swipeable pager.

- Lanes: Following · For you · Music · Visions · Live. Following is the default,
  and the last lane chosen is remembered across launches — a feed that reopens
  on the algorithmic lane after the reader deliberately left it is the complaint
  this layout exists to answer. Stored per device in `UserDefaults`, because it
  is a reading preference and not state the server owns.
- Lanes the deployment does not serve yet degrade to their empty state rather
  than an error: `live` answers 400 `unknown_surface` today, and
  `WorkFeed.home` turns that (and 404/501) into an empty page. A real fault
  still reaches the error state.
- Density: card or compact, persisted the same way, applied to the whole shell
  so a card looks the same in a thread or a profile as in the feed.
- Card: `@handle` leads, then age · kind · gate, a `why:` provenance chip on the
  right, and an action row of reply · repost · tip · save · ⋯.
- Tabs: Home · Explore · Visions · Inbox · Wallet, icons always with labels.

## Status

The app reads and writes real data against the dev server. Every screen below
has been built, run on the Simulator against seeded data, and looked at.

| | |
|---|---|
| Sign in, sign up, session restore, sign out | done |
| Home lanes (Following · For you · Music · Visions · Live), density, lane memory | done |
| Explore (For you · Trending), not-interested signal | done |
| Work card: like, repost/quote, tip, save, reply, mute, report; owner: pin, who-can-reply, delete | done, server-confirmed |
| Compose: post, reply, quote; photos (uploaded on attach); polls; who-can-reply; 18+ | done |
| Signing: per-device ECDSA key, registered on sign-in, envelope built once and retried as the same bytes | done |
| Work detail: ancestors, replies, stat row, glass reply bar | done |
| Poll voting | done |
| Profile: follow/unfollow, tip, tabs (likes owner-only) | done |
| Inbox: grouped notifications, mark read on open, mark all read, badge | done |
| Wallet: settled and pending balances, ledger | done |
| Visions (24h posts): tray under the Home header that collapses on scroll, full-bleed viewer with timer, private reply and tip, camera-first composer with text card presets, polls, audience and lifetime chips | done |
| Live: lane of rooms, go-live sheet with network and mic checks, broadcaster view with tip goal, chat, pin and end-of-stream summary, viewer with chat, hearts, quick tips, share | done — chat, presence, tips and hearts ride the room's SSE stream |
| Search: works, people, tags as you type; recents on the device; trending tags. iOS 26: the tab bar's own search control | done |
| Tag timelines, follower and following lists with follow buttons | done, server-confirmed |
| Edit profile: avatar and header upload, name, bio, pronouns, location, website, accent | done |
| Settings: density, content setting, sensitive media, celebrations, private account, change password, signed-in devices with revoke, sign out | done — every switch re-reads `/me` |
| Edit a work (owner, within the server's hour) | done |
| Media viewer: full-screen paging, pinch and double-tap zoom, drag to dismiss, share, save to Photos | done |
| User stream (`GET /api/v1/events`): live inbox badge, "N new works" pill on Following, deletions leave every list, wallet refetch on balance | done |
| Haptics on server-confirmed like/repost/save, symbol bounce, double-tap-to-like on media, tab re-tap pops or scrolls to top, iOS 26 tab bar minimises on scroll | done |
| Home chrome hides on scroll down (wordmark and visions row), returns on scroll up; lane strip stays | done |
| Appearance: System / Light / Dark kept on the device; the profile's six accent themes (`theme_id`: void, aurora, sakura, obsidian, moss, dusk) repaint the whole app, chosen in Edit profile | done — server validates the set |
| Live video transport, video upload, messaging | not yet — no media server in this deployment; the viewer says so where the picture would be |

The Visions and Live screens follow turns 4 and 5 of the "F33D3R Home Feed
Wireframes" design project (`F33D3R Home Feed Wireframes.dc.html`); Home is
its turn 3, direction 3a.

### How mutations work

Nothing in the app flips a value before the server has. A like sends
`work_like`, then fetches the work back and hands the server's row to every
list that holds it (`AppModel.current(_:)`). There is no optimistic state to
reconcile, which is the Facet Architecture rule, and against a local server the
round trip is a few milliseconds.

## Sample mode

The UI can be run without a backend:

```bash
F33D3R_SAMPLE=1                # serve SampleData instead of the network
F33D3R_SAMPLE_FROM=4           # rotate the feed so card #5 is on top
F33D3R_LANE=music              # open on a lane without tapping the strip
F33D3R_DENSITY=compact         # open at a density without tapping the chip
```

The last two are DEBUG-only overrides for one run. They do not write, so a run
started with them leaves the reader's own stored lane and density alone. They
exist because a Simulator driven entirely through `simctl` cannot tap.

Set these in the scheme's environment variables, or via
`SIMCTL_CHILD_*` when launching with `simctl`.

`F33D3R_PAGE=<home|explore|music|visions|inbox|wallet|profile|work>` roots the
app at one surface, bypassing the tab bar, and these go with it:

```bash
F33D3R_PAGE=explore            # open straight on a tab you cannot tap
F33D3R_EXPLORE_LANE=trending   # ...on its second lane
F33D3R_PAGE=profile F33D3R_PROFILE=miiyazuko   # root at a profile
F33D3R_PAGE=work F33D3R_WORK=<uuid>            # root at a work's detail
F33D3R_COMPOSE=1               # open the compose sheet over Home on launch
F33D3R_VISION=1                 # open the first ring in the vision viewer
F33D3R_VISION_COMPOSE=1         # open the vision composer
F33D3R_LANE=live F33D3R_GO_LIVE=1     # open the go-live sheet
F33D3R_LANE=live F33D3R_BROADCAST=1   # open your own open room as broadcaster
F33D3R_PAGE=live F33D3R_LIVE=<uuid>   # root at a room as a viewer
F33D3R_APPEARANCE=dark                # force light/dark/system for one run
F33D3R_PAGE=search F33D3R_QUERY=roof  # search with the field filled
F33D3R_PAGE=settings                  # the account's settings
F33D3R_PAGE=followers F33D3R_PROFILE=tehanibentley   # a follow list (also `following`)
F33D3R_PAGE=tag F33D3R_TAG=film       # a tag timeline
F33D3R_PAGE=editprofile               # own profile with the editor open
F33D3R_PAGE=work F33D3R_WORK=<uuid> F33D3R_MEDIA_VIEWER=1   # first image full-screen
F33D3R_SCROLLED=1              # open every list already scrolled to the end
F33D3R_SIGN_IN=dev:f33d3rdev   # sign in for this run, if the device has no session
```

All four are DEBUG-only and opt-in per run. They exist because the Simulator here
has no Simulator.app: every run is `simctl` plus a screenshot, nothing can be
tapped, typed or scrolled, and a screen that cannot be reached is a screen that
ships having been compiled and never looked at. `F33D3R_SCROLLED` in particular
is how "the feed runs under the glass chrome" is checked rather than asserted.
`F33D3R_SIGN_IN` only acts when the device has no session of its own; it never
replaces a real one.

Sample mode is opt-in per run and is never a fallback: if the server is
unreachable in a normal run the app shows its error state, because a feed of
invented works presented as real ones would be the app lying about what the
platform contains.

## Acting as a second account

The Simulator is one device signed in as one person. To see what another
account's action does to it — the "new works" pill, a mention landing in the
inbox, a reply under a work — post as that account through the same signed
write lane the app uses:

```sh
cd devserver && go run ./cmd/postas -as tehanibentley -body "morning from the roof @dev #f33d3r"
```

## Toolchain note

`xcodebuild` and `swift test` both need Xcode's toolchain, not the Command Line
Tools one — without it `swift test` fails with "plugin for module
'TestingMacros' not found". Either set it once:

```bash
sudo xcode-select -s /Applications/Xcode-beta.app/Contents/Developer
```

or prefix individual commands with
`DEVELOPER_DIR=/Applications/Xcode-beta.app/Contents/Developer`.
