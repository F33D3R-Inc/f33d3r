import SwiftUI
import F33D3RKit

/// The explanation behind the `why:` chip.
///
/// Everything on this screen is a string the server composed. The app has no
/// view into how a feed is ordered and must not appear to — a sentence written
/// on device would be the client inventing an account of a decision it did not
/// make, which is worse than not explaining at all.
struct WhyThisSheet: View {
    let provenance: WorkProvenance
    let handle: String

    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: F33Spacing.lg) {
            HStack(spacing: F33Spacing.md) {
                Image(systemName: icon)
                    .font(.system(size: 20, weight: .medium))
                    .foregroundStyle(F33Color.accent)
                    .frame(width: 40, height: 40)
                    .background(F33Color.accentSoft, in: Circle())

                Text("Why you're seeing this")
                    .font(.title3.weight(.semibold))
                    .foregroundStyle(F33Color.ink)

                Spacer(minLength: 0)
            }
            .padding(.top, F33Spacing.xl)

            // Verbatim. The server wrote this sentence; the app has no view
            // into how a feed is ordered and must not appear to.
            Text(provenance.text)
                .font(.callout)
                .foregroundStyle(F33Color.ink2)
                .fixedSize(horizontal: false, vertical: true)

            // Only when the reason is about somebody else. "You follow @x" above
            // "From @x." is the same fact printed twice.
            if provenance.handle != handle {
                Text("From @\(handle).")
                    .font(.footnote)
                    .foregroundStyle(F33Color.ink4)
            }

            Spacer(minLength: 0)

            Button("Done") { dismiss() }
                .buttonStyle(F33PrimaryButtonStyle())
                .padding(.bottom, F33Spacing.lg)
        }
        .padding(.horizontal, F33Spacing.xl)
        .frame(maxWidth: .infinity, alignment: .leading)
        // Glass rather than an opaque plate: this sheet answers a question about
        // the card underneath it, and keeping the shape of that card visible is
        // what keeps the answer attached to the thing it is about.
        .f33GlassSheet()
        .presentationDetents([.height(300)])
        .presentationDragIndicator(.visible)
    }

    /// `kind` is a closed vocabulary, but an unknown value has to draw
    /// something — a lane added server-side must not leave a hole in the sheet.
    private var icon: String {
        switch provenance.kind {
        case "reposted": return "arrow.2.squarepath"
        case "you_follow": return "person.2.fill"
        case "circle": return "person.3.fill"
        case "surface": return "square.stack"
        case "popular": return "chart.line.uptrend.xyaxis"
        default: return "sparkles"
        }
    }
}

/// The chip itself, on the author row.
///
/// The audit kept this through all three directions, and it is the one control
/// on a card that answers a question about the feed rather than about the work.
struct WhyChip: View {
    /// The server's own sentence. "why:" is drawn as a prefix beside it rather
    /// than spliced into it, so the reason itself stays exactly as it was sent.
    let reason: String
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            (Text("why: ").foregroundColor(F33Color.ink4) + Text(reason))
                .font(.system(size: 10.5, weight: .medium))
                .foregroundStyle(F33Color.ink3)
                .lineLimit(1)
                // Not fixed-size: on a row with a long handle the chip gives way
                // first. A truncated reason still points at the sheet that
                // carries the whole sentence, whereas a truncated handle is a
                // person the reader can no longer identify.
                .truncationMode(.tail)
                .padding(.horizontal, 7)
                .padding(.vertical, 3)
                .background(F33Color.bgSunken, in: Capsule())
                .overlay(Capsule().strokeBorder(F33Color.hairline, lineWidth: 1))
                // The chip is drawn small so it does not compete with the
                // handle beside it, and its hit area is pushed back out past
                // its own edges to the 44pt minimum. Giving the row a 44pt
                // control instead would add a quarter of an inch to the height
                // of every card in the feed to make one chip easier to hit.
                .contentShape(Rectangle().inset(by: -11))
        }
        .buttonStyle(.plain)
        .accessibilityLabel("Why you're seeing this: \(reason)")
        .accessibilityHint("Opens the full explanation")
    }
}
