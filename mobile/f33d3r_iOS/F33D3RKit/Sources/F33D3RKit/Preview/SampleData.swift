#if DEBUG
import Foundation

/// Works that stand in for a server, so the UI can be built and looked at
/// before `/api/v1/feed` is reachable.
///
/// These are JSON, decoded through `APIClient.makeDecoder()`, rather than Swift
/// values assembled by hand. That is deliberate: a hand-built `Work` would
/// compile whatever the model happened to say, whereas a fixture that must
/// survive the real decoder catches a key that does not match the Go DTO. The
/// samples double as a decode test every time a preview runs.
///
/// DEBUG only. Nothing here ships, and nothing in the app may fall back to it —
/// a screen showing sample data when the server is unreachable would be lying.
public enum SampleData {

    public static let works: [Work] = decode(json)

    public static var first: Work { works[0] }

    /// The samples rotated to start at `offset`, so any card shape can be put
    /// first. Wraps rather than truncating, so the list is always complete.
    public static func works(from offset: Int) -> [Work] {
        rotate(works, by: offset)
    }

    /// The samples a lane would plausibly carry.
    ///
    /// The filters are the app's guess at what each surface means, not the
    /// server's rule — the server owns that, and these exist only so the
    /// lanes are visibly different from each other while there is no server to
    /// ask. A lane that filters down to nothing keeps its empty state, which is
    /// a surface worth being able to look at too.
    public static func works(lane: FeedSurface, from offset: Int = 0) -> [Work] {
        let matching: [Work]
        switch lane {
        case .following, .forYou:
            matching = works
        case .nsfw:
            matching = works.filter(\.isNSFW)
        case .sports:
            matching = works.filter { $0.tags.contains { ["sports", "nfl", "nba"].contains($0) } }
        case .trending:
            // Whatever is being reacted to most. Ranking is the server's, so
            // this only has to be visibly a different order from Following.
            matching = works.sorted { $0.likeCount + $0.repostCount > $1.likeCount + $1.repostCount }
        case .music:
            matching = works.filter { $0.voice != nil || $0.tags.contains("music") }
        case .visions:
            // A vision is now a row in `visions` with a TTL, not a `kind` — the
            // vocabulary that had one was dropped in migration 0010. What a
            // vision still has is an expiry.
            matching = works.filter { $0.expiresAt != nil }
        case .live:
            matching = works.filter { $0.video != nil }
        }
        return rotate(matching, by: offset)
    }

    /// What the Live lane's badge would show. Page metadata on the wire, so it
    /// travels with a sample page the same way.
    public static let liveCount = 3

    /// One page of a lane, as the sample feed hands it to `WorkFeed`.
    public static func page(lane: FeedSurface, from offset: Int = 0) -> WorkPage {
        WorkPage(works: works(lane: lane, from: offset), liveCount: liveCount)
    }

    private static func rotate(_ list: [Work], by offset: Int) -> [Work] {
        guard !list.isEmpty else { return [] }
        let start = ((offset % list.count) + list.count) % list.count
        return Array(list[start...] + list[..<start])
    }

    /// A single work of each shape, for a preview that wants one case.
    public static func work(kind: String) -> Work {
        works.first { $0.kind == kind } ?? works[0]
    }

    public static let profile: Profile = decode(profileJSON)

    /// A work with the conversation around it, for the detail screen.
    ///
    /// Assembled from the same works the feed shows, so tapping a card in sample
    /// mode lands on the work you actually tapped rather than a different one.
    public static func thread(id: String) -> WorkThread {
        let work = works.first { $0.id == id } ?? first
        let others = works.filter { $0.id != work.id }
        return WorkThread(
            work: work,
            ancestors: [],
            replies: Array(others.prefix(3))
        )
    }

    public static let currentUser: CurrentUser = decode(meJSON)

    /// Fails loudly. A malformed fixture is a broken contract, and silently
    /// returning an empty array would hide it behind an empty-state screen.
    private static func decode<T: Decodable>(_ raw: String) -> T {
        do {
            return try APIClient.makeDecoder().decode(T.self, from: Data(raw.utf8))
        } catch {
            fatalError("SampleData no longer decodes — a model and its fixture have drifted: \(error)")
        }
    }

    /// Timestamps are relative to launch so the age labels exercise every branch
    /// of `RelativeTime` — "now", minutes, a clock time, a weekday, a date.
    private static func ago(_ seconds: TimeInterval) -> String {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f.string(from: Date().addingTimeInterval(-seconds))
    }

