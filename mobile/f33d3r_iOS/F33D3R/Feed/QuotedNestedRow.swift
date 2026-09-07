import SwiftUI
import F33D3RKit

/// The second level of a quote chain: the work the quoted work itself quotes.
///
/// The web's `quoted_work_nested` facet, drawn the same way. A quote of a quote
/// does not open a second bordered card — that reads as boxes inside boxes —
/// it hangs off a thin left rail inside the level-1 card, so the chain reads as
/// one nested column: an identity line, then a square media thumbnail beside a
/// clamped body. Depth stops here, because the server loads exactly two
/// levels and the model cannot express a third.
///
/// Flagged media collapses to a chip, the same rule the level above applies:
/// there is no viewer context down here to run a gate against, and a frame
/// that leaked because it was twice-quoted would be a hole through the gate on
/// the card at the top.
struct QuotedNestedRow: View {
    let nested: QuotedWork

    /// The web's `.quoted-nested-thumb`: 72×72, `radius-md`, cover-fit.
    private let thumbSide: CGFloat = 72

    var body: some View {
        NavigationLink(value: Route.work(id: nested.id)) {
            VStack(alignment: .leading, spacing: 6) {
                head
                HStack(alignment: .center, spacing: 10) {
                    if nested.hasMedia {
                        thumb
                    }
                    if !nested.body.isEmpty {
                        Text(nested.body)
                            .font(.system(size: 13.5))
                            .foregroundStyle(F33Color.ink2)
                            // Three lines alone, two beside a thumbnail — the
                            // row is the thumbnail's height when there is one.
                            .lineLimit(nested.hasMedia ? 2 : 3)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                }
            }
            .padding(.leading, 12)
            .overlay(alignment: .leading) {
                Rectangle()
                    .fill(F33Color.hairline)
                    .frame(width: 2)
            }
            .padding(.top, 8)
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityElement(children: .combine)
        .accessibilityLabel("Quoted: \(nested.author.displayName), \(nested.body)")
    }

    /// avatar · name · badge · handle · time, on one row. The handle absorbs
    /// the overflow, the name gives way after it, and the time is never
    /// clipped, because "Aug 28 22…" is not a time. Same rule and same reason
    /// as the level-1 head in `QuotedWorkCard`.
    private var head: some View {
        HStack(spacing: 6) {
            F33Avatar(author: nested.author, size: 16)

            Text(nested.author.displayName)
                .font(.system(size: 13, weight: .semibold))
                .foregroundStyle(F33Color.ink)
                .lineLimit(1)
                .layoutPriority(1)

            if let badge = nested.author.badge {
                BadgePill(badge: badge)
                    .fixedSize()
            }

            Text("@\(nested.author.handle)")
                .font(.system(size: 12))
                .foregroundStyle(F33Color.ink4)
                .lineLimit(1)
                .truncationMode(.tail)

            Text("·")
                .font(.system(size: 12))
                .foregroundStyle(F33Color.ink4)
                .fixedSize()
                .layoutPriority(2)

            Text(nested.compactTimeLabel)
                .font(.system(size: 12))
                .foregroundStyle(F33Color.ink4)
                .fixedSize()
                .layoutPriority(2)

            Spacer(minLength: 0)
        }
    }

    @ViewBuilder
    private var thumb: some View {
        let shape = RoundedRectangle(cornerRadius: F33Radius.md, style: .continuous)
        ZStack {
            if nested.isFlagged {
                shape
                    .strokeBorder(F33Color.hairline, style: StrokeStyle(lineWidth: 1, dash: [4, 3]))
                Text(nested.isNSFW ? "18+" : "Sensitive")
                    .font(.system(size: 10, design: .monospaced))
                    .foregroundStyle(F33Color.ink3)
                    .multilineTextAlignment(.center)
                    .padding(.horizontal, 6)
            } else {
                RemoteImage(path: nested.video?.posterURL ?? nested.mediaURLs.first, seed: nested.id)
                if nested.video != nil {
                    Color.black.opacity(0.28)
                    Image(systemName: "play.fill")
                        .font(.system(size: 22))
                        .foregroundStyle(.white)
                }
                if nested.mediaURLs.count > 1 {
                    Text("+\(nested.mediaURLs.count - 1)")
                        .font(.system(size: 15, weight: .bold))
                        .foregroundStyle(.white)
                        .shadow(color: .black.opacity(0.65), radius: 3, y: 1)
                }
            }
        }
        .frame(width: thumbSide, height: thumbSide)
        .background(F33Color.bgSunken, in: shape)
        .clipShape(shape)
        .accessibilityHidden(true)
    }
}
