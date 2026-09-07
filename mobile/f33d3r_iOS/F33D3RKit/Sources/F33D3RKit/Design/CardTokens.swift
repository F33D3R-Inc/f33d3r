#if canImport(SwiftUI)
import SwiftUI

/// Work-card geometry, ported from `.post-card` and its children in
/// `web/static/css/styles.css`.
///
/// These are the numbers the stylesheet actually uses, not a redesign. A card in
/// the app and a card on the site should measure the same, so when the two are
/// held side by side the difference is the platform and nothing else.
public enum F33Card {
    /// `.post-card { padding: 16px 18px 12px }`
    public static let paddingTop: CGFloat = 16
    public static let paddingHorizontal: CGFloat = 18
    public static let paddingBottom: CGFloat = 12

    /// `.thread-avatar-col { width: 44px }` — also the minimum touch target,
    /// which is why the avatar is tappable at its drawn size.
    public static let avatarColumnWidth: CGFloat = 44
    /// `.pcd-row { gap: 12px }`
    public static let columnGap: CGFloat = 12
    /// `.pcd-author-row { gap: 6px }`
    public static let authorRowGap: CGFloat = 6

    /// `.pcd-media { margin-top: 10px }`
    public static let mediaTopInset: CGFloat = 10
    /// `.media-img-group { gap: 3px; border-radius: 12px }`
    public static let mediaCornerRadius: CGFloat = 12
    public static let mediaTileGap: CGFloat = 3
    /// `.media-img-group[data-count="1"] { max-height: 520px }`
    public static let singleImageMaxHeight: CGFloat = 520
    /// `.pcd-media-portrait .vp-wrap { max-width: 340px }`
    public static let portraitVideoMaxWidth: CGFloat = 340

    /// `.pcd-author-name { font-size: 14px; font-weight: 500 }`
    public static let authorNameSize: CGFloat = 14
    /// `.wab-btn { font-size: 13px }`, `.wab-btn svg { 19px }`
    public static let actionLabelSize: CGFloat = 13
    public static let actionIconSize: CGFloat = 19
    /// `.wab-btn { padding: 7px 10px; border-radius: 999px }`
    public static let actionPaddingVertical: CGFloat = 7
    public static let actionPaddingHorizontal: CGFloat = 10
}

/// A card's geometry at one density.
///
/// The card lane is the ported `.post-card` geometry above, unchanged. The
/// compact lane is not a scaled copy of it: padding and the media ceiling come
/// down hard because that is what puts another work on screen, while the avatar
/// and the body only come down as far as they can without the card ceasing to
/// look like the same card.
public extension F33Card {
    struct Metrics: Hashable, Sendable {
        public let paddingTop: CGFloat
        public let paddingHorizontal: CGFloat
        public let paddingBottom: CGFloat
        public let avatarSize: CGFloat
        public let columnGap: CGFloat
        /// Ceiling on a lone image. `.media-img-group[data-count="1"]` on the
        /// web; lower in compact so one tall photo cannot own the screen.
        public let singleImageMaxHeight: CGFloat
        /// Ceiling on a portrait video's width, which is what bounds its height:
        /// a 9:16 clip drawn to the full column is most of a phone screen.
        public let portraitVideoMaxWidth: CGFloat
        public let bodySize: CGFloat
    }

    /// The card's measurements. One density ships, so this is a lookup with
    /// a single answer — kept as a function because every call site reads
    /// better asking for the metrics than reaching for loose constants.
    static func metrics(_ density: FeedDensity) -> Metrics {
        switch density {
        case .card:
            return Metrics(
                paddingTop: paddingTop,
                paddingHorizontal: paddingHorizontal,
                paddingBottom: paddingBottom,
                avatarSize: avatarColumnWidth,
                columnGap: columnGap,
                singleImageMaxHeight: singleImageMaxHeight,
                portraitVideoMaxWidth: portraitVideoMaxWidth,
                bodySize: 15
            )
        }
    }
}

