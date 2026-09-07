import SwiftUI
import F33D3RKit

/// The embedded card a quote wraps around the work it cites.
///
/// Two levels, the same two the server loads: this bordered card for the work
/// the quote cites and, when that work is itself a quote, `QuotedNestedRow`
/// hanging off a rail underneath for the work *it* cites. The chain ends there
/// because the data does.
///
/// Flagged media inside a quote gets a chip rather than a gate: the embed has no
/// viewer context to evaluate a gate against, and leaking the image raw because
/// it happened to be quoted would be a hole straight through the gate on the
/// card above it.
struct QuotedWorkCard: View {
    let quoted: QuotedWork

    var body: some View {
        VStack(alignment: .leading, spacing: F33Spacing.sm) {
            // One identity line. Under pressure the handle gives way first, the
            // name second, and the separator and time never — a head that read
            // "Tehani… @tehanib… · Aug 28 22…" had let every token shrink by the
            // same share, which is the one outcome that leaves none of them
            // legible. The web also caps the name at 120px; there is no
            // non-greedy way to say that in an HStack (a flexible frame takes
            // its whole maximum whenever surplus is offered, and the handle was
            // left with a bare ellipsis), so the order of priorities is the
            // whole rule here.
            HStack(spacing: F33Spacing.xs) {
                F33Avatar(author: quoted.author, size: 20)

                Text(quoted.author.displayName)
                    .font(.system(size: 13, weight: .semibold))
                    .foregroundStyle(F33Color.ink)
                    .lineLimit(1)
                    .layoutPriority(1)

                if let badge = quoted.author.badge {
                    BadgePill(badge: badge)
                        .fixedSize()
                }

                Text("@\(quoted.author.handle)")
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink4)
                    .lineLimit(1)
                    .truncationMode(.tail)

                Text("·")
                    .foregroundStyle(F33Color.ink4)
                    .fixedSize()
                    .layoutPriority(2)

                Text(quoted.compactTimeLabel)
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink4)
                    .fixedSize()
                    .layoutPriority(2)

                Spacer(minLength: 0)
            }

            if !quoted.body.isEmpty {
                Text(quoted.body)
                    .font(.system(size: 14))
                    .foregroundStyle(F33Color.ink2)
                    .lineLimit(6)
                    .fixedSize(horizontal: false, vertical: true)
            }

            if quoted.hasMedia {
                if quoted.isFlagged {
                    flaggedChip
                } else if let video = quoted.video {
                    RemoteImage(path: video.posterURL, seed: quoted.id)
                        .aspectRatio(video.aspectRatio, contentMode: .fit)
                        .clipShape(RoundedRectangle(cornerRadius: F33Radius.sm))
                        .overlay(
                            Image(systemName: "play.circle.fill")
                                .font(.system(size: 34))
                                .foregroundStyle(.white.opacity(0.9))
                        )
                } else {
                    MediaGrid(mediaURLs: quoted.mediaURLs, seed: quoted.id)
                }
            }

            if let nested = quoted.nestedWork {
                QuotedNestedRow(nested: nested)
            }
        }
        .padding(F33Spacing.md)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(F33Color.bgElevated, in: RoundedRectangle(cornerRadius: F33Radius.md))
        .overlay(
            RoundedRectangle(cornerRadius: F33Radius.md)
                .strokeBorder(F33Color.hairline, lineWidth: 1)
        )
        .padding(.top, F33Card.mediaTopInset)
    }

    private var flaggedChip: some View {
        Text(quoted.isNSFW ? "18+ content — open to view" : "Sensitive content — open to view")
            .font(.system(size: 12, weight: .medium))
            .foregroundStyle(F33Color.ink3)
            .padding(.horizontal, F33Spacing.md)
            .padding(.vertical, F33Spacing.sm)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(F33Color.bgSunken, in: RoundedRectangle(cornerRadius: F33Radius.sm))
    }
}