    /// A vision's TTL has to be in the future or the countdown reads "expired"
    /// on every launch.
    private static func ahead(_ seconds: TimeInterval) -> String {
        ago(-seconds)
    }

    private static let json = """
    [
      {
        "id": "0f7d1c2e-0001-4a00-9c11-000000000001",
        "cid": "sha256:1a9f0c4d7e2b8a6f3c5d1e0b9a8f7c6d5e4b3a2918070605040302010f0e0d0c",
        "kind": "post",
        "author": {
          "handle": "tehanibentley", "display_name": "Tehani Bentley",
          "is_verified": true, "is_creator": true, "role": "founder", "realm": 5
        },
        "body": "Shipped the native client's first screen tonight. Same tokens as the web, same card, no framework in sight.",
        "created_at": "\(ago(45))",
        "content_type": "text",
        "like_count": 128, "repost_count": 19, "reply_count": 7, "view_count": 4210,
        "liked_by_viewer": true,
        "score_band": "trending",
        "provenance": {
          "kind": "you_follow",
          "text": "You follow @tehanibentley",
          "handle": "tehanibentley"
        },
        "tip_total_uaet": 1250000000,
        "latest_replier_handles": ["dev", "miiyazuko", "admin"]
      },
      {
        "id": "0f7d1c2e-0002-4a00-9c11-000000000002",
        "cid": "sha256:2b8e1d5c6f3a9b7e4d6c2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c",
        "kind": "post",
        "author": {
          "handle": "miiyazuko", "display_name": "Mii Yazuko",
          "is_creator": true, "role": "creator", "realm": 4
        },
        "body": "four frames from the roll, nothing retouched",
        "created_at": "\(ago(1500))",
        "content_type": "image",
        "media_urls": [
          "/media/sample/roll-01.webp", "/media/sample/roll-02.webp",
          "/media/sample/roll-03.webp", "/media/sample/roll-04.webp"
        ],
        "tags": ["film", "35mm"],
        "like_count": 2411, "repost_count": 302, "reply_count": 44, "view_count": 88120,
        "bookmarked_by_viewer": true,
        "score_band": "rising",
        "provenance": {
          "kind": "circle",
          "text": "Liked by @dev, who you follow",
          "handle": "dev"
        },
        "tip_total_uaet": 4500000
      },
      {
        "id": "0f7d1c2e-0003-4a00-9c11-000000000003",
        "cid": "sha256:3c9f2e6d7a4b0c8f5e7d3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d",
        "kind": "post",
        "author": { "handle": "dev", "display_name": "Engineering", "role": "user", "realm": 3 },
        "body": "Reposts show the poster in the header and the original creator underneath. Never both in the same slot.",
        "created_at": "\(ago(9000))",
        "content_type": "text",
        "reposted_by": {
          "handle": "admin", "display_name": "Admin", "is_verified": true, "role": "admin", "realm": 5
        },
        "reposted_at": "\(ago(600))",
        "like_count": 61, "repost_count": 12, "reply_count": 3, "view_count": 1940,
        "reposted_by_viewer": true,
        "provenance": { "kind": "reposted", "text": "Reposted by @admin", "handle": "admin" }
      },
      {
        "id": "0f7d1c2e-0004-4a00-9c11-000000000004",
        "cid": "sha256:4d0a3f7e8b5c1d9a6f8e4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e",
        "kind": "video",
        "author": {
          "handle": "miiyazuko", "display_name": "Mii Yazuko",
          "is_creator": true, "role": "creator", "realm": 4
        },
        "body": "HLS, master stream. The watermarked rendition is the one that leaves the platform.",
        "created_at": "\(ago(50000))",
        "content_type": "video",
        "video": {
          "master_url": "/media/sample/clip/master.m3u8",
          "poster_url": "/media/sample/clip/poster.webp",
          "duration_secs": 47.5, "width": 1920, "height": 1080
        },
        "like_count": 940, "repost_count": 88, "reply_count": 21, "view_count": 31004
      },
      {
        "id": "0f7d1c2e-0005-4a00-9c11-000000000005",
        "cid": "sha256:5e1b4a8f9c6d2e0b7a9f5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f",
        "kind": "quote",
        "author": { "handle": "guest", "display_name": "Guest", "role": "user", "realm": 1 },
        "body": "this is the part everyone gets wrong",
        "created_at": "\(ago(140000))",
        "content_type": "text",
        "quoted_cid": "sha256:3c9f2e6d7a4b0c8f5e7d3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d",
        "quoted": {
          "id": "0f7d1c2e-0003-4a00-9c11-000000000003",
          "cid": "sha256:3c9f2e6d7a4b0c8f5e7d3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d",
          "author": { "handle": "dev", "display_name": "Engineering", "role": "user", "realm": 3 },
          "body": "Reposts show the poster in the header and the original creator underneath. Never both in the same slot.",
          "created_at": "\(ago(9000))"
        },
        "like_count": 14, "reply_count": 2, "view_count": 610
      },
      {
        "id": "0f7d1c2e-0006-4a00-9c11-000000000006",
        "cid": "sha256:6f2c5b9a0d7e3f1c8b0a6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a",
        "kind": "poll",
        "author": { "handle": "admin", "display_name": "Admin", "is_verified": true, "role": "admin", "realm": 5 },
        "body": "Which surface should land next in the app?",
        "created_at": "\(ago(400000))",
        "content_type": "poll",
        "poll": {
          "results": [
            { "index": 0, "label": "Visions", "votes": 412, "pct": 47, "is_winner": true },
            { "index": 1, "label": "Messages", "votes": 288, "pct": 33, "voted": true },
            { "index": 2, "label": "Wallet", "votes": 175, "pct": 20 }
          ],
          "total_votes": 875, "viewer_vote": 1, "closed": false, "time_left": "9 hours left"
        },
        "like_count": 33, "reply_count": 18, "view_count": 5120
      },
      {
        "id": "0f7d1c2e-0007-4a00-9c11-000000000007",
        "cid": "sha256:7a3d6c0b1e8f4a2d9c1b7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b",
        "kind": "post",
        "author": { "handle": "miiyazuko", "display_name": "Mii Yazuko", "is_creator": true, "role": "creator", "realm": 4 },
        "body": "behind the gate",
        "created_at": "\(ago(900000))",
        "content_type": "image",
        "media_urls": ["/media/sample/gated-01.webp"],
        "is_nsfw": true,
        "like_count": 1204, "reply_count": 66, "view_count": 22800
      },
      {
        "id": "0f7d1c2e-0008-4a00-9c11-000000000008",
        "cid": "sha256:8b4e7d1c2f9a5b3e0d2c8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c",
        "kind": "post",
        "author": { "handle": "guest", "display_name": "Guest", "role": "user", "realm": 1 },
        "body": "Two up.",
        "created_at": "\(ago(1400000))",
        "content_type": "image",
        "media_urls": ["/media/sample/two-01.webp", "/media/sample/two-02.webp"],
        "link_preview": {
          "url": "https://f33d3r.com/help/works",
          "title": "What is a work?",
          "description": "Works are content-addressed. A repost never duplicates the asset — it wraps it.",
          "site_name": "F33D3R"
        },
        "like_count": 8, "reply_count": 1, "view_count": 210,
        "comment_gating": "followers"
      },
      {
        "id": "0f7d1c2e-0009-4a00-9c11-000000000009",
        "cid": "sha256:9c5f8e2d3a0b6c4f1e3d9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d",
        "kind": "voice",
        "author": {
          "handle": "nocturnesignal", "display_name": "Nocturne Signal",
          "is_creator": true, "is_verified": true, "role": "creator", "realm": 4
        },
        "body": "third pass at the B-side. all analogue, one take, nothing quantised.",
        "created_at": "\(ago(4200))",
        "content_type": "voice",
        "voice": { "url": "/media/sample/nocturne-b-side.m4a", "duration_secs": 214 },
        "tags": ["music", "analogue"],
        "like_count": 316, "repost_count": 41, "reply_count": 12, "view_count": 9840,
        "provenance": {
          "kind": "popular",
          "text": "Popular in Music right now"
        },
        "tip_total_uaet": 12400000
      },
      {
        "id": "0f7d1c2e-0010-4a00-9c11-000000000010",
        "cid": "sha256:a0d6f9e3b4c1d7a5f2e0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8",
        "kind": "post",
        "author": {
          "handle": "miiyazuko", "display_name": "Mii Yazuko",
          "is_creator": true, "role": "creator", "realm": 4
        },
        "body": "3am, second roll, still raining",
        "created_at": "\(ago(2600))",
        "expires_at": "\(ahead(6 * 3600))",
        "content_type": "image",
        "media_urls": ["/media/sample/vision-01.webp"],
        "like_count": 88, "reply_count": 4, "view_count": 2140,
        "provenance": { "kind": "you_follow", "text": "You follow @miiyazuko", "handle": "miiyazuko" }
      },
      {
        "id": "0f7d1c2e-0011-4a00-9c11-000000000011",
        "cid": "sha256:b1e7a0f4c5d2e8b6a3f1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9",
        "kind": "video",
        "author": {
          "handle": "nocturnesignal", "display_name": "Nocturne Signal",
          "is_creator": true, "is_verified": true, "role": "creator", "realm": 4
        },
        "body": "on now — modular set, no setlist",
        "created_at": "\(ago(300))",
        "content_type": "video",
        "video": {
          "master_url": "/media/sample/live/master.m3u8",
          "poster_url": "/media/sample/live/poster.webp",
          "duration_secs": 0, "width": 1080, "height": 1920
        },
        "tags": ["music"],
        "like_count": 74, "reply_count": 31, "view_count": 1180,
        "provenance": { "kind": "surface", "text": "Live in Music right now" },
        "tip_total_uaet": 890000
      },
      {
        "id": "0f7d1c2e-0012-4a00-9c11-000000000012",
        "cid": "sha256:c2f8b1a5d6e3f9c7b4a2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0",
        "kind": "post",
        "author": {
          "handle": "miiyazuko", "display_name": "Mii Yazuko",
          "is_creator": true, "role": "creator", "realm": 4
        },
        "body": "The full contact sheet from Tuesday — 36 frames, my notes on each.",
        "created_at": "\(ago(21000))",
        "content_type": "image",
        "media_urls": ["/media/sample/sheet-01.webp"],
        "subscriber_only": true,
        "like_count": 402, "reply_count": 27, "view_count": 6100,
        "provenance": { "kind": "you_follow", "text": "You follow @miiyazuko", "handle": "miiyazuko" },
        "tip_total_uaet": 300000
      },
      {
        "id": "0f7d1c2e-0013-4a00-9c11-000000000013",
        "cid": "sha256:d3a9c2b6e7f4a0d8c5b3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1",
        "kind": "video",
        "author": {
          "handle": "nocturnesignal", "display_name": "Nocturne Signal",
          "is_creator": true, "is_verified": true, "role": "creator", "realm": 4
        },
        "body": "Full 42-minute session, unlisted mix, one file.",
        "created_at": "\(ago(320000))",
        "content_type": "video",
        "video": {
          "master_url": "/media/sample/session/master.m3u8",
          "poster_url": "/media/sample/session/poster.webp",
          "duration_secs": 2520, "width": 1920, "height": 1080
        },
        "tags": ["music"],
        "price_uaet": 3000000,
        "like_count": 51, "reply_count": 6, "view_count": 1420,
        "provenance": { "kind": "surface", "text": "From the Music surface" }
      }
    ]
    """

    private static let profileJSON = """
    {
      "user": {
        "handle": "miiyazuko", "display_name": "Mii Yazuko",
        "bio": "Film, mostly. Shooting the city at 3am so you don't have to.",
        "pronouns": "she/her", "location": "Lisbon", "website": "miiyazuko.example",
        "is_verified": false, "is_creator": true, "role": "creator",
        "follower_count": 18402, "following_count": 213, "post_count": 1128,
        "realm": 4, "realm_name": "Adept", "xp": 9400,
        "is_private": false
      },
      "viewer_follows": false,
      "follows_viewer": true
    }
    """

    private static let meJSON = """
    {
      "handle": "dev", "display_name": "Engineering",
      "bio": "Builds the thing.", "role": "user",
      "is_verified": false, "is_creator": false,
      "follower_count": 42, "following_count": 130, "post_count": 88,
      "realm": 3, "realm_name": "Seeker", "xp": 2600,
      "is_private": false,
      "content_setting": "default", "show_sensitive": false, "tier": "free",
      "unread_count": 3,
      "is_adult": true, "is_minor": false, "is_age_verified": true,
      "is_adult_creator": false, "two_fa_enabled": false,
      "celebrations_enabled": true
    }
    """
}
#endif
