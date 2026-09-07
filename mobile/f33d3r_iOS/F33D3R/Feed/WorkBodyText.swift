import SwiftUI
import F33D3RKit

/// A work's body, with handles, hashtags, tickers and links made tappable.
///
/// The web renders bodies through `markdownHTML` and `safeHTML`; there is no
/// equivalent hazard here, because this never produces markup — it builds an
/// `AttributedString` and hands it to `Text`, so a body containing `<script>` is
/// characters on screen and nothing else. There is no path from post content to
/// executable anything on this platform, which is the one genuine advantage the
/// native client has over the web on this particular problem.
///
/// Entities are linked with an internal `f33d3r:` scheme rather than a real URL,
/// so a tap routes inside the app instead of opening Safari on our own domain.
struct WorkBodyText: View {
    /// Not named `body` — that name belongs to the View's own body, and a
    /// struct cannot have both.
    let text: String
    var lineLimit: Int?
    /// Set by the caller because this view applies its own font — an outer
    /// `.font()` would be overridden by the one below and silently ignored.
    var size: CGFloat = 15

    /// The ported type scale is in fixed pixels, which is right for matching the
    /// web and wrong for a reader who has turned text size up. Scaling the
    /// number rather than swapping to a text style keeps the design's
    /// proportions at the default size and still honours the setting.
    @ScaledMetric(relativeTo: .body) private var scale: CGFloat = 1

    var body: some View {
        Text(attributed)
            .font(.system(size: size * scale))
            .foregroundStyle(F33Color.ink)
            .lineSpacing(2)
            .tint(F33Color.accent)
            .lineLimit(lineLimit)
            .fixedSize(horizontal: false, vertical: true)
            .multilineTextAlignment(.leading)
            .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var attributed: AttributedString {
        WorkBodyText.parse(text)
    }

    /// Splits a body into plain text and entities.
    ///
    /// Deliberately not `AttributedString(markdown:)`: F33D3R bodies are not
    /// markdown documents, and the markdown parser eats `#tag` as a heading and
    /// `_word_` inside a handle as emphasis — both common in real posts.
    static func parse(_ raw: String) -> AttributedString {
        var out = AttributedString()
        var cursor = raw.startIndex

        for match in entityPattern.matches(in: raw, range: NSRange(raw.startIndex..., in: raw)) {
            guard let range = Range(match.range, in: raw) else { continue }

            if cursor < range.lowerBound {
                out.append(AttributedString(String(raw[cursor..<range.lowerBound])))
            }

            let token = String(raw[range])
            var entity = AttributedString(token)
            entity.foregroundColor = F33Color.accent
            if let link = route(for: token) {
                entity.link = link
            }
            out.append(entity)

            cursor = range.upperBound
        }

        if cursor < raw.endIndex {
            out.append(AttributedString(String(raw[cursor...])))
        }
        return out
    }

    /// Handles are `[A-Za-z0-9_-]`, capped at 30 by the server's own rule; tags
    /// are word characters; links are http(s) only, so a `javascript:` string in
    /// a body is never turned into something tappable.
    ///
    /// The cashtag alternative is the server's own expression rather than a
    /// copy of it: `Cashtag.pattern` is the literal `cashtagRe` uses to
    /// linkify a body, and matching anything else would either light up a `$5`
    /// as a company or leave a live ticker as plain text on this platform
    /// alone.
    private static let entityPattern = try! NSRegularExpression(
        pattern: #"(https?://[^\s<>"]+)|(@[A-Za-z0-9_-]{1,30})|(#\w+)|"# + Cashtag.pattern
    )

    private static func route(for token: String) -> URL? {
        if token.hasPrefix("@") {
            return URL(string: "f33d3r://profile/\(token.dropFirst())")
        }
        if token.hasPrefix("#") {
            guard let encoded = String(token.dropFirst())
                .addingPercentEncoding(withAllowedCharacters: .urlPathAllowed) else { return nil }
            return URL(string: "f33d3r://tag/\(encoded)")
        }
        if token.hasPrefix("$") {
            // Capitals only by the time it gets here, so there is nothing to
            // encode.
            return URL(string: "f33d3r://stocks/\(token.dropFirst())")
        }
        return URL(string: token)
    }

    /// Turns an internal link back into a route. Returns nil for a real external
    /// URL, which the caller should hand to the system instead.
    static func route(from url: URL) -> Route? {
        guard url.scheme == "f33d3r" else { return nil }
        let value = url.path.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
        guard !value.isEmpty else { return nil }

        switch url.host {
        case "profile": return .profile(handle: value)
        case "tag": return .tag(value.removingPercentEncoding ?? value)
        case "stocks": return .stocks(ticker: value)
        default: return nil
        }
    }
}

/// The hashtag pills under a body, mirroring `.post-tags-row`.
struct TagRow: View {
    let tags: [String]

    var body: some View {
        // Wrapping matters more than scrolling here: a post with six tags should
        // show all six, not hide them behind a horizontal gesture nothing else
        // in the card uses.
        FlowLayout(spacing: F33Card.authorRowGap) {
            ForEach(tags, id: \.self) { tag in
                NavigationLink(value: Route.tag(tag)) {
                    Text("#\(tag)")
                        .font(.system(size: 13, weight: .medium))
                        .foregroundStyle(F33Color.accent)
                        .padding(.horizontal, 8)
                        .padding(.vertical, 3)
                        .background(F33Color.accentSoft, in: Capsule())
                }
                .buttonStyle(.plain)
            }
        }
        .padding(.top, F33Spacing.sm)
    }
}

/// A wrapping row. `LazyVGrid` cannot do this — its columns are fixed, and tag
/// pills are all different widths.
struct FlowLayout: Layout {
    var spacing: CGFloat = 6

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) -> CGSize {
        let width = proposal.width ?? .infinity
        let rows = layout(subviews: subviews, in: width)
        let height = rows.map(\.height).reduce(0, +) + spacing * CGFloat(max(0, rows.count - 1))
        return CGSize(width: proposal.width ?? rows.map(\.width).max() ?? 0, height: height)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout ()) {
        var y = bounds.minY
        for row in layout(subviews: subviews, in: bounds.width) {
            var x = bounds.minX
            for index in row.indices {
                let size = subviews[index].sizeThatFits(.unspecified)
                subviews[index].place(
                    at: CGPoint(x: x, y: y),
                    proposal: ProposedViewSize(size)
                )
                x += size.width + spacing
            }
            y += row.height + spacing
        }
    }

    private struct Row {
        var indices: [Int] = []
        var width: CGFloat = 0
        var height: CGFloat = 0
    }

    private func layout(subviews: Subviews, in width: CGFloat) -> [Row] {
        var rows: [Row] = []
        var current = Row()

        for index in subviews.indices {
            let size = subviews[index].sizeThatFits(.unspecified)
            let needed = current.indices.isEmpty ? size.width : current.width + spacing + size.width

            if needed > width, !current.indices.isEmpty {
                rows.append(current)
                current = Row()
                current.indices = [index]
                current.width = size.width
                current.height = size.height
            } else {
                current.indices.append(index)
                current.width = needed
                current.height = max(current.height, size.height)
            }
        }
        if !current.indices.isEmpty { rows.append(current) }
        return rows
    }
}
