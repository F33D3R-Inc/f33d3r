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
/// Mirrors `CommentGatings` in `feed-engine/internal/db/works.go`, the closed
/// vocabulary the `works_comment_gating_vocabulary` CHECK enforces and the
/// create path validates, in the server's order:
///
/// - `everyone` — anyone who may read the work
/// - `followers` — accounts that follow the author
/// - `circle` — accounts the author and they follow both ways
/// - `verified` — accounts with a fully verified identity
/// - `none` — nobody but the author
public enum CommentGating: String, CaseIterable, Codable, Hashable, Sendable {
    case everyone
    case followers
    case circle
    case verified
    case none

    /// What a reader is told the choice means — the server's own labels for
    /// the compose_gating Facet (`commentGatingLabels`, work_create.go).
    public var title: String {
        switch self {
        case .everyone: return "Everyone"
        case .followers: return "Your followers"
        case .circle: return "Your circle (you follow each other)"
        case .verified: return "Verified accounts"
        case .none: return "Only you"
        }
    }

    public var systemImage: String {
        switch self {
        case .everyone: return "globe"
        case .followers: return "person.2"
        case .circle: return "person.3"
        case .verified: return "checkmark.seal"
        case .none: return "bubble.left.and.exclamationmark.bubble.right"
        }
    }

    /// The compose screen's one-line summary of the choice.
    public var replySummary: String {
        switch self {
        case .everyone: return "Everyone can reply"
        case .followers: return "Your followers can reply"
        case .circle: return "Your circle can reply"
        case .verified: return "Verified accounts can reply"
        case .none: return "Only you can reply"
        }
    }

    /// What the server assumes when the field is empty, and what `malkuth.js`
    /// sends by default. Named rather than inlined so the two agree by
    /// construction.
    public static let `default`: CommentGating = .everyone
}
