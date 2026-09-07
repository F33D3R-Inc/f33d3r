import SwiftUI
import F33D3RKit

/// What to finish the token with: people for `@`, tags for `#`, tickers for `$`.
///
/// It sits between the draft and the compose bar rather than floating beside
/// the caret, which is what the web does. On a phone the caret is usually under
/// the reader's own hand and a panel drawn there covers the word being typed;
/// docked above the keyboard the list is in the one place the thumb is already
/// resting, and nothing it offers is ever hidden behind a finger.
///
/// Bounded, and scrolls past that. A list taller than a few rows is a list
/// covering the draft, and the way to the rest of an answer is another letter
/// rather than a longer scroll.
struct ComposeSuggestionList: View {
    let results: ComposeSuggestions.Results
    /// Called with what should replace the token — a handle, a tag, a ticker,
    /// each without its trigger.
    let choose: (String) -> Void

    /// Four rows and a glimpse of a fifth: enough to show there is more without
    /// taking the draft off screen.
    private static let maxHeight: CGFloat = 232

    var body: some View {
        ScrollView {
            LazyVStack(spacing: 0) {
                switch results {
                case .people(let people):
                    ForEach(people) { person in
                        row(label: label(person), hint: "Mentions @\(person.handle)") {
                            choose(person.handle)
                        } content: {
                            PersonSuggestionRow(person: person)
                        }
                    }
                case .tags(let tags):
                    ForEach(tags) { tag in
                        row(label: "#\(tag.tag), \(tag.count == 1 ? "1 work" : "\(Counts.exact(tag.count)) works")",
                            hint: "Adds #\(tag.tag)") {
                            choose(tag.tag)
                        } content: {
                            TagSuggestionRow(tag: tag)
                        }
                    }
                case .cashtags(let cashtags):
                    ForEach(cashtags) { cashtag in
                        row(label: "\(cashtag.display), \(cashtag.companyName)",
                            hint: "Adds \(cashtag.display)") {
                            choose(cashtag.ticker)
                        } content: {
                            CashtagSuggestionRow(cashtag: cashtag)
                        }
                    }
                }
            }
        }
        .scrollBounceBehavior(.basedOnSize)
        .frame(maxHeight: Self.maxHeight)
        .f33GlassBar()
        .overlay(alignment: .top) { CardDivider() }
        .accessibilityElement(children: .contain)
        .accessibilityLabel("\(results.count) suggestions")
        .onChange(of: results) { _, new in
            // Somebody who cannot see a list appear has to be told one did.
            AccessibilityNotification.Announcement("\(new.count) suggestions").post()
        }
        .transition(.move(edge: .bottom).combined(with: .opacity))
    }

    /// One tappable row. The tap target is the whole width and at least a
    /// finger tall — a row is only hit where it draws, and the glass behind it
    /// draws nothing this could be caught by.
    private func row<Content: View>(
        label: String,
        hint: String,
        action: @escaping () -> Void,
        @ViewBuilder content: () -> Content
    ) -> some View {
        Button(action: action) {
            content()
                .padding(.horizontal, F33Spacing.lg)
                .frame(maxWidth: .infinity, alignment: .leading)
                .frame(minHeight: F33Layout.minTouchTarget + 8)
                .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .accessibilityLabel(label)
        .accessibilityHint(hint)
    }

    private func label(_ person: User) -> String {
        let name = person.displayName.isEmpty ? person.handle : person.displayName
        return "\(name), @\(person.handle)"
    }
}

private struct PersonSuggestionRow: View {
    let person: User

    var body: some View {
        HStack(spacing: F33Spacing.md) {
            F33Avatar(user: person, size: 32)

            VStack(alignment: .leading, spacing: 1) {
                HStack(spacing: F33Card.authorRowGap) {
                    Text(person.displayName.isEmpty ? person.handle : person.displayName)
                        .font(.system(size: 15, weight: .semibold))
                        .foregroundStyle(F33Color.ink)
                        .lineLimit(1)
                    if let badge = person.badge {
                        BadgePill(badge: badge)
                    }
                }
                Text("@\(person.handle)")
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink4)
                    .lineLimit(1)
            }

            Spacer(minLength: 0)
        }
    }
}

private struct TagSuggestionRow: View {
    let tag: TagCount

    var body: some View {
        HStack(spacing: F33Spacing.md) {
            Image(systemName: "number")
                .font(.system(size: 15, weight: .semibold))
                .foregroundStyle(F33Color.accent)
                .frame(width: 32, height: 32)
                .background(F33Color.accentSoft, in: Circle())

            VStack(alignment: .leading, spacing: 1) {
                Text("#\(tag.tag)")
                    .font(.system(size: 15, weight: .semibold))
                    .foregroundStyle(F33Color.ink)
                    .lineLimit(1)
                Text(tag.count == 1 ? "1 work" : "\(Counts.exact(tag.count)) works")
                    .font(.system(size: 13))
                    .foregroundStyle(F33Color.ink4)
            }

            Spacer(minLength: 0)
        }
    }
}

private struct CashtagSuggestionRow: View {
    let cashtag: CashtagSuggestion

    var body: some View {
        HStack(spacing: F33Spacing.md) {
            Text(cashtag.ticker)
                .font(.system(size: 12, weight: .bold).monospaced())
                .foregroundStyle(F33Color.aet)
                .lineLimit(1)
                .minimumScaleFactor(0.7)
                .frame(width: 46, height: 32)
                .background(F33Color.aet.opacity(0.14), in: RoundedRectangle(cornerRadius: F33Radius.xs))

            VStack(alignment: .leading, spacing: 1) {
                Text(cashtag.companyName)
                    .font(.system(size: 15, weight: .semibold))
                    .foregroundStyle(F33Color.ink)
                    .lineLimit(1)
                if !cashtag.exchange.isEmpty {
                    Text(cashtag.exchange)
                        .font(.system(size: 13))
                        .foregroundStyle(F33Color.ink4)
                }
            }

            Spacer(minLength: 0)
        }
    }
}
