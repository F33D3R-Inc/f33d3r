import Foundation

/// The closed vocabulary of Work kinds.
///
/// Mirrors `workKinds` in `feed-engine/internal/handler/work_event.go`, which is
/// itself mirrored by the `works_kind_vocabulary` CHECK constraint in migration
/// 0011. Three copies of one list is two too many, but the server's is the one
/// that decides, and a kind outside it is rejected at the boundary with
/// `400 unknown work kind`.
///
/// It is an enum here rather than a string precisely so that rejection cannot
/// happen: a kind that does not exist cannot be constructed, so the round trip
/// to be told so never happens.
///
/// `vision` is deliberately absent. A Vision is not a Work: that lane left the
/// works table in migration 0008 and has its own table, its own routes and its
/// own events.
public enum WorkKind: String, CaseIterable, Codable, Hashable, Sendable {
    case post
    case reply
    case quote
    case poll
    case video
    case voice
    case threadPost = "thread_post"
    case reactVideo = "react_video"
}

/// Who may reply to a Work.
///
/// Mirrors the documented values of `comment_gating` on `workCanonicalPayload`.
/// The column itself has no CHECK constraint and defaults to `open`, so the
/// server will store whatever it is handed — which makes this enum the only
/// thing standing between a typo and a Work nobody can reply to for reasons no
/// surface explains.
public enum CommentGating: String, CaseIterable, Codable, Hashable, Sendable {
    case everyone
    case followers
    case circle
    case none

    /// What the server assumes when the field is empty, and what `malkuth.js`
    /// sends by default. Named rather than inlined so the two agree by
    /// construction.
    public static let `default`: CommentGating = .everyone
}