/// The action-bar accents. Hard-coded on the web rather than themed, because
/// like-pink and repost-green are recognised affordances that must not shift
/// with a user's chosen accent.
public enum F33Action {
    /// `.wab-like.is-on { color: #e05080 }`
    public static let like = Color(hex: 0xE05080)
    /// `.wab-repost.is-on { color: #22c55e }`
    public static let repost = Color(hex: 0x22C55E)
    /// Dislike and bookmark both take the theme accent on the web.
    public static var dislike: Color { F33Color.accent }
    public static var bookmark: Color { F33Color.accent }
    /// Tip is the one control in the row that moves money, and it takes the AET
    /// currency colour everywhere money appears rather than the theme accent —
    /// the same reason like is pink here and not violet.
    public static let tip = F33Color.aet
}

/// Role and identity badges. A direct port of `.role-badge` and its modifiers.
///
/// The web picks exactly one badge per person, in a fixed order, so the app does
/// too — two badges beside one name has never been a state this product has.
public struct F33Badge: Hashable, Sendable {
    public let label: String
    public let tint: Color

    /// Resolves the single badge for an identity, in the web's precedence order:
    /// founder, admin, government, business, verified creator, verified. Anything
    /// else has no badge.
    public static func resolve(
        role: String,
        officialType: String?,
        isVerified: Bool,
        isCreator: Bool
    ) -> F33Badge? {
        switch true {
        case role == "founder":
            return F33Badge(label: "Founder", tint: F33Color.accent)
        case role == "admin":
            return F33Badge(label: "Admin", tint: Color(hex: 0xE53E3E))
        case officialType == "government":
            return F33Badge(label: "Government", tint: Color(hex: 0x3B82F6))
        case officialType == "business":
            return F33Badge(label: "Business", tint: Color(hex: 0xA855F7))
        case isCreator && isVerified:
            return F33Badge(label: "Creator", tint: Color(hex: 0xA855F7))
        case isVerified:
            return F33Badge(label: "Verified", tint: Color(hex: 0x22C55E))
        default:
            return nil
        }
    }
}

public extension WorkAuthor {
    var badge: F33Badge? {
        F33Badge.resolve(
            role: role,
            officialType: officialType,
            isVerified: isVerified,
            isCreator: isCreator
        )
    }
}

public extension User {
    var badge: F33Badge? {
        F33Badge.resolve(
            role: role,
            officialType: officialType,
            isVerified: isVerified,
            isCreator: isCreator
        )
    }
}

/// The gradient behind an avatar that has no image.
///
/// A direct port of `AvatarColors` in `feed-engine/internal/handler/helpers.go`,
/// hash and palette both. The same handle must land on the same two colours in
/// the app as on the web — a person's placeholder avatar is how they are
/// recognised in a list before the image loads, and it changing between surfaces
/// would defeat the point of having one.
public enum AvatarPalette {
    private static let palettes: [(UInt32, UInt32)] = [
        (0x00C853, 0x00897B), (0x7C4DFF, 0x3D5AFE),
        (0xFF6D00, 0xFF3D00), (0x0091EA, 0x00B0FF),
        (0xAA00FF, 0xD500F9), (0x00BFA5, 0x1DE9B6),
        (0xFFD600, 0xFF6F00), (0xC51162, 0xF50057),
    ]

    /// The two stops for `seed`, normally a handle.
    ///
    /// The hash mods by the palette count on *every* step rather than once at
    /// the end, which is unusual but is exactly what the Go does — reproducing
    /// the arithmetic matters more than improving it, since the two have to
    /// agree.
    public static func colors(for seed: String) -> (Color, Color) {
        var h = 0
        for scalar in seed.unicodeScalars {
            h = (h &* 31 &+ Int(scalar.value)) % palettes.count
        }
        let picked = palettes[abs(h) % palettes.count]
        return (Color(hex: picked.0), Color(hex: picked.1))
    }

    /// The letter drawn over the gradient. Mirrors the `firstChar` template
    /// func, including its "?" fallback for an empty name.
    public static func initial(for name: String) -> String {
        guard let first = name.first else { return "?" }
        return String(first).uppercased()
    }
}
#endif
